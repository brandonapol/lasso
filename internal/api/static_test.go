package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoChat is a Chat that records who reached it.
type echoChat struct{ msgs []ChatMessage }

func (c *echoChat) History(context.Context) ([]ChatMessage, error) { return c.msgs, nil }
func (c *echoChat) Open(context.Context) ([]ChatMessage, error)    { return c.msgs, nil }
func (c *echoChat) Send(_ context.Context, text string) ([]ChatMessage, error) {
	c.msgs = append(c.msgs, ChatMessage{Role: RoleUser, Text: text})
	return c.msgs, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type webTest struct {
	t   *testing.T
	srv *httptest.Server
	clk *clock
}

func newWebTest(t *testing.T) *webTest {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	src := NewFakeSource()
	src.Set(Focus{Item: Item{ID: "issue:1", Tab: TabIssues, Title: "one"}})
	srv := httptest.NewServer(Handler(src, Options{
		Token: "user-tok", AgentToken: "agent-tok", Port: 7337, KeepAlive: time.Hour,
		Chat: &echoChat{}, Now: clk.now, HotRepos: []string{"brandonapol/lasso"},
	}))
	t.Cleanup(srv.Close)
	return &webTest{t: t, srv: srv, clk: clk}
}

// req sends one request; hdr holds header pairs.
func (w *webTest) req(method, path, body string, hdr ...string) *http.Response {
	w.t.Helper()
	req, _ := http.NewRequest(method, w.srv.URL+path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
			continue
		}
		req.Header.Set(hdr[i], hdr[i+1])
	}
	// Don't follow /login's redirect, so its Set-Cookie can be read.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (w *webTest) code(tok string) (string, int) {
	w.t.Helper()
	resp := w.req("POST", "/session/code", "", "Authorization", "Bearer "+tok)
	var out struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Code, resp.StatusCode
}

// login trades a fresh code for the session cookie's "name=value".
func (w *webTest) login() string {
	w.t.Helper()
	code, st := w.code("user-tok")
	if st != 200 || code == "" {
		w.t.Fatalf("mint code: %d %q", st, code)
	}
	resp := w.req("GET", "/login?code="+code, "")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		w.t.Fatalf("login: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	cs := resp.Cookies()
	if len(cs) != 1 {
		w.t.Fatalf("cookies = %v", cs)
	}
	return cs[0].Name + "=" + cs[0].Value
}

func TestStaticFilesServed(t *testing.T) {
	w := newWebTest(t)
	for _, c := range []struct{ path, ctype, has string }{
		{"/", "text/html", "<title>lasso</title>"},
		{"/web/app.js", "javascript", "EventSource"},
		{"/web/app.css", "text/css", "#0e1014"},
	} {
		resp := w.req("GET", c.path, "") // no token: the files hold nothing private
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), c.ctype) || !strings.Contains(string(b), c.has) {
			t.Fatalf("%s: %d %q", c.path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "connect-src 'self'") {
			t.Fatalf("%s: CSP %q", c.path, csp)
		}
	}
	if resp := w.req("GET", "/web/", ""); resp.StatusCode != 404 {
		t.Fatalf("directory listing: %d", resp.StatusCode)
	}
	if resp := w.req("GET", "/web/nope.js", ""); resp.StatusCode != 404 {
		t.Fatalf("missing file: %d", resp.StatusCode)
	}
	if resp := w.req("GET", "/", "", "Host", "evil.example"); resp.StatusCode != 403 {
		t.Fatalf("foreign host: %d", resp.StatusCode)
	}
}

func TestCodeExchange(t *testing.T) {
	w := newWebTest(t)
	code, _ := w.code("user-tok")
	resp := w.req("GET", "/login?code="+code, "")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first use: %d", resp.StatusCode)
	}
	sc := resp.Header.Get("Set-Cookie")
	for _, want := range []string{"lasso_session_7337=", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(sc, want) {
			t.Fatalf("Set-Cookie %q lacks %q", sc, want)
		}
	}
	if strings.Contains(strings.ToLower(sc), "domain=") {
		t.Fatalf("cookie must be host-only: %q", sc)
	}
	if strings.Contains(sc, "user-tok") || strings.Contains(resp.Header.Get("Location"), "user-tok") {
		t.Fatal("the bearer token leaked into the cookie or redirect")
	}
	// Once only.
	if resp := w.req("GET", "/login?code="+code, ""); resp.StatusCode != 403 || len(resp.Cookies()) != 0 {
		t.Fatalf("second use: %d", resp.StatusCode)
	}
	// Expires.
	code, _ = w.code("user-tok")
	w.clk.add(codeTTL + time.Second)
	if resp := w.req("GET", "/login?code="+code, ""); resp.StatusCode != 403 {
		t.Fatalf("expired code: %d", resp.StatusCode)
	}
	for _, bad := range []string{"", "nope"} {
		if resp := w.req("GET", "/login?code="+bad, ""); resp.StatusCode != 403 {
			t.Fatalf("code %q: %d", bad, resp.StatusCode)
		}
	}
	// Only the user's token mints codes: not the agent pane, not nobody,
	// and not a web session.
	if _, st := w.code("agent-tok"); st != 403 {
		t.Fatalf("agent minted a code: %d", st)
	}
	if resp := w.req("POST", "/session/code", ""); resp.StatusCode != 401 {
		t.Fatalf("no token minted a code: %d", resp.StatusCode)
	}
	cookie := w.login()
	if resp := w.req("POST", "/session/code", "", "Cookie", cookie, "Origin", "http://127.0.0.1:7337"); resp.StatusCode != 403 {
		t.Fatalf("session minted a code: %d", resp.StatusCode)
	}
}

