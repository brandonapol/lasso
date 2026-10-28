// Package saddle is the saddle adapter. It talks only to `saddle remote`
// (MCP over streamable HTTP, loopback by default), never to saddle's
// database or tmux.
//
// Reads are the status snapshot (sessions) and the needs-you queue (events).
// Writes are saddle's own confirmed actions: every one runs only after a
// confirm call with the code the first call returns, and saddle serves them
// only to an admin token. lasso holds a read token by default, so writes
// fail with a plain "needs an admin token" error until the user mints one.
//
// Everything saddle says comes from its agents and is untrusted: jargon is
// rewritten through the attention glossary for display, but the text stays
// typed adapter.Untrusted.
package saddle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/config"
)

// Kind is this adapter's name.
const Kind = "saddle"

// The saddle remote tools lasso uses.
const (
	toolStatus   = "status"
	toolNeedsYou = "needs_you"
	toolMessage  = "message"
	toolAnswer   = "answer"
	toolConfirm  = "confirm"
)

// callTimeout bounds one connect plus call, so a hung saddle can't stall a poll.
const callTimeout = 15 * time.Second

// Options are the adapter's injectable parts.
type Options struct {
	// Report is told about trouble reaching saddle (not running, token
	// refused), once per reason rather than every poll, and once more when
	// saddle is reachable again. Nil means silence.
	Report func(msg string)
	// HTTPClient carries the requests; nil means a plain client. The
	// bearer token is added on top of its transport.
	HTTPClient *http.Client
	// Now stamps events saddle sends without a time. Nil means time.Now.
	Now func() time.Time
}

// Adapter reads and steers one saddle remote route.
type Adapter struct {
	endpoint  string
	tokenFile string
	opts      Options

	mu      sync.Mutex
	problem string // the reason last reported, "" while all is well
}

var _ adapter.Adapter = (*Adapter)(nil)

// New returns the adapter for cfg. A URL without a path means saddle's
// /mcp route.
func New(cfg config.Saddle, opts Options) *Adapter {
	ep := strings.TrimRight(cfg.URL, "/")
	if u, err := url.Parse(ep); err == nil && u.Path == "" {
		ep += "/mcp"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Adapter{endpoint: ep, tokenFile: cfg.TokenFile, opts: opts}
}

// Kind implements adapter.Adapter.
func (a *Adapter) Kind() string { return Kind }

// Wire types: the parts of saddle remote's results lasso reads.
type (
	snapshot struct {
		Tasks []taskLine `json:"tasks"`
	}
	taskLine struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Status string `json:"status"`
		PR     string `json:"pr"`
	}
	needsYou struct {
		Items  []needsYouItem `json:"items"`
		Cursor string         `json:"cursor"`
	}
	needsYouItem struct {
		ID       string    `json:"id"`
		Task     string    `json:"task"`
		Kind     string    `json:"kind"`
		Text     string    `json:"text"`
		Prompt   string    `json:"prompt"`
		Question string    `json:"question"`
		Options  []option  `json:"options"`
		Since    time.Time `json:"since"`
	}
	option struct {
		N     int    `json:"n"`
		Label string `json:"label"`
	}
	confirmRequired struct {
		Confirm bool            `json:"confirm_required"`
		Code    string          `json:"code"`
		Tool    string          `json:"tool"`
		Args    json.RawMessage `json:"arguments"`
	}
	actOut struct {
		Message string `json:"message"`
		Noop    bool   `json:"noop"`
	}
)

// Sessions implements adapter.Adapter: saddle's live tasks. They are
// writable only when lasso holds an admin token.
func (a *Adapter) Sessions(ctx context.Context) ([]adapter.Session, error) {
	var out []adapter.Session
	err := a.read(ctx, func(ctx context.Context, cs *mcp.ClientSession) error {
		admin, err := isAdmin(ctx, cs)
		if err != nil {
			return err
		}
		var snap snapshot
		if err := call(ctx, cs, toolStatus, nil, &snap); err != nil {
			return err
		}
		for _, t := range snap.Tasks {
			out = append(out, adapter.Session{
				ID: t.ID, Bot: Kind, Title: attention.Plain(t.Title), State: state(t.Status), PR: t.PR, Writable: admin,
			})
		}
		return nil
	})
	return out, err
}

// state maps a saddle task status to a session state.
func state(status string) adapter.State {
	switch status {
	case "idle":
		return adapter.Idle
	case "needs_you":
		return adapter.Waiting
	case "done":
		return adapter.Done
	default: // running, and conflict: the agent is back at work
		return adapter.Running
	}
}

