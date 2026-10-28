// Package digest turns a bot's stream of events into a few points the user
// might read ("412 messages → 3 points", ADR 0001 §7).
//
// Each bot session's events are batched until the batch is DefaultWindow old
// or a salient event arrives (a prompt, an error, a turn end). A batch is then
// cut by Jev ("is this worth the user's attention?") when a Jev client exists,
// and what's left goes to Haiku as compact structured deltas. Haiku answers 0
// to 3 points, each at log or brief; anything else is coerced to brief, so a
// model never raises an ask. Jev only cuts; it never writes text the user
// reads. Without Jev, Haiku does the cutting too. Over Haiku's daily cap, or
// without a key, the digest is local text ("212 new messages from
// grok-scout") and says the cap is reached.
//
// All bot text is untrusted: it is clipped to MaxRunes and fenced with a
// random nonce, and the system prompt says fenced text is data.
package digest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/jev"
	"github.com/brandonapol/lasso/internal/llm"
)

// Event kinds the digest treats as salient beyond attention's prompts.
// Adapters emit them when they can tell.
const (
	KindError   attention.Kind = "bot_error" // the bot hit an error
	KindTurnEnd attention.Kind = "turn_end"  // the bot finished a turn
)

const (
	// DefaultWindow is how long a batch collects events before it's digested.
	DefaultWindow = 5 * time.Minute
	// MaxPoints is the most points one batch yields.
	MaxPoints = 3
	// MaxRunes is how much of any one bot text a model sees.
	MaxRunes = 500
	// DefaultMaxPromptEvents bounds how many deltas one Haiku call sees.
	DefaultMaxPromptEvents = 80
)

// Salient reports whether an event of kind k flushes its batch at once.
func Salient(k attention.Kind) bool {
	switch k {
	case attention.KindPermissionPrompt, attention.KindBotQuestion, KindError, KindTurnEnd:
		return true
	}
	return false
}

// Point is one thing worth saying about a batch.
type Point struct {
	Text  string          `json:"text"`
	Level attention.Level `json:"level"` // log or brief, never ask
}

