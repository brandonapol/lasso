package summary

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/github"
)

// Item is one merged PR, closed or opened issue, or release. It carries
// titles only: never bodies, comments or diffs.
type Item struct {
	Number int       `json:"number,omitempty"`
	Title  string    `json:"title"`
	URL    string    `json:"url"`
	Author string    `json:"author,omitempty"`
	At     time.Time `json:"at"`
}

// Activity is one repo's last 24 hours, as GitHub reports it.
type Activity struct {
	DefaultBranch string
	// Merged, Closed and Opened are the most recent items (up to a page);
	// the Total fields are GitHub's exact counts, which may be larger.
	Merged, Closed, Opened                []Item
	MergedTotal, ClosedTotal, OpenedTotal int
	// Releases published in the window.
	Releases []Item
	// Runs are Actions runs on the default branch started in the window,
	// newest first.
	Runs []github.Run
}

// Fetcher reads a repo's activity since a time.
type Fetcher interface {
	Activity(ctx context.Context, repo string, since time.Time) (Activity, error)
}

// activityQuery reads up to 30 merged PRs, closed issues and opened issues
// (titles, links, authors and times only: no bodies), GitHub's exact totals
// for each, the default branch and recent releases.
const activityQuery = `query($owner: String!, $name: String!, $merged: String!, $closed: String!, $opened: String!) {
  merged: search(query: $merged, type: ISSUE, first: 30) {
    issueCount
    nodes { ... on PullRequest { number title url mergedAt author { login } } }
  }
  closed: search(query: $closed, type: ISSUE, first: 30) {
    issueCount
    nodes { ... on Issue { number title url closedAt author { login } } }
  }
  opened: search(query: $opened, type: ISSUE, first: 30) {
    issueCount
    nodes { ... on Issue { number title url createdAt author { login } } }
  }
  repository(owner: $owner, name: $name) {
    defaultBranchRef { name }
    releases(first: 20, orderBy: {field: CREATED_AT, direction: DESC}) {
      nodes { name tagName url publishedAt isDraft author { login } }
    }
  }
}`

type gqlNode struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	URL       string     `json:"url"`
	MergedAt  *time.Time `json:"mergedAt"`
	ClosedAt  *time.Time `json:"closedAt"`
	CreatedAt *time.Time `json:"createdAt"`
	Author    *struct {
		Login string `json:"login"`
	} `json:"author"`
}

type gqlSearch struct {
	IssueCount int       `json:"issueCount"`
	Nodes      []gqlNode `json:"nodes"`
}

