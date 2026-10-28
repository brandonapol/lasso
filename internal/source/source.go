// Package source is the daemon's real api.Source: GitHub's hot repos, seen
// through the attention rules (ADR 0001 §3, §5).
//
// One goroutine (Run) polls GitHub and owns the raw state. After each round
// it rebuilds every tab's rows, diffs them against the last set and pushes
// item.changed and item.removed for what moved, and brief.updated when the
// "since you left" set changes. Readers (Items, Focus) only see the rows.
package source

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/github"
)

// Options configures New.
type Options struct {
	// Repos are the hot repos ("owner/name"). None means every tab shows
	// one item explaining how to add them.
	Repos []string
	// Runner starts gh (github.CLI{} in the daemon).
	Runner github.Runner
	// Cache keeps ETags and snapshots (the store); nil keeps them in memory.
	Cache github.Cache
	// Cursors reads the brief cursor (the store); nil means never read.
	Cursors attention.CursorReader
	// Sorter sorts items no level rule covers; nil keeps them as brief.
	Sorter attention.Sorter
	// Clock is the time source; nil is the wall clock.
	Clock github.Clock
	// ConfigPath is shown in the "add hot repos" explainer.
	ConfigPath string
	// Logf, if set, gets one line each time a repo starts or stops failing.
	Logf func(format string, args ...any)
}

// subBuffer is how many events a subscriber may fall behind before it is
// dropped and has to reconnect.
const subBuffer = 256

// Source implements api.Source over GitHub and attention.
type Source struct {
	opts    Options
	pollers map[string]*github.Poller
	called  map[string]bool // poll groups that called gh in the current poll

	// Raw state, owned by the polling goroutine.
	issues   map[string]github.Issue
	prs      map[string]github.PR
	runs     map[string]github.Run
	errs     map[string]repoError // by repo
	branches map[string]string    // repo -> default branch, once known
	login    string               // the gh user, once known
	levels   map[string]levelMemo // by attention item id

	mu    sync.Mutex
	rows  map[string]row
	brief string // key of the last published "since you left" set
	subs  map[chan api.Event]struct{}
}

type levelMemo struct {
	stamp string
	level attention.Level
}

// row is one item on one tab, with what orders it there.
type row struct {
	api.Focus
	group int       // lower groups come first
	at    time.Time // within a group: ascending when asc, else newest first
	asc   bool
	seq   int // ties after at
	// why is a line-jumper's reason. It names how long something has been
	// going on, so it changes every poll; it is kept out of Focus so that
	// doesn't count as a change, and added when the focus is read.
	why string
}

// focus is what Focus serves for r.
func (r row) focus() api.Focus {
	f := r.Focus
	if r.why != "" {
		f.Fields = append([]api.Field{{Name: "Why it's here", Value: r.why}}, f.Fields...)
	}
	return f
}

// New returns a Source. Call Run to start polling.
func New(opts Options) *Source {
	if opts.Clock == nil {
		opts.Clock = github.RealClock
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = "~/.config/lasso/config.toml"
	}
	s := &Source{
		opts:     opts,
		pollers:  map[string]*github.Poller{},
		called:   map[string]bool{},
		issues:   map[string]github.Issue{},
		prs:      map[string]github.PR{},
		runs:     map[string]github.Run{},
		errs:     map[string]repoError{},
		branches: map[string]string{},
		levels:   map[string]levelMemo{},
		rows:     map[string]row{},
		subs:     map[chan api.Event]struct{}{},
	}
	// One poller per repo, so each poll's error belongs to one repo.
	for _, repo := range opts.Repos {
		s.pollers[repo] = github.NewPoller(recordingRunner{opts.Runner, s.called}, opts.Cache, []string{repo}, opts.Clock, s.apply)
	}
	s.refresh(context.Background())
	return s
}

// Run polls until ctx ends. With no hot repos it just waits.
func (s *Source) Run(ctx context.Context) error {
	if len(s.opts.Repos) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	for {
		next := s.Step(ctx)
		wait := max(next.Sub(s.opts.Clock.Now()), time.Second)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.opts.Clock.After(wait):
		}
	}
}

// Step polls whatever is due in every hot repo, refreshes the rows and
// returns when something is next due.
func (s *Source) Step(ctx context.Context) time.Time {
	var soonest time.Time
	for _, repo := range s.opts.Repos {
		if ctx.Err() != nil {
			break
		}
		s.lookupBranch(ctx, repo)
		clear(s.called)
		next, err := s.pollers[repo].Poll(ctx)
		if ctx.Err() == nil {
			s.settle(repo, s.called, err)
		}
		if soonest.IsZero() || next.Before(soonest) {
			soonest = next
		}
	}
	s.lookupLogin(ctx)
	s.refresh(ctx)
	return soonest
}

// apply records one change from a poller.
func (s *Source) apply(c github.Change) {
	switch {
	case c.Issue != nil:
		update(s.issues, c.Key, *c.Issue, c.Op)
	case c.PR != nil:
		update(s.prs, c.Key, *c.PR, c.Op)
	case c.Run != nil:
		update(s.runs, c.Key, *c.Run, c.Op)
	}
	// Milestones only matter through issues' due dates, which the poller
	// already resolved.
}

