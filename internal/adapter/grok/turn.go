package grok

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
)

// update is one ACP session update, read from a session's updates.jsonl
// or from a headless turn's streaming-json output. Only the parts a turn
// delta needs are kept: thoughts, hook runs, usage and raw tool output are
// dropped on the floor.
type update struct {
	kind     string // userText, agentText, toolCall, toolUpdate, turnEnd
	prompt   string // the turn's prompt id, when the line carries one
	at       time.Time
	text     string
	callID   string
	tool     string // grok's tool name ("run_terminal_command")
	toolKind string // ACP or x.ai kind ("edit", "execute")
	input    json.RawMessage
	status   string
	paths    []string
	stop     string // turnEnd: stop reason
	cancel   string // turnEnd: cancellation category
	denied   string // turnEnd: the tool a permission was refused for
}

const (
	userText   = "user"
	agentText  = "text"
	toolCall   = "tool_call"
	toolUpdate = "tool_call_update"
	turnEnd    = "end"
)

// call is one tool call within a turn.
type call struct {
	tool     string
	toolKind string
	input    json.RawMessage
}

// turn accumulates one prompt's updates into a compact delta.
type turn struct {
	prompt    string
	start     int64 // byte offset of its first line in updates.jsonl
	at        time.Time
	counts    map[string]int
	order     []string
	files     []string
	calls     map[string]call
	refused   *call // the last call that failed waiting for permission
	final     strings.Builder
	ended     bool
	stop      string
	cancel    string
	deniedFor string
}

func newTurn(prompt string, start int64) *turn {
	return &turn{prompt: prompt, start: start, counts: map[string]int{}, calls: map[string]call{}}
}

func (t *turn) apply(u update) {
	if !u.at.IsZero() {
		t.at = u.at
	}
	switch u.kind {
	case agentText:
		t.final.WriteString(u.text)
	case toolCall:
		// Text before a tool call was narration; the final text is what
		// the bot says after its last tool.
		t.final.Reset()
		if u.tool != "" {
			if t.counts[u.tool] == 0 {
				t.order = append(t.order, u.tool)
			}
			t.counts[u.tool]++
		}
		c := t.calls[u.callID]
		c.tool = first(u.tool, c.tool)
		c.toolKind = first(u.toolKind, c.toolKind)
		if len(u.input) > 0 {
			c.input = u.input
		}
		t.calls[u.callID] = c
		paths := u.paths
		if len(paths) == 0 {
			if p := inputString(c.input, "file_path", "target_file", "path"); p != "" {
				paths = []string{p}
			}
		}
		t.touch(c, paths)
	case toolUpdate:
		c, ok := t.calls[u.callID]
		if !ok {
			return
		}
		c.toolKind = first(c.toolKind, u.toolKind)
		t.calls[u.callID] = c
		t.touch(c, u.paths)
		if u.status == "failed" && strings.Contains(strings.ToLower(u.text), "permission") {
			cc := c
			t.refused = &cc
		}
	case turnEnd:
		t.ended = true
		t.stop = u.stop
		t.cancel = u.cancel
		t.deniedFor = u.denied
	}
}

// touch records the files an editing call changed.
func (t *turn) touch(c call, paths []string) {
	if !edits(c) {
		return
	}
	for _, p := range paths {
		if p != "" && !slices.Contains(t.files, p) {
			t.files = append(t.files, p)
		}
	}
}

func edits(c call) bool {
	switch c.toolKind {
	case "edit", "write", "delete", "move":
		return true
	}
	switch c.tool {
	case "search_replace", "write", "edit_file", "delete_file":
		return true
	}
	return false
}

// waiting reports whether the turn stopped at a permission prompt grok
// couldn't put to anyone (a headless turn has no one to ask).
func (t *turn) waiting() bool {
	if !t.ended || t.stop != "cancelled" {
		return false
	}
	return t.cancel == "PermissionRejected" || t.refused != nil
}

// pending is a permission prompt lasso can answer by resuming the session
// with an allow or deny rule.
type pending struct {
	Session string
	Prompt  string // grok's prompt id for the stopped turn
	Cwd     string
	Tool    string
	Rule    string // grok rule text, "Bash(go test ./...)"
	Plain   string // what it asks, in plain words
	Paths   []string
}

// Limits that keep one turn's event small: grok bots are chatty.
const (
	maxTools = 6
	maxFiles = 5
	maxSaid  = 280
)

