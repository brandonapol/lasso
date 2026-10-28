package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
)

// Geometry.
const (
	leftWidth   = 38
	rightWidth  = 46
	wideWidth   = 140 // below this the focus pane overlays the middle
	narrowWidth = 90  // below this one pane shows at a time
	bodyTop     = 1   // the header line is above the panes
	listTop     = 4   // tabs, underline, hint, blank
	tabGap      = 3
)

// Palette: quiet, on the terminal's own dark background.
var (
	orange = lipgloss.Color("#f0a24a")
	dimC   = lipgloss.Color("#9097a1")
	blue   = lipgloss.Color("#79a8f0")
	ruleC  = lipgloss.Color("#3a3f47")
	selBg  = lipgloss.Color("#1f242c")

	dim    = lipgloss.NewStyle().Foreground(dimC)
	bold   = lipgloss.NewStyle().Bold(true)
	accent = lipgloss.NewStyle().Foreground(orange)
	rule   = lipgloss.NewStyle().Foreground(ruleC)
	dot    = lipgloss.NewStyle().Foreground(blue)
)

type layout struct {
	x, w  [3]int
	shown [3]bool
}

// layout places the panes for the current width. Between narrowWidth and
// wideWidth the focus pane takes the middle's place while it is active;
// below narrowWidth only the active pane shows.
func (m *Model) layout() layout {
	var l layout
	switch {
	case m.w >= wideWidth:
		mid := m.w - leftWidth - rightWidth - 2
		l.x = [3]int{0, leftWidth + 1, leftWidth + 1 + mid + 1}
		l.w = [3]int{leftWidth, mid, rightWidth}
		l.shown = [3]bool{true, true, true}
	case m.w >= narrowWidth:
		mid := m.w - leftWidth - 1
		l.x = [3]int{0, leftWidth + 1, leftWidth + 1}
		l.w = [3]int{leftWidth, mid, mid}
		l.shown = [3]bool{true, m.pane != paneFocus, m.pane == paneFocus}
	default:
		l.w = [3]int{m.w, m.w, m.w}
		l.shown[m.pane] = true
	}
	return l
}

// paneAt is the pane drawn at column x, or -1 for a separator.
func (l layout) paneAt(x int) int {
	for p := range 3 {
		if l.shown[p] && x >= l.x[p] && x < l.x[p]+l.w[p] {
			return p
		}
	}
	return -1
}

func (m *Model) bodyHeight() int { return max(m.h-2, 1) }
func (m *Model) listHeight() int { return max(m.bodyHeight()-listTop, 1) }

// View draws the whole screen.
func (m *Model) View() string {
	if m.w == 0 || m.h == 0 {
		return ""
	}
	l := m.layout()
	h := m.bodyHeight()
	var cols []string
	sep := rule.Render(strings.TrimSuffix(strings.Repeat("│\n", h), "\n"))
	for p := range 3 {
		if !l.shown[p] {
			continue
		}
		if len(cols) > 0 {
			cols = append(cols, sep)
		}
		var s string
		switch p {
		case paneChat:
			s = m.viewChat(l.w[p], h)
		case paneList:
			s = m.viewList(l.w[p], h)
		case paneFocus:
			s = m.viewFocus(l.w[p], h)
		}
		cols = append(cols, box(s, l.w[p], h))
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, cols...)
	return m.viewHeader() + "\n" + body + "\n" + m.viewFooter()
}

// box clips or pads s to exactly w×h cells.
func box(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i, ln := range lines {
		lines[i] = fit(ln, w)
	}
	return strings.Join(lines, "\n")
}

// fit truncates or pads one line to w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "…")
	if n := lipgloss.Width(s); n < w {
		s += strings.Repeat(" ", w-n)
	}
	return s
}

func (m *Model) viewHeader() string {
	left := " " + accent.Render("◆") + " " + bold.Render("lasso")
	if len(m.opt.HotRepos) > 0 {
		left += "   " + dim.Render(strings.Join(m.opt.HotRepos, "  "))
	}
	right := dot.Render("●") + dim.Render(" live ")
	switch {
	case m.connected:
	case m.dropped:
		right = dim.Render("○ reconnecting… ")
	default:
		right = dim.Render("○ connecting… ")
	}
	return spread(left, right, m.w)
}

