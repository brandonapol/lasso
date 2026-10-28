package agentpane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/rules"
)

// client calls the daemon's API with the agent pane's token. It is the
// pane's only way to see or touch anything: the API decides, by that token,
// that every action goes through the rules (never as the user).
type client struct {
	base  string
	token string
	http  *http.Client
}

// errRefused wraps a 403 from POST /actions: the rules said no.
var errRefused = errors.New("refused by your rules")

func (c *client) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if out != nil && json.Valid(b) {
		_ = json.Unmarshal(b, out)
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(b))
		if msg == "" || json.Valid(b) {
			msg = resp.Status
		}
		return resp.StatusCode, fmt.Errorf("%s", msg)
	}
	return resp.StatusCode, nil
}

func (c *client) items(ctx context.Context, tab api.Tab) ([]api.Item, error) {
	var out struct {
		Items []api.Item `json:"items"`
	}
	_, err := c.do(ctx, http.MethodGet, "/items?tab="+url.QueryEscape(string(tab)), nil, &out)
	return out.Items, err
}

func (c *client) focus(ctx context.Context, id string) (api.Focus, error) {
	var f api.Focus
	_, err := c.do(ctx, http.MethodGet, "/items/"+url.PathEscape(id), nil, &f)
	return f, err
}

// show asks the clients to move the focus pane to id.
func (c *client) show(ctx context.Context, id string) (api.Focus, error) {
	var f api.Focus
	_, err := c.do(ctx, http.MethodPost, "/focus", map[string]string{"id": id}, &f)
	return f, err
}

// act runs one action through POST /actions. A refusal comes back as
// errRefused with the rules' plain reason.
func (c *client) act(ctx context.Context, req rules.Request) (rules.Outcome, error) {
	var out rules.Outcome
	code, err := c.do(ctx, http.MethodPost, "/actions", req, &out)
	if code == http.StatusForbidden {
		reason := out.Decision.Reason
		if reason == "" {
			reason = "no rule allows it"
		}
		return out, fmt.Errorf("%w: %s", errRefused, reason)
	}
	if err != nil && out.Detail != "" {
		err = fmt.Errorf("%s", out.Detail)
	}
	return out, err
}
