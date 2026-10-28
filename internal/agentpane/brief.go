package agentpane

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/llm"
)

// facts are what "since you left" is written from.
type facts struct {
	since   time.Time
	moved   map[api.Tab][]api.Item // updated since the user last read the brief
	waiting []api.Item             // asks on In progress ("needs you")
	due     []api.Item             // issues with a due date
}

func (f facts) empty() bool { return len(f.moved) == 0 && len(f.waiting) == 0 && len(f.due) == 0 }

func (f facts) movedCount() int {
	n := 0
	for _, its := range f.moved {
		n += len(its)
	}
	return n
}

// statusNeedsYou is the In progress status of an ask (internal/source).
const statusNeedsYou = "needs you"

// gather reads every tab through the API and sorts out what's new since the
// brief cursor, what waits on the user and what is due.
func (p *Pane) gather(ctx context.Context) (facts, error) {
	since, err := attention.LastSeen(p.opts.Store)
	if err != nil {
		return facts{}, err
	}
	f := facts{since: since, moved: map[api.Tab][]api.Item{}}
	for _, tab := range api.Tabs {
		items, err := p.api.items(ctx, tab)
		if err != nil {
			return facts{}, err
		}
		for _, it := range items {
			if strings.HasPrefix(it.ID, "setup:") {
				continue
			}
			if it.Updated.After(since) {
				f.moved[tab] = append(f.moved[tab], it)
			}
			if tab == api.TabDoing && it.Status == statusNeedsYou {
				f.waiting = append(f.waiting, it)
			}
			if tab == api.TabIssues && strings.HasPrefix(it.Status, "due ") {
				f.due = append(f.due, it)
			}
		}
	}
	return f, nil
}

var tabWords = map[api.Tab][2]string{
	api.TabIssues:    {"issue", "issues"},
	api.TabActions:   {"workflow run", "workflow runs"},
	api.TabDoing:     {"bot or pull request", "bots and pull requests"},
	api.TabSummaries: {"summary", "summaries"},
}

func count(n int, words [2]string) string {
	if n == 1 {
		return "1 " + words[0]
	}
	return fmt.Sprintf("%d %s", n, words[1])
}

func titles(items []api.Item, n int) string {
	var ts []string
	for i, it := range items {
		if i == n {
			ts = append(ts, fmt.Sprintf("%d more", len(items)-n))
			break
		}
		ts = append(ts, fmt.Sprintf("%q", clip(it.Title, 60)))
	}
	return strings.Join(ts, ", ")
}

// local is the "since you left" message written without a model.
func (f facts) local() string {
	if f.empty() {
		return "Nothing new since you left."
	}
	var parts []string
	if n := f.movedCount(); n > 0 {
		var what []string
		for _, tab := range api.Tabs {
			if its := f.moved[tab]; len(its) > 0 {
				what = append(what, count(len(its), tabWords[tab]))
			}
		}
		parts = append(parts, "Since you left, "+strings.Join(what, ", ")+" changed.")
	}
	if len(f.waiting) > 0 {
		parts = append(parts, "Waiting on you: "+titles(f.waiting, 3)+".")
	}
	if len(f.due) > 0 {
		var ds []string
		for i, it := range f.due {
			if i == 3 {
				ds = append(ds, fmt.Sprintf("%d more", len(f.due)-3))
				break
			}
			ds = append(ds, fmt.Sprintf("%q (%s)", clip(it.Title, 60), it.Status))
		}
		parts = append(parts, "Due: "+strings.Join(ds, ", ")+".")
	}
	return strings.Join(parts, " ")
}

