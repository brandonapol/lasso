package attention

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/store"
)

func TestSinceYouLeft(t *testing.T) {
	left := noon.Add(-2 * time.Hour)
	items := []Item{
		{ID: "before", Level: Brief, Repo: "a/x", Created: left.Add(-time.Minute)},
		{ID: "at", Level: Brief, Repo: "a/x", Created: left},
		{ID: "x2", Level: Brief, Repo: "a/x", Created: left.Add(30 * time.Minute)},
		{ID: "x1", Level: Brief, Repo: "a/x", Created: left.Add(10 * time.Minute)},
		{ID: "log", Level: Log, Repo: "a/x", Created: left.Add(time.Minute)},
		{ID: "ask", Level: Ask, Repo: "a/x", Created: left.Add(time.Minute)},
		{ID: "loose", Level: Brief, Created: left.Add(time.Minute)},
		{ID: "w1", Level: Brief, Repo: "a/w", Created: left.Add(time.Minute)},
	}
	got := SinceYouLeft(items, left)
	want := []struct {
		repo string
		ids  []string
	}{
		{"a/w", []string{"w1"}},
		{"a/x", []string{"x1", "x2"}},
		{"", []string{"loose"}},
	}
	if len(got) != len(want) {
		t.Fatalf("groups = %+v", got)
	}
	for i, w := range want {
		if got[i].Repo != w.repo || !equal(itemIDs(got[i].Items), w.ids) {
			t.Errorf("group %d = %s %v, want %s %v", i, got[i].Repo, itemIDs(got[i].Items), w.repo, w.ids)
		}
	}
}

func TestLastSeenFromStore(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "lasso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if c, err := LastSeen(s); err != nil || !c.IsZero() {
		t.Fatalf("unset cursor = %v, %v", c, err)
	}
	if err := s.SetCursor(BriefCursor, FormatCursor(noon)); err != nil {
		t.Fatal(err)
	}
	c, err := LastSeen(s)
	if err != nil || !c.Equal(noon) {
		t.Fatalf("cursor = %v, %v", c, err)
	}
	got := SinceYouLeft([]Item{{ID: "old", Level: Brief, Created: noon}, {ID: "new", Level: Brief, Created: noon.Add(time.Second)}}, c)
	if len(got) != 1 || !equal(itemIDs(got[0].Items), []string{"new"}) {
		t.Fatalf("since = %+v", got)
	}
}