// Digest is what one batch came to.
type Digest struct {
	Bot     string `json:"bot"`
	Session string `json:"session"`
	// Events is how many events the batch had (the N of "N → k").
	Events int `json:"events"`
	// Kept is how many survived Jev's cut; Events when Jev didn't cut.
	Kept int `json:"kept"`
	// CutBy is "jev" when Jev cut the batch, else "haiku".
	CutBy  string  `json:"cut_by"`
	Points []Point `json:"points"`
	// Local is true when Points is lasso's own fallback text, not Haiku's.
	Local bool `json:"local,omitempty"`
	// OverCap is true when Haiku's daily cap stopped the call.
	OverCap bool `json:"over_cap,omitempty"`
	// Coerced is true when Haiku answered a level other than log or brief.
	Coerced bool      `json:"coerced,omitempty"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	// Err is why Haiku wasn't used, when it wasn't.
	Err error `json:"-"`
}

// Count is a session's running "N → k": events in, points out.
type Count struct {
	Events int `json:"events"`
	Points int `json:"points"`
}

func (c Count) String() string { return fmt.Sprintf("%d → %d", c.Events, c.Points) }

// Options configure a Digester.
type Options struct {
	// LLM writes the points; nil means local text only.
	LLM *llm.Client
	// Jev cuts batches before Haiku sees them; nil means Haiku cuts.
	Jev *jev.Client
	// Window defaults to DefaultWindow.
	Window time.Duration
	// MaxPromptEvents defaults to DefaultMaxPromptEvents.
	MaxPromptEvents int
	// Now defaults to time.Now.
	Now func() time.Time
}

type batch struct {
	bot, session string
	started      time.Time
	events       []adapter.Event
}

// Digester batches events per session and digests each batch.
type Digester struct {
	llm       *llm.Client
	jev       *jev.Client
	window    time.Duration
	maxEvents int
	now       func() time.Time
	newNonce  func() (string, error)

	mu      sync.Mutex
	batches map[string]*batch
	counts  map[string]Count
}

// New returns a Digester.
func New(o Options) *Digester {
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.MaxPromptEvents <= 0 {
		o.MaxPromptEvents = DefaultMaxPromptEvents
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Digester{
		llm: o.LLM, jev: o.Jev, window: o.Window, maxEvents: o.MaxPromptEvents, now: o.Now,
		newNonce: randomNonce,
		batches:  map[string]*batch{}, counts: map[string]Count{},
	}
}

func sessionKey(ev adapter.Event) string { return cmp.Or(ev.Session, ev.Bot) }

// Add puts ev in its session's batch. When ev is salient, or the batch has
// been open for the window, the batch is digested and returned.
func (d *Digester) Add(ctx context.Context, ev adapter.Event) *Digest {
	now := d.now()
	key := sessionKey(ev)
	d.mu.Lock()
	b := d.batches[key]
	if b == nil {
		b = &batch{bot: ev.Bot, session: key, started: now}
		d.batches[key] = b
	}
	b.events = append(b.events, ev)
	flush := Salient(ev.Kind) || now.Sub(b.started) >= d.window
	if flush {
		delete(d.batches, key)
	}
	d.mu.Unlock()
	if !flush {
		return nil
	}
	dg := d.digest(ctx, b)
	return &dg
}

// Tick digests every batch that has been open for the window. Call it
// periodically; quiet sessions are flushed only here.
func (d *Digester) Tick(ctx context.Context) []Digest {
	now := d.now()
	return d.flush(ctx, func(b *batch) bool { return now.Sub(b.started) >= d.window })
}

// FlushAll digests every open batch, for shutdown or when the user asks.
func (d *Digester) FlushAll(ctx context.Context) []Digest {
	return d.flush(ctx, func(*batch) bool { return true })
}

func (d *Digester) flush(ctx context.Context, due func(*batch) bool) []Digest {
	d.mu.Lock()
	var ready []*batch
	for k, b := range d.batches {
		if due(b) {
			ready = append(ready, b)
			delete(d.batches, k)
		}
	}
	d.mu.Unlock()
	slices.SortFunc(ready, func(a, b *batch) int { return cmp.Compare(a.session, b.session) })
	out := make([]Digest, 0, len(ready))
	for _, b := range ready {
		out = append(out, d.digest(ctx, b))
	}
	return out
}

// Counts returns each session's running "N → k".
func (d *Digester) Counts() map[string]Count {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]Count, len(d.counts))
	for k, v := range d.counts {
		out[k] = v
	}
	return out
}

// OverCap reports whether Haiku's daily cap is reached, so the UI can say so.
func (d *Digester) OverCap() bool {
	if d.llm == nil {
		return false
	}
	over, err := d.llm.OverCap()
	return err == nil && over
}

// worth is Jev's question for each event.
var worth = jev.Question{
	Type: "noul",
	Instructions: "This is one event from an AI coding bot's activity log. " +
		"Is it worth the attention of the person supervising the bot: progress, a result, " +
		"a problem, or something they would want to know? Routine chatter, tool noise and " +
		"repeated status lines are not.",
}

func (d *Digester) digest(ctx context.Context, b *batch) Digest {
	dg := Digest{Bot: b.bot, Session: b.session, Events: len(b.events), CutBy: "haiku"}
	if len(b.events) > 0 {
		dg.From, dg.To = b.events[0].At, b.events[len(b.events)-1].At
	}
	events, salient := b.events, false
	for _, ev := range events {
		salient = salient || Salient(ev.Kind)
	}
	if d.jev != nil {
		if kept, err := d.cut(ctx, events); err == nil {
			events, dg.CutBy = kept, "jev"
		}
		// On any Jev error (over its cap included) Haiku does the cutting.
	}
	dg.Kept = len(events)
	switch {
	case len(events) == 0:
		// Jev found nothing worth attention: no points, no Haiku call.
	case d.llm == nil:
		dg.Local, dg.Err = true, errors.New("digest: no Anthropic key")
		dg.Points = localPoints(dg, salient)
	default:
		pts, coerced, err := d.write(ctx, b, events, dg.CutBy == "jev")
		if err != nil {
			dg.Local, dg.Err, dg.OverCap = true, err, errors.Is(err, llm.ErrOverCap)
			dg.Points = localPoints(dg, salient)
		} else {
			dg.Points, dg.Coerced = pts, coerced
		}
	}
	d.mu.Lock()
	c := d.counts[b.session]
	c.Events += dg.Events
	c.Points += len(dg.Points)
	d.counts[b.session] = c
	d.mu.Unlock()
	return dg
}

// cut asks Jev about every non-salient event. Salient events (prompts,
// errors, turn ends) are always kept.
func (d *Digester) cut(ctx context.Context, events []adapter.Event) ([]adapter.Event, error) {
	var items []jev.Item
	for i, ev := range events {
		if !Salient(ev.Kind) {
			items = append(items, jev.Item{ID: fmt.Sprint(i), Text: string(ev.Kind) + ": " + string(ev.Text)})
		}
	}
	keep := map[string]bool{}
	if len(items) > 0 {
		kept, err := d.jev.Cut(ctx, items, worth)
		if err != nil {
			return nil, err
		}
		for _, k := range kept {
			keep[k.ID] = true
		}
	}
	var out []adapter.Event
	for i, ev := range events {
		if Salient(ev.Kind) || keep[fmt.Sprint(i)] {
			out = append(out, ev)
		}
	}
	return out, nil
}

// localPoints is the fallback when Haiku can't be used: a count, in lasso's
// own words. It is brief when the batch held a salient event, else log.
func localPoints(dg Digest, salient bool) []Point {
	noun := "messages"
	if dg.Events == 1 {
		noun = "message"
	}
	text := fmt.Sprintf("%d new %s from %s", dg.Events, noun, dg.Session)
	if dg.CutBy == "jev" && dg.Kept < dg.Events {
		text += fmt.Sprintf(" (%d worth a look)", dg.Kept)
	}
	if dg.OverCap {
		text += "; the digest's daily spending cap is reached"
	}
	level := attention.Log
	if salient {
		level = attention.Brief
	}
	return []Point{{Text: text, Level: level}}
}
