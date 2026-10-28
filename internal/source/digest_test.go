package source

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/digest"
	"github.com/brandonapol/lasso/internal/summary"
)

type fakeSummaries map[string]summary.Summary

func (f fakeSummaries) Get(repo string) (summary.Summary, bool, error) {
	s, ok := f[repo]
	return s, ok, nil
}

func ghWith(t *testing.T, repo string) *fakeGH {
	gh := newFakeGH(t)
	gh.repo(repo)
	return gh
}

func chat(id, session, text string, kind attention.Kind) adapter.Event {
	return adapter.Event{ID: id, Bot: "fake", Session: session, Kind: kind, Text: adapter.Untrusted(text), At: t0.Add(-time.Minute)}
}

func rowIDs(t *testing.T, s *Source, tab api.Tab) []string {
	t.Helper()
	items, err := s.Items(context.Background(), tab)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

// With a digest, a bot's chatter is digested rather than listed: only what
// it waits on the user for is an item, the digest's brief points are
// brief items tied to the session's repo, and the session's row shows its
// "N → k".
func TestDigestTakesBotChatter(t *testing.T) {
	bot := &fakeBot{
		sessions: []adapter.Session{{ID: "s1", Bot: "fake", Title: "sweep", State: adapter.Waiting}},
		queue: []adapter.Event{
			chat("e1", "s1", "Read three files.", attention.KindBotMessage),
			chat("e2", "s1", "Ran the tests; two fail.", digest.KindTurnEnd),
			prompt("p1", "s1"),
		},
	}
	clk := &fakeClock{now: t0}
	dg := digest.New(digest.Options{Now: clk.Now})
	s := New(Options{
		Repos: []string{"o/r"}, Runner: ghWith(t, "o/r"), Clock: clk, Adapters: []adapter.Adapter{bot},
		Digest: dg, Summaries: fakeSummaries{"o/r": {Repo: "o/r", Paragraph: "Quiet day.", Local: true, Note: "No key.", WrittenAt: t0}},
		SessionRepo: func(kind string, se adapter.Session) string { return "o/r" },
		Notices:     []Notice{{ID: "keys:jev", Title: "Some features are off: no Jev key", Body: "Set JEV_TOKEN.", At: t0}},
	})
	ctx := context.Background()
	s.Step(ctx)
	s.DigestStep(ctx)
	s.Step(ctx)

	doing := rowIDs(t, s, api.TabDoing)
	if !slices.Contains(doing, "doing:bot:fake:p1") || slices.Contains(doing, "doing:bot:fake:e1") {
		t.Fatalf("doing = %q", doing)
	}
	row, err := s.Focus(ctx, "session:fake:s1")
	if err != nil || !strings.HasSuffix(row.Status, "3 → 2") || row.Repo != "o/r" {
		// Two batches: chatter + turn end (2 → 1), then the prompt (1 → 1).
		t.Fatalf("session row = %+v %v", row.Item, err)
	}
	sums := rowIDs(t, s, api.TabSummaries)
	if len(sums) < 4 || sums[0] != "notice:keys:jev" || sums[1] != "summary:o:r" || !strings.HasPrefix(sums[2], "digest:") {
		t.Fatalf("summaries = %q", sums)
	}
	f, _ := s.Focus(ctx, "summary:o:r")
	if f.Status != "counts only" || f.Body != "Quiet day." {
		t.Fatalf("summary row = %+v", f)
	}
	if got := s.Briefs("O/R", t0.Add(-time.Hour)); len(got) != 2 || !strings.Contains(got[0], "from s1") {
		t.Fatalf("briefs = %q", got)
	}
	if got := s.Briefs("o/other", time.Time{}); len(got) != 0 {
		t.Fatalf("briefs for another repo = %q", got)
	}

	// Points age out after a day.
	clk.now = t0.Add(PointsKept + time.Hour)
	s.keepPoints(nil)
	if got := s.Briefs("o/r", time.Time{}); len(got) != 0 {
		t.Fatalf("old points kept: %q", got)
	}
}

func TestSummaryRowStates(t *testing.T) {
	f := summaryFocus(summary.Summary{Repo: "o/r", Paragraph: "p", OverCap: true, Local: true, WrittenAt: t0}, api.Focus{})
	if f.Status != "counts only: cap reached" || !slices.ContainsFunc(f.Fields, func(x api.Field) bool { return strings.Contains(x.Value, "sonnet_daily_usd") }) {
		t.Fatalf("over cap = %+v", f)
	}
	s := New(Options{Repos: []string{"o/r"}, Runner: ghWith(t, "o/r"), Clock: &fakeClock{now: t0}, Summaries: fakeSummaries{}})
	got, err := s.Focus(context.Background(), "summary:o:r")
	if err != nil || got.Status != "writing" {
		t.Fatalf("no summary yet = %+v %v", got, err)
	}
}
