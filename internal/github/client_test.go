package github

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/store"
)

var _ Cache = (*store.Store)(nil)

const repo = "acme/widgets"

func TestParseResponse(t *testing.T) {
	r, err := parseResponse([]byte("HTTP/2.0 304 Not Modified\r\nEtag: W/\"x\"\r\nX-Ratelimit-Remaining: 7\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != 304 || r.Header.Get("Etag") != `W/"x"` || r.Header.Get("X-RateLimit-Remaining") != "7" || len(r.Body) != 0 {
		t.Fatalf("got %+v", r)
	}
	r, err = parseResponse([]byte("HTTP/2.0 200 OK\nA: b\n\n{\"k\":1}\n"))
	if err != nil || r.Status != 200 || string(r.Body) != "{\"k\":1}\n" {
		t.Fatalf("got %+v, %v", r, err)
	}
	if _, err := parseResponse([]byte("gh: not logged in")); err == nil {
		t.Fatal("want an error for output without a status line")
	}
}

func TestIssuesDueDateSources(t *testing.T) {
	gh := newFakeGH(t)
	gh.issues = `{"repository":{"issues":{"nodes":[
	  {"number":1,"title":"all three","body":"Due: 2026-12-01","updatedAt":"2026-10-26T10:00:00Z",
	   "labels":{"nodes":[{"name":"bug"}]},"assignees":{"nodes":[{"login":"ann"}]},
	   "milestone":{"title":"v1","dueOn":"2026-11-20T08:00:00Z"},
	   "projectItems":{"nodes":[{"fieldValues":{"nodes":[{},{"date":"2026-10-01","field":{"name":"Start"}},{"date":"2026-11-05","field":{"name":"Due"}}]}}]}},
	  {"number":2,"title":"milestone and body","body":"Due: 2026-12-01","updatedAt":"2026-10-26T10:00:00Z",
	   "milestone":{"title":"v1","dueOn":"2026-11-20T08:00:00Z"},"projectItems":{"nodes":[]}},
	  {"number":3,"title":"body only","body":"text\nDue: 2026-12-01\n","updatedAt":"2026-10-26T10:00:00Z",
	   "milestone":{"title":"someday","dueOn":null},"projectItems":{"nodes":[]}},
	  {"number":4,"title":"none","body":"","updatedAt":"2026-10-26T10:00:00Z","milestone":null,"projectItems":{"nodes":[]}},
	  {"number":5,"title":"target date","body":"","updatedAt":"2026-10-26T10:00:00Z","milestone":null,
	   "projectItems":{"nodes":[{"fieldValues":{"nodes":[{"date":"2026-11-30","field":{"name":"Target date"}}]}}]}}
	]},"pullRequests":{"nodes":[
	  {"number":9,"title":"fix","url":"u","updatedAt":"2026-10-26T10:00:00Z","isDraft":true,"headRefName":"fix-it",
	   "reviewDecision":"CHANGES_REQUESTED","author":{"login":"bob"},
	   "commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"FAILURE"}}}]}}
	]}}}`
	c := NewClient(gh, nil, func() time.Time { return t0 })
	issues, prs, err := c.IssuesAndPRs(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		due    string
		source DueSource
	}{{"2026-11-05", DueProject}, {"2026-11-20", DueMilestone}, {"2026-12-01", DueBody}, {"", DueNone}, {"2026-11-30", DueProject}}
	if len(issues) != len(want) {
		t.Fatalf("got %d issues", len(issues))
	}
	for i, w := range want {
		is := issues[i]
		if is.DueSource != w.source || (w.due == "" && !is.Due.IsZero()) || (w.due != "" && !is.Due.Equal(date(w.due))) {
			t.Errorf("#%d due = %v from %q, want %s from %q", is.Number, is.Due, is.DueSource, w.due, w.source)
		}
	}
	if is := issues[0]; is.Milestone != "v1" || len(is.Labels) != 1 || is.Labels[0] != "bug" || is.Assignees[0] != "ann" || is.Repo != repo {
		t.Errorf("issue 1 = %+v", is)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d PRs", len(prs))
	}
	if pr := prs[0]; pr.Author != "bob" || !pr.Draft || pr.Branch != "fix-it" || pr.Checks != ChecksFailing || pr.Review != ReviewChangesRequested {
		t.Errorf("pr = %+v", pr)
	}
}

func TestIssuesWithoutProjectScopeFallBack(t *testing.T) {
	gh := newFakeGH(t)
	gh.issues = `{"repository":{"issues":{"nodes":[
	  {"number":1,"title":"t","body":"","updatedAt":"2026-10-26T10:00:00Z",
	   "milestone":{"title":"v1","dueOn":"2026-11-20T08:00:00Z"},"projectItems":null}
	]},"pullRequests":{"nodes":[]}}}`
	gh.gqlErrors = `[{"type":"INSUFFICIENT_SCOPES","message":"Your token has not been granted the required scopes: read:project"}]`
	c := NewClient(gh, nil, func() time.Time { return t0 })
	issues, _, err := c.IssuesAndPRs(context.Background(), repo)
	var partial *PartialError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want a PartialError", err)
	}
	if len(issues) != 1 || issues[0].DueSource != DueMilestone {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestRunsETag304(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "lasso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	gh := newFakeGH(t)
	gh.runs = `{"workflow_runs":[{"id":7,"name":"CI","head_branch":"main","status":"completed","conclusion":"success",
	  "run_attempt":1,"run_started_at":"2026-10-27T08:00:00Z","updated_at":"2026-10-27T08:05:00Z","html_url":"https://x/7"}]}`
	c := NewClient(gh, st, func() time.Time { return t0 })
	ctx := context.Background()

	first, err := c.Runs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Workflow != "CI" || !first[0].Finished.Equal(time.Date(2026, 10, 27, 8, 5, 0, 0, time.UTC)) {
		t.Fatalf("runs = %+v", first)
	}
	if gh.count("If-None-Match") != 0 {
		t.Fatal("first request was conditional")
	}

	// A new client over the same store still has the ETag: the cache, not
	// memory, carries it.
	c = NewClient(gh, st, func() time.Time { return t0.Add(time.Minute) })
	second, err := c.Runs(ctx, repo)
	if err != nil {
		t.Fatalf("304 should not be an error: %v", err)
	}
	if gh.count(`If-None-Match: W/"runs-1"`) != 1 {
		t.Fatalf("second request wasn't conditional: %q", gh.calls[len(gh.calls)-1])
	}
	if len(second) != 1 || second[0] != first[0] {
		t.Fatalf("304 gave %+v, want the cached %+v", second, first)
	}

	// A changed resource gets a new ETag and a new body.
	gh.runsETag = `W/"runs-2"`
	gh.runs = `{"workflow_runs":[]}`
	third, err := c.Runs(ctx, repo)
	if err != nil || len(third) != 0 {
		t.Fatalf("runs = %+v, %v", third, err)
	}
}

func TestRateLimitIsRecordedPerResource(t *testing.T) {
	gh := newFakeGH(t)
	gh.core = rate{5000, 999, time.Unix(1_800_000_000, 0)}
	c := NewClient(gh, nil, func() time.Time { return t0 })
	if _, err := c.Runs(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	rl := c.RateLimit("core")
	if rl.Limit != 5000 || rl.Remaining != 999 || !rl.Low() || !rl.Reset.Equal(time.Unix(1_800_000_000, 0)) {
		t.Fatalf("core = %+v", rl)
	}
	if c.RateLimit("graphql").Low() {
		t.Fatal("graphql limit is low before any graphql call")
	}
	if (RateLimit{Limit: 5000, Remaining: 1000}).Low() {
		t.Fatal("exactly 20% is not below 20%")
	}
}

func TestFailingStep(t *testing.T) {
	jobs := []Job{
		{Name: "lint", Conclusion: "success", Steps: []Step{{"Run lint", "success"}}},
		{Name: "test", Conclusion: "failure", Steps: []Step{{"Checkout", "success"}, {"Go test", "failure"}, {"Upload", "skipped"}}},
		{Name: "build", Conclusion: "failure", Steps: []Step{{"Build", "failure"}}},
	}
	if j, s := FailingStep(jobs); j != "test" || s != "Go test" {
		t.Fatalf("got %q / %q", j, s)
	}
	if j, s := FailingStep([]Job{{Name: "e2e", Conclusion: "timed_out", Steps: []Step{{"Setup", "success"}}}}); j != "e2e" || s != "" {
		t.Fatalf("timed-out job: got %q / %q", j, s)
	}
	if j, s := FailingStep(jobs[:1]); j != "" || s != "" {
		t.Fatalf("no failure: got %q / %q", j, s)
	}
}

func TestSplitRepo(t *testing.T) {
	for _, bad := range []string{"", "acme", "acme/", "/w", "a/b/c"} {
		if _, _, err := splitRepo(bad); err == nil {
			t.Errorf("splitRepo(%q) accepted", bad)
		}
	}
}
