package attention

import (
	"fmt"
	"sort"
	"time"
)

// BriefCursor is the store cursor name for "last time the user read the
// brief". Its value is an RFC 3339 time.
const BriefCursor = "brief"

// CursorReader is the part of the store SinceYouLeft needs.
type CursorReader interface {
	Cursor(name string) (string, error)
}

// LastSeen reads the brief cursor. Unset (a fresh or deleted database) is
// the zero time, so the first brief shows everything still open.
func LastSeen(s CursorReader) (time.Time, error) {
	v, err := s.Cursor(BriefCursor)
	if err != nil || v == "" {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("cursor %s: %w", BriefCursor, err)
	}
	return t, nil
}

// FormatCursor is the value to store under BriefCursor after the user reads
// the brief.
func FormatCursor(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// RepoBrief is one repo's share of the "since you left" message.
type RepoBrief struct {
	Repo  string // "" for items not tied to a repo
	Items []Item
}

// SinceYouLeft returns the brief items created after cursor, grouped by
// repo. Repos are in name order with repo-less items last; items within a
// repo are oldest first.
func SinceYouLeft(items []Item, cursor time.Time) []RepoBrief {
	byRepo := map[string][]Item{}
	for _, it := range items {
		if it.Level == Brief && it.Created.After(cursor) {
			byRepo[it.Repo] = append(byRepo[it.Repo], it)
		}
	}
	repos := make([]string, 0, len(byRepo))
	for r := range byRepo {
		repos = append(repos, r)
	}
	sort.Slice(repos, func(i, j int) bool {
		if (repos[i] == "") != (repos[j] == "") {
			return repos[j] == ""
		}
		return repos[i] < repos[j]
	})
	out := make([]RepoBrief, 0, len(repos))
	for _, r := range repos {
		its := byRepo[r]
		sortOldestFirst(its)
		out = append(out, RepoBrief{Repo: r, Items: its})
	}
	return out
}
