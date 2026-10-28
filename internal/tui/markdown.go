package tui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The focus pane draws the simple markdown GitHub issues and bots use, so
// "**Difficulty: hard**" reads as bold words and not as asterisks:
// headings, bold, italics, inline code, code blocks, lists, task lists,
// quotes and links. Anything fancier (tables, HTML) shows as text.

var (
	codeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#a8c99a"))
	quoteStyle = dim

	orderedRe = regexp.MustCompile(`^(\d{1,3})[.)]\s+(.*)$`)
	imageRe   = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	linkRe    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]*)[^)]*\)`)
	commentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// renderMarkdown draws md in w cells, wrapping each block.
func renderMarkdown(md string, w int) string {
	w = max(w, 4)
	md = strings.ReplaceAll(md, "\r\n", "\n")
	md = commentRe.ReplaceAllString(md, "")
	plain := lipgloss.NewStyle().Width(w)
	var out []string
	blank := func() {
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
	}
	fence := ""
	for _, ln := range strings.Split(md, "\n") {
		t := strings.TrimSpace(ln)
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
				continue
			}
			out = append(out, codeStyle.Render(clipCells("  "+strings.TrimRight(ln, " \t"), w)))
			continue
		}
		indent := (len(ln) - len(strings.TrimLeft(ln, " \t"))) / 2
		indent = min(indent, 3)
		switch {
		case strings.HasPrefix(t, "```"), strings.HasPrefix(t, "~~~"):
			fence = t[:3]
		case t == "":
			blank()
		case isRule(t):
			out = append(out, rule.Render(strings.Repeat("─", w)))
		case strings.HasPrefix(t, "#"):
			h := strings.TrimSpace(strings.TrimLeft(t, "#"))
			out = append(out, bold.Width(w).Render(plainInline(h)))
		case strings.HasPrefix(t, ">"):
			q := strings.TrimSpace(strings.TrimLeft(t, ">"))
			out = append(out, hang(quoteStyle.Render("│ "), quoteStyle.Render(inline(q)), "  ", w))
		case isBullet(t):
			item := strings.TrimSpace(t[1:])
			mark := "• "
			switch {
			case strings.HasPrefix(item, "[ ] "):
				mark, item = "☐ ", item[4:]
			case strings.HasPrefix(item, "[x] "), strings.HasPrefix(item, "[X] "):
				mark, item = "☑ ", item[4:]
			}
			pad := strings.Repeat("  ", indent)
			out = append(out, hang(pad+mark, inline(item), pad+"  ", w))
		case orderedRe.MatchString(t):
			m := orderedRe.FindStringSubmatch(t)
			pad := strings.Repeat("  ", indent)
			num := m[1] + ". "
			out = append(out, hang(pad+num, inline(m[2]), pad+strings.Repeat(" ", len(num)), w))
		default:
			out = append(out, plain.Render(inline(t)))
		}
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// hang wraps text in w cells after first, with later lines after rest.
func hang(first, text, rest string, w int) string {
	iw := max(w-lipgloss.Width(first), 1)
	lines := strings.Split(lipgloss.NewStyle().Width(iw).Render(text), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
		if i == 0 {
			lines[i] = first + lines[i]
		} else {
			lines[i] = rest + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func clipCells(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		return string(r[:w-1]) + "…"
	}
	return s
}

func isRule(t string) bool {
	if len(t) < 3 {
		return false
	}
	for _, c := range []string{"-", "*", "_"} {
		if strings.Trim(strings.ReplaceAll(t, " ", ""), c) == "" {
			return true
		}
	}
	return false
}

func isBullet(t string) bool {
	return len(t) > 1 && strings.ContainsRune("-*+", rune(t[0])) && t[1] == ' '
}

// inline styles one line's bold and code, and drops the rest of the
// markup (italics, links' URLs, escapes).
func inline(s string) string { return inlineWith(s, true) }

// plainInline drops every bit of markup, styling nothing.
func plainInline(s string) string { return inlineWith(s, false) }

func inlineWith(s string, styled bool) string {
	s = imageRe.ReplaceAllString(s, "$1")
	s = linkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := linkRe.FindStringSubmatch(m)
		if sub[1] == sub[2] || sub[2] == "" {
			return sub[1]
		}
		return sub[1] + " (" + sub[2] + ")"
	})
	var b strings.Builder
	r := []rune(s)
	style := func(st lipgloss.Style, text string) {
		if styled {
			b.WriteString(st.Render(text))
		} else {
			b.WriteString(text)
		}
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == '\\' && i+1 < len(r) && strings.ContainsRune("\\`*_[]()#+-.!>", r[i+1]):
			b.WriteRune(r[i+1])
			i++
		case c == '`':
			if j := indexRune(r, '`', i+1); j > i+1 {
				style(codeStyle, string(r[i+1:j]))
				i = j
				continue
			}
			b.WriteRune(c)
		case (c == '*' || c == '_') && i+1 < len(r) && r[i+1] == c:
			if j := indexPair(r, c, i+2); j > i+2 {
				style(bold, inlineWith(string(r[i+2:j]), false))
				i = j + 1
				continue
			}
			b.WriteRune(c)
			b.WriteRune(c)
			i++
		case c == '*' && i+1 < len(r) && r[i+1] != ' ':
			// *italic*: keep the words, drop the stars.
			if j := indexRune(r, '*', i+1); j > i+1 && r[j-1] != ' ' {
				b.WriteString(inlineWith(string(r[i+1:j]), styled))
				i = j
				continue
			}
			b.WriteRune(c)
		case c == '_' && (i == 0 || r[i-1] == ' ') && i+1 < len(r) && r[i+1] != ' ':
			// _italic_ only at a word's start, so snake_case stays.
			if j := indexRune(r, '_', i+1); j > i+1 && (j+1 == len(r) || !isWordRune(r[j+1])) {
				b.WriteString(inlineWith(string(r[i+1:j]), styled))
				i = j
				continue
			}
			b.WriteRune(c)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

func indexRune(r []rune, c rune, from int) int {
	for j := from; j < len(r); j++ {
		if r[j] == c {
			return j
		}
	}
	return -1
}

// indexPair finds the next cc (two of c) at or after from.
func indexPair(r []rune, c rune, from int) int {
	for j := from; j+1 < len(r); j++ {
		if r[j] == c && r[j+1] == c {
			return j
		}
	}
	return -1
}

func isWordRune(c rune) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
