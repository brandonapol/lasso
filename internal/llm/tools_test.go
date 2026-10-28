package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConverseSendsToolsAndReadsToolCalls(t *testing.T) {
	var seen []convRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body convRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		seen = append(seen, body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "Looking."},
				{"type": "tool_use", "id": "tu_1", "name": "list_items", "input": map[string]string{"tab": "issues"}},
			},
			"stop_reason": "tool_use",
			"usage":       map[string]int{"input_tokens": 1_000_000},
		})
	}))
	defer srv.Close()
	spend := NewMemSpend()
	c, err := New(Options{Key: "k", Model: Sonnet, BaseURL: srv.URL, DailyUSD: 3, Spend: spend,
		Now: func() time.Time { return time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	conv := Conversation{
		System: "sys",
		Messages: []Message{
			{Role: "user", Content: []Block{TextBlock("hi")}},
			{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "tu_0", Name: "explain", Input: json.RawMessage(`{"term":"rebase"}`)}}},
			{Role: "user", Content: []Block{ToolResult("tu_0", "bring its branch up to date", false)}},
		},
		Tools: []Tool{{Name: "list_items", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	turn, err := c.Converse(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	r := seen[0]
	if r.Model != Sonnet || len(r.Tools) != 1 || r.Tools[0].Name != "list_items" || len(r.Messages) != 3 ||
		r.Messages[2].Content[0].ToolUseID != "tu_0" || r.System[0].CacheControl == nil {
		t.Fatalf("request = %+v", r)
	}
	calls := turn.ToolCalls()
	if turn.StopReason != "tool_use" || turn.Text() != "Looking." || len(calls) != 1 ||
		calls[0].Name != "list_items" || string(calls[0].Input) != `{"tab":"issues"}` {
		t.Fatalf("turn = %+v", turn)
	}
	if got, _ := spend.Spent("2026-10-27"); got != 2 {
		t.Fatalf("spent %v, want 2", got)
	}
	// $2 is under the $3 cap, so one more call goes out; at $4 the next stops.
	if _, err := c.Converse(context.Background(), conv); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Converse(context.Background(), conv); !errors.Is(err, ErrOverCap) {
		t.Fatalf("over cap: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("calls = %d, want 2 (none past the cap)", len(seen))
	}
}
