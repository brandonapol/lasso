package summary

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/config"
	"github.com/brandonapol/lasso/internal/github"
	"github.com/brandonapol/lasso/internal/jev"
	"github.com/brandonapol/lasso/internal/llm"
	"github.com/brandonapol/lasso/internal/store"
)

var t0 = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// secretBody stands for an issue or PR body that must never reach a prompt.
const secretBody = "FULL-BODY-TEXT diff --git a/x b/x"

type fakeFetch struct {
	act   Activity
	calls int
}

func (f *fakeFetch) Activity(_ context.Context, repo string, since time.Time) (Activity, error) {
	f.calls++
	return f.act, nil
}

func (f *fakeFetch) Login(context.Context) (string, error) { return "brandon", nil }

func activity() Activity {
	long := strings.Repeat("very long title ", 50)
	return Activity{
		DefaultBranch: "main",
		Merged: []Item{
			{Number: 12, Title: "Add the digest", URL: "https://github.com/o/r/pull/12", Author: "brandon", At: t0.Add(-time.Hour)},
			{Number: 13, Title: "Bump deps", URL: "https://github.com/o/r/pull/13", Author: "grok-scout", At: t0.Add(-2 * time.Hour)},
			{Number: 14, Title: long, URL: "https://github.com/o/r/pull/14", Author: "grok-scout", At: t0.Add(-3 * time.Hour)},
		},
		MergedTotal: 5,
		Closed:      []Item{{Number: 7, Title: "Crash on start", URL: "https://github.com/o/r/issues/7", Author: "grok-scout"}},
		ClosedTotal: 1,
		Opened:      []Item{{Number: 8, Title: "Flaky test", URL: "https://github.com/o/r/issues/8", Author: "brandon"}},
		OpenedTotal: 2,
		Releases:    []Item{{Title: "v1.2.0", URL: "https://github.com/o/r/releases/tag/v1.2.0", Author: "brandon"}},
		Runs: []github.Run{
			{Workflow: "CI", Branch: "main", Status: "completed", Conclusion: "failure", Started: t0.Add(-time.Hour)},
			{Workflow: "CI", Branch: "main", Status: "completed", Conclusion: "success", Started: t0.Add(-2 * time.Hour)},
			{Workflow: "Lint", Branch: "main", Status: "completed", Conclusion: "success", Started: t0.Add(-2 * time.Hour)},
		},
	}
}

