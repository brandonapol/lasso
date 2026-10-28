package claude

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
)

// queued is a message waiting for its session's process to exit.
type queued struct {
	session, text string
	at            time.Time
}

// check is a relayed message lasso hasn't yet seen land in its session.
type check struct {
	session, msgID, body string
	until                time.Time
}

// Send implements adapter.Adapter.
//   - A closed session is resumed headless in its own directory with text
//     as the next message; the reply comes back as an event.
//   - An open one gets text through lasso's relay. Claude Code shows it in
//     the session as a message from another session named "lasso", marked
//     as relayed from the user. lasso checks the relay sent exactly that,
//     to that session, and that the session's transcript shows it arrived.
//   - When the relay can't reach an open session, text waits until the
//     session's process exits, and then goes out by resume. Send returns
//     nil and says so in an event.
func (a *Adapter) Send(ctx context.Context, sessionID, text string) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(text) == "" {
		return errors.New("claude: send needs a session and some text")
	}
	a.mu.Lock()
	vs, err := a.views()
	if err != nil {
		a.mu.Unlock()
		return fmt.Errorf("claude: %w", err)
	}
	i := slices.IndexFunc(vs, func(v view) bool { return v.f.id == sessionID })
	if i < 0 {
		a.mu.Unlock()
		return fmt.Errorf("claude: no session %q", sessionID)
	}
	v := vs[i]
	if !v.writable {
		a.mu.Unlock()
		return fmt.Errorf("claude: %w: its directory %s is gone", adapter.ErrReadOnly, v.f.cwd)
	}
	switch {
	case a.draining[sessionID] || a.hasQueued(sessionID):
		a.enqueue(sessionID, text, "earlier messages to it are still waiting, and yours goes after them")
		a.mu.Unlock()
		return nil
	case a.busy[sessionID]:
		a.enqueue(sessionID, text, "lasso is still sending it your last message")
		a.mu.Unlock()
		return nil
	case !v.live:
		a.mu.Unlock()
		return a.resume(ctx, sessionID, text)
	case v.ses == nil || v.ses.Socket == "" || v.ses.Name == "":
		a.enqueue(sessionID, text, "this Claude Code doesn't let other sessions message it")
		a.mu.Unlock()
		return nil
	case !v.unique:
		a.enqueue(sessionID, text, fmt.Sprintf("another open session has the same name (%s), so lasso can't address it alone", v.ses.Name))
		a.mu.Unlock()
		return nil
	}
	name := v.ses.Name
	a.mu.Unlock()

	body := relayed(text)
	res, err := a.relay.forward(ctx, name, body)
	switch {
	case errors.Is(err, errRelayDown), errors.Is(err, errUnreachable):
		a.mu.Lock()
		a.enqueue(sessionID, text, "lasso's relay couldn't reach it ("+clip(err.Error(), clipDetail)+")")
		a.mu.Unlock()
		return nil
	case errors.Is(err, errRelayChanged):
		return fmt.Errorf("claude: %w", err)
	case errors.Is(err, errRefused):
		return fmt.Errorf("claude: session %s refused your message: %s", name, clip(res.notice, clipDetail))
	case err != nil:
		return fmt.Errorf("claude: sending to %s through lasso's relay: %w", name, err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastSent = sessionID
	if res.outcome == relayHeld {
		a.note(sessionID, "lasso passed your message on, and Claude Code is holding it until you approve it in that session: it runs in a different permission mode from lasso's relay.")
		return nil
	}
	if res.msgID == "" {
		return nil // nothing to check it by
	}
	// Watch the session's transcript for it to land, briefly here, then
	// on later reads (a busy session takes it in at its next step).
	a.checks = append(a.checks, check{session: sessionID, msgID: res.msgID, body: body, until: a.opts.Now().Add(a.opts.VerifyFor)})
	for end := time.Now().Add(a.opts.VerifyWait); ; {
		_ = a.refresh()
		landed, err := a.landed(sessionID, res.msgID, body)
		if landed || err != nil {
			a.checks = slices.DeleteFunc(a.checks, func(c check) bool { return c.msgID == res.msgID })
			return err
		}
		if !time.Now().Before(end) || ctx.Err() != nil {
			return nil // queued in the session; verify keeps watching
		}
		a.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
		a.mu.Lock()
	}
}

// landed reports whether the relayed message msgID shows in the session's
// transcript, and fails if what landed isn't what lasso asked for. Caller
// holds a.mu.
func (a *Adapter) landed(session, msgID, body string) (bool, error) {
	f := a.files[session]
	if f == nil {
		return false, nil
	}
	p, ok := f.peers[msgID]
	if !ok {
		return false, nil
	}
	if p.body != body || p.from != relayName {
		return true, fmt.Errorf("claude: the message that reached session %s isn't the one you sent (it came from %q, and its text differs: %t); check that session", session, p.from, p.body != body)
	}
	return true, nil
}

// verify checks relayed messages lasso hasn't yet seen land. Caller holds
// a.mu, and the transcripts are fresh.
func (a *Adapter) verify() {
	now := a.opts.Now()
	a.checks = slices.DeleteFunc(a.checks, func(c check) bool {
		landed, err := a.landed(c.session, c.msgID, c.body)
		if err != nil {
			a.note(c.session, err.Error())
		}
		return landed || now.After(c.until)
	})
}

// resume sends text to a closed session by resuming it headless. Caller
// doesn't hold a.mu.
func (a *Adapter) resume(ctx context.Context, sessionID, text string) error {
	a.mu.Lock()
	f := a.files[sessionID]
	if f == nil {
		a.mu.Unlock()
		return fmt.Errorf("claude: no session %q", sessionID)
	}
	dir := f.cwd
	a.busy[sessionID] = true
	f.sent[text]++ // its transcript turn is the reply event below
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, a.opts.SendTimeout)
	defer cancel()
	arg := text
	if strings.HasPrefix(arg, "-") {
		arg = " " + arg // never read as a flag
	}
	out, runErr := a.opts.Run(ctx, dir, "-p", "--resume", sessionID, arg)

	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.busy, sessionID)
	_ = a.refresh()
	if nf := a.files[sessionID]; nf != nil {
		f = nf
	}
	if runErr != nil {
		if f.sent[text] > 0 {
			f.sent[text]--
		}
		return fmt.Errorf("claude: resuming %s: %w", sessionID, runErr)
	}
	reply := strings.TrimSpace(string(out))
	if reply == "" {
		reply = "(no reply)"
	}
	a.replies = append(a.replies, f.event("reply:"+randomID(), clip(reply, clipText), a.opts.Now()))
	return nil
}

