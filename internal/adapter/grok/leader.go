package grok

// The grok leader: when the user turns on grok's leader mode
// (`[cli] use_leader = true`), their grok windows run their sessions inside
// one shared "leader" process, and any number of clients can attach to it
// over a unix socket (~/.grok/leader.sock) and speak ACP. lasso attaches as
// one more client to put a message into a session the leader owns: the
// user sees it arrive in their open window, and grok queues it itself if a
// turn is running.
//
// What was checked against grok 1.0.52 (a throwaway leader on a temp
// socket, see docs/spikes/live-sessions.md):
//
//   - The socket does not speak bare ACP. Each message is a frame: a 4-byte
//     big-endian length, then a JSON envelope. The client first sends
//     {"type":"register",…} and gets {"type":"registered",
//     "leader_protocol_version":1,…}; after that every ACP message travels
//     as {"type":"acp","payload":"<the JSON-RPC message, as a string>"}.
//     lasso speaks this framing itself rather than running
//     `grok agent --leader stdio` as a bridge, because that bridge starts a
//     new leader when none answers, and lasso must never start one.
//   - `_x.ai/sessions/list` returns the leader's roster; "resident": true
//     marks a session the leader has loaded (an attached window has it
//     open). That is what "the leader owns it" means here.
//   - `session/load` of a resident session attaches to it (history is
//     replayed); `session/prompt` queues the text (`_x.ai/queue/changed`)
//     and the turn runs in the shared session.
//   - A load that leaves out `_meta.yoloMode` switches the shared session to
//     the loading client's default permission mode (always-approve, for a
//     user whose config says so). lasso always sends yoloMode: false, which
//     was seen to leave an ask-mode session in ask mode and not to change a
//     session that is already always-approve.
//
// NOT verified end to end: a model turn. The account's usage balance was
// exhausted (402), so no turn ran to completion and no permission prompt
// was ever raised. Answering prompts through the leader is written against
// the ACP spec (session/request_permission, answered with
// {"outcome":{"outcome":"selected","optionId":…}}) and tested only with a
// fake leader. Whether the leader sends a shared session's permission
// requests to every attached client, or only to the client whose prompt is
// running, is unknown; lasso only listens while its own message's turn
// runs, so it sees at most the prompts that turn raises.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
)

// leaderProtocol is the leader framing version lasso speaks.
const leaderProtocol = 1

// maxFrame bounds one leader frame. A session load replays the whole
// history as notifications, but each is one frame.
const maxFrame = 64 << 20

// leaderSocket is the socket lasso looks for a leader on: Options.LeaderSocket,
// else $GROK_LEADER_SOCKET (grok's own override), else <home>/leader.sock.
func (a *Adapter) leaderSocket() string {
	if a.opts.LeaderSocket != "" {
		return a.opts.LeaderSocket
	}
	if s := os.Getenv("GROK_LEADER_SOCKET"); s != "" {
		return s
	}
	return filepath.Join(a.opts.Home, "leader.sock")
}

func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}

// rpcError is a JSON-RPC error.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	msg := e.Message
	var d struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(e.Data, &d) == nil && d.Message != "" {
		msg = d.Message
	} else if s := strings.Trim(string(e.Data), `"`); s != "" && s != "null" && !strings.HasPrefix(s, "{") {
		msg += ": " + s
	}
	return msg
}

