package rules

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sample = `# My standing permissions.
# Reviewed 2026-10-01.

[[always_ask]]
id = "vendor"
why = "it touches vendored code"
paths = ["vendor/**"]

# Grok may file flaky-test tickets.
[[rules]]
id = "flaky"
who = "grok-*"
action = "file_issue"
labels = ["flaky-test"] # only this label
repos = ["brandonapol/*"]
per_day = 5
expires = 2026-12-01

[[rules]]
id = "docs"
who = "claude-*"
action = "fix_small"
paths = ["docs/**"]
max_lines = 40
`

func writeSample(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rules.toml")
	if err := os.WriteFile(p, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	f, err := LoadFile(writeSample(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Rules) != 2 || len(f.AlwaysAsk) != 1 {
		t.Fatalf("got %d rules, %d always_ask", len(f.Rules), len(f.AlwaysAsk))
	}
	r := f.Rules[0]
	if r.ID != "flaky" || r.Who != "grok-*" || r.Action != FileIssue || r.PerDay != 5 || r.Expires.IsZero() || r.Expires.Day() != 1 {
		t.Fatalf("rule 0 = %+v", r)
	}
}

func TestMissingFileIsNoRules(t *testing.T) {
	f, err := LoadFile(filepath.Join(t.TempDir(), "rules.toml"))
	if err != nil || len(f.Rules) != 0 {
		t.Fatalf("got %+v, %v", f, err)
	}
}

func TestPathUsesConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	p, err := Path()
	if err != nil || p != "/cfg/lasso/rules.toml" {
		t.Fatalf("Path() = %q, %v", p, err)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct{ name, toml, want string }{
		{"unknown key", "[[rules]]\nid='a'\nwho='x'\naction='comment'\nmax_line=3\n", "unknown key rules.max_line"},
		{"bad action", "[[rules]]\nid='a'\nwho='x'\naction='merge'\n", `action "merge" is not one of`},
		{"no id", "[[rules]]\nwho='x'\naction='comment'\n", "id is required"},
		{"no who", "[[rules]]\nid='a'\naction='comment'\n", "who is required"},
		{"dup id", "[[rules]]\nid='a'\nwho='x'\naction='comment'\n[[rules]]\nid='a'\nwho='y'\naction='label'\n", "used twice"},
		{"bad glob", "[[rules]]\nid='a'\nwho='x'\naction='comment'\npaths=['[']\n", "malformed"},
		{"negative cap", "[[rules]]\nid='a'\nwho='x'\naction='comment'\nper_day=-1\n", "0 or more"},
		{"empty always_ask", "[[always_ask]]\nid='a'\n", "needs paths"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.toml))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// Load, save, load gives the same rules.
func TestRoundTrip(t *testing.T) {
	p := writeSample(t)
	f, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(p, f); err != nil {
		t.Fatal(err)
	}
	g, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(norm(f), norm(g)) {
		t.Fatalf("round trip changed rules:\n%+v\n%+v", f, g)
	}
}

// norm compares expiry instants: a date-only expiry comes back as local
// midnight with an explicit offset, the same instant.
func norm(f File) File {
	for i := range f.Rules {
		f.Rules[i].Expires = f.Rules[i].Expires.UTC()
	}
	return f
}

// Adding a rule keeps every comment and the existing order byte for byte.
func TestAppendKeepsComments(t *testing.T) {
	p := writeSample(t)
	r := Rule{ID: "retry", Who: "grok-scout", Action: CIRetry, PerDay: 3}
	if err := Append(p, r); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), sample) {
		t.Fatalf("existing text changed:\n%s", b)
	}
	f, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Rules) != 3 || f.Rules[2].ID != "retry" || f.Rules[2].PerDay != 3 {
		t.Fatalf("rules after append = %+v", f.Rules)
	}
}

func TestAppendCreatesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "rules.toml")
	if err := Append(p, Rule{ID: "a", Who: "b", Action: Comment}); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFile(p)
	if err != nil || len(f.Rules) != 1 {
		t.Fatalf("got %+v, %v", f, err)
	}
}

func TestAppendRefusesDuplicateAndBrokenFile(t *testing.T) {
	p := writeSample(t)
	if err := Append(p, Rule{ID: "flaky", Who: "x", Action: Comment}); err == nil {
		t.Fatal("appended a rule with a duplicate id")
	}
	if err := Append(p, Rule{ID: "bad", Who: "x", Action: "nope"}); err == nil {
		t.Fatal("appended an invalid rule")
	}
	b, _ := os.ReadFile(p)
	if string(b) != sample {
		t.Fatal("a refused append changed the file")
	}
	broken := filepath.Join(t.TempDir(), "rules.toml")
	_ = os.WriteFile(broken, []byte("[[rules]\n"), 0o600)
	if err := Append(broken, Rule{ID: "a", Who: "b", Action: Comment}); err == nil {
		t.Fatal("appended to a file that doesn't parse")
	}
}
