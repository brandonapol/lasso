package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/agentpane"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/audit"
	"github.com/brandonapol/lasso/internal/llm"
	"github.com/brandonapol/lasso/internal/rules"
	"github.com/brandonapol/lasso/internal/store"
)

// factBot is a fake bot that keeps what it is sent and, like grok, knows
// the facts of its prompts: each one wants to run command.
type factBot struct {
	fakeBot
	command string

	sendMu sync.Mutex
	sent   []string // "session: text"
}

func (f *factBot) Send(_ context.Context, session, text string) error {
	f.sendMu.Lock()
	defer f.sendMu.Unlock()
	f.sent = append(f.sent, session+": "+text)
	return nil
}

func (f *factBot) messages() []string {
	f.sendMu.Lock()
	defer f.sendMu.Unlock()
	return slices.Clone(f.sent)
}

func (f *factBot) Request(promptID string, c adapter.Choice) (rules.Request, error) {
	return rules.Request{
		Bot: "fake", Action: rules.AnswerPrompt, Target: promptID, Paths: []string{"main.go"},
		Args: map[string]string{"tool": "bash", "rule": "Bash(" + f.command + ")"},
	}, nil
}

// scripted is a model that makes the tool calls it is given, one per
// turn, then says done. It keeps every tool result it is sent.
type scripted struct {
	mu      sync.Mutex
	calls   []llm.Block
	results []llm.Block
}

func (s *scripted) Converse(_ context.Context, conv llm.Conversation) (llm.Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := conv.Messages[len(conv.Messages)-1]; m.Role == "user" {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				s.results = append(s.results, b)
			}
		}
	}
	if len(s.calls) == 0 {
		return llm.Turn{Content: []llm.Block{llm.TextBlock("Done.")}, StopReason: "end_turn"}, nil
	}
	c := s.calls[0]
	s.calls = s.calls[1:]
	return llm.Turn{Content: []llm.Block{c}, StopReason: "tool_use"}, nil
}

func (s *scripted) result(t *testing.T) llm.Block {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.results) == 0 {
		t.Fatal("the model was sent no tool result")
	}
	return s.results[len(s.results)-1]
}

func toolCall(name string, input map[string]string) llm.Block {
	b, _ := json.Marshal(input)
	return llm.Block{Type: "tool_use", ID: "tu-" + name, Name: name, Input: b}
}

// assistant is the real agent pane over r's API with the agent pane's
// token, talking to a scripted model.
func (r *rig) assistant(calls ...llm.Block) (*agentpane.Pane, *scripted) {
	r.t.Helper()
	st, err := store.Open(filepath.Join(r.t.TempDir(), "pane.db"))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = st.Close() })
	m := &scripted{calls: calls}
	return agentpane.New(agentpane.Options{
		API: r.base, Token: r.agentTok, Store: st,
		Model: func() (agentpane.Model, error) { return m, nil },
	}), m
}

func fieldOf(f api.Focus, name string) string {
	for _, fl := range f.Fields {
		if fl.Name == name {
			return fl.Value
		}
	}
	return ""
}

const messageRules = `
[[rules]]
id = "lasso-may-message-fake"
who = "fake-*"
action = "send_message"
per_day = 5
`

func TestAssistantSendsThroughARule(t *testing.T) {
	bot := &factBot{fakeBot: fakeBot{prompts: []string{"p1"}}}
	r := startRigWith(t, messageRules, bot)
	r.eventually("the session on In progress", func() bool { return slices.Contains(r.ids(api.TabDoing), "session:fake:s-p1") })

	pane, m := r.assistant(toolCall("send_to_bot", map[string]string{"session": "session:fake:s-p1", "text": "please rebase on main"}))
	if _, err := pane.Send(context.Background(), "tell it to rebase"); err != nil {
		t.Fatal(err)
	}
	if res := m.result(t); res.IsError || !strings.Contains(res.Content, "lasso-may-message-fake") {
		t.Fatalf("tool result = %+v", res)
	}
	if got := bot.messages(); !slices.Equal(got, []string{"s-p1: please rebase on main"}) {
		t.Fatalf("the bot got %q", got)
	}
	es := r.audit()
	if len(es) != 2 || es[0].Actor != "rule:lasso-may-message-fake" || es[0].Action != string(rules.SendMessage) || es[1].Result != audit.OK {
		t.Fatalf("audit = %+v", es)
	}
}

func TestAssistantSendWithNoRuleIsRefused(t *testing.T) {
	bot := &factBot{fakeBot: fakeBot{prompts: []string{"p1"}}}
	r := startRigWith(t, testRules, bot) // a rule for answers, none for messages
	r.eventually("the session on In progress", func() bool { return slices.Contains(r.ids(api.TabDoing), "session:fake:s-p1") })

	pane, m := r.assistant(toolCall("send_to_bot", map[string]string{"session": "session:fake:s-p1", "text": "stop"}))
	if _, err := pane.Send(context.Background(), "tell it to stop"); err != nil {
		t.Fatal(err)
	}
	res := m.result(t)
	if !res.IsError || !strings.Contains(res.Content, "REFUSED") || !strings.Contains(res.Content, "No rule lets fake-s-p1 send messages to bot sessions.") {
		t.Fatalf("tool result = %+v", res)
	}
	if got := bot.messages(); len(got) != 0 {
		t.Fatalf("sent anyway: %q", got)
	}
	if es := r.audit(); len(es) != 0 {
		t.Fatalf("a refused send was logged: %+v", es)
	}
}

const answerRules = `
[[rules]]
id = "fake-answers"
who = "fake-*"
action = "answer_prompt"
`

// The bot's prompt would force-push. A rule lets it answer its prompts,
// but the ask carries the effect from the adapter's facts, and the
// always-ask list stops the assistant's yes.
func TestAssistantAnswerThatForcePushesIsRefused(t *testing.T) {
	bot := &factBot{fakeBot: fakeBot{prompts: []string{"p1"}}, command: "git push --force origin feature"}
	r := startRigWith(t, answerRules, bot)
	r.eventually("the ask on In progress", func() bool { return slices.Contains(r.ids(api.TabDoing), "doing:bot:fake:p1") })

	ask := r.focus("doing:bot:fake:p1")
	if got := fieldOf(ask, "Effects"); got != string(rules.ForcePush) {
		t.Fatalf("ask Effects = %q, fields %+v", got, ask.Fields)
	}
	if got := fieldOf(ask, "Paths"); got != "main.go" {
		t.Fatalf("ask Paths = %q", got)
	}

	pane, m := r.assistant(toolCall("answer", map[string]string{"prompt_id": "p1", "choice": "1"}))
	if _, err := pane.Send(context.Background(), "say yes"); err != nil {
		t.Fatal(err)
	}
	res := m.result(t)
	if !res.IsError || !strings.Contains(res.Content, "REFUSED") || !strings.Contains(res.Content, "Always ask: it force-pushes") {
		t.Fatalf("tool result = %+v", res)
	}
	if got := bot.answered(); len(got) != 0 {
		t.Fatalf("answered anyway: %q", got)
	}

}
