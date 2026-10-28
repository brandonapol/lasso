package attention

import (
	"fmt"
	"time"
)

// The line-jumper thresholds (ADR 0001 §5). The list is fixed on purpose:
// adding to it makes lasso louder, so it takes a code change and a review.
const (
	RedThreshold     = 30 * time.Minute // default branch red this long in a hot repo
	DueWindow        = 24 * time.Hour   // due within this long...
	ProgressWindow   = 24 * time.Hour   // ...with no progress in this long
	BlockedThreshold = 2 * time.Hour    // a bot waiting on the user this long
)

type lineJumper struct {
	name  string
	check func(it Item, now time.Time) (reason string, ok bool)
}

var lineJumpers = []lineJumper{
	{"default-branch-red", func(it Item, now time.Time) (string, bool) {
		s := it.Signals
		if it.Kind != KindRunFailed || !s.DefaultBranch || !s.HotRepo || s.RedSince.IsZero() {
			return "", false
		}
		red := now.Sub(s.RedSince)
		if red <= RedThreshold {
			return "", false
		}
		return fmt.Sprintf("The default branch of %s has been failing for %s.", it.Repo, span(red)), true
	}},
	{"due-without-progress", func(it Item, now time.Time) (string, bool) {
		if it.Due.IsZero() || it.Due.Sub(now) >= DueWindow {
			return "", false
		}
		if p := it.Signals.LastProgress; !p.IsZero() && now.Sub(p) < ProgressWindow {
			return "", false
		}
		if it.Due.Before(now) {
			return fmt.Sprintf("%q was due %s ago and nothing has moved on it.", it.Title, span(now.Sub(it.Due))), true
		}
		return fmt.Sprintf("%q is due in %s and nothing has moved on it.", it.Title, span(it.Due.Sub(now))), true
	}},
	{"bot-blocked", func(it Item, now time.Time) (string, bool) {
		b := it.Signals.BlockedSince
		if it.Bot == "" || b.IsZero() || now.Sub(b) <= BlockedThreshold {
			return "", false
		}
		return fmt.Sprintf("%s has been waiting on you for %s.", it.Bot, span(now.Sub(b))), true
	}},
}

// LineJumper reports whether an item may skip the budget and quiet hours,
// and says why in plain words.
func LineJumper(it Item, now time.Time) (reason string, ok bool) {
	for _, j := range lineJumpers {
		if reason, ok := j.check(it, now); ok {
			return reason, true
		}
	}
	return "", false
}

// span renders a duration the way a person would say it.
func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < 2*time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
