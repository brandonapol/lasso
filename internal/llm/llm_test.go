package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/lasso/internal/store"
)

// fake serves /v1/messages. statuses are answered first, one per call.
func fake(t *testing.T, daily float64, spend SpendTracker, reply string, statuses ...int) (*Client, *[]apiRequest) {
	t.Helper()
	var seen []apiRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "sk-secret" || r.Header.Get("anthropic-version") != apiVersion {
			t.Errorf("bad request %s", r.URL.Path)
		}
		var body apiRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		seen = append(seen, body)
		if len(seen) <= len(statuses) {
			w.WriteHeader(statuses[len(seen)-1])
			fmt.Fprint(w, `{"type":"error","error":{"message":"slow down"}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]string{{"type": "text", "text": reply}},
			"stop_reason": "end_turn",
			"usage": map[string]int{
				"input_tokens": 100_000, "output_tokens": 10_000,
				"cache_creation_input_tokens": 0, "cache_read_input_tokens": 200_000,
			},
		})
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{Key: "sk-secret", BaseURL: srv.URL, DailyUSD: daily, Spend: spend,
		Now: func() time.Time { return time.Date(2026, 10, 27, 9, 0, 0, 0, time.Local) }})
	if err != nil {
		t.Fatal(err)
	}
	c.backoff = time.Millisecond
	return c, &seen
}

// 100k input ($0.10) + 10k output ($0.05) + 200k cache reads ($0.02).
const callUSD = 0.17

func TestCompleteSendsCachedSystemAndRecordsCost(t *testing.T) {
	spend := NewMemSpend()
	c, seen := fake(t, 1, spend, "hello")
	resp, err := c.Complete(context.Background(), Request{System: "sys", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hello" || math.Abs(resp.USD-callUSD) > 1e-9 {
		t.Fatalf("resp = %+v", resp)
	}
	r := (*seen)[0]
	if r.Model != Haiku || r.MaxTokens != 1024 || len(r.System) != 1 || r.System[0].Text != "sys" ||
		r.System[0].CacheControl == nil || r.System[0].CacheControl.Type != "ephemeral" ||
		len(r.Messages) != 1 || r.Messages[0].Role != "user" || r.Messages[0].Content != "u" {
		t.Fatalf("request = %+v", r)
	}
	if usd, _ := spend.Spent("2026-10-27"); math.Abs(usd-callUSD) > 1e-9 {
		t.Fatalf("spent = %v", usd)
	}
}

func TestOverCap(t *testing.T) {
	c, seen := fake(t, 0.3, nil, "x")
	for i := 0; i < 2; i++ {
		if _, err := c.Complete(context.Background(), Request{System: "s", User: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Complete(context.Background(), Request{System: "s", User: "u"}); !errors.Is(err, ErrOverCap) {
		t.Fatalf("err = %v", err)
	}
	if len(*seen) != 2 {
		t.Fatalf("called the API %d times", len(*seen))
	}
}

func TestStoreSpendPerFeature(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "lasso.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, _ := fake(t, 0.1, StoreSpend{S: s, Feature: "digest"}, "x")
	if _, err := c.Complete(context.Background(), Request{System: "s", User: "u"}); err != nil {
		t.Fatal(err)
	}
	if usd, _ := s.Spent("digest", "2026-10-27"); math.Abs(usd-callUSD) > 1e-9 {
		t.Fatalf("stored = %v", usd)
	}
	if usd, _ := s.Spent("summaries", "2026-10-27"); usd != 0 {
		t.Fatalf("other feature charged %v", usd)
	}
	if _, err := c.Complete(context.Background(), Request{System: "s", User: "u"}); !errors.Is(err, ErrOverCap) {
		t.Fatalf("err = %v", err)
	}
}

func TestRetries429And529(t *testing.T) {
	c, seen := fake(t, 1, nil, "ok", http.StatusTooManyRequests, 529)
	resp, err := c.Complete(context.Background(), Request{System: "s", User: "u"})
	if err != nil || resp.Text != "ok" || len(*seen) != 3 {
		t.Fatalf("resp = %+v, err = %v, calls = %d", resp, err, len(*seen))
	}
}

func TestNon200IncludesBodyNotKey(t *testing.T) {
	c, seen := fake(t, 1, nil, "ok", http.StatusBadRequest)
	_, err := c.Complete(context.Background(), Request{System: "s", User: "u"})
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "slow down") || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("err = %v", err)
	}
	if len(*seen) != 1 {
		t.Fatal("retried a 400")
	}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
		if strings.Contains(s, "sk-secret") {
			t.Fatalf("key printed: %s", s)
		}
	}
}

func TestUnknownModelRefused(t *testing.T) {
	if _, err := New(Options{Key: "k", Model: "claude-unpriced"}); err == nil {
		t.Fatal("unpriced model accepted")
	}
	if _, err := New(Options{}); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	none := func(string) string { return "" }
	if _, err := LoadKey(none, dir); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v", err)
	}
	env := func(k string) string {
		if k == "ANTHROPIC_API_KEY" {
			return "from-env"
		}
		return ""
	}
	if k, _ := LoadKey(env, dir); k != "from-env" {
		t.Fatalf("key = %q", k)
	}
	path := filepath.Join(dir, KeyFile)
	if err := os.WriteFile(path, []byte("from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(none, dir); err == nil || strings.Contains(err.Error(), "from-file") {
		t.Fatalf("world-readable key file accepted: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := LoadKey(none, dir); err != nil || k != "from-file" {
		t.Fatalf("key = %q, err = %v", k, err)
	}
}
