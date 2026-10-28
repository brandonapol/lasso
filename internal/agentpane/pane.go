// Package agentpane is lasso's assistant, the left pane (ADR 0001 §5–§7).
//
// It is a chat with claude-sonnet-5-5 whose tools are calls to the daemon's
// own API, made with the agent pane's token. That token is the whole of its
// power: the API runs its actions through the rules engine (never as the
// user), refuses it the chat and undo endpoints, and offers no way to write
// rules. So the assistant can't widen its own permissions; when the rules
// say ask, it has to ask the user in chat.
//
// It speaks unprompted only twice: one short "since you left" message when
// a client opens, and one line per line-jumper (ADR 0001 §5).
package agentpane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/llm"
)

// Model is the part of *llm.Client the pane uses.
type Model interface {
	Converse(ctx context.Context, conv llm.Conversation) (llm.Turn, error)
}

// Store is the part of lasso's database the pane uses (*store.Store): the
// conversation is kept in the cache table, which survives restarts, and the
// brief cursor marks when the user last read "since you left".
type Store interface {
	CacheGet(key string, now time.Time) ([]byte, bool, error)
	CachePut(key string, value []byte, now time.Time, ttl time.Duration) error
	Cursor(name string) (string, error)
	SetCursor(name, value string) error
}

// Options configures New.
type Options struct {
	// API is the daemon's base URL ("http://127.0.0.1:7337") and Token the
	// agent pane's token for it. Never the user's token.
	API   string
	Token string
	HTTP  *http.Client
	// Model returns the model to talk to. It is called for every message,
	// so a key saved after the daemon started is picked up. llm.ErrNoKey
	// makes the pane say how to set one.
	Model func() (Model, error)
	// KeyHelp is how to set the key, shown when there is none.
	KeyHelp string
	Store   Store
	// Notify gets api.EventChatUpdated whenever the conversation grows.
	Notify func(api.Event)
	Now    func() time.Time
	Logf   func(format string, args ...any)
}

// Limits.
const (
	// MaxTurns is how many messages the conversation keeps.
	MaxTurns = 50
	// contextTurns is how many of them go to the model with a new message.
	contextTurns = 20
	// maxRounds caps tool-use round trips for one message.
	maxRounds = 8
	// reopenQuiet: opening again this soon after the last "since you left"
	// adds no new one.
	reopenQuiet = 10 * time.Minute

	stateKey = "agentpane.chat"
	stateTTL = 100 * 365 * 24 * time.Hour
)

// DefaultKeyHelp says how to give lasso a key.
const DefaultKeyHelp = "I can't talk yet: lasso has no Anthropic API key. Set ANTHROPIC_API_KEY where the daemon runs and restart it, " +
	"or save the key in ~/.config/lasso/anthropic_key (chmod 600) and send your message again."

// Pane is the assistant. It implements api.Chat.
type Pane struct {
	opts Options
	api  *client
	now  func() time.Time

	mu sync.Mutex // one turn at a time; guards the stored state
}

var _ api.Chat = (*Pane)(nil)

// state is what the pane keeps in the store.
type state struct {
	Messages []api.ChatMessage `json:"messages"`
	// Announced are the line-jumpers already mentioned, by item id.
	Announced map[string]time.Time `json:"announced,omitempty"`
	// LastOpen is when the last "since you left" was written.
	LastOpen time.Time `json:"last_open,omitzero"`
}

// New returns a pane.
func New(opts Options) *Pane {
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.KeyHelp == "" {
		opts.KeyHelp = DefaultKeyHelp
	}
	if opts.Model == nil {
		opts.Model = func() (Model, error) { return nil, llm.ErrNoKey }
	}
	return &Pane{
		opts: opts,
		api:  &client{base: strings.TrimRight(opts.API, "/"), token: opts.Token, http: opts.HTTP},
		now:  opts.Now,
	}
}

func (p *Pane) logf(format string, args ...any) {
	if p.opts.Logf != nil {
		p.opts.Logf(format, args...)
	}
}

func (p *Pane) load() (state, error) {
	var st state
	b, ok, err := p.opts.Store.CacheGet(stateKey, p.now())
	if err != nil || !ok {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		// A damaged conversation is only lost history; start again.
		p.logf("lasso: agent pane: dropping unreadable history: %v", err)
		return state{}, nil
	}
	return st, nil
}