func (a *Adapter) hasQueued(session string) bool {
	return slices.ContainsFunc(a.queued, func(q queued) bool { return q.session == session })
}

// enqueue holds text until its session's process exits, and says so.
// Caller holds a.mu.
func (a *Adapter) enqueue(session, text, why string) {
	a.queued = append(a.queued, queued{session: session, text: text, at: a.opts.Now()})
	a.note(session, "Your message is waiting until that session closes: "+why+". lasso will send it as soon as the session's process exits. It won't resume the session while a terminal still has it open, because that would fork the conversation.")
}

// flush sends what was queued for sessions that have closed, in order,
// one session's messages after another. Caller holds a.mu.
func (a *Adapter) flush(vs []view) {
	if len(a.queued) == 0 {
		return
	}
	closed := map[string]bool{}
	for _, v := range vs {
		if !v.live && v.writable {
			closed[v.f.id] = true
		}
	}
	for id := range closed {
		if !a.hasQueued(id) || a.draining[id] {
			continue
		}
		a.draining[id] = true
		go a.drain(id)
	}
}

// drain sends a closed session's queued messages, oldest first, until none
// are left.
func (a *Adapter) drain(id string) {
	for {
		a.mu.Lock()
		i := slices.IndexFunc(a.queued, func(q queued) bool { return q.session == id })
		if i < 0 {
			delete(a.draining, id)
			a.mu.Unlock()
			return
		}
		q := a.queued[i]
		a.queued = slices.Delete(a.queued, i, i+1)
		a.mu.Unlock()
		if err := a.resume(context.Background(), id, q.text); err != nil {
			a.mu.Lock()
			a.note(id, "lasso couldn't send the message it was holding for this session: "+err.Error())
			a.mu.Unlock()
		}
	}
}

// notice is a delivery notice the relay got outside a send: a message
// held for approval or refused after the send returned.
func (a *Adapter) notice(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	session := a.lastSent
	for _, c := range a.checks {
		if strings.Contains(text, c.msgID) {
			session = c.session
		}
	}
	held, refused := classifyNotice(text)
	switch {
	case refused:
		a.note(session, "The session refused the message lasso passed on. Claude Code said: "+clip(text, clipDetail))
	case held:
		a.note(session, "Claude Code is holding the message lasso passed on until you approve it in that session.")
	default:
		a.note(session, "Claude Code said about the message lasso passed on: "+clip(text, clipDetail))
	}
}

// note queues lasso's own words about a session as an event. Caller holds
// a.mu.
func (a *Adapter) note(session, text string) {
	a.replies = append(a.replies, adapter.Event{
		ID: "lasso:" + randomID(), Bot: Kind, Session: session, Kind: attention.KindBotMessage,
		Text: adapter.Untrusted(text), At: a.opts.Now(),
	})
}

// Answer implements adapter.Adapter. Claude Code's prompts are answered in
// Claude Code itself: a message lasso passes into a session arrives from
// another session, and Claude Code never lets one approve a prompt.
func (a *Adapter) Answer(context.Context, string, adapter.Choice) error {
	return fmt.Errorf("claude: %w: answer this in the Claude Code session itself. lasso can pass messages into an open session, but Claude Code doesn't let a passed-on message approve a prompt", adapter.ErrReadOnly)
}