// rpcMsg is any JSON-RPC 2.0 message: a request (Method and ID), a
// notification (Method only) or a response (ID with Result or Error).
type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// envelope is one leader frame.
type envelope struct {
	Type string `json:"type"`
	// register
	ClientType   string          `json:"client_type,omitempty"`
	Mode         string          `json:"mode,omitempty"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	// registered
	Ready           bool `json:"ready,omitempty"`
	ProtocolVersion int  `json:"leader_protocol_version,omitempty"`
	// acp
	Payload string `json:"payload,omitempty"`
}

// leaderConn is one ACP client connection to the leader.
type leaderConn struct {
	conn net.Conn

	wmu sync.Mutex // one frame at a time

	mu     sync.Mutex
	next   int64
	waits  map[int64]chan rpcMsg
	closed bool
	err    error

	// onNote and onRequest see the leader's notifications and requests
	// (guarded by mu). They run on the reader goroutine and must not block
	// for long.
	onNote    func(rpcMsg)
	onRequest func(rpcMsg)

	done chan struct{}
}

// dialLeader connects to the leader and registers lasso as a client. It
// returns errNoLeader when nothing is listening: leader mode is off, which
// is normal and quiet.
func (a *Adapter) dialLeader(ctx context.Context) (*leaderConn, error) {
	sock := a.leaderSocket()
	if fi, err := os.Stat(sock); err != nil || fi.Mode()&os.ModeSocket == 0 {
		return nil, errNoLeader
	}
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := a.opts.Dial(dctx, sock)
	if err != nil {
		return nil, errNoLeader // a stale socket file: no leader
	}
	c := &leaderConn{conn: conn, waits: map[int64]chan rpcMsg{}, done: make(chan struct{})}
	caps, _ := json.Marshal(map[string]any{
		// lasso never asks for a session's tools to run unasked.
		"yolo_mode": false, "auto_mode": false,
		"terminal": false, "fs_read": false, "fs_write": false,
		"status_line": false, "user_message_echo": false,
		"client_version": "lasso",
	})
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := c.write(envelope{Type: "register", ClientType: "lasso", Mode: "stdio", Capabilities: caps}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("grok leader: register: %w", err)
	}
	reg, err := readFrame(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("grok leader: register: %w", err)
	}
	if reg.Type != "registered" || !reg.Ready {
		_ = conn.Close()
		return nil, fmt.Errorf("grok leader: register: got %q, not ready", reg.Type)
	}
	if reg.ProtocolVersion != leaderProtocol {
		_ = conn.Close()
		return nil, fmt.Errorf("grok leader speaks protocol %d; lasso knows %d", reg.ProtocolVersion, leaderProtocol)
	}
	_ = conn.SetDeadline(time.Time{})
	go c.read()
	ictx, icancel := context.WithTimeout(ctx, 20*time.Second)
	defer icancel()
	_, err = c.call(ictx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]string{"name": "lasso", "version": "1"},
	})
	if err != nil {
		c.close()
		return nil, fmt.Errorf("grok leader: initialize: %w", err)
	}
	return c, nil
}

var errNoLeader = errors.New("no grok leader running")

func readFrame(r io.Reader) (envelope, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return envelope{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return envelope{}, fmt.Errorf("leader frame of %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return envelope{}, err
	}
	var e envelope
	if err := json.Unmarshal(buf, &e); err != nil {
		return envelope{}, fmt.Errorf("leader frame: %w", err)
	}
	return e, nil
}

func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

func (c *leaderConn) write(e envelope) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return writeFrame(c.conn, e)
}

func (c *leaderConn) send(m rpcMsg) error {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.write(envelope{Type: "acp", Payload: string(b)})
}

// read dispatches the leader's frames until the connection ends.
func (c *leaderConn) read() {
	var err error
	for {
		var e envelope
		e, err = readFrame(c.conn)
		if err != nil {
			break
		}
		if e.Type == "shutting_down" {
			err = errors.New("the grok leader is shutting down")
			break
		}
		if e.Type != "acp" {
			continue
		}
		var m rpcMsg
		if json.Unmarshal([]byte(e.Payload), &m) != nil {
			continue
		}
		c.mu.Lock()
		onNote, onRequest := c.onNote, c.onRequest
		c.mu.Unlock()
		switch {
		case m.Method != "" && len(m.ID) > 0:
			if onRequest != nil {
				onRequest(m)
			} else {
				_ = c.send(rpcMsg{ID: m.ID, Error: &rpcError{Code: -32601, Message: "Method not found"}})
			}
		case m.Method != "":
			if onNote != nil {
				onNote(m)
			}
		default:
			id, perr := strconv.ParseInt(string(m.ID), 10, 64)
			if perr != nil {
				continue
			}
			c.mu.Lock()
			ch := c.waits[id]
			delete(c.waits, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.closed = true
	waits := c.waits
	c.waits = map[int64]chan rpcMsg{}
	c.mu.Unlock()
	for _, ch := range waits {
		close(ch)
	}
	close(c.done)
}

// request sends a request and returns the channel its response arrives on
// (closed if the connection ends first).
func (c *leaderConn) request(method string, params any) (chan rpcMsg, error) {
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, c.lostErr()
	}
	c.next++
	id := c.next
	ch := make(chan rpcMsg, 1)
	c.waits[id] = ch
	c.mu.Unlock()
	if err := c.send(rpcMsg{ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: p}); err != nil {
		c.mu.Lock()
		delete(c.waits, id)
		c.mu.Unlock()
		return nil, err
	}
	return ch, nil
}

func (c *leaderConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch, err := c.request(method, params)
	if err != nil {
		return nil, err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, c.lostErr()
		}
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *leaderConn) lostErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil && !errors.Is(c.err, io.EOF) && !errors.Is(c.err, net.ErrClosed) {
		return fmt.Errorf("lost the grok leader: %w", c.err)
	}
	return errors.New("lost the grok leader")
}

func (c *leaderConn) close() {
	_ = c.conn.Close()
	<-c.done
}

// rosterEntry is one session in the leader's `_x.ai/sessions/list`.
type rosterEntry struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Activity  string `json:"activity"` // "dormant", "idle", "working"
	Resident  bool   `json:"resident"` // loaded in the leader: a client has it open
}

// roster returns the leader's sessions.
func (c *leaderConn) roster(ctx context.Context) (map[string]rosterEntry, error) {
	res, err := c.call(ctx, "_x.ai/sessions/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var r struct {
		Result struct {
			Sessions []rosterEntry `json:"sessions"`
		} `json:"result"`
		Sessions []rosterEntry `json:"sessions"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	out := map[string]rosterEntry{}
	for _, s := range append(r.Result.Sessions, r.Sessions...) {
		out[strings.ToLower(s.SessionID)] = s
	}
	return out, nil
}

