package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Event kinds the API itself pushes over GET /events, alongside the
// source's.
const (
	// EventFocus asks the clients to show one item in the focus pane. Its
	// data is {"id": ..., "tab": ...}. The agent pane's focus_item sends it
	// through POST /focus.
	EventFocus = "focus"
	// EventChatUpdated says the agent pane's conversation has new messages;
	// clients re-read GET /chat.
	EventChatUpdated = "chat.updated"
)

// Chat roles.
const (
	RoleUser  = "user"
	RoleLasso = "lasso"
)

// ChatMessage is one message in the agent pane's conversation.
type ChatMessage struct {
	Role string `json:"role"` // RoleUser or RoleLasso
	Text string `json:"text"`
	// Kind marks lasso's unprompted messages: "since_you_left" and
	// "line_jumper". Empty for ordinary replies.
	Kind string    `json:"kind,omitempty"`
	At   time.Time `json:"at"`
}

// Chat is the agent pane (internal/agentpane). Each method returns the
// whole conversation afterwards, oldest first.
type Chat interface {
	// History is the conversation so far.
	History(ctx context.Context) ([]ChatMessage, error)
	// Send adds the user's message and lasso's reply.
	Send(ctx context.Context, text string) ([]ChatMessage, error)
	// Open is called when a client opens: it may add one short "since you
	// left" message.
	Open(ctx context.Context) ([]ChatMessage, error)
}

// Hub fans events the API makes (focus requests, chat updates) out to every
// open GET /events stream.
type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewHub returns an empty Hub.
func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

// Publish sends ev to every subscriber. A subscriber that has fallen too
// far behind is dropped, so its stream closes and the client re-reads.
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// Subscribe returns a channel of events and a func to stop.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// maxChatText caps one chat message from the user.
const maxChatText = 8 << 10

// chatRoutes adds GET /chat, POST /chat, POST /chat/open and POST /focus.
//
// Only the user may use the chat endpoints: a message there is the user's
// own words, and the agent pane must never be able to put words in the
// user's mouth (say, "yes, go ahead"). Either caller may ask for a focus.
func chatRoutes(mux *http.ServeMux, src Source, chat Chat, hub *Hub) {
	userOnly := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if CallerFrom(r.Context()) != CallerUser {
				http.Error(w, "only you can use the chat", http.StatusForbidden)
				return
			}
			if chat == nil {
				http.Error(w, "the assistant isn't running in this daemon", http.StatusNotImplemented)
				return
			}
			next(w, r)
		}
	}
	reply := func(w http.ResponseWriter, msgs []ChatMessage, err error) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if msgs == nil {
			msgs = []ChatMessage{}
		}
		writeJSON(w, map[string]any{"messages": msgs})
	}
	mux.HandleFunc("GET /chat", userOnly(func(w http.ResponseWriter, r *http.Request) {
		msgs, err := chat.History(r.Context())
		reply(w, msgs, err)
	}))
	mux.HandleFunc("POST /chat", userOnly(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatText+1024))
		if err := dec.Decode(&body); err != nil {
			http.Error(w, "bad message: "+err.Error(), http.StatusBadRequest)
			return
		}
		body.Text = strings.TrimSpace(body.Text)
		if body.Text == "" || len(body.Text) > maxChatText {
			http.Error(w, "a message needs some text, and at most 8 KiB of it", http.StatusBadRequest)
			return
		}
		msgs, err := chat.Send(r.Context(), body.Text)
		reply(w, msgs, err)
	}))
	mux.HandleFunc("POST /chat/open", userOnly(func(w http.ResponseWriter, r *http.Request) {
		msgs, err := chat.Open(r.Context())
		reply(w, msgs, err)
	}))
	mux.HandleFunc("POST /focus", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.ID == "" {
			http.Error(w, "focus needs an item id", http.StatusBadRequest)
			return
		}
		f, err := src.Focus(r.Context(), body.ID)
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "no such item", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		hub.Publish(Event{Kind: EventFocus, Data: map[string]string{"id": f.ID, "tab": string(f.Tab)}})
		writeJSON(w, f)
	})
}