// spread puts left and right at either end of a w-cell line.
func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return fit(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m *Model) viewFooter() string {
	keys := []struct{ k, what string }{
		{"tab", "next pane"}, {"1–4", "tabs"}, {"↑↓ enter", "focus"},
		{"o", "open on GitHub"}, {"/", "search"}, {"r", "rules"},
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = bold.Render(k.k) + " " + dim.Render(k.what)
	}
	left := " " + strings.Join(parts, dim.Render(" · "))
	right := ""
	if m.status != "" {
		right = dim.Render(m.status + " ")
	}
	if lipgloss.Width(left)+lipgloss.Width(right)+1 > m.w {
		if right != "" {
			return fit(" "+right, m.w)
		}
		return fit(left, m.w)
	}
	return spread(left, right, m.w)
}

func paneTitle(name string, active bool) string {
	if active {
		return " " + accent.Render(name)
	}
	return " " + dim.Render(name)
}

func (m *Model) viewChat(w, h int) string {
	head := []string{paneTitle("assistant", m.pane == paneChat), ""}
	cw := max(w-2, 1)
	var body []string
	add := func(s string) {
		for _, ln := range strings.Split(s, "\n") {
			body = append(body, " "+ln)
		}
	}
	for i, msg := range m.chat {
		if i > 0 {
			body = append(body, "")
		}
		switch {
		case msg.Role == api.RoleUser:
			add(dim.Width(cw).Render("› " + msg.Text))
		case msg.Kind == "line_jumper":
			add(accent.Width(cw).Render("! " + msg.Text))
		default:
			add(lipgloss.NewStyle().Width(cw).Render(msg.Text))
		}
	}
	if m.pending != "" {
		if len(body) > 0 {
			body = append(body, "")
		}
		add(dim.Width(cw).Render("› " + m.pending))
		body = append(body, "")
		add(dim.Render("thinking…"))
	}
	if m.chatErr != "" {
		if len(body) > 0 {
			body = append(body, "")
		}
		add(dim.Width(cw).Render("couldn't reach the assistant: " + m.chatErr))
	}
	if len(body) == 0 {
		add(dim.Width(cw).Render("Ask about anything on the tabs. I only speak up on my own when something can't wait."))
	}
	// Newest at the bottom, just above the input.
	room := max(h-2-len(head), 0)
	if len(body) > room {
		body = body[len(body)-room:]
	}
	lines := append(head, body...)
	for len(lines) < h-2 {
		lines = append(lines, "")
	}
	lines = append(lines[:h-2], rule.Render(strings.Repeat("─", w)), " "+m.input.View())
	return strings.Join(lines, "\n")
}

var tabHints = []string{
	"open issues in your hot repos",
	"workflow runs in your hot repos",
	"what your bots are working on now",
	"the last 24 hours in each hot repo",
}

// tabAt is the tab whose label covers column x of the middle pane.
func tabAt(x int) int {
	pos := 1
	for i, n := range tabNames {
		w := lipgloss.Width(fmt.Sprintf("%d %s", i+1, n))
		if x >= pos && x < pos+w {
			return i
		}
		pos += w + tabGap
	}
	return -1
}

