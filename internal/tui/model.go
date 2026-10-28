// Package tui is lasso's terminal client: three panes (the assistant, the
// tabbed middle, and the focus pane) over the daemon's API. It keeps no state
// of its own beyond what is selected and what is being typed; every row and
// every focus body comes from the daemon (ADR 0001 §1).
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/rules"
)

// Options configures the TUI.
type Options struct {
	// Client reaches the daemon. Required.
	Client *Client
	// Context ends the event stream when cancelled (default Background).
	Context context.Context
	// HotRepos are shown in the header line.
	HotRepos []string
	// Open opens a URL in the browser (default: xdg-open, or open on macOS).
	Open func(url string) error
	// Now is the clock used for "3h ago" (default time.Now).
	Now func() time.Time
	// Reconnect is how long to wait before reopening a dropped event stream
	// (default 3s).
	Reconnect time.Duration
	// Tick is how often the screen is redrawn so ages ("synced 4m ago")
	// move with no event (default 30s).
	Tick time.Duration
}

// Run shows the TUI until the user quits.
func Run(opts Options) error {
	m := New(opts)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(m.ctx))
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && m.ctx.Err() != nil {
		return nil
	}
	return err
}

// Panes, in tab order.
const (
	paneChat = iota
	paneList
	paneFocus
)

var tabNames = []string{"Issues", "Actions", "In progress", "Summaries"}

const tabSummaries = 3

// Model is the Bubble Tea model. Use New.
type Model struct {
	opt Options
	ctx context.Context

	w, h int
	pane int
	tab  int
	sel  [4]int
	off  [4]int

	// Views of the daemon's data, replaced wholesale on every fetch.
	items   [4][]api.Item
	loaded  [4]bool
	errs    [4]string
	focus   *api.Focus
	focusID string
	bodies  map[string]api.Focus
	// changed marks rows that changed since the user last looked. Nothing
	// is marked until the daemon has read GitHub once (status) and the
	// row's tab has loaded, so the first load marks nothing.
	changed map[string]bool
	// daemon is the header's status, as /status last said.
	daemon *api.Status

	input     textinput.Model
	search    textinput.Model
	searching bool
	overlay   string // "", "rules" or "explain"
	status    string
	connected bool
	dropped   bool

	// The assistant's conversation, as the daemon keeps it.
	chat    []api.ChatMessage
	chatErr string
	// pending is the message being answered, shown until the reply lands.
	pending string
	// pendingFocus is an item the assistant asked to show that wasn't
	// loaded yet; it is selected when its tab's rows arrive.
	pendingFocus string

	events chan tea.Msg
}

// New returns a model ready for tea.NewProgram.
func New(opts Options) *Model {
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	if opts.Open == nil {
		opts.Open = openURL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Reconnect <= 0 {
		opts.Reconnect = 3 * time.Second
	}
	if opts.Tick <= 0 {
		opts.Tick = 30 * time.Second
	}
	in := textinput.New()
	in.Prompt = "› "
	in.Placeholder = "ask lasso"
	_ = in.Cursor.SetMode(cursor.CursorStatic)
	se := textinput.New()
	se.Prompt = "/ "
	_ = se.Cursor.SetMode(cursor.CursorStatic)
	return &Model{
		opt:     opts,
		ctx:     opts.Context,
		pane:    paneList,
		bodies:  map[string]api.Focus{},
		changed: map[string]bool{},
		input:   in,
		search:  se,
		events:  make(chan tea.Msg, 16),
	}
}

// Messages.
type (
	itemsMsg struct {
		tab   int
		items []api.Item
		err   error
	}
	focusMsg struct {
		id  string
		f   api.Focus
		err error
	}
	chatMsg struct {
		msgs []api.ChatMessage
		err  error
		// reply is true for the answer to the user's message.
		reply bool
	}
	eventMsg     struct{ ev Event }
	connectedMsg struct{}
	droppedMsg   struct{ err error }
	reconnectMsg struct{}
	statusMsg    string
	daemonStatus struct {
		st  api.Status
		err error
	}
	tickMsg struct{}
)

// Init fetches every tab and opens the event stream.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.connect(), m.waitEvent(), m.openChat(), m.fetchStatus(), m.tick()}
	for i := range api.Tabs {
		cmds = append(cmds, m.fetchItems(i))
	}
	return tea.Batch(cmds...)
}

