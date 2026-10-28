package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/attention"
)

// The transcript format is Claude Code's own and changes without notice,
// so everything here is defensive: a line that doesn't parse, or whose
// type isn't known, is skipped and counted, never fatal.

// line is the part of one transcript line lasso reads.
type line struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	UUID        string          `json:"uuid"`
	Cwd         string          `json:"cwd"`
	Timestamp   string          `json:"timestamp"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	IsAPIError  bool            `json:"isApiErrorMessage"`
	Level       string          `json:"level"`
	Content     json.RawMessage `json:"content"` // system lines
	Message     *message        `json:"message"`
	CustomTitle string          `json:"customTitle"`
	AITitle     string          `json:"aiTitle"`
	AgentName   string          `json:"agentName"`
	PRURL       string          `json:"prUrl"`
	Origin      *origin         `json:"origin"`
}

// origin is where a meta user line came from. For a message from another
// session (kind "peer") it carries the sender's name, the message's id,
// and its text as delivered.
type origin struct {
	Kind  string `json:"kind"`
	MsgID string `json:"msg_id"`
	Name  string `json:"name"`
	Body  string `json:"body"`
}

// peerMsg is a message another session put into this one.
type peerMsg struct{ from, body string }

// maxPeers bounds how many peer messages a file remembers.
const maxPeers = 64

type message struct {
	ID         string          `json:"id"`
	Content    json.RawMessage `json:"content"` // a string, or a list of blocks
	StopReason string          `json:"stop_reason"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// knownTypes are the line types lasso has seen and deliberately ignores
// beyond what parse reads. Anything else is counted as unknown.
var knownTypes = map[string]bool{
	"user": true, "assistant": true, "system": true, "attachment": true, "summary": true,
	"queue-operation": true, "mode": true, "permission-mode": true, "atis-latch": true,
	"last-prompt": true, "ai-title": true, "custom-title": true, "agent-name": true,
	"pr-link": true, "frame-link": true, "file-history-snapshot": true, "file-history-delta": true,
	"bridge-session": true, "cost-state": true, "artifact-comment-monitor": true,
	"artifact-autoreact-ledger": true,
}

// unreadable is the skip count's key for lines that aren't JSON.
const unreadable = "(unreadable)"

// toolAsk is Claude Code's tool for asking the user a multiple-choice question.
const toolAsk = "AskUserQuestion"

// Clip lengths, in runes.
const (
	clipText   = 600
	clipDetail = 200
	clipTitle  = 80
)

// maxEvents is how many past events a session keeps for readers behind.
const maxEvents = 50

// openTool is a tool call with no result yet.
type openTool struct {
	id, name, detail string
	question         string
	options          []string
	at               time.Time
}

// turn accumulates one assistant turn until it ends.
type turn struct {
	prompt    string // the user's words that opened it
	tools     []string
	toolCount map[string]int
	refused   []string
	toolErrs  int
	lastText  string
	lastUUID  string
	lastAt    time.Time
	ending    bool   // an assistant line said end_turn
	endingMsg string // that line's message id
}

func (t *turn) empty() bool { return len(t.tools) == 0 && t.lastText == "" && len(t.refused) == 0 }

// stored is an event and the transcript offset it ends at.
type stored struct {
	ev  adapter.Event
	off int64
}

// file is everything lasso knows about one transcript, built up
// incrementally as the file grows.
type file struct {
	id, path string
	size     int64
	mtime    time.Time
	off      int64 // parsed up to here (always at a line start)

	cwd, customTitle, aiTitle, agentName, firstPrompt, pr string

	ignored   bool // saddle's or lasso's: not read past its first cwd
	cur       *turn
	closedMsg string // the message id whose turn already ended
	open      map[string]*openTool
	events    []stored
	sent      map[string]int     // texts lasso sent, whose turns its own reply event stands for
	peers     map[string]peerMsg // messages from other sessions, by msg_id
}

func newFile(id, path string) *file {
	return &file{id: id, path: path, open: map[string]*openTool{}, sent: map[string]int{}, peers: map[string]peerMsg{}}
}

func (f *file) title() string {
	for _, t := range []string{f.customTitle, f.aiTitle, f.agentName, f.firstPrompt} {
		if t = strings.TrimSpace(t); t != "" {
			return clip(attention.Gloss(firstLine(t)), clipTitle)
		}
	}
	return f.id
}