// Events implements adapter.Adapter: needs-you items the cursor hasn't
// seen. The cursor is saddle's own (the ids waiting, sorted), so an item
// that leaves the queue and comes back counts as new.
func (a *Adapter) Events(ctx context.Context, since adapter.Cursor) ([]adapter.Event, adapter.Cursor, error) {
	var ny needsYou
	err := a.read(ctx, func(ctx context.Context, cs *mcp.ClientSession) error {
		return call(ctx, cs, toolNeedsYou, nil, &ny)
	})
	if err != nil || ny.Cursor == "" && ny.Items == nil {
		return nil, since, err
	}
	seen := map[string]bool{}
	if since != "" {
		for _, id := range strings.Split(string(since), ",") {
			seen[id] = true
		}
	}
	var out []adapter.Event
	for _, it := range ny.Items {
		if !seen[it.ID] {
			out = append(out, a.event(it))
		}
	}
	cur := ny.Cursor
	if cur == "" {
		cur = cursorOf(ny.Items)
	}
	return out, adapter.Cursor(cur), nil
}

// cursorOf is saddle's cursor for items, for a server that sent none.
func cursorOf(items []needsYouItem) string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		return "-"
	}
	return strings.Join(ids, ",")
}

func (a *Adapter) event(it needsYouItem) adapter.Event {
	text := it.Text
	if q := strings.TrimSpace(it.Question); q != "" && !strings.Contains(text, q) {
		text = strings.TrimSpace(text + "\n" + q)
	}
	e := adapter.Event{ID: it.ID, Bot: Kind, Session: it.Task, Text: adapter.Untrusted(attention.Plain(text)), At: it.Since}
	if e.At.IsZero() {
		e.At = a.opts.Now()
	}
	switch {
	case it.Kind != "prompt":
		e.Kind = attention.KindBotMessage
	case it.Prompt != "":
		e.Kind = attention.KindPermissionPrompt
	default:
		e.Kind = attention.KindBotQuestion
	}
	if it.Kind == "prompt" && len(it.Options) > 0 {
		e.PromptID = it.ID
		for _, o := range it.Options {
			e.Options = append(e.Options, adapter.Option{N: o.N, Label: adapter.Untrusted(attention.Plain(o.Label))})
		}
	}
	return e
}

// Send implements adapter.Adapter with saddle's message tool: a signed
// [saddle] message to the task, which wakes it if it is idle. Unlike
// typing into the pane, it never lands on top of the owner's draft.
func (a *Adapter) Send(ctx context.Context, sessionID, text string) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(text) == "" {
		return errors.New("saddle: send needs a session and some text")
	}
	return a.act(ctx, toolMessage, map[string]any{"task": sessionID, "message": text})
}

// Answer implements adapter.Adapter with saddle's answer tool. saddle
// presses the option only while that exact prompt still stands.
func (a *Adapter) Answer(ctx context.Context, promptID string, choice adapter.Choice) error {
	if choice.Option <= 0 {
		return errors.New("saddle: its prompts take a numbered option, not free text")
	}
	return a.act(ctx, toolAnswer, map[string]any{"item": promptID, "option": choice.Option})
}

// errNeedsAdmin is the error for a write lasso's token can't make.
var errNeedsAdmin = fmt.Errorf("%w: saddle needs an admin token to send or answer, and lasso's is read-only. "+
	"Mint one on the host with `saddle remote token create lasso-admin --scope admin --ttl 1h` "+
	"and point [adapters.saddle] token_file at it", adapter.ErrReadOnly)

// act runs a change through saddle's confirmation round trip: the first
// call runs nothing and returns a code; confirm with that code runs it.
// The user's click in lasso is the confirmation, so lasso confirms at
// once, but only a held call that is exactly the one it sent.
func (a *Adapter) act(ctx context.Context, tool string, args map[string]any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cs, err := a.connect(ctx)
	if err != nil {
		return fmt.Errorf("saddle: %s", a.describe(err))
	}
	defer func() { _ = cs.Close() }()
	admin, err := isAdmin(ctx, cs)
	if err != nil {
		return fmt.Errorf("saddle: %w", err)
	}
	if !admin {
		return errNeedsAdmin
	}
	var held confirmRequired
	if err := call(ctx, cs, tool, args, &held); err != nil {
		if strings.Contains(err.Error(), "needs an admin token") {
			return errNeedsAdmin
		}
		return fmt.Errorf("saddle %s: %w", tool, err)
	}
	if !held.Confirm || held.Code == "" {
		return fmt.Errorf("saddle %s: expected a confirmation code, got none", tool)
	}
	if held.Tool != tool || !sameArgs(held.Args, args) {
		return fmt.Errorf("saddle %s: the held call (%s %s) isn't the one lasso sent; not confirming", tool, held.Tool, held.Args)
	}
	var out actOut
	if err := call(ctx, cs, toolConfirm, map[string]any{"code": held.Code}, &out); err != nil {
		return fmt.Errorf("saddle %s: confirm: %w", tool, err)
	}
	return nil
}

