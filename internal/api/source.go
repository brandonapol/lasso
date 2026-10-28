package api

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/rules"
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
	ID    string `json:"id"`
	Tab   Tab    `json:"tab"`
	Repo  string `json:"repo,omitempty"`
	Title string `json:"title"`
	// Status is the item's state in a word or two ("due 2026-11-03",
	// "failed", "needs you"), for the agent pane and filters.
	Status string `json:"status,omitempty"`
	// Right is the text a row shows at its right-hand end: a due date
	// ("due Nov 3") or a run's or session's status.
	Right string `json:"right,omitempty"`
	// Due is an issue's due date, if it has one.
	Due     *time.Time `json:"due,omitempty"`
	URL     string     `json:"url,omitempty"`
	Updated time.Time  `json:"updated"`
	// Byline says who wrote a summary ("written by claude-sonnet-5-5");
	// clients add the time from Updated. Summaries only.
	Byline string `json:"byline,omitempty"`
	// Body is the text a Summaries card shows, so that tab needs only one
	// request. Summaries only. (A Focus has a Body of its own, and that is
	// the one its JSON "body" carries.)
	Body string `json:"body,omitempty"`
}

// Focus is what the focus pane shows for one item.
type Focus struct {
	Item
	// Fields are the item's facts in display order: a due date and where
	// it came from, a run's failing step, a PR's review state.
	Fields []Field `json:"fields,omitempty"`
	// Sections are the pane's labelled paragraphs, in display order. Their
	// bodies may use simple markdown (bold, code, lists).
	Sections []Section `json:"sections,omitempty"`
	// Take is lasso's own one-line view of the item, if it has one.
	Take string `json:"take,omitempty"`
	// Actions are what the user can do from the focus pane, each on a key.
	Actions []Action `json:"actions,omitempty"`
	// Body is the item's raw text (an issue body, a bot's own words).
	// Clients show it when there are no Sections.
	Body string `json:"body,omitempty"`
}

// Field is one labelled fact in the focus pane.
type Field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Section is one labelled paragraph in the focus pane. Label may be empty.
type Section struct {
	Label string `json:"label,omitempty"`
	Body  string `json:"body"`
}

// Action kinds a client handles itself. Any other kind is the action its
// Request runs.
const (
	ActionOpen    = "open"    // open the item's URL
	ActionExplain = "explain" // explain the bot jargon on screen
)

// Action is one thing the user can do from the focus pane.
type Action struct {
	// Key is the key that does it ("g", "y").
	Key   string `json:"key"`
	Label string `json:"label"`
	// Kind is ActionOpen, ActionExplain, or the action Request runs
	// ("regenerate_summary", "answer_prompt").
	Kind string `json:"kind"`
	// Request is what the client sends to POST /actions, as it is, when
	// the user presses Key. Nil for ActionOpen and ActionExplain.
	Request *rules.Request `json:"request,omitempty"`
}

// Bot states in a Status.
const (
	BotWorking     = "working"     // a session is running
	BotWaiting     = "waiting"     // a session waits on the user
	BotIdle        = "idle"        // nothing is running
	BotUnreachable = "unreachable" // lasso can't read it
)

// BotStatus is one bot adapter's dot in the header.
type BotStatus struct {
	Kind  string `json:"kind"`  // the adapter: "claude", "grok", "saddle"
	Count int    `json:"count"` // how many sessions it has
	State string `json:"state"` // BotWorking, BotWaiting, BotIdle or BotUnreachable
}

// Status is what GET /status answers and status.changed carries: the bots'
// states and when GitHub was last read.
type Status struct {
	Bots []BotStatus `json:"bots"`
	// GitHubSynced is when the hot repos were last read from GitHub
	// without an error; nil until the first time.
	GitHubSynced *time.Time `json:"github_synced,omitempty"`
	// HotRepos are the repos the header names. GET /status fills them from
	// Options.HotRepos when the source leaves them empty.
	HotRepos []string `json:"hot_repos,omitempty"`
}

// StatusSource is a Source that knows the header's status. GET /status
// answers no bots and no sync time for a Source that isn't one.
type StatusSource interface {
	Status(ctx context.Context) (Status, error)
}

// Event kinds pushed over GET /events.
const (
	EventItemChanged  = "item.changed"
	EventItemRemoved  = "item.removed"
	EventBriefUpdated = "brief.updated"
	// EventStatusChanged carries the new Status.
	EventStatusChanged = "status.changed"
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
	mu     sync.Mutex
	items  map[string]Focus
	status Status
	subs   map[chan Event]struct{}
}

// NewFakeSource returns an empty FakeSource.
func NewFakeSource() *FakeSource {
	return &FakeSource{items: map[string]Focus{}, status: Status{Bots: []BotStatus{}}, subs: map[chan Event]struct{}{}}
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

// Status implements StatusSource.
func (f *FakeSource) Status(context.Context) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, nil
}

// SetStatus replaces the status and pushes status.changed.
func (f *FakeSource) SetStatus(st Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st.Bots == nil {
		st.Bots = []BotStatus{}
	}
	f.status = st
	f.publish(Event{Kind: EventStatusChanged, Data: st})
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
