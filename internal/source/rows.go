package source

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/github"
	"github.com/brandonapol/lasso/internal/rules"
)

// BodyLimit is how many runes of an issue body the focus pane gets.
const BodyLimit = 2000

// dueSoon is how close a due date must be for an issue to count as
// issue_due (a brief) rather than an ordinary update.
const dueSoon = 7 * 24 * time.Hour

// kindGitHubError is the attention kind of a repo that can't be read.
const kindGitHubError attention.Kind = "github_error"

// Item ids are path-safe ("/items/{id}"): GitHub names never contain ':'.
func repoID(repo string) string { return strings.ReplaceAll(repo, "/", ":") }

func issueID(i github.Issue) string { return "issue:" + repoID(i.Repo) + ":" + strconv.Itoa(i.Number) }
func prID(p github.PR) string       { return "pr:" + repoID(p.Repo) + ":" + strconv.Itoa(p.Number) }
func runID(r github.Run) string {
	return "run:" + repoID(r.Repo) + ":" + strconv.FormatInt(r.ID, 10)
}
func errorID(repo string) string { return "error:" + repoID(repo) }

// attentionItems turns the raw state into attention items with levels.
func (s *Source) attentionItems(ctx context.Context) []attention.Item {
	var items []attention.Item
	now := s.opts.Clock.Now()
	for _, is := range s.issues {
		it := attention.Item{
			ID: issueID(is), Source: "github", Repo: is.Repo, Kind: attention.KindIssueUpdated,
			Title: is.Title, Created: is.Updated, Due: is.Due,
			Signals: attention.Signals{HotRepo: true, LastProgress: is.Updated},
		}
		if !is.Due.IsZero() && is.Due.Sub(now) < dueSoon {
			it.Kind = attention.KindIssueDue
		}
		items = append(items, it)
	}
	for _, pr := range s.prs {
		items = append(items, attention.Item{
			ID: prID(pr), Source: "github", Repo: pr.Repo, Kind: attention.KindIssueUpdated,
			Title: pr.Title, Created: pr.Updated, Signals: attention.Signals{HotRepo: true},
		})
	}
	red := s.redSince()
	for _, r := range s.runs {
		if r.InProgress() {
			continue
		}
		it := attention.Item{
			ID: runID(r), Source: "github", Repo: r.Repo, Kind: attention.KindRunPassed,
			Title: r.Workflow + " on " + r.Branch, Created: runTime(r),
			Signals: attention.Signals{HotRepo: true, DefaultBranch: s.isDefault(r.Repo, r.Branch), RedSince: red[runID(r)]},
		}
		if r.Failed() {
			it.Kind = attention.KindRunFailed
			it.Summary = failingSummary(r)
		}
		items = append(items, it)
	}
	for i := range items {
		items[i].Level = s.level(ctx, items[i])
	}
	for repo, e := range s.errs {
		items = append(items, attention.Item{
			ID: errorID(repo), Source: "github", Repo: repo, Kind: kindGitHubError,
			Title: "Can't read " + repo + " from GitHub", Summary: plainError(e.text()),
			Level: attention.Brief, Created: e.since,
		})
	}
	items = append(items, s.botItems(ctx)...)
	items = append(items, s.digestItems()...)
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

// level classifies it, asking the sorter only when the item changed.
func (s *Source) level(ctx context.Context, it attention.Item) attention.Level {
	stamp := fmt.Sprintf("%s|%d|%+v", it.Kind, it.Created.UnixNano(), it.Signals)
	if m, ok := s.levels[it.ID]; ok && m.stamp == stamp {
		return m.level
	}
	l := attention.Classify(ctx, it, s.opts.Sorter).Level
	s.levels[it.ID] = levelMemo{stamp: stamp, level: l}
	return l
}

// redSince finds, per repo and workflow, the newest failed run on the
// default branch and when the branch went red: the finish of the oldest
// failure in the unbroken run of failures leading up to it.
func (s *Source) redSince() map[string]time.Time {
	byWorkflow := map[string][]github.Run{}
	for _, r := range s.runs {
		if !r.InProgress() && s.isDefault(r.Repo, r.Branch) {
			k := r.Repo + "\x00" + r.Workflow
			byWorkflow[k] = append(byWorkflow[k], r)
		}
	}
	out := map[string]time.Time{}
	for _, runs := range byWorkflow {
		sort.Slice(runs, func(i, j int) bool { return runs[i].Started.After(runs[j].Started) })
		var since time.Time
		for _, r := range runs {
			if !r.Failed() {
				break
			}
			since = runTime(r)
		}
		if !since.IsZero() {
			out[runID(runs[0])] = since
		}
	}
	return out
}

// runTime is when a run finished, or started if it hasn't.
func runTime(r github.Run) time.Time {
	if !r.Finished.IsZero() {
		return r.Finished
	}
	return r.Started
}

// buildRows lays out every tab.
func (s *Source) buildRows(items []attention.Item, now time.Time, sugs []rules.Suggestion) map[string]row {
	rows := map[string]row{}
	add := func(r row) { rows[r.ID] = r }
	if len(s.opts.Repos) == 0 {
		for _, tab := range api.Tabs {
			add(s.setupRow(tab))
		}
	}
	s.botRows(add)
	suggestionRows(sugs, add)
	s.summaryRows(add)
	s.digestRows(add)
	for repo, e := range s.errs {
		for _, tab := range []api.Tab{api.TabIssues, api.TabActions} {
			add(errorRow(repo, e, tab))
		}
	}
	for _, is := range s.issues {
		add(issueRow(is))
	}
	for _, r := range s.runs {
		add(runRow(r))
	}
	for _, pr := range s.prs {
		if s.waitingOnUser(pr) {
			add(prRow(pr))
		}
	}
	for i, sh := range attention.InProgress(items, now) {
		base, ok := rows[sh.Item.ID]
		if !ok {
			e, isBot := s.botEvent(sh.Item.ID)
			base = row{Focus: askFocus(sh.Item, e, isBot)}
			if isBot {
				base.Fields = append(base.Fields, s.askFacts(e)...)
			}
		}
		f := base.Focus
		f.ID, f.Tab, f.Status, f.Right = "doing:"+sh.Item.ID, api.TabDoing, "needs you", "needs you"
		add(row{Focus: f, group: 1, seq: i, why: sh.LineJumper})
	}
	for id, r := range rows {
		rows[id] = finish(r)
	}
	return rows
}

// finish fills what every row has the same way: its right-hand text, when
// the row didn't set its own, is its status; and its focus can open its
// link and explain its jargon.
func finish(r row) row {
	if r.Right == "" && r.Byline == "" {
		r.Right = r.Status
	}
	acts := slices.Clone(r.Actions)
	if r.URL != "" {
		acts = append(acts, api.Action{Key: "o", Label: "open on GitHub", Kind: api.ActionOpen})
	}
	r.Actions = append(acts, api.Action{Key: "?", Label: "explain", Kind: api.ActionExplain})
	return r
}

// waitingOnUser reports whether pr belongs on In progress: the user's own,
// not a draft, and not waiting on its author to fix checks or review notes.
func (s *Source) waitingOnUser(pr github.PR) bool {
	if pr.Draft || pr.Checks == github.ChecksFailing || pr.Review == github.ReviewChangesRequested {
		return false
	}
	// Until the gh user is known, every PR in a hot repo counts as theirs.
	return s.login == "" || strings.EqualFold(pr.Author, s.login)
}

func (s *Source) setupRow(tab api.Tab) row {
	body := fmt.Sprintf("lasso shows issues, Actions runs and pull requests from the repos you list as hot, "+
		"and none are listed yet. Add them to %s:\n\n"+
		"    hot_repos = [\"owner/name\", \"owner/other\"]\n\n"+
		"then restart the daemon (stop it, and run lasso again).", s.opts.ConfigPath)
	return row{Focus: api.Focus{
		Item: api.Item{ID: "setup:" + string(tab), Tab: tab, Title: "No hot repos yet: add hot_repos to " + s.opts.ConfigPath, Status: "setup", Body: body},
		Body: body,
	}}
}

func errorRow(repo string, e repoError, tab api.Tab) row {
	return row{group: 0, Focus: api.Focus{
		Item: api.Item{
			ID: errorID(repo) + ":" + string(tab), Tab: tab, Repo: repo,
			Title: "Can't read " + repo + " from GitHub", Status: "github error", Updated: e.since,
		},
		Fields: []api.Field{
			{Name: "What happened", Value: plainError(e.text())},
			{Name: "Since", Value: e.since.UTC().Format(time.RFC3339)},
		},
		Sections: []api.Section{
			{Label: "What happened", Body: plainError(e.text())},
			{Label: "What GitHub said", Body: "```\n" + clip(e.text(), BodyLimit) + "\n```"},
		},
		Body: clip(e.text(), BodyLimit),
	}}
}

func issueRow(is github.Issue) row {
	r := row{Focus: api.Focus{
		Item: api.Item{
			ID: issueID(is), Tab: api.TabIssues, Repo: is.Repo, Title: fmt.Sprintf("#%d %s", is.Number, is.Title),
			URL: is.URL, Updated: is.Updated,
		},
		Body: clip(is.Body, BodyLimit),
	}}
	due, from := "none", "none"
	if !is.Due.IsZero() {
		due = is.Due.Format(time.DateOnly)
		r.Status = "due " + due
		r.Right = "due " + is.Due.Format("Jan 2")
		d := is.Due
		r.Due = &d
		from = dueFrom(is)
		r.group, r.at, r.asc = 1, is.Due, true
		r.Sections = append(r.Sections, api.Section{Label: "Due", Body: is.Due.Format("Mon Jan 2, 2006") + ", from " + from + "."})
	} else {
		r.Right = "no due date"
		r.group, r.at = 2, is.Updated
	}
	if body := strings.TrimSpace(clip(is.Body, BodyLimit)); body != "" {
		r.Sections = append(r.Sections, api.Section{Label: "What it says", Body: body})
	}
	r.Fields = []api.Field{
		{Name: "Due", Value: due},
		{Name: "Due from", Value: from},
		{Name: "Labels", Value: list(is.Labels)},
		{Name: "Assignees", Value: list(is.Assignees)},
	}
	if is.Milestone != "" {
		r.Fields = append(r.Fields, api.Field{Name: "Milestone", Value: is.Milestone})
	}
	return r
}

func dueFrom(is github.Issue) string {
	switch is.DueSource {
	case github.DueProject:
		return "a Due or Target date field on a project"
	case github.DueMilestone:
		return "milestone " + is.Milestone
	case github.DueBody:
		return "a Due: line in the issue body"
	}
	return "none"
}

func runRow(r github.Run) row {
	out := row{Focus: api.Focus{
		Item: api.Item{
			ID: runID(r), Tab: api.TabActions, Repo: r.Repo, Title: r.Workflow + " on " + r.Branch,
			Status: runStatus(r), URL: r.URL, Updated: runTime(r),
		},
	}}
	if r.InProgress() {
		out.group, out.at = 1, r.Started
	} else {
		out.group, out.at = 2, runTime(r)
	}
	out.Fields = []api.Field{
		{Name: "Workflow", Value: r.Workflow},
		{Name: "Branch", Value: r.Branch},
		{Name: "Status", Value: runStatus(r)},
	}
	if r.Attempt > 1 {
		out.Fields = append(out.Fields, api.Field{Name: "Attempt", Value: strconv.Itoa(r.Attempt)})
	}
	if r.FailingJob != "" {
		out.Fields = append(out.Fields, api.Field{Name: "Failing job", Value: r.FailingJob})
	}
	if r.FailingStep != "" {
		out.Fields = append(out.Fields, api.Field{Name: "Failing step", Value: r.FailingStep})
	}
	where := r.Workflow + " on `" + r.Branch + "`"
	if r.Attempt > 1 {
		where += ", attempt " + strconv.Itoa(r.Attempt)
	}
	out.Sections = []api.Section{{Label: "Run", Body: where + ": " + runStatus(r) + "."}}
	if r.Failed() {
		out.Sections = append(out.Sections, api.Section{Label: "What failed", Body: failingSummary(r)})
	}
	out.Fields = append(out.Fields, api.Field{Name: "URL", Value: r.URL})
	return out
}

func runStatus(r github.Run) string {
	if r.InProgress() {
		if r.Status == "queued" || r.Status == "waiting" || r.Status == "pending" {
			return "queued"
		}
		return "running"
	}
	switch {
	case r.Conclusion == "success":
		return "passed"
	case r.Failed():
		return "failed"
	case r.Conclusion == "cancelled":
		return "cancelled"
	case r.Conclusion == "":
		return "finished"
	}
	return strings.ReplaceAll(r.Conclusion, "_", " ")
}

func failingSummary(r github.Run) string {
	switch {
	case r.FailingStep != "":
		return fmt.Sprintf("%s failed at %q in %s.", r.Workflow, r.FailingStep, r.FailingJob)
	case r.FailingJob != "":
		return fmt.Sprintf("%s failed in %s.", r.Workflow, r.FailingJob)
	}
	return r.Workflow + " failed."
}

func prRow(pr github.PR) row {
	return row{group: 2, at: pr.Updated, Focus: api.Focus{
		Item: api.Item{
			ID: prID(pr), Tab: api.TabDoing, Repo: pr.Repo, Title: fmt.Sprintf("#%d %s", pr.Number, pr.Title),
			Status: prStatus(pr), URL: pr.URL, Updated: pr.Updated,
		},
		Fields: []api.Field{
			{Name: "Checks", Value: checksText(pr.Checks)},
			{Name: "Review", Value: reviewText(pr.Review)},
			{Name: "Branch", Value: pr.Branch},
			{Name: "Author", Value: pr.Author},
		},
		Sections: []api.Section{{Label: "Where it stands", Body: fmt.Sprintf(
			"Checks: %s. Review: %s. Branch `%s`, by %s.", checksText(pr.Checks), reviewText(pr.Review), pr.Branch, pr.Author)}},
	}}
}

func prStatus(pr github.PR) string {
	switch {
	case pr.Checks == github.ChecksPending:
		return "checks running"
	case pr.Review == github.ReviewRequired:
		return "waiting on your review"
	}
	return "ready to merge"
}

func checksText(c github.CheckState) string {
	switch c {
	case github.ChecksPassing:
		return "passing"
	case github.ChecksFailing:
		return "failing"
	case github.ChecksPending:
		return "running"
	}
	return "none"
}

func reviewText(r github.ReviewState) string {
	switch r {
	case github.ReviewRequired:
		return "review required"
	case github.ReviewApproved:
		return "approved"
	case github.ReviewChangesRequested:
		return "changes requested"
	}
	return "not required"
}

// plainError says in plain words why a repo can't be read.
func plainError(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "auth login") || strings.Contains(m, "not logged") ||
		strings.Contains(m, "bad credentials") || strings.Contains(m, "http 401"):
		return "gh isn't signed in to GitHub. Run `gh auth login`."
	case strings.Contains(m, "rate limit"):
		return "GitHub's rate limit ran out. lasso will try again after it resets."
	case strings.Contains(m, "partial data"):
		return "GitHub left some fields out, usually project due dates, which need the read:project scope " +
			"(`gh auth refresh -s read:project`). Everything else is shown."
	case strings.Contains(m, "not found") || strings.Contains(m, "could not resolve"):
		return "GitHub can't find this repo, or this account can't see it. Check hot_repos."
	}
	return "lasso couldn't read this repo from GitHub. It will keep trying."
}

// clip cuts s to n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func list(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}
