package grok

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
)

// cursor is where Events read up to: the poll time, and for each session
// written to lately, the byte offset in its updates.jsonl. An offset sits
// at the start of an unfinished turn, so the turn is delivered whole once
// it ends.
//
// Encoded as "v1 <unix>;<id>=<offset>,…". Sessions quiet for longer than
// Options.Recent drop out; when one wakes, only turns that ended after the
// poll time are new.
type cursor struct {
	at  time.Time
	off map[string]int64
}

func parseCursor(c adapter.Cursor) (cursor, bool) {
	rest, ok := strings.CutPrefix(string(c), "v1 ")
	if !ok {
		return cursor{}, false
	}
	ts, offs, _ := strings.Cut(rest, ";")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return cursor{}, false
	}
	cur := cursor{at: time.Unix(sec, 0), off: map[string]int64{}}
	for _, kv := range strings.Split(offs, ",") {
		id, n, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if o, err := strconv.ParseInt(n, 10, 64); err == nil {
			cur.off[id] = o
		}
	}
	return cur, true
}

func (c cursor) String() string {
	ids := make([]string, 0, len(c.off))
	for id := range c.off {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id + "=" + strconv.FormatInt(c.off[id], 10)
	}
	return fmt.Sprintf("v1 %d;%s", c.at.Unix(), strings.Join(parts, ","))
}

// partialTurn is an unfinished turn already read, so the next poll goes on
// from where it stopped instead of rereading the turn from its start.
type partialTurn struct {
	upto int64
	t    *turn
}

// Events implements adapter.Adapter: one compact event per finished turn,
// across every grok session written to since the cursor.
//
// The zero cursor means "what grok still has waiting": the permission
// prompts sessions are stopped at. Older chatter isn't replayed.
func (a *Adapter) Events(ctx context.Context, since adapter.Cursor) ([]adapter.Event, adapter.Cursor, error) {
	now := a.opts.Now()
	prev, resumed := parseCursor(since)
	all, err := a.scan(a.opts.Recent)
	if err != nil {
		return nil, since, fmt.Errorf("grok: %w", err)
	}
	out := a.drain()
	next := cursor{at: now, off: map[string]int64{}}
	for _, s := range all {
		if err := ctx.Err(); err != nil {
			return nil, since, err
		}
		var evs []adapter.Event
		var off int64
		var err error
		switch o, known := prev.off[s.ID]; {
		case !resumed:
			evs, off, err = a.waitingNow(s)
		case known:
			evs, off, err = a.read(s, o, time.Time{})
		default:
			// A session the cursor didn't track: new, or quiet until now.
			evs, off, err = a.read(s, 0, prev.at.Add(-time.Second))
		}
		if err != nil {
			continue // a session removed mid-read; it drops out of the cursor
		}
		next.off[s.ID] = off
		out = append(out, a.fresh(evs)...)
	}
	slices.SortStableFunc(out, func(x, y adapter.Event) int { return x.At.Compare(y.At) })
	return out, adapter.Cursor(next.String()), nil
}

// waitingNow is the zero cursor's read of one session: its last turn if
// that stopped at a permission prompt, and the offset to go on from.
func (a *Adapter) waitingNow(s session) ([]adapter.Event, int64, error) {
	st := a.tail(s)
	fi, err := os.Stat(s.updates())
	if err != nil {
		return nil, 0, err
	}
	if !st.waiting && !st.open {
		return nil, completeEnd(s.updates(), fi.Size()), nil
	}
	// Read the last stretch only: the turn in question is near the end.
	from := max(fi.Size()-(1<<20), 0)
	evs, off, err := a.read(s, from, time.Time{})
	if err != nil || !st.waiting || len(evs) == 0 {
		return nil, off, err
	}
	last := evs[len(evs)-1]
	if last.Kind != attention.KindPermissionPrompt || last.ID != s.ID+"/"+st.prompt {
		return nil, off, nil
	}
	return []adapter.Event{last}, off, nil
}

// completeEnd is size, less any partial last line.
func completeEnd(path string, size int64) int64 {
	f, err := os.Open(path)
	if err != nil || size == 0 {
		return size
	}
	defer f.Close()
	var b [1]byte
	for p := size - 1; p >= 0 && size-p < 1<<20; p-- {
		if _, err := f.ReadAt(b[:], p); err != nil {
			return size
		}
		if b[0] == '\n' {
			return p + 1
		}
	}
	return 0
}

