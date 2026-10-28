package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/muesli/termenv"

	"github.com/brandonapol/lasso/internal/api"
)

const token = "t0ken"

var now = time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)

func init() { lipgloss.SetColorProfile(termenv.Ascii) }

// seed fills src with a few rows per tab. Each body carries a word no other
// item uses, so a test can wait for exactly that focus to load.
func seed(src *api.FakeSource) {
	add := func(id string, tab api.Tab, repo, title, status string, age time.Duration, body string) {
		src.Set(api.Focus{Item: api.Item{
			ID: id, Tab: tab, Repo: repo, Title: title, Status: status,
			URL: "https://github.com/" + repo, Updated: now.Add(-age),
		}, Body: body})
	}
	add("lasso#21", api.TabIssues, "brandonapol/lasso", "Due dates from Projects v2 fields", "due Fri", 2*time.Hour,
		"## Why it matters\nThe focus pane should say where a due date came from (zebra).\n\n## Where it stands\nNo one has started it; the milestone is due Friday.\nlasso: needs an owner by Thursday to make the milestone.")
	add("saddle#88", api.TabIssues, "brandonapol/saddle", "Restack loses claims after a crash", "due Nov 3", 26*time.Hour,
		"## What happens\nAfter a crash the agent's claims vanish and a restack (yak) picks the wrong base.\nlasso: real but rare; worth a test before a fix.")
	add("lasso#30", api.TabIssues, "brandonapol/lasso", "Flaky poller test", "no due date", 72*time.Hour,
		"## What happens\nThe poller test times out about once in fifty runs (koala).")
	add("run:lasso:991", api.TabActions, "brandonapol/lasso", "ci · main", "failed", 12*time.Minute,
		"## What failed\nlint: gofmt needed in internal/tui (heron).\nlasso: a one-line fix; rerun after.")
	add("run:saddle:552", api.TabActions, "brandonapol/saddle", "release · v0.4.0", "passed", time.Hour,
		"## Result\nAll jobs passed in 4m (otter).")
	add("doing:lasso#13", api.TabDoing, "brandonapol/lasso", "#13 TUI: three panes", "claude · 40m", 3*time.Minute,
		"## Now\nWriting the golden tests (lynx).\nlasso: on track; nothing needs you.")
	add("doing:lasso#9", api.TabDoing, "brandonapol/lasso", "#9 grok live sessions spike", "grok · idle", 5*time.Hour,
		"## Now\nWaiting for an answer about the headless mode (moose).")
	add("sum:lasso", api.TabSummaries, "brandonapol/lasso", "lasso, last 24h", "written by claude-sonnet-5-5", 55*time.Minute,
		"Three pull requests merged: the API skeleton, the GitHub poller and the attention levels. The TUI is in progress and CI is red on a formatting slip (walrus).")
	add("sum:saddle", api.TabSummaries, "brandonapol/saddle", "saddle, last 24h", "written by claude-sonnet-5-5", 3*time.Hour,
		"Quiet day. One release went out and an issue about lost claims after a crash was filed (badger).")
}

type harness struct {
	src    *api.FakeSource
	hub    *api.Hub
	chat   *fakeChat
	client *Client
	opened []string
	mu     sync.Mutex
}

// sinceYouLeft is what the fake assistant says when the TUI opens; golden
// tests wait for it so the left pane is settled.
const sinceYouLeft = "Since you left, CI on main failed."

// fakeChat is an assistant that echoes.
type fakeChat struct {
	mu   sync.Mutex
	msgs []api.ChatMessage
}

func (f *fakeChat) History(context.Context) ([]api.ChatMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.ChatMessage(nil), f.msgs...), nil
}

func (f *fakeChat) Send(_ context.Context, text string) ([]api.ChatMessage, error) {
	f.mu.Lock()
	f.msgs = append(f.msgs, api.ChatMessage{Role: api.RoleUser, Text: text, At: now},
		api.ChatMessage{Role: api.RoleLasso, Text: "You said: " + text, At: now})
	f.mu.Unlock()
	return f.History(context.Background())
}

