package attention

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/config"
)

type recorder struct{ bodies []string }

func (r *recorder) Notify(_ context.Context, _, body string) error {
	r.bodies = append(r.bodies, body)
	return nil
}

func blockedBot(id string, now time.Time) Item {
	return Item{ID: id, Kind: KindBotQuestion, Level: Ask, Bot: "grok-scout", Signals: Signals{BlockedSince: now.Add(-3 * time.Hour)}}
}

func TestNotificationsOffByDefault(t *testing.T) {
	r := &recorder{}
	c := &fakeClock{noon}
	n, err := NewNotifications(config.Default().Attention, r, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	if sent, _ := n.Consider(context.Background(), blockedBot("x", noon)); sent || len(r.bodies) != 0 {
		t.Fatal("notified with notifications off")
	}
}

func TestNotificationsLineJumpersOnlyRateLimited(t *testing.T) {
	ctx := context.Background()
	r := &recorder{}
	c := &fakeClock{noon}
	a := config.Default().Attention
	a.Notifications = true
	n, err := NewNotifications(a, r, c.Now)
	if err != nil {
		t.Fatal(err)
	}

	if sent, _ := n.Consider(ctx, ask("plain-ask", noon)); sent {
		t.Fatal("notified for an ordinary ask")
	}
	if sent, _ := n.Consider(ctx, blockedBot("b1", noon)); !sent {
		t.Fatal("didn't notify for a line-jumper")
	}
	if sent, _ := n.Consider(ctx, blockedBot("b1", noon)); sent {
		t.Fatal("notified twice for one item")
	}
	c.now = noon.Add(29 * time.Minute)
	if sent, _ := n.Consider(ctx, blockedBot("b2", c.now)); sent {
		t.Fatal("notified twice within 30 minutes")
	}
	c.now = noon.Add(30 * time.Minute)
	if sent, _ := n.Consider(ctx, blockedBot("b2", c.now)); !sent {
		t.Fatal("rate limit didn't lift after 30 minutes")
	}

	// Quiet hours: shown in-app (see the gate) but no notification.
	c.now = time.Date(2026, 10, 27, 23, 0, 0, 0, tz)
	if sent, _ := n.Consider(ctx, blockedBot("b3", c.now)); sent {
		t.Fatal("notified during quiet hours")
	}
	if len(r.bodies) != 2 {
		t.Fatalf("bodies = %q", r.bodies)
	}
	for _, b := range r.bodies {
		if u := Unexplained(b); len(u) > 0 {
			t.Fatalf("notification %q uses jargon %v", b, u)
		}
	}
}

func TestNotifySendArgs(t *testing.T) {
	var got []string
	n := NotifySend{run: func(_ context.Context, name string, args ...string) error {
		got = append([]string{name}, args...)
		return nil
	}}
	if err := n.Notify(context.Background(), "lasso", "-x looks like a flag"); err != nil {
		t.Fatal(err)
	}
	want := []string{"notify-send", "--app-name=lasso", "--urgency=normal", "--", "lasso", "-x looks like a flag"}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %q", got)
	}
	for _, a := range got {
		if a == "--urgency=critical" || a == "-u critical" {
			t.Fatal("critical urgency pins the popup; lasso must stay quiet")
		}
	}
}
