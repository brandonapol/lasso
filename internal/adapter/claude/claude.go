// Package claude is the Claude Code adapter. It reads Claude Code's own
// session transcripts (~/.claude/projects/<escaped cwd>/<session>.jsonl)
// and the files Claude Code keeps for its running sessions
// (~/.claude/sessions/<pid>.json), and installs nothing into Claude Code.
//
// A session is open while Claude Code's sessions directory lists it, and
// it is busy or idle as that file says. With an older Claude Code that
// writes no such files, lasso falls back to a guess: open while its
// transcript changed in the last two minutes or a claude process is
// working in it.
//
// Sending:
//   - To a closed session, lasso resumes it headless (`claude -p --resume`)
//     in its own directory.
//   - To an open one, lasso passes the message on through its relay, a
//     headless Claude session of its own (see relay.go), using Claude
//     Code's messaging between sessions.
//   - When the relay can't reach an open session, the message waits until
//     the session's process exits, then goes out by headless resume.
//     lasso never resumes a session a terminal still has open: a second
//     writer would fork the conversation under the user's feet.
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
	// LiveWindow: without Claude Code's sessions files, a transcript written
	// this recently belongs to a live session.
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
	// RelayTimeout bounds the relay starting, and making its call.
	RelayTimeout = 90 * time.Second
	// NoticeWait is how long a send waits after the relay's call for
	// Claude Code to say the receiver held or refused it.
	NoticeWait = 3 * time.Second
	// VerifyWait is how long a send watches the receiver's transcript for
	// the message to land. A busy session takes it in later; lasso keeps
	// checking for VerifyFor.
	VerifyWait = 5 * time.Second
	VerifyFor  = time.Hour
	// RelayIdle: the relay stops after this long unused.
	RelayIdle = 15 * time.Minute
	// RelayModel is the model the relay runs: it only copies text.
	RelayModel = "haiku"
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
	// SessionsDir is where Claude Code lists its running sessions; ""
	// means "sessions" beside Root.
	SessionsDir string
	// Home is the user's home directory; "" means os.UserHomeDir.
	Home string
	// SkipDirs are working directories whose sessions are lasso's own
	// (its agent pane). lasso's config and state directories are always
	// skipped.
	SkipDirs []string
	// Procs lists running processes; nil means reading /proc.
	Procs func() ([]Proc, error)
	// Alive reports whether a pid is running; nil means asking the OS.
	Alive func(pid int) bool
	// Run runs claude; nil means the `claude` on PATH.
	Run Runner
	// StartRelay starts the relay session; nil means the `claude` on PATH.
	StartRelay RelayStarter
	// RelayDir is the relay's working directory; "" means
	// <lasso's state dir>/claude-relay.
	RelayDir string
	// RelayModel is the relay's model; "" means RelayModel.
	RelayModel string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Durations override the defaults above.
	MaxAge, LiveWindow, PromptQuiet, SendTimeout               time.Duration
	RelayTimeout, NoticeWait, VerifyWait, VerifyFor, RelayIdle time.Duration
}

