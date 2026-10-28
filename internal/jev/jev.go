// Package jev is lasso's client for TypeSafe's Jev, a decision model that
// answers typed questions (pick one key, or true/false) with a calibrated
// confidence. lasso uses it to cut long lists down before Haiku or Sonnet
// writes about them (ADR 0001 §7).
//
// The request shape, retry and cost accounting are copied from saddle's
// internal/triage/jev.go (github.com/brandonapol/saddle). Fixes there don't
// flow here automatically.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/config"
)

const (
	defaultBaseURL = "https://api.typesafe.ai"
	defaultModel   = "jev-latest"
)

// InputUSDPerMillion is Jev's list price in USD per million input tokens.
// Output tokens are free. Source: docs.typesafe.ai/models, Jev 1.13 ($42
// per billion input tokens).
const InputUSDPerMillion = 0.042

// ApproxSpendUSD estimates what inputTokens cost at the list price.
func ApproxSpendUSD(inputTokens int64) float64 {
	return float64(inputTokens) / 1_000_000 * InputUSDPerMillion
}

// ErrOverCap means today's Jev spending has reached [jev] daily_usd. Callers
// fall back to Haiku.
var ErrOverCap = errors.New("jev: over the daily cap")

// Question is one typed question for Jev: "choice" (pick a key of Criteria,
// a map) or "noul" (true/false, optional Criteria with "true"/"false").
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
	// Drop lists the choices that mean an item can go, for Cut. It is never
	// sent. A "noul" question drops items answered false.
	Drop []string `json:"-"`
}

// Answer is Jev's answer to one question.
type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Noul          float64            `json:"noul"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Client calls the Jev System One API. A nil *Client means Jev is off;
// callers check for nil and fall back to Haiku.
type Client struct {
	key      string
	baseURL  string
	model    string
	dailyUSD float64
	spend    SpendTracker
	now      func() time.Time
	backoff  time.Duration

	// MinConfidence is the confidence below which Cut keeps an item whatever
	// the answer. Default 0.7.
	MinConfidence float64
	HTTP          *http.Client
}

// New returns a client, or nil when Jev is disabled or no key is set. The key
// comes from JEV_TOKEN, else TYPESAFE_API_KEY, and the base URL from
// TYPESAFE_BASE_URL, all read through env (os.Getenv in production). A nil
// spend tracks spending in memory only.
func New(cfg config.Jev, env func(string) string, spend SpendTracker) *Client {
	if cfg.Disabled {
		return nil
	}
	key := env("JEV_TOKEN")
	if key == "" {
		key = env("TYPESAFE_API_KEY")
	}
	if key == "" {
		return nil
	}
	if spend == nil {
		spend = NewMemSpend()
	}
	model := cfg.Model
	if model == "" {
		model = defaultModel
	}
	base := env("TYPESAFE_BASE_URL")
	if base == "" {
		base = defaultBaseURL
	}
	return &Client{
		key: key, baseURL: base, model: model, dailyUSD: cfg.DailyUSD, spend: spend,
		now:           time.Now,
		backoff:       250 * time.Millisecond,
		MinConfidence: DefaultMinConfidence,
		HTTP:          &http.Client{Timeout: 10 * time.Second},
	}
}

// Status says, for `lasso doctor`, whether Jev is configured. It never
// includes the key.
func Status(cfg config.Jev, env func(string) string) string {
	switch {
	case cfg.Disabled:
		return "disabled in config"
	case env("JEV_TOKEN") != "":
		return "configured (JEV_TOKEN)"
	case env("TYPESAFE_API_KEY") != "":
		return "configured (TYPESAFE_API_KEY)"
	}
	return "not configured (set JEV_TOKEN)"
}

// String describes the client without its key, so printing one never leaks it.
func (c *Client) String() string {
	if c == nil {
		return "jev(off)"
	}
	return fmt.Sprintf("jev(%s at %s)", c.model, c.baseURL)
}

// GoString is String, so %#v doesn't print the key either.
func (c *Client) GoString() string { return c.String() }

func (c *Client) day() string { return c.now().Format("2006-01-02") }

// Ask evaluates every question against state in one call, after checking the
// daily cap, and records what the call cost. Rate limits and overload (429,
// 529) are retried with backoff.
func (c *Client) Ask(ctx context.Context, state string, qs map[string]Question) (map[string]Answer, error) {
	day := c.day()
	spent, err := c.spend.Spent(day)
	if err != nil {
		return nil, fmt.Errorf("jev: spend: %w", err)
	}
	if spent >= c.dailyUSD {
		return nil, ErrOverCap
	}
	ans, tokens, err := c.ask(ctx, state, qs)
	if tokens > 0 {
		if serr := c.spend.AddSpend(day, ApproxSpendUSD(int64(tokens))); serr != nil && err == nil {
			err = fmt.Errorf("jev: spend: %w", serr)
		}
	}
	return ans, err
}

// ask is one request plus the input tokens the API reported. Tokens are 0
// when the call fails before a usage block.
func (c *Client) ask(ctx context.Context, state string, qs map[string]Question) (map[string]Answer, int, error) {
	body, err := json.Marshal(map[string]any{"state": state, "model": c.model, "questions": qs})
	if err != nil {
		return nil, 0, err
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(c.backoff << attempt):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+"/v1/systemone", bytes.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			last = fmt.Errorf("jev: %s", resp.Status)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, 0, fmt.Errorf("jev: %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		var out struct {
			Answers map[string]Answer `json:"answers"`
			Usage   struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, 0, fmt.Errorf("jev: decode: %w", err)
		}
		return out.Answers, out.Usage.InputTokens, nil
	}
	return nil, 0, last
}
