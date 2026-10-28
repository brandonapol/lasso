package github

import (
	"context"
	"testing"
	"time"
)

func runJSON(status, conclusion string) string {
	c := `null`
	if conclusion != "" {
		c = `"` + conclusion + `"`
	}
	return `{"workflow_runs":[{"id":7,"name":"CI","head_branch":"main","status":"` + status + `","conclusion":` + c + `,
	  "run_attempt":1,"run_started_at":"2026-10-27T08:00:00Z","updated_at":"2026-10-27T08:05:00Z","html_url":"https://x/7"}]}`
}

type recorder struct{ changes []Change }

func (r *recorder) emit(c Change) { r.changes = append(r.changes, c) }

func (r *recorder) take() []Change {
	c := r.changes
	r.changes = nil
	return c
}

// pollAt advances the clock to at and polls.
func pollAt(t *testing.T, p *Poller, clk *fakeClock, at time.Time) time.Time {
	t.Helper()
	clk.now = at
	next, err := p.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestCadence(t *testing.T) {
	gh := newFakeGH(t)
	gh.runs = runJSON("in_progress", "")
	gh.runsETag = `"a"`
	clk := &fakeClock{now: t0}
	p := NewPoller(gh, nil, []string{repo}, clk, nil)

	next := pollAt(t, p, clk, t0)
	if next != t0.Add(RunsActiveEvery) {
		t.Fatalf("with a run in progress, next poll at %v, want +60s", next.Sub(t0))
	}
	if gh.count("/actions/runs") != 1 || gh.count("pullRequests(") != 1 || gh.count("milestones(") != 1 {
		t.Fatalf("first poll calls = %q", gh.calls)
	}

	// +60s: only runs are due. The run finishes.
	gh.runs, gh.runsETag = runJSON("completed", "success"), `"b"`
	next = pollAt(t, p, clk, t0.Add(time.Minute))
	if gh.count("/actions/runs") != 2 || gh.count("pullRequests(") != 1 {
		t.Fatalf("at +60s calls = %q", gh.calls)
	}
	if next != t0.Add(IssuesEvery) {
		t.Fatalf("next = +%v, want issues at +2m", next.Sub(t0))
	}

	// +2m: issues are due; runs are idle now, so not until +1m+5m.
	next = pollAt(t, p, clk, t0.Add(2*time.Minute))
	if gh.count("pullRequests(") != 2 || gh.count("/actions/runs") != 2 {
		t.Fatalf("at +2m calls = %q", gh.calls)
	}
	if next != t0.Add(4*time.Minute) {
		t.Fatalf("next = +%v, want issues again at +4m", next.Sub(t0))
	}
	pollAt(t, p, clk, t0.Add(4*time.Minute))
	pollAt(t, p, clk, t0.Add(6*time.Minute))
	if gh.count("/actions/runs") != 3 {
		t.Fatalf("idle runs should poll at +6m: %d runs calls", gh.count("/actions/runs"))
	}
	if gh.count("milestones(") != 1 {
		t.Fatal("milestones polled before 15m")
	}
	pollAt(t, p, clk, t0.Add(15*time.Minute))
	if gh.count("milestones(") != 2 {
		t.Fatal("milestones not polled at 15m")
	}
}

func TestRateLimitBackoff(t *testing.T) {
	gh := newFakeGH(t)
	gh.runs = runJSON("in_progress", "")
	gh.core = rate{5000, 999, t0.Add(time.Hour)} // under 20%
	clk := &fakeClock{now: t0}
	p := NewPoller(gh, nil, []string{repo}, clk, nil)
	pollAt(t, p, clk, t0)

	// Runs (REST, core) back off; issues (GraphQL) don't.
	for _, at := range []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute} {
		pollAt(t, p, clk, t0.Add(at))
	}
	if n := gh.count("/actions/runs"); n != 1 {
		t.Fatalf("runs polled %d times in 3m with a low core limit, want 1", n)
	}
	if n := gh.count("pullRequests("); n != 2 {
		t.Fatalf("issues polled %d times in 3m, want 2", n)
	}
	pollAt(t, p, clk, t0.Add(RunsActiveEvery*BackoffFactor))
	if n := gh.count("/actions/runs"); n != 2 {
		t.Fatalf("runs not polled after the backed-off interval: %d", n)
	}

	// With nothing left, wait for the reset.
	gh.graphql = rate{5000, 0, t0.Add(90 * time.Minute)}
	pollAt(t, p, clk, t0.Add(10*time.Minute))
	if got, want := p.next[repo][groupIssues], t0.Add(90*time.Minute); !got.Equal(want) {
		t.Fatalf("issues next at %v, want the reset %v", got, want)
	}

	// Recovered: back to normal.
	gh.graphql = rate{5000, 4000, t0.Add(2 * time.Hour)}
	pollAt(t, p, clk, t0.Add(90*time.Minute))
	if got, want := p.next[repo][groupIssues], t0.Add(90*time.Minute+IssuesEvery); !got.Equal(want) {
		t.Fatalf("issues next at %v, want %v", got, want)
	}
}