// Adapter reads and steers the user's Claude Code sessions.
type Adapter struct {
	opts  Options
	skip  []string // directory prefixes whose sessions are someone else's
	relay *relay

	mu       sync.Mutex
	files    map[string]*file // by session id
	skipped  map[string]int   // unusable lines, by type
	busy     map[string]bool  // sessions lasso is resuming right now
	draining map[string]bool  // sessions whose queued messages are going out
	queued   []queued         // messages waiting for their session to close
	checks   []check          // relayed messages not yet seen landing
	lastSent string           // the session the relay last sent to
	replies  []adapter.Event  // lasso's own notes and replies, for the next Events call
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
	if opts.SessionsDir == "" {
		opts.SessionsDir = filepath.Join(filepath.Dir(opts.Root), "sessions")
	}
	if opts.Procs == nil {
		opts.Procs = procs
	}
	if opts.Alive == nil {
		opts.Alive = pidAlive
	}
	if opts.Run == nil {
		opts.Run = run
	}
	if opts.StartRelay == nil {
		opts.StartRelay = startRelay
	}
	if opts.RelayModel == "" {
		opts.RelayModel = RelayModel
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
	def(&opts.RelayTimeout, RelayTimeout)
	def(&opts.NoticeWait, NoticeWait)
	def(&opts.VerifyWait, VerifyWait)
	def(&opts.VerifyFor, VerifyFor)
	def(&opts.RelayIdle, RelayIdle)

	skip := []string{filepath.Join(home, ".cache", "saddle")}
	state := ""
	for i, f := range []func() (string, error){config.Dir, config.StateDir} {
		if d, err := f(); err == nil {
			skip = append(skip, d)
			if i == 1 {
				state = d
			}
		}
	}
	if opts.RelayDir == "" {
		if state == "" {
			state = filepath.Join(os.TempDir(), "lasso")
		}
		opts.RelayDir = filepath.Join(state, "claude-relay")
	}
	skip = append(skip, opts.RelayDir)
	skip = append(skip, opts.SkipDirs...)
	a := &Adapter{
		opts: opts, skip: skip,
		files: map[string]*file{}, skipped: map[string]int{}, busy: map[string]bool{}, draining: map[string]bool{},
	}
	a.relay = &relay{
		start: opts.StartRelay, dir: opts.RelayDir, args: relayArgs(opts.RelayModel),
		timeout: opts.RelayTimeout, noticeWait: opts.NoticeWait, idle: opts.RelayIdle,
		onNotice: a.notice,
	}
	return a
}

// Close stops the relay, if it is running. Messages still queued for open
// sessions are dropped with it.
func (a *Adapter) Close() error {
	a.relay.shutdown()
	return nil
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
	live     bool         // open somewhere: lasso mustn't resume it
	ses      *liveSession // Claude Code's own record of it, when open
	unique   bool         // ses's name is the only running session with that name
	state    adapter.State
	prompt   *openTool // the prompt it's stopped at, if any
	asking   bool      // prompt is a question, not a permission prompt
	writable bool
}

// views refreshes and derives every listed session's state, and sends
// whatever was waiting for a session that has since closed. Caller holds
// a.mu.
func (a *Adapter) views() ([]view, error) {
	if err := a.refresh(); err != nil {
		return nil, err
	}
	ls := readLive(a.opts.SessionsDir, a.opts.Alive)
	ps, err := a.opts.Procs()
	if err != nil {
		ps = nil // can't see processes: fall back to the files alone
	}
	if ls.ok {
		// Processes Claude Code already lists are matched by session id
		// below; only the rest (an older claude) need the guesswork.
		ps = slices.DeleteFunc(slices.Clone(ps), func(p Proc) bool { return ls.pids[p.PID] })
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
		v := view{f: f}
		if s, ok := ls.byID[f.id]; ok {
			v.ses, v.unique = &s, s.Name != "" && ls.byName[s.Name] == 1
		}
		proc := v.ses != nil || byID[f.id] || byCwd[f.cwd] && newest[f.cwd] == f
		quiet := now.Sub(f.mtime)
		// Claude Code's own list is the truth when it keeps one; without
		// it, a recently written transcript is taken to be open.
		v.live = proc || a.busy[f.id] || a.draining[f.id] || !ls.ok && quiet < a.opts.LiveWindow
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
		case a.busy[f.id] || a.draining[f.id]:
			v.state = adapter.Running
		case v.prompt != nil:
			v.state = adapter.Waiting
		case v.ses != nil && v.ses.Status != "":
			if v.ses.busy() {
				v.state = adapter.Running
			} else {
				v.state = adapter.Idle
			}
		case v.live && f.turnOpen():
			v.state = adapter.Running
		default:
			v.state = adapter.Idle
		}
		// Open or not, lasso can take a message: the relay passes it on,
		// or it waits for the session to close.
		v.writable = dirExists(f.cwd)
		out = append(out, v)
	}
	slices.SortFunc(out, func(x, y view) int { return y.f.mtime.Compare(x.f.mtime) })
	a.flush(out)
	a.verify()
	return out, nil
}

// longRunning tools run for minutes without a prompt: subagents write to
// their own transcripts while the parent's sits quiet.
func longRunning(name string) bool {
	return name == "Agent" || name == "Task" || strings.HasPrefix(name, "mcp__")
}

// Sessions implements adapter.Adapter: recent sessions, newest first.
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
// touched in the past day. Replies to Send, and lasso's notes about
// messages it is holding or has passed on, come once, with the next call,
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
