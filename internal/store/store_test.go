package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteAndReopenGivesEmptyMigratedDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "lasso.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCursor("brief", "2026-10-27T13:38:00Z"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, err := s.Version(); err != nil || v != len(migrations) {
		t.Fatalf("version = %d, %v", v, err)
	}
	if c, err := s.Cursor("brief"); err != nil || c != "" {
		t.Fatalf("cursor survived delete: %q %v", c, err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lasso.db")
	s, _ := Open(path)
	s.SetCursor("brief", "a")
	s.SetCursor("brief", "b")
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if c, _ := s.Cursor("brief"); c != "b" {
		t.Fatalf("cursor = %q", c)
	}
}

func TestCacheExpiry(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "lasso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1_800_000_000, 0)
	if err := s.CachePut("k", []byte("v"), now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := s.CacheGet("k", now.Add(30*time.Second)); !ok || string(v) != "v" {
		t.Fatalf("fresh get = %q %v", v, ok)
	}
	if _, ok, _ := s.CacheGet("k", now.Add(2*time.Minute)); ok {
		t.Fatal("expired value returned")
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lasso.db")
	s, _ := Open(path)
	s.DB.Exec(`PRAGMA user_version = 999`)
	s.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("opened a newer schema")
	}
}

func TestSpendAddsUpPerFeatureAndDay(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "lasso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if usd, err := s.Spent("jev", "2026-10-27"); err != nil || usd != 0 {
		t.Fatalf("empty spend = %v %v", usd, err)
	}
	s.AddSpend("jev", "2026-10-27", 0.25)
	s.AddSpend("jev", "2026-10-27", 0.5)
	s.AddSpend("jev", "2026-10-28", 1)
	s.AddSpend("haiku", "2026-10-27", 2)
	if usd, _ := s.Spent("jev", "2026-10-27"); usd != 0.75 {
		t.Fatalf("jev spend = %v", usd)
	}
}

func TestAnswersSince(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "lasso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{now.Add(-8 * 24 * time.Hour), now.Add(-time.Hour), now} {
		if err := s.RecordAnswer(Answer{PromptID: string(rune('a' + i)), Bot: "grok-scout", Action: "answer_prompt", Choice: "yes", Repo: "o/r", At: at}); err != nil {
			t.Fatal(err)
		}
	}
	// Answering prompt c again replaces it rather than adding a fourth.
	if err := s.RecordAnswer(Answer{PromptID: "c", Bot: "grok-scout", Action: "answer_prompt", Choice: "no", At: now}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Answers(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PromptID != "b" || got[0].Repo != "o/r" || !got[0].At.Equal(now.Add(-time.Hour)) || got[1].Choice != "no" {
		t.Fatalf("answers = %+v", got)
	}
	if err := s.RecordAnswer(Answer{}); err == nil {
		t.Fatal("an answer without a prompt id was stored")
	}
}