type gqlActivity struct {
	Data *struct {
		Merged     gqlSearch `json:"merged"`
		Closed     gqlSearch `json:"closed"`
		Opened     gqlSearch `json:"opened"`
		Repository *struct {
			DefaultBranchRef *struct {
				Name string `json:"name"`
			} `json:"defaultBranchRef"`
			Releases struct {
				Nodes []struct {
					Name        string     `json:"name"`
					TagName     string     `json:"tagName"`
					URL         string     `json:"url"`
					PublishedAt *time.Time `json:"publishedAt"`
					IsDraft     bool       `json:"isDraft"`
					Author      *struct {
						Login string `json:"login"`
					} `json:"author"`
				} `json:"nodes"`
			} `json:"releases"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// GH reads activity through gh (one GraphQL query per repo) and Actions
// runs from the poller's cached snapshot, so it adds no REST calls.
type GH struct {
	Runner github.Runner
	// Cache holds the poller's snapshots (*store.Store).
	Cache github.Cache
	// Now defaults to time.Now.
	Now func() time.Time
}

// Activity implements Fetcher.
func (g GH) Activity(ctx context.Context, repo string, since time.Time) (Activity, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Activity{}, fmt.Errorf("summary: repo %q: want owner/name", repo)
	}
	stamp := since.UTC().Format("2006-01-02T15:04:05Z")
	vars := map[string]string{
		"owner":  owner,
		"name":   name,
		"merged": fmt.Sprintf("repo:%s is:pr is:merged merged:>=%s sort:updated-desc", repo, stamp),
		"closed": fmt.Sprintf("repo:%s is:issue is:closed closed:>=%s sort:updated-desc", repo, stamp),
		"opened": fmt.Sprintf("repo:%s is:issue created:>=%s sort:created-desc", repo, stamp),
	}
	args := []string{"api", "graphql", "-f", "query=" + activityQuery}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-f", k+"="+vars[k])
	}
	out, runErr := g.Runner.Run(ctx, args...)
	var data gqlActivity
	if err := json.Unmarshal(out, &data); err != nil || data.Data == nil {
		if runErr != nil {
			return Activity{}, fmt.Errorf("summary: %s: %w", repo, runErr)
		}
		var msgs []string
		for _, e := range data.Errors {
			msgs = append(msgs, e.Message)
		}
		return Activity{}, fmt.Errorf("summary: %s: no data (%v %s)", repo, err, strings.Join(msgs, "; "))
	}
	d := data.Data
	a := Activity{
		Merged: items(d.Merged.Nodes, func(n gqlNode) *time.Time { return n.MergedAt }),
		Closed: items(d.Closed.Nodes, func(n gqlNode) *time.Time { return n.ClosedAt }),
		Opened: items(d.Opened.Nodes, func(n gqlNode) *time.Time { return n.CreatedAt }),
	}
	a.MergedTotal, a.ClosedTotal, a.OpenedTotal = d.Merged.IssueCount, d.Closed.IssueCount, d.Opened.IssueCount
	if r := d.Repository; r != nil {
		if r.DefaultBranchRef != nil {
			a.DefaultBranch = r.DefaultBranchRef.Name
		}
		for _, n := range r.Releases.Nodes {
			if n.IsDraft || n.PublishedAt == nil || n.PublishedAt.Before(since) {
				continue
			}
			it := Item{Title: firstNonEmpty(n.Name, n.TagName), URL: n.URL, At: *n.PublishedAt}
			if n.Author != nil {
				it.Author = n.Author.Login
			}
			a.Releases = append(a.Releases, it)
		}
	}
	if g.Cache != nil && a.DefaultBranch != "" {
		snap, ok, err := github.LoadSnapshot(g.Cache, repo, now(g.Now))
		if err != nil {
			return a, fmt.Errorf("summary: %s snapshot: %w", repo, err)
		}
		if ok {
			a.Runs = defaultBranchRuns(snap.Runs, a.DefaultBranch, since)
		}
	}
	return a, nil
}

// Login is the gh user's login: "you" in the summary.
func (g GH) Login(ctx context.Context) (string, error) {
	out, err := g.Runner.Run(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return "", fmt.Errorf("summary: who am I: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func items(nodes []gqlNode, at func(gqlNode) *time.Time) []Item {
	var out []Item
	for _, n := range nodes {
		if n.URL == "" { // a search hit of the other type
			continue
		}
		it := Item{Number: n.Number, Title: n.Title, URL: n.URL}
		if t := at(n); t != nil {
			it.At = *t
		}
		if n.Author != nil {
			it.Author = n.Author.Login
		}
		out = append(out, it)
	}
	return out
}

// defaultBranchRuns keeps runs on branch started at or after since, newest
// first.
func defaultBranchRuns(runs []github.Run, branch string, since time.Time) []github.Run {
	var out []github.Run
	for _, r := range runs {
		if r.Branch == branch && !r.Started.Before(since) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// stillRed returns the workflows whose latest completed run in runs (newest
// first) failed, sorted by name.
func stillRed(runs []github.Run) []string {
	seen := map[string]bool{}
	var red []string
	for _, r := range runs {
		if r.InProgress() || seen[r.Workflow] {
			continue
		}
		seen[r.Workflow] = true
		if r.Failed() {
			red = append(red, r.Workflow)
		}
	}
	sort.Strings(red)
	return red
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x = strings.TrimSpace(x); x != "" {
			return x
		}
	}
	return ""
}

func now(f func() time.Time) time.Time {
	if f == nil {
		return time.Now()
	}
	return f()
}
