package source

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/rules"
)

// fakeBot is an adapter with settable sessions and a needs-you queue. Its
// cursor is the ids it has handed out, like saddle's.
type fakeBot struct {
	mu       sync.Mutex
	sessions []adapter.Session
	queue    []adapter.Event
	// down, when set, is reported through report and reads return nothing,
	// as the saddle adapter does when it can't connect.
	down   string
	err    error
	report func(string)
}

func (f *fakeBot) Kind() string { return "fake" }

func (f *fakeBot) Sessions(context.Context) ([]adapter.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.report != nil {
		f.report(f.down)
	}
	if f.down != "" {
		return nil, nil
	}
	return append([]adapter.Session(nil), f.sessions...), f.err
}

func (f *fakeBot) Events(_ context.Context, since adapter.Cursor) ([]adapter.Event, adapter.Cursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	for _, id := range strings.Split(string(since), ",") {
		seen[id] = true
	}
	var out []adapter.Event
	var ids []string
	for _, e := range f.queue {
		ids = append(ids, e.ID)
		if !seen[e.ID] {
			out = append(out, e)
		}
	}
	return out, adapter.Cursor(strings.Join(ids, ",")), nil
}

func (f *fakeBot) Send(context.Context, string, string) (adapter.Delivery, error) {
	return adapter.Sent, nil
}

func (f *fakeBot) Answer(context.Context, string, adapter.Choice) error { return nil }

func prompt(id, session string) adapter.Event {
	return adapter.Event{
		ID: id, Bot: "fake", Session: session, Kind: attention.KindPermissionPrompt,
		Text: "May I run the tests?", PromptID: id, At: t0.Add(-10 * time.Minute),
		Options: []adapter.Option{{N: 1, Label: "Yes"}, {N: 2, Label: "No"}},
	}
}

func newBotSource(t *testing.T, bot *fakeBot, suggest func(time.Time) ([]rules.Suggestion, error)) (*Source, *fakeClock) {
	t.Helper()
	clk := &fakeClock{now: t0}
	var s *Source
	bot.report = func(msg string) { s.Report("fake", msg) }
	s = New(Options{Runner: newFakeGH(t), Clock: clk, Adapters: []adapter.Adapter{bot}, Suggest: suggest})
	return s, clk
}

func TestBotSessionsAndAsks(t *testing.T) {
	bot := &fakeBot{
		sessions: []adapter.Session{
			{ID: "t1", Bot: "fake", Title: "fix the flaky test", State: adapter.Waiting},
			{ID: "t2", Bot: "fake", Title: "write docs", State: adapter.Running, Writable: true},
		},
		queue: []adapter.Event{prompt("p1", "t1")},
	}
	s, clk := newBotSource(t, bot, nil)
	ctx := context.Background()
	s.Step(ctx)

	// The ask first, then the sessions; the setup row stays, since there
	// are still no hot repos.
	wantIDs(t, s, api.TabDoing, "setup:doing", "doing:bot:fake:p1", "session:fake:t1", "session:fake:t2")
	f, err := s.Focus(ctx, "doing:bot:fake:p1")
	if err != nil {
		t.Fatal(err)
	}
	if f.Status != "needs you" || field(f, "Prompt") != "p1" || field(f, "Options") != "1 Yes; 2 No" ||
		!strings.Contains(field(f, "Why it's asking"), "None of your standing rules") ||
		!strings.Contains(field(f, "In plain words"), "1) Yes; 2) No") || f.Body != "May I run the tests?" {
		t.Errorf("ask focus: %+v", f)
	}
	for _, name := range []string{"Wants", "If yes", "If no", "lasso suggests"} {
		if v := field(f, name); v == "" || v == "<missing>" {
			t.Errorf("card field %s is empty", name)
		}
	}
	if f, _ := s.Focus(ctx, "session:fake:t1"); f.Status != "waiting on you" || field(f, "Can lasso send to it") == "yes" {
		t.Errorf("session focus: %+v", f)
	}

	// Answered through lasso: gone at once, without waiting for a read.
	s.Resolve("fake", "p1")
	s.refresh(ctx)
	wantIDs(t, s, api.TabDoing, "setup:doing", "session:fake:t1", "session:fake:t2")

	// A new prompt whose session then moves on is dropped on the next read.
	bot.mu.Lock()
	bot.queue = []adapter.Event{prompt("p1", "t1"), prompt("p2", "t1")}
	bot.mu.Unlock()
	clk.now = clk.now.Add(BotsEvery)
	s.Step(ctx)
	wantIDs(t, s, api.TabDoing, "setup:doing", "doing:bot:fake:p2", "session:fake:t1", "session:fake:t2")
	bot.mu.Lock()
	bot.sessions[0].State = adapter.Running
	bot.mu.Unlock()
	clk.now = clk.now.Add(BotsEvery)
	s.Step(ctx)
	wantIDs(t, s, api.TabDoing, "setup:doing", "session:fake:t1", "session:fake:t2")
}

