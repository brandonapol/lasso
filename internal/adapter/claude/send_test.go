package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestSendRefusesLiveSessions(t *testing.T) {
	e := setup(t)
	ran := false
	a := e.adapter(t, func(context.Context, string, ...string) ([]byte, error) { ran = true; return nil, nil })
	for _, id := range []string{"asking", "perm"} {
		err := a.Send(context.Background(), id, "hello")
		if !errors.Is(err, adapter.ErrReadOnly) || !strings.Contains(err.Error(), "can't send while it's running") {
			t.Errorf("send to live %s = %v", id, err)
		}
	}
	if err := a.Send(context.Background(), "worker", "hello"); err == nil || errors.Is(err, adapter.ErrReadOnly) {
		t.Errorf("send to saddle's worker = %v, want no such session", err)
	}
	if ran {
		t.Error("claude ran for a session lasso can't write to")
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
