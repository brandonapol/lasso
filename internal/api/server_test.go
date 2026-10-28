package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "s3cret"

func newTestServer(t *testing.T) (*httptest.Server, *FakeSource) {
	t.Helper()
	src := NewFakeSource()
	srv := httptest.NewServer(Handler(src, Options{Token: testToken, Port: 7337}))
	t.Cleanup(srv.Close)
	return srv, src
}

func TestGuard(t *testing.T) {
	srv, _ := newTestServer(t)
	cases := []struct {
		name   string
		path   string
		host   string
		origin string
		auth   string
		want   int
	}{
		{"ok", "/items?tab=issues", "", "", "Bearer " + testToken, 200},
		{"localhost host", "/items?tab=issues", "localhost:7337", "", "Bearer " + testToken, 200},
		{"ipv6 loopback host", "/items?tab=issues", "[::1]:7337", "", "Bearer " + testToken, 200},
		{"wrong host", "/items?tab=issues", "evil.example:7337", "", "Bearer " + testToken, 403},
		{"wrong host on healthz", "/healthz", "evil.example", "", "", 403},
		{"own origin", "/items?tab=issues", "", "http://127.0.0.1:7337", "Bearer " + testToken, 200},
		{"own origin localhost", "/items?tab=issues", "", "http://localhost:7337", "Bearer " + testToken, 200},
		{"foreign origin", "/items?tab=issues", "", "http://evil.example", "Bearer " + testToken, 403},
		{"other port origin", "/items?tab=issues", "", "http://127.0.0.1:8080", "Bearer " + testToken, 403},
		{"https origin", "/items?tab=issues", "", "https://127.0.0.1:7337", "Bearer " + testToken, 403},
		{"no token", "/items?tab=issues", "", "", "", 401},
		{"bad token", "/items?tab=issues", "", "", "Bearer nope", 401},
		{"wrong scheme", "/items?tab=issues", "", "", "Basic " + testToken, 401},
		{"healthz needs no token", "/healthz", "", "", "", 200},
		{"events need a token", "/events", "", "", "", 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", srv.URL+c.path, nil)
			if c.host != "" {
				req.Host = c.host
			}
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			if c.auth != "" {
				req.Header.Set("Authorization", c.auth)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
}

func do(t *testing.T, srv *httptest.Server, method, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestEndpoints(t *testing.T) {
	srv, src := newTestServer(t)
	now := time.Now().UTC()
	src.Set(Focus{Item: Item{ID: "i1", Tab: TabIssues, Title: "older", Updated: now.Add(-time.Hour)}, Body: "body 1"})
	src.Set(Focus{Item: Item{ID: "i2", Tab: TabIssues, Title: "newer", Updated: now}})
	src.Set(Focus{Item: Item{ID: "a1", Tab: TabActions, Title: "run"}})

	cases := []struct {
		name     string
		method   string
		path     string
		want     int
		contains string
	}{
		{"issues newest first", "GET", "/items?tab=issues", 200, `"items":[{"id":"i2"`},
		{"empty tab is a list", "GET", "/items?tab=summaries", 200, `"items":[]`},
		{"bad tab", "GET", "/items?tab=nope", 400, "tab must be"},
		{"missing tab", "GET", "/items", 400, "tab must be"},
		{"focus", "GET", "/items/i1", 200, `"body":"body 1"`},
		{"unknown item", "GET", "/items/zz", 404, "no such item"},
		{"actions stubbed", "POST", "/actions", 501, "rules engine not built (#5)"},
		{"wrong method", "DELETE", "/items/i1", 405, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := do(t, srv, c.method, c.path)
			if code != c.want || !strings.Contains(body, c.contains) {
				t.Fatalf("got %d %q, want %d containing %q", code, body, c.want, c.contains)
			}
		})
	}
}

func TestEventsArriveWithinASecond(t *testing.T) {
	srv, src := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	r := bufio.NewReader(resp.Body)
	// The stream opens with a comment once the subscription exists.
	if line, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(line, ":") {
		t.Fatalf("first line = %q, %v", line, err)
	}

	type got struct{ kind, data string }
	events := make(chan got, 8)
	go func() {
		var kind string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				close(events)
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				events <- got{kind, strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	next := func() got {
		t.Helper()
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatal("stream closed")
			}
			return e
		case <-time.After(time.Second):
			t.Fatal("no event within 1s")
		}
		return got{}
	}

	src.Set(Focus{Item: Item{ID: "i1", Tab: TabIssues, Title: "hello"}})
	e := next()
	var it Item
	if e.kind != EventItemChanged || json.Unmarshal([]byte(e.data), &it) != nil || it.ID != "i1" {
		t.Fatalf("got %+v", e)
	}
	src.Remove("i1")
	if e := next(); e.kind != EventItemRemoved || e.data != `{"id":"i1"}` {
		t.Fatalf("got %+v", e)
	}
	src.SetBrief("all quiet")
	if e := next(); e.kind != EventBriefUpdated || e.data != `{"text":"all quiet"}` {
		t.Fatalf("got %+v", e)
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	src := NewFakeSource()
	ch, stop := src.Subscribe()
	defer stop()
	for i := 0; i < 100; i++ {
		src.SetBrief("x")
	}
	n := 0
	for range ch {
		n++
	}
	if n == 0 || n >= 100 {
		t.Fatalf("got %d events before close", n)
	}
}

func TestLoadToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lasso", "token")

	tok, err := LoadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Fatalf("token %q is not 32 bytes of hex", tok)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	again, err := LoadToken(path)
	if err != nil || again != tok {
		t.Fatalf("second load = %q, %v", again, err)
	}

	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o620} {
		os.Chmod(path, mode)
		if _, err := LoadToken(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("mode %v: err = %v", mode, err)
		}
	}
}
