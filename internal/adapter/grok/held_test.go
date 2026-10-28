package grok

import (
	"context"
	"os"
	"path/filepath"
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

// Messages held for an open window are kept in the store. After a restart
// the new adapter reloads them and sends them, as one turn, once the
// window closes; then it clears them from the store.
func TestHeldMessagesSurviveRestart(t *testing.T) {
	f := newFixture(t) // no leader
	store := &memHeld{}
	if err := f.a.UseHeldStore(store); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"one", "two"} {
		d, err := f.a.Send(context.Background(), sLive, m)
		if err != nil || d != adapter.Held {
			t.Fatalf("send = %q, %v; want held", d, err)
		}
	}
	if held, _ := store.snapshot(); len(held) != 2 || held[0].Session != sLive || held[0].Adapter != Kind || held[1].Text != "two" {
		t.Fatalf("store = %+v", held)
	}
	_ = f.a.Close() // lasso stops

	f.a = New(f.a.opts)
	if err := f.a.UseHeldStore(store); err != nil {
		t.Fatal(err)
	}
	_, _, _ = f.a.Events(context.Background(), "v1 0;")
	f.a.wait(5 * time.Second)
	if c := ranGrok(t, f); len(c) > 0 {
		t.Fatalf("resumed while the window is open: %+v", c)
	}
	if err := os.WriteFile(filepath.Join(f.home, "active_sessions.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, _ = f.a.Events(context.Background(), "v1 0;")
	if !f.a.wait(10 * time.Second) {
		t.Fatal("turn never finished")
	}
	c := ranGrok(t, f)
	if len(c) != 1 || flagValue(c[0].Args, "--resume") != sLive || c[0].Prompt != "one\n\ntwo" {
		t.Fatalf("calls = %+v", c)
	}
	if held, released := store.snapshot(); len(held) != 0 || !slices.Equal(released, []string{"one", "two"}) {
		t.Fatalf("store = %+v, released %q", held, released)
	}
}

// Close hangs up a leader connection held open for a running turn, and
// says nothing about losing the leader: lasso is the one leaving.
func TestCloseHangsUpTheLeader(t *testing.T) {
	f, fl := leaderFixture(t)
	fl.own(sLive, true)
	fl.perm = true // the turn stops at a prompt, so the connection stays open
	d, err := f.a.Send(context.Background(), sLive, "run the tests")
	if err != nil || d != adapter.Sent {
		t.Fatalf("send = %q, %v", d, err)
	}
	if f.a.wait(100 * time.Millisecond) {
		t.Fatal("the turn ended before Close; the test proves nothing")
	}
	if err := f.a.Close(); err != nil {
		t.Fatal(err)
	}
	if !f.a.wait(5 * time.Second) {
		t.Fatal("the leader connection is still open after Close")
	}
	if evs := eventsWith(t, f, "lost contact"); len(evs) > 0 {
		t.Errorf("notes = %+v", evs)
	}
	if _, err := f.a.Send(context.Background(), sLive, "again"); err == nil {
		t.Error("sent through the leader after Close")
	}
}