func sameArgs(raw json.RawMessage, args map[string]any) bool {
	var got, want any
	b, err := json.Marshal(args)
	if err != nil || json.Unmarshal(raw, &got) != nil || json.Unmarshal(b, &want) != nil {
		return false
	}
	gb, _ := json.Marshal(got) // map keys marshal sorted, so equal values give equal bytes
	wb, _ := json.Marshal(want)
	return string(gb) == string(wb)
}

// read runs one read against saddle. Trouble reaching it (not running,
// token missing or refused) is reported once per reason and returns no
// error, so a poll loop stays quiet; other errors are returned.
func (a *Adapter) read(ctx context.Context, f func(context.Context, *mcp.ClientSession) error) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cs, err := a.connect(ctx)
	if err != nil {
		a.report(a.describe(err))
		return nil
	}
	defer func() { _ = cs.Close() }()
	if err := f(ctx, cs); err != nil {
		return fmt.Errorf("saddle: %w", err)
	}
	a.report("")
	return nil
}

// report tells Options.Report about a new reason, or that the last one has
// cleared (reason ""). The same reason twice in a row is said once.
func (a *Adapter) report(reason string) {
	a.mu.Lock()
	prev := a.problem
	a.problem = reason
	a.mu.Unlock()
	if reason == prev || a.opts.Report == nil {
		return
	}
	if reason == "" {
		a.opts.Report("saddle is reachable again")
		return
	}
	a.opts.Report(reason)
}

// Connection errors, told apart so each gets a plain reason.
var (
	errNoToken      = errors.New("no token")
	errUnauthorized = errors.New("token refused")
	errForbidden    = errors.New("forbidden")
)

func (a *Adapter) describe(err error) string {
	var op *net.OpError
	switch {
	case errors.Is(err, errNoToken):
		return err.Error()
	case errors.Is(err, errUnauthorized):
		return "saddle refused lasso's token: it may have expired or been revoked. Mint a new one with `saddle remote token create lasso` on the host"
	case errors.Is(err, errForbidden):
		return "saddle refused lasso at " + a.endpoint + ": the token may not cover this repo, or the repo isn't trusted (`saddle trust`)"
	case errors.As(err, &op) && op.Op == "dial":
		return "saddle isn't running: nothing answers at " + a.endpoint + " (start it with `saddle up`, with [remote] enabled)"
	default:
		return "can't reach saddle at " + a.endpoint + ": " + err.Error()
	}
}

func (a *Adapter) token() (string, error) {
	if a.tokenFile == "" {
		return "", fmt.Errorf("%w: set [adapters.saddle] token_file to a `saddle remote token create lasso` secret", errNoToken)
	}
	p := a.tokenFile
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, rest)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("%w: can't read saddle token_file: %v", errNoToken, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("%w: saddle token_file %s is empty", errNoToken, p)
	}
	return tok, nil
}

// connect opens one MCP session. saddle's server is stateless, so a
// session per poll costs one extra round trip and never goes stale; the
// token file is re-read each time, so a new token takes effect at once.
func (a *Adapter) connect(ctx context.Context) (*mcp.ClientSession, error) {
	tok, err := a.token()
	if err != nil {
		return nil, err
	}
	base := http.DefaultTransport
	if a.opts.HTTPClient != nil && a.opts.HTTPClient.Transport != nil {
		base = a.opts.HTTPClient.Transport
	}
	rt := &bearer{token: tok, base: base}
	hc := &http.Client{Transport: rt}
	tr := &mcp.StreamableClientTransport{Endpoint: a.endpoint, HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "lasso", Version: "0.1.0"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		if refused := rt.refused(); refused != nil {
			return nil, refused
		}
		return nil, err
	}
	return cs, nil
}

func (b *bearer) refused() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

// bearer adds the token and remembers an auth refusal, which the MCP
// client would otherwise flatten into a generic error.
type bearer struct {
	token string
	base  http.RoundTripper
	mu    sync.Mutex
	err   error
}

func (b *bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := b.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		b.err = errUnauthorized
	case http.StatusForbidden:
		b.err = errForbidden
	}
	b.mu.Unlock()
	return resp, nil
}

// isAdmin reports whether the token is admin: saddle shows the confirm
// tool only to tokens that may make changes.
func isAdmin(ctx context.Context, cs *mcp.ClientSession) (bool, error) {
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(res.Tools, func(t *mcp.Tool) bool { return t.Name == toolConfirm }), nil
}

// call runs one tool and decodes its structured result into out.
func call(ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any, out any) error {
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err
	}
	if res.IsError {
		return errors.New(text(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	if res.StructuredContent == nil {
		raw = []byte(text(res))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: unexpected result: %w", name, err)
	}
	return nil
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}