func TestChangesAndCache(t *testing.T) {
	gh := newFakeGH(t)
	gh.issues = `{"repository":{"issues":{"nodes":[
	  {"number":1,"title":"one","body":"","updatedAt":"2026-10-26T10:00:00Z"},
	  {"number":2,"title":"two","body":"","updatedAt":"2026-10-26T10:00:00Z"}]},"pullRequests":{"nodes":[]}}}`
	clk := &fakeClock{now: t0}
	rec := &recorder{}
	cache := newMemCache()
	p := NewPoller(gh, cache, []string{repo}, clk, rec.emit)
	pollAt(t, p, clk, t0)
	added := 0
	for _, c := range rec.take() {
		if c.Kind == KindIssue && c.Op == Added && c.Issue != nil && c.Repo == repo {
			added++
		}
	}
	if added != 2 {
		t.Fatalf("want 2 issues added")
	}

	gh.issues = `{"repository":{"issues":{"nodes":[
	  {"number":1,"title":"one, renamed","body":"","updatedAt":"2026-10-27T10:00:00Z"}]},"pullRequests":{"nodes":[]}}}`
	pollAt(t, p, clk, t0.Add(IssuesEvery))
	got := rec.take()
	if len(got) != 2 || got[0].Op != Updated || got[0].Issue.Title != "one, renamed" || got[0].Key != repo+"#1" ||
		got[1].Op != Removed || got[1].Issue.Number != 2 {
		t.Fatalf("changes = %+v", got)
	}

	// Nothing changed: nothing emitted.
	pollAt(t, p, clk, t0.Add(2*IssuesEvery))
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("unchanged poll emitted %+v", got)
	}

	snap, ok, err := LoadSnapshot(cache, repo, clk.now)
	if err != nil || !ok || len(snap.Issues) != 1 || snap.Issues[0].Title != "one, renamed" {
		t.Fatalf("cached snapshot = %+v, %v, %v", snap, ok, err)
	}
}

func TestETag304EmitsNothing(t *testing.T) {
	gh := newFakeGH(t)
	gh.runs = runJSON("in_progress", "")
	clk := &fakeClock{now: t0}
	rec := &recorder{}
	p := NewPoller(gh, nil, []string{repo}, clk, rec.emit)
	pollAt(t, p, clk, t0)
	if len(rec.take()) != 1 {
		t.Fatal("want the run added")
	}
	pollAt(t, p, clk, t0.Add(RunsActiveEvery))
	if gh.count("If-None-Match") != 1 {
		t.Fatal("second runs poll wasn't conditional")
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("304 emitted %+v", got)
	}
	// The 304 kept the run in progress, so the cadence stays fast.
	if got := p.next[repo][groupRuns]; !got.Equal(t0.Add(2 * RunsActiveEvery)) {
		t.Fatalf("runs next at +%v", got.Sub(t0))
	}
}

func TestFailedRunGetsFailingStepOnce(t *testing.T) {
	gh := newFakeGH(t)
	gh.runs = runJSON("in_progress", "")
	gh.jobs["runs/7/attempts/1"] = `{"jobs":[
	  {"name":"lint","conclusion":"success","steps":[{"name":"golangci-lint","conclusion":"success"}]},
	  {"name":"test","conclusion":"failure","steps":[{"name":"Checkout","conclusion":"success"},{"name":"go test","conclusion":"failure"}]}]}`
	clk := &fakeClock{now: t0}
	rec := &recorder{}
	p := NewPoller(gh, nil, []string{repo}, clk, rec.emit)
	pollAt(t, p, clk, t0)
	if gh.count("/jobs") != 0 {
		t.Fatal("fetched jobs for a run in progress")
	}
	rec.take()

	gh.runs, gh.runsETag = runJSON("completed", "failure"), `"failed"`
	pollAt(t, p, clk, t0.Add(RunsActiveEvery))
	got := rec.take()
	if len(got) != 1 || got[0].Op != Updated || got[0].Run.FailingJob != "test" || got[0].Run.FailingStep != "go test" {
		t.Fatalf("changes = %+v", got)
	}
	if r := p.Snapshot(repo).Runs[0]; r.FailingStep != "go test" || r.Finished.IsZero() {
		t.Fatalf("snapshot run = %+v", r)
	}

	pollAt(t, p, clk, t0.Add(RunsActiveEvery+RunsIdleEvery))
	if n := gh.count("/jobs"); n != 1 {
		t.Fatalf("jobs fetched %d times for one failed attempt", n)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("re-poll emitted %+v", got)
	}
}

func TestRunSleepsUntilNextPoll(t *testing.T) {
	gh := newFakeGH(t)
	ctx, cancel := context.WithCancel(context.Background())
	clk := &fakeClock{now: t0}
	clk.after = func(time.Duration) bool {
		if len(clk.waits) == 2 {
			cancel()
			return false
		}
		return true
	}
	p := NewPoller(gh, nil, []string{repo}, clk, nil)
	if err := p.Run(ctx, func(err error) { t.Error(err) }); err != context.Canceled {
		t.Fatalf("Run = %v", err)
	}
	if len(clk.waits) != 2 || clk.waits[0] != IssuesEvery {
		t.Fatalf("waits = %v, want the first to be the issues cadence", clk.waits)
	}
}