// leaderOwner connects to the leader and reports whether it owns session
// id. ok is false, with no connection, when there is no leader or it
// doesn't own the session. A leader lasso can't talk to is reported once.
func (a *Adapter) leaderOwner(ctx context.Context, id string) (*leaderConn, rosterEntry, bool) {
	c, err := a.dialLeader(ctx)
	if err != nil {
		if !errors.Is(err, errNoLeader) {
			a.report("can't use the grok leader: " + err.Error())
		}
		return nil, rosterEntry{}, false
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ros, err := c.roster(rctx)
	if err != nil {
		c.close()
		a.report("can't list the grok leader's sessions: " + err.Error())
		return nil, rosterEntry{}, false
	}
	e, ok := ros[strings.ToLower(id)]
	if !ok || !e.Resident {
		c.close()
		return nil, rosterEntry{}, false
	}
	return c, e, true
}

// queuedWait bounds how long Send waits to see grok queue the message
// before taking the prompt as accepted.
const queuedWait = 10 * time.Second

// sendViaLeader puts text into a session the leader owns: session/load
// (which attaches lasso to it), then session/prompt. It returns once grok
// has queued the prompt or answered it. The connection stays open until
// the turn ends, so lasso can field the permission prompts it raises; the
// turn's words reach lasso through the session files, like any turn typed
// in the window.
func (a *Adapter) sendViaLeader(ctx context.Context, c *leaderConn, e rosterEntry, text string) error {
	sid := e.SessionID
	queued := make(chan struct{}, 1)
	onNote := func(m rpcMsg) {
		if m.Method != "_x.ai/queue/changed" {
			return
		}
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(m.Params, &p) == nil && p.SessionID == sid {
			select {
			case queued <- struct{}{}:
			default:
			}
		}
	}
	c.mu.Lock()
	c.onNote = onNote
	c.onRequest = func(m rpcMsg) { a.leaderRequest(c, sid, e.Cwd, m) }
	c.mu.Unlock()

	lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	_, err := c.call(lctx, "session/load", map[string]any{
		"sessionId": sid, "cwd": e.Cwd, "mcpServers": []any{},
		// Without this the load switches the shared session to lasso's
		// default permission mode. See the top of this file.
		"_meta": map[string]any{"yoloMode": false},
	})
	cancel()
	if err != nil {
		c.close()
		return fmt.Errorf("grok: the leader wouldn't load session %s: %w", sid, err)
	}
	// Drain anything a load's replay queued up before the prompt goes in.
	select {
	case <-queued:
	default:
	}
	resp, err := c.request("session/prompt", map[string]any{
		"sessionId": sid,
		"prompt":    []map[string]string{{"type": "text", "text": text}},
	})
	if err != nil {
		c.close()
		return fmt.Errorf("grok: sending through the leader: %w", err)
	}

	started := make(chan error, 1)
	tctx, tcancel := context.WithTimeout(context.WithoutCancel(ctx), a.opts.TurnTimeout)
	a.turns.Add(1)
	go func() {
		defer a.turns.Done()
		defer tcancel()
		defer a.dropLeaderPrompts(c)
		defer c.close()
		select {
		case m, ok := <-resp:
			switch {
			case !ok:
				err := c.lostErr()
				started <- fmt.Errorf("grok: %w", err)
				a.leaderNote(sid, "lasso lost contact with grok's leader before your message's turn finished ("+err.Error()+"); it may or may not have run.")
			case m.Error != nil:
				started <- nil // grok took the message; the turn failed
				a.leaderNote(sid, "grok couldn't run your message: "+m.Error.Error())
			default:
				started <- nil
			}
		case <-tctx.Done():
			started <- nil
			a.leaderNote(sid, "lasso stopped waiting for your message's turn in grok after "+a.opts.TurnTimeout.String()+".")
		}
	}()
	// Wait until grok has queued it, answered it, or a while has passed.
	t := time.NewTimer(queuedWait)
	defer t.Stop()
	select {
	case <-queued:
		return nil
	case err := <-started:
		return err
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// leaderNote queues one event about a turn lasso sent through the leader.
func (a *Adapter) leaderNote(sid, text string) {
	a.enqueue(adapter.Event{
		ID: sid + "/lasso-" + strconv.FormatInt(a.opts.Now().UnixNano(), 36), Bot: Kind, Session: sid,
		Kind: attention.KindBotMessage, At: a.opts.Now(),
		Text: adapter.Untrusted(attention.Gloss(clip(text, 500))),
	}, nil)
}

// acpOption is one choice in an ACP permission request.
type acpOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // allow_once, allow_always, reject_once, reject_always
}

// acpPermission is the params of ACP's session/request_permission.
type acpPermission struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		Kind       string          `json:"kind"`
		RawInput   json.RawMessage `json:"rawInput"`
		Locations  []struct {
			Path string `json:"path"`
		} `json:"locations"`
		Meta struct {
			Tool struct {
				Name string `json:"name"`
			} `json:"x.ai/tool"`
		} `json:"_meta"`
	} `json:"toolCall"`
	Options []acpOption `json:"options"`
}