func (f *fakeChat) Open(context.Context) ([]api.ChatMessage, error) {
	f.mu.Lock()
	if len(f.msgs) == 0 {
		f.msgs = append(f.msgs, api.ChatMessage{Role: api.RoleLasso, Kind: "since_you_left", Text: sinceYouLeft, At: now})
	}
	f.mu.Unlock()
	return f.History(context.Background())
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	src := api.NewFakeSource()
	seed(src)
	hub, chat := api.NewHub(), &fakeChat{}
	srv := httptest.NewServer(api.Handler(src, api.Options{Token: token, Port: 7337, KeepAlive: time.Hour, Hub: hub, Chat: chat}))
	t.Cleanup(srv.Close)
	return &harness{src: src, hub: hub, chat: chat, client: NewClient(srv.URL, token, nil)}
}

func (h *harness) model(t *testing.T) *Model {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // runs before srv.Close, so the event stream ends first
	return New(Options{
		Client: h.client, Context: ctx, Now: func() time.Time { return now },
		HotRepos: []string{"brandonapol/lasso", "brandonapol/saddle"},
		Open: func(u string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.opened = append(h.opened, u)
			return nil
		},
		Reconnect: 50 * time.Millisecond,
	})
}

func start(t *testing.T, m *Model, w, h int) *teatest.TestModel {
	t.Helper()
	return teatest.NewTestModel(t, m, teatest.WithInitialTermSize(w, h))
}

func keys(tm *teatest.TestModel, ks ...string) {
	for _, k := range ks {
		switch k {
		case "down":
			tm.Send(tea.KeyMsg{Type: tea.KeyDown})
		case "up":
			tm.Send(tea.KeyMsg{Type: tea.KeyUp})
		case "enter":
			tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
		case "tab":
			tm.Send(tea.KeyMsg{Type: tea.KeyTab})
		case "esc":
			tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
		default:
			tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		}
	}
}

// seen accumulates each program's output, so a wait can match words drawn
// before an earlier wait returned.
var (
	seenMu sync.Mutex
	seen   = map[*teatest.TestModel]*bytes.Buffer{}
)

func waitFor(t *testing.T, tm *teatest.TestModel, words ...string) {
	t.Helper()
	seenMu.Lock()
	buf := seen[tm]
	if buf == nil {
		buf = &bytes.Buffer{}
		seen[tm] = buf
	}
	seenMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := io.ReadAll(tm.Output())
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		ok := true
		for _, w := range words {
			if !bytes.Contains(buf.Bytes(), []byte(w)) {
				ok = false
			}
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw %q; output so far:\n%s", words, buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func finish(t *testing.T, tm *teatest.TestModel) *Model {
	t.Helper()
	keys(tm, "q")
	return tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(*Model)
}

// What each tab's golden waits for: its rows, and at full width the first
// row's focus body. Summaries also waits for every card's body.
var tabWaits = []struct {
	key   string
	name  string
	rows  []string
	focus string
}{
	{"1", "issues", []string{"Due dates from Projects", "Flaky poller test"}, "zebra"},
	{"2", "actions", []string{"ci · main", "release · v0.4.0"}, "heron"},
	{"3", "in-progress", []string{"#13 TUI: three panes", "#9 grok live"}, "lynx"},
	{"4", "summaries", []string{"walrus", "badger"}, "walrus"},
}

func TestTabsGolden(t *testing.T) {
	for _, size := range [][2]int{{200, 50}, {100, 30}} {
		for _, tab := range tabWaits {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], tab.name), func(t *testing.T) {
				h := newHarness(t)
				tm := start(t, h.model(t), size[0], size[1])
				waitFor(t, tm, "live")
				keys(tm, tab.key)
				want := append([]string{sinceYouLeft}, tab.rows...)
				if size[0] >= wideWidth {
					want = append(want, tab.focus)
				}
				waitFor(t, tm, want...)
				m := finish(t, tm)
				golden.RequireEqual(t, []byte(m.View()))
			})
		}
	}
}