func (m *Model) fetchItems(tab int) tea.Cmd {
	c, ctx := m.opt.Client, m.ctx
	return func() tea.Msg {
		items, err := c.Items(ctx, api.Tabs[tab])
		return itemsMsg{tab: tab, items: items, err: err}
	}
}

func (m *Model) fetchStatus() tea.Cmd {
	c, ctx := m.opt.Client, m.ctx
	return func() tea.Msg {
		st, err := c.Status(ctx)
		return daemonStatus{st: st, err: err}
	}
}

func (m *Model) tick() tea.Cmd {
	return tea.Tick(m.opt.Tick, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *Model) fetchFocus(id string) tea.Cmd {
	c, ctx := m.opt.Client, m.ctx
	return func() tea.Msg {
		f, err := c.Focus(ctx, id)
		return focusMsg{id: id, f: f, err: err}
	}
}

// openChat tells the assistant the TUI opened and reads the conversation.
func (m *Model) openChat() tea.Cmd {
	c, ctx := m.opt.Client, m.ctx
	return func() tea.Msg {
		msgs, err := c.OpenChat(ctx)
		return chatMsg{msgs: msgs, err: err}
	}
}

func (m *Model) fetchChat() tea.Cmd {
	c, ctx := m.opt.Client, m.ctx
	return func() tea.Msg {
		msgs, err := c.Chat(ctx)
		return chatMsg{msgs: msgs, err: err}
	}
}

func (m *Model) sendChat(text string) tea.Cmd {
	c, ctx := m.opt.Client, m.ctx
	return func() tea.Msg {
		msgs, err := c.SendChat(ctx, text)
		return chatMsg{msgs: msgs, err: err, reply: true}
	}
}

func (m *Model) gotChat(msg chatMsg) {
	if msg.reply {
		m.pending = ""
	}
	if msg.err != nil {
		m.chatErr = msg.err.Error()
		return
	}
	m.chat, m.chatErr = msg.msgs, ""
}

// connect starts one event-stream reader. It feeds m.events and ends with a
// droppedMsg.
func (m *Model) connect() tea.Cmd {
	c, ctx, ch := m.opt.Client, m.ctx, m.events
	send := func(msg tea.Msg) {
		select {
		case ch <- msg:
		case <-ctx.Done():
		}
	}
	return func() tea.Msg {
		go func() {
			err := c.Events(ctx, func() { send(connectedMsg{}) }, func(ev Event) { send(eventMsg{ev}) })
			send(droppedMsg{err})
		}()
		return nil
	}
}

func (m *Model) waitEvent() tea.Cmd {
	ctx, ch := m.ctx, m.events
	return func() tea.Msg {
		select {
		case msg := <-ch:
			return msg
		case <-ctx.Done():
			return nil
		}
	}
}

// Update handles one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.input.Width = max(leftWidth-6, 1)
		for t := range m.items {
			m.clamp(t)
		}
		return m, nil
	case itemsMsg:
		return m, m.gotItems(msg)
	case focusMsg:
		if msg.err != nil {
			if msg.id == m.focusID {
				m.status = "couldn't load " + msg.id + ": " + msg.err.Error()
			}
			return m, nil
		}
		m.bodies[msg.id] = msg.f
		if msg.id == m.focusID {
			f := msg.f
			m.focus = &f
		}
		return m, nil
	case connectedMsg:
		m.connected = true
		cmds := []tea.Cmd{m.waitEvent()}
		if m.dropped {
			// Catch up on whatever changed while the stream was down.
			m.dropped = false
			cmds = append(cmds, m.fetchStatus())
			for t := range api.Tabs {
				cmds = append(cmds, m.fetchItems(t))
			}
		}
		return m, tea.Batch(cmds...)
	case droppedMsg:
		m.connected, m.dropped = false, true
		wait := m.opt.Reconnect
		return m, tea.Batch(m.waitEvent(), tea.Tick(wait, func(time.Time) tea.Msg { return reconnectMsg{} }))
	case reconnectMsg:
		if m.ctx.Err() != nil {
			return m, nil
		}
		return m, m.connect()
	case chatMsg:
		m.gotChat(msg)
		return m, nil
	case eventMsg:
		return m, tea.Batch(m.gotEvent(msg.ev), m.waitEvent())
	case statusMsg:
		m.status = string(msg)
		return m, nil
	case daemonStatus:
		// A daemon without /status (an older one) just shows no dots.
		if msg.err == nil {
			m.setStatus(msg.st)
		}
		return m, nil
	case tickMsg:
		if m.ctx.Err() != nil {
			return m, nil
		}
		return m, m.tick()
	case tea.MouseMsg:
		return m, m.mouse(msg)
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, nil
}

