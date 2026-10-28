package source

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/github"
)

var t0 = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

const repoA, repoB = "o/a", "o/b"

// seed fills repo A with a bit of everything and leaves repo B quiet.
func seed(gh *fakeGH) {
	a := gh.repo(repoA)
	msDue := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	long := "Due: 2026-10-28\n" + strings.Repeat("é", 2500)
	a.issues = []map[string]any{
		issue(1, "no due date", "", t0.Add(-time.Hour), nil),
		issue(2, "milestone due", "", t0.Add(-2*time.Hour), &msDue),
		issue(3, "due tomorrow, stale", long, t0.Add(-72*time.Hour), nil),
	}
	a.prs = []map[string]any{
		pr(10, "mine, needs review", "me", false, "REVIEW_REQUIRED", "SUCCESS", t0.Add(-time.Hour)),
		pr(11, "mine, draft", "me", true, "", "", t0.Add(-time.Hour)),
		pr(12, "someone else's", "them", false, "REVIEW_REQUIRED", "SUCCESS", t0.Add(-time.Hour)),
		pr(13, "mine, failing", "me", false, "", "FAILURE", t0.Add(-time.Hour)),
	}
	a.runs = []map[string]any{
		run(100, "CI", "main", "in_progress", "", t0.Add(-5*time.Minute), t0),
		run(101, "CI", "main", "completed", "failure", t0.Add(-2*time.Hour), t0.Add(-90*time.Minute)),
		run(102, "Lint", "feature", "completed", "success", t0.Add(-time.Hour), t0.Add(-50*time.Minute)),
	}
	a.jobs[101] = `{"jobs":[{"name":"test","conclusion":"failure","steps":[{"name":"checkout","conclusion":"success"},{"name":"go test","conclusion":"failure"}]}]}`
	gh.repo(repoB)
}

func newTestSource(t *testing.T, gh *fakeGH, repos ...string) (*Source, *fakeClock) {
	clk := &fakeClock{now: t0}
	return New(Options{Repos: repos, Runner: gh, Clock: clk}), clk
}

func ids(t *testing.T, s *Source, tab api.Tab) []string {
	t.Helper()
	items, err := s.Items(context.Background(), tab)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func wantIDs(t *testing.T, s *Source, tab api.Tab, want ...string) {
	t.Helper()
	if got := ids(t, s, tab); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("%s tab:\n got %q\nwant %q", tab, got, want)
	}
}

func field(f api.Focus, name string) string {
	for _, fl := range f.Fields {
		if fl.Name == name {
			return fl.Value
		}
	}
	return "<missing>"
}

func TestTabs(t *testing.T) {
	gh := newFakeGH(t)
	seed(gh)
	s, _ := newTestSource(t, gh, repoA, repoB)
	s.Step(context.Background())

	// Issues: soonest due first, undated last.
	wantIDs(t, s, api.TabIssues, "issue:o:a:3", "issue:o:a:2", "issue:o:a:1")
	// Actions: running first, then the most recent.
	wantIDs(t, s, api.TabActions, "run:o:a:100", "run:o:a:102", "run:o:a:101")
	// In progress: line-jumpers (an issue due tomorrow untouched for 3
	// days; main red for 90 minutes), then the user's PR waiting on them.
	// Not the draft, not someone else's, not the one with failing checks.
	wantIDs(t, s, api.TabDoing, "doing:issue:o:a:3", "doing:run:o:a:101", "pr:o:a:10")
	wantIDs(t, s, api.TabSummaries)
}

