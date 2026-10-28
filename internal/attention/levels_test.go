package attention

import (
	"context"
	"errors"
	"testing"
)

type fixedSorter struct {
	level Level
	err   error
	calls int
}

func (f *fixedSorter) Sort(context.Context, Item) (Level, error) {
	f.calls++
	return f.level, f.err
}

func TestLevelRules(t *testing.T) {
	cases := []struct {
		name string
		kind Kind
		sig  Signals
		want Level
	}{
		{"failing run on default branch", KindRunFailed, Signals{DefaultBranch: true}, Brief},
		{"failing run on a feature branch", KindRunFailed, Signals{}, Log},
		{"passing run", KindRunPassed, Signals{DefaultBranch: true}, Log},
		{"permission prompt no rule covers", KindPermissionPrompt, Signals{}, Ask},
		{"permission prompt a rule covers", KindPermissionPrompt, Signals{CoveredByRule: true}, Log},
		{"bot question", KindBotQuestion, Signals{}, Ask},
		{"bot question a rule covers", KindBotQuestion, Signals{CoveredByRule: true}, Log},
		{"rule acted", KindRuleAction, Signals{}, Log},
		{"rule suggestion", KindRuleSuggestion, Signals{}, Brief},
		{"issue due", KindIssueDue, Signals{}, Brief},
		{"pr merged", KindPRMerged, Signals{}, Brief},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &fixedSorter{level: Ask}
			got := Classify(context.Background(), Item{Kind: c.kind, Signals: c.sig}, s)
			if got.Level != c.want {
				t.Fatalf("level = %s, want %s (rule %s)", got.Level, c.want, got.Rule)
			}
			if s.calls != 0 {
				t.Fatal("a level rule matched, but the sorter was still asked")
			}
			if got.Level == Ask && got.Why == "" {
				t.Fatal("an ask has no reason for the decision card")
			}
		})
	}
}

func TestSorterCanNeverAsk(t *testing.T) {
	for _, l := range []Level{Ask, Level("urgent"), Level("")} {
		got := Classify(context.Background(), Item{Kind: KindBotMessage}, &fixedSorter{level: l})
		if got.Level != Brief || !got.Coerced || got.Rule != "sorter" {
			t.Fatalf("sorter said %q: got %+v, want brief, coerced", l, got)
		}
	}
}

func TestSorterLogAndBrief(t *testing.T) {
	for _, l := range []Level{Log, Brief} {
		got := Classify(context.Background(), Item{Kind: KindBotMessage}, &fixedSorter{level: l})
		if got.Level != l || got.Coerced {
			t.Fatalf("sorter said %s: got %+v", l, got)
		}
	}
}

func TestLeftoversKeptWithoutSorter(t *testing.T) {
	ctx := context.Background()
	if got := Classify(ctx, Item{Kind: KindBotMessage}, nil); got.Level != Brief {
		t.Fatalf("no sorter: %+v", got)
	}
	if got := Classify(ctx, Item{Kind: KindBotMessage}, &fixedSorter{err: errors.New("over cap")}); got.Level != Brief {
		t.Fatalf("sorter failed: %+v", got)
	}
}

// Only level rules may produce an ask; the table must have at least one.
func TestOnlyRulesAsk(t *testing.T) {
	asks := 0
	for _, r := range levelRules {
		if !r.level.Valid() {
			t.Fatalf("rule %s has level %q", r.name, r.level)
		}
		if r.level == Ask {
			asks++
		}
	}
	if asks == 0 {
		t.Fatal("no rule produces an ask")
	}
}
