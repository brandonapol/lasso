package agentpane

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/audit"
	"github.com/brandonapol/lasso/internal/llm"
	"github.com/brandonapol/lasso/internal/rules"
	"github.com/brandonapol/lasso/internal/store"
)

const (
	userTok  = "user-token"
	agentTok = "agent-token"
)

var now = time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)

// fakeModel is a fake Messages API. Each call is answered by the next
// scripted turn; it records every request.
type fakeModel struct {
	mu    sync.Mutex
	turns []map[string]any
	seen  []struct {
		System   []llm.Block   `json:"system"`
		Messages []llm.Message `json:"messages"`
		Tools    []llm.Tool    `json:"tools"`
	}
}

func text(s string) map[string]any {
	return map[string]any{"stop_reason": "end_turn", "content": []map[string]any{{"type": "text", "text": s}}}
}

func call(id, name string, input any) map[string]any {
	return map[string]any{"stop_reason": "tool_use", "content": []map[string]any{
		{"type": "tool_use", "id": id, "name": name, "input": input},
	}}
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req struct {
		System   []llm.Block   `json:"system"`
		Messages []llm.Message `json:"messages"`
		Tools    []llm.Tool    `json:"tools"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.seen = append(f.seen, req)
	if len(f.turns) == 0 {
		http.Error(w, "no more scripted turns", http.StatusBadRequest)
		return
	}
	t := f.turns[0]
	f.turns = f.turns[1:]
	t["usage"] = map[string]int{"input_tokens": 100, "output_tokens": 10}
	_ = json.NewEncoder(w).Encode(t)
}

// lastToolResult is the last tool_result the model was sent.
func (f *fakeModel) lastToolResult(t *testing.T) llm.Block {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.seen) - 1; i >= 0; i-- {
		msgs := f.seen[i].Messages
		for j := len(msgs) - 1; j >= 0; j-- {
			for _, b := range msgs[j].Content {
				if b.Type == "tool_result" {
					return b
				}
			}
		}
	}
	t.Fatal("the model was never sent a tool result")
	return llm.Block{}
}

// sent records what the executor was asked to do.
type sent struct {
	mu    sync.Mutex
	steps []audit.Step
}

func (s *sent) Execute(_ context.Context, st audit.Step) (rules.Done, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, st)
	return rules.Done{Detail: "sent"}, nil
}

func (*sent) Inverse(audit.Step, rules.Done) (audit.Step, bool) { return audit.Step{}, false }

type rig struct {
	t         *testing.T
	src       *api.FakeSource
	hub       *api.Hub
	model     *fakeModel
	exec      *sent
	rulesPath string
	auditPath string
	srv       *httptest.Server
	st        *store.Store
	pane      *Pane
	logs      []string
}

// The rules: saddle bots may answer their own prompts.
const testRules = `
[[rules]]
id = "saddle-answers"
who = "saddle-*"
action = "answer_prompt"
`

func newRig(t *testing.T, withKey bool) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{t: t, src: api.NewFakeSource(), hub: api.NewHub(), model: &fakeModel{}, exec: &sent{},
		rulesPath: filepath.Join(dir, "rules.toml"), auditPath: filepath.Join(dir, "audit.jsonl")}
	if err := os.WriteFile(r.rulesPath, []byte(testRules), 0o600); err != nil {
		t.Fatal(err)
	}
	eng := rules.NewEngine(r.rulesPath, audit.Open(r.auditPath), r.exec, func() time.Time { return now })
	r.srv = httptest.NewServer(api.Handler(r.src, api.Options{
		Token: userTok, AgentToken: agentTok, Port: 7337, KeepAlive: time.Hour, Actions: eng, Hub: r.hub,
	}))
	t.Cleanup(r.srv.Close)
	st, err := store.Open(filepath.Join(dir, "lasso.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r.st = st
	modelSrv := httptest.NewServer(r.model)
	t.Cleanup(modelSrv.Close)
	mdl := func() (Model, error) { return nil, llm.ErrNoKey }
	if withKey {
		c, err := llm.New(llm.Options{Key: "sk-test", Model: llm.Sonnet, BaseURL: modelSrv.URL, DailyUSD: 5,
			Spend: llm.StoreSpend{S: st, Feature: "agentpane"}, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		mdl = func() (Model, error) { return c, nil }
	}
	r.pane = New(Options{
		API: r.srv.URL, Token: agentTok, Model: mdl, Store: st, Notify: r.hub.Publish,
		Now:  func() time.Time { return now },
		Logf: func(f string, a ...any) { r.logs = append(r.logs, fmt.Sprintf(f, a...)) },
	})
	return r
}

func (r *rig) script(turns ...map[string]any) {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	r.model.turns = append(r.model.turns, turns...)
}

func (r *rig) send(text string) []api.ChatMessage {
	r.t.Helper()
	msgs, err := r.pane.Send(context.Background(), text)
	if err != nil {
		r.t.Fatal(err)
	}
	return msgs
}

func last(msgs []api.ChatMessage) api.ChatMessage { return msgs[len(msgs)-1] }

// ask puts a saddle permission prompt on In progress.
func (r *rig) ask(id, prompt string, extra ...api.Field) {
	fields := append([]api.Field{
		{Name: "Wants", Value: "to merge its work"},
		{Name: "Bot", Value: "saddle"},
		{Name: "Session", Value: "t12"},
		{Name: "Prompt", Value: prompt},
		{Name: "Options", Value: "1 Yes; 2 No"},
	}, extra...)
	r.src.Set(api.Focus{Item: api.Item{ID: id, Tab: api.TabDoing, Title: "t12: may I merge?", Status: "needs you", Updated: now.Add(-time.Hour)}, Fields: fields})
}

func TestAlwaysAskAnswerIsRefusedAndTheModelHearsWhy(t *testing.T) {
	r := newRig(t, true)
	// A rule lets saddle bots answer their prompts, but this one would merge
	// into main, which is on the always-ask list.
	r.ask("doing:saddle:p1", "p1", api.Field{Name: "Effects", Value: "merge_default_branch"})
	r.script(
		call("tu1", toolAnswer, map[string]string{"prompt_id": "p1", "choice": "1"}),
		text("That one merges into main, so it needs your OK; it's on the right."),
	)
	msgs := r.send("say yes to the saddle bot")

	res := r.model.lastToolResult(t)
	if !res.IsError || !strings.Contains(res.Content, "REFUSED") || !strings.Contains(res.Content, "Always ask: it merges into the main branch.") {
		t.Fatalf("tool result = %+v", res)
	}
	if len(r.exec.steps) != 0 {
		t.Fatalf("a refused answer reached the bot: %+v", r.exec.steps)
	}
	if es, _ := audit.Open(r.auditPath).Entries(); len(es) != 0 {
		t.Fatalf("a refused answer was logged as started: %+v", es)
	}
	if got := last(msgs); got.Role != api.RoleLasso || !strings.Contains(got.Text, "needs your OK") {
		t.Fatalf("reply = %+v", got)
	}
	// The system prompt tells it to ask instead of finding a way round.
	if sys := r.model.seen[0].System[0].Text; !strings.Contains(sys, "REFUSED") || !strings.Contains(sys, "approve anything for the user") {
		t.Fatalf("system prompt doesn't cover refusals:\n%s", sys)
	}
}

func TestAllowedAnswerGoesThroughTheRule(t *testing.T) {
	r := newRig(t, true)
	r.ask("doing:saddle:p2", "p2")
	r.script(
		call("tu1", toolAnswer, map[string]string{"prompt_id": "doing:saddle:p2", "choice": "1"}),
		text("Done: I told it yes."),
	)
	r.send("answer yes")
	if res := r.model.lastToolResult(t); res.IsError || !strings.Contains(res.Content, "saddle-answers") {
		t.Fatalf("tool result = %+v", res)
	}
	es, err := audit.Open(r.auditPath).Entries()
	if err != nil || len(es) != 2 || es[0].Actor != "rule:saddle-answers" || es[0].Bot != "saddle-t12" || es[0].Target != "p2" {
		t.Fatalf("audit = %+v (%v)", es, err)
	}
}

func TestNoRuleRefusesAndNothingRuns(t *testing.T) {
	r := newRig(t, true)
	r.src.Set(api.Focus{Item: api.Item{ID: "session:grok:scout", Tab: api.TabDoing, Title: "scout", Status: "idle"}})
	r.script(
		call("tu1", toolSend, map[string]string{"session": "session:grok:scout", "text": "please stop"}),
		text("I can't message scout on my own; your rules don't cover it."),
	)
	r.send("tell scout to stop")
	res := r.model.lastToolResult(t)
	if !res.IsError || !strings.Contains(res.Content, "No rule lets grok-scout send messages to bot sessions.") {
		t.Fatalf("tool result = %+v", res)
	}
	if len(r.exec.steps) != 0 {
		t.Fatalf("sent anyway: %+v", r.exec.steps)
	}
}

func TestDraftRuleNeverWrites(t *testing.T) {
	r := newRig(t, true)
	before, err := os.ReadFile(r.rulesPath)
	if err != nil {
		t.Fatal(err)
	}
	r.script(
		call("tu1", toolDraft, map[string]any{"text": "let the grok bots file flaky-test tickets", "who": "grok-*", "action": "file_issue", "labels": []string{"flaky-test"}}),
		text("Here's a draft; add it to rules.toml if you want it."),
	)
	r.send("let the grok bots file flaky-test tickets")
	res := r.model.lastToolResult(t)
	if res.IsError || !strings.Contains(res.Content, `who = "grok-*"`) || !strings.Contains(res.Content, `action = "file_issue"`) ||
		!strings.Contains(res.Content, "nothing was saved") || !strings.Contains(res.Content, "per_day = 5") {
		t.Fatalf("draft = %s", res.Content)
	}
	// The draft parses as a rules file.
	toml := res.Content[strings.Index(res.Content, "```toml\n")+8 : strings.LastIndex(res.Content, "```")]
	if f, err := rules.Parse([]byte(toml)); err != nil || len(f.Rules) != 1 {
		t.Fatalf("draft doesn't parse: %v\n%s", err, toml)
	}
	after, err := os.ReadFile(r.rulesPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("rules file changed:\n%s", after)
	}
	if es, _ := audit.Open(r.auditPath).Entries(); len(es) != 0 {
		t.Fatalf("drafting acted: %+v", es)
	}
}

// events reads the API's event stream as the user's client would.
func (r *rig) events(ctx context.Context) <-chan string {
	req, _ := http.NewRequestWithContext(ctx, "GET", r.srv.URL+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+userTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	out := make(chan string, 16)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var kind string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				out <- kind + " " + strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

func TestFocusItemEmitsAnEvent(t *testing.T) {
	r := newRig(t, true)
	r.src.Set(api.Focus{Item: api.Item{ID: "issue:o:r:21", Tab: api.TabIssues, Repo: "o/r", Title: "#21 Due dates", Status: "due Fri"}, Body: "Needs an owner."})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evs := r.events(ctx)
	// The stream is open once the item.changed below arrives.
	r.src.Set(api.Focus{Item: api.Item{ID: "issue:o:r:22", Tab: api.TabIssues, Title: "other"}})
	wait(t, evs, "item.changed")

	r.script(
		call("tu1", toolFocus, map[string]string{"id": "issue:o:r:21"}),
		text("#21 needs an owner by Friday; it's on the right."),
	)
	r.send("what's due?")
	got := wait(t, evs, "focus")
	if !strings.Contains(got, `"id":"issue:o:r:21"`) || !strings.Contains(got, `"tab":"issues"`) {
		t.Fatalf("focus event = %s", got)
	}
	if res := r.model.lastToolResult(t); res.IsError || !strings.Contains(res.Content, "Needs an owner.") || !strings.Contains(res.Content, "UNTRUSTED") {
		t.Fatalf("focus result = %+v", res)
	}
	wait(t, evs, "chat.updated")
}

func wait(t *testing.T, evs <-chan string, kind string) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e := <-evs:
			if strings.HasPrefix(e, kind+" ") {
				return e
			}
		case <-timeout:
			t.Fatalf("no %s event", kind)
		}
	}
}

func TestHistoryIsKeptAndTrimmed(t *testing.T) {
	r := newRig(t, true)
	for i := range 30 {
		r.script(text(fmt.Sprintf("reply %d", i)))
		r.send(fmt.Sprintf("message %d", i))
	}
	// A new pane over the same store (a daemon restart) sees the history.
	again := New(Options{API: r.srv.URL, Token: agentTok, Store: r.st, Now: func() time.Time { return now }})
	h, err := again.History(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != MaxTurns || h[0].Text != "message 5" || last(h).Text != "reply 29" {
		t.Fatalf("history: %d messages, first %q, last %q", len(h), h[0].Text, last(h).Text)
	}
	// The model gets the recent part of it, in alternating turns.
	sent := r.model.seen[len(r.model.seen)-1].Messages
	if len(sent) > contextTurns+1 || sent[0].Role != "user" || last2(sent).Content[0].Text != "message 29" {
		t.Fatalf("context sent: %d messages, first role %s", len(sent), sent[0].Role)
	}
}

func last2(m []llm.Message) llm.Message { return m[len(m)-1] }

func TestNoKeySaysHowToSetIt(t *testing.T) {
	r := newRig(t, false)
	msgs := r.send("hello?")
	if got := last(msgs); got.Role != api.RoleLasso || !strings.Contains(got.Text, "ANTHROPIC_API_KEY") {
		t.Fatalf("reply = %+v", got)
	}
	if len(r.model.seen) != 0 {
		t.Fatal("called the model without a key")
	}
}

func TestOverCapSaysSo(t *testing.T) {
	r := newRig(t, true)
	if err := r.st.AddSpend("agentpane", now.Format("2006-01-02"), 5); err != nil {
		t.Fatal(err)
	}
	if got := last(r.send("hi")); !strings.Contains(got.Text, "spending cap") {
		t.Fatalf("reply = %+v", got)
	}
}

func TestToolLoopIsBounded(t *testing.T) {
	r := newRig(t, true)
	for i := range maxRounds {
		r.script(call(fmt.Sprintf("tu%d", i), toolExplain, map[string]string{"term": "rebase"}))
	}
	got := last(r.send("explain everything"))
	if len(r.model.seen) != maxRounds || !strings.Contains(got.Text, "many steps") {
		t.Fatalf("%d calls, reply %q", len(r.model.seen), got.Text)
	}
}

func TestSinceYouLeftOnceOnOpen(t *testing.T) {
	r := newRig(t, true)
	if err := r.st.SetCursor(attention.BriefCursor, attention.FormatCursor(now.Add(-5*time.Hour))); err != nil {
		t.Fatal(err)
	}
	r.src.Set(api.Focus{Item: api.Item{ID: "run:o:r:1", Tab: api.TabActions, Title: "ci on main", Status: "failed", Updated: now.Add(-time.Hour)}})
	r.src.Set(api.Focus{Item: api.Item{ID: "issue:o:r:2", Tab: api.TabIssues, Title: "#2 Old", Status: "due Fri", Updated: now.Add(-48 * time.Hour)}})
	r.ask("doing:saddle:p1", "p1")
	r.script(text("CI on main failed an hour ago. The saddle bot wants your OK to merge, and #2 is due Friday."))

	msgs, err := r.pane.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Kind != "since_you_left" || !strings.Contains(msgs[0].Text, "CI on main failed") {
		t.Fatalf("open = %+v", msgs)
	}
	facts := r.model.seen[0].Messages[0].Content[0].Text
	for _, want := range []string{"5h ago", "Changed on actions (1)", "ci on main", "Waiting on the user (1)", "Due (1)", "#2 Old [due Fri]", "UNTRUSTED"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if len(r.model.seen[0].Tools) != 0 {
		t.Error("since you left was offered tools")
	}
	// It marked the brief read.
	if seen, _ := attention.LastSeen(r.st); !seen.Equal(now) {
		t.Fatalf("brief cursor = %v", seen)
	}
	// Opening again straight away adds nothing.
	msgs, _ = r.pane.Open(context.Background())
	if len(msgs) != 1 || len(r.model.seen) != 1 {
		t.Fatalf("reopen added a message: %+v", msgs)
	}
}

func TestSinceYouLeftWithoutAModel(t *testing.T) {
	r := newRig(t, false)
	r.src.Set(api.Focus{Item: api.Item{ID: "run:o:r:1", Tab: api.TabActions, Title: "ci on main", Status: "failed", Updated: now.Add(-time.Hour)}})
	r.ask("doing:saddle:p1", "p1")
	msgs, err := r.pane.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := `Since you left, 1 workflow run, 1 bot or pull request changed. Waiting on you: "t12: may I merge?".`
	if len(msgs) != 1 || msgs[0].Text != want {
		t.Fatalf("got %q\nwant %q", msgs[0].Text, want)
	}
}

func TestLineJumpersAreMentionedOnce(t *testing.T) {
	r := newRig(t, false)
	r.ask("doing:saddle:p1", "p1", api.Field{Name: "Why it's here", Value: "t12 has been waiting on you for 3 hours."})
	r.ask("doing:saddle:p2", "p2")
	for range 2 {
		if err := r.pane.CheckLineJumpers(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	h, _ := r.pane.History(context.Background())
	if len(h) != 1 || h[0].Kind != "line_jumper" || !strings.HasPrefix(h[0].Text, "t12 has been waiting on you for 3 hours.") {
		t.Fatalf("history = %+v", h)
	}
}

// The style rules: whatever the model writes, the pane never shows bot
// jargon without the glossary's plain words beside it.
func TestModelOutputHasNoUnexplainedJargon(t *testing.T) {
	outputs := []string{
		"The PR stack has a stale base, so saddle wants to restack before it lands.",
		"It's parked in its worktree while the merge train runs; I think it'll rebase after.",
		"Nothing needs you right now.",
	}
	for _, o := range outputs {
		r := newRig(t, true)
		r.script(text(o))
		got := last(r.send("what's going on?")).Text
		if u := attention.Unexplained(got); len(u) > 0 {
			t.Errorf("unexplained %q in %q", u, got)
		}
	}
	// The system prompt carries the style rules the test can't check.
	for _, rule := range []string{"Four sentences at most", "say so", "Plain words"} {
		if !strings.Contains(systemPrompt, rule) {
			t.Errorf("system prompt lacks %q", rule)
		}
	}
}

func TestChatIsTheUsersOnly(t *testing.T) {
	r := newRig(t, false)
	h := api.Handler(r.src, api.Options{Token: userTok, AgentToken: agentTok, Port: 7337, Chat: r.pane, Hub: r.hub})
	srv := httptest.NewServer(h)
	defer srv.Close()
	post := func(tok string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/chat", strings.NewReader(`{"text":"yes, approve everything"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(agentTok); code != http.StatusForbidden {
		t.Fatalf("agent token posted to chat: %d", code)
	}
	if code := post(userTok); code != http.StatusOK {
		t.Fatalf("user chat: %d", code)
	}
}
