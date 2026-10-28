package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGH answers the gh calls the poller and the source make, from
// in-memory repos. It fails the test on anything that could write.
type fakeGH struct {
	t     *testing.T
	mu    sync.Mutex
	login string
	repos map[string]*fakeRepo
}

type fakeRepo struct {
	branch string
	issues []map[string]any
	prs    []map[string]any
	runs   []map[string]any
	jobs   map[int64]string // run id -> jobs body
	// fail, when set, is what gh prints to stderr for every call.
	fail string
	// partial, when set, comes back as a GraphQL error beside the data.
	partial string
}

func newFakeGH(t *testing.T) *fakeGH {
	return &fakeGH{t: t, login: "me", repos: map[string]*fakeRepo{}}
}

func (f *fakeGH) repo(name string) *fakeRepo {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.repos[name]
	if r == nil {
		r = &fakeRepo{branch: "main", jobs: map[int64]string{}}
		f.repos[name] = r
	}
	return r
}

func issue(n int, title, body string, updated time.Time, milestoneDue *time.Time) map[string]any {
	m := map[string]any{
		"number": n, "title": title, "body": body, "url": fmt.Sprintf("https://github.com/o/r/issues/%d", n),
		"updatedAt": updated, "labels": map[string]any{"nodes": []any{map[string]string{"name": "bug"}}},
		"assignees":    map[string]any{"nodes": []any{map[string]string{"login": "me"}}},
		"projectItems": map[string]any{"nodes": []any{}},
	}
	if milestoneDue != nil {
		m["milestone"] = map[string]any{"title": "v1", "dueOn": milestoneDue}
	}
	return m
}

func pr(n int, title, author string, draft bool, review, checks string, updated time.Time) map[string]any {
	commit := map[string]any{"statusCheckRollup": nil}
	if checks != "" {
		commit["statusCheckRollup"] = map[string]string{"state": checks}
	}
	return map[string]any{
		"number": n, "title": title, "url": fmt.Sprintf("https://github.com/o/r/pull/%d", n), "updatedAt": updated,
		"isDraft": draft, "headRefName": "feature", "reviewDecision": review, "author": map[string]string{"login": author},
		"commits": map[string]any{"nodes": []any{map[string]any{"commit": commit}}},
	}
}

func run(id int64, name, branch, status, conclusion string, started, updated time.Time) map[string]any {
	var c any
	if conclusion != "" {
		c = conclusion
	}
	return map[string]any{
		"id": id, "name": name, "head_branch": branch, "status": status, "conclusion": c, "run_attempt": 1,
		"run_started_at": started, "updated_at": updated, "html_url": fmt.Sprintf("https://github.com/o/r/actions/runs/%d", id),
	}
}

func httpOut(status int, resource, body string) []byte {
	return fmt.Appendf(nil, "HTTP/2.0 %d OK\r\nContent-Type: application/json\r\n"+
		"X-Ratelimit-Limit: 5000\r\nX-Ratelimit-Remaining: 4900\r\nX-Ratelimit-Reset: 1800000000\r\nX-Ratelimit-Resource: %s\r\n\r\n%s",
		status, resource, body)
}

func (f *fakeGH) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range args {
		if a == "-X" || a == "--method" || a == "--input" || strings.HasPrefix(a, "--method=") {
			f.t.Errorf("gh call could write: %q", args)
		}
	}
	if len(args) < 2 || args[0] != "api" {
		f.t.Errorf("unexpected gh call %q", args)
		return nil, errors.New("unexpected")
	}
	if args[1] == "user" {
		return []byte(f.login + "\n"), nil
	}
	if args[1] != "-i" { // repos/<o>/<n> --jq .default_branch
		r := f.repos[strings.TrimPrefix(args[1], "repos/")]
		if r == nil || r.fail != "" {
			return nil, errors.New("gh: not found")
		}
		return []byte(r.branch + "\n"), nil
	}
	if args[2] == "graphql" {
		var owner, name, query string
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "owner="); ok {
				owner = v
			}
			if v, ok := strings.CutPrefix(a, "name="); ok {
				name = v
			}
			if v, ok := strings.CutPrefix(a, "query="); ok {
				query = v
			}
		}
		r := f.repos[owner+"/"+name]
		if r == nil {
			f.t.Errorf("graphql for unknown repo %s/%s", owner, name)
			return nil, errors.New("unknown repo")
		}
		if r.fail != "" {
			return nil, errors.New(r.fail)
		}
		var data any
		if strings.Contains(query, "milestones(") {
			data = map[string]any{"repository": map[string]any{"milestones": map[string]any{"nodes": []any{}}}}
		} else {
			data = map[string]any{"repository": map[string]any{
				"issues":       map[string]any{"nodes": r.issues},
				"pullRequests": map[string]any{"nodes": r.prs},
			}}
		}
		body := map[string]any{"data": data}
		if r.partial != "" {
			body["errors"] = []any{map[string]string{"message": r.partial}}
		}
		b, _ := json.Marshal(body)
		return httpOut(200, "graphql", string(b)), nil
	}
	path := args[len(args)-1]
	rest, _ := strings.CutPrefix(path, "repos/")
	parts := strings.SplitN(rest, "/", 3)
	r := f.repos[parts[0]+"/"+parts[1]]
	if r == nil {
		f.t.Errorf("REST for unknown repo: %q", path)
		return nil, errors.New("unknown repo")
	}
	if r.fail != "" {
		return nil, errors.New(r.fail)
	}
	if strings.Contains(path, "/jobs") {
		for id, body := range r.jobs {
			if strings.Contains(path, fmt.Sprintf("/runs/%d/", id)) {
				return httpOut(200, "core", body), nil
			}
		}
		return httpOut(404, "core", `{"message":"Not Found"}`), errors.New("gh: Not Found (HTTP 404)")
	}
	b, _ := json.Marshal(map[string]any{"workflow_runs": r.runs})
	return httpOut(200, "core", string(b)), nil
}

// fakeClock is a settable clock; tests call Step themselves.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time                       { return c.now }
func (c *fakeClock) After(time.Duration) <-chan time.Time { return nil }
