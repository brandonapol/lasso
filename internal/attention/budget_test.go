package attention

import (
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/config"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func ask(id string, created time.Time) Item {
	return Item{ID: id, Kind: KindPermissionPrompt, Level: Ask, Bot: "grok-scout", Created: created}
}

func ids(shown []Shown) []string {
	var out []string
	for _, s := range shown {
		out = append(out, s.Item.ID)
	}
	return out
}

func itemIDs(items []Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func newGate(t *testing.T, a config.Attention, c *fakeClock) *Gate {
	t.Helper()
	g, err := NewGate(a, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBudgetQueuesThirdAsk(t *testing.T) {
	c := &fakeClock{noon}
	g := newGate(t, config.Default().Attention, c)
	asks := []Item{ask("a3", noon.Add(-time.Minute)), ask("a1", noon.Add(-3*time.Minute)), ask("a2", noon.Add(-2*time.Minute))}

	v := g.Present(asks)
	if got := ids(v.Now); !equal(got, []string{"a1", "a2"}) {
		t.Fatalf("now = %v, want the two oldest", got)
	}
	if got := itemIDs(v.Waiting); !equal(got, []string{"a3"}) {
		t.Fatalf("waiting = %v, want a3", got)
	}

	// Refreshing doesn't spend more budget, and answering doesn't refund it.
	c.now = noon.Add(time.Hour)
	v = g.Present(asks[:1]) // a1 and a2 answered
	if len(v.Now) != 0 || !equal(itemIDs(v.Waiting), []string{"a3"}) {
		t.Fatalf("after answering: now %v waiting %v", ids(v.Now), itemIDs(v.Waiting))
	}

	// The In progress tab shows it anyway, at no cost.
	if got := ids(InProgress(asks[:1], c.now)); !equal(got, []string{"a3"}) {
		t.Fatalf("in progress = %v", got)
	}
	if v = g.Present(asks[:1]); len(v.Now) != 0 {
		t.Fatal("looking at In progress spent budget")
	}

	// Next local day, it's presented.
	c.now = time.Date(2026, 10, 28, 9, 0, 0, 0, tz)
	if v = g.Present(asks[:1]); !equal(ids(v.Now), []string{"a3"}) {
		t.Fatalf("next day: now = %v", ids(v.Now))
	}
}

func TestPresentedAskStaysAcrossDays(t *testing.T) {
	c := &fakeClock{noon}
	g := newGate(t, config.Attention{AsksPerDay: 1}, c)
	old := ask("old", noon)
	g.Present([]Item{old})

	c.now = noon.Add(24 * time.Hour)
	v := g.Present([]Item{old, ask("new", c.now)})
	if !equal(ids(v.Now), []string{"old", "new"}) {
		t.Fatalf("now = %v: yesterday's open ask should stay and not count today", ids(v.Now))
	}
}

func TestZeroBudgetPresentsNothing(t *testing.T) {
	c := &fakeClock{noon}
	g := newGate(t, config.Attention{AsksPerDay: 0}, c)
	v := g.Present([]Item{ask("a", noon)})
	if len(v.Now) != 0 || len(v.Waiting) != 1 {
		t.Fatalf("view = %+v", v)
	}
}

func TestQuietHours(t *testing.T) {
	cases := []struct {
		hours string
		at    string
		quiet bool
	}{
		{"22:00-08:00", "23:30", true},
		{"22:00-08:00", "00:00", true},
		{"22:00-08:00", "07:59", true},
		{"22:00-08:00", "08:00", false},
		{"22:00-08:00", "21:59", false},
		{"22:00-08:00", "22:00", true},
		{"13:00-14:00", "13:30", true},
		{"13:00-14:00", "14:00", false},
		{"13:00-14:00", "12:59", false},
		{"09:00-09:00", "09:00", false},
		{"", "03:00", false},
	}
	for _, c := range cases {
		q, err := ParseQuietHours(config.Attention{QuietHours: c.hours})
		if err != nil {
			t.Fatal(err)
		}
		at, _ := time.Parse("15:04", c.at)
		now := time.Date(2026, 10, 27, at.Hour(), at.Minute(), 0, 0, tz)
		if got := q.Contains(now); got != c.quiet {
			t.Errorf("%q at %s: quiet = %v, want %v", c.hours, c.at, got, c.quiet)
		}
	}
}

func TestQuietHoursHoldEverythingButLineJumpers(t *testing.T) {
	night := time.Date(2026, 10, 27, 23, 0, 0, 0, tz)
	c := &fakeClock{night}
	g := newGate(t, config.Default().Attention, c)

	blocked := Item{ID: "blocked", Kind: KindBotQuestion, Level: Ask, Bot: "grok-scout", Created: night.Add(-3 * time.Hour),
		Signals: Signals{BlockedSince: night.Add(-3 * time.Hour)}}
	v := g.Present([]Item{ask("a1", night), blocked})
	if !equal(ids(v.Now), []string{"blocked"}) || v.Now[0].LineJumper == "" {
		t.Fatalf("now = %v, want only the line-jumper", ids(v.Now))
	}
	if !equal(itemIDs(v.Waiting), []string{"a1"}) {
		t.Fatalf("waiting = %v", itemIDs(v.Waiting))
	}

	// Held asks spent no budget: at 08:00 both slots are free.
	c.now = time.Date(2026, 10, 28, 8, 0, 0, 0, tz)
	v = g.Present([]Item{ask("a1", night), ask("a2", night.Add(time.Minute))})
	if !equal(ids(v.Now), []string{"a1", "a2"}) {
		t.Fatalf("morning: now = %v", ids(v.Now))
	}
}

func TestLineJumpersSkipBudget(t *testing.T) {
	c := &fakeClock{noon}
	g := newGate(t, config.Attention{AsksPerDay: 1}, c)
	red := Item{ID: "red", Kind: KindRunFailed, Level: Brief, Repo: "a/b",
		Signals: Signals{DefaultBranch: true, HotRepo: true, RedSince: noon.Add(-time.Hour)}}
	v := g.Present([]Item{red, ask("a1", noon), ask("a2", noon.Add(time.Second))})
	if !equal(ids(v.Now), []string{"red", "a1"}) || !equal(itemIDs(v.Waiting), []string{"a2"}) {
		t.Fatalf("now %v waiting %v", ids(v.Now), itemIDs(v.Waiting))
	}
}

func TestBriefAndLogNeverPresented(t *testing.T) {
	c := &fakeClock{noon}
	g := newGate(t, config.Default().Attention, c)
	v := g.Present([]Item{{ID: "b", Level: Brief, Kind: KindPRMerged}, {ID: "l", Level: Log, Kind: KindRunPassed}})
	if len(v.Now)+len(v.Waiting) != 0 {
		t.Fatalf("view = %+v", v)
	}
}