// turnOpen reports whether the assistant is mid-turn.
func (f *file) turnOpen() bool { return f.cur != nil && !f.cur.ending }

func (f *file) add(ev adapter.Event, off int64) {
	f.events = append(f.events, stored{ev: ev, off: off})
	if n := len(f.events) - maxEvents; n > 0 {
		f.events = append([]stored(nil), f.events[n:]...)
	}
}

// parse reads the complete lines added since the last parse. skip counts
// lines it couldn't use. Once the session's directory is known, foreign
// says whether it is someone else's, and if so reading stops for good.
func (f *file) parse(skip map[string]int, foreign func(cwd string) bool) error {
	fh, err := os.Open(f.path)
	if err != nil {
		return err
	}
	defer fh.Close()
	if _, err := fh.Seek(f.off, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(fh, 64<<10)
	for {
		b, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break // a partial last line waits for the rest
		}
		if err != nil {
			return err
		}
		f.off += int64(len(b))
		f.line(b, skip)
		if f.cwd != "" && foreign(f.cwd) {
			f.ignored = true
			return nil
		}
	}
	// An ended turn is complete once its lines are all in.
	if f.cur != nil && f.cur.ending {
		f.finish()
	}
	return nil
}

func (f *file) line(b []byte, skip map[string]int) {
	var l line
	if err := json.Unmarshal(b, &l); err != nil || l.Type == "" {
		skip[unreadable]++
		return
	}
	if !knownTypes[l.Type] {
		skip[l.Type]++
		return
	}
	if l.IsSidechain {
		return // a subagent's own conversation
	}
	// The directory it was started in, not wherever it has wandered since:
	// that's the project Claude Code files it under, and where --resume
	// finds it again.
	if f.cwd == "" {
		f.cwd = l.Cwd
	}
	at := parseTime(l.Timestamp)
	switch l.Type {
	case "custom-title":
		f.customTitle = l.CustomTitle
	case "ai-title":
		f.aiTitle = l.AITitle
	case "agent-name":
		f.agentName = l.AgentName
	case "pr-link":
		if l.PRURL != "" {
			f.pr = l.PRURL
		}
	case "user":
		f.user(l)
	case "assistant":
		f.assistant(l, at)
	case "system":
		f.system(l, at)
	}
}

func (f *file) user(l line) {
	if o := l.Origin; l.IsMeta && o != nil && o.Kind == "peer" && o.MsgID != "" {
		if len(f.peers) >= maxPeers {
			clear(f.peers) // only the newest matter: lasso checks its sends within the hour
		}
		f.peers[o.MsgID] = peerMsg{from: o.Name, body: o.Body}
	}
	if l.Message == nil || l.IsMeta {
		return
	}
	if s, ok := asString(l.Message.Content); ok {
		f.prompt(s)
		return
	}
	var blocks []block
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return
	}
	var text []string
	results := false
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			results = true
			if o := f.open[b.ToolUseID]; o != nil {
				delete(f.open, b.ToolUseID)
				if b.IsError && f.cur != nil {
					if refusal(b.Content) {
						f.cur.refused = append(f.cur.refused, o.name)
					} else {
						f.cur.toolErrs++
					}
				}
			}
		case "text":
			text = append(text, b.Text)
		}
	}
	if !results {
		f.prompt(strings.Join(text, "\n"))
	}
}

// prompt is the user speaking: any turn still open was interrupted.
func (f *file) prompt(s string) {
	if f.cur != nil && !f.cur.empty() {
		f.finish()
	}
	clear(f.open)
	if f.firstPrompt == "" && !strings.HasPrefix(strings.TrimSpace(s), "<") {
		f.firstPrompt = s
	}
	f.cur = &turn{prompt: s, toolCount: map[string]int{}}
}