type modelReq struct {
	Model    string `json:"model"`
	System   []struct{ Text string } `json:"system"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// fakeModel serves /v1/messages, answering replies in turn (the last one
// repeats).
func fakeModel(t *testing.T, daily float64, replies ...string) (*llm.Client, *[]modelReq) {
	t.Helper()
	var mu sync.Mutex
	var seen []modelReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body modelReq
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		seen = append(seen, body)
		reply := replies[min(len(seen), len(replies))-1]
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]string{{"type": "thinking", "text": ""}, {"type": "text", "text": reply}},
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 1000, "output_tokens": 200},
		})
	}))
	t.Cleanup(srv.Close)
	c, err := llm.New(llm.Options{Key: "sk", Model: llm.Sonnet, BaseURL: srv.URL, DailyUSD: daily,
		Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	return c, &seen
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "lasso.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

const goodReply = `Here you go: {"paragraph": "Five pull requests went in and version 1.2.0 shipped. The main checks are failing right now.", "changes": [{"item": 1, "title": "Version 1.2.0 shipped"}, {"item": 2, "title": "The digest arrived"}, {"item": 99, "title": "made up"}, {"item": 3, "title": "Updated libraries"}, {"item": 4, "title": "fourth"}], "who": "You did about half; grok-scout did the rest."}`

func newSum(t *testing.T, f Fetcher, c *llm.Client, j *jev.Client, clk *clock) *Summarizer {
	t.Helper()
	return New(Options{Fetch: f, Cache: openStore(t), LLM: c, Jev: j, Now: clk.Now,
		Briefs: func(repo string, _ time.Time) []string { return []string{"grok-scout fixed the crash on start"} }})
}

func TestSummaryParsedCompactAndCounted(t *testing.T) {
	f := &fakeFetch{act: activity()}
	c, seen := fakeModel(t, 5, goodReply)
	clk := &clock{t: t0}
	s := newSum(t, f, c, nil, clk)

	sum, err := s.Refresh(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || (*seen)[0].Model != "claude-sonnet-5-5" {
		t.Fatalf("calls %+v", *seen)
	}
	user := (*seen)[0].Messages[0].Content
	if strings.Contains(user, strings.Repeat("very long title ", 20)) {
		t.Errorf("prompt not compact:\n%s", user)
	}
	if len(user) > 4000 {
		t.Errorf("prompt is %d bytes", len(user))
	}
	for _, want := range []string{"pull requests merged: 5", "issues opened: 2", "2 passed, 1 failed", "you: 3", "grok-scout fixed the crash"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt lacks %q:\n%s", want, user)
		}
	}
	if !strings.Contains((*seen)[0].System[0].Text, `"restack"`) {
		t.Error("system prompt lacks the glossary")
	}

	if sum.Local || sum.OverCap || sum.Model != "claude-sonnet-5-5" || !sum.WrittenAt.Equal(t0) {
		t.Errorf("summary %+v", sum)
	}
	if !strings.HasPrefix(sum.Paragraph, "Five pull requests") || !strings.HasPrefix(sum.Who, "You did") {
		t.Errorf("paragraph/who %q / %q", sum.Paragraph, sum.Who)
	}
	want := []Change{
		{"Version 1.2.0 shipped", "https://github.com/o/r/releases/tag/v1.2.0"},
		{"The digest arrived", "https://github.com/o/r/pull/12"},
		{"Updated libraries", "https://github.com/o/r/pull/13"},
	}
	if fmt.Sprint(sum.Changes) != fmt.Sprint(want) {
		t.Errorf("changes %v", sum.Changes)
	}
	// Counts come from GitHub: totals, not the items' lengths.
	if sum.Counts != (Counts{Merged: 5, Closed: 1, Opened: 2, Releases: 1, RedRuns: 1}) {
		t.Errorf("counts %+v", sum.Counts)
	}
}

func TestCacheHonoursHourlyLimit(t *testing.T) {
	f := &fakeFetch{act: activity()}
	c, seen := fakeModel(t, 5, goodReply)
	clk := &clock{t: t0}
	s := newSum(t, f, c, nil, clk)
	ctx := context.Background()

	if _, ok, _ := s.Get("o/r"); ok {
		t.Fatal("cached before any summary")
	}
	first, err := s.Refresh(ctx, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	clk.add(59 * time.Minute)
	again, err := s.Refresh(ctx, "o/r")
	if err != nil || len(*seen) != 1 || !again.WrittenAt.Equal(first.WrittenAt) {
		t.Fatalf("refresh within the hour called the model: %d calls, %v", len(*seen), err)
	}
	got, ok, err := s.Get("o/r")
	if err != nil || !ok || got.Paragraph != first.Paragraph {
		t.Fatalf("get %+v %v %v", got, ok, err)
	}
	if _, err := s.Regenerate(ctx, "o/r"); err != nil || len(*seen) != 2 {
		t.Fatalf("regenerate didn't force a call: %d %v", len(*seen), err)
	}
	clk.add(30 * time.Minute)
	if _, err := s.Refresh(ctx, "o/r"); err != nil || len(*seen) != 2 {
		t.Fatalf("refresh after a forced regenerate: %d calls", len(*seen))
	}
	clk.add(31 * time.Minute)
	if sum, err := s.Refresh(ctx, "o/r"); err != nil || len(*seen) != 3 || !sum.WrittenAt.Equal(clk.Now()) {
		t.Fatalf("refresh after the hour: %d calls %v", len(*seen), err)
	}
}

func TestOverCapFallsBackToCounts(t *testing.T) {
	f := &fakeFetch{act: activity()}
	c, seen := fakeModel(t, 0, goodReply) // a $0 cap is always reached
	s := newSum(t, f, c, nil, &clock{t: t0})
	sum, err := s.Regenerate(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 0 {
		t.Fatal("called the model over the cap")
	}
	if !sum.OverCap || !sum.Local || sum.Model != "" || !strings.Contains(sum.Note, "cap") {
		t.Errorf("summary %+v", sum)
	}
	want := "In the last 24 hours: 5 pull requests merged, 1 issue closed, 2 issues opened and 1 release. The CI checks are still failing on main."
	if sum.Paragraph != want {
		t.Errorf("paragraph %q", sum.Paragraph)
	}
	if sum.Counts.Merged != 5 || len(sum.Changes) != 3 || sum.Changes[0].URL != "https://github.com/o/r/releases/tag/v1.2.0" {
		t.Errorf("counts/changes %+v %+v", sum.Counts, sum.Changes)
	}
	if sum.Who != "By you (3) and grok-scout (3)." {
		t.Errorf("who %q", sum.Who)
	}
}

func TestJargonIsSentBackOnceThenCountsOnly(t *testing.T) {
	jargon := `{"paragraph": "Two branches were restacked and the merge train is clear.", "changes": [], "who": "Bots."}`
	plain := `{"paragraph": "Two chains of work were rebuilt on the latest code.", "changes": [], "who": "Bots."}`

	c, seen := fakeModel(t, 5, jargon, plain)
	s := newSum(t, &fakeFetch{act: activity()}, c, nil, &clock{t: t0})
	sum, err := s.Regenerate(context.Background(), "o/r")
	if err != nil || len(*seen) != 2 || sum.Local || !strings.HasPrefix(sum.Paragraph, "Two chains") {
		t.Fatalf("re-ask: %d calls %+v %v", len(*seen), sum, err)
	}
	if second := (*seen)[1].Messages[0].Content; !strings.Contains(second, "restacked") || !strings.Contains(second, "merge train") {
		t.Errorf("re-ask didn't name the jargon:\n%s", second)
	}

	c, seen = fakeModel(t, 5, jargon)
	s = newSum(t, &fakeFetch{act: activity()}, c, nil, &clock{t: t0})
	sum, err = s.Regenerate(context.Background(), "o/r")
	if err != nil || len(*seen) != 2 || !sum.Local || sum.OverCap || !strings.Contains(sum.Note, "jargon") {
		t.Fatalf("fallback: %d calls %+v %v", len(*seen), sum, err)
	}
}

func TestUnreadableAnswerFallsBack(t *testing.T) {
	c, seen := fakeModel(t, 5, "sorry, no")
	s := newSum(t, &fakeFetch{act: activity()}, c, nil, &clock{t: t0})
	sum, err := s.Regenerate(context.Background(), "o/r")
	if err != nil || len(*seen) != 2 || !sum.Local || sum.Paragraph == "" {
		t.Fatalf("%d calls %+v %v", len(*seen), sum, err)
	}
}

func TestQuietRepoAndNoKeyMakeNoCall(t *testing.T) {
	c, seen := fakeModel(t, 5, goodReply)
	clk := &clock{t: t0}
	s := New(Options{Fetch: &fakeFetch{}, Cache: openStore(t), LLM: c, Now: clk.Now})
	sum, err := s.Refresh(context.Background(), "o/r")
	if err != nil || len(*seen) != 0 || !sum.Local || !strings.HasPrefix(sum.Paragraph, "Nothing was") {
		t.Fatalf("quiet: %d calls %+v %v", len(*seen), sum, err)
	}
	s = New(Options{Fetch: &fakeFetch{act: activity()}, Cache: openStore(t), Now: clk.Now})
	sum, err = s.Refresh(context.Background(), "o/r")
	if err != nil || !sum.Local || sum.OverCap || !strings.Contains(sum.Note, "key") {
		t.Fatalf("no key: %+v %v", sum, err)
	}
}

func TestJevCutsBeforeSonnet(t *testing.T) {
	var jevCalls int
	jsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jevCalls++
		var body struct {
			State     string                  `json:"state"`
			Questions map[string]jev.Question `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(body.State, "v1.2.0") {
			t.Error("releases went to Jev")
		}
		// Drop the dependency bump confidently; keep everything else.
		answers := map[string]jev.Answer{}
		for k := range body.Questions {
			answers[k] = jev.Answer{Type: "noul", Noul: 0.9, Confidence: 0.9}
			if strings.Contains(itemText(body.State, k), "Bump deps") {
				answers[k] = jev.Answer{Type: "noul", Noul: 0.05, Confidence: 0.95}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "usage": map[string]int{"input_tokens": 100}})
	}))
	t.Cleanup(jsrv.Close)
	env := map[string]string{"JEV_TOKEN": "k", "TYPESAFE_BASE_URL": jsrv.URL}
	j := jev.New(config.Jev{DailyUSD: 1}, func(k string) string { return env[k] }, nil)

	c, seen := fakeModel(t, 5, goodReply)
	s := newSum(t, &fakeFetch{act: activity()}, c, j, &clock{t: t0})
	sum, err := s.Regenerate(context.Background(), "o/r")
	if err != nil || jevCalls != 1 || sum.CutBy != "jev" {
		t.Fatalf("jev: %d calls %+v %v", jevCalls, sum, err)
	}
	user := (*seen)[0].Messages[0].Content
	if strings.Contains(user, "Bump deps") || !strings.Contains(user, "v1.2.0") || !strings.Contains(user, "Add the digest") {
		t.Errorf("cut wrong:\n%s", user)
	}
	if sum.Counts.Merged != 5 {
		t.Errorf("the cut changed the counts: %+v", sum.Counts)
	}
}

