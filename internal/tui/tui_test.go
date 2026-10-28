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
	"github.com/brandonapol/lasso/internal/rules"
)

const token = "t0ken"

var now = time.Date(2026, 10, 27, 15, 0, 0, 0, time.UTC)

func init() { lipgloss.SetColorProfile(termenv.Ascii) }

// sec is one focus section.
func sec(label, body string) api.Section { return api.Section{Label: label, Body: body} }

var (
	openKey    = api.Action{Key: "o", Label: "open on GitHub", Kind: api.ActionOpen}
	explainKey = api.Action{Key: "?", Label: "explain", Kind: api.ActionExplain}
)

func regenKey(repo string) api.Action {
	return api.Action{Key: "g", Label: "regenerate", Kind: "regenerate_summary",
		Request: &rules.Request{Action: "regenerate_summary", Repo: repo}}
}

// seed fills src with a few rows per tab. Each focus carries a word no
// other item uses, so a test can wait for exactly that focus to load.
func seed(src *api.FakeSource) {
	type row struct {
		id     string
		tab    api.Tab
		repo   string
		title  string
		right  string
		age    time.Duration
		secs   []api.Section
		take   string
		byline string
		card   string
		acts   []api.Action
	}
	rows := []row{
		{id: "lasso#21", tab: api.TabIssues, repo: "brandonapol/lasso", title: "Due dates from Projects v2 fields", right: "due Fri", age: 2 * time.Hour,
			secs: []api.Section{
				sec("Why it matters", "The focus pane should say where a due date came from (zebra)."),
				sec("Where it stands", "**Difficulty: standard.** No one has started it; the `milestone` is due Friday.\n- needs an owner\n- needs a test"),
			},
			take: "needs an owner by Thursday to make the milestone."},
		{id: "saddle#88", tab: api.TabIssues, repo: "brandonapol/saddle", title: "Restack loses claims after a crash", right: "due Nov 3", age: 26 * time.Hour,
			secs: []api.Section{sec("What happens", "After a crash the agent's claims vanish and a restack (yak) picks the wrong base.")},
			take: "real but rare; worth a test before a fix."},
		{id: "lasso#30", tab: api.TabIssues, repo: "brandonapol/lasso", title: "Flaky poller test", right: "no due date", age: 72 * time.Hour,
			secs: []api.Section{sec("What happens", "The poller test times out about once in fifty runs (koala).")}},
		{id: "run:lasso:991", tab: api.TabActions, repo: "brandonapol/lasso", title: "ci · main", right: "failed", age: 12 * time.Minute,
			secs: []api.Section{sec("What failed", "lint: gofmt needed in internal/tui (heron).")}, take: "a one-line fix; rerun after."},
		{id: "run:saddle:552", tab: api.TabActions, repo: "brandonapol/saddle", title: "release · v0.4.0", right: "passed", age: time.Hour,
			secs: []api.Section{sec("Result", "All jobs passed in 4m (otter).")}},
		{id: "doing:lasso#13", tab: api.TabDoing, repo: "brandonapol/lasso", title: "#13 TUI: three panes", right: "claude · 40m", age: 3 * time.Minute,
			secs: []api.Section{sec("Now", "Writing the golden tests (lynx).")}, take: "on track; nothing needs you."},
		{id: "doing:lasso#9", tab: api.TabDoing, repo: "brandonapol/lasso", title: "#9 grok live sessions spike", right: "grok · idle", age: 5 * time.Hour,
			secs: []api.Section{sec("Now", "Waiting for an answer about the headless mode (moose).")}},
		{id: "sum:lasso", tab: api.TabSummaries, repo: "brandonapol/lasso", title: "lasso, last 24h", age: 55 * time.Minute,
			byline: "written by claude-sonnet-5-5",
			card:   "Three pull requests merged: the API skeleton, the GitHub poller and the attention levels. The TUI is in progress and CI is red on a formatting slip (walrus).",
			acts:   []api.Action{regenKey("brandonapol/lasso")}},
		{id: "sum:saddle", tab: api.TabSummaries, repo: "brandonapol/saddle", title: "saddle, last 24h", age: 3 * time.Hour,
			byline: "written by claude-sonnet-5-5",
			card:   "Quiet day. One release went out and an issue about lost claims after a crash was filed (badger).",
			acts:   []api.Action{regenKey("brandonapol/saddle")}},
	}
	for _, r := range rows {
		f := api.Focus{
			Item: api.Item{
				ID: r.id, Tab: r.tab, Repo: r.repo, Title: r.title, Status: "status is not shown", Right: r.right,
				URL: "https://github.com/" + r.repo, Updated: now.Add(-r.age), Byline: r.byline, Body: r.card,
			},
			Sections: r.secs, Take: r.take,
			Actions: append(append([]api.Action(nil), r.acts...), openKey, explainKey),
		}
		if r.card != "" {
			f.Sections = []api.Section{sec("The last 24 hours", r.card), sec("Counts", "3 pull requests merged ("+r.id+" counts).")}
		}
		src.Set(f)
	}
	synced := now.Add(-4 * time.Minute)
	src.SetStatus(api.Status{
		Bots: []api.BotStatus{
			{Kind: "claude", Count: 2, State: api.BotWorking},
			{Kind: "grok", Count: 1, State: api.BotWaiting},
			{Kind: "saddle", State: api.BotIdle},
		},
		GitHubSynced: &synced,
	})
}

