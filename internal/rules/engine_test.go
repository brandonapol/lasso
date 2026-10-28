package rules

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/audit"
)

// fakeExec records calls and checks the audit log already has the started
// line when it runs.
type fakeExec struct {
	t     *testing.T
	log   *audit.Log
	calls []audit.Step
	fail  error
}

func (x *fakeExec) Execute(_ context.Context, s audit.Step) (Done, error) {
	x.calls = append(x.calls, s)
	es, err := x.log.Entries()
	if err != nil || len(es) == 0 || es[len(es)-1].Result != audit.Started || es[len(es)-1].Action != s.Action {
		x.t.Errorf("executor ran before its audit line was written: %+v %v", es, err)
	}
	if x.fail != nil {
		return Done{}, x.fail
	}
	switch s.Action {
	case string(FileIssue):
		return Done{Target: "o/r#42", Detail: "filed o/r#42", Args: map[string]string{"number": "42"}}, nil
	}
	return Done{Detail: "did " + s.Action}, nil
}

func (x *fakeExec) Inverse(s audit.Step, d Done) (audit.Step, bool) {
	switch s.Action {
	case string(FileIssue):
		return audit.Step{Action: "close_issue", Target: d.Target}, true
	case string(Label):
		return audit.Step{Action: "unlabel", Target: s.Target, Args: s.Args}, true
	}
	return audit.Step{}, false
}

func newEngine(t *testing.T, f File, clock *time.Time) (*Engine, *fakeExec) {
	t.Helper()
	log := audit.Open(filepath.Join(t.TempDir(), "audit.jsonl"))
	x := &fakeExec{t: t, log: log}
	return &Engine{
		Rules: func() (File, error) { return f, nil },
		Log:   log, Exec: x, Now: func() time.Time { return *clock },
	}, x
}

func TestDoRecordsThenRuns(t *testing.T) {
	clock := now
	e, x := newEngine(t, File{Rules: []Rule{{ID: "issues", Who: "grok-*", Action: FileIssue}}}, &clock)
	out, err := e.Do(context.Background(), Request{Bot: "grok-scout", Action: FileIssue, Target: "o/r", Args: map[string]string{"title": "flaky", "token": "hunter2"}})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Decision.Allowed || out.Decision.Rule != "issues" || !out.Undoable || out.ID == "" {
		t.Fatalf("outcome %+v", out)
	}
	if len(x.calls) != 1 || x.calls[0].Args["token"] != "hunter2" {
		t.Fatalf("executor got %+v (it needs the unscrubbed args)", x.calls)
	}
	es, _ := e.Log.Entries()
	if len(es) != 2 || es[0].Result != audit.Started || es[1].Result != audit.OK || es[0].Actor != "rule:issues" || es[1].Target != "o/r#42" {
		t.Fatalf("audit = %+v", es)
	}
	if es[0].Args["token"] != audit.Scrubbed {
		t.Fatalf("token reached the audit log: %+v", es[0].Args)
	}
}

func TestRefusedActionIsNotRunOrRecorded(t *testing.T) {
	clock := now
	e, x := newEngine(t, File{}, &clock)
	out, err := e.Do(context.Background(), Request{Bot: "grok-scout", Action: Comment, Target: "o/r#1"})
	if !errors.Is(err, ErrRefused) || out.Decision.Allowed || !strings.Contains(out.Decision.Reason, "No rule") {
		t.Fatalf("got %+v, %v", out, err)
	}
	if len(x.calls) != 0 {
		t.Fatal("refused action ran")
	}
}