func (f *file) assistant(l line, at time.Time) {
	m := l.Message
	if m == nil {
		return
	}
	if l.IsAPIError {
		f.add(f.event(l.UUID, "Error: "+blockText(m.Content), at), f.off)
		return
	}
	if m.ID != "" && m.ID == f.closedMsg {
		return // more of a turn that already ended
	}
	if f.cur != nil && f.cur.ending && m.ID != f.cur.endingMsg {
		f.finish() // a new message after the end: a turn the user didn't start
	}
	if f.cur == nil {
		f.cur = &turn{toolCount: map[string]int{}}
	}
	t := f.cur
	var blocks []block
	_ = json.Unmarshal(m.Content, &blocks)
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if s := strings.TrimSpace(b.Text); s != "" {
				t.lastText = s
			}
		case "tool_use":
			if t.toolCount[b.Name] == 0 {
				t.tools = append(t.tools, b.Name)
			}
			t.toolCount[b.Name]++
			o := &openTool{id: b.ID, name: b.Name, at: at}
			o.detail, o.question, o.options = describe(b.Name, b.Input)
			if b.ID != "" {
				f.open[b.ID] = o
			}
		}
	}
	t.lastUUID, t.lastAt = l.UUID, at
	switch m.StopReason {
	case "", "tool_use", "pause_turn":
	default: // end_turn, max_tokens, refusal, stop_sequence: the turn is over
		t.ending, t.endingMsg = true, m.ID
	}
}

func (f *file) system(l line, at time.Time) {
	switch {
	case l.Level == "error":
		text, _ := asString(l.Content)
		f.add(f.event(l.UUID, "Error: "+text, at), f.off)
	case l.Subtype == "turn_duration" || l.Subtype == "stop_hook_summary":
		if f.cur != nil && f.cur.ending {
			f.finish()
		}
	}
}

// finish turns the current turn into one compact event: the tools used,
// by name only, and the final text, clipped.
func (f *file) finish() {
	t := f.cur
	f.cur = nil
	f.closedMsg = t.endingMsg
	clear(f.open)
	if t.empty() {
		return
	}
	if n := f.sent[t.prompt]; n > 0 {
		// lasso sent this one, and its reply event already stands for it.
		if n == 1 {
			delete(f.sent, t.prompt)
		} else {
			f.sent[t.prompt] = n - 1
		}
		return
	}
	var parts []string
	if len(t.tools) > 0 {
		names := make([]string, len(t.tools))
		for i, n := range t.tools {
			names[i] = n
			if c := t.toolCount[n]; c > 1 {
				names[i] = fmt.Sprintf("%s ×%d", n, c)
			}
		}
		used := "Used " + strings.Join(names, ", ")
		if t.toolErrs > 0 {
			used += fmt.Sprintf(" (%d failed)", t.toolErrs)
		}
		parts = append(parts, used+".")
	}
	if len(t.refused) > 0 {
		parts = append(parts, "Permission refused for "+strings.Join(t.refused, ", ")+".")
	}
	if t.lastText != "" {
		parts = append(parts, clip(t.lastText, clipText))
	}
	f.add(f.event(t.lastUUID, strings.Join(parts, "\n"), t.lastAt), f.off)
}

func (f *file) event(id, text string, at time.Time) adapter.Event {
	if id == "" {
		id = fmt.Sprintf("%s@%d", f.id, f.off)
	}
	if at.IsZero() {
		at = f.mtime
	}
	return adapter.Event{
		ID: id, Bot: Kind, Session: f.id, Kind: attention.KindBotMessage,
		Text: adapter.Untrusted(attention.Gloss(text)), At: at,
	}
}

// describe pulls the part of a tool call's input a prompt about it shows.
func describe(name string, raw json.RawMessage) (detail, question string, options []string) {
	var in struct {
		Command   string `json:"command"`
		FilePath  string `json:"file_path"`
		URL       string `json:"url"`
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(raw, &in)
	if name == toolAsk {
		var qs []string
		for i, q := range in.Questions {
			qs = append(qs, q.Question)
			if i == 0 {
				for _, o := range q.Options {
					options = append(options, o.Label)
				}
			}
		}
		return "", strings.Join(qs, "\n"), options
	}
	for _, s := range []string{in.Command, in.FilePath, in.URL} {
		if s != "" {
			return clip(s, clipDetail), "", nil
		}
	}
	return "", "", nil
}

// refusal reports whether a failed tool result is the user (or a rule)
// saying no, rather than the tool failing.
func refusal(raw json.RawMessage) bool {
	s, ok := asString(raw)
	if !ok {
		s = blockText(raw)
	}
	return strings.Contains(s, "Permission for this action was denied") ||
		strings.Contains(s, "doesn't want to proceed with this tool use")
}

func asString(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// blockText joins the text blocks of a content list.
func blockText(raw json.RawMessage) string {
	if s, ok := asString(raw); ok {
		return s
	}
	var blocks []block
	_ = json.Unmarshal(raw, &blocks)
	var out []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "\n")
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// clip cuts s to n runes, marking the cut.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n])) + "…"
}
