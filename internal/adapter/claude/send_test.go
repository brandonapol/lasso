package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
)

// fakeClaude puts a `claude` on PATH that records where and how it was
// run, appends the resumed turn to the transcript as the real one does,
// and prints a reply. FAKE_FAIL makes it fail instead.
func fakeClaude(t *testing.T, transcript, cwd string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "log")
	script := `#!/bin/sh
printf '%s\n' "$PWD" "$@" > "$FAKE_LOG"
if [ -n "$FAKE_FAIL" ]; then echo "No conversation found with session ID: $3" >&2; exit 1; fi
for a; do last=$a; done
printf '%s\n' '{"type":"user","uuid":"r1","sessionId":"idle","cwd":"'"$FAKE_CWD"'","timestamp":"2026-10-27T12:00:01Z","message":{"role":"user","content":"'"$last"'"}}' >> "$FAKE_TRANSCRIPT"
printf '%s\n' '{"type":"assistant","uuid":"r2","sessionId":"idle","cwd":"'"$FAKE_CWD"'","timestamp":"2026-10-27T12:00:02Z","message":{"id":"mr","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"Deployed to staging."}]}}' >> "$FAKE_TRANSCRIPT"
echo "Deployed to staging."
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	t.Setenv("FAKE_TRANSCRIPT", transcript)
	t.Setenv("FAKE_CWD", cwd)
	return log
}

func TestSendResumesIdleSession(t *testing.T) {
	e := setup(t)
	log := fakeClaude(t, e.path("idle"), e.cwd)
	a := e.adapter(t, nil) // the real runner, finding the fake on PATH
	ctx := context.Background()
	_, cur, err := a.Events(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Send(ctx, "idle", "ship it to staging"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	want := []string{e.cwd, "-p", "--resume", "idle", "ship it to staging"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("ran %q, want %q (in the session's cwd)", got, want)
	}

	// The reply comes back once, as an event; the transcript's copy of the
	// same turn doesn't come back a second time.
	evs, cur, err := a.Events(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Session != "idle" || string(evs[0].Text) != "Deployed to staging." || !strings.HasPrefix(evs[0].ID, "reply:") {
		t.Fatalf("after send = %+v", evs)
	}
	if evs, _, _ = a.Events(ctx, cur); len(evs) != 0 {
		t.Errorf("reply repeated: %+v", evs)
	}
}

func TestSendFailureIsReported(t *testing.T) {
	e := setup(t)
	fakeClaude(t, e.path("idle"), e.cwd)
	t.Setenv("FAKE_FAIL", "1")
	err := e.adapter(t, nil).Send(context.Background(), "idle", "hello")
	if err == nil || !strings.Contains(err.Error(), "No conversation found") {
		t.Errorf("send = %v, want claude's own complaint", err)
	}
}

// recorder is a Runner that records each resume, in order.
type recorder struct {
	mu   sync.Mutex
	runs [][]string
}

func (r *recorder) run(_ context.Context, dir string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, append([]string{dir}, args...))
	return []byte("ok"), nil
}

func (r *recorder) got() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.runs)
}

// waitFor polls until cond holds, poking the adapter so it notices closed
// sessions.
func waitFor(t *testing.T, a *Adapter, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, err := a.Sessions(context.Background()); err != nil {
			t.Fatal(err)
		}
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Without Claude Code's sessions files (an older Claude Code) lasso can't
// reach an open session, so the message waits for its process to exit.
func TestSendQueuesUntilTheSessionCloses(t *testing.T) {
	e := setup(t)
	rec := &recorder{}
	a := e.adapter(t, rec.run)
	ctx := context.Background()
	for _, text := range []string{"first", "second"} {
		if err := a.Send(ctx, "perm", text); err != nil {
			t.Fatalf("send to open perm = %v, want it queued", err)
		}
	}
	if err := a.Send(ctx, "asking", "hello"); err != nil {
		t.Fatal(err)
	}
	evs, _, err := a.Events(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	notes := 0
	for _, ev := range evs {
		if strings.HasPrefix(ev.ID, "lasso:") {
			notes++
			if !strings.Contains(string(ev.Text), "waiting until that session closes") {
				t.Errorf("note = %q", ev.Text)
			}
		}
	}
	if notes != 3 {
		t.Errorf("got %d notes, want one per queued message: %+v", notes, evs)
	}
	if n := len(rec.got()); n != 0 {
		t.Fatalf("resumed %d times while the sessions were open", n)
	}

	// perm's process exits: its messages go out by resume, in order.
	// "asking" is still open (written to a moment ago), so it waits.
	e.procs = nil
	waitFor(t, a, "perm's queue to go out", func() bool { return len(rec.got()) == 2 })
	runs := rec.got()
	for i, want := range []string{"first", "second"} {
		r := runs[i]
		if r[0] != e.cwd || r[1] != "-p" || r[2] != "--resume" || r[3] != "perm" || r[4] != want {
			t.Errorf("run %d = %q", i, r)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(rec.got()); n != 2 {
		t.Errorf("resumed %d times; the still-open session was resumed", n)
	}

	if err := a.Send(ctx, "worker", "hello"); err == nil || errors.Is(err, adapter.ErrReadOnly) {
		t.Errorf("send to saddle's worker = %v, want no such session", err)
	}
}

func TestSendFlagLikeText(t *testing.T) {
	e := setup(t)
	var args []string
	a := e.adapter(t, func(_ context.Context, _ string, as ...string) ([]byte, error) { args = as; return []byte("ok"), nil })
	if err := a.Send(context.Background(), "idle", "--dangerously-skip-permissions"); err != nil {
		t.Fatal(err)
	}
	if last := args[len(args)-1]; strings.HasPrefix(last, "-") {
		t.Errorf("text passed as a flag: %q", args)
	}
}
