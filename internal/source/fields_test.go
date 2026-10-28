package source

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/rules"
)

func action(f api.Focus, key string) *api.Action {
	for i := range f.Actions {
		if f.Actions[i].Key == key {
			return &f.Actions[i]
		}
	}
	return nil
}

func section(f api.Focus, label string) string {
	for _, s := range f.Sections {
		if s.Label == label {
			return s.Body
		}
	}
	return "<missing>"
}

// Every row says its right-hand text, its sections and its keys in fields
// of their own; nothing is packed into Status or Body for a client to parse.
func TestExplicitFields(t *testing.T) {
	gh := newFakeGH(t)
	seed(gh)
	s, _ := newTestSource(t, gh, repoA)
	ctx := context.Background()
	s.Step(ctx)

	items, _ := s.Items(ctx, api.TabIssues)
	byID := map[string]api.Item{}
	for _, it := range items {
		byID[it.ID] = it
	}
	due := byID["issue:o:a:3"]
	if due.Right != "due Oct 28" || due.Due == nil || due.Due.Format(time.DateOnly) != "2026-10-28" {
		t.Errorf("due issue: right %q due %v", due.Right, due.Due)
	}
	if none := byID["issue:o:a:1"]; none.Right != "no due date" || none.Due != nil {
		t.Errorf("issue without a due date: right %q due %v", none.Right, none.Due)
	}

	f, _ := s.Focus(ctx, "issue:o:a:3")
	if !strings.Contains(section(f, "Due"), "a Due: line in the issue body") || !strings.HasPrefix(section(f, "What it says"), "Due: 2026-10-28") {
		t.Errorf("issue sections: %+v", f.Sections)
	}
	if o := action(f, "o"); o == nil || o.Kind != api.ActionOpen || action(f, "?") == nil {
		t.Errorf("issue actions: %+v", f.Actions)
	}

	f, _ = s.Focus(ctx, "run:o:a:101")
	if f.Right != "failed" || !strings.Contains(section(f, "What failed"), `"go test"`) {
		t.Errorf("run: right %q sections %+v", f.Right, f.Sections)
	}
}

func TestSummaryCardFields(t *testing.T) {
	s := New(Options{
		Repos: []string{"o/r"}, Runner: ghWith(t, "o/r"), Clock: &fakeClock{now: t0},
		Summaries: fakeSummaries{"o/r": {Repo: "o/r", Paragraph: "Quiet day.", Model: "claude-sonnet-5-5", WrittenAt: t0}},
	})
	ctx := context.Background()
	items, _ := s.Items(ctx, api.TabSummaries)
	if len(items) != 1 || items[0].Byline != "written by claude-sonnet-5-5" || items[0].Body != "Quiet day." {
		t.Fatalf("summary item = %+v", items)
	}
	f, _ := s.Focus(ctx, "summary:o:r")
	g := action(f, "g")
	if g == nil || g.Request == nil || g.Request.Action != "regenerate_summary" || g.Request.Repo != "o/r" {
		t.Fatalf("g = %+v", g)
	}
	if section(f, "The last 24 hours") != "Quiet day." {
		t.Fatalf("sections = %+v", f.Sections)
	}
}

func TestAskActionsAnswerWithChoice(t *testing.T) {
	ev := prompt("p1", "t1")
	ev.Options = append(ev.Options, adapter.Option{N: 3, Label: "Yes, and don't ask again"}, adapter.Option{N: 4, Label: "Something else"})
	bot := &fakeBot{
		sessions: []adapter.Session{{ID: "t1", Bot: "fake", Title: "fix", State: adapter.Waiting}},
		queue:    []adapter.Event{ev},
	}
	s, _ := newBotSource(t, bot, nil)
	ctx := context.Background()
	s.Step(ctx)
	f, err := s.Focus(ctx, "doing:bot:fake:p1")
	if err != nil {
		t.Fatal(err)
	}
	if f.Take == "" || section(f, "What fake said") != "> May I run the tests?" {
		t.Errorf("ask take %q sections %+v", f.Take, f.Sections)
	}
	want := map[string][2]string{"y": {"1", "yes"}, "n": {"2", "no"}, "a": {"3", "yes"}, "b": {"4", "option 4"}}
	for key, w := range want {
		a := action(f, key)
		if a == nil || a.Request == nil {
			t.Fatalf("no action on %s: %+v", key, f.Actions)
		}
		r := a.Request
		if r.Action != rules.AnswerPrompt || r.Bot != "fake-t1" || r.Target != "p1" || r.Args["option"] != w[0] || r.Args["choice"] != w[1] {
			t.Errorf("%s: %+v", key, r)
		}
	}
}

func TestBotStatus(t *testing.T) {
	bot := &fakeBot{sessions: []adapter.Session{
		{ID: "t1", Bot: "fake", State: adapter.Running},
		{ID: "t2", Bot: "fake", State: adapter.Idle},
	}}
	s, clk := newBotSource(t, bot, nil)
	ctx := context.Background()
	s.Step(ctx)
	st, _ := s.Status(ctx)
	if len(st.Bots) != 1 || st.Bots[0] != (api.BotStatus{Kind: "fake", Count: 2, State: api.BotWorking}) || st.GitHubSynced != nil {
		t.Fatalf("status = %+v", st)
	}
	bot.mu.Lock()
	bot.sessions[1].State = adapter.Waiting
	bot.mu.Unlock()
	clk.now = clk.now.Add(BotsEvery)
	s.Step(ctx)
	if st, _ := s.Status(ctx); st.Bots[0].State != api.BotWaiting {
		t.Fatalf("status = %+v", st)
	}
	bot.mu.Lock()
	bot.down = "connection refused"
	bot.mu.Unlock()
	clk.now = clk.now.Add(BotsEvery)
	s.Step(ctx)
	if st, _ := s.Status(ctx); st.Bots[0].State != api.BotUnreachable {
		t.Fatalf("status = %+v", st)
	}
}

func TestGitHubSyncedOnlyWithoutError(t *testing.T) {
	gh := newFakeGH(t)
	seed(gh)
	gh.repo(repoA).fail = "HTTP 401: Bad credentials"
	s, clk := newTestSource(t, gh, repoA)
	ctx := context.Background()
	s.Step(ctx)
	if st, _ := s.Status(ctx); st.GitHubSynced != nil {
		t.Fatalf("synced despite errors: %v", st.GitHubSynced)
	}
	gh.repo(repoA).fail = ""
	clk.now = clk.now.Add(10 * time.Minute)
	s.Step(ctx)
	if st, _ := s.Status(ctx); st.GitHubSynced == nil || !st.GitHubSynced.Equal(clk.now) {
		t.Fatalf("synced = %v, want %v", st.GitHubSynced, clk.now)
	}
}
