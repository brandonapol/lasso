package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/audit"
	"github.com/brandonapol/lasso/internal/config"
)

// fakeBot is a bot adapter with one waiting session per prompt it holds.
// It records every answer it is sent.
type fakeBot struct {
	mu      sync.Mutex
	prompts []string // waiting prompt ids; prompt p waits in session s-p
	answers []string // "prompt=option"
}

func (f *fakeBot) Kind() string { return "fake" }

func (f *fakeBot) Sessions(context.Context) ([]adapter.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []adapter.Session
	for _, p := range f.prompts {
		out = append(out, adapter.Session{ID: "s-" + p, Bot: "fake", Title: "task " + p, State: adapter.Waiting})
	}
	return out, nil
}

func (f *fakeBot) Events(_ context.Context, since adapter.Cursor) ([]adapter.Event, adapter.Cursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := strings.Split(string(since), ",")
	var out []adapter.Event
	for _, p := range f.prompts {
		if !slices.Contains(seen, p) {
			out = append(out, adapter.Event{
				ID: p, Session: "s-" + p, Kind: attention.KindPermissionPrompt, Text: "May I go on?",
				PromptID: p, Options: []adapter.Option{{N: 1, Label: "Yes"}, {N: 2, Label: "No"}}, At: time.Now(),
			})
		}
	}
	return out, adapter.Cursor(strings.Join(f.prompts, ",")), nil
}

func (f *fakeBot) Send(context.Context, string, string) error { return nil }

func (f *fakeBot) Answer(_ context.Context, promptID string, c adapter.Choice) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, fmt.Sprintf("%s=%d", promptID, c.Option))
	f.prompts = slices.DeleteFunc(f.prompts, func(p string) bool { return p == promptID })
	return nil
}

func (f *fakeBot) answered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.answers...)
}

type rig struct {
	t                 *testing.T
	base              string
	userTok, agentTok string
	bot               *fakeBot
	opts              Options
}

const testRules = `
[[rules]]
id = "fake-ok-answers"
who = "fake-ok"
action = "answer_prompt"
`