func (m *Model) viewList(w, h int) string {
	var tabs, under strings.Builder
	tabs.WriteString(" ")
	under.WriteString(rule.Render("─"))
	for i, n := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, n)
		lw := lipgloss.Width(label)
		if i == m.tab {
			tabs.WriteString(bold.Render(label))
			under.WriteString(accent.Render(strings.Repeat("━", lw)))
		} else {
			tabs.WriteString(dim.Render(label))
			under.WriteString(rule.Render(strings.Repeat("─", lw)))
		}
		if i < len(tabNames)-1 {
			tabs.WriteString(strings.Repeat(" ", tabGap))
			under.WriteString(rule.Render(strings.Repeat("─", tabGap)))
		}
	}
	if n := w - lipgloss.Width(under.String()); n > 0 {
		under.WriteString(rule.Render(strings.Repeat("─", n)))
	}
	hint := " " + dim.Render(tabHints[m.tab])
	switch {
	case m.searching || m.search.Value() != "":
		hint = " " + m.search.View()
	case m.tab == tabSummaries:
		hint += dim.Render(" · ") + bold.Render("g") + dim.Render(" regenerate")
	}
	lines := []string{tabs.String(), under.String(), hint, ""}

	if m.overlay == "rules" {
		lines = append(lines,
			" "+bold.Render("rules"),
			"",
			" "+dim.Width(w-2).Render("Standing permissions will be listed here once the rules engine exists (#5). Until then nothing acts for you without asking."),
			"",
			" "+dim.Render("r or esc to close"))
		return strings.Join(lines, "\n")
	}

	v := m.visible(m.tab)
	switch {
	case m.errs[m.tab] != "" && !m.loaded[m.tab]:
		lines = append(lines, " "+dim.Render("couldn't load this tab: "+m.errs[m.tab]))
	case !m.loaded[m.tab]:
		lines = append(lines, " "+dim.Render("loading…"))
	case len(v) == 0 && m.search.Value() != "":
		lines = append(lines, " "+dim.Render("nothing matches"))
	case len(v) == 0:
		lines = append(lines, " "+dim.Render("nothing here right now"))
	}
	avail := h - listTop
	for i := m.off[m.tab]; i < len(v) && avail > 0; i++ {
		var r string
		if m.tab == tabSummaries {
			r = m.card(v[i], i == m.sel[m.tab], w)
		} else {
			r = m.row(v[i], i == m.sel[m.tab], w)
		}
		lines = append(lines, r, "")
		avail -= m.rowHeight(m.tab, v[i])
	}
	return strings.Join(lines, "\n")
}

// rowHeight is how many lines one row takes, its trailing blank included.
func (m *Model) rowHeight(tab int, it api.Item) int {
	if tab == tabSummaries {
		return lipgloss.Height(m.card(it, false, m.layout().w[paneList])) + 1
	}
	return 3
}

func (m *Model) row(it api.Item, sel bool, w int) string {
	st := func(s lipgloss.Style) lipgloss.Style {
		if sel {
			return s.Background(selBg)
		}
		return s
	}
	bar := st(lipgloss.NewStyle()).Render("  ")
	if sel {
		bar = st(accent).Render("▌") + st(lipgloss.NewStyle()).Render(" ")
	}
	inner := w - 3
	mark := ""
	if m.changed[it.ID] {
		mark = st(dot).Render("• ")
	}
	status := st(dim).Render(it.Status)
	tw := inner - lipgloss.Width(status) - lipgloss.Width(mark) - 1
	title := mark + st(bold).Render(ansi.Truncate(it.Title, max(tw, 1), "…"))
	gap := inner - lipgloss.Width(title) - lipgloss.Width(status)
	line1 := bar + title + st(lipgloss.NewStyle()).Render(strings.Repeat(" ", max(gap, 1))) + status + st(lipgloss.NewStyle()).Render(" ")
	sub := ansi.Truncate(m.subtitle(it), inner, "…")
	line2 := bar + st(dim).Render(sub) + st(lipgloss.NewStyle()).Render(strings.Repeat(" ", max(inner-lipgloss.Width(sub)+1, 0)))
	return line1 + "\n" + line2
}

func (m *Model) subtitle(it api.Item) string {
	parts := []string{}
	if it.Repo != "" {
		parts = append(parts, it.Repo)
	}
	if !it.Updated.IsZero() {
		parts = append(parts, "updated "+ago(m.opt.Now(), it.Updated))
	}
	return strings.Join(parts, " · ")
}

