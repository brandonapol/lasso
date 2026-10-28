package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/config"
	"github.com/brandonapol/lasso/internal/digest"
	"github.com/brandonapol/lasso/internal/jev"
	"github.com/brandonapol/lasso/internal/llm"
	"github.com/brandonapol/lasso/internal/rules"
	"github.com/brandonapol/lasso/internal/store"
	"github.com/brandonapol/lasso/internal/summary"
)

// fakeKind is a bot adapter of a given kind with one session. Its events
// are delivered once; what it is sent is recorded.
type fakeKind struct {
	kind   string
	sess   adapter.Session
	events []adapter.Event

	mu   sync.Mutex
	sent []string // "session: text"
}

func (f *fakeKind) Kind() string { return f.kind }

func (f *fakeKind) Sessions(context.Context) ([]adapter.Session, error) {
	return []adapter.Session{f.sess}, nil
}

func (f *fakeKind) Events(_ context.Context, since adapter.Cursor) ([]adapter.Event, adapter.Cursor, error) {
	if since != "" {
		return nil, since, nil
	}
	return f.events, "read", nil
}

func (f *fakeKind) Send(_ context.Context, session, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, session+": "+text)
	return nil
}

func (f *fakeKind) Answer(context.Context, string, adapter.Choice) error { return nil }

// Cwd puts every session in /work/<kind>.
func (f *fakeKind) Cwd(string) (string, bool) { return "/work/" + f.kind, true }

func (f *fakeKind) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// fakeGrok also knows its prompts' facts, as the grok adapter does.
type fakeGrok struct{ *fakeKind }

func (f fakeGrok) Request(promptID string, choice adapter.Choice) (rules.Request, error) {
	return rules.Request{
		Bot: "grok", Action: rules.AnswerPrompt, Target: promptID, Paths: []string{"src/main.go"},
		Args: map[string]string{"tool": "edit", "rule": "Edit(src/main.go)", "option": fmt.Sprint(choice.Option)},
	}, nil
}

// fakeModels is a fake Anthropic Messages API (Haiku writes digest points
// and sorts, Sonnet writes summaries) and a fake Jev that keeps everything.
type fakeModels struct {
	anthropic, jev *httptest.Server
	sonnet, haiku  atomic.Int32
	jevCalls       atomic.Int32
}

const (
	digestPoint = "Claude finished wiring the bots into the daemon."
	paragraph   = "The bots were wired into the daemon, so their sessions now show up in lasso."
)

var jevItem = regexp.MustCompile(`id="(item_\d+)"`)

func newFakeModels(t *testing.T) *fakeModels {
	f := &fakeModels{}
	f.anthropic = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.System) == 0 {
			t.Errorf("bad model request: %v", err)
		}
		var reply string
		switch {
		case body.Model == llm.Sonnet:
			f.sonnet.Add(1)
			reply = `{"paragraph": "` + paragraph + `", "changes": [{"item": 1, "title": "Bots wired in"}], "who": "You did all of it."}`
		case strings.Contains(body.System[0].Text, `"points"`):
			f.haiku.Add(1)
			reply = `{"points": [{"text": "` + digestPoint + `", "level": "brief"}]}`
		default:
			f.haiku.Add(1)
			reply = `{"level": "log"}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]string{{"type": "text", "text": reply}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 100, "output_tokens": 20},
		})
	}))
	f.jev = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.jevCalls.Add(1)
		var body struct {
			State     string                  `json:"state"`
			Questions map[string]jev.Question `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		answers := map[string]jev.Answer{}
		for _, m := range jevItem.FindAllStringSubmatch(body.State, -1) {
			if body.Questions[m[1]].Type == "choice" {
				answers[m[1]] = jev.Answer{Type: "choice", Choice: "brief", Confidence: 0.95}
			} else {
				answers[m[1]] = jev.Answer{Type: "noul", Noul: 0.98, Confidence: 0.96}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "usage": map[string]int{"input_tokens": 100}})
	}))
	t.Cleanup(f.anthropic.Close)
	t.Cleanup(f.jev.Close)
	return f
}

// oneMerge is o/r's last 24 hours: one merged pull request.
type oneMerge struct{}

func (oneMerge) Activity(context.Context, string, time.Time) (summary.Activity, error) {
	return summary.Activity{
		DefaultBranch: "main", MergedTotal: 1,
		Merged: []summary.Item{{Number: 7, Title: "Wire the bots into the daemon", URL: "https://github.com/o/r/pull/7", Author: "me", At: time.Now().Add(-time.Hour)}},
	}, nil
}

// offlineGH answers every gh call with an error, quietly: GitHub isn't
// what this test is about.
type offlineGH struct{}

func (offlineGH) Run(context.Context, ...string) ([]byte, error) {
	return nil, errors.New("offline")
}

const wireRules = `
[[rules]]
id = "grok-docs"
who = "grok"
action = "answer_prompt"
paths = ["docs/**"]
`

func (r *rig) items(tab api.Tab) []api.Item {
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
	return body.Items
}

