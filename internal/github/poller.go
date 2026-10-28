package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"
)

// Polling cadence (ADR 0001 §3).
const (
	RunsActiveEvery = 60 * time.Second // while any run is in progress
	RunsIdleEvery   = 5 * time.Minute
	IssuesEvery     = 2 * time.Minute // issues and PRs
	MilestonesEvery = 15 * time.Minute
	// BackoffFactor stretches every interval while less than 20% of the
	// rate limit remains. With none left, polling waits for the reset.
	BackoffFactor = 4
)

// snapshotTTL is how long a repo's last snapshot stays in the cache.
const snapshotTTL = 24 * time.Hour

// Clock is the poller's time source, injected so tests control it.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock is the wall clock.
var RealClock Clock = realClock{}

// Kind is the kind of item a Change is about.
type Kind string

// The item kinds.
const (
	KindIssue     Kind = "issue"
	KindPR        Kind = "pr"
	KindRun       Kind = "run"
	KindMilestone Kind = "milestone"
)

// Op is what happened to an item.
type Op string

// The ops. Removed means the item left the polled set: an issue, PR or
// milestone was closed, or a run fell off the recent-runs page.
const (
	Added   Op = "added"
	Updated Op = "updated"
	Removed Op = "removed"
)

// Change is one item that appeared, changed or went away. Exactly one of
// Issue, PR, Run and Milestone is set: the new value, or for Removed the
// last value seen.
type Change struct {
	Op        Op
	Kind      Kind
	Repo      string
	Key       string
	Issue     *Issue
	PR        *PR
	Run       *Run
	Milestone *Milestone
}

// Snapshot is everything last seen for one repo. It is cached under
// SnapshotKey for readers that want it without polling.
type Snapshot struct {
	Issues     []Issue
	PRs        []PR
	Runs       []Run
	Milestones []Milestone
}

// SnapshotKey is the cache key of repo's Snapshot.
func SnapshotKey(repo string) string { return "github:snapshot:" + repo }

// LoadSnapshot reads repo's cached Snapshot.
func LoadSnapshot(cache Cache, repo string, now time.Time) (Snapshot, bool, error) {
	b, ok, err := cache.CacheGet(SnapshotKey(repo), now)
	if err != nil || !ok {
		return Snapshot{}, false, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return Snapshot{}, false, err
	}
	return s, true, nil
}

// group is a set of items polled together on one cadence.
type group int

const (
	groupRuns group = iota
	groupIssues
	groupMilestones
	numGroups
)

// resource is the rate-limit resource a group spends.
func (g group) resource() string {
	if g == groupRuns {
		return "core"
	}
	return "graphql"
}

// Poller polls hot repos on the ADR 0001 §3 cadence and emits changes. It
// never writes to GitHub. It is not safe for concurrent use.
type Poller struct {
	client *Client
	cache  Cache
	repos  []string
	clock  Clock
	emit   func(Change)

	next  map[string]*[numGroups]time.Time
	state map[string]*Snapshot
}

// NewPoller returns a Poller over repos ("owner/name"). It starts gh through
// runner, keeps ETags and snapshots in cache (in memory when nil), reads
// time from clock (RealClock when nil) and hands every change to emit.
func NewPoller(runner Runner, cache Cache, repos []string, clock Clock, emit func(Change)) *Poller {
	if cache == nil {
		cache = newMemCache()
	}
	if clock == nil {
		clock = RealClock
	}
	if emit == nil {
		emit = func(Change) {}
	}
	return &Poller{
		client: NewClient(runner, cache, clock.Now),
		cache:  cache,
		repos:  repos,
		clock:  clock,
		emit:   emit,
		next:   map[string]*[numGroups]time.Time{},
		state:  map[string]*Snapshot{},
	}
}

// Client is the poller's GitHub client, for its rate limits and for one-off
// reads such as a run's jobs.
func (p *Poller) Client() *Client { return p.client }

// Snapshot returns what was last seen for repo.
func (p *Poller) Snapshot(repo string) Snapshot {
	if s := p.state[repo]; s != nil {
		return *s
	}
	return Snapshot{}
}

