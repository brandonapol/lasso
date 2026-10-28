package attention

import "context"

// levelRule is one row of the level table: the first row whose kind matches
// and whose condition holds decides the level.
type levelRule struct {
	name  string
	kind  Kind
	when  func(Signals) bool // nil means always
	level Level
	// why is the plain reason shown on a decision card ("why it's asking").
	why string
}

func always(Signals) bool { return true }

// levelRules is the whole deterministic policy. Keep it short and quiet:
// anything that isn't here goes to the Sorter, which can only pick log or
// brief. Only rows here may produce an ask.
var levelRules = []levelRule{
	{name: "default-branch-run-failed", kind: KindRunFailed, when: func(s Signals) bool { return s.DefaultBranch }, level: Brief},
	{name: "branch-run-failed", kind: KindRunFailed, when: always, level: Log},
	{name: "run-passed", kind: KindRunPassed, when: always, level: Log},
	{name: "permission-covered", kind: KindPermissionPrompt, when: func(s Signals) bool { return s.CoveredByRule }, level: Log},
	{name: "permission-uncovered", kind: KindPermissionPrompt, when: always, level: Ask,
		why: "None of your standing rules covers this, so it needs you."},
	{name: "question-covered", kind: KindBotQuestion, when: func(s Signals) bool { return s.CoveredByRule }, level: Log},
	{name: "question-uncovered", kind: KindBotQuestion, when: always, level: Ask,
		why: "The bot stopped and is waiting for an answer only you can give."},
	{name: "rule-action", kind: KindRuleAction, when: always, level: Log},
	{name: "rule-suggestion", kind: KindRuleSuggestion, when: always, level: Brief},
	{name: "issue-due", kind: KindIssueDue, when: always, level: Brief},
	{name: "pr-merged", kind: KindPRMerged, when: always, level: Brief},
}

// Sorter sorts items no level rule covers. Jev (#15) or Haiku (#10) will
// implement it. It may answer only Log or Brief: Classify turns anything else
// into Brief, so a model can never raise an ask.
type Sorter interface {
	Sort(ctx context.Context, it Item) (Level, error)
}

// Classification is the level an item got and what decided it.
type Classification struct {
	Level Level
	// Rule names the level rule that matched, or "sorter" / "default".
	Rule string
	// Why is the plain reason for an ask; empty otherwise.
	Why string
	// Coerced is true when the sorter answered something other than log or
	// brief and was overruled.
	Coerced bool
}

// Classify gives an item its level: level rules first, then the sorter for
// leftovers. With no sorter, or when it fails, a leftover is Brief: kept
// rather than dropped, so a sorting mistake costs a line, not information.
func Classify(ctx context.Context, it Item, s Sorter) Classification {
	for _, r := range levelRules {
		if r.kind == it.Kind && (r.when == nil || r.when(it.Signals)) {
			return Classification{Level: r.level, Rule: r.name, Why: r.why}
		}
	}
	if s == nil {
		return Classification{Level: Brief, Rule: "default"}
	}
	l, err := s.Sort(ctx, it)
	if err != nil {
		return Classification{Level: Brief, Rule: "default"}
	}
	switch l {
	case Log, Brief:
		return Classification{Level: l, Rule: "sorter"}
	default: // Ask, or nonsense: a model never interrupts the user.
		return Classification{Level: Brief, Rule: "sorter", Coerced: true}
	}
}
