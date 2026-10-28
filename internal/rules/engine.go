package rules

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/audit"
)

// Errors Do and Undo return, so callers (the API) can tell them apart.
var (
	// ErrRefused: no rule allows the action, or the always-ask list stops
	// it. The Outcome's Reason says which.
	ErrRefused = errors.New("refused")
	// ErrAudit: the audit line couldn't be written, so nothing was sent.
	ErrAudit = errors.New("audit log not writable")
	// ErrFailed: the adapter call failed. lasso shows it and stops.
	ErrFailed = errors.New("action failed")
	// ErrNoUndo: the action has no inverse. The Outcome says what happened.
	ErrNoUndo = errors.New("can't undo")
	// ErrUnknown: no action with that id is in the audit log.
	ErrUnknown = errors.New("no such action")
)

// Done is what an executor reports after sending an action.
type Done struct {
	// Target is what was actually touched, if it differs from the request's
	// (a filed issue's new number).
	Target string
	// Detail says what happened, in plain words.
	Detail string
	// Args are facts about the result the inverse may need.
	Args map[string]string
}

// Executor sends actions: one adapter call each (ADR 0001 §2).
type Executor interface {
	// Execute sends one step and reports what happened.
	Execute(ctx context.Context, step audit.Step) (Done, error)
	// Inverse returns the step that reverses a done step (close the filed
	// issue, remove the label), or false when there is none.
	Inverse(step audit.Step, done Done) (audit.Step, bool)
}

// Outcome is the result of Do or Undo.
type Outcome struct {
	// ID is the action's id in the audit log; empty if it never got there.
	ID       string   `json:"id,omitempty"`
	Decision Decision `json:"decision"`
	// Detail says what happened, in plain words.
	Detail string `json:"detail,omitempty"`
	// Undoable is true when the action has a recorded inverse.
	Undoable bool `json:"undoable,omitempty"`
}

// Engine runs every outward action through rules check, then audit write,
// then the executor, in that order.
type Engine struct {
	Rules func() (File, error)
	Log   *audit.Log
	Exec  Executor
	Now   Clock

	// mu makes check-then-record atomic, so two requests can't both take
	// the last slot of a per-day cap.
	mu sync.Mutex
}

// NewEngine returns an Engine that reads the rules file at path on every
// action, so edits take effect without a restart.
func NewEngine(rulesPath string, log *audit.Log, exec Executor, now Clock) *Engine {
	return &Engine{
		Rules: func() (File, error) { return LoadFile(rulesPath) },
		Log:   log, Exec: exec, Now: now,
	}
}

// Check matches req against the current rules without acting or recording.
func (e *Engine) Check(req Request) (Decision, error) {
	f, err := e.Rules()
	if err != nil {
		return Decision{}, err
	}
	return f.Match(req, e.Now(), e.usage), nil
}

func (e *Engine) usage(ruleID string, since time.Time) (int, error) {
	return e.Log.CountStarted(actorFor(ruleID), since)
}

func actorFor(ruleID string) string { return "rule:" + ruleID }

// Do checks req against the rules and, if a rule allows it, records it in
// the audit log and sends it. A rules file that doesn't load refuses
// everything: a broken file never means "allow".
func (e *Engine) Do(ctx context.Context, req Request) (Outcome, error) {
	e.mu.Lock()
	f, err := e.Rules()
	if err != nil {
		e.mu.Unlock()
		return Outcome{Decision: Decision{Reason: "The rules file has a problem, so nothing runs until it's fixed: " + err.Error()}}, ErrRefused
	}
	now := e.Now()
	dec := f.Match(req, now, e.usage)
	if !dec.Allowed {
		e.mu.Unlock()
		return Outcome{Decision: dec}, ErrRefused
	}
	id := newID()
	step := audit.Step{Action: string(req.Action), Target: req.Target, Args: req.Args}
	err = e.Log.Append(audit.Entry{
		Time: now, ID: id, Actor: actorFor(dec.Rule), Bot: req.Bot,
		Action: step.Action, Target: step.Target, Args: step.Args, Result: audit.Started,
		Detail: dec.Reason,
	})
	e.mu.Unlock()
	if err != nil {
		return Outcome{Decision: Decision{Rule: dec.Rule, Reason: "Not done: lasso couldn't write the audit log, and it never acts without a record. " + err.Error()}}, fmt.Errorf("%w: %v", ErrAudit, err)
	}
	return e.run(ctx, id, actorFor(dec.Rule), req.Bot, "", step, dec)
}

// UserActor is the audit actor for actions the user approved themselves.
const UserActor = "user"