// leaderPrompt is a permission request the leader is holding open on one
// of lasso's connections, waiting for Answer.
type leaderPrompt struct {
	conn  *leaderConn
	reqID json.RawMessage
	allow string // optionId for "allow once"
	deny  string // optionId for "reject once"
	p     *pending
}

// leaderRequest handles a request the leader sends lasso: a permission
// prompt in the session lasso's message is running in. Anything else is
// refused, since lasso offers the leader no client capabilities.
//
// UNVERIFIED against a real grok turn (see the top of this file).
func (a *Adapter) leaderRequest(c *leaderConn, sid, cwd string, m rpcMsg) {
	if m.Method != "session/request_permission" {
		_ = c.send(rpcMsg{ID: m.ID, Error: &rpcError{Code: -32601, Message: "Method not found"}})
		return
	}
	var r acpPermission
	if json.Unmarshal(m.Params, &r) != nil || r.SessionID == "" {
		_ = c.send(rpcMsg{ID: m.ID, Error: &rpcError{Code: -32602, Message: "Invalid params"}})
		return
	}
	lp := &leaderPrompt{conn: c, reqID: m.ID}
	for _, o := range r.Options {
		switch o.Kind {
		case "allow_once":
			lp.allow = o.OptionID
		case "reject_once":
			lp.deny = o.OptionID
		}
	}
	tc := r.ToolCall
	callID := first(tc.ToolCallID, strings.Trim(string(m.ID), `"`))
	tool := first(tc.Meta.Tool.Name, tc.Kind)
	lp.p = permission(r.SessionID, "acp-"+callID, cwd, call{tool: tool, input: tc.RawInput})
	if lp.p.Plain == "use "+tool || lp.p.Plain == "use a tool" {
		if t := strings.TrimSpace(tc.Title); t != "" {
			lp.p.Plain = clip(oneLine(t), 160)
		}
	}
	for _, l := range tc.Locations {
		lp.p.Paths = append(lp.p.Paths, shortPath(l.Path, cwd))
	}
	if lp.p.Rule == "" {
		lp.p.Rule = first(tc.Title, tool)
	}
	promptID := r.SessionID + "/acp-" + callID

	e := adapter.Event{
		ID: promptID, Bot: Kind, Session: r.SessionID, At: a.opts.Now(),
		Kind: attention.KindPermissionPrompt,
		Text: adapter.Untrusted(attention.Gloss("Asks permission to " + lp.p.Plain + ", in the turn lasso sent it.")),
	}
	if lp.allow != "" || lp.deny != "" {
		e.PromptID = promptID
		if lp.allow != "" {
			e.Options = append(e.Options, adapter.Option{N: 1, Label: adapter.Untrusted("Allow it this once")})
		}
		if lp.deny != "" {
			e.Options = append(e.Options, adapter.Option{N: 2, Label: adapter.Untrusted("Deny it")})
		}
		a.mu.Lock()
		a.leaderPrompts[promptID] = lp
		a.mu.Unlock()
	} else {
		e.Text += adapter.Untrusted(" lasso can't answer this one; answer it in grok.")
	}
	a.enqueue(e, nil)
}