// Poll polls every group that is due now and returns when the next one is
// due. Errors from one repo or group don't stop the others; a group that
// failed is retried on its normal cadence. A *PartialError still applies
// the data that came back.
func (p *Poller) Poll(ctx context.Context) (time.Time, error) {
	var errs []error
	var soonest time.Time
	for _, repo := range p.repos {
		next := p.next[repo]
		if next == nil {
			next = &[numGroups]time.Time{}
			p.next[repo] = next
			p.state[repo] = &Snapshot{}
		}
		for g := range numGroups {
			if now := p.clock.Now(); !now.Before(next[g]) {
				if err := p.pollGroup(ctx, repo, g); err != nil {
					errs = append(errs, err)
				}
				now = p.clock.Now()
				next[g] = now.Add(p.interval(repo, g, now))
			}
			if soonest.IsZero() || next[g].Before(soonest) {
				soonest = next[g]
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	return soonest, errors.Join(errs...)
}

// Run polls until ctx ends, sleeping on the clock between polls and
// passing each Poll error to onError (which may be nil).
func (p *Poller) Run(ctx context.Context, onError func(error)) error {
	for {
		next, err := p.Poll(ctx)
		if err != nil && onError != nil {
			onError(err)
		}
		wait := next.Sub(p.clock.Now())
		if wait < time.Second {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.clock.After(wait):
		}
	}
}

// interval is how long to wait before polling g for repo again.
func (p *Poller) interval(repo string, g group, now time.Time) time.Duration {
	var d time.Duration
	switch g {
	case groupRuns:
		d = RunsIdleEvery
		for _, r := range p.state[repo].Runs {
			if r.InProgress() {
				d = RunsActiveEvery
				break
			}
		}
	case groupIssues:
		d = IssuesEvery
	case groupMilestones:
		d = MilestonesEvery
	}
	rl := p.client.RateLimit(g.resource())
	if rl.Low() {
		d *= BackoffFactor
		if rl.Remaining == 0 && rl.Reset.After(now) {
			d = max(d, rl.Reset.Sub(now))
		}
	}
	return d
}

func (p *Poller) pollGroup(ctx context.Context, repo string, g group) error {
	st := p.state[repo]
	var errs []error
	keep := func(err error) bool {
		var partial *PartialError
		if err == nil || errors.As(err, &partial) {
			if err != nil {
				errs = append(errs, err)
			}
			return true
		}
		errs = append(errs, err)
		return false
	}
	switch g {
	case groupRuns:
		runs, err := p.client.Runs(ctx, repo)
		if !keep(err) {
			break
		}
		p.fillFailingSteps(ctx, repo, runs, st.Runs, keep)
		diff(p, KindRun, repo, st.Runs, runs, runKey, func(c *Change, r *Run) { c.Run = r })
		st.Runs = runs
	case groupIssues:
		issues, prs, err := p.client.IssuesAndPRs(ctx, repo)
		if !keep(err) {
			break
		}
		diff(p, KindIssue, repo, st.Issues, issues, func(i Issue) string { return numKey(repo, "#", i.Number) }, func(c *Change, i *Issue) { c.Issue = i })
		diff(p, KindPR, repo, st.PRs, prs, func(pr PR) string { return numKey(repo, "!", pr.Number) }, func(c *Change, pr *PR) { c.PR = pr })
		st.Issues, st.PRs = issues, prs
	case groupMilestones:
		ms, err := p.client.Milestones(ctx, repo)
		if !keep(err) {
			break
		}
		diff(p, KindMilestone, repo, st.Milestones, ms, func(m Milestone) string { return numKey(repo, "/milestone/", m.Number) }, func(c *Change, m *Milestone) { c.Milestone = m })
		st.Milestones = ms
	}
	if b, err := json.Marshal(st); err == nil {
		// Speed only (ADR 0001 §2): a failed write loses nothing.
		_ = p.cache.CachePut(SnapshotKey(repo), b, p.clock.Now(), snapshotTTL)
	}
	return errors.Join(errs...)
}

// fillFailingSteps sets FailingJob and FailingStep on failed runs, reusing
// what was found for the same run attempt last time so each failed attempt
// costs one jobs request.
func (p *Poller) fillFailingSteps(ctx context.Context, repo string, runs, prev []Run, keep func(error) bool) {
	known := map[string]Run{}
	for _, r := range prev {
		if r.FailingJob != "" {
			known[runKey(r)+"/"+strconv.Itoa(r.Attempt)] = r
		}
	}
	for i := range runs {
		r := &runs[i]
		if r.InProgress() || !r.Failed() {
			continue
		}
		if k, ok := known[runKey(*r)+"/"+strconv.Itoa(r.Attempt)]; ok {
			r.FailingJob, r.FailingStep = k.FailingJob, k.FailingStep
			continue
		}
		jobs, err := p.client.Jobs(ctx, repo, r.ID, r.Attempt)
		if !keep(err) {
			continue
		}
		r.FailingJob, r.FailingStep = FailingStep(jobs)
	}
}

func runKey(r Run) string { return fmt.Sprintf("%s/run/%d", r.Repo, r.ID) }

func numKey(repo, sep string, n int) string { return repo + sep + strconv.Itoa(n) }

// diff emits a Change for every item added, updated or removed between old
// and cur.
func diff[T any](p *Poller, kind Kind, repo string, old, cur []T, key func(T) string, set func(*Change, *T)) {
	before := make(map[string]T, len(old))
	for _, it := range old {
		before[key(it)] = it
	}
	for i := range cur {
		k := key(cur[i])
		prev, ok := before[k]
		delete(before, k)
		if ok && reflect.DeepEqual(prev, cur[i]) {
			continue
		}
		c := Change{Op: Added, Kind: kind, Repo: repo, Key: k}
		if ok {
			c.Op = Updated
		}
		it := cur[i]
		set(&c, &it)
		p.emit(c)
	}
	for _, it := range old {
		k := key(it)
		if _, gone := before[k]; !gone {
			continue
		}
		c := Change{Op: Removed, Kind: kind, Repo: repo, Key: k}
		set(&c, &it)
		p.emit(c)
	}
}