// itemText returns the fenced text of item k in a Jev state.
func itemText(state, k string) string {
	i := strings.Index(state, fmt.Sprintf("id=%q>", k))
	if i < 0 {
		return ""
	}
	rest := state[i:]
	if j := strings.Index(rest, "</"); j >= 0 {
		return rest[:j]
	}
	return rest
}

type fakeRunner struct {
	out  string
	args [][]string
}

func (r *fakeRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.args = append(r.args, args)
	if args[1] == "user" {
		return []byte("brandon\n"), nil
	}
	return []byte(r.out), nil
}

func TestGHFetcher(t *testing.T) {
	since := t0.Add(-Window)
	out := `{"data": {
	  "merged": {"issueCount": 41, "nodes": [{"number": 12, "title": "Add the digest", "url": "https://github.com/o/r/pull/12", "mergedAt": "2026-10-27T08:00:00Z", "author": {"login": "brandon"}}, {}]},
	  "closed": {"issueCount": 1, "nodes": [{"number": 7, "title": "Crash", "url": "https://github.com/o/r/issues/7", "closedAt": "2026-10-27T07:00:00Z", "author": null}]},
	  "opened": {"issueCount": 0, "nodes": []},
	  "repository": {"defaultBranchRef": {"name": "main"}, "releases": {"nodes": [
	    {"name": "", "tagName": "v1.2.0", "url": "https://github.com/o/r/releases/tag/v1.2.0", "publishedAt": "2026-10-27T06:00:00Z", "isDraft": false, "author": {"login": "brandon"}},
	    {"name": "Old", "tagName": "v1.1.0", "url": "u", "publishedAt": "2026-10-20T06:00:00Z", "isDraft": false},
	    {"name": "Draft", "tagName": "v2", "url": "u", "publishedAt": null, "isDraft": true}
	  ]}}}}`
	st := openStore(t)
	snap := github.Snapshot{
		Issues: []github.Issue{{Repo: "o/r", Number: 3, Body: secretBody}},
		Runs: []github.Run{
			{Workflow: "CI", Branch: "main", Status: "completed", Conclusion: "failure", Started: t0.Add(-time.Hour)},
			{Workflow: "CI", Branch: "feature", Status: "completed", Conclusion: "failure", Started: t0.Add(-time.Hour)},
			{Workflow: "CI", Branch: "main", Status: "completed", Conclusion: "success", Started: t0.Add(-30 * time.Hour)},
		},
	}
	b, _ := json.Marshal(snap)
	if err := st.CachePut(github.SnapshotKey("o/r"), b, t0, time.Hour); err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{out: out}
	g := GH{Runner: r, Cache: st, Now: func() time.Time { return t0 }}
	a, err := g.Activity(context.Background(), "o/r", since)
	if err != nil {
		t.Fatal(err)
	}
	if a.DefaultBranch != "main" || a.MergedTotal != 41 || len(a.Merged) != 1 || a.Merged[0].Author != "brandon" ||
		len(a.Closed) != 1 || a.Closed[0].Author != "" || len(a.Releases) != 1 || a.Releases[0].Title != "v1.2.0" || len(a.Runs) != 1 {
		t.Errorf("activity %+v", a)
	}
	joined := strings.Join(r.args[0], " ")
	if !strings.Contains(joined, "merged=repo:o/r is:pr is:merged merged:>=2026-10-26T09:00:00Z") || strings.Contains(joined, "body") {
		t.Errorf("query args %q", joined)
	}
	if me, err := g.Login(context.Background()); err != nil || me != "brandon" {
		t.Errorf("login %q %v", me, err)
	}
	if _, err := g.Activity(context.Background(), "nope", since); err == nil {
		t.Error("bad repo accepted")
	}
}
