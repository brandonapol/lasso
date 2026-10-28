package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/rules"
)

// The fixture sessions under testdata/home (synthetic, shaped like real
// grok 1.0 session files).
const (
	sChatty   = "01a00000-0000-7000-8000-00000000000a" // two finished turns' worth of chatter
	sWaiting  = "01a00000-0000-7000-8000-00000000000b" // stopped at a permission prompt
	sSubagent = "01a00000-0000-7000-8000-00000000000c" // a subagent: left out
	sRunning  = "01a00000-0000-7000-8000-00000000000d" // mid-turn, headless
	sLive     = "01a00000-0000-7000-8000-00000000000e" // open in a grok TUI
	sListed   = "01a00000-0000-7000-8000-00000000000f" // only in `grok sessions list`
	sFresh    = "01a00000-0000-7000-8000-000000000099" // the fake's fresh session
	fixCwd    = "/tmp/lasso-fixture/proj"
	groupDir  = "%2Ftmp%2Flasso-fixture%2Fproj"
)

// TestMain lets the test binary stand in for grok: with LASSO_FAKE_GROK
// set, it behaves as a scripted grok instead of running tests.
func TestMain(m *testing.M) {
	if os.Getenv("LASSO_FAKE_GROK") != "" {
		os.Exit(fakeGrok(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeGrok logs each call to $FAKE_GROK_LOG (args and prompt text, one
// JSON line) and answers in grok's shapes.
func fakeGrok(args []string) int {
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	prompt := ""
	if p := flag("--prompt-file"); p != "" {
		b, _ := os.ReadFile(p)
		prompt = string(b)
	}
	if log := os.Getenv("FAKE_GROK_LOG"); log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			b, _ := json.Marshal(fakeCall{Args: args, Prompt: prompt})
			_, _ = f.Write(append(b, '\n'))
			_ = f.Close()
		}
	}
	out := func(v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	if len(args) > 0 && args[0] == "sessions" {
		fmt.Println("(no label)")
		fmt.Println("SESSION ID                            CREATED     UPDATED     STATUS      SUMMARY")
		fmt.Println(sListed + "  2026-10-08  2026-10-08  local  Closed stale branches")
		fmt.Println(sChatty + "  2026-10-27  2026-10-27  local  Fix the flaky parser test")
		return 0
	}
	sid := flag("--resume")
	if sid == sListed {
		out(map[string]any{"type": "error", "message": "Couldn't start session: no session " + sid})
		fmt.Fprintln(os.Stderr, "error: session not found")
		return 1
	}
	if sid == "" {
		sid = sFresh
	}
	out(map[string]any{"type": "available_commands", "tools": []string{"read_file"}})
	out(map[string]any{"type": "thought", "data": "SECRET THOUGHT"})
	switch {
	case flag("--allow") != "":
		out(map[string]any{"type": "tool_call", "toolCallId": "c1", "toolName": "run_terminal_command", "kind": "execute", "status": "in_progress", "rawInput": map[string]any{"command": "go test ./..."}})
		out(map[string]any{"type": "tool_call_update", "toolCallId": "c1", "status": "completed"})
		out(map[string]any{"type": "text", "data": "Tests pass."})
	case flag("--deny") != "":
		out(map[string]any{"type": "text", "data": "OK, skipping the tests."})
	case strings.Contains(prompt, "PLEASE_PROMPT"):
		out(map[string]any{"type": "tool_call", "toolCallId": "c1", "toolName": "run_terminal_command", "kind": "execute", "status": "in_progress", "rawInput": map[string]any{"command": "rm -rf build"}})
		out(map[string]any{"type": "tool_call_update", "toolCallId": "c1", "status": "failed", "content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "Failed to request permission from user"}}}})
		out(map[string]any{"type": "end", "stopReason": "cancelled", "sessionId": sid, "requestId": "req-perm"})
		return 0
	default:
		out(map[string]any{"type": "text", "data": "Looking. "})
		out(map[string]any{"type": "tool_call", "toolCallId": "c1", "toolName": "search_replace", "kind": "edit", "status": "in_progress", "rawInput": map[string]any{"file_path": "main.go"}, "locations": []any{map[string]any{"path": fixCwd + "/main.go"}}})
		out(map[string]any{"type": "tool_call_update", "toolCallId": "c1", "status": "completed"})
		out(map[string]any{"type": "text", "data": "Hello from grok."})
	}
	out(map[string]any{"type": "end", "stopReason": "end_turn", "sessionId": sid, "requestId": "req-" + strings.TrimPrefix(flag("--allow")+flag("--deny"), "Bash")})
	return 0
}

type fakeCall struct {
	Args   []string `json:"args"`
	Prompt string   `json:"prompt"`
}

type fixture struct {
	home string
	log  string
	a    *Adapter
}

// newFixture copies the fixture grok home into a temp dir (fresh mtimes,
// safe to append to) and points the adapter at it and the fake grok.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := filepath.Join(t.TempDir(), "grok")
	err := filepath.WalkDir("testdata/home", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(home, strings.TrimPrefix(p, "testdata/home"))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	active, _ := json.Marshal([]map[string]any{
		{"session_id": sLive, "pid": os.Getpid(), "pid_start": 1, "cwd": fixCwd, "opened_at": "2026-10-27T15:00:00Z"},
		{"session_id": sChatty, "pid": 0x7ffffff0, "pid_start": 1, "cwd": fixCwd, "opened_at": "2026-10-27T15:00:00Z"}, // a dead TUI
	})
	if err := os.WriteFile(filepath.Join(home, "active_sessions.json"), active, 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls.jsonl")
	a := New(Options{
		Home: home,
		Grok: exe,
		Env:  []string{"LASSO_FAKE_GROK=1", "FAKE_GROK_LOG=" + log},
		Dir:  t.TempDir(),
	})
	return &fixture{home: home, log: log, a: a}
}

func (f *fixture) updates(id string) string {
	return filepath.Join(f.home, "sessions", groupDir, id, "updates.jsonl")
}

func (f *fixture) appendLines(t *testing.T, id string, lines ...string) {
	t.Helper()
	fh, err := os.OpenFile(f.updates(id), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	for _, l := range lines {
		if _, err := fh.WriteString(l); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fixture) calls(t *testing.T) []fakeCall {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []fakeCall
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var c fakeCall
		if err := json.Unmarshal([]byte(l), &c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

// Lines in grok's updates.jsonl shape, for appending turns.
func ln(sid, prompt string, ts int64, update string) string {
	meta := fmt.Sprintf(`{"agentTimestampMs":%d`, ts*1000)
	if prompt != "" {
		meta += fmt.Sprintf(`,"promptId":%q`, prompt)
	}
	return fmt.Sprintf(`{"timestamp":%d,"method":"session/update","params":{"sessionId":%q,"update":%s,"_meta":%s}}}`+"\n", ts, sid, update, meta)
}

func lnEnd(sid, prompt string, ts int64) string {
	return fmt.Sprintf(`{"timestamp":%d,"method":"_x.ai/session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"turn_completed","prompt_id":%q,"stop_reason":"end_turn"},"_meta":{"agentTimestampMs":%d}}}`+"\n", ts, sid, prompt, ts*1000)
}

func lnText(sid, prompt string, ts int64, text string) string {
	return ln(sid, prompt, ts, fmt.Sprintf(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":%q}}`, text))
}

func TestSessions(t *testing.T) {
	f := newFixture(t)
	got, err := f.a.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]adapter.Session{}
	for _, s := range got {
		by[s.ID] = s
		if s.Bot != Kind {
			t.Errorf("%s: bot %q", s.ID, s.Bot)
		}
	}
	if _, ok := by[sSubagent]; ok {
		t.Error("subagent session listed")
	}
	want := map[string]struct {
		state    adapter.State
		writable bool
	}{
		sChatty:  {adapter.Idle, true}, // its TUI entry has a dead pid
		sWaiting: {adapter.Waiting, true},
		sRunning: {adapter.Running, false},
		sLive:    {adapter.Running, true}, // a message waits for the window, or goes through the leader
		sListed:  {adapter.Idle, true},
	}
	for id, w := range want {
		s, ok := by[id]
		if !ok {
			t.Errorf("%s missing", id)
			continue
		}
		if s.State != w.state || s.Writable != w.writable {
			t.Errorf("%s: state %s writable %v, want %s %v", id, s.State, s.Writable, w.state, w.writable)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d sessions, want %d", len(got), len(want))
	}
	if by[sChatty].Title != "Fix the flaky parser test" || by[sListed].Title != "Closed stale branches" {
		t.Errorf("titles: %q, %q", by[sChatty].Title, by[sListed].Title)
	}
}

func TestSessionsLeaveOutOldOnes(t *testing.T) {
	f := newFixture(t)
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(f.updates(sChatty), old, old); err != nil {
		t.Fatal(err)
	}
	got, err := f.a.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got {
		if s.ID == sChatty {
			t.Error("a session quiet for three days is listed")
		}
	}
}

func TestParseList(t *testing.T) {
	out := "(no label)\nSESSION ID                            CREATED     UPDATED     STATUS      SUMMARY\n" +
		"01A11B8B-D691-7350-98FD-51D50C1DFB76  2026-10-08  2026-10-08  local  Closed stale  branches\nNo sessions found.\n"
	rows := parseList([]byte(out))
	if len(rows) != 1 || rows[0].ID != "01a11b8b-d691-7350-98fd-51d50c1dfb76" || rows[0].Status != "local" || rows[0].Summary != "Closed stale  branches" {
		t.Fatalf("rows = %+v", rows)
	}
}

// The zero cursor hands over only what's waiting: the permission prompt.
func TestEventsZeroCursorIsWhatsWaiting(t *testing.T) {
	f := newFixture(t)
	evs, cur, err := f.a.Events(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want the one prompt: %+v", len(evs), evs)
	}
	e := evs[0]
	if e.Kind != attention.KindPermissionPrompt || e.Session != sWaiting || e.PromptID != sWaiting+"/pb-1" {
		t.Fatalf("event = %+v", e)
	}
	if !strings.Contains(string(e.Text), "run `go test ./...`") {
		t.Errorf("text = %q", e.Text)
	}
	if strings.Contains(string(e.Text), "Ignore previous instructions") {
		t.Errorf("the bot's own description leaked into the prompt: %q", e.Text)
	}
	if len(e.Options) != 2 || !strings.Contains(string(e.Options[0].Label), "Bash(go test ./...)") {
		t.Errorf("options = %+v", e.Options)
	}
	if !e.Item().Signals.BlockedSince.Equal(e.At) {
		t.Error("a permission prompt should count as blocking")
	}
	// Nothing new: nothing again.
	evs, _, err = f.a.Events(context.Background(), cur)
	if err != nil || len(evs) != 0 {
		t.Fatalf("second poll: %v, %+v", err, evs)
	}
}

// Each finished turn is one compact delta; an unfinished one waits.
func TestEventsTurnDeltas(t *testing.T) {
	f := newFixture(t)
	_, cur, err := f.a.Events(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	// Half of a new turn in the chatty session.
	f.appendLines(t, sChatty,
		ln(sChatty, "", 1793200000, `{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Now the lexer."}}`),
		lnText(sChatty, "pa-2", 1793200001, "Reading the lexer."),
		ln(sChatty, "pa-2", 1793200002, `{"sessionUpdate":"tool_call","toolCallId":"x1","rawInput":{"file_path":"`+fixCwd+`/lexer.go"},"_meta":{"x.ai/tool":{"name":"search_replace","kind":"edit"}}}`),
	)
	evs, cur, err := f.a.Events(context.Background(), cur)
	if err != nil || len(evs) != 0 {
		t.Fatalf("mid-turn: %v, %+v", err, evs)
	}
	var lines []string
	for i := range 40 {
		lines = append(lines, ln(sChatty, "pa-2", 1793200003, fmt.Sprintf(`{"sessionUpdate":"tool_call","toolCallId":"r%d","rawInput":{"target_file":"f%d.go"},"_meta":{"x.ai/tool":{"name":"read_file","kind":"read"}}}`, i, i)))
	}
	lines = append(lines, ln(sChatty, "pa-2", 1793200004, `{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"SECRET THOUGHT"}}`))
	lines = append(lines, lnText(sChatty, "pa-2", 1793200005, strings.Repeat("Done with the lexer. ", 60)))
	lines = append(lines, lnEnd(sChatty, "pa-2", 1793200006))
	f.appendLines(t, sChatty, lines...)

	evs, cur, err = f.a.Events(context.Background(), cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	e := evs[0]
	text := string(e.Text)
	if e.ID != sChatty+"/pa-2" || e.Kind != attention.KindBotMessage || e.PromptID != "" {
		t.Errorf("event = %+v", e)
	}
	for _, want := range []string{"Finished a turn.", "read_file ×40", "search_replace", "Changed: lexer.go.", "Said: Done with the lexer."} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q: %q", want, text)
		}
	}
	for _, not := range []string{"SECRET", "Reading the lexer"} {
		if strings.Contains(text, not) {
			t.Errorf("text has %q: %q", not, text)
		}
	}
	if len(text) > 600 {
		t.Errorf("delta is %d bytes, want it compact", len(text))
	}
	if !e.At.Equal(time.Unix(1793200006, 0)) {
		t.Errorf("at = %v", e.At)
	}

	// The running session finishes its turn.
	f.appendLines(t, sRunning, lnText(sRunning, "pd-1", 1793200010, "Loader split in two."), lnEnd(sRunning, "pd-1", 1793200011))
	evs, _, err = f.a.Events(context.Background(), cur)
	if err != nil || len(evs) != 1 {
		t.Fatalf("running session's turn: %v, %+v", err, evs)
	}
	if got := string(evs[0].Text); !strings.Contains(got, "write") || !strings.Contains(got, "Changed: config/load.go.") {
		t.Errorf("text = %q", got)
	}
}

// A resumed cursor works across adapters (lasso restarted), and a session
// the cursor didn't track gives only the turns after the cursor's time.
func TestEventsCursorAcrossRestart(t *testing.T) {
	f := newFixture(t)
	_, cur, err := f.a.Events(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	pc, ok := parseCursor(cur)
	if !ok || pc.off[sChatty] == 0 || pc.String() != string(cur) {
		t.Fatalf("cursor %q", cur)
	}
	// A new session appears, with one old turn and one new one.
	dir := filepath.Join(f.home, "sessions", groupDir, sFresh)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	body := lnText(sFresh, "old", now-3600, "old news") + lnEnd(sFresh, "old", now-3600) +
		lnText(sFresh, "new", now+5, "new news") + lnEnd(sFresh, "new", now+5)
	if err := os.WriteFile(filepath.Join(dir, "updates.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f.appendLines(t, sChatty, lnText(sChatty, "pa-3", now+6, "more"), lnEnd(sChatty, "pa-3", now+6))

	b := New(f.a.opts)
	evs, _, err := b.Events(context.Background(), cur)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range evs {
		ids = append(ids, e.ID)
	}
	slices.Sort(ids)
	if want := []string{sChatty + "/pa-3", sFresh + "/new"}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if evs[0].Text == "" || !strings.Contains(string(evs[0].Text)+string(evs[1].Text), "new news") {
		t.Errorf("events = %+v", evs)
	}
}

func TestSendResumes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, cur, _ := f.a.Events(ctx, "")
	if err := f.a.Send(ctx, sChatty, "Please also fix the lexer."); err != nil {
		t.Fatal(err)
	}
	if !f.a.wait(10 * time.Second) {
		t.Fatal("turn didn't finish")
	}
	calls := f.calls(t)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	args := calls[0].Args
	if args[0] != "--resume" || args[1] != sChatty || !slices.Contains(args, "--prompt-file") || !hasPair(args, "--output-format", "streaming-json") {
		t.Errorf("args = %v", args)
	}
	if calls[0].Prompt != "Please also fix the lexer." {
		t.Errorf("prompt = %q", calls[0].Prompt)
	}
	if p := flagValue(args, "--prompt-file"); fileExists(p) {
		t.Errorf("prompt file %s left behind", p)
	}
	evs, cur, err := f.a.Events(ctx, cur)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events: %v %+v", err, evs)
	}
	e := evs[0]
	if e.ID != sChatty+"/req-" || e.Session != sChatty || !strings.Contains(string(e.Text), "Said: Hello from grok.") || !strings.Contains(string(e.Text), "Changed: main.go.") || strings.Contains(string(e.Text), "SECRET") {
		t.Errorf("event = %+v", e)
	}
	// grok writes the same turn to its file; it isn't delivered twice.
	f.appendLines(t, sChatty, lnText(sChatty, "req-", time.Now().Unix()+5, "Hello from grok."), lnEnd(sChatty, "req-", time.Now().Unix()+5))
	evs, _, _ = f.a.Events(ctx, cur)
	if len(evs) != 0 {
		t.Errorf("turn delivered twice: %+v", evs)
	}
}

func TestSendRetriesFresh(t *testing.T) {
	f := newFixture(t)
	if err := f.a.Send(context.Background(), sListed, "hello"); err != nil {
		t.Fatal(err)
	}
	f.a.wait(10 * time.Second)
	calls := f.calls(t)
	var turns []fakeCall
	for _, c := range calls {
		if c.Args[0] != "sessions" {
			turns = append(turns, c)
		}
	}
	if len(turns) != 2 || turns[0].Args[1] != sListed || slices.Contains(turns[1].Args, "--resume") || turns[1].Prompt != "hello" {
		t.Fatalf("calls = %+v", turns)
	}
	evs, _, _ := f.a.Events(context.Background(), "v1 0;")
	var e *adapter.Event
	for i := range evs {
		if evs[i].Session == sFresh {
			e = &evs[i]
		}
	}
	if e == nil || !strings.Contains(string(e.Text), "Couldn't resume grok session "+sListed) || !strings.Contains(string(e.Text), "Hello from grok.") {
		t.Fatalf("events = %+v", evs)
	}
}

func TestSendReadOnly(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{sRunning} {
		err := f.a.Send(context.Background(), id, "hi")
		if !errors.Is(err, adapter.ErrReadOnly) {
			t.Errorf("%s: err = %v, want read-only", id, err)
		}
	}
	if err := f.a.Send(context.Background(), "nope", "hi"); err == nil || errors.Is(err, adapter.ErrReadOnly) {
		t.Errorf("unknown session: err = %v", err)
	}
	if calls := f.calls(t); slices.ContainsFunc(calls, func(c fakeCall) bool { return c.Args[0] != "sessions" }) {
		t.Errorf("grok ran: %+v", calls)
	}
}

// A turn lasso runs that stops at a prompt comes back answerable.
func TestSendThenPrompt(t *testing.T) {
	f := newFixture(t)
	if err := f.a.Send(context.Background(), sChatty, "PLEASE_PROMPT"); err != nil {
		t.Fatal(err)
	}
	f.a.wait(10 * time.Second)
	evs, _, _ := f.a.Events(context.Background(), "v1 0;")
	var e *adapter.Event
	for i := range evs {
		if evs[i].ID == sChatty+"/req-perm" {
			e = &evs[i]
		}
	}
	if e == nil || e.Kind != attention.KindPermissionPrompt || e.PromptID != sChatty+"/req-perm" {
		t.Fatalf("events = %+v", evs)
	}
	req, err := f.a.Request(e.PromptID, adapter.Choice{Option: 2})
	if err != nil || req.Args["rule"] != "Bash(rm -rf build)" {
		t.Fatalf("request = %+v, %v", req, err)
	}
}

func TestAnswerAllowAndDeny(t *testing.T) {
	for _, tc := range []struct {
		option int
		flag   string
		said   string
	}{{1, "--allow", "Tests pass."}, {2, "--deny", "OK, skipping the tests."}} {
		t.Run(tc.flag, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			evs, cur, _ := f.a.Events(ctx, "")
			if len(evs) != 1 {
				t.Fatalf("events = %+v", evs)
			}
			if err := f.a.Answer(ctx, evs[0].PromptID, adapter.Choice{Option: tc.option, Text: "be quick"}); err != nil {
				t.Fatal(err)
			}
			f.a.wait(10 * time.Second)
			calls := f.calls(t)
			if len(calls) != 1 {
				t.Fatalf("calls = %+v", calls)
			}
			c := calls[0]
			if !hasPair(c.Args, "--resume", sWaiting) || !hasPair(c.Args, tc.flag, "Bash(go test ./...)") {
				t.Errorf("args = %v", c.Args)
			}
			if !strings.Contains(c.Prompt, "Bash(go test ./...)") || !strings.Contains(c.Prompt, "The user adds: be quick") {
				t.Errorf("prompt = %q", c.Prompt)
			}
			evs, _, _ = f.a.Events(ctx, cur)
			if len(evs) != 1 || !strings.Contains(string(evs[0].Text), tc.said) {
				t.Errorf("events = %+v", evs)
			}
		})
	}
}

// After a restart the adapter finds the prompt in grok's file; once the
// session has moved on, the prompt can't be answered.
func TestAnswerAfterRestartAndStale(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := sWaiting + "/pb-1"
	if err := f.a.Answer(ctx, id, adapter.Choice{Option: 3}); err == nil {
		t.Error("option 3 accepted")
	}
	req, err := f.a.Request(id, adapter.Choice{Option: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := rules.Request{Bot: Kind, Action: rules.AnswerPrompt, Target: id, Args: map[string]string{"tool": "run_terminal_command", "rule": "Bash(go test ./...)", "option": "1"}}
	if fmt.Sprint(req) != fmt.Sprint(want) {
		t.Errorf("request = %+v", req)
	}

	f.appendLines(t, sWaiting, lnText(sWaiting, "pb-2", 1793300000, "moved on"), lnEnd(sWaiting, "pb-2", 1793300001))
	err = New(f.a.opts).Answer(ctx, id, adapter.Choice{Option: 1})
	if err == nil || !strings.Contains(err.Error(), "no longer waiting") {
		t.Errorf("stale prompt: err = %v", err)
	}
	if err := f.a.Answer(ctx, sChatty+"/pa-1", adapter.Choice{Option: 1}); err == nil {
		t.Error("answered a turn that never asked")
	}
}

func TestPermissionRules(t *testing.T) {
	for _, tc := range []struct {
		tool, input, rule string
	}{
		{"run_terminal_command", `{"command":"go test ./..."}`, "Bash(go test ./...)"},
		{"search_replace", `{"file_path":"a/b.go"}`, "Edit(/w/a/b.go)"},
		{"write", `{"file_path":"/abs/x"}`, "Edit(/abs/x)"},
		{"read_file", `{"target_file":"/etc/hosts"}`, "Read(/etc/hosts)"},
		{"web_fetch", `{"url":"https://example.com/x"}`, "WebFetch(https://example.com/x)"},
		{"use_tool", `{"tool_name":"linear__get_issue"}`, "MCPTool(linear__get_issue)"},
		{"spawn_subagent", `{}`, ""},
	} {
		p := permission("s", "p", "/w", call{tool: tc.tool, input: json.RawMessage(tc.input)})
		if p.Rule != tc.rule {
			t.Errorf("%s: rule %q, want %q", tc.tool, p.Rule, tc.rule)
		}
	}
}

// A prompt lasso can't turn into a rule still shows, but can't be answered.
func TestUnanswerablePrompt(t *testing.T) {
	tr := newTurn("p", 0)
	tr.apply(update{kind: toolCall, callID: "c", tool: "spawn_subagent"})
	tr.apply(update{kind: toolUpdate, callID: "c", status: "failed", text: "Failed to request permission"})
	tr.apply(update{kind: turnEnd, stop: "cancelled"})
	e, p := tr.event("s", "/w")
	if e.Kind != attention.KindPermissionPrompt || e.PromptID != "" || p != nil || !strings.Contains(string(e.Text), "answer it in grok") {
		t.Errorf("event = %+v, pending = %+v", e, p)
	}
}

func hasPair(args []string, k, v string) bool {
	for i := range len(args) - 1 {
		if args[i] == k && args[i+1] == v {
			return true
		}
	}
	return false
}

func flagValue(args []string, k string) string {
	for i := range len(args) - 1 {
		if args[i] == k {
			return args[i+1]
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
