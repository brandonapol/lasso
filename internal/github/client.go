package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cache is where the client keeps ETags and response bodies.
// *store.Store satisfies it.
type Cache interface {
	CacheGet(key string, now time.Time) ([]byte, bool, error)
	CachePut(key string, value []byte, now time.Time, ttl time.Duration) error
}

// etagTTL is how long a REST response is kept for conditional requests.
const etagTTL = 24 * time.Hour

// RateLimit is the last rate limit GitHub reported for one resource
// ("core" for REST, "graphql" for GraphQL).
type RateLimit struct {
	Resource  string
	Limit     int
	Remaining int
	Reset     time.Time
}

// Low reports whether less than 20% of the limit remains.
func (r RateLimit) Low() bool { return r.Limit > 0 && r.Remaining*5 < r.Limit }

// Client makes read-only GitHub requests through gh.
type Client struct {
	runner Runner
	cache  Cache
	now    func() time.Time

	mu     sync.Mutex
	limits map[string]RateLimit
}

// NewClient returns a Client that runs gh through runner and keeps ETags in
// cache (in memory when cache is nil). now is the clock (time.Now when nil).
func NewClient(runner Runner, cache Cache, now func() time.Time) *Client {
	if cache == nil {
		cache = newMemCache()
	}
	if now == nil {
		now = time.Now
	}
	return &Client{runner: runner, cache: cache, now: now, limits: map[string]RateLimit{}}
}

// RateLimit returns the last rate limit seen for resource.
func (c *Client) RateLimit(resource string) RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limits[resource]
}

// response is a parsed `gh api -i` output.
type response struct {
	Status int
	Header http.Header
	Body   []byte
}

// parseResponse splits `gh api -i` output into status, headers and body.
func parseResponse(out []byte) (response, error) {
	var r response
	status, rest, ok := bytes.Cut(out, []byte("\n"))
	if !ok || !bytes.HasPrefix(status, []byte("HTTP/")) {
		return r, errors.New("gh api: no HTTP status line in output")
	}
	fields := strings.Fields(string(status))
	if len(fields) < 2 {
		return r, fmt.Errorf("gh api: bad status line %q", status)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return r, fmt.Errorf("gh api: bad status line %q", status)
	}
	r.Status = code
	r.Header = http.Header{}
	for {
		var line []byte
		line, rest, ok = bytes.Cut(rest, []byte("\n"))
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			break
		}
		if k, v, found := bytes.Cut(line, []byte(":")); found {
			r.Header.Add(string(bytes.TrimSpace(k)), string(bytes.TrimSpace(v)))
		}
		if !ok {
			break
		}
	}
	r.Body = rest
	return r, nil
}

// noteRate records the rate limit headers of a response.
func (c *Client) noteRate(h http.Header) {
	limit, err1 := strconv.Atoi(h.Get("X-Ratelimit-Limit"))
	remaining, err2 := strconv.Atoi(h.Get("X-Ratelimit-Remaining"))
	if err1 != nil || err2 != nil {
		return
	}
	rl := RateLimit{Resource: h.Get("X-Ratelimit-Resource"), Limit: limit, Remaining: remaining}
	if rl.Resource == "" {
		rl.Resource = "core"
	}
	if reset, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
		rl.Reset = time.Unix(reset, 0).UTC()
	}
	c.mu.Lock()
	c.limits[rl.Resource] = rl
	c.mu.Unlock()
}

// etagEntry is a cached REST response.
type etagEntry struct {
	ETag string          `json:"etag"`
	Body json.RawMessage `json:"body"`
}

// get makes a conditional REST GET of path. A 304 returns the cached body.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	key := "github:rest:" + path
	var cached etagEntry
	if b, ok, err := c.cache.CacheGet(key, c.now()); err == nil && ok {
		if json.Unmarshal(b, &cached) != nil {
			cached = etagEntry{}
		}
	}
	args := []string{"api", "-i"}
	if cached.ETag != "" && len(cached.Body) > 0 {
		args = append(args, "-H", "If-None-Match: "+cached.ETag)
	}
	args = append(args, path)
	out, runErr := c.runner.Run(ctx, args...)
	resp, err := parseResponse(out)
	if err != nil {
		if runErr != nil {
			return nil, runErr
		}
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	c.noteRate(resp.Header)
	switch {
	case resp.Status == http.StatusNotModified && len(cached.Body) > 0:
		// gh exits non-zero on a 304; that's expected.
		return cached.Body, nil
	case resp.Status == http.StatusOK && runErr == nil:
		if etag := resp.Header.Get("Etag"); etag != "" && json.Valid(resp.Body) {
			if b, err := json.Marshal(etagEntry{ETag: etag, Body: resp.Body}); err == nil {
				// The cache is for speed only (ADR 0001 §2); a failed write
				// costs one full response next time.
				_ = c.cache.CachePut(key, b, c.now(), etagTTL)
			}
		}
		return resp.Body, nil
	}
	if runErr != nil {
		return nil, runErr
	}
	return nil, fmt.Errorf("GET %s: HTTP %d", path, resp.Status)
}

// PartialError is a GraphQL response that carried data and errors, such as
// a token without the read:project scope asking for project fields. The
// data that came back was used.
type PartialError struct {
	Messages []string
}

func (e *PartialError) Error() string {
	return "github graphql: partial data: " + strings.Join(e.Messages, "; ")
}

// graphql runs a read-only GraphQL query with string variables and decodes
// its data into out. Data that came back with errors is decoded and the
// errors return as a *PartialError.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]string, out any) error {
	args := []string{"api", "-i", "graphql", "-f", "query=" + query}
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		args = append(args, "-f", k+"="+vars[k])
	}
	raw, runErr := c.runner.Run(ctx, args...)
	resp, err := parseResponse(raw)
	if err != nil {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("graphql: %w", err)
	}
	c.noteRate(resp.Header)
	var body struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("graphql: HTTP %d: %w", resp.Status, err)
	}
	var msgs []string
	for _, e := range body.Errors {
		msgs = append(msgs, e.Message)
	}
	if len(body.Data) == 0 || string(body.Data) == "null" {
		if len(msgs) > 0 {
			return fmt.Errorf("graphql: %s", strings.Join(msgs, "; "))
		}
		if runErr != nil {
			return runErr
		}
		return fmt.Errorf("graphql: HTTP %d with no data", resp.Status)
	}
	if err := json.Unmarshal(body.Data, out); err != nil {
		return fmt.Errorf("graphql: %w", err)
	}
	if len(msgs) > 0 {
		return &PartialError{Messages: msgs}
	}
	return nil
}

// memCache is the in-memory Cache used when none is given.
type memCache struct {
	mu sync.Mutex
	m  map[string]memEntry
}

type memEntry struct {
	value   []byte
	expires time.Time
}

func newMemCache() *memCache { return &memCache{m: map[string]memEntry{}} }

func (c *memCache) CacheGet(key string, now time.Time) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || !now.Before(e.expires) {
		return nil, false, nil
	}
	return e.value, true, nil
}

func (c *memCache) CachePut(key string, value []byte, now time.Time, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = memEntry{value: value, expires: now.Add(ttl)}
	return nil
}
