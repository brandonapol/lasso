// Package claude is the Claude Code adapter. It reads Claude Code's own
// session transcripts (~/.claude/projects/<escaped cwd>/<session>.jsonl)
// and installs nothing into Claude Code.
//
// A session is live while its transcript changed in the last two minutes
// or a claude process is working in it; otherwise it is idle. lasso sends
// only to idle sessions, by resuming them headless (`claude -p --resume`)
// in their own directory. A live session is read-only: a second writer
// would fork the conversation under the user's feet.
//
// Events are compacted: one per finished assistant turn (the tools it used,
// by name, and its final words, clipped; never tool output), plus errors
// and the prompts a session is stopped at. Everything a transcript says is
// untrusted and stays typed adapter.Untrusted.
//
// Sessions saddle runs (its workers) and lasso's own are skipped: saddle
// and lasso already own those.
package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/config"
)

// Kind is this adapter's name.
const Kind = "claude"

// Defaults for Options.
const (
	// LiveWindow: a transcript written this recently belongs to a live session.
	LiveWindow = 2 * time.Minute
	// PromptQuiet: a live session whose transcript has been quiet this long
	// with a tool call still open is taken to be at a permission prompt.
	// Claude Code's shell tool times out at two minutes by default, so a
	// call open longer than that is almost always waiting on the user.
	PromptQuiet = 150 * time.Second
	// MaxAge: transcripts untouched for longer aren't listed.
	MaxAge = 7 * 24 * time.Hour
	// SendTimeout bounds one resumed run.
	SendTimeout = 15 * time.Minute
)

// recentWindow bounds what a zero cursor replays: the last turn of each
// session touched this recently.
const recentWindow = 24 * time.Hour

// Proc is one running process, as the process lister sees it.
type Proc struct {
	PID  int
	Cwd  string
	Args []string
}

// Runner runs the claude binary with args in dir and returns its stdout.
type Runner func(ctx context.Context, dir string, args ...string) ([]byte, error)

// Options are the adapter's injectable parts. The zero value is the real
// thing for the current user.
type Options struct {
	// Root is Claude Code's projects directory; "" means ~/.claude/projects.
	Root string
	// Home is the user's home directory; "" means os.UserHomeDir.
	Home string
	// SkipDirs are working directories whose sessions are lasso's own
	// (its agent pane). lasso's config and state directories are always
	// skipped.
	SkipDirs []string
	// Procs lists running processes; nil means reading /proc.
	Procs func() ([]Proc, error)
	// Run runs claude; nil means the `claude` on PATH.
	Run Runner
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// MaxAge, LiveWindow, PromptQuiet and SendTimeout override the defaults.
	MaxAge, LiveWindow, PromptQuiet, SendTimeout time.Duration
}

// Adapter reads and steers the user's Claude Code sessions.
type Adapter struct {
	opts Options
	skip []string // directory prefixes whose sessions are someone else's

	mu      sync.Mutex
	files   map[string]*file // by session id
	skipped map[string]int   // unusable lines, by type
	busy    map[string]bool  // sessions lasso is sending to right now
	replies []adapter.Event  // replies to sends, for the next Events call
}

var _ adapter.Adapter = (*Adapter)(nil)

// New returns the adapter.
func New(opts Options) *Adapter {
	home := opts.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	opts.Home = home
	if opts.Root == "" {
		opts.Root = filepath.Join(home, ".claude", "projects")
	}
	if opts.Procs == nil {
		opts.Procs = procs
	}
	if opts.Run == nil {
		opts.Run = run
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&opts.MaxAge, MaxAge)
	def(&opts.LiveWindow, LiveWindow)
	def(&opts.PromptQuiet, PromptQuiet)
	def(&opts.SendTimeout, SendTimeout)

	skip := []string{filepath.Join(home, ".cache", "saddle")}
	for _, f := range []func() (string, error){config.Dir, config.StateDir} {
		if d, err := f(); err == nil {
			skip = append(skip, d)
		}
	}
	skip = append(skip, opts.SkipDirs...)
	return &Adapter{opts: opts, skip: skip, files: map[string]*file{}, skipped: map[string]int{}, busy: map[string]bool{}}
}

// Kind implements adapter.Adapter.
func (a *Adapter) Kind() string { return Kind }

// Root is the projects directory the adapter reads.
func (a *Adapter) Root() string { return a.opts.Root }

// Skipped is how many transcript lines were skipped, by line type
// ("(unreadable)" for lines that aren't JSON). A growing count for a type
// means Claude Code's format moved on.
func (a *Adapter) Skipped() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.skipped))
	for k, v := range a.skipped {
		out[k] = v
	}
	return out
}

