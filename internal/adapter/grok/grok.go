// Package grok is the grok adapter (ADR 0001 §4). It reads grok's own
// session files under ~/.grok/sessions and the `grok sessions list`
// output, and writes only by resuming an idle session headlessly:
// `grok --resume <id> --prompt-file <tmp> --output-format streaming-json`.
//
// grok bots are lasso's noisiest source, so events are compact per-turn
// deltas: the tools a turn used, the files it changed, its final words
// (clipped), and permission prompts. Thoughts, tool output and narration
// stay in grok's files; the raw log view reads them there.
//
// A permission prompt in a headless turn can't be put to anyone, so grok
// cancels the turn. Answer resumes the session with an --allow or --deny
// rule built from the refused call's own arguments, for that turn only.
//
// Sessions open in a grok TUI are read-only until the live-session spike
// (#9) lands. lasso never reads ~/.grok/auth.json or anything beside the
// session tree and the list of open sessions.
//
// Everything grok writes is untrusted: text is rewritten through the
// attention glossary for display but stays typed adapter.Untrusted.
package grok

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
)

// Kind is this adapter's name.
const Kind = "grok"

// Options are the adapter's injectable parts. The zero value reads the
// real ~/.grok and runs the grok on PATH.
type Options struct {
	// Home is grok's home directory: $GROK_HOME, else ~/.grok. lasso reads
	// only its sessions/ tree and active_sessions.json.
	Home string
	// Grok is the grok binary. Empty means "grok" on PATH.
	Grok string
	// Env is added to grok's environment (tests point it at a fake).
	Env []string
	// Dir is where `grok sessions list` runs: grok lists the sessions of
	// one working directory. Empty means lasso's own.
	Dir string
	// Recent is how far back a session's last activity may be for it to
	// count as current. Zero means two days.
	Recent time.Duration
	// TurnTimeout bounds one headless turn. Zero means an hour.
	TurnTimeout time.Duration
	// Report is told about trouble (grok missing, a turn that failed to
	// start), once per reason. Nil means silence.
	Report func(msg string)
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Adapter reads and steers grok sessions on this machine.
type Adapter struct {
	opts Options

	mu       sync.Mutex
	partial  map[string]partialTurn // session -> the unfinished turn read so far
	prompts  map[string]*pending    // prompt id -> what it asks
	busy     map[string]bool        // sessions lasso is running a turn in
	queue    []adapter.Event        // events parsed from lasso's own turns
	seen     map[string]time.Time   // event ids already handed out
	listAt   time.Time
	listRows []listRow
	problem  string
	turns    sync.WaitGroup
}

var _ adapter.Adapter = (*Adapter)(nil)

// New returns the adapter.
func New(opts Options) *Adapter {
	if opts.Home == "" {
		opts.Home = os.Getenv("GROK_HOME")
	}
	if opts.Home == "" {
		if home, err := os.UserHomeDir(); err == nil {
			opts.Home = filepath.Join(home, ".grok")
		}
	}
	if opts.Grok == "" {
		opts.Grok = "grok"
	}
	if opts.Recent == 0 {
		opts.Recent = 48 * time.Hour
	}
	if opts.TurnTimeout == 0 {
		opts.TurnTimeout = time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Adapter{
		opts:    opts,
		partial: map[string]partialTurn{},
		prompts: map[string]*pending{},
		busy:    map[string]bool{},
		seen:    map[string]time.Time{},
	}
}

// Kind implements adapter.Adapter.
func (a *Adapter) Kind() string { return Kind }

// session is one session directory on disk.
type session struct {
	ID      string
	Dir     string
	Cwd     string
	Title   string
	Kind    string // grok's session_kind: "", "headless", "subagent"
	Updated time.Time
}

func (s session) updates() string { return filepath.Join(s.Dir, "updates.jsonl") }

// summary is the part of summary.json lasso reads.
type summary struct {
	Info struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"info"`
	Title       string    `json:"generated_title"`
	Summary     string    `json:"session_summary"`
	LastTurn    string    `json:"last_turn_summary"`
	SessionKind string    `json:"session_kind"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// scan lists the session directories whose updates moved within window
// (all of them for window 0). Subagent sessions are left out: their
// parent's turn already says it spawned them.
func (a *Adapter) scan(window time.Duration) ([]session, error) {
	root := filepath.Join(a.opts.Home, "sessions")
	groups, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cutoff := a.opts.Now().Add(-window)
	var out []session
	for _, g := range groups {
		if !g.IsDir() {
			continue
		}
		gdir := filepath.Join(root, g.Name())
		ids, err := os.ReadDir(gdir)
		if err != nil {
			continue
		}
		for _, id := range ids {
			if !id.IsDir() {
				continue
			}
			s := session{ID: id.Name(), Dir: filepath.Join(gdir, id.Name())}
			fi, err := os.Stat(s.updates())
			if err != nil {
				continue
			}
			s.Updated = fi.ModTime()
			if window > 0 && s.Updated.Before(cutoff) {
				continue
			}
			if !a.readSummary(&s, gdir) {
				continue
			}
			out = append(out, s)
		}
	}
	return out, nil
}

func (a *Adapter) readSummary(s *session, gdir string) bool {
	var sum summary
	if b, err := os.ReadFile(filepath.Join(s.Dir, "summary.json")); err == nil {
		_ = json.Unmarshal(b, &sum)
	}
	if sum.SessionKind == "subagent" {
		return false
	}
	s.Kind = sum.SessionKind
	s.Cwd = sum.Info.Cwd
	if s.Cwd == "" {
		s.Cwd = groupCwd(gdir)
	}
	s.Title = first(sum.Title, sum.Summary, sum.LastTurn)
	if s.Title == "" {
		s.Title = "grok in " + filepath.Base(s.Cwd)
	}
	return true
}

// groupCwd recovers the working directory a session group is named after:
// the URL-encoded path, or a .cwd file when the name was too long.
func groupCwd(gdir string) string {
	if b, err := os.ReadFile(filepath.Join(gdir, ".cwd")); err == nil {
		return strings.TrimSpace(string(b))
	}
	if p, err := url.PathUnescape(filepath.Base(gdir)); err == nil {
		return p
	}
	return ""
}

// onDisk is the set of session ids in the session tree, read from the
// directory names alone.
func (a *Adapter) onDisk() map[string]bool {
	out := map[string]bool{}
	root := filepath.Join(a.opts.Home, "sessions")
	groups, _ := os.ReadDir(root)
	for _, g := range groups {
		ids, _ := os.ReadDir(filepath.Join(root, g.Name()))
		for _, id := range ids {
			out[id.Name()] = true
		}
	}
	return out
}

func (a *Adapter) find(id string) (session, bool) {
	all, err := a.scan(0)
	if err != nil {
		return session{}, false
	}
	for _, s := range all {
		if s.ID == id {
			return s, true
		}
	}
	return session{}, false
}

// Sessions implements adapter.Adapter: grok sessions active within
// Options.Recent, plus any open in a grok TUI. A session is writable only
// when it is idle or waiting: not open in a TUI, not mid-turn.
func (a *Adapter) Sessions(ctx context.Context) ([]adapter.Session, error) {
	all, err := a.scan(a.opts.Recent)
	if err != nil {
		return nil, fmt.Errorf("grok: %w", err)
	}
	live := a.live()
	var out []adapter.Session
	have := map[string]bool{}
	for _, s := range all {
		have[s.ID] = true
		out = append(out, a.describe(s, live[s.ID]))
	}
	// Sessions open in a TUI but quiet for longer than Recent.
	for id := range live {
		if have[id] {
			continue
		}
		if s, ok := a.find(id); ok {
			have[id] = true
			out = append(out, a.describe(s, true))
		}
	}
	disk := a.onDisk()
	for _, r := range a.list(ctx) {
		if have[r.ID] || disk[r.ID] {
			continue
		}
		have[r.ID] = true
		out = append(out, adapter.Session{ID: r.ID, Bot: Kind, Title: attention.Gloss(r.Summary), State: adapter.Idle, Writable: !a.isBusy(r.ID)})
	}
	return out, nil
}

func (a *Adapter) describe(s session, live bool) adapter.Session {
	out := adapter.Session{ID: s.ID, Bot: Kind, Title: attention.Gloss(s.Title)}
	st := a.tail(s)
	switch {
	case live:
		out.State = adapter.Running
	case a.isBusy(s.ID), st.open && a.opts.Now().Sub(s.Updated) < staleTurn:
		out.State = adapter.Running
	case st.waiting:
		out.State = adapter.Waiting
	default:
		out.State = adapter.Idle
	}
	out.Writable = out.State != adapter.Running
	return out
}

// staleTurn is how long a turn may go without writing before lasso takes
// it for dead (grok killed mid-turn) rather than running.
const staleTurn = 15 * time.Minute

// tailState is what the end of a session's updates says.
type tailState struct {
	open    bool   // the last prompt has no turn_completed yet
	waiting bool   // the last turn stopped at a permission prompt
	prompt  string // the last turn's prompt id
}

// tail reads the last lines of a session's updates, growing the window
// until it holds a line that says where the last turn stands.
func (a *Adapter) tail(s session) tailState {
	f, err := os.Open(s.updates())
	if err != nil {
		return tailState{}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return tailState{}
	}
	for window := int64(64 << 10); ; window *= 4 {
		from := max(fi.Size()-window, 0)
		buf := make([]byte, fi.Size()-from)
		if _, err := f.ReadAt(buf, from); err != nil {
			return tailState{}
		}
		lines := bytes.Split(buf, []byte("\n"))
		if from > 0 {
			lines = lines[1:] // the first is a fragment
		}
		for i := len(lines) - 1; i >= 0; i-- {
			u, ok := parseDisk(lines[i])
			if !ok {
				continue
			}
			if u.kind == turnEnd {
				t := newTurn(u.prompt, 0)
				t.apply(u)
				return tailState{waiting: t.waiting(), prompt: u.prompt}
			}
			if u.prompt != "" || u.kind == userText {
				return tailState{open: true, prompt: u.prompt}
			}
		}
		if from == 0 || window >= 16<<20 {
			return tailState{}
		}
	}
}

// activeSession is one entry of grok's active_sessions.json: a session
// open in a TUI tab.
type activeSession struct {
	SessionID string `json:"session_id"`
	PID       int    `json:"pid"`
}

// live returns the sessions open in a grok TUI whose process still runs.
func (a *Adapter) live() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(filepath.Join(a.opts.Home, "active_sessions.json"))
	if err != nil {
		return out
	}
	var list []activeSession
	if json.Unmarshal(b, &list) != nil {
		return out
	}
	for _, s := range list {
		if s.SessionID != "" && alive(s.PID) {
			out[s.SessionID] = true
		}
	}
	return out
}

// alive reports whether pid runs. A missing pid counts as alive: grok says
// the session is open and lasso can't prove otherwise.
func alive(pid int) bool {
	if pid <= 0 {
		return true
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// listRow is one row of `grok sessions list`.
type listRow struct {
	ID      string
	Status  string
	Summary string
}

var listRe = regexp.MustCompile(`^([0-9a-fA-F-]{36})\s+(\S+)\s+(\S+)\s+(\S+)\s*(.*)$`)

// parseList reads `grok sessions list`'s table. Group headings
// ("(no label)") and the column header are skipped.
func parseList(out []byte) []listRow {
	var rows []listRow
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		m := listRe.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		rows = append(rows, listRow{ID: strings.ToLower(m[1]), Status: m[4], Summary: strings.TrimSpace(m[5])})
	}
	return rows
}

// listEvery is how long one `grok sessions list` answer is reused.
const listEvery = time.Minute

// list runs `grok sessions list` in Options.Dir, at most once a minute. It
// adds sessions the session tree doesn't hold (another GROK_HOME, say).
// A grok that won't run is reported once and lists nothing.
func (a *Adapter) list(ctx context.Context) []listRow {
	a.mu.Lock()
	if !a.listAt.IsZero() && a.opts.Now().Sub(a.listAt) < listEvery {
		rows := a.listRows
		a.mu.Unlock()
		return rows
	}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.opts.Grok, "sessions", "list", "--limit", "50")
	cmd.Dir = a.opts.Dir
	cmd.Env = append(os.Environ(), a.opts.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var rows []listRow
	if err != nil {
		a.report("can't list grok sessions: " + first(lastLine(stderr.String()), err.Error()))
	} else {
		rows = parseList(out)
		a.report("")
	}
	a.mu.Lock()
	a.listAt, a.listRows = a.opts.Now(), rows
	a.mu.Unlock()
	return rows
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
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
		a.opts.Report("grok is reachable again")
		return
	}
	a.opts.Report(reason)
}

func (a *Adapter) isBusy(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy[id]
}

// Cwd is the folder session id works in, if it is on disk. The daemon
// uses it to tell which repo a session works in.
func (a *Adapter) Cwd(id string) (string, bool) {
	s, ok := a.find(id)
	if !ok || s.Cwd == "" {
		return "", false
	}
	return s.Cwd, true
}