func (m *Model) gotItems(msg itemsMsg) tea.Cmd {
	if msg.err != nil {
		m.errs[msg.tab] = msg.err.Error()
		return nil
	}
	prev := m.selected(msg.tab)
	m.items[msg.tab], m.loaded[msg.tab], m.errs[msg.tab] = msg.items, true, ""
	// Keep the selection on the same item when rows move.
	if prev != nil {
		for i, it := range m.visible(msg.tab) {
			if it.ID == prev.ID {
				m.sel[msg.tab] = i
			}
		}
	}
	m.clamp(msg.tab)
	if m.pendingFocus != "" && msg.tab == m.tab && m.selectID(m.pendingFocus) {
		m.pendingFocus = ""
	}
	var cmds []tea.Cmd
	if msg.tab == m.tab {
		cmds = append(cmds, m.refocus(false))
	}
	return tea.Batch(cmds...)
}

func (m *Model) gotEvent(ev Event) tea.Cmd {
	switch ev.Kind {
	case api.EventItemChanged:
		var it api.Item
		if json.Unmarshal(ev.Data, &it) != nil {
			return nil
		}
		t := tabIndex(it.Tab)
		if t < 0 {
			return nil
		}
		if m.marks(t) {
			if sel := m.selected(m.tab); sel == nil || sel.ID != it.ID {
				m.changed[it.ID] = true
			}
		}
		cmds := []tea.Cmd{m.fetchItems(t)}
		if it.ID == m.focusID {
			cmds = append(cmds, m.fetchFocus(it.ID))
		}
		return tea.Batch(cmds...)
	case api.EventItemRemoved:
		var d struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		delete(m.changed, d.ID)
		delete(m.bodies, d.ID)
		var cmds []tea.Cmd
		for t := range api.Tabs {
			for _, it := range m.items[t] {
				if it.ID == d.ID {
					cmds = append(cmds, m.fetchItems(t))
					break
				}
			}
		}
		return tea.Batch(cmds...)
	case api.EventChatUpdated:
		return m.fetchChat()
	case api.EventStatusChanged:
		var st api.Status
		if json.Unmarshal(ev.Data, &st) == nil {
			m.setStatus(st)
		}
		return nil
	case api.EventFocus:
		var d struct {
			ID  string  `json:"id"`
			Tab api.Tab `json:"tab"`
		}
		if json.Unmarshal(ev.Data, &d) != nil {
			return nil
		}
		return m.focusOn(d.ID, d.Tab)
	}
	// brief.updated needs nothing here: the assistant writes "since you
	// left" when the TUI opens.
	return nil
}

// setStatus takes a new status from the daemon.
func (m *Model) setStatus(st api.Status) {
	m.daemon = &st
}

// marks reports whether a change to a row on tab t gets a marker: only
// once the tab has loaded and the daemon has read GitHub at least once
// (or has no hot repos to read), so what the first load and the first
// sync bring in counts as already seen.
func (m *Model) marks(t int) bool {
	if !m.loaded[t] || m.daemon == nil {
		return false
	}
	return m.daemon.GitHubSynced != nil || len(m.opt.HotRepos) == 0
}

// focusOn moves the middle and right panes to one item, as the assistant
// asked. The pane the user is typing in keeps the keyboard.
func (m *Model) focusOn(id string, tab api.Tab) tea.Cmd {
	t := tabIndex(tab)
	if t < 0 || id == "" {
		return nil
	}
	m.overlay = ""
	if m.searching || m.search.Value() != "" {
		m.searching = false
		m.search.Blur()
		m.search.SetValue("")
	}
	m.tab = t
	if !m.selectID(id) {
		// Not loaded yet: select it when its rows arrive.
		m.pendingFocus = id
		return m.fetchItems(t)
	}
	m.pendingFocus = ""
	return m.refocus(false)
}

