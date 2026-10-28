package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/rules"
)

// Send implements adapter.Adapter: it resumes an idle session headlessly
// with text as the next prompt. It returns once grok has started the turn
// (or failed to); the turn runs on, and its delta arrives through Events.
//
// A session lasso can't resume (deleted, or from another grok home) gets
// one retry as a fresh session in the same folder, so a message is never
// silently dropped; the turn's event says so.
func (a *Adapter) Send(ctx context.Context, sessionID, text string) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(text) == "" {
		return errors.New("grok: send needs a session and some text")
	}
	s, err := a.writable(ctx, sessionID)
	if err != nil {
		return err
	}
	return a.start(ctx, s, text, nil, true)
}

// Answer implements adapter.Adapter for a permission prompt a headless
// turn stopped at. Option 1 resumes the session with `--allow <rule>`,
// option 2 with `--deny <rule>`; either way the rule lasts that one turn.
// Free text, if any, goes to grok with the answer.
func (a *Adapter) Answer(ctx context.Context, promptID string, choice adapter.Choice) error {
	p, err := a.lookup(promptID)
	if err != nil {
		return err
	}
	var flag, msg string
	switch choice.Option {
	case 1:
		flag = "--allow"
		msg = fmt.Sprintf("The user allowed %s for this turn. Go ahead with it, then carry on.", p.Rule)
	case 2:
		flag = "--deny"
		msg = fmt.Sprintf("The user denied %s. Don't try it again; carry on without it, or stop and say what you need.", p.Rule)
	default:
		return errors.New("grok: answer with option 1 (allow) or 2 (deny)")
	}
	if t := strings.TrimSpace(choice.Text); t != "" {
		msg += "\n\nThe user adds: " + t
	}
	s, err := a.writable(ctx, p.Session)
	if err != nil {
		return err
	}
	if st := a.tail(s); !st.waiting || st.prompt != p.Prompt {
		return errors.New("grok: that permission prompt is no longer waiting; the session has moved on")
	}
	// No fresh-session retry here: a new session wouldn't know what the
	// answer is about.
	return a.start(ctx, s, msg, []string{flag, p.Rule}, false)
}

// Request is the rules-engine request for answering promptID: the facts
// come from the refused call's own arguments, as grok recorded them.
func (a *Adapter) Request(promptID string, choice adapter.Choice) (rules.Request, error) {
	p, err := a.lookup(promptID)
	if err != nil {
		return rules.Request{}, err
	}
	return rules.Request{
		Bot:    Kind,
		Action: rules.AnswerPrompt,
		Target: promptID,
		Paths:  p.Paths,
		Args:   map[string]string{"tool": p.Tool, "rule": p.Rule, "option": strconv.Itoa(choice.Option)},
	}, nil
}