func (p *Pane) save(st *state) error {
	if n := len(st.Messages); n > MaxTurns {
		st.Messages = append([]api.ChatMessage(nil), st.Messages[n-MaxTurns:]...)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := p.opts.Store.CachePut(stateKey, b, p.now(), stateTTL); err != nil {
		return err
	}
	if p.opts.Notify != nil {
		p.opts.Notify(api.Event{Kind: api.EventChatUpdated, Data: map[string]int{"messages": len(st.Messages)}})
	}
	return nil
}

func (p *Pane) say(st *state, kind, text string) {
	st.Messages = append(st.Messages, api.ChatMessage{Role: api.RoleLasso, Kind: kind, Text: text, At: p.now()})
}

// History implements api.Chat.
func (p *Pane) History(context.Context) ([]api.ChatMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := p.load()
	return st.Messages, err
}

// Send implements api.Chat: it adds the user's message, runs the model
// (and its tool calls) and adds the reply.
func (p *Pane) Send(ctx context.Context, text string) ([]api.ChatMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := p.load()
	if err != nil {
		return nil, err
	}
	st.Messages = append(st.Messages, api.ChatMessage{Role: api.RoleUser, Text: text, At: p.now()})
	p.say(&st, "", p.reply(ctx, st.Messages))
	return st.Messages, p.save(&st)
}

// reply runs the conversation to the model's final answer. Problems become
// a plain message rather than an error, so the user always hears back.
func (p *Pane) reply(ctx context.Context, history []api.ChatMessage) string {
	m, err := p.opts.Model()
	if err != nil {
		return p.problem(err)
	}
	msgs := conversation(history)
	var said []string
	for round := 0; round < maxRounds; round++ {
		turn, err := m.Converse(ctx, llm.Conversation{System: systemPrompt, Messages: msgs, Tools: tools})
		if err != nil {
			return p.problem(err)
		}
		if t := strings.TrimSpace(turn.Text()); t != "" {
			said = append(said, t)
		}
		calls := turn.ToolCalls()
		if len(calls) == 0 {
			break
		}
		msgs = append(msgs, llm.Message{Role: "assistant", Content: turn.Content})
		results := make([]llm.Block, 0, len(calls))
		for _, c := range calls {
			out, isErr := p.runTool(ctx, c)
			results = append(results, llm.ToolResult(c.ID, out, isErr))
		}
		msgs = append(msgs, llm.Message{Role: "user", Content: results})
		if round == maxRounds-1 {
			said = append(said, "I stopped there because it was taking many steps; ask again if you want me to carry on.")
		}
	}
	if len(said) == 0 {
		return "I don't have anything useful to add."
	}
	// Only the last thing it said is the answer; earlier text was narration
	// between tool calls ("Let me look").
	return tidy(said[len(said)-1])
}

func (p *Pane) problem(err error) string {
	switch {
	case errors.Is(err, llm.ErrNoKey):
		return p.opts.KeyHelp
	case errors.Is(err, llm.ErrOverCap):
		return "I've reached today's spending cap for the assistant, so I'll stay quiet until tomorrow. The tabs still work; raise sonnet_daily_usd in config.toml if you want more."
	case errors.Is(err, context.Canceled):
		return "That was cancelled before I could answer."
	}
	p.logf("lasso: agent pane: %v", err)
	return "I couldn't reach the model just now (" + clip(err.Error(), 200) + "). Try again in a moment."
}

// conversation turns the stored history into Messages API turns: the last
// contextTurns messages, same-speaker runs merged, starting with the user.
func conversation(history []api.ChatMessage) []llm.Message {
	if n := len(history); n > contextTurns {
		history = history[n-contextTurns:]
	}
	var out []llm.Message
	for _, m := range history {
		role := "user"
		if m.Role == api.RoleLasso {
			role = "assistant"
		}
		if len(out) > 0 && out[len(out)-1].Role == role {
			last := &out[len(out)-1].Content[0]
			last.Text += "\n\n" + m.Text
			continue
		}
		out = append(out, llm.Message{Role: role, Content: []llm.Block{llm.TextBlock(m.Text)}})
	}
	if len(out) > 0 && out[0].Role == "assistant" {
		out = append([]llm.Message{{Role: "user", Content: []llm.Block{llm.TextBlock("(I opened lasso.)")}}}, out...)
	}
	return out
}

// Open implements api.Chat: it writes the one "since you left" message,
// unless one was written moments ago, and marks the brief read.
func (p *Pane) Open(ctx context.Context) ([]api.ChatMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := p.load()
	if err != nil {
		return nil, err
	}
	now := p.now()
	if !st.LastOpen.IsZero() && now.Sub(st.LastOpen) < reopenQuiet {
		return st.Messages, nil
	}
	b, err := p.gather(ctx)
	if err != nil {
		return st.Messages, fmt.Errorf("since you left: %w", err)
	}
	p.say(&st, "since_you_left", p.sinceYouLeft(ctx, b))
	st.LastOpen = now
	if err := p.markRead(now); err != nil {
		p.logf("lasso: agent pane: couldn't mark the brief read: %v", err)
	}
	return st.Messages, p.save(&st)
}
