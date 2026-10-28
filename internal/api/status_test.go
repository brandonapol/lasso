package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/rules"
)

func TestStatusAndFields(t *testing.T) {
	srv, src := newTestServer(t)
	if code, body := do(t, srv, "GET", "/status"); code != 200 || !strings.Contains(body, `"bots":[]`) || strings.Contains(body, "github_synced") {
		t.Fatalf("empty status = %d %q", code, body)
	}
	synced := time.Date(2026, 10, 27, 14, 56, 0, 0, time.UTC)
	src.SetStatus(Status{Bots: []BotStatus{{Kind: "claude", Count: 2, State: BotWorking}}, GitHubSynced: &synced})
	code, body := do(t, srv, "GET", "/status")
	var st Status
	if code != 200 || json.Unmarshal([]byte(body), &st) != nil || len(st.Bots) != 1 || st.Bots[0].Count != 2 ||
		st.GitHubSynced == nil || !st.GitHubSynced.Equal(synced) {
		t.Fatalf("status = %d %q", code, body)
	}

	due := synced.Add(72 * time.Hour)
	src.Set(Focus{
		Item:     Item{ID: "s1", Tab: TabSummaries, Title: "sum", Right: "due Oct 30", Due: &due, Byline: "written by m", Body: "card text"},
		Sections: []Section{{Label: "What happened", Body: "**a lot**"}},
		Take:     "fine",
		Actions:  []Action{{Key: "g", Label: "regenerate", Kind: "regenerate_summary", Request: &rules.Request{Action: "regenerate_summary", Repo: "o/r"}}},
		Body:     "focus text",
	})
	_, body = do(t, srv, "GET", "/items?tab=summaries")
	for _, want := range []string{`"right":"due Oct 30"`, `"byline":"written by m"`, `"body":"card text"`, `"due":"2026-10-30T14:56:00Z"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("items lack %s: %s", want, body)
		}
	}
	_, body = do(t, srv, "GET", "/items/s1")
	var f Focus
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		t.Fatal(err)
	}
	if f.Body != "focus text" || f.Take != "fine" || len(f.Sections) != 1 || len(f.Actions) != 1 ||
		f.Actions[0].Request == nil || f.Actions[0].Request.Repo != "o/r" {
		t.Fatalf("focus = %s", body)
	}
}

// A Source that doesn't know the status still answers GET /status.
func TestStatusWithoutStatusSource(t *testing.T) {
	type plain struct{ Source }
	srv := httptest.NewServer(Handler(plain{NewFakeSource()}, Options{Token: testToken, Port: 7337}))
	t.Cleanup(srv.Close)
	if code, body := do(t, srv, "GET", "/status"); code != 200 || strings.TrimSpace(body) != `{"bots":[]}` {
		t.Fatalf("status = %d %q", code, body)
	}
}

func TestStatusChangedEvent(t *testing.T) {
	srv, src := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	src.SetStatus(Status{Bots: []BotStatus{{Kind: "grok", State: BotIdle}}})
	done := make(chan string, 1)
	go func() {
		var kind string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			if strings.HasPrefix(line, "event: ") {
				kind = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				done <- kind + " " + strings.TrimPrefix(line, "data: ")
				return
			}
		}
	}()
	select {
	case got := <-done:
		if got != `status.changed {"bots":[{"kind":"grok","count":0,"state":"idle"}]}` {
			t.Fatalf("got %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no status.changed within 1s")
	}
}
