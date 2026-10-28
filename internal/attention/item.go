// Package attention decides how much of the user's attention each event gets
// (ADR 0001 §5).
//
// Every event becomes an Item at exactly one Level: log (history only), brief
// (shown when the user looks) or ask (needs a decision no rule covers). Level
// rules are plain Go tables; a model may sort leftovers into log or brief but
// can never produce an ask. Asks are rationed by a daily budget and held
// during quiet hours. Only a short, fixed list of line-jumpers skips the
// line, and only they may ever raise a desktop notification.
//
// Nothing here reads the wall clock: every time comes from an injected Clock.
package attention

import (
	"fmt"
	"time"
)

// Clock returns the current time. Tests pass a fixed one.
type Clock func() time.Time

// Level is how much attention an item gets.
type Level string

// The three levels, quietest first.
const (
	Log   Level = "log"   // happened, doesn't matter: history only
	Brief Level = "brief" // worth knowing: shown when the user looks
	Ask   Level = "ask"   // needs a human decision: counted against the budget
)

// Valid reports whether l is one of the three levels.
func (l Level) Valid() bool { return l == Log || l == Brief || l == Ask }

// Kind is what sort of event an item came from.
type Kind string

// Known kinds. Adapters may add others; an unknown kind simply matches no
// level rule and goes to the Sorter.
const (
	KindRunFailed        Kind = "run_failed"        // an Actions run failed
	KindRunPassed        Kind = "run_passed"        // an Actions run passed
	KindPermissionPrompt Kind = "permission_prompt" // a bot asks permission to do something
	KindBotQuestion      Kind = "bot_question"      // a bot stopped and asks the user something
	KindBotMessage       Kind = "bot_message"       // ordinary bot chatter
	KindIssueUpdated     Kind = "issue_updated"     // an issue or PR changed
	KindIssueDue         Kind = "issue_due"         // an issue has a due date coming up
	KindPRMerged         Kind = "pr_merged"         // a pull request was merged
	KindRuleAction       Kind = "rule_action"       // a standing rule acted for the user
	KindRuleSuggestion   Kind = "rule_suggestion"   // lasso offers to turn repeated answers into a rule
)

// Signals are the facts level rules and line-jumpers look at. Adapters fill
// in what they know; zero values mean "not known / not the case".
type Signals struct {
	// DefaultBranch is true when the event is on the repo's default branch.
	DefaultBranch bool `json:"default_branch,omitempty"`
	// HotRepo is true when the repo is in config's hot_repos.
	HotRepo bool `json:"hot_repo,omitempty"`
	// RedSince is when the default branch started failing (zero if green).
	RedSince time.Time `json:"red_since,omitzero"`
	// LastProgress is the last sign of work on a due item: a commit, a
	// linked PR, a comment. Zero means none at all.
	LastProgress time.Time `json:"last_progress,omitzero"`
	// BlockedSince is when a bot started waiting on the user (zero if not).
	BlockedSince time.Time `json:"blocked_since,omitzero"`
	// CoveredByRule is true when a standing rule already answers this.
	CoveredByRule bool `json:"covered_by_rule,omitempty"`
}

// Item is one event at one level.
type Item struct {
	ID      string    `json:"id"`
	Source  string    `json:"source"`         // "github", "saddle", "claude", "grok"
	Repo    string    `json:"repo,omitempty"` // owner/name
	Bot     string    `json:"bot,omitempty"`  // session or bot name, if a bot raised it
	Kind    Kind      `json:"kind"`
	Title   string    `json:"title"`
	Summary string    `json:"summary,omitempty"` // plain words, no jargon (see Plain)
	Level   Level     `json:"level"`
	Created time.Time `json:"created"`
	Due     time.Time `json:"due,omitzero"`
	Signals Signals   `json:"signals"`
	// Card is set on every ask.
	Card *DecisionCard `json:"card,omitempty"`
}

func (it Item) String() string { return fmt.Sprintf("%s %s %s", it.Level, it.Kind, it.ID) }