// Selecting the second row in each tab shows that row's focus content.
func TestSelectionShowsFocus(t *testing.T) {
	cases := []struct {
		key, id, word, take string
	}{
		{"1", "saddle#88", "yak", "real but rare"},
		{"2", "run:saddle:552", "otter", ""},
		{"3", "doing:lasso#9", "moose", ""},
		{"4", "sum:saddle", "badger", ""},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			h := newHarness(t)
			tm := start(t, h.model(t), 200, 50)
			waitFor(t, tm, "live")
			keys(tm, c.key)
			time.Sleep(50 * time.Millisecond) // let the tab's rows land before moving
			keys(tm, "down")
			waitFor(t, tm, c.word)
			m := finish(t, tm)
			if m.focus == nil || m.focus.ID != c.id {
				t.Fatalf("focus = %+v, want %s", m.focus, c.id)
			}
			focus := m.viewFocus(rightWidth, 48)
			if !strings.Contains(focus, c.word) {
				t.Fatalf("focus pane lacks %q:\n%s", c.word, focus)
			}
			if c.take != "" && !strings.Contains(focus, "lasso · "+c.take) {
				t.Fatalf("focus pane lacks lasso's take %q:\n%s", c.take, focus)
			}
		})
	}
}

// Below 140 columns, enter lays the focus pane over the middle one.
func TestNarrowFocusOverlay(t *testing.T) {
	h := newHarness(t)
	tm := start(t, h.model(t), 100, 30)
	waitFor(t, tm, "live", "Due dates from Projects", sinceYouLeft)
	keys(tm, "enter")
	waitFor(t, tm, "zebra")
	m := finish(t, tm)
	golden.RequireEqual(t, []byte(m.View()))
}

// Below 90 columns one pane shows at a time.
func TestOnePaneAtATime(t *testing.T) {
	h := newHarness(t)
	tm := start(t, h.model(t), 80, 24)
	waitFor(t, tm, "live", "Due dates from Projects")
	keys(tm, "tab")
	waitFor(t, tm, "zebra")
	m := finish(t, tm)
	v := m.View()
	if strings.Contains(v, "1 Issues") || strings.Contains(v, "assistant") {
		t.Fatalf("expected only the focus pane:\n%s", v)
	}
	golden.RequireEqual(t, []byte(v))
}