func TestUnwritableAuditRefuses(t *testing.T) {
	clock := now
	e, x := newEngine(t, File{Rules: []Rule{{ID: "c", Who: "*", Action: Comment}}}, &clock)
	dir := t.TempDir()
	blocked := filepath.Join(dir, "audit.jsonl")
	if err := os.Mkdir(blocked, 0o700); err != nil { // a directory can't be appended to
		t.Fatal(err)
	}
	e.Log = audit.Open(blocked)
	x.log = e.Log
	out, err := e.Do(context.Background(), Request{Bot: "b", Action: Comment, Target: "o/r#1"})
	if !errors.Is(err, ErrAudit) || !strings.Contains(out.Decision.Reason, "audit log") {
		t.Fatalf("got %+v, %v", out, err)
	}
	if len(x.calls) != 0 {
		t.Fatal("action ran without an audit line")
	}
}

func TestBrokenRulesFileRefuses(t *testing.T) {
	clock := now
	e, x := newEngine(t, File{}, &clock)
	e.Rules = func() (File, error) { return File{}, errors.New("line 3: bad") }
	_, err := e.Do(context.Background(), Request{Bot: "b", Action: Comment})
	if !errors.Is(err, ErrRefused) || len(x.calls) != 0 {
		t.Fatalf("err = %v, calls = %d", err, len(x.calls))
	}
}

// Per-day caps are counted from the audit log and reset the next day.
func TestPerDayCapFromAuditLog(t *testing.T) {
	clock := now
	e, x := newEngine(t, File{Rules: []Rule{{ID: "retry", Who: "*", Action: CIRetry, PerDay: 2}}}, &clock)
	req := Request{Bot: "b", Action: CIRetry, Target: "o/r/runs/1"}
	x.fail = errors.New("flaky network") // failed tries still count
	if _, err := e.Do(context.Background(), req); !errors.Is(err, ErrFailed) {
		t.Fatalf("first: %v", err)
	}
	x.fail = nil
	if _, err := e.Do(context.Background(), req); err != nil {
		t.Fatalf("second: %v", err)
	}
	out, err := e.Do(context.Background(), req)
	if !errors.Is(err, ErrRefused) || !strings.Contains(out.Decision.Reason, "2 of 2") {
		t.Fatalf("third: %+v, %v", out, err)
	}
	clock = now.Add(24 * time.Hour)
	if _, err := e.Do(context.Background(), req); err != nil {
		t.Fatalf("next day: %v", err)
	}
}

func TestUndo(t *testing.T) {
	clock := now
	e, x := newEngine(t, File{Rules: []Rule{
		{ID: "issues", Who: "*", Action: FileIssue},
		{ID: "retry", Who: "*", Action: CIRetry},
	}}, &clock)
	ctx := context.Background()
	filed, err := e.Do(ctx, Request{Bot: "b", Action: FileIssue, Target: "o/r"})
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	out, err := e.Undo(ctx, filed.ID)
	if err != nil {
		t.Fatalf("undo: %+v %v", out, err)
	}
	if last := x.calls[len(x.calls)-1]; last.Action != "close_issue" || last.Target != "o/r#42" {
		t.Fatalf("undo ran %+v", last)
	}
	if _, err := e.Undo(ctx, filed.ID); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("second undo: %v", err)
	}

	retried, err := e.Do(ctx, Request{Bot: "b", Action: CIRetry, Target: "o/r/runs/9"})
	if err != nil {
		t.Fatal(err)
	}
	out, err = e.Undo(ctx, retried.ID)
	if !errors.Is(err, ErrNoUndo) || !strings.Contains(out.Detail, "Can't undo") || !strings.Contains(out.Detail, "rule:retry ci_retry on o/r/runs/9") {
		t.Fatalf("no-inverse undo: %+v %v", out, err)
	}
	if _, err := e.Undo(ctx, "act-nope"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown id: %v", err)
	}
	es, _ := e.Log.Entries()
	var undoLines int
	for _, en := range es {
		if en.UndoOf == filed.ID {
			undoLines++
			if en.Actor != "user" {
				t.Errorf("undo actor = %q", en.Actor)
			}
		}
	}
	if undoLines != 2 {
		t.Fatalf("undo wrote %d lines, want started+ok", undoLines)
	}
}
