package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brandonapol/lasso/internal/rules"
)

const agentToken = "agent-s3cret"

type fakeActions struct{}

func (fakeActions) Do(_ context.Context, req rules.Request) (rules.Outcome, error) {
	switch req.Bot {
	case "ok":
		return rules.Outcome{ID: "act-1", Decision: rules.Decision{Allowed: true, Rule: "r"}}, nil
	case "audit":
		return rules.Outcome{}, rules.ErrAudit
	case "fail":
		return rules.Outcome{ID: "act-2"}, rules.ErrFailed
	}
	return rules.Outcome{Decision: rules.Decision{Reason: "No rule lets " + req.Bot}}, rules.ErrRefused
}

func (fakeActions) DoAsUser(_ context.Context, req rules.Request) (rules.Outcome, error) {
	return rules.Outcome{ID: "act-user", Decision: rules.Decision{Allowed: true, Reason: "You approved it."}}, nil
}

func (fakeActions) Undo(_ context.Context, id string) (rules.Outcome, error) {
	switch id {
	case "act-1":
		return rules.Outcome{ID: "act-3"}, nil
	case "act-2":
		return rules.Outcome{Detail: "Can't undo"}, rules.ErrNoUndo
	}
	return rules.Outcome{}, rules.ErrUnknown
}

func TestActions(t *testing.T) {
	srv := httptest.NewServer(Handler(NewFakeSource(), Options{Token: testToken, AgentToken: agentToken, Port: 7337, Actions: fakeActions{}}))
	t.Cleanup(srv.Close)
	cases := []struct {
		name, token, path, body string
		want                    int
		contains                string
	}{
		// The agent pane's actions go through the rules.
		{"allowed", agentToken, "/actions", `{"bot":"ok","action":"comment","target":"o/r#1"}`, 200, `"id":"act-1"`},
		{"refused", agentToken, "/actions", `{"bot":"grok","action":"comment"}`, 403, "No rule lets grok"},
		{"audit down", agentToken, "/actions", `{"bot":"audit","action":"comment"}`, 503, ""},
		{"adapter failed", agentToken, "/actions", `{"bot":"fail","action":"comment"}`, 502, ""},
		// The user's token is the approval, whatever the body says.
		{"user", testToken, "/actions", `{"bot":"grok","action":"comment"}`, 200, `"id":"act-user"`},
		// No body field can claim to be the user.
		{"agent claims user", agentToken, "/actions", `{"bot":"grok","action":"comment","actor":"user"}`, 400, "bad action"},
		{"unknown field", testToken, "/actions", `{"bot":"ok","sneaky":true}`, 400, "bad action"},
		{"not json", testToken, "/actions", `nope`, 400, "bad action"},
		{"undo", testToken, "/actions/act-1/undo", ``, 200, `"id":"act-3"`},
		{"agent can't undo", agentToken, "/actions/act-1/undo", ``, 403, "Only you can undo"},
		{"can't undo", testToken, "/actions/act-2/undo", ``, 409, "Can't undo"},
		{"undo unknown", testToken, "/actions/zz/undo", ``, 404, ""},
		{"no token", "", "/actions", `{"bot":"ok","action":"comment"}`, 401, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", srv.URL+c.path, strings.NewReader(c.body))
			if c.token != "" {
				req.Header.Set("Authorization", "Bearer "+c.token)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != c.want || !strings.Contains(string(b), c.contains) {
				t.Fatalf("got %d %q, want %d containing %q", resp.StatusCode, b, c.want, c.contains)
			}
		})
	}
}

// One token for both callers would let the agent pass as the user, so the
// agent token is dropped and the shared token means the user.
func TestSharedTokenIsNeverAgent(t *testing.T) {
	var got Caller
	inner := guard(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = CallerFrom(r.Context()) }),
		map[Caller]string{CallerUser: testToken, CallerAgent: testToken}, 7337, openPath, nil)
	req := httptest.NewRequest("GET", "http://127.0.0.1:7337/items", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	inner.ServeHTTP(httptest.NewRecorder(), req)
	if got != CallerUser {
		t.Fatalf("caller = %q", got)
	}
}
