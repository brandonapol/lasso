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
type GlossaryEntry struct {
	Jargon string
	Plain  string
}

// Glossary is the bot jargon lasso must never show unexplained. It is seeded
// from saddle's docs/GLOSSARY.md, the source of most of the jargon, plus
// the agent-speak the other bots use. The plain side must itself be jargon
// free (a test checks).
var Glossary = []GlossaryEntry{
	// The PR stack and how work reaches it.
	{"stale base", "the branch it started from has moved on"},
	{"has a stale base", "started from a branch that has since moved on"},
	{"restack", "rebuild its chain of pull requests on the latest code"},
	{"restacked", "rebuilt its chain of pull requests on the latest code"},
	{"restacking", "rebuilding its chain of pull requests on the latest code"},
	{"unstack", "take out of the chain of pull requests"},
	{"unstacked", "taken out of the chain of pull requests"},
	{"PR stack", "chain of pull requests"},
	{"stack", "chain of pull requests"},
	{"land", "add to the shared work branch"},
	{"lands", "adds to the shared work branch"},
	{"landed", "added to the shared work branch"},
	{"landing", "adding to the shared work branch"},
	{"merge train", "line of finished work waiting to be added one at a time"},
	{"train", "line of finished work"},
	{"integration branch", "shared work branch"},
	{"requeue", "put back in line"},
	{"requeued", "put back in line"},
	{"publish", "open as a standalone pull request"},
	{"published", "opened as a standalone pull request"},
	{"rebase", "move onto the latest code"},
	{"rebased", "moved onto the latest code"},
	{"rebasing", "moving onto the latest code"},

	// Ownership and workspaces.
	{"claim", "file reservation"},
	{"claims", "reserved files"},
	{"claimed", "reserved"},
	{"worktree", "separate working copy"},
	{"worktrees", "separate working copies"},
	{"serial file", "file only the line of finished work may change"},

	// Checks and watchers.
	{"gate", "checks that must pass"},
	{"sentinel", "watcher that checks the chain of pull requests"},
	{"at-risk", "possibly broken"},
	{"ci-red", "failing its checks"},
	{"test_failed", "failed its tests"},
	{"heavy run", "slow test run waiting its turn"},

	// Bot states.
	{"needs_you", "waiting on you"},
	{"needs-you", "waiting on you"},
	{"stall budget", "time it waits for an answer"},
	{"on_hold", "held back"},
	{"parked", "paused to free up room"},
	{"park", "pause to free up room"},
	{"orphaned", "lost its window"},
	{"escalated", "handed to you after it gave up"},
	{"spawn", "start"},
	{"spawned", "started"},
	{"orchestrator", "coordinating bot"},
	{"headless", "running in the background"},
	{"checkpoint", "automatic snapshot"},
	{"permission prompt", "request for permission"},
}

var (
	glossaryRe    *regexp.Regexp
	glossaryPlain map[string]string
)

func init() {
	entries := append([]GlossaryEntry(nil), Glossary...)
	// Longest first, so "merge train" wins over "train".
	sort.SliceStable(entries, func(i, j int) bool { return len(entries[i].Jargon) > len(entries[j].Jargon) })
	alts := make([]string, len(entries))
	glossaryPlain = make(map[string]string, len(entries))
	for i, e := range entries {
		alts[i] = regexp.QuoteMeta(e.Jargon)
		glossaryPlain[strings.ToLower(e.Jargon)] = e.Plain
	}
	glossaryRe = regexp.MustCompile(`(?i)\b(?:` + strings.Join(alts, "|") + `)\b`)
}

// Plain rewrites bot jargon in text into plain words. A capitalised term
// ("Restack") gets a capitalised replacement; an acronym ("PR stack") doesn't.
func Plain(text string) string {
	return glossaryRe.ReplaceAllStringFunc(text, func(m string) string {
		p := glossaryPlain[strings.ToLower(m)]
		if capitalised(m) {
			r, n := utf8.DecodeRuneInString(p)
			return string(unicode.ToUpper(r)) + p[n:]
		}
		return p
	})
}

// capitalised reports whether s starts with an upper-case letter followed by
// a lower-case one.
func capitalised(s string) bool {
	r1, n := utf8.DecodeRuneInString(s)
	r2, _ := utf8.DecodeRuneInString(s[n:])
	return unicode.IsUpper(r1) && unicode.IsLower(r2)
}

// Unexplained returns the glossary terms text still uses, lowercased and in
// order of appearance, each once. Empty means the text is plain.
func Unexplained(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range glossaryRe.FindAllString(text, -1) {
		k := strings.ToLower(m)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}
