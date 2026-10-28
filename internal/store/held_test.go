package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHeldMessagesSurviveReopenUntilDeleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lasso.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	for _, h := range []Held{
		{ID: "b", Adapter: "claude", Session: "s1", Text: "second", Created: t0.Add(time.Minute)},
		{ID: "a", Adapter: "claude", Session: "s1", Text: "first", Created: t0},
		{ID: "g", Adapter: "grok", Session: "s2", Text: "other bot", Created: t0},
	} {
		if err := s.AddHeld(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddHeld(Held{Adapter: "claude"}); err == nil {
		t.Fatal("a held message with no id was kept")
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hs, err := s.HeldFor("claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 2 || hs[0].Text != "first" || hs[1].Text != "second" || hs[0].Session != "s1" || !hs[0].Created.Equal(t0) {
		t.Fatalf("held = %+v", hs)
	}
	if err := s.DeleteHeld("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteHeld("a"); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
	if hs, _ := s.HeldFor("claude"); len(hs) != 1 || hs[0].ID != "b" {
		t.Fatalf("after delete: %+v", hs)
	}
	if hs, _ := s.HeldFor("grok"); len(hs) != 1 {
		t.Fatalf("grok's: %+v", hs)
	}
}