func TestFocus(t *testing.T) {
	gh := newFakeGH(t)
	seed(gh)
	s, _ := newTestSource(t, gh, repoA)
	s.Step(context.Background())
	ctx := context.Background()

	f, err := s.Focus(ctx, "issue:o:a:3")
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "#3 due tomorrow, stale" || f.Repo != repoA || f.Status != "due 2026-10-28" {
		t.Errorf("issue item: %+v", f.Item)
	}
	if field(f, "Due") != "2026-10-28" || field(f, "Due from") != "a Due: line in the issue body" ||
		field(f, "Labels") != "bug" || field(f, "Assignees") != "me" {
		t.Errorf("issue fields: %+v", f.Fields)
	}
	if n := utf8.RuneCountInString(f.Body); n != BodyLimit+1 || !strings.HasSuffix(f.Body, "…") {
		t.Errorf("body is %d runes, want %d plus the cut mark", n, BodyLimit)
	}
	f, _ = s.Focus(ctx, "issue:o:a:2")
	if field(f, "Due from") != "milestone v1" || field(f, "Milestone") != "v1" {
		t.Errorf("milestone issue fields: %+v", f.Fields)
	}

	f, err = s.Focus(ctx, "run:o:a:101")
	if err != nil {
		t.Fatal(err)
	}
	if field(f, "Workflow") != "CI" || field(f, "Branch") != "main" || field(f, "Status") != "failed" ||
		field(f, "Failing job") != "test" || field(f, "Failing step") != "go test" ||
		field(f, "URL") != "https://github.com/o/r/actions/runs/101" {
		t.Errorf("run fields: %+v", f.Fields)
	}

	f, err = s.Focus(ctx, "pr:o:a:10")
	if err != nil {
		t.Fatal(err)
	}
	if field(f, "Checks") != "passing" || field(f, "Review") != "review required" || f.Status != "waiting on your review" {
		t.Errorf("pr: %+v", f)
	}

	f, _ = s.Focus(ctx, "doing:run:o:a:101")
	if !strings.Contains(field(f, "Why it's here"), "failing for 90 minutes") {
		t.Errorf("line-jumper focus: %+v", f.Fields)
	}

	if _, err := s.Focus(ctx, "nope"); err != api.ErrNotFound {
		t.Errorf("unknown id: %v", err)
	}
}

