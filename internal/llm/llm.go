// Package llm is lasso's minimal client for Anthropic's Messages API
// (POST /v1/messages), over plain net/http. It does one thing: send a cached
// system prompt plus one user message and return the reply's text, after
// checking a per-feature daily spending cap kept in lasso's database
// (ADR 0001 §7).
//
// The API key comes from ANTHROPIC_API_KEY, else ~/.config/lasso/anthropic_key
// (which must be mode 0600). It is never logged or printed.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/store"
)

const (
	// Haiku is the model the digest uses.
	Haiku = "claude-haiku-4-5-20251001"

	defaultBaseURL = "https://api.anthropic.com"
	apiVersion     = "2023-06-01"
	// KeyFile is the key's file name inside lasso's config directory.
	KeyFile = "anthropic_key"
)

// Price is a model's list price in USD per million tokens.
type Price struct {
	Input, Output, CacheWrite, CacheRead float64
}

// Prices are Anthropic's published per-token list prices. Source:
// https://docs.anthropic.com/en/docs/about-claude/pricing (Claude Haiku 4.5:
// $1 input, $5 output per MTok; 5-minute cache writes are 1.25x input and
// cache reads 0.1x input). Update this table when prices change; a model
// missing from it can't be used, so spending is never under-counted.
var Prices = map[string]Price{
	Haiku: {Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.10},
}

// Usage is the token counts the API reports for one call.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// Cost is what u cost at p, in USD.
func (p Price) Cost(u Usage) float64 {
	return (float64(u.InputTokens)*p.Input +
		float64(u.OutputTokens)*p.Output +
		float64(u.CacheCreationInputTokens)*p.CacheWrite +
		float64(u.CacheReadInputTokens)*p.CacheRead) / 1_000_000
}

// ErrOverCap means today's spending for the client's feature has reached its
// daily cap. Callers fall back to local text and say the cap is reached.
var ErrOverCap = errors.New("llm: over the daily cap")

// ErrNoKey means no API key is configured.
var ErrNoKey = errors.New("llm: no Anthropic API key (set ANTHROPIC_API_KEY)")

// SpendTracker keeps one feature's spending per day (YYYY-MM-DD), in USD.
type SpendTracker interface {
	Spent(day string) (float64, error)
	AddSpend(day string, usd float64) error
}

// StoreSpend tracks spending in lasso's database under Feature.
type StoreSpend struct {
	S       *store.Store
	Feature string
}

// Spent implements SpendTracker.
func (s StoreSpend) Spent(day string) (float64, error) { return s.S.Spent(s.Feature, day) }

// AddSpend implements SpendTracker.
func (s StoreSpend) AddSpend(day string, usd float64) error {
	return s.S.AddSpend(s.Feature, day, usd)
}

// MemSpend tracks spending in memory, for tests and running without a
// database.
type MemSpend struct {
	mu  sync.Mutex
	usd map[string]float64
}

// NewMemSpend returns an empty MemSpend.
func NewMemSpend() *MemSpend { return &MemSpend{usd: map[string]float64{}} }

// Spent implements SpendTracker.
func (m *MemSpend) Spent(day string) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usd[day], nil
}

// AddSpend implements SpendTracker.
func (m *MemSpend) AddSpend(day string, usd float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usd[day] += usd
	return nil
}

// LoadKey finds the API key: env("ANTHROPIC_API_KEY"), else the file
// KeyFile in dir. The file must not be readable by group or others. No key
// at all is ErrNoKey.
func LoadKey(env func(string) string, dir string) (string, error) {
	if k := strings.TrimSpace(env("ANTHROPIC_API_KEY")); k != "" {
		return k, nil
	}
	if dir == "" {
		return "", ErrNoKey
	}
	path := filepath.Join(dir, KeyFile)
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoKey
	}
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("llm: %s is mode %04o; run chmod 600 on it", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", ErrNoKey
	}
	return k, nil
}

// Options configure a Client.
type Options struct {
	Key   string
	Model string // default Haiku
	// BaseURL defaults to https://api.anthropic.com.
	BaseURL string
	// DailyUSD caps this feature's spending per day.
	DailyUSD float64
	// Spend tracks spending; nil tracks it in memory only.
	Spend SpendTracker
	Now   func() time.Time
	HTTP  *http.Client
}

// Client calls the Messages API for one feature under one daily cap.
type Client struct {
	key      string
	model    string
	price    Price
	baseURL  string
	dailyUSD float64
	spend    SpendTracker
	now      func() time.Time
	backoff  time.Duration
	http     *http.Client
}

