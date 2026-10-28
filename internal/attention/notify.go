package attention

import (
	"context"
	"os/exec"
	"time"

	"github.com/brandonapol/lasso/internal/config"
)

// NotifyInterval is the most often lasso raises a desktop notification.
const NotifyInterval = 30 * time.Minute

// Notifier shows one desktop notification. It must never take focus, and
// there is no terminal bell anywhere in lasso.
type Notifier interface {
	Notify(ctx context.Context, title, body string) error
}

// NotifySend is the Notifier for Linux desktops, via notify-send.
type NotifySend struct {
	// run executes the command; nil means exec. Tests replace it.
	run func(ctx context.Context, name string, args ...string) error
}

// Notify runs notify-send at normal urgency. A notification popup doesn't
// take keyboard focus.
func (n NotifySend) Notify(ctx context.Context, title, body string) error {
	run := n.run
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run()
		}
	}
	return run(ctx, "notify-send", "--app-name=lasso", "--urgency=normal", "--", title, body)
}

// Notifications decides whether an item earns a desktop notification. They
// are off unless config turns them on, and then fire only for line-jumpers,
// outside quiet hours, at most once per NotifyInterval, and once per item.
//
// Line-jumpers still show in-app during quiet hours (see Gate.Present); they
// just don't notify until quiet hours end.
type Notifications struct {
	enabled  bool
	quiet    QuietHours
	clock    Clock
	notifier Notifier
	last     time.Time
	sent     map[string]bool
}

// NewNotifications builds the notification policy from config.
func NewNotifications(a config.Attention, n Notifier, clock Clock) (*Notifications, error) {
	q, err := ParseQuietHours(a)
	if err != nil {
		return nil, err
	}
	return &Notifications{enabled: a.Notifications, quiet: q, clock: clock, notifier: n, sent: map[string]bool{}}, nil
}

// Consider notifies about it if every condition holds, and reports whether
// it did. Call it with each open item on every refresh; most calls do
// nothing.
func (n *Notifications) Consider(ctx context.Context, it Item) (bool, error) {
	if !n.enabled || n.notifier == nil || n.sent[it.ID] {
		return false, nil
	}
	now := n.clock()
	reason, ok := LineJumper(it, now)
	if !ok || n.quiet.Contains(now) {
		return false, nil
	}
	if !n.last.IsZero() && now.Sub(n.last) < NotifyInterval {
		return false, nil
	}
	if err := n.notifier.Notify(ctx, "lasso", Plain(reason)); err != nil {
		return false, err
	}
	n.last = now
	n.sent[it.ID] = true
	return true, nil
}
