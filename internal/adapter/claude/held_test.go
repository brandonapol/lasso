package claude

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
)

// memHeld is an adapter.HeldStore in memory, standing in for lasso.db.
type memHeld struct {
	mu       sync.Mutex
	held     []adapter.HeldMessage
	released []string // "text" or "text: error"
}

func (m *memHeld) Hold(h adapter.HeldMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.held = append(m.held, h)
	return nil
}

func (m *memHeld) Release(h adapter.HeldMessage, err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.held = slices.DeleteFunc(m.held, func(x adapter.HeldMessage) bool { return x.ID == h.ID })
	r := h.Text
	if err != nil {
		r += ": " + err.Error()
	}
	m.released = append(m.released, r)
	return nil
}

func (m *memHeld) Held(kind string) ([]adapter.HeldMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []adapter.HeldMessage
	for _, h := range m.held {
		if h.Adapter == kind {
			out = append(out, h)
		}
	}
	return out, nil
}

func (m *memHeld) snapshot() (held []adapter.HeldMessage, released []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.held), slices.Clone(m.released)
}

// A message held for an open session is kept in the store. After a
// restart the new adapter reloads it and resumes the session with it
// once the session's process exits, then clears it from the store.
func TestHeldMessageSurvivesRestart(t *testing.T) {
	e := setup(t)
	e.live(t, 4242, "idle", "app-idle", "idle", "/run/x.sock") // no relay can start
	store := &memHeld{}

	a1 := e.adapter(t, noResume(t))
	if err := a1.UseHeldStore(store); err != nil {
		t.Fatal(err)
	}
	d, err := a1.Send(context.Background(), "idle", "hello")
	if err != nil || d != adapter.Held {
		t.Fatalf("send = %q, %v; want held", d, err)
	}
	held, _ := store.snapshot()
	if len(held) != 1 || held[0].Adapter != Kind || held[0].Session != "idle" || held[0].Text != "hello" || held[0].ID == "" {
		t.Fatalf("store = %+v", held)
	}
	_ = a1.Close() // lasso stops

	rec := &recorder{}
	a2 := e.adapter(t, rec.run)
	if err := a2.UseHeldStore(store); err != nil {
		t.Fatal(err)
	}
	if _, err := a2.Sessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(rec.got()); n != 0 {
		t.Fatalf("resumed %d times while the session is open", n)
	}
	e.closed(t, 4242) // the terminal closes
	waitFor(t, a2, "the held message to go out", func() bool { return len(rec.got()) == 1 })
	if r := rec.got()[0]; r[3] != "idle" || r[4] != "hello" {
		t.Errorf("resumed with %q", r)
	}
	waitFor(t, a2, "the store to be cleared", func() bool {
		held, released := store.snapshot()
		return len(held) == 0 && slices.Equal(released, []string{"hello"})
	})
}