// New returns a client. It fails when there's no key or the model has no
// price in Prices.
func New(o Options) (*Client, error) {
	if o.Key == "" {
		return nil, ErrNoKey
	}
	if o.Model == "" {
		o.Model = Haiku
	}
	price, ok := Prices[o.Model]
	if !ok {
		return nil, fmt.Errorf("llm: no price for model %q", o.Model)
	}
	if o.BaseURL == "" {
		o.BaseURL = defaultBaseURL
	}
	if o.Spend == nil {
		o.Spend = NewMemSpend()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		key: o.Key, model: o.Model, price: price, baseURL: strings.TrimRight(o.BaseURL, "/"),
		dailyUSD: o.DailyUSD, spend: o.Spend, now: o.Now, backoff: 500 * time.Millisecond, http: o.HTTP,
	}, nil
}

// String describes the client without its key.
func (c *Client) String() string {
	if c == nil {
		return "llm(off)"
	}
	return fmt.Sprintf("llm(%s at %s)", c.model, c.baseURL)
}

// GoString is String, so %#v doesn't print the key either.
func (c *Client) GoString() string { return c.String() }

// Model is the model the client calls.
func (c *Client) Model() string { return c.model }

func (c *Client) day() string { return c.now().Format("2006-01-02") }

// OverCap reports whether today's spending has reached the cap.
func (c *Client) OverCap() (bool, error) {
	spent, err := c.spend.Spent(c.day())
	if err != nil {
		return false, fmt.Errorf("llm: spend: %w", err)
	}
	return spent >= c.dailyUSD, nil
}

// Request is one call: a system prompt (sent with prompt caching, so keep it
// byte-for-byte stable) and one user message.
type Request struct {
	System    string
	User      string
	MaxTokens int // default 1024
}

// Response is the reply's text and what it cost.
type Response struct {
	Text       string
	StopReason string
	Usage      Usage
	USD        float64
}

type textBlock struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	CacheControl *struct {
		Type string `json:"type"`
	} `json:"cache_control,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiRequest struct {
	Model     string      `json:"model"`
	MaxTokens int         `json:"max_tokens"`
	System    []textBlock `json:"system"`
	Messages  []message   `json:"messages"`
}

type apiResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      Usage  `json:"usage"`
}

// Complete checks the cap, makes the call, and records its cost. Rate limits,
// overload and server errors (429, 5xx) are retried with backoff. Over the
// cap it returns ErrOverCap without calling the API.
func (c *Client) Complete(ctx context.Context, r Request) (Response, error) {
	over, err := c.OverCap()
	if err != nil {
		return Response{}, err
	}
	if over {
		return Response{}, ErrOverCap
	}
	if r.MaxTokens == 0 {
		r.MaxTokens = 1024
	}
	sys := textBlock{Type: "text", Text: r.System}
	sys.CacheControl = &struct {
		Type string `json:"type"`
	}{Type: "ephemeral"}
	body, err := json.Marshal(apiRequest{
		Model: c.model, MaxTokens: r.MaxTokens,
		System:   []textBlock{sys},
		Messages: []message{{Role: "user", Content: r.User}},
	})
	if err != nil {
		return Response{}, err
	}
	out, err := c.post(ctx, body)
	if err != nil {
		return Response{}, err
	}
	resp := Response{StopReason: out.StopReason, Usage: out.Usage, USD: c.price.Cost(out.Usage)}
	for _, b := range out.Content {
		if b.Type == "text" {
			resp.Text += b.Text
		}
	}
	if serr := c.spend.AddSpend(c.day(), resp.USD); serr != nil {
		return resp, fmt.Errorf("llm: spend: %w", serr)
	}
	return resp, nil
}

func (c *Client) post(ctx context.Context, body []byte) (apiResponse, error) {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return apiResponse{}, ctx.Err()
			case <-time.After(c.backoff << attempt):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return apiResponse{}, err
		}
		req.Header.Set("x-api-key", c.key)
		req.Header.Set("anthropic-version", apiVersion)
		req.Header.Set("content-type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			last = fmt.Errorf("llm: %w", err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			last = fmt.Errorf("llm: %s", resp.Status)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return apiResponse{}, fmt.Errorf("llm: %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		var out apiResponse
		if err := json.Unmarshal(b, &out); err != nil {
			return apiResponse{}, fmt.Errorf("llm: decode: %w", err)
		}
		return out, nil
	}
	return apiResponse{}, last
}
