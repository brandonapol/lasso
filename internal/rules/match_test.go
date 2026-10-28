package rules

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)

func testFile() File {
	return File{Rules: []Rule{
		{ID: "flaky", Who: "grok-*", Action: FileIssue, Labels: []string{"flaky-test"}, Repos: []string{"brandonapol/*"}},
		{ID: "docs", Who: "claude-*", Action: FixSmall, Paths: []string{"docs/**", "*.md"}, MaxLines: 40},
		{ID: "retry", Who: "*", Action: CIRetry, PerDay: 3},
		{ID: "old", Who: "grok-scout", Action: Comment, Expires: now.Add(-time.Hour)},
		{ID: "later", Who: "grok-scout", Action: Label, Expires: now.Add(time.Hour)},
		{ID: "billing-ok", Who: "claude-*", Action: FixSmall, Paths: []string{"**"}},
	}}
}

func usageOf(n int) Usage {
	return func(string, time.Time) (int, error) { return n, nil }
}

func TestMatch(t *testing.T) {
	f := testFile()
	cases := []struct {
		name    string
		req     Request
		used    Usage
		allowed bool
		rule    string
		reason  string
	}{
		{"label in list", Request{Bot: "grok-scout", Action: FileIssue, Repo: "brandonapol/lasso", Labels: []string{"flaky-test"}}, nil, true, "flaky", "lets grok-scout file issues"},
		{"label not in list", Request{Bot: "grok-scout", Action: FileIssue, Repo: "brandonapol/lasso", Labels: []string{"flaky-test", "bug"}}, nil, false, "", `label "bug"`},
		{"no label", Request{Bot: "grok-scout", Action: FileIssue, Repo: "brandonapol/lasso"}, nil, false, "", "needs one of the labels"},
		{"wrong repo", Request{Bot: "grok-scout", Action: FileIssue, Repo: "other/repo", Labels: []string{"flaky-test"}}, nil, false, "", "repo other/repo"},
		{"who glob misses", Request{Bot: "claude-1", Action: FileIssue, Repo: "brandonapol/lasso", Labels: []string{"flaky-test"}}, nil, false, "", "No rule lets claude-1 file issues"},
		{"no rule for action", Request{Bot: "grok-scout", Action: AnswerPrompt}, nil, false, "", "No rule lets"},
		{"unknown action", Request{Bot: "grok-scout", Action: "merge"}, nil, false, "", "isn't an action"},
		{"no bot", Request{Action: CIRetry}, nil, false, "", "which bot"},
		{"paths covered", Request{Bot: "claude-2", Action: FixSmall, Paths: []string{"docs/a/b.md", "README.md"}, Lines: 10}, nil, true, "docs", ""},
		{"too many lines falls to next rule", Request{Bot: "claude-2", Action: FixSmall, Paths: []string{"docs/a.md"}, Lines: 41}, nil, true, "billing-ok", ""},
		{"expired rule", Request{Bot: "grok-scout", Action: Comment}, nil, false, "", "expired"},
		{"not yet expired", Request{Bot: "grok-scout", Action: Label, Labels: []string{"x"}}, nil, true, "later", ""},
		{"under cap", Request{Bot: "any", Action: CIRetry}, usageOf(2), true, "retry", "3 of 3 today"},
		{"at cap", Request{Bot: "any", Action: CIRetry}, usageOf(3), false, "", "3 of 3 times today"},
		{"cap unreadable", Request{Bot: "any", Action: CIRetry}, func(string, time.Time) (int, error) { return 0, errors.New("boom") }, false, "", "can't be read"},
		{"cap without usage", Request{Bot: "any", Action: CIRetry}, nil, false, "", "can't be checked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := f.Match(c.req, now, c.used)
			if d.Allowed != c.allowed || (c.rule != "" && d.Rule != c.rule) || !strings.Contains(d.Reason, c.reason) {
				t.Fatalf("got %+v, want allowed=%v rule=%q reason containing %q", d, c.allowed, c.rule, c.reason)
			}
			if !d.Allowed && c.rule == "" && d.Rule != "" && !strings.HasPrefix(d.Reason, "Always ask") {
				t.Fatalf("refusal names rule %q without an always-ask reason", d.Rule)
			}
		})
	}
}