// event builds the turn's compact delta. cwd makes file paths short.
func (t *turn) event(session, cwd string) (adapter.Event, *pending) {
	e := adapter.Event{ID: session + "/" + t.prompt, Bot: Kind, Session: session, At: t.at, Kind: attention.KindBotMessage}
	var parts []string
	var p *pending
	if t.waiting() {
		c := call{tool: t.deniedFor}
		if t.refused != nil && (t.deniedFor == "" || t.refused.tool == t.deniedFor) {
			c = *t.refused
		}
		p = permission(session, t.prompt, cwd, c)
		e.Kind = attention.KindPermissionPrompt
		parts = append(parts, "Stopped to ask permission to "+p.Plain+".")
		if p.Rule != "" {
			e.PromptID = p.Session + "/" + p.Prompt
			e.Options = []adapter.Option{
				{N: 1, Label: adapter.Untrusted("Allow " + p.Rule + " and carry on")},
				{N: 2, Label: adapter.Untrusted("Deny it and carry on without it")},
			}
		} else {
			parts = append(parts, "lasso can't turn this tool into a grok rule; answer it in grok.")
			p = nil
		}
	} else {
		parts = append(parts, ended(t.stop))
	}
	if s := t.toolSummary(); s != "" {
		parts = append(parts, "Tools: "+s+".")
	}
	if s := listClip(shortPaths(t.files, cwd), maxFiles); s != "" {
		parts = append(parts, "Changed: "+s+".")
	}
	if said := oneLine(t.final.String()); said != "" {
		parts = append(parts, "Said: "+clip(said, maxSaid))
	}
	e.Text = adapter.Untrusted(attention.Gloss(strings.Join(parts, " ")))
	return e, p
}

func ended(stop string) string {
	switch stop {
	case "", "end_turn":
		return "Finished a turn."
	case "cancelled":
		return "Turn was cancelled."
	case "max_tokens", "max_turn_requests":
		return "Stopped at its limit (" + stop + ")."
	default:
		return "Turn ended (" + stop + ")."
	}
}

func (t *turn) toolSummary() string {
	names := slices.Clone(t.order)
	slices.SortStableFunc(names, func(a, b string) int { return t.counts[b] - t.counts[a] })
	var out []string
	for _, n := range names {
		if c := t.counts[n]; c > 1 {
			out = append(out, fmt.Sprintf("%s ×%d", n, c))
		} else {
			out = append(out, n)
		}
	}
	return listClip(out, maxTools)
}

// permission turns a refused call into the rule that would let it through.
// The rule comes from the call's own arguments as grok recorded them, never
// from the bot's description of what it was doing.
func permission(session, prompt, cwd string, c call) *pending {
	p := &pending{Session: session, Prompt: prompt, Cwd: cwd, Tool: c.tool}
	abs := func(path string) string {
		if path != "" && !filepath.IsAbs(path) && cwd != "" {
			return filepath.Join(cwd, path)
		}
		return path
	}
	switch c.tool {
	case "run_terminal_command", "bash", "Bash":
		if cmd := strings.TrimSpace(inputString(c.input, "command")); cmd != "" {
			p.Rule = "Bash(" + cmd + ")"
			p.Plain = "run `" + clip(oneLine(cmd), 160) + "`"
		}
	case "search_replace", "write", "edit_file", "delete_file":
		if f := abs(inputString(c.input, "file_path", "target_file", "path")); f != "" {
			p.Rule = "Edit(" + f + ")"
			p.Plain = "change " + shortPath(f, cwd)
			p.Paths = []string{shortPath(f, cwd)}
		}
	case "read_file", "list_dir":
		if f := abs(inputString(c.input, "target_file", "target_directory", "path")); f != "" {
			p.Rule = "Read(" + f + ")"
			p.Plain = "read " + shortPath(f, cwd)
		}
	case "grep":
		if f := abs(inputString(c.input, "path")); f != "" {
			p.Rule = "Grep(" + f + ")"
			p.Plain = "search " + shortPath(f, cwd)
		}
	case "web_fetch":
		if u := inputString(c.input, "url"); u != "" {
			p.Rule = "WebFetch(" + u + ")"
			p.Plain = "fetch " + clip(u, 160)
		}
	case "use_tool":
		if n := inputString(c.input, "tool_name"); n != "" {
			p.Rule = "MCPTool(" + n + ")"
			p.Plain = "use the " + n + " tool"
		}
	}
	if p.Plain == "" {
		p.Plain = "use " + first(c.tool, "a tool")
	}
	return p
}

