package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool is one tool the model may call: a name, what it does, and a JSON
// Schema for its input.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Block is one content block of a conversation message: text, a tool call
// the model made (tool_use), or the result lasso sends back (tool_result).
type Block struct {
	Type string `json:"type"`
	// Text is set for "text".
	Text string `json:"text,omitempty"`
	// ID, Name and Input are set for "tool_use".
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// ToolUseID, Content and IsError are set for "tool_result".
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// TextBlock is a text content block.
func TextBlock(s string) Block { return Block{Type: "text", Text: s} }

// ToolResult is the answer to the tool call id.
func ToolResult(id, content string, isError bool) Block {
	return Block{Type: "tool_result", ToolUseID: id, Content: content, IsError: isError}
}

// Message is one turn of a conversation: "user" or "assistant".
type Message struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

// Conversation is a call with a whole conversation and tools. System is sent
// with prompt caching, like Request's.
type Conversation struct {
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int // default 1024
}

// Turn is the model's reply: its content blocks (text and tool calls), why
// it stopped ("end_turn", "tool_use", "max_tokens"), and what it cost.
type Turn struct {
	Content    []Block
	StopReason string
	Usage      Usage
	USD        float64
}

// Text is the reply's text blocks joined.
func (t Turn) Text() string {
	var s string
	for _, b := range t.Content {
		if b.Type == "text" {
			s += b.Text
		}
	}
	return s
}

// ToolCalls are the reply's tool_use blocks, in order.
func (t Turn) ToolCalls() []Block {
	var out []Block
	for _, b := range t.Content {
		if b.Type == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}

type convRequest struct {
	Model     string      `json:"model"`
	MaxTokens int         `json:"max_tokens"`
	System    []textBlock `json:"system"`
	Messages  []Message   `json:"messages"`
	Tools     []Tool      `json:"tools,omitempty"`
}

type convResponse struct {
	Content    []Block `json:"content"`
	StopReason string  `json:"stop_reason"`
	Usage      Usage   `json:"usage"`
}

// Converse sends one round of a conversation with tools, under the same cap,
// retries and cost recording as Complete. The caller runs any tool calls in
// the reply and calls Converse again with their results.
func (c *Client) Converse(ctx context.Context, conv Conversation) (Turn, error) {
	over, err := c.OverCap()
	if err != nil {
		return Turn{}, err
	}
	if over {
		return Turn{}, ErrOverCap
	}
	if conv.MaxTokens == 0 {
		conv.MaxTokens = 1024
	}
	sys := textBlock{Type: "text", Text: conv.System}
	sys.CacheControl = &struct {
		Type string `json:"type"`
	}{Type: "ephemeral"}
	body, err := json.Marshal(convRequest{
		Model: c.model, MaxTokens: conv.MaxTokens,
		System: []textBlock{sys}, Messages: conv.Messages, Tools: conv.Tools,
	})
	if err != nil {
		return Turn{}, err
	}
	b, err := c.postRaw(ctx, body)
	if err != nil {
		return Turn{}, err
	}
	var out convResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return Turn{}, fmt.Errorf("llm: decode: %w", err)
	}
	t := Turn{Content: out.Content, StopReason: out.StopReason, Usage: out.Usage, USD: c.price.Cost(out.Usage)}
	if serr := c.spend.AddSpend(c.day(), t.USD); serr != nil {
		return t, fmt.Errorf("llm: spend: %w", serr)
	}
	return t, nil
}
