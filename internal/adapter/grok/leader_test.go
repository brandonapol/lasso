package grok

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
)

// fakeLeader is a stand-in for grok's shared leader on a unix socket in a
// temp dir. It speaks the leader framing (length-prefixed JSON envelopes,
// register first, then ACP payloads) and records what clients send.
type fakeLeader struct {
	t       *testing.T
	sock    string
	ln      net.Listener
	version int

	mu        sync.Mutex
	roster    []map[string]any
	registers []envelope
	requests  []rpcMsg // client requests, in order
	answers   chan rpcMsg
	answered  []rpcMsg
	perm      bool      // raise a permission request during a prompt
	promptErr *rpcError // fail prompts with this
}

func newFakeLeader(t *testing.T) *fakeLeader {
	t.Helper()
	// Unix socket paths are short; t.TempDir() can be too long.
	dir, err := os.MkdirTemp("", "lasso-leader")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	fl := &fakeLeader{t: t, sock: filepath.Join(dir, "leader.sock"), version: 1, answers: make(chan rpcMsg, 4)}
	fl.ln, err = net.Listen("unix", fl.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fl.ln.Close() })
	go func() {
		for {
			c, err := fl.ln.Accept()
			if err != nil {
				return
			}
			go fl.serve(c)
		}
	}()
	return fl
}

func (fl *fakeLeader) own(id string, resident bool) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.roster = append(fl.roster, map[string]any{"sessionId": id, "cwd": fixCwd, "activity": "idle", "resident": resident, "yolo": false})
}

func (fl *fakeLeader) serve(c net.Conn) {
	defer c.Close()
	var wmu sync.Mutex
	write := func(e envelope) {
		wmu.Lock()
		defer wmu.Unlock()
		_ = writeFrame(c, e)
	}
	send := func(m rpcMsg) {
		m.JSONRPC = "2.0"
		b, _ := json.Marshal(m)
		write(envelope{Type: "acp", Payload: string(b)})
	}
	note := func(method string, params any) {
		p, _ := json.Marshal(params)
		send(rpcMsg{Method: method, Params: p})
	}
	reply := func(id json.RawMessage, result any) {
		r, _ := json.Marshal(result)
		send(rpcMsg{ID: id, Result: r})
	}
	reg, err := readFrame(c)
	if err != nil || reg.Type != "register" {
		return
	}
	fl.mu.Lock()
	fl.registers = append(fl.registers, reg)
	fl.mu.Unlock()
	write(envelope{Type: "registered", Ready: true, ProtocolVersion: fl.version})
	for {
		e, err := readFrame(c)
		if err != nil {
			return
		}
		var m rpcMsg
		if e.Type != "acp" || json.Unmarshal([]byte(e.Payload), &m) != nil {
			continue
		}
		if m.Method == "" { // a response: the answer to a permission request
			fl.mu.Lock()
			fl.answered = append(fl.answered, m)
			fl.mu.Unlock()
			fl.answers <- m
			continue
		}
		fl.mu.Lock()
		fl.requests = append(fl.requests, m)
		roster := slices.Clone(fl.roster)
		perm, perr := fl.perm, fl.promptErr
		fl.mu.Unlock()
		switch m.Method {
		case "initialize":
			reply(m.ID, map[string]any{"protocolVersion": 1})
		case "_x.ai/sessions/list":
			reply(m.ID, map[string]any{"result": map[string]any{"sessions": roster}})
		case "session/load":
			var p struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			// The history replay a real leader sends.
			note("session/update", map[string]any{"sessionId": p.SessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "old words"}}, "_meta": map[string]any{"isReplay": true}})
			reply(m.ID, map[string]any{})
		case "session/prompt":
			var p struct {
				SessionID string `json:"sessionId"`
				Prompt    []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(m.Params, &p)
			note("_x.ai/queue/changed", map[string]any{"sessionId": p.SessionID, "entries": []any{map[string]any{"id": "q1", "kind": "prompt", "text": p.Prompt[0].Text, "position": 0}}})
			go func(id json.RawMessage) {
				if perm {
					pp, _ := json.Marshal(map[string]any{
						"sessionId": p.SessionID,
						"toolCall": map[string]any{
							"toolCallId": "call-1", "title": "go test ./...", "kind": "execute",
							"rawInput": map[string]any{"command": "go test ./..."},
							"_meta":    map[string]any{"x.ai/tool": map[string]any{"name": "run_terminal_command"}},
						},
						"options": []any{
							map[string]any{"optionId": "yes-once", "name": "Allow", "kind": "allow_once"},
							map[string]any{"optionId": "yes-always", "name": "Always allow", "kind": "allow_always"},
							map[string]any{"optionId": "no-once", "name": "Deny", "kind": "reject_once"},
						},
					})
					send(rpcMsg{ID: json.RawMessage(`"perm-1"`), Method: "session/request_permission", Params: pp})
					select {
					case <-fl.answers:
					case <-time.After(10 * time.Second):
					}
				}
				if perr != nil {
					send(rpcMsg{ID: id, Error: perr})
					return
				}
				note("session/update", map[string]any{"sessionId": p.SessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "Done."}}})
				reply(id, map[string]any{"stopReason": "end_turn"})
			}(m.ID)
		default:
			send(rpcMsg{ID: m.ID, Error: &rpcError{Code: -32601, Message: "Method not found"}})
		}
	}
}