// ask is a bot's question with two answers, on In progress.
func ask() api.Focus {
	answer := func(key, label, option, choice string) api.Action {
		return api.Action{Key: key, Label: label, Kind: "answer_prompt", Request: &rules.Request{
			Bot: "grok-scout", Action: "answer_prompt", Repo: "brandonapol/lasso", Target: "p7",
			Args: map[string]string{"option": option, "choice": choice},
		}}
	}
	return api.Focus{
		Item: api.Item{ID: "doing:bot:grok:p7", Tab: api.TabDoing, Repo: "brandonapol/lasso",
			Title: "scout: may I run the tests?", Right: "needs you", Updated: now},
		Sections: []api.Section{sec("What it wants", "scout (grok) wants your permission (heron2).")},
		Take:     "Read its words before picking.",
		Actions:  []api.Action{answer("y", "Yes", "1", "yes"), answer("n", "No", "2", "no"), explainKey},
	}
}

type harness struct {
	src    *api.FakeSource
	hub    *api.Hub
	chat   *fakeChat
	acts   *fakeActions
	client *Client
	opened []string
	mu     sync.Mutex
}

// fakeActions records what POST /actions gets.
type fakeActions struct {
	mu   sync.Mutex
	reqs []rules.Request
}

func (f *fakeActions) record(req rules.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
}

func (f *fakeActions) got() []rules.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rules.Request(nil), f.reqs...)
}

func (f *fakeActions) Do(_ context.Context, req rules.Request) (rules.Outcome, error) {
	f.record(req)
	return rules.Outcome{}, rules.ErrRefused
}

func (f *fakeActions) DoAsUser(_ context.Context, req rules.Request) (rules.Outcome, error) {
	f.record(req)
	if req.Action == "regenerate_summary" {
		return rules.Outcome{Detail: "Wrote a new summary of " + req.Repo + "."}, nil
	}
	return rules.Outcome{Detail: "Answered with " + req.Args["choice"] + "."}, nil
}

func (f *fakeActions) Undo(context.Context, string) (rules.Outcome, error) {
	return rules.Outcome{}, rules.ErrUnknown
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
	return newHarnessWith(t, src)
}

func newHarnessWith(t *testing.T, src *api.FakeSource) *harness {
	t.Helper()
	hub, chat, acts := api.NewHub(), &fakeChat{}, &fakeActions{}
	srv := httptest.NewServer(api.Handler(src, api.Options{Token: token, Port: 7337, KeepAlive: time.Hour, Hub: hub, Chat: chat, Actions: acts}))
	t.Cleanup(srv.Close)
	return &harness{src: src, hub: hub, chat: chat, acts: acts, client: NewClient(srv.URL, token, nil)}
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
	{"4", "summaries", []string{"walrus", "badger", "g regenerate"}, "sum:lasso counts"},
}

func TestTabsGolden(t *testing.T) {
	for _, size := range [][2]int{{200, 50}, {100, 30}} {
		for _, tab := range tabWaits {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], tab.name), func(t *testing.T) {
				h := newHarness(t)
				tm := start(t, h.model(t), size[0], size[1])
				waitFor(t, tm, "live", "synced 4m ago")
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
		{"4", "sum:saddle", "sum:saddle counts", ""},
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
	waitFor(t, tm, "live", "synced 4m ago", "Due dates from Projects", sinceYouLeft)
	keys(tm, "enter")
	waitFor(t, tm, "zebra")
	m := finish(t, tm)
	golden.RequireEqual(t, []byte(m.View()))
}

// Below 90 columns one pane shows at a time.
func TestOnePaneAtATime(t *testing.T) {
	h := newHarness(t)
	tm := start(t, h.model(t), 80, 24)
	waitFor(t, tm, "live", "synced 4m ago", "Due dates from Projects")
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
		Title: "Flaky poller test, again", Right: "no due date", Updated: now.Add(-72 * time.Hour)}})
	h.src.Set(api.Focus{Item: api.Item{ID: "run:saddle:552", Tab: api.TabActions, Repo: "brandonapol/saddle",
		Title: "release · v0.4.1", Right: "running", Updated: now.Add(-2 * time.Hour)}})
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

