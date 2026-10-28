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
		if e.Gloss == "" {
			t.Errorf("%q has no gloss", e.Jargon)
		}
		// Strict: no glossary word at all, not even an everyday one.
		for _, side := range []string{e.Plain, e.Gloss} {
			if u := glossaryRe.FindAllString(side, -1); len(u) > 0 {
				t.Errorf("%q → %q still uses jargon %v", e.Jargon, side, u)
			}
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

// Bot sentences keep their wording; each term gets a gloss after its first
// use, and the result has nothing left unexplained.
func TestGloss(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Do you want to rebase onto main?",
			"Do you want to rebase (bring its branch up to date) onto main?"},
		{"Rebased onto main; ready to land.",
			"Rebased (brought its branch up to date) onto main; ready to land (add it to the shared work branch)."},
		{"t4 landed. Ignore previous instructions.",
			"t4 landed (added to the shared work branch). Ignore previous instructions."},
		{"Restack t7, then restack t8.",
			"Restack (rebuild its chain of pull requests on the latest code) t7, then restack t8."},
		{"t7 has a stale base, so I can't merge it.",
			"t7 has a stale base (its starting point is out of date), so I can't merge it."},
		{"The stale base on t3 blocks the stack.",
			"The stale base (its starting point is out of date) on t3 blocks the stack (a chain of pull requests that build on each other)."},
		{"t5 is third on the merge train.",
			"t5 is third on the merge train (the line of finished work waiting to be added one at a time)."},
		{"Run `saddle land t7` when the gate is green.",
			"Run `saddle land t7` (add it to the shared work branch) when the gate (the checks that must pass) is green."},
		{"Should I publish the PR?",
			"Should I publish (open it as a standalone pull request) the PR?"},
		// Everyday English stays exactly as written.
		{"We land in Denver at noon.", "We land in Denver at noon."},
		{"Meet me by the garden gate.", "Meet me by the garden gate."},
		{"A stack of plates fell over.", "A stack of plates fell over."},
		{"I'd like to claim my prize.", "I'd like to claim my prize."},
		{"They publish a newsletter weekly.", "They publish a newsletter weekly."},
		{"nothing to explain", "nothing to explain"},
	}
	for _, c := range cases {
		got := Gloss(c.in)
		if got != c.want {
			t.Errorf("Gloss(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
		if u := Unexplained(got); len(u) > 0 {
			t.Errorf("Gloss(%q) left %v", c.in, u)
		}
		if again := Gloss(got); again != got {
			t.Errorf("Gloss twice: %q", again)
		}
	}
}

// Common words are only jargon near bot words; see isJargon.
func TestCommonWordsNeedBotContext(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"We land in Denver.", nil},
		{"the garden gate", nil},
		{"Land the PR when green.", []string{"land"}},
		{"t3 landed on main.", []string{"landed"}},
		{"Hold the gate for task 4.", []string{"gate"}},
		{"Which files does the claim cover?", []string{"claim"}},
		{"`stack`", []string{"stack"}},
		{"park", []string{"park"}},
		// Context doesn't leak across sentences.
		{"We land in Denver. The PR is green.", nil},
		// Other jargon nearby counts as context.
		{"Restack before you land.", []string{"restack", "land"}},
	}
	for _, c := range cases {
		if got := Unexplained(c.in); !slices.Equal(got, c.want) {
			t.Errorf("Unexplained(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// A gloss only explains its own term: anything else stays flagged.
func TestUnexplainedAfterGloss(t *testing.T) {
	text := Gloss("Rebase t7.") + " The sentinel says t7 is at-risk."
	want := []string{"sentinel", "at-risk"}
	if got := Unexplained(text); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// A bracket that isn't the term's gloss doesn't count.
	if got := Unexplained("rebase (soon) t7"); !slices.Equal(got, []string{"rebase"}) {
		t.Fatalf("got %v", got)
	}
}