// answerLeader answers a permission prompt the leader is holding open.
// Free text goes in after the answer, as a queued message.
//
// UNVERIFIED against a real grok turn (see the top of this file).
func (a *Adapter) answerLeader(promptID string, lp *leaderPrompt, choice adapter.Choice) error {
	var opt string
	switch choice.Option {
	case 1:
		opt = lp.allow
	case 2:
		opt = lp.deny
	default:
		return errors.New("grok: answer with option 1 (allow) or 2 (deny)")
	}
	if opt == "" {
		return fmt.Errorf("grok: this prompt has no option %d", choice.Option)
	}
	a.mu.Lock()
	if a.leaderPrompts[promptID] != lp {
		a.mu.Unlock()
		return errors.New("grok: that permission prompt is no longer waiting; the session has moved on")
	}
	delete(a.leaderPrompts, promptID)
	a.mu.Unlock()
	res, _ := json.Marshal(map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": opt}})
	if err := lp.conn.send(rpcMsg{ID: lp.reqID, Result: res}); err != nil {
		return fmt.Errorf("grok: answering through the leader: %w", err)
	}
	if t := strings.TrimSpace(choice.Text); t != "" {
		if _, err := lp.conn.request("session/prompt", map[string]any{
			"sessionId": lp.p.Session,
			"prompt":    []map[string]string{{"type": "text", "text": "The user adds: " + t}},
		}); err != nil {
			return fmt.Errorf("grok: answered, but couldn't pass on your words: %w", err)
		}
	}
	return nil
}

// dropLeaderPrompts forgets the prompts held on a closed connection: the
// turn that asked is over.
func (a *Adapter) dropLeaderPrompts(c *leaderConn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, lp := range a.leaderPrompts {
		if lp.conn == c {
			delete(a.leaderPrompts, id)
		}
	}
}

// held is a message waiting for a session to close in a grok window that
// isn't in leader mode. Resuming it headlessly while the window has it
// open would fork the conversation, so lasso waits.
type held struct {
	text string
	at   time.Time
}

// hold keeps text for session id until it can go out, and says so.
func (a *Adapter) hold(id, text string) {
	a.mu.Lock()
	a.held[id] = append(a.held[id], held{text: text, at: a.opts.Now()})
	a.mu.Unlock()
	a.leaderNote(id, "Your message is waiting until that session closes: it's open in a grok window that isn't in leader mode, and sending it now would fork the conversation. lasso sends it once the window is closed.")
}

// flushHeld sends held messages whose session can now take them: through
// the leader if it has taken the session over, else by headless resume
// once no grok window has the session open and no turn is running.
func (a *Adapter) flushHeld(ctx context.Context) {
	a.mu.Lock()
	ids := make([]string, 0, len(a.held))
	for id := range a.held {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	live := a.live()
	for _, id := range ids {
		if c, e, ok := a.leaderOwner(ctx, id); ok {
			msgs := a.takeHeld(id)
			err := a.sendViaLeader(ctx, c, e, joinHeld(msgs))
			if err != nil {
				a.leaderNote(id, "lasso couldn't send your held message through grok's leader: "+err.Error())
			}
			continue
		}
		if live[id] || a.isBusy(id) {
			continue
		}
		s, err := a.writable(ctx, id)
		if errors.Is(err, adapter.ErrReadOnly) {
			continue // a turn is running; try again next poll
		}
		if err != nil {
			msgs := a.takeHeld(id)
			a.leaderNote(id, fmt.Sprintf("lasso couldn't send %d held message(s): %v", len(msgs), err))
			continue
		}
		msgs := a.takeHeld(id)
		if err := a.start(ctx, s, joinHeld(msgs), nil, true); err != nil {
			a.leaderNote(id, "lasso couldn't send your held message: "+err.Error())
		}
	}
}

// takeHeld removes and returns session id's held messages.
func (a *Adapter) takeHeld(id string) []held {
	a.mu.Lock()
	defer a.mu.Unlock()
	msgs := a.held[id]
	delete(a.held, id)
	return msgs
}

// joinHeld makes one prompt of the messages held for a session, oldest
// first, so they go out as one turn rather than racing each other.
func joinHeld(msgs []held) string {
	if len(msgs) == 1 {
		return msgs[0].text
	}
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = m.text
	}
	return strings.Join(parts, "\n\n")
}
