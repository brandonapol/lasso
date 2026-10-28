package attention

import (
	"testing"
	"time"
)

var (
	tz   = time.FixedZone("AKST", -9*3600)
	noon = time.Date(2026, 10, 27, 12, 0, 0, 0, tz)
)

func TestLineJumpers(t *testing.T) {
	cases := []struct {
		name string
		it   Item
		want bool
	}{
		// Default branch red in a hot repo.
		{"red 31m in hot repo", Item{Kind: KindRunFailed, Repo: "a/b", Signals: Signals{DefaultBranch: true, HotRepo: true, RedSince: noon.Add(-31 * time.Minute)}}, true},
		{"red 30m exactly", Item{Kind: KindRunFailed, Signals: Signals{DefaultBranch: true, HotRepo: true, RedSince: noon.Add(-30 * time.Minute)}}, false},
		{"red 2h in a cold repo", Item{Kind: KindRunFailed, Signals: Signals{DefaultBranch: true, RedSince: noon.Add(-2 * time.Hour)}}, false},
		{"red 2h on a feature branch", Item{Kind: KindRunFailed, Signals: Signals{HotRepo: true, RedSince: noon.Add(-2 * time.Hour)}}, false},
		{"failed run, red since unknown", Item{Kind: KindRunFailed, Signals: Signals{DefaultBranch: true, HotRepo: true}}, false},

		// Due within 24h with no progress.
		{"due in 23h, never touched", Item{Kind: KindIssueDue, Title: "Ship it", Due: noon.Add(23 * time.Hour)}, true},
		{"due in 23h, stale progress", Item{Kind: KindIssueDue, Due: noon.Add(23 * time.Hour), Signals: Signals{LastProgress: noon.Add(-25 * time.Hour)}}, true},
		{"overdue, never touched", Item{Kind: KindIssueDue, Due: noon.Add(-time.Hour)}, true},
		{"due in 23h, recent progress", Item{Kind: KindIssueDue, Due: noon.Add(23 * time.Hour), Signals: Signals{LastProgress: noon.Add(-time.Hour)}}, false},
		{"due in 25h, never touched", Item{Kind: KindIssueDue, Due: noon.Add(25 * time.Hour)}, false},
		{"no due date", Item{Kind: KindIssueUpdated}, false},

		// A bot blocked on the user.
		{"blocked 2h1m", Item{Kind: KindBotQuestion, Bot: "grok-scout", Signals: Signals{BlockedSince: noon.Add(-121 * time.Minute)}}, true},
		{"blocked 2h exactly", Item{Kind: KindBotQuestion, Bot: "grok-scout", Signals: Signals{BlockedSince: noon.Add(-2 * time.Hour)}}, false},
		{"blocked 1h", Item{Kind: KindPermissionPrompt, Bot: "grok-scout", Signals: Signals{BlockedSince: noon.Add(-time.Hour)}}, false},

		// Nothing else jumps, however loud it looks.
		{"ordinary ask", Item{Kind: KindPermissionPrompt, Level: Ask, Bot: "grok-scout"}, false},
		{"bot chatter", Item{Kind: KindBotMessage, Level: Brief, Bot: "grok-scout"}, false},
		{"merged pr", Item{Kind: KindPRMerged, Level: Brief, Repo: "a/b", Signals: Signals{HotRepo: true, DefaultBranch: true}}, false},
		{"passing run", Item{Kind: KindRunPassed, Signals: Signals{HotRepo: true, DefaultBranch: true, RedSince: noon.Add(-time.Hour)}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, ok := LineJumper(c.it, noon)
			if ok != c.want {
				t.Fatalf("jumps = %v, want %v (%q)", ok, c.want, reason)
			}
			if ok && reason == "" {
				t.Fatal("a line-jumper with no reason")
			}
		})
	}
}