// collect drains what is already queued on ch.
func collect(ch <-chan api.Event) []api.Event {
	var out []api.Event
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func eventIDs(evs []api.Event, kind string) []string {
	var out []string
	for _, ev := range evs {
		if ev.Kind != kind {
			continue
		}
		switch d := ev.Data.(type) {
		case api.Item:
			out = append(out, d.ID)
		case map[string]string:
			out = append(out, d["id"])
		default:
			out = append(out, "")
		}
	}
	return out
}

func TestEvents(t *testing.T) {
	gh := newFakeGH(t)
	seed(gh)
	s, clk := newTestSource(t, gh, repoA)
	ctx := context.Background()
	s.Step(ctx)
	ch, stop := s.Subscribe()
	defer stop()

	// Nothing changed: a poll sends only the new sync time.
	clk.now = clk.now.Add(github.IssuesEvery)
	s.Step(ctx)
	evs := collect(ch)
	if len(evs) != 1 || evs[0].Kind != api.EventStatusChanged {
		t.Fatalf("quiet poll sent %v", evs)
	}
	if st := evs[0].Data.(api.Status); st.GitHubSynced == nil || !st.GitHubSynced.Equal(clk.now) {
		t.Fatalf("status = %+v", st)
	}

	a := gh.repo(repoA)
	a.issues[0] = issue(1, "renamed", "", clk.now, nil)
	a.issues = a.issues[:2] // #3 closed
	clk.now = clk.now.Add(github.IssuesEvery)
	s.Step(ctx)
	evs = collect(ch)
	if got := eventIDs(evs, api.EventItemChanged); strings.Join(got, " ") != "issue:o:a:1" {
		t.Errorf("item.changed: %q", got)
	}
	if got := eventIDs(evs, api.EventItemRemoved); strings.Join(got, " ") != "doing:issue:o:a:3 issue:o:a:3" {
		t.Errorf("item.removed: %q", got)
	}
	if got := eventIDs(evs, api.EventBriefUpdated); len(got) != 1 {
		t.Errorf("brief.updated sent %d times, want 1", len(got))
	}
}

func TestEventsOverSSE(t *testing.T) {
	gh := newFakeGH(t)
	seed(gh)
	s, clk := newTestSource(t, gh, repoA)
	ctx := context.Background()
	s.Step(ctx)
	srv := httptest.NewServer(api.Handler(s, api.Options{Token: "tok", Port: 7337}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if _, err := r.ReadString('\n'); err != nil { // ": lasso events"
		t.Fatal(err)
	}

	gh.repo(repoA).issues[0] = issue(1, "renamed", "", clk.now, nil)
	clk.now = clk.now.Add(github.IssuesEvery)
	s.Step(ctx)

	got := make(chan string, 1)
	go func() {
		var kind string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if k, ok := strings.CutPrefix(line, "event: "); ok {
				kind = strings.TrimSpace(k)
			}
			if d, ok := strings.CutPrefix(line, "data: "); ok && kind == api.EventItemChanged {
				var it api.Item
				_ = json.Unmarshal([]byte(d), &it)
				got <- it.ID + " " + it.Title
				return
			}
		}
	}()
	select {
	case v := <-got:
		if v != "issue:o:a:1 #1 renamed" {
			t.Errorf("event: %q", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no item.changed event")
	}
}

func TestNoHotRepos(t *testing.T) {
	s, _ := newTestSource(t, newFakeGH(t))
	for _, tab := range api.Tabs {
		items, _ := s.Items(context.Background(), tab)
		if len(items) != 1 || !strings.Contains(items[0].Title, "hot_repos") {
			t.Fatalf("%s: %+v", tab, items)
		}
		f, err := s.Focus(context.Background(), items[0].ID)
		if err != nil || !strings.Contains(f.Body, `hot_repos = ["owner/name"`) {
			t.Errorf("%s focus: %+v %v", tab, f, err)
		}
	}
}

func TestGitHubErrorIsOneBriefThatClears(t *testing.T) {
	gh := newFakeGH(t)
	seed(gh)
	gh.repo(repoB).fail = "gh: To get started with GitHub CLI, please run:  gh auth login"
	s, clk := newTestSource(t, gh, repoA, repoB)
	ctx := context.Background()
	s.Step(ctx)

	wantIDs(t, s, api.TabIssues, "error:o:b:issues", "issue:o:a:3", "issue:o:a:2", "issue:o:a:1")
	f, _ := s.Focus(ctx, "error:o:b:actions")
	if !strings.Contains(field(f, "What happened"), "gh auth login") {
		t.Errorf("error focus: %+v", f)
	}

	ch, stop := s.Subscribe()
	defer stop()
	for range 3 {
		clk.now = clk.now.Add(github.MilestonesEvery)
		s.Step(ctx)
	}
	for _, ev := range collect(ch) {
		if ev.Kind == api.EventBriefUpdated || strings.HasPrefix(strings.Join(eventIDs([]api.Event{ev}, ev.Kind), ""), "error:") {
			t.Errorf("a repo still failing the same way sent %s %v", ev.Kind, ev.Data)
		}
	}

	gh.repo(repoB).fail = ""
	clk.now = clk.now.Add(github.MilestonesEvery)
	s.Step(ctx)
	evs := collect(ch)
	if got := eventIDs(evs, api.EventItemRemoved); strings.Join(got, " ") != "error:o:b:actions error:o:b:issues" {
		t.Errorf("item.removed: %q", got)
	}
	wantIDs(t, s, api.TabIssues, "issue:o:a:3", "issue:o:a:2", "issue:o:a:1")
}

// A partial GraphQL error must outlive a runs poll that succeeds in between;
// only the issues poll can clear it.
func TestPartialErrorClearsOnlyWithItsGroup(t *testing.T) {
	gh := newFakeGH(t)
	seed(gh)
	a := gh.repo(repoA)
	a.partial = "Your token has not been granted the required scopes to execute this query (read:project)."
	s, clk := newTestSource(t, gh, repoA)
	ctx := context.Background()
	s.Step(ctx)
	if _, err := s.Focus(ctx, "error:o:a:issues"); err != nil {
		t.Fatalf("no error item: %v", err)
	}
	if got := ids(t, s, api.TabIssues); len(got) != 4 {
		t.Errorf("partial data should still show the issues: %q", got)
	}

	a.partial = ""
	clk.now = clk.now.Add(github.RunsActiveEvery) // only runs are due (one is in progress)
	s.Step(ctx)
	if _, err := s.Focus(ctx, "error:o:a:issues"); err != nil {
		t.Fatal("a runs poll cleared the issues error")
	}
	// The fake sends the partial error with the milestones query too, so
	// the item lasts until both GraphQL groups have come back clean.
	clk.now = t0.Add(github.IssuesEvery)
	s.Step(ctx)
	if _, err := s.Focus(ctx, "error:o:a:issues"); err != nil {
		t.Fatal("the issues poll cleared the milestones error")
	}
	clk.now = t0.Add(github.MilestonesEvery)
	s.Step(ctx)
	if _, err := s.Focus(ctx, "error:o:a:issues"); err != api.ErrNotFound {
		t.Fatalf("error item still there after every poll succeeded: %v", err)
	}
}