// lookup finds the prompt behind a prompt id ("<session>/<prompt>"),
// rereading the session file if lasso has restarted since it saw it.
func (a *Adapter) lookup(promptID string) (*pending, error) {
	a.mu.Lock()
	p := a.prompts[promptID]
	a.mu.Unlock()
	if p != nil {
		return p, nil
	}
	sid, prompt, ok := strings.Cut(promptID, "/")
	if !ok || prompt == "" {
		return nil, fmt.Errorf("grok: %q isn't a grok prompt id", promptID)
	}
	s, ok := a.find(sid)
	if !ok {
		return nil, fmt.Errorf("grok: no session %s", sid)
	}
	f, err := os.Open(s.updates())
	if err != nil {
		return nil, fmt.Errorf("grok: %w", err)
	}
	defer f.Close()
	_, _, _, err = scanTurns(bufio.NewReaderSize(f, 64<<10), 0, nil, func(t *turn) {
		if t.prompt == prompt && t.waiting() {
			_, p = t.event(s.ID, s.Cwd)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("grok: %w", err)
	}
	if p == nil {
		return nil, fmt.Errorf("grok: no permission prompt %s waiting", promptID)
	}
	return p, nil
}

// writable finds a session lasso may resume now.
func (a *Adapter) writable(ctx context.Context, id string) (session, error) {
	s, ok := a.find(id)
	if !ok {
		for _, r := range a.list(ctx) {
			if r.ID == id {
				s, ok = session{ID: id, Cwd: a.opts.Dir}, true
			}
		}
	}
	if !ok {
		return session{}, fmt.Errorf("grok: no session %s", id)
	}
	if a.live()[id] {
		return session{}, fmt.Errorf("%w: grok session %s is open in a grok window; lasso can't send to it until it's closed", adapter.ErrReadOnly, id)
	}
	if a.isBusy(id) {
		return session{}, fmt.Errorf("%w: lasso is already running a turn in grok session %s", adapter.ErrReadOnly, id)
	}
	if s.Dir != "" {
		if st := a.tail(s); st.open && a.opts.Now().Sub(s.Updated) < staleTurn {
			return session{}, fmt.Errorf("%w: can't send to grok session %s while it's running", adapter.ErrReadOnly, id)
		}
	}
	return s, nil
}

// start runs one headless turn in the background and waits until grok
// has begun it or given up.
func (a *Adapter) start(ctx context.Context, s session, text string, extra []string, retry bool) error {
	a.mu.Lock()
	if a.busy[s.ID] {
		a.mu.Unlock()
		return fmt.Errorf("%w: lasso is already running a turn in grok session %s", adapter.ErrReadOnly, s.ID)
	}
	a.busy[s.ID] = true
	a.mu.Unlock()

	f, err := os.CreateTemp("", "lasso-grok-prompt-*.md")
	if err == nil {
		_, err = f.WriteString(text)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		a.release(s.ID)
		return fmt.Errorf("grok: prompt file: %w", err)
	}

	// The turn outlives the caller's request; it gets its own deadline.
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.opts.TurnTimeout)
	started := make(chan error, 1)
	a.turns.Add(1)
	go func() {
		defer a.turns.Done()
		defer cancel()
		defer a.release(s.ID)
		defer func() { _ = os.Remove(f.Name()) }()
		a.run(tctx, s, f.Name(), extra, retry, started)
	}()
	select {
	case err := <-started:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Adapter) release(id string) {
	a.mu.Lock()
	delete(a.busy, id)
	a.mu.Unlock()
}

// run is one turn: resume, and if grok can't, one fresh session.
//
// The resume-then-one-fresh-retry shape is borrowed from saddle's
// internal/agent/grok_bridge.go (RunGrokBridge), by the same author:
// "A session id from another harness, or a deleted session, should not
// wedge the chat. One retry starts fresh."
func (a *Adapter) run(ctx context.Context, s session, promptFile string, extra []string, retry bool, started chan<- error) {
	once := func(err error) {
		select {
		case started <- err:
		default:
		}
	}
	res, err := a.runOnce(ctx, s, promptFile, s.ID, extra, func() { once(nil) })
	note := ""
	if err != nil && !res.began && retry {
		note = fmt.Sprintf("Couldn't resume grok session %s (%s), so lasso started a fresh one in the same folder. ", s.ID, clip(err.Error(), 200))
		res, err = a.runOnce(ctx, s, promptFile, "", extra, func() { once(nil) })
	}
	if err != nil && !res.began {
		a.report("grok turn didn't start: " + clip(err.Error(), 300))
		once(fmt.Errorf("grok: %w", err))
		return
	}
	once(nil)
	sid := first(res.session, s.ID)
	if res.turn == nil || !res.turn.ended {
		why := "grok stopped before the turn ended"
		if err != nil {
			why += ": " + err.Error()
		}
		a.enqueue(adapter.Event{
			ID: sid + "/lasso-" + strconv.FormatInt(a.opts.Now().UnixNano(), 36), Bot: Kind, Session: sid,
			Kind: attention.KindBotMessage, At: a.opts.Now(),
			Text: adapter.Untrusted(attention.Gloss(note + clip(why, 400))),
		}, nil)
		return
	}
	if res.turn.prompt == "" {
		res.turn.prompt = "lasso-" + strconv.FormatInt(a.opts.Now().UnixNano(), 36)
	}
	e, p := res.turn.event(sid, s.Cwd)
	if note != "" {
		e.Text = adapter.Untrusted(attention.Gloss(note)) + e.Text
	}
	a.enqueue(e, p)
}

func (a *Adapter) enqueue(e adapter.Event, p *pending) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queue = append(a.queue, e)
	if p != nil {
		a.prompts[e.PromptID] = p
	}
}

type turnResult struct {
	began   bool // grok wrote at least one update
	session string
	turn    *turn
}

// runOnce runs grok once and parses its streaming-json into a turn.
func (a *Adapter) runOnce(ctx context.Context, s session, promptFile, resume string, extra []string, began func()) (turnResult, error) {
	args := []string{"--prompt-file", promptFile, "--output-format", "streaming-json"}
	if resume != "" {
		args = append([]string{"--resume", resume}, args...)
	}
	args = append(args, extra...)
	cmd := exec.CommandContext(ctx, a.opts.Grok, args...)
	cmd.Dir = s.Cwd
	if fi, err := os.Stat(s.Cwd); err != nil || !fi.IsDir() {
		cmd.Dir = a.opts.Dir
	}
	cmd.Env = append(os.Environ(), a.opts.Env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return turnResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return turnResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return turnResult{}, err
	}
	errCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(stderr)
		if len(b) > 4096 {
			b = b[len(b)-4096:]
		}
		errCh <- strings.TrimSpace(string(b))
	}()

	var res turnResult
	var failure string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var l streamLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type == "" {
			continue
		}
		if l.SessionID != "" {
			res.session = l.SessionID
		}
		if l.Type == "error" {
			failure = first(l.Message, "grok reported an error")
			continue
		}
		u, ok := l.update(a.opts.Now())
		if !ok {
			continue
		}
		if !res.began {
			res.began = true
			res.turn = newTurn("", 0)
			began()
		}
		if u.kind == turnEnd {
			res.turn.prompt = l.RequestID
		}
		res.turn.apply(u)
	}
	scanErr := sc.Err()
	tail := <-errCh // drain stderr before Wait closes the pipe
	waitErr := cmd.Wait()
	switch {
	case failure != "":
		return res, errors.New(failure)
	case scanErr != nil:
		return res, scanErr
	case waitErr != nil && (res.turn == nil || !res.turn.ended):
		return res, errors.New(first(lastLine(tail), waitErr.Error()))
	}
	return res, nil
}

// wait blocks until every turn lasso started has finished. Tests use it.
func (a *Adapter) wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { a.turns.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
