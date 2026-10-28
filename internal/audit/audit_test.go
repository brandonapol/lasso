package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendAndRead(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "state", "audit.jsonl"))
	t0 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	for i, e := range []Entry{
		{Time: t0, ID: "a1", Actor: "rule:r1", Action: "label", Target: "o/r#1", Result: Started},
		{Time: t0, ID: "a1", Actor: "rule:r1", Action: "label", Target: "o/r#1", Result: OK, Undo: &Step{Action: "unlabel", Target: "o/r#1"}},
		{Time: t0.Add(time.Hour), ID: "a2", Actor: "rule:r1", Action: "label", Target: "o/r#2", Result: Started},
		{Time: t0.Add(-24 * time.Hour), ID: "a0", Actor: "rule:r1", Action: "label", Target: "o/r#0", Result: Started},
	} {
		if err := l.Append(e); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	fi, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
	n, err := l.CountStarted("rule:r1", t0)
	if err != nil || n != 2 {
		t.Fatalf("CountStarted = %d, %v; want 2", n, err)
	}
	e, ok, err := l.Find("a1")
	if err != nil || !ok || e.Result != OK || e.Undo == nil || e.Undo.Action != "unlabel" {
		t.Fatalf("Find a1 = %+v %v %v", e, ok, err)
	}
	if _, ok, _ := l.Find("zz"); ok {
		t.Fatal("found an id that was never written")
	}
}

func TestMissingLogIsEmpty(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "audit.jsonl"))
	es, err := l.Entries()
	if err != nil || len(es) != 0 {
		t.Fatalf("got %v, %v", es, err)
	}
}

func TestUnwritableLogErrors(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: no write can succeed, even as root.
	path := filepath.Join(dir, "audit.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Open(path).Append(Entry{ID: "x", Result: Started}); err == nil {
		t.Fatal("append to an unwritable log succeeded")
	}
}

func TestCorruptLineIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, []byte("{\"id\":\"a\"}\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path).Entries()
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v, want one naming line 2", err)
	}
}

func TestScrub(t *testing.T) {
	cases := []struct {
		key, in, want string
	}{
		{"body", "plain words", "plain words"},
		{"token", "anything", Scrubbed},
		{"GITHUB_TOKEN", "x", Scrubbed},
		{"api_key", "x", Scrubbed},
		{"body", "use ghp_abcdefghijklmnopqrstuvwxyz0123 here", "use [scrubbed] here"},
		{"body", "key sk-ant-REDACTEDREDACTEDREDACTED ok", "key [scrubbed] ok"},
		{"header", "Bearer abcdefghijklmnop1234", Scrubbed},
		{"body", "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----", Scrubbed},
	}
	for _, c := range cases {
		got := Scrub(map[string]string{c.key: c.in})[c.key]
		if got != c.want {
			t.Errorf("Scrub(%s=%q) = %q, want %q", c.key, c.in, got, c.want)
		}
	}
	long := Scrub(map[string]string{"body": strings.Repeat("a", 2000)})["body"]
	if len(long) > maxArg+len("…") {
		t.Errorf("long value not clipped: %d bytes", len(long))
	}
	if Scrub(nil) != nil {
		t.Error("Scrub(nil) should be nil")
	}
}

func TestAppendScrubsArgs(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err := l.Append(Entry{ID: "a", Result: Started, Args: map[string]string{"token": "hunter2"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(l.Path())
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("secret reached the log: %s", b)
	}
}