func (fl *fakeLeader) methods() []string {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	var out []string
	for _, r := range fl.requests {
		out = append(out, r.Method)
	}
	return out
}

func (fl *fakeLeader) request(method string) (map[string]any, bool) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for _, r := range fl.requests {
		if r.Method == method {
			var p map[string]any
			_ = json.Unmarshal(r.Params, &p)
			return p, true
		}
	}
	return nil, false
}

// ranGrok reports whether lasso ran grok for anything but listing.
func ranGrok(t *testing.T, f *fixture) []fakeCall {
	t.Helper()
	var out []fakeCall
	for _, c := range f.calls(t) {
		if c.Args[0] != "sessions" {
			out = append(out, c)
		}
	}
	return out
}

func leaderFixture(t *testing.T) (*fixture, *fakeLeader) {
	t.Helper()
	f := newFixture(t)
	fl := newFakeLeader(t)
	f.a.opts.LeaderSocket = fl.sock
	return f, fl
}

func eventsWith(t *testing.T, f *fixture, want string) []adapter.Event {
	t.Helper()
	evs, _, err := f.a.Events(context.Background(), "v1 0;")
	if err != nil {
		t.Fatal(err)
	}
	var out []adapter.Event
	for _, e := range evs {
		if strings.Contains(string(e.Text), want) {
			out = append(out, e)
		}
	}
	return out
}

// A session the leader owns gets the message through the leader: loaded
// without touching its permission mode, then prompted. grok never runs.
func TestLeaderSend(t *testing.T) {
	f, fl := leaderFixture(t)
	fl.own(sLive, true)
	if err := f.a.Send(context.Background(), sLive, "hello there"); err != nil {
		t.Fatal(err)
	}
	if !f.a.wait(10 * time.Second) {
		t.Fatal("turn never finished")
	}
	if got := fl.methods(); !slices.Equal(got, []string{"initialize", "_x.ai/sessions/list", "session/load", "session/prompt"}) {
		t.Errorf("leader saw %v", got)
	}
	load, _ := fl.request("session/load")
	if load["sessionId"] != sLive || load["cwd"] != fixCwd {
		t.Errorf("load = %v", load)
	}
	if meta, _ := load["_meta"].(map[string]any); meta["yoloMode"] != false {
		t.Errorf("load must pin yoloMode false, got _meta %v", load["_meta"])
	}
	prompt, _ := fl.request("session/prompt")
	if b, _ := json.Marshal(prompt["prompt"]); string(b) != `[{"text":"hello there","type":"text"}]` {
		t.Errorf("prompt = %s", b)
	}
	var caps map[string]any
	_ = json.Unmarshal(fl.registers[0].Capabilities, &caps)
	if caps["yolo_mode"] != false || caps["auto_mode"] != false {
		t.Errorf("register capabilities = %v", caps)
	}
	if c := ranGrok(t, f); len(c) > 0 {
		t.Errorf("grok ran: %+v", c)
	}
	// A good turn's words come from the session files; lasso adds nothing.
	if evs := eventsWith(t, f, "grok"); len(evs) > 0 {
		t.Errorf("unexpected notes: %+v", evs)
	}
}