func (r *rig) focus(id string) api.Focus {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.base+"/items/"+url.PathEscape(id), nil)
	req.Header.Set("Authorization", "Bearer "+r.userTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var f api.Focus
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		r.t.Fatal(err)
	}
	return f
}

func find(items []api.Item, pred func(api.Item) bool) (api.Item, bool) {
	for _, it := range items {
		if pred(it) {
			return it, true
		}
	}
	return api.Item{}, false
}

// The whole wiring: fake claude, grok and saddle adapters, a fake
// Anthropic API and a fake Jev, with the daemon's own digest, summaries
// and rules engine between them and the API.
func TestBotsDigestAndSummariesWired(t *testing.T) {
	models := newFakeModels(t)
	port := freePort(t)
	opts := testOptions(t, port)
	opts.Source = nil
	cfg := config.Default()
	cfg.HotRepos = []string{"o/r"}
	opts.Config = &cfg
	opts.Runner = offlineGH{}
	opts.AgentToken = "agent-pane-token"
	opts.RulesPath = filepath.Join(t.TempDir(), "rules.toml")
	if err := os.WriteFile(opts.RulesPath, []byte(wireRules), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"ANTHROPIC_API_KEY": "test-key", "JEV_TOKEN": "j", "TYPESAFE_BASE_URL": models.jev.URL}
	opts.Env = func(k string) string { return env[k] }
	opts.AnthropicURL = models.anthropic.URL
	opts.SummaryFetch = oneMerge{}
	opts.gitRemote = func(dir string) (string, error) {
		if strings.HasPrefix(dir, "/work/") {
			return "git@github.com:o/r.git", nil
		}
		return "", errors.New("not a repo")
	}

	now := time.Now()
	cl := &fakeKind{
		kind: "claude",
		sess: adapter.Session{ID: "c1", Bot: "claude", Title: "wire the bots", State: adapter.Idle, Writable: true},
		events: []adapter.Event{
			{ID: "c1-a", Session: "c1", Kind: attention.KindBotMessage, Text: "Read the daemon code.", At: now.Add(-2 * time.Minute)},
			{ID: "c1-b", Session: "c1", Kind: digest.KindTurnEnd, Text: "Done: the bots are wired in.", At: now.Add(-time.Minute)},
		},
	}
	gk := fakeGrok{&fakeKind{
		kind: "grok",
		sess: adapter.Session{ID: "g1", Bot: "grok", Title: "tidy the docs", State: adapter.Waiting, Writable: true},
		events: []adapter.Event{{
			ID: "g1/p1", Session: "g1", Kind: attention.KindPermissionPrompt, Text: "Edit src/main.go?", PromptID: "g1/p1",
			Options: []adapter.Option{{N: 1, Label: "Allow"}, {N: 2, Label: "Deny"}}, At: now,
		}},
	}}
	sd := &fakeKind{kind: "saddle", sess: adapter.Session{ID: "s1", Bot: "saddle", Title: "agent 3: tests", State: adapter.Running}}
	opts.Adapters = []adapter.Adapter{cl, gk, sd}
	if err := start(t, opts); err != nil {
		t.Fatal(err)
	}
	tok, err := os.ReadFile(opts.TokenPath)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", port), userTok: strings.TrimSpace(string(tok)), agentTok: opts.AgentToken, opts: opts}

	// In progress: a row for each kind of bot, the claude one with its
	// digest count, and grok's uncovered prompt as an ask with a card.
	r.eventually("a row per bot and grok's ask", func() bool {
		ids := r.ids(api.TabDoing)
		return slices.Contains(ids, "session:claude:c1") && slices.Contains(ids, "session:grok:g1") &&
			slices.Contains(ids, "session:saddle:s1") && slices.Contains(ids, "doing:bot:grok:g1/p1")
	})
	r.eventually("claude's N → k", func() bool {
		it, _ := find(r.items(api.TabDoing), func(it api.Item) bool { return it.ID == "session:claude:c1" })
		return strings.HasSuffix(it.Status, "2 → 1") && it.Repo == "o/r"
	})
	card := r.focus("doing:bot:grok:g1/p1")
	if !slices.ContainsFunc(card.Fields, func(f api.Field) bool { return f.Name == "Wants" && strings.Contains(f.Value, "permission") }) {
		t.Fatalf("grok's ask has no decision card: %+v", card.Fields)
	}

	// Summaries: the digest's point as a brief item tied to o/r through the
	// session's directory, and o/r's summary written by Sonnet.
	r.eventually("the digest point", func() bool {
		_, ok := find(r.items(api.TabSummaries), func(it api.Item) bool {
			return it.Title == digestPoint && it.Repo == "o/r" && strings.HasPrefix(it.ID, "digest:")
		})
		return ok
	})
	r.eventually("o/r's summary", func() bool {
		it, _ := find(r.items(api.TabSummaries), func(it api.Item) bool { return it.ID == "summary:o:r" })
		return it.Status == "written by "+llm.Sonnet
	})
	f := r.focus("summary:o:r")
	if !strings.Contains(f.Body, paragraph) || !strings.Contains(f.Body, "https://github.com/o/r/pull/7") {
		t.Fatalf("summary body = %q", f.Body)
	}
	if models.jevCalls.Load() == 0 {
		t.Fatal("Jev was never asked")
	}
	// Both keys are set: no notice about missing ones.
	if _, ok := find(r.items(api.TabSummaries), func(it api.Item) bool { return strings.HasPrefix(it.ID, "notice:") }); ok {
		t.Fatal("a missing-key notice with both keys set")
	}

	// g: regenerate_summary writes a new one now, whatever its age.
	before := models.sonnet.Load()
	code, body := r.post(r.userTok, `{"action":"regenerate_summary","repo":"o/r"}`)
	if code != 200 || !strings.Contains(body, "Wrote a new summary of o/r.") {
		t.Fatalf("regenerate: %d %s", code, body)
	}
	if models.sonnet.Load() != before+1 {
		t.Fatalf("regenerate made %d Sonnet calls", models.sonnet.Load()-before)
	}
	// The Summaries row's own id works too; a repo that isn't hot doesn't.
	if code, body := r.post(r.userTok, `{"action":"regenerate_summary","target":"summary:o:r"}`); code != 200 {
		t.Fatalf("regenerate by row id: %d %s", code, body)
	}
	if code, _ := r.post(r.userTok, `{"action":"regenerate_summary","repo":"x/y"}`); code != 403 {
		t.Fatalf("regenerate of a cold repo: %d", code)
	}

	// Sending goes through the rules engine: the user's own send runs and
	// is audited; the agent pane's, with no rule, is refused.
	code, body = r.post(r.userTok, `{"bot":"grok","action":"send_message","target":"session:grok:g1","args":{"text":"carry on"}}`)
	if code != 200 || !slices.Equal(gk.got(), []string{"g1: carry on"}) {
		t.Fatalf("user send: %d %s %q", code, body, gk.got())
	}
	if code, _ := r.post(r.agentTok, `{"bot":"claude","action":"send_message","target":"c1","args":{"text":"hi"}}`); code != 403 || len(cl.got()) != 0 {
		t.Fatalf("agent send without a rule: %d %q", code, cl.got())
	}
	es := r.audit()
	if len(es) != 2 || es[0].Action != "send_message" || es[0].Actor != "user" {
		t.Fatalf("audit = %+v", es)
	}

	// grok's prompt facts come from grok, not the caller: the rule allows
	// docs/** only, and the refused call edits src/main.go.
	code, body = r.post(r.agentTok, `{"bot":"grok","action":"answer_prompt","target":"g1/p1","paths":["docs/x.md"],"args":{"option":"1"}}`)
	if code != 403 {
		t.Fatalf("answer outside the rule's paths: %d %s", code, body)
	}
}

