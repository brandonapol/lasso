package source

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/github"
)

// GitHub errors are tracked per repo and per poll group (issues and PRs,
// milestones, runs), because the groups are polled on different cadences:
// a runs poll succeeding says nothing about the GraphQL query that failed
// a minute ago. A repo's error item lasts while any group is failing and
// keeps the time the first one failed, so a repo failing every poll stays
// one brief item instead of becoming a new one each time.

// The poll groups, as named in github's error prefixes ("owner/name issues:").
const (
	groupIssues     = "issues"
	groupMilestones = "milestones"
	groupRuns       = "runs"
)

// repoError is what is failing in one repo.
type repoError struct {
	since  time.Time
	groups map[string]string // group -> error text
}

// text is every failing group's error, in a stable order.
func (e repoError) text() string {
	keys := make([]string, 0, len(e.groups))
	for k := range e.groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	msgs := make([]string, 0, len(keys))
	for _, k := range keys {
		msgs = append(msgs, e.groups[k])
	}
	return strings.Join(msgs, "\n")
}

// recordingRunner notes which poll groups made gh calls.
type recordingRunner struct {
	r      github.Runner
	called map[string]bool
}

func (c recordingRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	c.called[callGroup(args)] = true
	return c.r.Run(ctx, args...)
}

// callGroup tells which poll group a gh call belongs to.
func callGroup(args []string) string {
	if len(args) > 2 && args[2] == "graphql" {
		for _, a := range args {
			if strings.HasPrefix(a, "query=") && strings.Contains(a, "milestones(") {
				return groupMilestones
			}
		}
		return groupIssues
	}
	return groupRuns
}

// errGroup tells which poll group an error came from by its prefix.
func errGroup(repo string, err error) string {
	rest, _ := strings.CutPrefix(err.Error(), repo+" ")
	switch {
	case strings.HasPrefix(rest, "milestones:"):
		return groupMilestones
	case strings.HasPrefix(rest, "runs:"), strings.HasPrefix(rest, "run "):
		return groupRuns
	}
	return groupIssues
}

// leaves splits an errors.Join into its parts.
func leaves(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range j.Unwrap() {
			out = append(out, leaves(e)...)
		}
		return out
	}
	return []error{err}
}

// settle updates repo's error item after a poll: groups that made calls and
// didn't fail are cleared, groups that failed are set.
func (s *Source) settle(repo string, called map[string]bool, err error) {
	failed := map[string]string{}
	for _, e := range leaves(err) {
		g := errGroup(repo, e)
		if failed[g] != "" {
			failed[g] += "; "
		}
		failed[g] += e.Error()
	}
	prev, had := s.errs[repo]
	before := prev.text()
	groups := map[string]string{}
	for g, msg := range prev.groups {
		if !called[g] {
			groups[g] = msg // not polled this time: still failing as far as we know
		}
	}
	for g, msg := range failed {
		groups[g] = msg
	}
	if len(groups) == 0 {
		if had {
			delete(s.errs, repo)
			s.logf("lasso: github: %s: reading again", repo)
		}
		return
	}
	since := prev.since
	if !had {
		since = s.opts.Clock.Now()
	}
	e := repoError{since: since, groups: groups}
	s.errs[repo] = e
	if e.text() != before {
		s.logf("lasso: github: %s: %s", repo, strings.ReplaceAll(e.text(), "\n", "; "))
	}
}
