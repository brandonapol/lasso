package attention

import (
	"sort"
	"time"

	"github.com/brandonapol/lasso/internal/config"
)

// QuietHours is a daily window, in local time, when lasso holds everything
// but line-jumpers and sends no notifications. The window may wrap past
// midnight ("22:00-08:00"). The zero value is never quiet.
type QuietHours struct {
	start, end time.Duration
	set        bool
}

// ParseQuietHours reads config's quiet_hours; "" means none.
func ParseQuietHours(a config.Attention) (QuietHours, error) {
	if a.QuietHours == "" {
		return QuietHours{}, nil
	}
	start, end, err := a.Quiet()
	if err != nil {
		return QuietHours{}, err
	}
	return QuietHours{start: start, end: end, set: start != end}, nil
}

// Contains reports whether t falls in the quiet window. The window starts at
// its start minute and ends just before its end minute.
func (q QuietHours) Contains(t time.Time) bool {
	if !q.set {
		return false
	}
	off := t.Sub(midnight(t))
	if q.start < q.end {
		return off >= q.start && off < q.end
	}
	return off >= q.start || off < q.end // wraps past midnight
}

func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// day is the local calendar day of t, used to reset the budget.
func day(t time.Time) string { return t.Format("2006-01-02") }

// Gate rations interruptions: at most AsksPerDay asks are presented per
// local day, nothing but line-jumpers is presented during quiet hours, and
// line-jumpers never count against the budget.
//
// A Gate lives in the daemon's memory. Losing it (a restart) at worst
// presents a day's asks again, which is within what ADR 0001 §2 allows to be
// lost.
type Gate struct {
	asksPerDay int
	quiet      QuietHours
	clock      Clock
	// presented maps an ask's ID to the local day it was first presented.
	presented map[string]string
}

// NewGate builds a Gate from config. The local day is the clock's location.
func NewGate(a config.Attention, clock Clock) (*Gate, error) {
	q, err := ParseQuietHours(a)
	if err != nil {
		return nil, err
	}
	return &Gate{asksPerDay: a.AsksPerDay, quiet: q, clock: clock, presented: map[string]string{}}, nil
}

// Shown is one item put in front of the user, with why it jumped the line
// (empty for an ordinary ask within budget).
type Shown struct {
	Item       Item
	LineJumper string
}

// View is what the gate lets through right now.
type View struct {
	// Now is shown in-app: line-jumpers first, then asks within budget.
	Now []Shown
	// Waiting are asks held by the budget or quiet hours. They are presented
	// on a later day, and are always visible in the In progress tab.
	Waiting []Item
}

// Present decides which of the open items interrupt the user now. Pass every
// open ask and any item that may be a line-jumper; plain log and brief items
// are ignored. It is safe to call on every refresh: an ask presented earlier
// stays presented and isn't counted twice.
func (g *Gate) Present(items []Item) View {
	now := g.clock()
	today := day(now)
	quiet := g.quiet.Contains(now)

	var v View
	var asks []Item
	for _, it := range items {
		if reason, ok := LineJumper(it, now); ok {
			v.Now = append(v.Now, Shown{Item: it, LineJumper: reason})
			continue
		}
		if it.Level == Ask {
			asks = append(asks, it)
		}
	}
	sortOldestFirst(asks)

	// Count today's presentations, including asks already answered: the
	// user was still interrupted. Forget earlier days' asks that are closed.
	open := make(map[string]bool, len(asks))
	for _, it := range asks {
		open[it.ID] = true
	}
	used := 0
	for id, d := range g.presented {
		switch {
		case d == today:
			used++
		case !open[id]:
			delete(g.presented, id)
		}
	}
	for _, it := range asks {
		_, seen := g.presented[it.ID]
		switch {
		case quiet:
			v.Waiting = append(v.Waiting, it)
		case seen:
			v.Now = append(v.Now, Shown{Item: it})
		case used < g.asksPerDay:
			g.presented[it.ID] = today
			used++
			v.Now = append(v.Now, Shown{Item: it})
		default:
			v.Waiting = append(v.Waiting, it)
		}
	}
	return v
}

// InProgress is what the In progress tab lists when the user opens it: every
// open ask, budget and quiet hours notwithstanding, line-jumpers first, then
// oldest first. Looking is the user's choice, so it costs no budget.
func InProgress(items []Item, now time.Time) []Shown {
	var jumpers, asks []Shown
	for _, it := range items {
		if reason, ok := LineJumper(it, now); ok {
			jumpers = append(jumpers, Shown{Item: it, LineJumper: reason})
		} else if it.Level == Ask {
			asks = append(asks, Shown{Item: it})
		}
	}
	sort.SliceStable(asks, func(i, j int) bool { return before(asks[i].Item, asks[j].Item) })
	return append(jumpers, asks...)
}

func sortOldestFirst(items []Item) {
	sort.SliceStable(items, func(i, j int) bool { return before(items[i], items[j]) })
}

func before(a, b Item) bool {
	if !a.Created.Equal(b.Created) {
		return a.Created.Before(b.Created)
	}
	return a.ID < b.ID
}