func TestCookieActsAsUser(t *testing.T) {
	w := newWebTest(t)
	cookie := w.login()
	own := "http://127.0.0.1:7337"

	for _, path := range []string{"/items?tab=issues", "/items/issue:1", "/status", "/chat"} {
		if resp := w.req("GET", path, "", "Cookie", cookie, "Sec-Fetch-Site", "same-origin"); resp.StatusCode != 200 {
			t.Fatalf("GET %s with cookie: %d", path, resp.StatusCode)
		}
	}
	resp := w.req("GET", "/status", "", "Cookie", cookie)
	var st Status
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if len(st.HotRepos) != 1 || st.HotRepos[0] != "brandonapol/lasso" {
		t.Fatalf("status hot repos = %v", st.HotRepos)
	}
	// The chat is user-only; the cookie gets through it.
	resp = w.req("POST", "/chat", `{"text":"hi"}`, "Cookie", cookie, "Origin", own, "Sec-Fetch-Site", "same-origin")
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || !strings.Contains(string(b), `"hi"`) {
		t.Fatalf("POST /chat with cookie: %d %s", resp.StatusCode, b)
	}
	// The event stream takes the cookie (EventSource can't send a header).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", w.srv.URL+"/events", nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	er, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer er.Body.Close()
	if er.StatusCode != 200 || er.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events with cookie: %d %q", er.StatusCode, er.Header.Get("Content-Type"))
	}

	// Sessions expire.
	w.clk.add(sessionTTL + time.Second)
	if resp := w.req("GET", "/status", "", "Cookie", cookie); resp.StatusCode != 401 {
		t.Fatalf("expired session: %d", resp.StatusCode)
	}
}

func TestCookieRefusedCrossOrigin(t *testing.T) {
	w := newWebTest(t)
	cookie := w.login()
	cases := []struct {
		name, method, path string
		hdr                []string
		want               int
	}{
		{"foreign origin", "GET", "/items?tab=issues", []string{"Origin", "http://evil.example"}, 403},
		{"other local port", "POST", "/chat", []string{"Origin", "http://127.0.0.1:8080"}, 403},
		{"foreign origin posts", "POST", "/chat", []string{"Origin", "https://evil.example"}, 403},
		// Cookies ignore ports, so a page on another 127.0.0.1 port is
		// "same-site" and the browser sends the cookie; it's refused.
		{"same-site subresource", "GET", "/items?tab=issues", []string{"Sec-Fetch-Site", "same-site"}, 401},
		{"cross-site navigation", "GET", "/items?tab=issues", []string{"Sec-Fetch-Site", "cross-site"}, 401},
		{"change without origin", "POST", "/chat", nil, 401},
		{"no cookie", "GET", "/items?tab=issues", []string{"Cookie", ""}, 401},
		{"bad cookie", "GET", "/items?tab=issues", []string{"Cookie", "lasso_session_7337=nope"}, 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hdr := append([]string{"Cookie", cookie}, c.hdr...)
			if resp := w.req(c.method, c.path, `{"text":"yes"}`, hdr...); resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
}

func TestAgentStillCantChat(t *testing.T) {
	w := newWebTest(t)
	cookie := w.login()
	// The agent token, even alongside a user's cookie, is the agent: a
	// bearer token decides the caller before any cookie is looked at.
	for _, hdr := range [][]string{
		{"Authorization", "Bearer agent-tok"},
		{"Authorization", "Bearer agent-tok", "Cookie", cookie, "Origin", "http://127.0.0.1:7337"},
	} {
		if resp := w.req("POST", "/chat", `{"text":"yes"}`, hdr...); resp.StatusCode != 403 {
			t.Fatalf("agent POST /chat (%v): %d", hdr, resp.StatusCode)
		}
	}
	if resp := w.req("POST", "/logout", "", "Cookie", cookie, "Origin", "http://127.0.0.1:7337"); resp.StatusCode != 204 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp := w.req("GET", "/status", "", "Cookie", cookie); resp.StatusCode != 401 {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
}
