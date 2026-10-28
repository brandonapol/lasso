package attention

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// GlossaryEntry maps one piece of bot jargon to plain words. Each word form
// is its own entry so the replacement reads right ("landed" isn't "land").
//
// Plain replaces the term outright, so it must fit where the term stood; it
// is only used on text lasso writes itself. Gloss is the short explanation
// that goes in brackets after the term in a bot's own sentence, so it must
// read well there: "rebase (bring its branch up to date)".
type GlossaryEntry struct {
	Jargon string
	Plain  string
	Gloss  string
}

// Glossary is the bot jargon lasso must never show unexplained. It is seeded
// from saddle's docs/GLOSSARY.md, the source of most of the jargon, plus
// the agent-speak the other bots use. The plain and gloss sides must
// themselves be jargon free (a test checks).
var Glossary = []GlossaryEntry{
	// The PR stack and how work reaches it.
	{"stale base", "the branch it started from has moved on", "its starting point is out of date"},
	{"has a stale base", "started from a branch that has since moved on", "its starting point is out of date"},
	{"restack", "rebuild its chain of pull requests on the latest code", "rebuild its chain of pull requests on the latest code"},
	{"restacked", "rebuilt its chain of pull requests on the latest code", "rebuilt its chain of pull requests on the latest code"},
	{"restacking", "rebuilding its chain of pull requests on the latest code", "rebuilding its chain of pull requests on the latest code"},
	{"unstack", "take out of the chain of pull requests", "take it out of the chain of pull requests"},
	{"unstacked", "taken out of the chain of pull requests", "taken out of the chain of pull requests"},
	{"PR stack", "chain of pull requests", "a chain of pull requests that build on each other"},
	{"stack", "chain of pull requests", "a chain of pull requests that build on each other"},
	{"land", "add to the shared work branch", "add it to the shared work branch"},
	{"lands", "adds to the shared work branch", "adds it to the shared work branch"},
	{"landed", "added to the shared work branch", "added to the shared work branch"},
	{"landing", "adding to the shared work branch", "adding it to the shared work branch"},
	{"merge train", "line of finished work waiting to be added one at a time", "the line of finished work waiting to be added one at a time"},
	{"train", "line of finished work", "the line of finished work waiting to be added"},
	{"integration branch", "shared work branch", "the shared work branch"},
	{"requeue", "put back in line", "put it back in line"},
	{"requeued", "put back in line", "put back in line"},
	{"publish", "open as a standalone pull request", "open it as a standalone pull request"},
	{"published", "opened as a standalone pull request", "opened as a standalone pull request"},
	{"rebase", "move onto the latest code", "bring its branch up to date"},
	{"rebased", "moved onto the latest code", "brought its branch up to date"},
	{"rebasing", "moving onto the latest code", "bringing its branch up to date"},

	// Ownership and workspaces.
	{"claim", "file reservation", "a reservation on some files"},
	{"claims", "reserved files", "the files it has reserved"},
	{"claimed", "reserved", "reserved"},
	{"worktree", "separate working copy", "a separate working copy"},
	{"worktrees", "separate working copies", "separate working copies"},
	{"serial file", "file only the line of finished work may change", "a file only the line of finished work may change"},

	// Checks and watchers.
	{"gate", "checks that must pass", "the checks that must pass"},
	{"sentinel", "watcher that checks the chain of pull requests", "a watcher that checks the chain of pull requests"},
	{"at-risk", "possibly broken", "possibly broken"},
	{"ci-red", "failing its checks", "failing its checks"},
	{"test_failed", "failed its tests", "its tests failed"},
	{"heavy run", "slow test run waiting its turn", "a slow test run waiting its turn"},

	// Bot states.
	{"needs_you", "waiting on you", "waiting on you"},
	{"needs-you", "waiting on you", "waiting on you"},
	{"stall budget", "time it waits for an answer", "how long it waits for an answer"},
	{"on_hold", "held back", "held back"},
	{"parked", "paused to free up room", "paused to free up room"},
	{"park", "pause to free up room", "pause to free up room"},
	{"orphaned", "lost its window", "lost its window"},
	{"escalated", "handed to you after it gave up", "handed to you after it gave up"},
	{"spawn", "start", "start"},
	{"spawned", "started", "started"},
	{"orchestrator", "coordinating bot", "the coordinating bot"},
	{"headless", "running in the background", "running in the background"},
	{"checkpoint", "automatic snapshot", "an automatic snapshot"},
	{"permission prompt", "request for permission", "a request for permission"},
}