// foreign reports whether a session in cwd belongs to saddle or lasso.
func (a *Adapter) foreign(cwd string) bool {
	if strings.Contains(filepath.ToSlash(cwd)+"/", "/.saddle/worktrees/") {
		return true // a saddle worker's worktree
	}
	for _, d := range a.skip {
		if d != "" && (cwd == d || strings.HasPrefix(cwd, strings.TrimRight(d, string(filepath.Separator))+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// refresh brings every recent transcript up to date. Caller holds a.mu.
func (a *Adapter) refresh() error {
	paths, err := filepath.Glob(filepath.Join(a.opts.Root, "*", "*.jsonl"))
	if err != nil {
		return err
	}
	now := a.opts.Now()
	seen := map[string]bool{}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() || now.Sub(st.ModTime()) > a.opts.MaxAge {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		seen[id] = true
		f := a.files[id]
		if f == nil || f.path != p || st.Size() < f.off {
			f = newFile(id, p) // new, moved, or rewritten shorter: start over
			a.files[id] = f
		}
		f.size, f.mtime = st.Size(), st.ModTime()
		if f.off < f.size && !f.ignored {
			if err := f.parse(a.skipped, a.foreign); err != nil {
				continue // vanished mid-read; next poll sees what's left
			}
		}
	}
	for id := range a.files {
		if !seen[id] {
			delete(a.files, id)
		}
	}
	return nil
}

// view is one session's derived state at a moment.
type view struct {
	f        *file
	live     bool
	state    adapter.State
	prompt   *openTool // the prompt it's stopped at, if any
	asking   bool      // prompt is a question, not a permission prompt
	writable bool
}

// views refreshes and derives every listed session's state. Caller holds a.mu.
func (a *Adapter) views() ([]view, error) {
	if err := a.refresh(); err != nil {
		return nil, err
	}
	ps, err := a.opts.Procs()
	if err != nil {
		ps = nil // can't see processes: fall back to file times alone
	}
	byID, byCwd := liveSets(ps)
	newest := map[string]*file{} // per cwd, the session a bare `claude` there is in
	for _, f := range a.files {
		if n := newest[f.cwd]; n == nil || f.mtime.After(n.mtime) {
			newest[f.cwd] = f
		}
	}
	now := a.opts.Now()
	var out []view
	for _, f := range a.files {
		if f.cwd == "" || f.ignored {
			continue
		}
		proc := byID[f.id] || byCwd[f.cwd] && newest[f.cwd] == f
		quiet := now.Sub(f.mtime)
		v := view{f: f, live: quiet < a.opts.LiveWindow || proc || a.busy[f.id]}
		for _, o := range f.open {
			if o.name == toolAsk {
				v.prompt, v.asking = o, true
				break
			}
			// A long quiet with a call open means a permission prompt,
			// except for tools that legitimately run long.
			if proc && quiet >= a.opts.PromptQuiet && !longRunning(o.name) && (v.prompt == nil || o.at.Before(v.prompt.at)) {
				v.prompt = o
			}
		}
		if v.asking && !v.live {
			v.prompt, v.asking = nil, false // its process is gone: nobody is asking any more
		}
		switch {
		case a.busy[f.id]:
			v.state = adapter.Running
		case v.prompt != nil:
			v.state = adapter.Waiting
		case v.live && f.turnOpen():
			v.state = adapter.Running
		default:
			v.state = adapter.Idle
		}
		v.writable = !v.live && dirExists(f.cwd)
		out = append(out, v)
	}
	slices.SortFunc(out, func(x, y view) int { return y.f.mtime.Compare(x.f.mtime) })
	return out, nil
}

// longRunning tools run for minutes without a prompt: subagents write to
// their own transcripts while the parent's sits quiet.
func longRunning(name string) bool {
	return name == "Agent" || name == "Task" || strings.HasPrefix(name, "mcp__")
}

// Sessions implements adapter.Adapter: recent sessions, newest first.
// Live ones aren't writable.
func (a *Adapter) Sessions(context.Context) ([]adapter.Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	vs, err := a.views()
	if err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	out := make([]adapter.Session, 0, len(vs))
	for _, v := range vs {
		out = append(out, adapter.Session{
			ID: v.f.id, Bot: Kind, Title: v.f.title(), State: v.state, PR: v.f.pr, Writable: v.writable,
		})
	}
	return out, nil
}

// cursor is this adapter's adapter.Cursor: per session, the transcript
// offset read up to; the prompts already announced; and when it was made,
// so a session it has never seen replays only what came after.
type cursor struct {
	T int64            `json:"t"`
	O map[string]int64 `json:"o"`
	P []string         `json:"p,omitempty"`
}

// Events implements adapter.Adapter. A zero cursor means what's waiting:
// the prompts sessions are stopped at, and the last turn of each session
// touched in the past day. Replies to Send come once, with the next call,
// whatever the cursor.
func (a *Adapter) Events(_ context.Context, since adapter.Cursor) ([]adapter.Event, adapter.Cursor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	vs, err := a.views()
	if err != nil {
		return nil, since, fmt.Errorf("claude: %w", err)
	}
	var c cursor
	zero := since == "" || json.Unmarshal([]byte(since), &c) != nil
	shown := map[string]bool{}
	for _, p := range c.P {
		shown[p] = true
	}
	now := a.opts.Now()
	next := cursor{T: now.Unix(), O: map[string]int64{}}
	var out []adapter.Event
	for _, v := range vs {
		f := v.f
		next.O[f.id] = f.off
		off, known := c.O[f.id]
		switch {
		case zero:
			if len(f.events) > 0 && now.Sub(f.mtime) < recentWindow {
				out = append(out, f.events[len(f.events)-1].ev)
			}
		case known:
			for _, s := range f.events {
				if s.off > off {
					out = append(out, s.ev)
				}
			}
		default: // new to this cursor
			for _, s := range f.events {
				if s.ev.At.Unix() >= c.T {
					out = append(out, s.ev)
				}
			}
		}
		if v.prompt != nil {
			ev := promptEvent(f.id, v.prompt, v.asking, f.mtime)
			next.P = append(next.P, ev.ID)
			if !shown[ev.ID] {
				out = append(out, ev)
			}
		}
	}
	out = append(out, a.replies...)
	a.replies = nil
	slices.SortStableFunc(out, func(x, y adapter.Event) int { return x.At.Compare(y.At) })
	slices.Sort(next.P)
	b, err := json.Marshal(next)
	if err != nil {
		return nil, since, err
	}
	return out, adapter.Cursor(b), nil
}

// promptEvent is the event for a session stopped at a prompt. Claude Code's
// prompts can't be answered from outside yet, so it carries no PromptID:
// the user answers in Claude Code itself.
func promptEvent(session string, o *openTool, asking bool, fallback time.Time) adapter.Event {
	at := o.at
	if at.IsZero() {
		at = fallback
	}
	ev := adapter.Event{ID: "prompt:" + o.id, Bot: Kind, Session: session, At: at}
	if asking {
		ev.Kind = attention.KindBotQuestion
		ev.Text = adapter.Untrusted(attention.Gloss(o.question))
		for i, l := range o.options {
			ev.Options = append(ev.Options, adapter.Option{N: i + 1, Label: adapter.Untrusted(attention.Gloss(l))})
		}
		return ev
	}
	ev.Kind = attention.KindPermissionPrompt
	text := "Probably waiting for permission to use " + o.name
	if o.detail != "" {
		text += ": " + o.detail
	}
	ev.Text = adapter.Untrusted(attention.Gloss(text))
	return ev
}

// errLive is Send's error for a session that's running.
var errLive = fmt.Errorf("claude: %w: can't send while it's running; it takes messages once it has been idle for a couple of minutes", adapter.ErrReadOnly)

// Send implements adapter.Adapter for idle sessions: it resumes the
// session headless in its own directory with text as the next message,
// and the reply comes back as an event.
func (a *Adapter) Send(ctx context.Context, sessionID, text string) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(text) == "" {
		return errors.New("claude: send needs a session and some text")
	}
	a.mu.Lock()
	vs, err := a.views()
	if err != nil {
		a.mu.Unlock()
		return fmt.Errorf("claude: %w", err)
	}
	i := slices.IndexFunc(vs, func(v view) bool { return v.f.id == sessionID })
	if i < 0 {
		a.mu.Unlock()
		return fmt.Errorf("claude: no session %q", sessionID)
	}
	v := vs[i]
	if v.live {
		a.mu.Unlock()
		return errLive
	}
	if !v.writable {
		a.mu.Unlock()
		return fmt.Errorf("claude: %w: its directory %s is gone", adapter.ErrReadOnly, v.f.cwd)
	}
	f, dir := v.f, v.f.cwd
	a.busy[sessionID] = true
	f.sent[text]++ // its transcript turn is the reply event below
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, a.opts.SendTimeout)
	defer cancel()
	arg := text
	if strings.HasPrefix(arg, "-") {
		arg = " " + arg // never read as a flag
	}
	out, runErr := a.opts.Run(ctx, dir, "-p", "--resume", sessionID, arg)

	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.busy, sessionID)
	_ = a.refresh()
	f = a.files[sessionID]
	if f == nil {
		f = v.f
	}
	if runErr != nil {
		if f.sent[text] > 0 {
			f.sent[text]--
		}
		return fmt.Errorf("claude: resuming %s: %w", sessionID, runErr)
	}
	reply := strings.TrimSpace(string(out))
	if reply == "" {
		reply = "(no reply)"
	}
	a.replies = append(a.replies, f.event("reply:"+randomID(), clip(reply, clipText), a.opts.Now()))
	return nil
}

// Answer implements adapter.Adapter. Claude Code's prompts are answered in
// Claude Code itself until the live-session spike (#9) finds a way in.
func (a *Adapter) Answer(context.Context, string, adapter.Choice) error {
	return fmt.Errorf("claude: %w: answer Claude Code's prompts in its own window; lasso can't reach them yet", adapter.ErrReadOnly)
}

// run is the real Runner: the claude on PATH.
func run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := clip(stderr.String(), clipDetail); msg != "" {
			return stdout.Bytes(), fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func randomID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Cwd is the directory session id was started in, if lasso has read it.
// The daemon uses it to tell which repo a session works in.
func (a *Adapter) Cwd(id string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.files[id]
	if f == nil || f.cwd == "" {
		return "", false
	}
	return f.cwd, true
}