func TestKeyNoticeOnce(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "lasso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	ns := keyNotice(st, llm.ErrNoKey, true, "/cfg", t0)
	if len(ns) != 1 {
		t.Fatalf("notices = %+v", ns)
	}
	n := ns[0]
	for _, want := range []string{"ANTHROPIC_API_KEY", "/cfg/anthropic_key", "JEV_TOKEN", "Summaries", "digest"} {
		if !strings.Contains(n.Body, want) {
			t.Errorf("notice body lacks %q:\n%s", want, n.Body)
		}
	}
	if !strings.Contains(n.Title, "Anthropic or Jev") {
		t.Errorf("title = %q", n.Title)
	}
	// A restart a day later raises the same notice with its first time,
	// so it is new only once.
	again := keyNotice(st, llm.ErrNoKey, true, "/cfg", t0.Add(24*time.Hour))
	if len(again) != 1 || !again[0].At.Equal(t0) || again[0].ID != n.ID {
		t.Fatalf("again = %+v", again)
	}
	if ns := keyNotice(st, nil, false, "/cfg", t0); ns != nil {
		t.Fatalf("notice with every key set: %+v", ns)
	}
	if ns := keyNotice(st, nil, true, "/cfg", t0); len(ns) != 1 || strings.Contains(ns[0].Body, "ANTHROPIC") {
		t.Fatalf("jev-only notice = %+v", ns)
	}
}

// With no keys at all, the real daemon raises the notice as a brief item.
func TestMissingKeysNotice(t *testing.T) {
	r := startRig(t)
	r.eventually("the missing-keys notice", func() bool {
		return slices.Contains(r.ids(api.TabSummaries), "notice:keys:anthropic+jev")
	})
}

func TestGithubRepo(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:o/r.git":         "o/r",
		"https://github.com/o/r":         "o/r",
		"https://github.com/o/r.git":     "o/r",
		"ssh://git@github.com/o/r.git":   "o/r",
		"https://github.com/o/r/pull/12": "o/r",
		"https://gitlab.com/o/r":         "",
		"https://github.com/o":           "",
		"":                               "",
	} {
		if got := githubRepo(in); got != want {
			t.Errorf("githubRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReachable(t *testing.T) {
	if reachable("grok is reachable again") != "" || reachable("saddle is reachable again") != "" || reachable("grok isn't on PATH") == "" {
		t.Fatal("reachable")
	}
}