// selectID selects id on the current tab, if it is there.
func (m *Model) selectID(id string) bool {
	for i, it := range m.visible(m.tab) {
		if it.ID == id {
			m.sel[m.tab] = i
			m.clamp(m.tab)
			return true
		}
	}
	return false
}

func tabIndex(t api.Tab) int {
	for i, x := range api.Tabs {
		if x == t {
			return i
		}
	}
	return -1
}

// visible is the tab's rows after the search filter.
func (m *Model) visible(tab int) []api.Item {
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	if q == "" || tab != m.tab {
		return m.items[tab]
	}
	var out []api.Item
	for _, it := range m.items[tab] {
		if strings.Contains(strings.ToLower(it.Title+" "+it.Repo+" "+it.Right), q) {
			out = append(out, it)
		}
	}
	return out
}

func (m *Model) selected(tab int) *api.Item {
	v := m.visible(tab)
	if i := m.sel[tab]; i >= 0 && i < len(v) {
		return &v[i]
	}
	return nil
}

func (m *Model) clamp(tab int) {
	n := len(m.visible(tab))
	m.sel[tab] = min(max(m.sel[tab], 0), max(n-1, 0))
	m.off[tab] = min(m.off[tab], m.sel[tab])
	if m.h == 0 {
		return
	}
	avail := m.listHeight()
	for m.off[tab] < m.sel[tab] && m.span(tab, m.off[tab], m.sel[tab]) > avail {
		m.off[tab]++
	}
}

// span is the height of rows from..to inclusive.
func (m *Model) span(tab, from, to int) int {
	v := m.visible(tab)
	h := 0
	for i := from; i <= to && i < len(v); i++ {
		h += m.rowHeight(tab, v[i])
	}
	return h
}

// refocus points the focus pane at the current selection, fetching its
// body. force refetches even if it is already showing.
func (m *Model) refocus(force bool) tea.Cmd {
	it := m.selected(m.tab)
	if it == nil {
		m.focusID, m.focus = "", nil
		return nil
	}
	delete(m.changed, it.ID)
	if it.ID == m.focusID && !force {
		return nil
	}
	m.focusID = it.ID
	if b, ok := m.bodies[it.ID]; ok && b.Updated.Equal(it.Updated) {
		m.focus = &b
		return nil
	}
	return m.fetchFocus(it.ID)
}

func (m *Model) move(d int) tea.Cmd {
	m.sel[m.tab] += d
	m.clamp(m.tab)
	return m.refocus(false)
}

func (m *Model) setTab(t int) tea.Cmd {
	if t == m.tab {
		return nil
	}
	m.tab = t
	m.overlay = ""
	m.searching = false
	m.search.Blur()
	m.search.SetValue("")
	if m.pane == paneChat && m.w < narrowWidth {
		m.pane = paneList
	}
	m.clamp(t)
	return m.refocus(false)
}

func (m *Model) setPane(p int) {
	m.pane = p
	if p == paneChat {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
}

func (m *Model) key(msg tea.KeyMsg) tea.Cmd {
	k := msg.String()
	if k == "ctrl+c" {
		return tea.Quit
	}
	if m.searching {
		switch k {
		case "esc":
			m.searching = false
			m.search.Blur()
			m.search.SetValue("")
			m.clamp(m.tab)
			return m.refocus(false)
		case "enter":
			m.searching = false
			m.search.Blur()
			return nil
		case "up", "down":
		default:
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			m.sel[m.tab] = 0
			m.clamp(m.tab)
			return tea.Batch(cmd, m.refocus(false))
		}
	}
	if m.pane == paneChat {
		switch k {
		case "tab", "shift+tab", "esc":
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return nil
			}
			if m.pending != "" {
				m.status = "still answering your last message"
				return nil
			}
			m.pending = text
			m.input.SetValue("")
			return m.sendChat(text)
		default:
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return cmd
		}
	}
	switch k {
	case "q":
		return tea.Quit
	case "tab":
		m.setPane((m.pane + 1) % 3)
	case "shift+tab":
		m.setPane((m.pane + 2) % 3)
	case "esc":
		switch {
		case m.overlay != "":
			m.overlay = ""
		case m.pane != paneList:
			m.setPane(paneList)
		default:
			m.status = ""
		}
	case "1", "2", "3", "4":
		return m.setTab(int(k[0] - '1'))
	case "up", "k":
		return m.move(-1)
	case "down", "j":
		return m.move(1)
	case "enter":
		m.overlay = ""
		m.setPane(paneFocus)
		return m.refocus(false)
	case "/":
		m.overlay = ""
		m.setPane(paneList)
		m.searching = true
		return m.search.Focus()
	case "r":
		m.overlay = toggle(m.overlay, "rules")
	default:
		if a := m.action(k); a != nil {
			return m.do(*a)
		}
	}
	return nil
}

