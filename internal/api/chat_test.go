package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatEndpoints(t *testing.T) {
	src := NewFakeSource()
	src.Set(Focus{Item: Item{ID: "issue:1", Tab: TabIssues, Title: "one"}})
	srv := httptest.NewServer(Handler(src, Options{Token: "u", AgentToken: "a", Port: 7337, KeepAlive: time.Hour}))
	defer srv.Close()
	do := func(method, path, tok, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// No chat wired: the user hears so; the agent pane may never post as the user.
	if code := do("POST", "/chat", "u", `{"text":"hi"}`); code != http.StatusNotImplemented {
		t.Fatalf("no chat: %d", code)
	}
	for _, p := range []string{"/chat", "/chat/open"} {
		if code := do("POST", p, "a", `{"text":"yes"}`); code != http.StatusForbidden {
			t.Fatalf("agent POST %s: %d", p, code)
		}
	}
	if code := do("GET", "/chat", "a", ""); code != http.StatusForbidden {
		t.Fatalf("agent GET /chat: %d", code)
	}
	if code := do("POST", "/focus", "a", `{"id":"nope"}`); code != http.StatusNotFound {
		t.Fatalf("focus unknown: %d", code)
	}

	// A focus request reaches the event stream.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/events", nil)
	req.Header.Set("Authorization", "Bearer u")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Scan() // ": lasso events"
	if code := do("POST", "/focus", "a", `{"id":"issue:1"}`); code != http.StatusOK {
		t.Fatalf("focus: %d", code)
	}
	var got []string
	for sc.Scan() && len(got) < 2 {
		if l := sc.Text(); l != "" {
			got = append(got, l)
		}
	}
	if strings.Join(got, "\n") != "event: focus\ndata: {\"id\":\"issue:1\",\"tab\":\"issues\"}" {
		t.Fatalf("events = %q", got)
	}
}
