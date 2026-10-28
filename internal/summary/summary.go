// Package summary writes the Summaries tab: for each hot repo, a short
// plain-language paragraph about its last 24 hours, its biggest changes (up
// to three links) and who did the work, you or the bots (ADR 0001 §7).
//
// The input is compact: titles, authors, times and counts from GitHub (no
// bodies, comments or diffs), plus the digest's brief points about bots
// working in the repo. Jev cuts the merged PRs and issues to the notable
// ones when a Jev client exists; then one claude-sonnet-5-5 call writes.
// The counts shown on the tab come from GitHub, never from the model.
//
// The writer is told to avoid jargon and given lasso's glossary. An answer
// that still uses glossary terms (attention.Unexplained) is sent back once;
// if the second answer does too, or the model can't be used (no key, over
// its daily cap, an error), the summary is lasso's own counts-only text.
//
// Each repo's summary is cached in lasso's database and rewritten at most
// once an hour, unless the user asks (Regenerate).
package summary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/github"
	"github.com/brandonapol/lasso/internal/jev"
	"github.com/brandonapol/lasso/internal/llm"
)

const (
	// Window is how far back a summary looks.
	Window = 24 * time.Hour
	// Interval is the least time between two summaries of one repo, unless
	// the user asks for a new one.
	Interval = time.Hour
	// MaxChanges is the most "biggest changes" a summary lists.
	MaxChanges = 3
	// MaxPromptItems bounds how many items one Sonnet call sees.
	MaxPromptItems = 30
	// MaxBriefs bounds how many digest points one Sonnet call sees.
	MaxBriefs = 10
	// cacheTTL is how long a summary stays in the database. It outlives
	// Interval so the tab has something to show while a new one is written.
	cacheTTL = 7 * 24 * time.Hour
)

// Change is one of the biggest changes, with its link.
type Change struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Counts are the repo's last 24 hours as GitHub reports them. They never
// come from a model.
type Counts struct {
	Merged   int `json:"merged"`   // pull requests merged
	Closed   int `json:"closed"`   // issues closed
	Opened   int `json:"opened"`   // issues opened
	Releases int `json:"releases"` // releases published
	// RedRuns is how many workflows' latest run on the default branch
	// failed: still red.
	RedRuns int `json:"red_runs"`
}

// Summary is one repo's last 24 hours.
type Summary struct {
	Repo      string   `json:"repo"`
	Paragraph string   `json:"paragraph"`
	Changes   []Change `json:"changes"`
	Who       string   `json:"who"`
	Counts    Counts   `json:"counts"`
	// Model is the model that wrote the paragraph ("written by Model at
	// WrittenAt"), empty when Local.
	Model     string    `json:"model,omitempty"`
	WrittenAt time.Time `json:"written_at"`
	// OverCap is true when Sonnet's daily spending cap stopped the call.
	OverCap bool `json:"over_cap,omitempty"`
	// Local is true when the text is lasso's counts-only fallback.
	Local bool `json:"local,omitempty"`
	// Note says, in plain words, why the summary is Local.
	Note string `json:"note,omitempty"`
	// CutBy is "jev" when Jev chose the notable items.
	CutBy string `json:"cut_by,omitempty"`
}

// Cache is where summaries are kept. *store.Store satisfies it.
type Cache = github.Cache

// Options configure a Summarizer.
type Options struct {
	// Fetch reads a repo's activity. Required.
	Fetch Fetcher
	// Cache keeps summaries. Required (*store.Store in production).
	Cache Cache
	// LLM writes the summary (a claude-sonnet-5-5 client under the
	// Sonnet cap). Nil means counts only.
	LLM *llm.Client
	// Jev cuts the items to the notable ones. Nil means no cut beyond
	// MaxPromptItems.
	Jev *jev.Client
	// Briefs returns the digest's brief points about bots working in repo
	// since a time. Nil means none.
	Briefs func(repo string, since time.Time) []string
	// Me is the user's GitHub login. Empty asks Fetch, when it has a
	// Login method (GH does).
	Me string
	// Now defaults to time.Now.
	Now func() time.Time
}

// Summarizer writes and caches summaries. It is safe for concurrent use.
type Summarizer struct {
	fetch    Fetcher
	cache    Cache
	llm      *llm.Client
	jev      *jev.Client
	briefs   func(string, time.Time) []string
	now      func() time.Time
	newNonce func() (string, error)

	mu    sync.Mutex
	me    string
	repos map[string]*sync.Mutex
}

// New returns a Summarizer.
func New(o Options) *Summarizer {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Summarizer{
		fetch: o.Fetch, cache: o.Cache, llm: o.LLM, jev: o.Jev, briefs: o.Briefs, now: o.Now,
		newNonce: randomNonce, me: o.Me, repos: map[string]*sync.Mutex{},
	}
}

// CacheKey is repo's summary's key in the cache.
func CacheKey(repo string) string { return "summary:" + repo }