// forModel lays the facts out for the model. Titles come from GitHub and the
// bots, so the caller fences them.
func (f facts) forModel(now time.Time) string {
	var b strings.Builder
	if f.since.IsZero() {
		b.WriteString("This is the first time the user has opened lasso; everything open counts as new.\n")
	} else {
		fmt.Fprintf(&b, "The user was last here %s ago.\n", ago(now.Sub(f.since)))
	}
	for _, tab := range api.Tabs {
		its := f.moved[tab]
		if len(its) == 0 {
			continue
		}
		fmt.Fprintf(&b, "Changed on %s (%d):\n", tab, len(its))
		for i, it := range its {
			if i == 8 {
				fmt.Fprintf(&b, "  …and %d more\n", len(its)-8)
				break
			}
			fmt.Fprintf(&b, "  - %s [%s] %s\n", clip(it.Title, 120), it.Status, it.Repo)
		}
	}
	if len(f.waiting) > 0 {
		fmt.Fprintf(&b, "Waiting on the user (%d):\n", len(f.waiting))
		for _, it := range f.waiting {
			fmt.Fprintf(&b, "  - %s\n", clip(it.Title, 120))
		}
	}
	if len(f.due) > 0 {
		fmt.Fprintf(&b, "Due (%d):\n", len(f.due))
		for _, it := range f.due {
			fmt.Fprintf(&b, "  - %s [%s]\n", clip(it.Title, 120), it.Status)
		}
	}
	return b.String()
}

// sinceYouLeft writes the message: by the model when there is something to
// say and a model to say it, else locally.
func (p *Pane) sinceYouLeft(ctx context.Context, f facts) string {
	if f.empty() {
		return f.local()
	}
	m, err := p.opts.Model()
	if err != nil {
		return f.local()
	}
	body, err := fence(f.forModel(p.now()))
	if err != nil {
		return f.local()
	}
	turn, err := m.Converse(ctx, llm.Conversation{
		System:    systemPrompt,
		Messages:  []llm.Message{{Role: "user", Content: []llm.Block{llm.TextBlock(sinceYouLeftPrompt + "\n" + body)}}},
		MaxTokens: 300,
	})
	if err != nil {
		p.logf("lasso: agent pane: since you left: %v", err)
		return f.local()
	}
	if t := strings.TrimSpace(turn.Text()); t != "" {
		return tidy(t)
	}
	return f.local()
}

func (p *Pane) markRead(now time.Time) error {
	return p.opts.Store.SetCursor(attention.BriefCursor, attention.FormatCursor(now))
}

// fieldWhy is the focus field the source adds to a line-jumper's row.
const fieldWhy = "Why it's here"

// CheckLineJumpers adds one message for each line-jumper on In progress not
// mentioned before. It's the only other time the pane speaks unprompted.
func (p *Pane) CheckLineJumpers(ctx context.Context) error {
	items, err := p.api.items(ctx, api.TabDoing)
	if err != nil {
		return err
	}
	type jumper struct{ id, why string }
	var found []jumper
	present := map[string]bool{}
	for _, it := range items {
		if it.Status != statusNeedsYou {
			continue
		}
		present[it.ID] = true
		f, err := p.api.focus(ctx, it.ID)
		if err != nil {
			continue
		}
		if why := field(f, fieldWhy); why != "" {
			found = append(found, jumper{it.ID, why})
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := p.load()
	if err != nil {
		return err
	}
	changed := false
	for id := range st.Announced {
		if !present[id] {
			delete(st.Announced, id)
			changed = true
		}
	}
	for _, j := range found {
		if _, ok := st.Announced[j.id]; ok {
			continue
		}
		if st.Announced == nil {
			st.Announced = map[string]time.Time{}
		}
		st.Announced[j.id] = p.now()
		p.say(&st, "line_jumper", j.why+" It's at the top of In progress.")
		changed = true
	}
	if !changed {
		return nil
	}
	return p.save(&st)
}

// Watch checks for line-jumpers every interval until ctx ends. The first
// check is one interval in, once the daemon is serving and has read its
// sources.
func (p *Pane) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := p.CheckLineJumpers(ctx); err != nil && ctx.Err() == nil {
			p.logf("lasso: agent pane: line-jumpers: %v", err)
		}
	}
}
