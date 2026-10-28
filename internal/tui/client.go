package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/brandonapol/lasso/internal/api"
)

// Client talks to the daemon's local API (ADR 0001 §1). It is the TUI's only
// source of data.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a client for the API at base (e.g.
// "http://127.0.0.1:7337") that sends token as its bearer token. A nil hc
// means a client with no overall timeout, which the event stream needs.
func NewClient(base, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: hc}
}

// Items returns one tab's rows.
func (c *Client) Items(ctx context.Context, tab api.Tab) ([]api.Item, error) {
	var out struct {
		Items []api.Item `json:"items"`
	}
	if err := c.getJSON(ctx, "/items?tab="+url.QueryEscape(string(tab)), &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Focus returns one item with its body.
func (c *Client) Focus(ctx context.Context, id string) (api.Focus, error) {
	var f api.Focus
	err := c.getJSON(ctx, "/items/"+url.PathEscape(id), &f)
	return f, err
}

// Action asks the daemon to take one action on an item, e.g. regenerate a
// summary. The daemon decides whether it may.
func (c *Client) Action(ctx context.Context, kind, id string) error {
	body, err := json.Marshal(map[string]string{"kind": kind, "id": id})
	if err != nil {
		return err
	}
	req, err := c.request(ctx, "POST", "/actions", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return statusErr(resp)
}

// Event is one change from GET /events, its data still JSON.
type Event struct {
	Kind string
	Data json.RawMessage
}

// Events reads GET /events until ctx ends or the stream closes, calling
// onOpen once connected and fn for each event. It always returns an error
// saying why it stopped.
func (c *Client) Events(ctx context.Context, onOpen func(), fn func(Event)) error {
	req, err := c.request(ctx, "GET", "/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusErr(resp); err != nil {
		return err
	}
	if onOpen != nil {
		onOpen()
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var kind string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if kind != "" || len(data) > 0 {
				fn(Event{Kind: kind, Data: json.RawMessage(strings.Join(data, "\n"))})
			}
			kind, data = "", nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return io.EOF
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := c.request(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusErr(resp); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return req, nil
}

// statusErr turns a non-2xx response into an error carrying the server's
// one-line reason.
func statusErr(resp *http.Response) error {
	if resp.StatusCode/100 == 2 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("%s", msg)
}