func update[T any](m map[string]T, key string, v T, op github.Op) {
	if op == github.Removed {
		delete(m, key)
		return
	}
	m[key] = v
}

// lookupBranch learns repo's default branch once. Until it is known, main
// and master count as default.
func (s *Source) lookupBranch(ctx context.Context, repo string) {
	if _, ok := s.branches[repo]; ok {
		return
	}
	out, err := s.opts.Runner.Run(ctx, "api", "repos/"+repo, "--jq", ".default_branch")
	if b := strings.TrimSpace(string(out)); err == nil && b != "" {
		s.branches[repo] = b
	}
}

// lookupLogin learns the gh user once, for the In progress tab's PRs.
func (s *Source) lookupLogin(ctx context.Context) {
	if s.login != "" || ctx.Err() != nil {
		return
	}
	out, err := s.opts.Runner.Run(ctx, "api", "user", "--jq", ".login")
	if l := strings.TrimSpace(string(out)); err == nil && l != "" {
		s.login = l
	}
}

func (s *Source) isDefault(repo, branch string) bool {
	if b, ok := s.branches[repo]; ok {
		return branch == b
	}
	return branch == "main" || branch == "master"
}

func (s *Source) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// refresh rebuilds every row, publishes what changed and, if the "since
// you left" set moved, brief.updated.
func (s *Source) refresh(ctx context.Context) {
	now := s.opts.Clock.Now()
	items := s.attentionItems(ctx)
	rows := s.buildRows(items, now)

	var cursor time.Time
	if s.opts.Cursors != nil {
		cursor, _ = attention.LastSeen(s.opts.Cursors)
	}
	brief := attention.SinceYouLeft(items, cursor)
	key := briefKey(brief)

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range sortedKeys(s.rows) {
		if _, ok := rows[id]; !ok {
			s.publish(api.Event{Kind: api.EventItemRemoved, Data: map[string]string{"id": id}})
		}
	}
	for _, id := range sortedKeys(rows) {
		if old, ok := s.rows[id]; !ok || !reflect.DeepEqual(old.Focus, rows[id].Focus) {
			s.publish(api.Event{Kind: api.EventItemChanged, Data: rows[id].Item})
		}
	}
	s.rows = rows
	if key != s.brief {
		s.brief = key
		s.publish(api.Event{Kind: api.EventBriefUpdated, Data: briefData(brief)})
	}
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func briefKey(b []attention.RepoBrief) string {
	var sb strings.Builder
	for _, r := range b {
		for _, it := range r.Items {
			fmt.Fprintf(&sb, "%s@%d;", it.ID, it.Created.UnixNano())
		}
	}
	return sb.String()
}

// BriefRepo is one repo's line in a brief.updated event.
type BriefRepo struct {
	Repo  string   `json:"repo"`
	Count int      `json:"count"`
	IDs   []string `json:"ids"`
}

// Brief is a brief.updated event's data.
type Brief struct {
	Text  string      `json:"text"`
	Count int         `json:"count"`
	Repos []BriefRepo `json:"repos"`
}

func briefData(b []attention.RepoBrief) Brief {
	out := Brief{Repos: []BriefRepo{}}
	var parts []string
	for _, r := range b {
		br := BriefRepo{Repo: r.Repo, Count: len(r.Items)}
		for _, it := range r.Items {
			br.IDs = append(br.IDs, it.ID)
		}
		out.Repos = append(out.Repos, br)
		out.Count += br.Count
		name := r.Repo
		if name == "" {
			name = "elsewhere"
		}
		parts = append(parts, fmt.Sprintf("%d new in %s", br.Count, name))
	}
	if len(parts) == 0 {
		out.Text = "Nothing new since you left."
	} else {
		out.Text = strings.Join(parts, ", ") + "."
	}
	return out
}

// Items implements api.Source.
func (s *Source) Items(_ context.Context, tab api.Tab) ([]api.Item, error) {
	s.mu.Lock()
	var rows []row
	for _, r := range s.rows {
		if r.Tab == tab {
			rows = append(rows, r)
		}
	}
	s.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return less(rows[i], rows[j]) })
	out := make([]api.Item, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Item)
	}
	return out, nil
}

func less(a, b row) bool {
	if a.group != b.group {
		return a.group < b.group
	}
	if !a.at.Equal(b.at) {
		if a.asc {
			return a.at.Before(b.at)
		}
		return a.at.After(b.at)
	}
	if a.seq != b.seq {
		return a.seq < b.seq
	}
	return a.ID < b.ID
}

// Focus implements api.Source.
func (s *Source) Focus(_ context.Context, id string) (api.Focus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return api.Focus{}, api.ErrNotFound
	}
	return r.focus(), nil
}

// Subscribe implements api.Source.
func (s *Source) Subscribe() (<-chan api.Event, func()) {
	ch := make(chan api.Event, subBuffer)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// publish sends ev to every subscriber; s.mu must be held. A subscriber
// whose buffer is full is dropped so it reconnects and re-reads.
func (s *Source) publish(ev api.Event) {
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
			delete(s.subs, ch)
			close(ch)
		}
	}
}