func TestBotTroubleIsOneBriefThatClears(t *testing.T) {
	bot := &fakeBot{
		sessions: []adapter.Session{{ID: "t1", Bot: "fake", Title: "fix it", State: adapter.Waiting}},
		queue:    []adapter.Event{prompt("p1", "t1")},
	}
	s, clk := newBotSource(t, bot, nil)
	ctx := context.Background()
	s.Step(ctx)

	ch, stop := s.Subscribe()
	defer stop()
	bot.mu.Lock()
	bot.down = "fake isn't running"
	bot.mu.Unlock()
	for range 3 {
		clk.now = clk.now.Add(BotsEvery)
		s.Step(ctx)
	}
	// One brief item, and the ask it had waiting is kept.
	wantIDs(t, s, api.TabDoing, "bot-error:fake", "setup:doing", "doing:bot:fake:p1", "session:fake:t1")
	if got := eventIDs(collect(ch), api.EventItemChanged); strings.Join(got, " ") != "bot-error:fake" {
		t.Errorf("a bot down the same way for three reads sent item.changed for %q", got)
	}
	f, _ := s.Focus(ctx, "bot-error:fake")
	if !strings.Contains(field(f, "What happened"), "isn't running") || field(f, "Since") != t0.Add(BotsEvery).UTC().Format(time.RFC3339) {
		t.Errorf("error focus: %+v", f)
	}

	bot.mu.Lock()
	bot.down = ""
	bot.mu.Unlock()
	clk.now = clk.now.Add(BotsEvery)
	s.Step(ctx)
	wantIDs(t, s, api.TabDoing, "setup:doing", "doing:bot:fake:p1", "session:fake:t1")

	// A read that fails outright is a problem too, until a read works.
	bot.mu.Lock()
	bot.err = errors.New("status: boom")
	bot.mu.Unlock()
	clk.now = clk.now.Add(BotsEvery)
	s.Step(ctx)
	if f, _ := s.Focus(ctx, "bot-error:fake"); !strings.Contains(field(f, "What happened"), "boom") {
		t.Errorf("error focus: %+v", f)
	}
	bot.mu.Lock()
	bot.err = nil
	bot.mu.Unlock()
	clk.now = clk.now.Add(BotsEvery)
	s.Step(ctx)
	wantIDs(t, s, api.TabDoing, "setup:doing", "doing:bot:fake:p1", "session:fake:t1")
}

func TestSuggestionIsABriefThatKeepsItsTime(t *testing.T) {
	answers := []rules.Answer{}
	for i := range 3 {
		answers = append(answers, rules.Answer{Bot: "grok-scout", Action: rules.AnswerPrompt, Choice: "yes", Repo: "o/r", At: t0.Add(-time.Duration(i) * time.Hour)})
	}
	suggest := func(now time.Time) ([]rules.Suggestion, error) {
		return rules.Suggest(answers, rules.File{}, now), nil
	}
	s, clk := newBotSource(t, &fakeBot{}, suggest)
	ctx := context.Background()
	s.Step(ctx)
	id := "rule-suggestion:grok-scout-answer-prompt"
	wantIDs(t, s, api.TabSummaries, id, "setup:summaries")
	f, _ := s.Focus(ctx, id)
	if f.Status != "suggested rule" || field(f, "In") != "o/r" || !strings.Contains(f.Body, `who = "grok-scout"`) || !f.Updated.Equal(t0) {
		t.Errorf("suggestion focus: %+v", f)
	}

	ch, stop := s.Subscribe()
	defer stop()
	clk.now = clk.now.Add(time.Hour)
	s.Step(ctx)
	if evs := collect(ch); len(evs) != 0 {
		t.Errorf("a standing suggestion sent %+v", evs)
	}
}