// shownFocus is the focus of the selected row, once loaded.
func (m *Model) shownFocus() *api.Focus {
	it := m.selected(m.tab)
	if it == nil || m.focus == nil || m.focus.ID != it.ID {
		return nil
	}
	return m.focus
}

// action is the selected item's action on key k, if it has one.
func (m *Model) action(k string) *api.Action {
	f := m.shownFocus()
	if f == nil {
		return nil
	}
	for i := range f.Actions {
		if f.Actions[i].Key == k {
			return &f.Actions[i]
		}
	}
	return nil
}

// do runs one of the focus pane's actions.
func (m *Model) do(a api.Action) tea.Cmd {
	switch a.Kind {
	case api.ActionOpen:
		return m.open()
	case api.ActionExplain:
		m.overlay = toggle(m.overlay, "explain")
		if m.overlay == "explain" && m.w < wideWidth {
			m.setPane(paneFocus)
		}
		return nil
	}
	if a.Request == nil {
		return nil
	}
	if a.Kind == string(rules.AnswerPrompt) && m.pane != paneFocus {
		// An answer goes to a bot at once; ask for the focus pane first so
		// a stray key in the list can't answer for the user.
		m.status = "press enter to open the question, then " + a.Key + " to answer"
		return nil
	}
	c, ctx, req, label := m.opt.Client, m.ctx, *a.Request, a.Label
	m.status = "sending: " + label + "…"
	return func() tea.Msg {
		out, err := c.Action(ctx, req)
		if err != nil {
			return statusMsg("couldn't " + strings.ToLower(label) + ": " + err.Error())
		}
		if d := first(out.Detail, out.Decision.Reason); d != "" {
			return statusMsg(d)
		}
		return statusMsg("done: " + label)
	}
}

func toggle(cur, want string) string {
	if cur == want {
		return ""
	}
	return want
}

func (m *Model) open() tea.Cmd {
	it := m.selected(m.tab)
	if it == nil || it.URL == "" {
		m.status = "nothing to open"
		return nil
	}
	open, url := m.opt.Open, it.URL
	return func() tea.Msg {
		if err := open(url); err != nil {
			return statusMsg("couldn't open " + url + ": " + err.Error())
		}
		return statusMsg("opened " + url)
	}
}

func (m *Model) mouse(msg tea.MouseMsg) tea.Cmd {
	l := m.layout()
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if l.paneAt(msg.X) == paneList {
			return m.move(-1)
		}
		return nil
	case tea.MouseButtonWheelDown:
		if l.paneAt(msg.X) == paneList {
			return m.move(1)
		}
		return nil
	case tea.MouseButtonLeft:
	default:
		return nil
	}
	if msg.Action != tea.MouseActionPress {
		return nil
	}
	p := l.paneAt(msg.X)
	if p < 0 {
		return nil
	}
	m.setPane(p)
	if p != paneList || m.overlay == "rules" {
		return nil
	}
	x, y := msg.X-l.x[paneList], msg.Y-bodyTop
	if y == 0 {
		if t := tabAt(x); t >= 0 {
			return m.setTab(t)
		}
		return nil
	}
	y -= listTop
	if y < 0 {
		return nil
	}
	v := m.visible(m.tab)
	for i := m.off[m.tab]; i < len(v); i++ {
		h := m.rowHeight(m.tab, v[i])
		if y < h {
			m.sel[m.tab] = i
			m.clamp(m.tab)
			return m.refocus(false)
		}
		y -= h
	}
	return nil
}

// openURL opens url in the user's browser.
func openURL(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