// read parses s's updates from off and returns the turns that finished at
// or after notBefore, and the offset to read from next. off may land mid
// line (a tail read); that fragment is skipped.
func (a *Adapter) read(s session, off int64, notBefore time.Time) ([]adapter.Event, int64, error) {
	f, err := os.Open(s.updates())
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if fi.Size() < off {
		// Rewritten (a rewind): nothing before here can be trusted to line
		// up, and nothing after it is new.
		a.forget(s.ID)
		return nil, completeEnd(s.updates(), fi.Size()), nil
	}

	pos := off
	var cur *turn
	a.mu.Lock()
	if p, ok := a.partial[s.ID]; ok && p.t.start == off && p.upto <= fi.Size() {
		cur, pos = p.t, p.upto
	}
	delete(a.partial, s.ID)
	a.mu.Unlock()

	if _, err := f.Seek(pos, io.SeekStart); err != nil {
		return nil, 0, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	if pos > 0 && cur == nil && !lineStart(f, pos) {
		skipped, err := r.ReadBytes('\n')
		pos += int64(len(skipped))
		if err != nil {
			return nil, off, nil
		}
	}
	var out []adapter.Event
	pos, committed, cur, err := scanTurns(r, pos, cur, func(t *turn) {
		if notBefore.IsZero() || !t.at.Before(notBefore) {
			out = append(out, a.turnEvent(s, t))
		}
	})
	if err != nil {
		return nil, 0, err
	}
	if cur != nil {
		a.mu.Lock()
		a.partial[s.ID] = partialTurn{upto: pos, t: cur}
		a.mu.Unlock()
		return out, cur.start, nil
	}
	return out, committed, nil
}

// scanTurns reads whole lines from r, which is at pos, feeding them to
// cur (nil: no turn open) and calling done for each turn that ends. It
// returns where it stopped, the offset after the last line outside any
// turn, and the turn still open.
func scanTurns(r *bufio.Reader, pos int64, cur *turn, done func(*turn)) (int64, int64, *turn, error) {
	committed := pos
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return pos, committed, cur, nil // a partial line: wait for the rest
		}
		if err != nil {
			return pos, committed, cur, err
		}
		at := pos
		pos += int64(len(line))
		u, ok := parseDisk(line)
		if !ok {
			if cur == nil {
				committed = pos
			}
			continue
		}
		switch {
		case cur == nil && (u.prompt != "" || u.kind == userText):
			cur = newTurn(u.prompt, at)
		case cur != nil && u.prompt != "" && cur.prompt == "":
			cur.prompt = u.prompt
		case cur != nil && u.prompt != "" && u.prompt != cur.prompt:
			// A new prompt with no end to the last one: grok was killed
			// mid-turn. Nothing to deliver for the dead one.
			cur = newTurn(u.prompt, at)
		}
		if cur == nil {
			committed = pos
			continue
		}
		cur.apply(u)
		if cur.ended {
			done(cur)
			cur = nil
			committed = pos
		}
	}
}

// lineStart reports whether pos begins a line.
func lineStart(f *os.File, pos int64) bool {
	var b [1]byte
	_, err := f.ReadAt(b[:], pos-1)
	return err == nil && b[0] == '\n'
}

func (a *Adapter) turnEvent(s session, t *turn) adapter.Event {
	e, p := t.event(s.ID, s.Cwd)
	if p != nil {
		a.mu.Lock()
		a.prompts[e.PromptID] = p
		a.mu.Unlock()
	}
	return e
}

func (a *Adapter) forget(id string) {
	a.mu.Lock()
	delete(a.partial, id)
	a.mu.Unlock()
}

// seenFor is how long a handed-out event id is remembered, so a turn lasso
// ran itself (parsed from grok's output) isn't delivered again when it
// turns up in the session file.
const seenFor = 24 * time.Hour

// fresh drops events already handed out and remembers the rest.
func (a *Adapter) fresh(evs []adapter.Event) []adapter.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.opts.Now()
	for id, at := range a.seen {
		if now.Sub(at) > seenFor {
			delete(a.seen, id)
		}
	}
	var out []adapter.Event
	for _, e := range evs {
		if _, ok := a.seen[e.ID]; ok {
			continue
		}
		a.seen[e.ID] = now
		out = append(out, e)
	}
	return out
}

// drain hands out the events lasso's own turns produced.
func (a *Adapter) drain() []adapter.Event {
	a.mu.Lock()
	q := a.queue
	a.queue = nil
	a.mu.Unlock()
	return a.fresh(q)
}
