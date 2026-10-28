package attention

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGlossaryPlainSideIsPlain(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Glossary {
		k := strings.ToLower(e.Jargon)
		if seen[k] {
			t.Errorf("%q listed twice", e.Jargon)
		}
		seen[k] = true
		if e.Plain == "" {
			t.Errorf("%q has no plain words", e.Jargon)
		}
		if u := Unexplained(e.Plain); len(u) > 0 {
			t.Errorf("%q → %q still uses jargon %v", e.Jargon, e.Plain, u)
		}
	}
}

// The saddle terms the user named must stay covered.
func TestGlossarySeeded(t *testing.T) {
	for _, term := range []string{"stale base", "restack", "land", "claims", "merge train", "sentinel",
		"at-risk", "requeue", "publish", "unstack", "worktree", "gate"} {
		if !slices.Contains(Unexplained(term), term) {
			t.Errorf("glossary lacks %q", term)
		}
	}
}

func TestPlain(t *testing.T) {
	cases := []struct{ in, want string }{
		{"t7 has a stale base.", "t7 started from a branch that has since moved on."},
		{"Stale base: yes", "The branch it started from has moved on: yes"},
		{"Restack before you land.", "Rebuild its chain of pull requests on the latest code before you add to the shared work branch."},
		{"t7 landed on the merge train", "t7 added to the shared work branch on the line of finished work waiting to be added one at a time"},
		{"the PR stack is at-risk", "the chain of pull requests is possibly broken"},
		{"files in its claims", "files in its reserved files"},
		{"investigate the landscape", "investigate the landscape"},
		{"nothing to translate", "nothing to translate"},
	}
	for _, c := range cases {
		if got := Plain(c.in); got != c.want {
			t.Errorf("Plain(%q) = %q, want %q", c.in, got, c.want)
		}
		if u := Unexplained(Plain(c.in)); len(u) > 0 {
			t.Errorf("Plain(%q) left %v", c.in, u)
		}
	}
}

func TestUnexplainedFlagsJargon(t *testing.T) {
	got := Unexplained("The sentinel says t3 is at-risk; requeue it after the gate. Sentinel again.")
	want := []string{"sentinel", "at-risk", "requeue", "gate"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Every string lasso itself writes for the user must be plain: level-rule
// reasons, line-jumper reasons and the decision-card checks.
func TestOwnTextIsPlain(t *testing.T) {
	for _, r := range levelRules {
		if u := Unexplained(r.why); len(u) > 0 {
			t.Errorf("rule %s reason %q uses jargon %v", r.name, r.why, u)
		}
	}
	jumpers := []Item{
		{Kind: KindRunFailed, Repo: "a/b", Signals: Signals{DefaultBranch: true, HotRepo: true, RedSince: noon.Add(-time.Hour)}},
		{Kind: KindIssueDue, Title: "Ship", Due: noon.Add(time.Hour)},
		{Kind: KindIssueDue, Title: "Ship", Due: noon.Add(-time.Hour)},
		{Kind: KindBotQuestion, Bot: "grok-scout", Signals: Signals{BlockedSince: noon.Add(-3 * time.Hour)}},
	}
	for _, it := range jumpers {
		reason, ok := LineJumper(it, noon)
		if !ok {
			t.Fatalf("%v didn't jump", it)
		}
		if u := Unexplained(reason); len(u) > 0 {
			t.Errorf("line-jumper reason %q uses jargon %v", reason, u)
		}
	}
}

func TestDecisionCard(t *testing.T) {
	card := DecisionCard{
		Wants:          "grok-scout wants to restack t7 onto main.",
		WhyAsking:      "None of your standing rules covers this, so it needs you.",
		Plain:          "t7 has a stale base, so the work is out of date.",
		IfYes:          "It rebuilds t7 and opens the pull request again.",
		IfNo:           "t7 waits, and nothing else changes.",
		Recommendation: "Yes: it only rewrites its own branch.",
	}
	err := card.Validate()
	if err == nil || !strings.Contains(err.Error(), "restack") || !strings.Contains(err.Error(), "stale base") {
		t.Fatalf("Validate = %v, want jargon flagged", err)
	}
	if err := PlainCard(card).Validate(); err != nil {
		t.Fatalf("plain card: %v", err)
	}
	if err := (DecisionCard{Wants: "x"}).Validate(); err == nil || !strings.Contains(err.Error(), "if_no is empty") {
		t.Fatalf("empty fields not caught: %v", err)
	}
}
