package api

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"
)

// Tab is one of the middle pane's tabs.
type Tab string

// The tabs GET /items accepts.
const (
	TabIssues    Tab = "issues"
	TabActions   Tab = "actions"
	TabDoing     Tab = "doing"
	TabSummaries Tab = "summaries"
)

// Tabs lists every tab in display order.
var Tabs = []Tab{TabIssues, TabActions, TabDoing, TabSummaries}

// Valid reports whether t is a known tab.
func (t Tab) Valid() bool { return slices.Contains(Tabs, t) }

// Item is one row in a tab.
type Item struct {
	ID      string    `json:"id"`
	Tab     Tab       `json:"tab"`
	Repo    string    `json:"repo,omitempty"`
	Title   string    `json:"title"`
	Status  string    `json:"status,omitempty"`
	URL     string    `json:"url,omitempty"`
	Updated time.Time `json:"updated"`
}

// Focus is what the focus pane shows for one item.
type Focus struct {
	Item
	// Fields are the item's facts in display order: a due date and where
	// it came from, a run's failing step, a PR's review state.
	Fields []Field `json:"fields,omitempty"`
	Body   string  `json:"body,omitempty"`
}

// Field is one labelled fact in the focus pane.
type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Event kinds pushed over GET /events.
const (
	EventItemChanged  = "item.changed"
	EventItemRemoved  = "item.removed"
	EventBriefUpdated = "brief.updated"
)

// Event is one change pushed to clients. Data is JSON-encoded as the SSE
// data line.
type Event struct {
	Kind string `json:"kind"`
	Data any    `json:"data"`
}

// ErrNotFound is returned by Source.Focus for an unknown id.
var ErrNotFound = errors.New("not found")

// Source is what the API serves. Real sources (GitHub, bots, summaries)
// plug in behind it; FakeSource stands in until they exist.
type Source interface {
	Items(ctx context.Context, tab Tab) ([]Item, error)
	Focus(ctx context.Context, id string) (Focus, error)
	// Subscribe returns a channel of changes and a func to stop. The source
	// closes the channel if the subscriber falls too far behind, so the
	// client reconnects and re-reads instead of missing changes silently.
	Subscribe() (<-chan Event, func())
}

// FakeSource is an in-memory Source for tests and for the daemon until the
// real sources land.
type FakeSource struct {
	mu    sync.Mutex
	items map[string]Focus
	subs  map[chan Event]struct{}
}

// NewFakeSource returns an empty FakeSource.
func NewFakeSource() *FakeSource {
	return &FakeSource{items: map[string]Focus{}, subs: map[chan Event]struct{}{}}
}

// Items returns the tab's items, newest first.
func (f *FakeSource) Items(_ context.Context, tab Tab) ([]Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []Item{}
	for _, it := range f.items {
		if it.Tab == tab {
			out = append(out, it.Item)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.After(out[j].Updated)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Focus returns one item with its body.
func (f *FakeSource) Focus(_ context.Context, id string) (Focus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	it, ok := f.items[id]
	if !ok {
		return Focus{}, ErrNotFound
	}
	return it, nil
}

// Set adds or replaces an item and pushes item.changed.
func (f *FakeSource) Set(it Focus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[it.ID] = it
	f.publish(Event{Kind: EventItemChanged, Data: it.Item})
}

// Remove deletes an item and pushes item.removed.
func (f *FakeSource) Remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.items[id]; !ok {
		return
	}
	delete(f.items, id)
	f.publish(Event{Kind: EventItemRemoved, Data: map[string]string{"id": id}})
}

// SetBrief pushes brief.updated with the new brief text.
func (f *FakeSource) SetBrief(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publish(Event{Kind: EventBriefUpdated, Data: map[string]string{"text": text}})
}

// Subscribe implements Source.
func (f *FakeSource) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.subs[ch]; ok {
			delete(f.subs, ch)
			close(ch)
		}
	}
}

// publish sends ev to every subscriber; f.mu must be held. A subscriber
// whose buffer is full is dropped (its channel closed).
func (f *FakeSource) publish(ev Event) {
	for ch := range f.subs {
		select {
		case ch <- ev:
		default:
			delete(f.subs, ch)
			close(ch)
		}
	}
}