// commonWords are glossary terms that are also everyday English ("we land
// in Denver", "the garden gate"). They only count as jargon in a bot-y
// context; see isJargon.
var commonWords = map[string]bool{
	"land": true, "lands": true, "landed": true, "landing": true,
	"stack": true, "train": true, "gate": true,
	"claim": true, "claims": true, "claimed": true,
	"publish": true, "published": true,
	"park": true, "parked": true,
}

// botContextRe matches the words near a common word that mark it as bot
// talk: pull requests, branches, tasks and their ids ("t7", "#12"), merges,
// queues, commits, checks, files, worktrees, agents, and "onto main".
var botContextRe = regexp.MustCompile(`(?i)\b(?:prs?|pull requests?|branch(?:es)?|tasks?|t\d+|#\d+|merge[sd]?|queued?|commits?|ci|checks|files?|agents?|bots?|saddle|(?:onto|into|on|to|from) main)\b`)

var (
	glossaryRe    *regexp.Regexp
	glossaryByKey map[string]GlossaryEntry
	codeSpanRe    = regexp.MustCompile("`[^`\n]*`")
	wordRe        = regexp.MustCompile(`[\w#-]+`)
)

// contextWords is how many words either side of a common word isJargon
// looks at, within the same sentence.
const contextWords = 4

func init() {
	entries := append([]GlossaryEntry(nil), Glossary...)
	// Longest first, so "merge train" wins over "train".
	sort.SliceStable(entries, func(i, j int) bool { return len(entries[i].Jargon) > len(entries[j].Jargon) })
	alts := make([]string, len(entries))
	glossaryByKey = make(map[string]GlossaryEntry, len(entries))
	for i, e := range entries {
		alts[i] = regexp.QuoteMeta(e.Jargon)
		glossaryByKey[strings.ToLower(e.Jargon)] = e
	}
	glossaryRe = regexp.MustCompile(`(?i)\b(?:` + strings.Join(alts, "|") + `)\b`)
}

// isJargon reports whether the glossary term at text[s:e] is being used as
// bot jargon. Most terms ("restack", "worktree") always are. A common word
// (see commonWords) is jargon only when:
//
//   - it is inside a `code span`;
//   - the text is nothing but the term (someone asking what it means); or
//   - within contextWords words either side, in the same sentence, there is
//     a bot-context word (botContextRe) or another glossary term that isn't
//     a common word itself ("Restack before you land").
//
// So "t7 landed on main" and "land the PR" are jargon; "we land in Denver"
// and "the garden gate" are not.
func isJargon(text string, s, e int) bool {
	key := strings.ToLower(text[s:e])
	if !commonWords[key] {
		return true
	}
	if strings.EqualFold(strings.Trim(text, " \t\n.:?!"), key) {
		return true
	}
	if inCodeSpan(text, s) >= 0 {
		return true
	}
	before := text[:s]
	if i := strings.LastIndexAny(before, ".!?\n"); i >= 0 {
		before = before[i+1:]
	}
	after := text[e:]
	if i := strings.IndexAny(after, ".!?\n"); i >= 0 {
		after = after[:i]
	}
	bw := wordRe.FindAllString(before, -1)
	if len(bw) > contextWords {
		bw = bw[len(bw)-contextWords:]
	}
	aw := wordRe.FindAllString(after, -1)
	if len(aw) > contextWords {
		aw = aw[:contextWords]
	}
	for _, window := range []string{strings.Join(bw, " "), strings.Join(aw, " ")} {
		if botContextRe.MatchString(window) {
			return true
		}
		for _, m := range glossaryRe.FindAllString(window, -1) {
			if !commonWords[strings.ToLower(m)] {
				return true
			}
		}
	}
	return false
}

// inCodeSpan returns the end of the `code span` holding text[pos], or -1.
func inCodeSpan(text string, pos int) int {
	for _, r := range codeSpanRe.FindAllStringIndex(text, -1) {
		if r[0] < pos && pos < r[1] {
			return r[1]
		}
	}
	return -1
}

// jargonAt returns the [start, end) of each glossary term text uses as
// jargon, in order.
func jargonAt(text string) [][]int {
	var out [][]int
	for _, m := range glossaryRe.FindAllStringIndex(text, -1) {
		if isJargon(text, m[0], m[1]) {
			out = append(out, m)
		}
	}
	return out
}