// DoAsUser runs req as the user's own decision: the user is the approval,
// so no rule is matched and the always-ask list doesn't stop it (it exists
// to ask the user, who is the one asking). It is still written to the audit
// log, with actor "user", before it is sent.
//
// Only call it for a request the authenticated user made (the TUI or web
// view's token), never for one from a bot or the agent pane: those go
// through Do.
func (e *Engine) DoAsUser(ctx context.Context, req Request) (Outcome, error) {
	if !req.Action.Valid() {
		return Outcome{Decision: Decision{Reason: fmt.Sprintf("%q isn't an action lasso knows", req.Action)}}, ErrRefused
	}
	dec := Decision{Allowed: true, Reason: "You approved it."}
	id := newID()
	step := audit.Step{Action: string(req.Action), Target: req.Target, Args: req.Args}
	if err := e.Log.Append(audit.Entry{
		Time: e.Now(), ID: id, Actor: UserActor, Bot: req.Bot,
		Action: step.Action, Target: step.Target, Args: step.Args, Result: audit.Started,
		Detail: dec.Reason,
	}); err != nil {
		return Outcome{Decision: Decision{Reason: "Not done: lasso couldn't write the audit log, and it never acts without a record. " + err.Error()}}, fmt.Errorf("%w: %v", ErrAudit, err)
	}
	return e.run(ctx, id, UserActor, req.Bot, "", step, dec)
}

// run sends a step already recorded as started, then records its result.
func (e *Engine) run(ctx context.Context, id, actor, bot, undoOf string, step audit.Step, dec Decision) (Outcome, error) {
	done, err := e.Exec.Execute(ctx, step)
	res := audit.Entry{
		Time: e.Now(), ID: id, Actor: actor, Bot: bot, UndoOf: undoOf,
		Action: step.Action, Target: step.Target, Args: step.Args,
	}
	if done.Target != "" {
		res.Target = done.Target
	}
	if err != nil {
		res.Result, res.Detail = audit.Failed, err.Error()
		out := Outcome{ID: id, Decision: dec, Detail: "It failed: " + err.Error()}
		if aerr := e.Log.Append(res); aerr != nil {
			out.Detail += " (and the failure couldn't be recorded: " + aerr.Error() + ")"
		}
		return out, fmt.Errorf("%w: %v", ErrFailed, err)
	}
	res.Result, res.Detail = audit.OK, done.Detail
	if inv, ok := e.Exec.Inverse(step, done); ok && undoOf == "" {
		res.Undo = &inv
	}
	out := Outcome{ID: id, Decision: dec, Detail: done.Detail, Undoable: res.Undo != nil}
	if aerr := e.Log.Append(res); aerr != nil {
		// It already happened; say so rather than pretend it didn't.
		out.Detail += " (done, but the result couldn't be recorded: " + aerr.Error() + ")"
		out.Undoable = false
	}
	return out, nil
}

// Undo reverses the action id when it has a recorded inverse. The inverse
// is the user's own request, so it isn't matched against rules, but it is
// written to the audit log first like everything else. With no inverse it
// returns ErrNoUndo and says what happened.
func (e *Engine) Undo(ctx context.Context, id string) (Outcome, error) {
	last, ok, err := e.Log.Find(id)
	if err != nil {
		return Outcome{}, fmt.Errorf("%w: %v", ErrAudit, err)
	}
	if !ok {
		return Outcome{}, ErrUnknown
	}
	happened := fmt.Sprintf("%s %s on %s at %s", last.Actor, last.Action, last.Target, last.Time.Format("2 Jan 15:04"))
	if last.Detail != "" && last.Result != audit.Started {
		happened += ": " + last.Detail
	}
	if by, done, err := e.Log.UndoneBy(id); err != nil {
		return Outcome{}, fmt.Errorf("%w: %v", ErrAudit, err)
	} else if done {
		return Outcome{ID: id, Detail: "Already undone (" + by + "). What happened: " + happened}, ErrNoUndo
	}
	switch {
	case last.Result == audit.Started:
		return Outcome{ID: id, Detail: "Can't undo: it was started but no result was recorded, so lasso doesn't know what it did. What happened: " + happened}, ErrNoUndo
	case last.Result == audit.Failed:
		return Outcome{ID: id, Detail: "Nothing to undo: it failed. What happened: " + happened}, ErrNoUndo
	case last.Undo == nil:
		return Outcome{ID: id, Detail: "Can't undo this kind of action. What happened: " + happened}, ErrNoUndo
	}
	uid := newID()
	if err := e.Log.Append(audit.Entry{
		Time: e.Now(), ID: uid, Actor: UserActor, Bot: last.Bot, UndoOf: id,
		Action: last.Undo.Action, Target: last.Undo.Target, Args: last.Undo.Args, Result: audit.Started,
		Detail: "undo of " + id,
	}); err != nil {
		return Outcome{Detail: "Not undone: lasso couldn't write the audit log. " + err.Error()}, fmt.Errorf("%w: %v", ErrAudit, err)
	}
	dec := Decision{Allowed: true, Reason: "You asked to undo " + id + "."}
	return e.run(ctx, uid, UserActor, last.Bot, id, *last.Undo, dec)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "act-" + hex.EncodeToString(b)
}
