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
	srv := httptest.NewServer(Handler(NewFakeSource(), Options{Token: testToken, Port: 7337, Actions: fakeActions{}}))
	t.Cleanup(srv.Close)
	cases := []struct {
		name, path, body string
		want             int
		contains         string
	}{
		{"allowed", "/actions", `{"bot":"ok","action":"comment","target":"o/r#1"}`, 200, `"id":"act-1"`},
		{"refused", "/actions", `{"bot":"grok","action":"comment"}`, 403, "No rule lets grok"},
		{"audit down", "/actions", `{"bot":"audit","action":"comment"}`, 503, ""},
		{"adapter failed", "/actions", `{"bot":"fail","action":"comment"}`, 502, ""},
		{"unknown field", "/actions", `{"bot":"ok","sneaky":true}`, 400, "bad action"},
		{"not json", "/actions", `nope`, 400, "bad action"},
		{"undo", "/actions/act-1/undo", ``, 200, `"id":"act-3"`},
		{"can't undo", "/actions/act-2/undo", ``, 409, "Can't undo"},
		{"undo unknown", "/actions/zz/undo", ``, 404, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", srv.URL+c.path, strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer "+testToken)
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