func TestMarkdown(t *testing.T) {
	md := "# Title\n**Difficulty: hard (design judgment).** Run `make check`, see [the ADR](https://x/adr) and *why*.\n\n" +
		"- one\n  - nested\n- [ ] todo\n- [x] done\n1. first\n> quoted\n```\ncode **stays**\n```\nsnake_case_name stays\n<!-- hidden -->\n---"
	got := renderMarkdown(md, 200)
	for _, want := range []string{
		"Title", "Difficulty: hard (design judgment). Run make check, see the ADR (https://x/adr) and why.",
		"• one", "  • nested", "☐ todo", "☑ done", "1. first", "│ quoted", "  code **stays**", "snake_case_name stays", "────",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered markdown lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"**Difficulty", "`make", "# Title", "hidden", "```", "*why*"} {
		if strings.Contains(got, bad) {
			t.Errorf("rendered markdown keeps %q:\n%s", bad, got)
		}
	}
	// Long list items wrap under their text, not under the bullet.
	if got := renderMarkdown("- "+strings.Repeat("word ", 12), 20); !strings.Contains(got, "\n  word") {
		t.Errorf("list item doesn't hang:\n%s", got)
	}
}

// Nothing is marked on first load or by what the first GitHub sync brings
// in; only changes after it are.
func TestNoMarkersOnFirstLoad(t *testing.T) {
	src := api.NewFakeSource()
	seed(src)
	src.SetStatus(api.Status{}) // the daemon hasn't read GitHub yet
	h := newHarnessWith(t, src)
	tm := start(t, h.model(t), 200, 50)
	waitFor(t, tm, "live", "zebra", "github not read yet")
	// The first sync: rows arrive, then the status says GitHub was read.
	h.src.Set(api.Focus{Item: api.Item{ID: "lasso#40", Tab: api.TabIssues, Repo: "brandonapol/lasso",
		Title: "Brought in by the first sync", Right: "no due date", Updated: now}})
	synced := now
	h.src.SetStatus(api.Status{GitHubSynced: &synced})
	waitFor(t, tm, "Brought in by the first sync", "github synced just now")
	h.src.Set(api.Focus{Item: api.Item{ID: "lasso#41", Tab: api.TabIssues, Repo: "brandonapol/lasso",
		Title: "Filed after the sync", Right: "no due date", Updated: now}})
	waitFor(t, tm, "• Filed after the sync")
	m := finish(t, tm)
	if m.changed["lasso#40"] || !m.changed["lasso#41"] {
		t.Fatalf("changed = %v", m.changed)
	}
}

func TestHeaderShowsBotsAndSync(t *testing.T) {
	h := newHarness(t)
	tm := start(t, h.model(t), 200, 50)
	waitFor(t, tm, "live", "github synced 4m ago")
	m := finish(t, tm)
	head := m.viewHeader()
	for _, want := range []string{"● claude 2", "◉ grok 1", "○ saddle", "github synced 4m ago", "● live"} {
		if !strings.Contains(head, want) {
			t.Fatalf("header lacks %q:\n%s", want, head)
		}
	}
}

// g sends regenerate_summary with the repo, as the focus's action says.
func TestRegenerateSendsRepo(t *testing.T) {
	h := newHarness(t)
	tm := start(t, h.model(t), 200, 50)
	waitFor(t, tm, "live", "zebra")
	keys(tm, "4")
	waitFor(t, tm, "walrus", "g regenerate")
	keys(tm, "g")
	waitFor(t, tm, "Wrote a new summary of brandonapol/lasso.")
	finish(t, tm)
	got := h.acts.got()
	if len(got) != 1 || got[0].Action != "regenerate_summary" || got[0].Repo != "brandonapol/lasso" || got[0].Bot != "" {
		t.Fatalf("requests = %+v", got)
	}
}

// Answering an ask sends answer_prompt with args.choice, and only from the
// focus pane.
func TestAnswerSendsChoice(t *testing.T) {
	h := newHarness(t)
	h.src.Set(ask())
	tm := start(t, h.model(t), 200, 50)
	waitFor(t, tm, "live", "zebra")
	keys(tm, "3")
	waitFor(t, tm, "heron2", "ANSWER")
	keys(tm, "y")
	waitFor(t, tm, "press enter to open the question")
	if got := h.acts.got(); len(got) != 0 {
		t.Fatalf("answered from the list: %+v", got)
	}
	keys(tm, "enter", "y")
	waitFor(t, tm, "Answered with yes.")
	finish(t, tm)
	got := h.acts.got()
	if len(got) != 1 {
		t.Fatalf("requests = %+v", got)
	}
	r := got[0]
	if r.Action != "answer_prompt" || r.Bot != "grok-scout" || r.Target != "p7" || r.Args["choice"] != "yes" || r.Args["option"] != "1" {
		t.Fatalf("request = %+v", r)
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