// startRig runs the real daemon (no hot repos) with a fake bot adapter and
// one rule: the bot fake-ok may answer its own prompts.
func startRig(t *testing.T, prompts ...string) *rig {
	t.Helper()
	port := freePort(t)
	opts := testOptions(t, port)
	opts.Source = nil
	cfg := config.Default()
	opts.Config = &cfg
	opts.Runner = noGH{t}
	opts.AgentToken = "agent-pane-token"
	opts.RulesPath = filepath.Join(t.TempDir(), "rules.toml")
	if err := os.WriteFile(opts.RulesPath, []byte(testRules), 0o600); err != nil {
		t.Fatal(err)
	}
	bot := &fakeBot{prompts: prompts}
	opts.Adapters = []adapter.Adapter{bot}
	if err := start(t, opts); err != nil {
		t.Fatal(err)
	}
	tok, err := os.ReadFile(opts.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	return &rig{
		t: t, base: fmt.Sprintf("http://127.0.0.1:%d", port), userTok: strings.TrimSpace(string(tok)),
		agentTok: opts.AgentToken, bot: bot, opts: opts,
	}
}

func (r *rig) post(token, body string) (int, string) {
	r.t.Helper()
	req, _ := http.NewRequest("POST", r.base+"/actions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *rig) ids(tab api.Tab) []string {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.base+"/items?tab="+string(tab), nil)
	req.Header.Set("Authorization", "Bearer "+r.userTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Items []api.Item }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, it := range body.Items {
		out = append(out, it.ID)
	}
	return out
}

// eventually waits for cond, which the source's refresh makes true soon
// after an action.
func (r *rig) eventually(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *rig) audit() []audit.Entry {
	r.t.Helper()
	es, err := audit.Open(AuditFile(r.opts.StateDir)).Entries()
	if err != nil {
		r.t.Fatal(err)
	}
	return es
}

func TestBotActionsGoThroughRules(t *testing.T) {
	r := startRig(t, "p1")

	// Allowed by a rule: it runs through the adapter and is audited with
	// the rule as the actor.
	code, body := r.post(r.agentTok, `{"bot":"fake-ok","action":"answer_prompt","target":"p1","args":{"option":"1"}}`)
	if code != 200 || !strings.Contains(body, `"rule":"fake-ok-answers"`) {
		t.Fatalf("allowed: %d %s", code, body)
	}
	if got := r.bot.answered(); !slices.Equal(got, []string{"p1=1"}) {
		t.Fatalf("adapter got %q", got)
	}
	es := r.audit()
	if len(es) != 2 || es[0].Actor != "rule:fake-ok-answers" || es[0].Result != audit.Started || es[1].Result != audit.OK || es[0].Args["adapter"] != "fake" {
		t.Fatalf("audit = %+v", es)
	}

	// No rule covers it: 403 with the plain reason, nothing sent or logged.
	code, body = r.post(r.agentTok, `{"bot":"fake-other","action":"answer_prompt","target":"p9","args":{"option":"1"}}`)
	if code != 403 || !strings.Contains(body, "No rule lets fake-other answer its own permission prompts.") {
		t.Fatalf("refused: %d %s", code, body)
	}
	// The always-ask list beats the rule, for the agent pane too.
	code, body = r.post(r.agentTok, `{"bot":"fake-ok","action":"answer_prompt","target":"p9","args":{"option":"1"},"effects":["merge_default_branch"]}`)
	if code != 403 || !strings.Contains(body, "Always ask: it merges into the main branch.") {
		t.Fatalf("always ask: %d %s", code, body)
	}
	// The agent pane can't undo, which would act as the user.
	req, _ := http.NewRequest("POST", r.base+"/actions/"+es[0].ID+"/undo", nil)
	req.Header.Set("Authorization", "Bearer "+r.agentTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("agent undo: %d", resp.StatusCode)
	}
	if got := r.bot.answered(); len(got) != 1 || len(r.audit()) != 2 {
		t.Fatalf("a refused action reached the adapter or the log: %q", got)
	}
}

func TestUserAnswerRunsAsUser(t *testing.T) {
	r := startRig(t, "p1")
	r.eventually("the ask on In progress", func() bool { return slices.Contains(r.ids(api.TabDoing), "doing:bot:fake:p1") })

	// No rule covers this bot, but the user's own answer needs none. The
	// body can't name an actor; the user's token is what makes it theirs.
	code, body := r.post(r.userTok, `{"bot":"fake-1","action":"answer_prompt","target":"p1","args":{"option":"1","choice":"yes"}}`)
	if code != 200 || !strings.Contains(body, "You approved it.") {
		t.Fatalf("user answer: %d %s", code, body)
	}
	if got := r.bot.answered(); !slices.Equal(got, []string{"p1=1"}) {
		t.Fatalf("adapter got %q", got)
	}
	es := r.audit()
	if len(es) != 2 || es[0].Actor != "user" || es[1].Actor != "user" || es[1].Result != audit.OK {
		t.Fatalf("audit = %+v", es)
	}
	r.eventually("the answered ask to go", func() bool { return !slices.Contains(r.ids(api.TabDoing), "doing:bot:fake:p1") })

	// The same request from the agent pane is refused: it can't act as the user.
	code, _ = r.post(r.agentTok, `{"bot":"fake-1","action":"answer_prompt","target":"p1","args":{"option":"1","choice":"yes"}}`)
	if code != 403 {
		t.Fatalf("agent as user: %d", code)
	}
}

func TestThirdAnswerSuggestsARule(t *testing.T) {
	r := startRig(t, "p1", "p2", "p3")
	const suggestion = "rule-suggestion:fake-1-answer-prompt"
	answer := func(p string) {
		t.Helper()
		code, body := r.post(r.userTok, `{"bot":"fake-1","action":"answer_prompt","target":"`+p+`","repo":"o/r","args":{"option":"1","choice":"yes"}}`)
		if code != 200 {
			t.Fatalf("answer %s: %d %s", p, code, body)
		}
	}
	r.eventually("the asks", func() bool { return slices.Contains(r.ids(api.TabDoing), "doing:bot:fake:p3") })
	for _, p := range []string{"p1", "p2"} {
		answer(p)
		r.eventually(p+" to go", func() bool { return !slices.Contains(r.ids(api.TabDoing), "doing:bot:fake:"+p) })
		if slices.Contains(r.ids(api.TabSummaries), suggestion) {
			t.Fatalf("suggested a rule after answering %s", p)
		}
	}
	answer("p3")
	r.eventually("the suggested rule", func() bool { return slices.Contains(r.ids(api.TabSummaries), suggestion) })
}