func TestMouseSelectsTabAndRow(t *testing.T) {
	h := newHarness(t)
	m := h.model(t)
	tm := start(t, m, 200, 50)
	waitFor(t, tm, "live", "zebra")
	// "2 Actions" starts at column 1 + len("1 Issues") + 3 of the middle pane.
	x := leftWidth + 1 + 1 + len("1 Issues") + tabGap + 2
	tm.Send(tea.MouseMsg{X: x, Y: bodyTop, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	waitFor(t, tm, "heron")
	// The second row's title line.
	tm.Send(tea.MouseMsg{X: leftWidth + 10, Y: bodyTop + listTop + 3, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	waitFor(t, tm, "otter")
	m = finish(t, tm)
	if m.tab != 1 || m.focusID != "run:saddle:552" {
		t.Fatalf("tab=%d focus=%s", m.tab, m.focusID)
	}
}

func TestOpenAndChangedMarker(t *testing.T) {
	h := newHarness(t)
	tm := start(t, h.model(t), 200, 50)
	waitFor(t, tm, "live", "zebra")
	keys(tm, "o")
	waitFor(t, tm, "opened https://github.com/brandonapol/lasso")
	// A change to an unselected row marks it quietly and doesn't move the
	// selection or switch tabs.
	h.src.Set(api.Focus{Item: api.Item{ID: "lasso#30", Tab: api.TabIssues, Repo: "brandonapol/lasso",
		Title: "Flaky poller test, again", Status: "no due date", Updated: now.Add(-72 * time.Hour)}})
	h.src.Set(api.Focus{Item: api.Item{ID: "run:saddle:552", Tab: api.TabActions, Repo: "brandonapol/saddle",
		Title: "release · v0.4.1", Status: "running", Updated: now.Add(-2 * time.Hour)}})
	waitFor(t, tm, "• Flaky poller test, again")
	time.Sleep(50 * time.Millisecond) // give a wrong tab switch time to show
	m := finish(t, tm)
	if m.tab != 0 || m.focusID != "lasso#21" {
		t.Fatalf("selection moved: tab=%d focus=%s", m.tab, m.focusID)
	}
	if !m.changed["lasso#30"] || !m.changed["run:saddle:552"] {
		t.Fatalf("changed rows aren't marked: %v", m.changed)
	}
	if v := m.View(); !strings.Contains(v, "• Flaky poller test, again") {
		t.Fatalf("no marker:\n%s", v)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.opened) != 1 || h.opened[0] != "https://github.com/brandonapol/lasso" {
		t.Fatalf("opened %v", h.opened)
	}
}

func TestSearchFilters(t *testing.T) {
	h := newHarness(t)
	tm := start(t, h.model(t), 200, 50)
	waitFor(t, tm, "live", "zebra")
	keys(tm, "/", "f", "l", "a", "k", "enter")
	waitFor(t, tm, "koala")
	m := finish(t, tm)
	if v := m.visible(0); len(v) != 1 || v[0].ID != "lasso#30" {
		t.Fatalf("visible = %+v", v)
	}
}

func TestExplainUsesGlossary(t *testing.T) {
	h := newHarness(t)
	tm := start(t, h.model(t), 200, 50)
	waitFor(t, tm, "live", "zebra")
	keys(tm, "down")
	waitFor(t, tm, "yak")
	keys(tm, "?")
	waitFor(t, tm, "EXPLAIN")
	m := finish(t, tm)
	v := m.viewFocus(rightWidth, 48)
	if !strings.Contains(v, "restack means") || !strings.Contains(v, "reserved files") {
		t.Fatalf("explain overlay:\n%s", v)
	}
}

func TestParseBody(t *testing.T) {
	secs, take := parseBody("intro\n## One\nfirst\n\n## Two\nsecond\nlasso: short take")
	if take != "short take" {
		t.Fatalf("take = %q", take)
	}
	want := []section{{"", "intro"}, {"One", "first"}, {"Two", "second"}}
	if fmt.Sprint(secs) != fmt.Sprint(want) {
		t.Fatalf("sections = %v", secs)
	}
}

func TestChatSendsAndShowsReply(t *testing.T) {
	h := newHarness(t)
	tm := start(t, h.model(t), 200, 50)
	waitFor(t, tm, "live", sinceYouLeft)
	tm.Send(tea.KeyMsg{Type: tea.KeyShiftTab}) // list → assistant
	keys(tm, "what's due?", "enter")
	waitFor(t, tm, "You said: what's due?")
	keys(tm, "esc")
	m := finish(t, tm)
	if len(m.chat) != 3 || m.pending != "" || m.chat[0].Text != sinceYouLeft {
		t.Fatalf("chat = %+v pending %q", m.chat, m.pending)
	}
}

func TestAssistantFocusMovesRightPane(t *testing.T) {
	h := newHarness(t)
	tm := start(t, h.model(t), 200, 50)
	waitFor(t, tm, "live", "zebra", sinceYouLeft)
	h.hub.Publish(api.Event{Kind: api.EventFocus, Data: map[string]string{"id": "run:saddle:552", "tab": "actions"}})
	waitFor(t, tm, "otter")
	// A chat update from the daemon (a line-jumper) is read and shown.
	h.chat.mu.Lock()
	h.chat.msgs = append(h.chat.msgs, api.ChatMessage{Role: api.RoleLasso, Kind: "line_jumper", Text: "grok has been waiting on you for 3 hours.", At: now})
	h.chat.mu.Unlock()
	h.hub.Publish(api.Event{Kind: api.EventChatUpdated, Data: map[string]int{"messages": 2}})
	waitFor(t, tm, "waiting on you")
	m := finish(t, tm)
	if m.tab != 1 || m.focusID != "run:saddle:552" {
		t.Fatalf("tab=%d focus=%s", m.tab, m.focusID)
	}
}