func inputString(raw json.RawMessage, keys ...string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// shortPaths makes paths relative to cwd, dropping repeats (one file
// named both ways).
func shortPaths(ps []string, cwd string) []string {
	var out []string
	for _, p := range ps {
		if !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		if s := shortPath(p, cwd); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func shortPath(p, cwd string) string {
	if cwd != "" {
		if rel, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return p
}

func listClip(xs []string, n int) string {
	if len(xs) <= n {
		return strings.Join(xs, ", ")
	}
	return strings.Join(xs[:n], ", ") + fmt.Sprintf(" +%d more", len(xs)-n)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func first(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// diskLine is one line of a session's updates.jsonl: an ACP
// session/update notification, or grok's _x.ai/session/update extension.
type diskLine struct {
	Timestamp int64 `json:"timestamp"`
	Params    struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"`
			ToolCallID    string          `json:"toolCallId"`
			Kind          string          `json:"kind"`
			Status        string          `json:"status"`
			RawInput      json.RawMessage `json:"rawInput"`
			Locations     []struct {
				Path string `json:"path"`
			} `json:"locations"`
			PromptID   string `json:"prompt_id"`
			StopReason string `json:"stop_reason"`
			Meta       struct {
				Tool struct {
					Name string `json:"name"`
					Kind string `json:"kind"`
				} `json:"x.ai/tool"`
			} `json:"_meta"`
		} `json:"update"`
		Meta struct {
			PromptID             string `json:"promptId"`
			AgentTimestampMs     int64  `json:"agentTimestampMs"`
			CancellationCategory string `json:"cancellationCategory"`
			CancellationContext  struct {
				ToolName string `json:"tool_name"`
			} `json:"cancellationContext"`
		} `json:"_meta"`
	} `json:"params"`
}

// parseDisk reads one updates.jsonl line. ok is false for lines a turn
// delta doesn't use.
func parseDisk(line []byte) (update, bool) {
	var l diskLine
	if json.Unmarshal(line, &l) != nil {
		return update{}, false
	}
	up := l.Params.Update
	u := update{prompt: first(l.Params.Meta.PromptID, up.PromptID), callID: up.ToolCallID, status: up.Status, input: up.RawInput}
	switch {
	case l.Params.Meta.AgentTimestampMs > 0:
		u.at = time.UnixMilli(l.Params.Meta.AgentTimestampMs).UTC()
	case l.Timestamp > 0:
		u.at = time.Unix(l.Timestamp, 0).UTC()
	}
	for _, loc := range up.Locations {
		u.paths = append(u.paths, loc.Path)
	}
	switch up.SessionUpdate {
	case "user_message_chunk":
		u.kind = userText
	case "agent_message_chunk":
		u.kind = agentText
		u.text = contentText(up.Content)
	case "tool_call":
		u.kind = toolCall
		u.tool = up.Meta.Tool.Name
		u.toolKind = first(up.Meta.Tool.Kind, up.Kind)
	case "tool_call_update":
		u.kind = toolUpdate
		u.toolKind = first(up.Meta.Tool.Kind, up.Kind)
		u.text = contentText(up.Content)
	case "turn_completed":
		u.kind = turnEnd
		u.stop = up.StopReason
		u.cancel = l.Params.Meta.CancellationCategory
		u.denied = l.Params.Meta.CancellationContext.ToolName
	default:
		// Hook runs carry the prompt id before any chatter does, so they
		// still mark where a turn starts.
		return u, u.prompt != ""
	}
	return u, true
}

// contentText reads an ACP content block, or a tool update's list of them.
func contentText(raw json.RawMessage) string {
	type block struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Content *struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	var many []block
	var one block
	if json.Unmarshal(raw, &one) == nil {
		many = []block{one}
	} else if json.Unmarshal(raw, &many) != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range many {
		b.WriteString(bl.Text)
		if bl.Content != nil {
			b.WriteString(bl.Content.Text)
		}
	}
	return b.String()
}

// streamLine is one line of `grok --output-format streaming-json`: the
// same ACP updates, flattened and tagged by type.
type streamLine struct {
	Type       string          `json:"type"`
	Data       string          `json:"data"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	Content    json.RawMessage `json:"content"`
	Locations  []struct {
		Path string `json:"path"`
	} `json:"locations"`
	StopReason string `json:"stopReason"`
	SessionID  string `json:"sessionId"`
	RequestID  string `json:"requestId"`
	Message    string `json:"message"`
}

func (l streamLine) update(now time.Time) (update, bool) {
	u := update{at: now, callID: l.ToolCallID, status: l.Status, input: l.RawInput, tool: l.ToolName, toolKind: l.Kind}
	for _, loc := range l.Locations {
		u.paths = append(u.paths, loc.Path)
	}
	switch l.Type {
	case "text":
		u.kind, u.text = agentText, l.Data
	case "tool_call":
		u.kind = toolCall
	case "tool_call_update":
		u.kind = toolUpdate
		u.text = contentText(l.Content)
	case "end":
		u.kind, u.stop = turnEnd, l.StopReason
	default:
		return u, false
	}
	return u, true
}