func (m *Model) card(it api.Item, sel bool, w int) string {
	border := ruleC
	if sel {
		border = orange
	}
	cw := max(w-6, 10) // margin, border and padding
	name := it.Repo
	if name == "" {
		name = it.Title
	}
	regen := dim.Render("g regenerate")
	head := bold.Render(ansi.Truncate(name, max(cw-lipgloss.Width(regen)-1, 1), "…"))
	head += strings.Repeat(" ", max(cw-lipgloss.Width(head)-lipgloss.Width(regen), 1)) + regen
	text := dim.Render("loading…")
	if b, ok := m.bodies[it.ID]; ok {
		text = b.Body
		if strings.TrimSpace(text) == "" {
			text = it.Title
		}
		text = lipgloss.NewStyle().Width(cw).Render(text)
	}
	foot := "last 24h · updated " + it.Updated.In(m.opt.Now().Location()).Format("15:04")
	if it.Status != "" {
		foot = "last 24h · " + it.Status + " at " + it.Updated.In(m.opt.Now().Location()).Format("15:04")
	}
	body := head + "\n" + text + "\n\n" + dim.Render(ansi.Truncate(foot, cw, "…"))
	card := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(cw + 2).Render(body)
	lines := strings.Split(card, "\n")
	for i := range lines {
		lines[i] = " " + lines[i]
	}
	return strings.Join(lines, "\n")
}

func (m *Model) viewFocus(w, h int) string {
	lines := []string{paneTitle("focus", m.pane == paneFocus), ""}
	cw := w - 2
	add := func(s string) {
		for _, ln := range strings.Split(s, "\n") {
			lines = append(lines, " "+ln)
		}
	}
	it := m.selected(m.tab)
	if it == nil {
		add(dim.Render("nothing selected"))
		return strings.Join(lines, "\n")
	}
	add(bold.Width(cw).Render(it.Title))
	meta := []string{}
	for _, s := range []string{it.Repo, it.Status} {
		if s != "" {
			meta = append(meta, s)
		}
	}
	if !it.Updated.IsZero() {
		meta = append(meta, "updated "+ago(m.opt.Now(), it.Updated))
	}
	add(dim.Width(cw).Render(strings.Join(meta, " · ")))
	lines = append(lines, "")

	var body string
	loaded := m.focus != nil && m.focus.ID == it.ID
	if loaded {
		body = m.focus.Body
	}
	if m.overlay == "explain" {
		add(dim.Render("EXPLAIN"))
		terms := attention.Unexplained(it.Title + "\n" + it.Status + "\n" + body)
		if len(terms) == 0 {
			add(dim.Width(cw).Render("Nothing here is bot jargon."))
		}
		for _, t := range terms {
			add(bold.Render(t) + dim.Render(" means"))
			add(lipgloss.NewStyle().Width(cw - 2).Render("  " + attention.Plain(t)))
		}
		lines = append(lines, "")
		add(dim.Render("? or esc to close"))
		return strings.Join(lines, "\n")
	}
	if !loaded {
		add(dim.Render("loading…"))
	} else {
		secs, take := parseBody(body)
		for _, s := range secs {
			if s.label != "" {
				add(dim.Render(strings.ToUpper(s.label)))
			}
			if s.text != "" {
				add(lipgloss.NewStyle().Width(cw).Render(s.text))
			}
			lines = append(lines, "")
		}
		if take != "" {
			add(lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(orange).
				Foreground(orange).Padding(0, 1).Width(cw - 2).Render("lasso · " + take))
		}
	}

	actions := []string{bold.Render("o") + dim.Render(" open on GitHub"), bold.Render("?") + dim.Render(" explain")}
	if m.tab == tabSummaries {
		actions = append(actions, bold.Render("g")+dim.Render(" regenerate"))
	}
	foot := " " + strings.Join(actions, "   ")
	if len(lines) > h-2 {
		lines = lines[:h-2]
	}
	for len(lines) < h-2 {
		lines = append(lines, "")
	}
	return strings.Join(append(lines, "", foot), "\n")
}

type section struct{ label, text string }

// parseBody splits a focus body into labelled sections ("## Label" lines
// start one) and lasso's one-line take (a line starting "lasso:").
func parseBody(body string) ([]section, string) {
	var secs []section
	var take string
	cur := section{}
	flush := func() {
		cur.text = strings.TrimSpace(cur.text)
		if cur.label != "" || cur.text != "" {
			secs = append(secs, cur)
		}
	}
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(t, "## "):
			flush()
			cur = section{label: strings.TrimSpace(t[3:])}
		case strings.HasPrefix(strings.ToLower(t), "lasso:"):
			take = strings.TrimSpace(t[len("lasso:"):])
		default:
			cur.text += ln + "\n"
		}
	}
	flush()
	return secs, take
}

// ago says how long before now t was, in the fewest words.
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
