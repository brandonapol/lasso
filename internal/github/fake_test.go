package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeGH is a fake gh: an in-memory GitHub that answers `gh api -i` the way
// the real one prints, and fails the test on anything that could write.
type fakeGH struct {
	t     *testing.T
	calls [][]string

	issues     string // GraphQL data for the issues query
	gqlErrors  string // GraphQL errors array, if any
	milestones string // GraphQL data for the milestones query
	runs       string // REST body for the runs list
	runsETag   string
	jobs       map[string]string // run path fragment "runs/<id>/attempts/<n>" -> REST body

	core, graphql rate
}

type rate struct {
	limit, remaining int
	reset            time.Time
}

func newFakeGH(t *testing.T) *fakeGH {
	return &fakeGH{
		t:          t,
		issues:     `{"repository":{"issues":{"nodes":[]},"pullRequests":{"nodes":[]}}}`,
		milestones: `{"repository":{"milestones":{"nodes":[]}}}`,
		runs:       `{"workflow_runs":[]}`,
		runsETag:   `W/"runs-1"`,
		jobs:       map[string]string{},
		core:       rate{5000, 4900, time.Unix(1_800_000_000, 0)},
		graphql:    rate{5000, 4900, time.Unix(1_800_000_000, 0)},
	}
}

// httpOut renders a response as `gh api -i` prints it.
func httpOut(status int, r rate, resource string, headers map[string]string, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/2.0 %d %s\r\n", status, map[int]string{200: "OK", 304: "Not Modified", 404: "Not Found"}[status])
	fmt.Fprintf(&b, "Content-Type: application/json; charset=utf-8\r\n")
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	fmt.Fprintf(&b, "X-Ratelimit-Limit: %d\r\nX-Ratelimit-Remaining: %d\r\nX-Ratelimit-Reset: %d\r\nX-Ratelimit-Resource: %s\r\n",
		r.limit, r.remaining, r.reset.Unix(), resource)
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

func (f *fakeGH) Run(_ context.Context, args ...string) ([]byte, error) {
	f.t.Helper()
	f.calls = append(f.calls, args)
	if len(args) < 3 || args[0] != "api" || args[1] != "-i" {
		f.t.Fatalf("unexpected gh call %q", args)
	}
	for _, a := range args {
		if a == "-X" || a == "--method" || a == "--input" || strings.HasPrefix(a, "--method=") {
			f.t.Fatalf("gh call could write: %q", args)
		}
	}
	if args[2] == "graphql" {
		var query string
		for _, a := range args {
			if q, ok := strings.CutPrefix(a, "query="); ok {
				query = q
			}
		}
		if !strings.HasPrefix(strings.TrimSpace(query), "query") {
			f.t.Fatalf("graphql call is not a query: %q", query)
		}
		data := f.issues
		if strings.Contains(query, "milestones(") {
			data = f.milestones
		}
		body := `{"data":` + data
		if f.gqlErrors != "" {
			body += `,"errors":` + f.gqlErrors
		}
		body += "}"
		return []byte(httpOut(200, f.graphql, "graphql", nil, body)), nil
	}
	path := args[len(args)-1]
	ifNoneMatch := ""
	if i := slices.Index(args, "-H"); i >= 0 {
		ifNoneMatch, _ = strings.CutPrefix(args[i+1], "If-None-Match: ")
	}
	switch {
	case strings.Contains(path, "/jobs"):
		for frag, body := range f.jobs {
			if strings.Contains(path, frag+"/jobs") {
				return []byte(httpOut(200, f.core, "core", map[string]string{"Etag": `"jobs-` + frag + `"`}, body)), nil
			}
		}
		return []byte(httpOut(404, f.core, "core", nil, `{"message":"Not Found"}`)), errors.New("gh: Not Found (HTTP 404)")
	case strings.Contains(path, "/actions/runs"):
		if ifNoneMatch != "" && ifNoneMatch == f.runsETag {
			// Real gh prints the 304's headers and exits 1.
			return []byte(httpOut(304, f.core, "core", map[string]string{"Etag": f.runsETag}, "")), errors.New("gh api: exit status 1: gh: HTTP 304")
		}
		return []byte(httpOut(200, f.core, "core", map[string]string{"Etag": f.runsETag}, f.runs)), nil
	}
	f.t.Fatalf("unexpected gh path %q", path)
	return nil, nil
}

// count returns how many calls so far contain substr in some arg.
func (f *fakeGH) count(substr string) int {
	n := 0
	for _, c := range f.calls {
		if slices.ContainsFunc(c, func(a string) bool { return strings.Contains(a, substr) }) {
			n++
		}
	}
	return n
}

// fakeClock is a settable clock.
type fakeClock struct {
	now   time.Time
	waits []time.Duration
	// after, when set, sees each wait; returning false makes the wait
	// never end.
	after func(d time.Duration) bool
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	if c.after != nil && !c.after(d) {
		return nil
	}
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

var t0 = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