// Plain rewrites bot jargon in text into plain words. A capitalised term
// ("Restack") gets a capitalised replacement; an acronym ("PR stack") doesn't.
//
// Swapping words can break a sentence ("rebase onto main" → "move onto the
// latest code onto main"), so Plain is only for text lasso writes itself,
// where the wording is ours. Bot text goes through Gloss.
func Plain(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range jargonAt(text) {
		t := text[m[0]:m[1]]
		p := glossaryByKey[strings.ToLower(t)].Plain
		if capitalised(t) {
			r, n := utf8.DecodeRuneInString(p)
			p = string(unicode.ToUpper(r)) + p[n:]
		}
		b.WriteString(text[last:m[0]])
		b.WriteString(p)
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

// Gloss explains bot jargon without rewriting the bot's sentence: after the
// first use of each term it adds the term's gloss in brackets, so "Do you
// want to rebase onto main?" reads "Do you want to rebase (bring its branch
// up to date) onto main?". A term inside a `code span` gets its gloss after
// the span. Glossing twice changes nothing.
func Gloss(text string) string {
	text, _ = stripGlosses(text)
	var b strings.Builder
	last := 0
	done := map[string]bool{}
	// Glosses for terms inside a code span wait for the span to close. Spans
	// don't overlap, so there is at most one waiting at a time.
	spanEnd, waiting := -1, []string(nil)
	flush := func() {
		if spanEnd < 0 {
			return
		}
		b.WriteString(text[last:spanEnd])
		for _, g := range waiting {
			b.WriteString(" (" + g + ")")
		}
		last, spanEnd, waiting = spanEnd, -1, nil
	}
	for _, m := range jargonAt(text) {
		if spanEnd >= 0 && spanEnd <= m[0] {
			flush()
		}
		key := strings.ToLower(text[m[0]:m[1]])
		if done[key] {
			continue
		}
		done[key] = true
		g := glossaryByKey[key].Gloss
		if end := inCodeSpan(text, m[0]); end >= 0 {
			spanEnd, waiting = end, append(waiting, g)
			continue
		}
		b.WriteString(text[last:m[1]])
		b.WriteString(" (" + g + ")")
		last = m[1]
	}
	flush()
	b.WriteString(text[last:])
	return b.String()
}

// stripGlosses removes the brackets Gloss added, so the text can be read
// (or glossed again) as the bot wrote it, and returns the terms that had
// one, lowercased. Only a term's own gloss counts, right after the term or
// after the code span it sits in.
func stripGlosses(text string) (string, map[string]bool) {
	glossed := map[string]bool{}
	var b strings.Builder
	last := 0
	for _, m := range glossaryRe.FindAllStringIndex(text, -1) {
		if m[0] < last {
			continue
		}
		end, keys := m[1], []string{strings.ToLower(text[m[0]:m[1]])}
		if c := inCodeSpan(text, m[0]); c >= 0 {
			end, keys = c, nil
			for _, t := range glossaryRe.FindAllString(text[m[0]:c], -1) {
				keys = append(keys, strings.ToLower(t))
			}
		}
		cut := end
		for found := true; found; {
			found = false
			for _, k := range keys {
				if p := " (" + glossaryByKey[k].Gloss + ")"; strings.HasPrefix(text[cut:], p) {
					glossed[k] = true
					cut += len(p)
					found = true
				}
			}
		}
		if cut > end {
			b.WriteString(text[last:end])
			last = cut
		}
	}
	b.WriteString(text[last:])
	return b.String(), glossed
}

// capitalised reports whether s starts with an upper-case letter followed by
// a lower-case one.
func capitalised(s string) bool {
	r1, n := utf8.DecodeRuneInString(s)
	r2, _ := utf8.DecodeRuneInString(s[n:])
	return unicode.IsUpper(r1) && unicode.IsLower(r2)
}

// Unexplained returns the glossary terms text uses as jargon with no gloss
// beside them, lowercased and in order of appearance, each once. Empty
// means the text is plain, or every term in it has been glossed.
func Unexplained(text string) []string {
	text, glossed := stripGlosses(text)
	var out []string
	seen := map[string]bool{}
	for _, m := range jargonAt(text) {
		k := strings.ToLower(text[m[0]:m[1]])
		if !seen[k] && !glossed[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}