// Get returns repo's cached summary, however old, and whether there is one.
func (s *Summarizer) Get(repo string) (Summary, bool, error) {
	b, ok, err := s.cache.CacheGet(CacheKey(repo), s.now())
	if err != nil || !ok {
		return Summary{}, false, err
	}
	var sum Summary
	if err := json.Unmarshal(b, &sum); err != nil {
		return Summary{}, false, fmt.Errorf("summary: cached %s: %w", repo, err)
	}
	return sum, true, nil
}

// Refresh returns repo's cached summary when it's less than Interval old,
// and otherwise writes a new one.
func (s *Summarizer) Refresh(ctx context.Context, repo string) (Summary, error) {
	return s.generate(ctx, repo, false)
}

// Regenerate writes a new summary of repo now, whatever its age: the user
// asked for it.
func (s *Summarizer) Regenerate(ctx context.Context, repo string) (Summary, error) {
	return s.generate(ctx, repo, true)
}

func (s *Summarizer) lock(repo string) func() {
	s.mu.Lock()
	m := s.repos[repo]
	if m == nil {
		m = &sync.Mutex{}
		s.repos[repo] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (s *Summarizer) generate(ctx context.Context, repo string, force bool) (Summary, error) {
	defer s.lock(repo)()
	if !force {
		if cur, ok, err := s.Get(repo); err == nil && ok && s.now().Sub(cur.WrittenAt) < Interval {
			return cur, nil
		}
	}
	sum, err := s.write(ctx, repo)
	if err != nil {
		return Summary{}, err
	}
	b, err := json.Marshal(sum)
	if err != nil {
		return Summary{}, err
	}
	if err := s.cache.CachePut(CacheKey(repo), b, s.now(), cacheTTL); err != nil {
		return sum, fmt.Errorf("summary: cache %s: %w", repo, err)
	}
	return sum, nil
}

// login is "you": Options.Me, else the fetcher's login, asked once.
func (s *Summarizer) login(ctx context.Context) string {
	s.mu.Lock()
	me := s.me
	s.mu.Unlock()
	if me != "" {
		return me
	}
	l, ok := s.fetch.(interface {
		Login(context.Context) (string, error)
	})
	if !ok {
		return ""
	}
	me, err := l.Login(ctx)
	if err != nil || me == "" {
		return ""
	}
	s.mu.Lock()
	s.me = me
	s.mu.Unlock()
	return me
}

// input is everything one summary is written from.
type input struct {
	repo    string
	me      string
	since   time.Time
	act     Activity
	counts  Counts
	red     []string
	items   []entry // notable items, in prompt order
	briefs  []string
	cutBy   string
	authors []tally
}

// entry is one numbered item in the prompt.
type entry struct {
	id   string // jev item id
	kind string // "merged pull request", "closed issue", ...
	it   Item
	// always items (releases) skip Jev.
	always bool
}

type tally struct {
	who string
	n   int
}

func (s *Summarizer) write(ctx context.Context, repo string) (Summary, error) {
	now := s.now()
	since := now.Add(-Window)
	act, err := s.fetch.Activity(ctx, repo, since)
	if err != nil {
		return Summary{}, err
	}
	in := input{repo: repo, me: s.login(ctx), since: since, act: act}
	in.red = stillRed(act.Runs)
	in.counts = Counts{
		Merged: act.MergedTotal, Closed: act.ClosedTotal, Opened: act.OpenedTotal,
		Releases: len(act.Releases), RedRuns: len(in.red),
	}
	in.authors = authors(act, in.me)
	if s.briefs != nil {
		in.briefs = s.briefs(repo, since)
		if len(in.briefs) > MaxBriefs {
			in.briefs = in.briefs[len(in.briefs)-MaxBriefs:]
		}
	}
	in.items, in.cutBy = s.notable(ctx, act)

	sum := Summary{Repo: repo, Counts: in.counts, WrittenAt: now, CutBy: in.cutBy}
	if s.llm == nil {
		return s.local(sum, in, "no Anthropic key, so this is lasso's own count"), nil
	}
	if in.counts == (Counts{}) && len(in.briefs) == 0 && len(act.Runs) == 0 {
		return s.local(sum, in, ""), nil // nothing happened: no call
	}
	w, err := s.ask(ctx, in)
	switch {
	case errors.Is(err, llm.ErrOverCap):
		sum.OverCap = true
		return s.local(sum, in, "the summaries' daily spending cap is reached, so this is lasso's own count"), nil
	case errors.Is(err, errJargon):
		return s.local(sum, in, "the writer kept using jargon, so this is lasso's own count"), nil
	case err != nil:
		return s.local(sum, in, "the writer couldn't be used ("+clip(err.Error(), 120)+"), so this is lasso's own count"), nil
	}
	sum.Paragraph, sum.Changes, sum.Who, sum.Model = w.paragraph, w.changes, w.who, s.llm.Model()
	return sum, nil
}

// notable is the items worth writing about. Releases always are. Merged
// PRs and issues are cut by Jev when there is one; without Jev, or on any
// Jev error, they're all kept, newest first, up to MaxPromptItems.
func (s *Summarizer) notable(ctx context.Context, act Activity) ([]entry, string) {
	var all []entry
	for _, it := range act.Releases {
		all = append(all, entry{kind: "release", it: it, always: true})
	}
	add := func(kind, prefix string, its []Item) {
		for _, it := range its {
			all = append(all, entry{id: fmt.Sprintf("%s%d", prefix, it.Number), kind: kind, it: it})
		}
	}
	add("merged pull request", "pr", act.Merged)
	add("closed issue", "closed", act.Closed)
	add("opened issue", "opened", act.Opened)

	cutBy := ""
	if s.jev != nil {
		var items []jev.Item
		for _, e := range all {
			if !e.always {
				items = append(items, jev.Item{ID: e.id, Text: e.kind + ": " + e.it.Title})
			}
		}
		if len(items) > 0 {
			if kept, err := s.jev.Cut(ctx, items, worth); err == nil {
				keep := map[string]bool{}
				for _, k := range kept {
					keep[k.ID] = true
				}
				var out []entry
				for _, e := range all {
					if e.always || keep[e.id] {
						out = append(out, e)
					}
				}
				all, cutBy = out, "jev"
			}
		}
	}
	if len(all) > MaxPromptItems {
		all = all[:MaxPromptItems]
	}
	return all, cutBy
}

// worth is Jev's question for each item.
var worth = jev.Question{
	Type: "noul",
	Instructions: "This is one pull request or issue from a software repository's last 24 hours. " +
		"Is it notable enough to mention in a short daily summary for the repository's owner: " +
		"a feature, a fix that matters, a breaking change, or a decision? Routine dependency bumps, " +
		"typo fixes, chores and duplicates are not.",
}

// authors tallies who merged PRs, closed and opened issues and published
// releases in the window: "you" for me, everyone else by login.
func authors(act Activity, me string) []tally {
	n := map[string]int{}
	for _, its := range [][]Item{act.Merged, act.Closed, act.Opened, act.Releases} {
		for _, it := range its {
			who := it.Author
			switch {
			case who == "":
				who = "unknown"
			case me != "" && strings.EqualFold(who, me):
				who = "you"
			}
			n[who]++
		}
	}
	out := make([]tally, 0, len(n))
	for w, c := range n {
		out = append(out, tally{w, c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		if (out[i].who == "you") != (out[j].who == "you") {
			return out[i].who == "you"
		}
		return out[i].who < out[j].who
	})
	return out
}

// local is lasso's own counts-only summary.
func (s *Summarizer) local(sum Summary, in input, note string) Summary {
	sum.Local, sum.Note, sum.Model = true, note, ""
	sum.Paragraph = countsText(in)
	for _, e := range in.items {
		if len(sum.Changes) == MaxChanges {
			break
		}
		if e.kind == "release" || e.kind == "merged pull request" {
			sum.Changes = append(sum.Changes, Change{Title: localTitle(e), URL: e.it.URL})
		}
	}
	sum.Who = whoText(in.authors)
	return sum
}

func localTitle(e entry) string {
	t := attention.Plain(clip(oneLine(e.it.Title), 120))
	if e.kind == "release" {
		return "Released " + t
	}
	return t
}

// countsText is the counts-only paragraph.
func countsText(in input) string {
	c := in.counts
	var parts []string
	add := func(n int, one, many string) {
		switch {
		case n == 1:
			parts = append(parts, "1 "+one)
		case n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", n, many))
		}
	}
	add(c.Merged, "pull request merged", "pull requests merged")
	add(c.Closed, "issue closed", "issues closed")
	add(c.Opened, "issue opened", "issues opened")
	add(c.Releases, "release", "releases")
	text := "Nothing was merged, closed, opened or released in the last 24 hours."
	if len(parts) > 0 {
		text = "In the last 24 hours: " + joinList(parts) + "."
	}
	switch len(in.red) {
	case 0:
	case 1:
		text += fmt.Sprintf(" The %s checks are still failing on %s.", in.red[0], in.act.DefaultBranch)
	default:
		text += fmt.Sprintf(" %d sets of checks are still failing on %s.", len(in.red), in.act.DefaultBranch)
	}
	return text
}

// whoText is the counts-only "who did it".
func whoText(ts []tally) string {
	if len(ts) == 0 {
		return ""
	}
	var parts []string
	for _, t := range ts {
		if t.who == "you" {
			parts = append(parts, fmt.Sprintf("you (%d)", t.n))
		} else {
			parts = append(parts, fmt.Sprintf("%s (%d)", t.who, t.n))
		}
	}
	return "By " + joinList(parts) + "."
}

func joinList(p []string) string {
	switch len(p) {
	case 0:
		return ""
	case 1:
		return p[0]
	}
	return strings.Join(p[:len(p)-1], ", ") + " and " + p[len(p)-1]
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