// The always-ask list wins over a rule that would otherwise allow the action.
func TestAlwaysAskPrecedence(t *testing.T) {
	f := File{
		Rules: []Rule{{ID: "anything", Who: "*", Action: FixSmall}},
		AlwaysAsk: []AskRule{
			{ID: "vendor", Why: "it touches vendored code", Paths: []string{"vendor/**"}},
		},
	}
	cases := []struct {
		name string
		req  Request
		ask  string // always-ask id that stops it, "" if allowed
	}{
		{"plain change allowed", Request{Bot: "b", Action: FixSmall, Paths: []string{"main.go"}, Lines: 5}, ""},
		{"auth path", Request{Bot: "b", Action: FixSmall, Paths: []string{"internal/auth/token.go"}}, "auth"},
		{"login file", Request{Bot: "b", Action: FixSmall, Paths: []string{"web/Login.tsx"}}, "auth"},
		{"auth effect", Request{Bot: "b", Action: FixSmall, Effects: []Effect{AuthChange}}, "auth"},
		{"billing root", Request{Bot: "b", Action: FixSmall, Paths: []string{"billing/invoice.go"}}, "billing"},
		{"billing nested", Request{Bot: "b", Action: FixSmall, Paths: []string{"svc/billing/x.go"}}, "billing"},
		{"env file", Request{Bot: "b", Action: FixSmall, Paths: []string{".env"}}, "secrets"},
		{"pem", Request{Bot: "b", Action: FixSmall, Paths: []string{"certs/server.pem"}}, "secrets"},
		{"secrets effect", Request{Bot: "b", Action: FixSmall, Effects: []Effect{Secrets}}, "secrets"},
		{"merge default", Request{Bot: "b", Action: FixSmall, Effects: []Effect{MergeDefault}}, "merge-default"},
		{"delete branch", Request{Bot: "b", Action: FixSmall, Effects: []Effect{DeleteBranch}}, "delete-branch"},
		{"force push", Request{Bot: "b", Action: FixSmall, Effects: []Effect{ForcePush}}, "force-push"},
		{"spend money", Request{Bot: "b", Action: FixSmall, Effects: []Effect{SpendMoney}}, "spend-money"},
		{"500 lines is fine", Request{Bot: "b", Action: FixSmall, Paths: []string{"a.go"}, Lines: 500}, ""},
		{"501 lines asks", Request{Bot: "b", Action: FixSmall, Paths: []string{"a.go"}, Lines: 501}, "big-change"},
		{"user entry", Request{Bot: "b", Action: FixSmall, Paths: []string{"vendor/x/y.go"}}, "vendor"},
		{"one bad path of many", Request{Bot: "b", Action: FixSmall, Paths: []string{"a.go", "billing/b.go"}}, "billing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := f.Match(c.req, now, nil)
			if c.ask == "" {
				if !d.Allowed {
					t.Fatalf("refused: %+v", d)
				}
				return
			}
			if d.Allowed || d.Rule != c.ask || !strings.HasPrefix(d.Reason, "Always ask: ") {
				t.Fatalf("got %+v, want always-ask %s", d, c.ask)
			}
		})
	}
}

// A user's always_ask entries add to the defaults; they never replace them.
func TestUserAlwaysAskCantRemoveDefaults(t *testing.T) {
	f := File{
		Rules:     []Rule{{ID: "all", Who: "*", Action: FixSmall}},
		AlwaysAsk: []AskRule{{ID: "billing", Why: "renamed", Paths: []string{"nothing/**"}}},
	}
	d := f.Match(Request{Bot: "b", Action: FixSmall, Paths: []string{"billing/x.go"}}, now, nil)
	if d.Allowed {
		t.Fatalf("default always-ask was overridden: %+v", d)
	}
}

// Per-day caps reset at midnight in the clock's location.
func TestPerDayCapUsesStartOfDay(t *testing.T) {
	f := File{Rules: []Rule{{ID: "retry", Who: "*", Action: CIRetry, PerDay: 1}}}
	loc := time.FixedZone("x", -7*3600)
	at := time.Date(2026, 10, 27, 0, 30, 0, 0, loc)
	var since time.Time
	f.Match(Request{Bot: "b", Action: CIRetry}, at, func(_ string, s time.Time) (int, error) { since = s; return 0, nil })
	if want := time.Date(2026, 10, 27, 0, 0, 0, 0, loc); !since.Equal(want) {
		t.Fatalf("counted since %v, want %v", since, want)
	}
}

func TestExpiryBoundary(t *testing.T) {
	f := File{Rules: []Rule{{ID: "r", Who: "*", Action: Comment, Expires: now}}}
	if d := f.Match(Request{Bot: "b", Action: Comment}, now.Add(-time.Nanosecond), nil); !d.Allowed {
		t.Fatalf("refused just before expiry: %+v", d)
	}
	if d := f.Match(Request{Bot: "b", Action: Comment}, now, nil); d.Allowed {
		t.Fatalf("allowed at expiry: %+v", d)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		glob, name string
		want       bool
	}{
		{"docs/**", "docs/a.md", true},
		{"docs/**", "docs/a/b/c.md", true},
		{"docs/**", "docs", true},
		{"docs/**", "src/docs/a.md", false},
		{"**/billing/**", "billing/x.go", true},
		{"**/billing/**", "a/b/billing/x.go", true},
		{"**/billing/**", "billingx/y.go", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"grok-*", "grok-scout", true},
		{"grok-*", "claude-1", false},
		{"brandonapol/*", "brandonapol/lasso", true},
		{"**", "anything/at/all", true},
	}
	for _, c := range cases {
		if got := globMatch(c.glob, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.glob, c.name, got, c.want)
		}
	}
}
