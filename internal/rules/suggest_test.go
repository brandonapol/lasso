package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/attention"
)

func yes(bot string, a Action, repo string, ago time.Duration) Answer {
	return Answer{Bot: bot, Action: a, Choice: "yes", Repo: repo, At: now.Add(-ago)}
}

func TestSuggest(t *testing.T) {
	day := 24 * time.Hour
	answers := []Answer{
		yes("grok-scout", FileIssue, "o/r", 1*day),
		yes("grok-scout", FileIssue, "o/r", 2*day),
		yes("grok-scout", FileIssue, "o/r", 6*day),
		// Only two in the window for retries.
		yes("grok-scout", CIRetry, "o/r", 1*day),
		yes("grok-scout", CIRetry, "o/r", 2*day),
		yes("grok-scout", CIRetry, "o/r", 8*day),
		// Three "no"s never make a rule.
		{Bot: "claude-1", Action: Comment, Choice: "no", At: now.Add(-time.Hour)},
		{Bot: "claude-1", Action: Comment, Choice: "no", At: now.Add(-2 * time.Hour)},
		{Bot: "claude-1", Action: Comment, Choice: "no", At: now.Add(-3 * time.Hour)},
		// Mixed repos: drafted without a repo limit.
		yes("claude-1", Label, "o/a", time.Hour),
		yes("claude-1", Label, "o/b", time.Hour),
		yes("claude-1", Label, "o/b", time.Hour),
	}
	got := Suggest(answers, File{}, now)
	if len(got) != 2 {
		t.Fatalf("got %d suggestions: %+v", len(got), got)
	}
	label, issue := got[0], got[1]
	if issue.Rule.ID != "grok-scout-file-issue" || issue.Rule.Who != "grok-scout" || issue.Rule.Action != FileIssue ||
		issue.Rule.PerDay != DraftPerDay || !issue.Rule.Expires.Equal(now.Add(DraftLifetime)) ||
		len(issue.Rule.Repos) != 1 || issue.Rule.Repos[0] != "o/r" || issue.Count != 3 {
		t.Fatalf("issue draft = %+v", issue)
	}
	if label.Rule.Action != Label || len(label.Rule.Repos) != 0 {
		t.Fatalf("label draft = %+v", label.Rule)
	}
	if issue.Item.Level != attention.Brief || issue.Item.Kind != attention.KindRuleSuggestion {
		t.Fatalf("item = %+v", issue.Item)
	}
	if _, err := Parse([]byte(issue.TOML)); err != nil || !strings.Contains(issue.TOML, `who = "grok-scout"`) {
		t.Fatalf("draft TOML doesn't parse (%v):\n%s", err, issue.TOML)
	}
}

func TestSuggestSkipsCoveredAndAvoidsIDClash(t *testing.T) {
	answers := []Answer{
		yes("grok-scout", FileIssue, "", time.Hour),
		yes("grok-scout", FileIssue, "", time.Hour),
		yes("grok-scout", FileIssue, "", time.Hour),
	}
	covered := File{Rules: []Rule{{ID: "x", Who: "grok-*", Action: FileIssue}}}
	if got := Suggest(answers, covered, now); len(got) != 0 {
		t.Fatalf("suggested a rule that already exists: %+v", got)
	}
	expired := File{Rules: []Rule{{ID: "grok-scout-file-issue", Who: "grok-*", Action: FileIssue, Expires: now.Add(-time.Hour)}}}
	got := Suggest(answers, expired, now)
	if len(got) != 1 || got[0].Rule.ID != "grok-scout-file-issue-2" {
		t.Fatalf("got %+v", got)
	}
}

// A suggestion writes nothing; Confirm does.
func TestConfirmWritesOnlyThen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rules.toml")
	answers := []Answer{yes("b", Comment, "", time.Hour), yes("b", Comment, "", time.Hour), yes("b", Comment, "", time.Hour)}
	s := Suggest(answers, File{}, now)
	if len(s) != 1 {
		t.Fatalf("got %+v", s)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("Suggest wrote the rules file")
	}
	if err := Confirm(p, s[0].Rule); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFile(p)
	if err != nil || len(f.Rules) != 1 || f.Rules[0].ID != s[0].Rule.ID {
		t.Fatalf("after confirm: %+v, %v", f, err)
	}
	if d := f.Match(Request{Bot: "b", Action: Comment}, now, usageOf(0)); !d.Allowed {
		t.Fatalf("confirmed rule doesn't allow: %+v", d)
	}
}