// A session the leader owns but no window lists still goes through the
// leader: resuming it headlessly would fork it as surely as a window would.
func TestLeaderOwnsUnlistedSession(t *testing.T) {
	f, fl := leaderFixture(t)
	fl.own(sChatty, true)
	if err := f.a.Send(context.Background(), sChatty, "hi"); err != nil {
		t.Fatal(err)
	}
	f.a.wait(10 * time.Second)
	if _, ok := fl.request("session/prompt"); !ok {
		t.Error("not sent through the leader")
	}
	if c := ranGrok(t, f); len(c) > 0 {
		t.Errorf("grok ran: %+v", c)
	}
}

// When grok takes the message but the turn fails, lasso says why.
func TestLeaderPromptFails(t *testing.T) {
	f, fl := leaderFixture(t)
	fl.own(sLive, true)
	fl.promptErr = &rpcError{Code: -32603, Message: "Internal error", Data: json.RawMessage(`{"message":"API error (status 402 Payment Required): Grok Build usage balance exhausted","http_status":402}`)}
	if err := f.a.Send(context.Background(), sLive, "hi"); err != nil {
		t.Fatal(err)
	}
	f.a.wait(10 * time.Second)
	evs := eventsWith(t, f, "usage balance exhausted")
	if len(evs) != 1 || evs[0].Session != sLive || !strings.HasPrefix(string(evs[0].Text), "grok couldn't run your message") {
		t.Fatalf("events = %+v", evs)
	}
}

// A window outside leader mode: the message waits until it closes, then
// goes out by headless resume. Nothing reaches the leader.
func TestLeaderNotOwnedHoldsUntilClosed(t *testing.T) {
	f, fl := leaderFixture(t)
	fl.own(sLive, false) // on the leader's roster, but not loaded there
	if err := f.a.Send(context.Background(), sLive, "later please"); err != nil {
		t.Fatal(err)
	}
	if _, ok := fl.request("session/load"); ok {
		t.Error("loaded a session the leader doesn't own")
	}
	if evs := eventsWith(t, f, "waiting until that session closes"); len(evs) != 1 || evs[0].Session != sLive {
		t.Fatalf("no waiting note: %+v", evs)
	}
	// Still open: nothing goes out however often lasso polls.
	_, _, _ = f.a.Events(context.Background(), "v1 0;")
	f.a.wait(5 * time.Second)
	if c := ranGrok(t, f); len(c) > 0 {
		t.Fatalf("resumed while the window is open: %+v", c)
	}
	// The window closes.
	if err := os.WriteFile(filepath.Join(f.home, "active_sessions.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _ = f.a.Events(context.Background(), "v1 0;")
	if !f.a.wait(10 * time.Second) {
		t.Fatal("turn never finished")
	}
	c := ranGrok(t, f)
	if len(c) != 1 || flagValue(c[0].Args, "--resume") != sLive || c[0].Prompt != "later please" {
		t.Fatalf("calls = %+v", c)
	}
	if _, ok := fl.request("session/prompt"); ok {
		t.Error("prompted through the leader too")
	}
	// Delivered once.
	_, _, _ = f.a.Events(context.Background(), "v1 0;")
	f.a.wait(5 * time.Second)
	if c := ranGrok(t, f); len(c) != 1 {
		t.Errorf("sent %d times", len(c))
	}
}

// Messages held for one window go out together, oldest first.
func TestHeldMessagesJoin(t *testing.T) {
	f := newFixture(t) // no leader at all
	for _, m := range []string{"one", "two"} {
		if err := f.a.Send(context.Background(), sLive, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.home, "active_sessions.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _ = f.a.Events(context.Background(), "v1 0;")
	f.a.wait(10 * time.Second)
	c := ranGrok(t, f)
	if len(c) != 1 || c[0].Prompt != "one\n\ntwo" {
		t.Fatalf("calls = %+v", c)
	}
}

// A permission prompt raised in lasso's turn comes back answerable, and
// the answer goes to the leader as the request's allow-once option.
// This is tested against the ACP spec only; see leader.go.
func TestLeaderPermission(t *testing.T) {
	f, fl := leaderFixture(t)
	fl.own(sLive, true)
	fl.perm = true
	if err := f.a.Send(context.Background(), sLive, "run the tests"); err != nil {
		t.Fatal(err)
	}
	var e *adapter.Event
	deadline := time.Now().Add(10 * time.Second)
	for e == nil && time.Now().Before(deadline) {
		evs, _, _ := f.a.Events(context.Background(), "v1 0;")
		for i := range evs {
			if evs[i].Kind == attention.KindPermissionPrompt && evs[i].Session == sLive {
				e = &evs[i]
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e == nil {
		t.Fatal("no permission prompt")
	}
	if e.PromptID != sLive+"/acp-call-1" || len(e.Options) != 2 || !strings.Contains(string(e.Text), "go test ./...") {
		t.Fatalf("prompt = %+v", e)
	}
	req, err := f.a.Request(e.PromptID, adapter.Choice{Option: 1})
	if err != nil || req.Args["tool"] != "run_terminal_command" || req.Args["rule"] != "Bash(go test ./...)" || req.Args["option"] != "1" {
		t.Fatalf("request = %+v, %v", req, err)
	}
	if err := f.a.Answer(context.Background(), e.PromptID, adapter.Choice{Option: 3}); err == nil {
		t.Error("option 3 accepted")
	}
	if err := f.a.Answer(context.Background(), e.PromptID, adapter.Choice{Option: 1}); err != nil {
		t.Fatal(err)
	}
	if !f.a.wait(10 * time.Second) {
		t.Fatal("turn never finished")
	}
	// The request's own allow-once option, as an ACP "selected" outcome.
	fl.mu.Lock()
	answered := slices.Clone(fl.answered)
	fl.mu.Unlock()
	if len(answered) != 1 || string(answered[0].ID) != `"perm-1"` || string(answered[0].Result) != `{"outcome":{"optionId":"yes-once","outcome":"selected"}}` {
		t.Errorf("answers = %+v", answered)
	}
	// The turn is over: the prompt can't be answered again.
	if err := f.a.Answer(context.Background(), e.PromptID, adapter.Choice{Option: 2}); err == nil {
		t.Error("answered a prompt whose turn is over")
	}
	if c := ranGrok(t, f); len(c) > 0 {
		t.Errorf("grok ran: %+v", c)
	}
}

// A leader lasso can't speak to is reported, and lasso falls back to the
// old rules instead of failing the send.
func TestLeaderProtocolMismatch(t *testing.T) {
	f, fl := leaderFixture(t)
	fl.version = 2
	fl.own(sChatty, true)
	var said []string
	f.a.opts.Report = func(msg string) { said = append(said, msg) }
	if err := f.a.Send(context.Background(), sChatty, "hi"); err != nil {
		t.Fatal(err)
	}
	f.a.wait(10 * time.Second)
	if len(said) == 0 || !strings.Contains(said[0], "protocol 2") {
		t.Errorf("reports = %q", said)
	}
	if c := ranGrok(t, f); len(c) != 1 || flagValue(c[0].Args, "--resume") != sChatty {
		t.Errorf("calls = %+v", c)
	}
}

// A socket file with nothing behind it is no leader, and costs nothing.
func TestLeaderStaleSocket(t *testing.T) {
	f := newFixture(t)
	dir, err := os.MkdirTemp("", "lasso-leader")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "leader.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	f.a.opts.LeaderSocket = sock
	var said []string
	f.a.opts.Report = func(msg string) { said = append(said, msg) }
	if _, _, ok := f.a.leaderOwner(context.Background(), sChatty); ok {
		t.Fatal("owned by nothing")
	}
	if len(said) > 0 {
		t.Errorf("reported %q", said)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	go func() { _ = writeFrame(a, envelope{Type: "acp", Payload: `{"x":1}`}) }()
	e, err := readFrame(b)
	if err != nil || e.Type != "acp" || e.Payload != `{"x":1}` {
		t.Fatalf("%+v, %v", e, err)
	}
	go func() { _, _ = a.Write([]byte{0xff, 0xff, 0xff, 0xff}) }()
	if _, err := readFrame(b); err == nil {
		t.Error("oversized frame read")
	}
	_ = a.Close()
	if _, err := readFrame(b); !errors.Is(err, os.ErrClosed) && err == nil {
		t.Error("read from a closed pipe")
	}
}
