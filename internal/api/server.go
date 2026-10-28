// Package api is the daemon's local HTTP API (ADR 0001 §1): JSON endpoints
// for the tabs and the focus pane, and a Server-Sent Events stream of
// changes. Both clients, the TUI and the web view, talk only to it.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/brandonapol/lasso/internal/rules"
)

// HealthBody is what GET /healthz answers, so a client can tell lasso's
// daemon from something else on the port.
const HealthBody = "lasso ok\n"

// Options configures Handler.
type Options struct {
	// Token is the user's bearer token. Every request but /healthz must
	// carry it or AgentToken.
	Token string
	// AgentToken, if set, is the agent pane's token. Requests carrying it
	// are CallerAgent: their actions always go through the rules, and they
	// can't undo. Empty means the agent pane can't call the API.
	AgentToken string
	// Port is the listener's port; it fixes the one Origin allowed.
	Port int
	// KeepAlive is how often /events sends a comment line to keep the
	// connection open (default 25s).
	KeepAlive time.Duration
	// Actions runs POST /actions (rules check, audit write, adapter call).
	// Nil answers 501.
	Actions Actions
	// Chat is the agent pane behind GET/POST /chat. Nil answers 501.
	Chat Chat
	// Hub carries the API's own events (focus requests, chat updates) to
	// GET /events. Nil makes a private one.
	Hub *Hub
}

// Actions is what POST /actions and POST /actions/{id}/undo call;
// *rules.Engine implements it.
//
// Which method runs is decided by the caller's token, not the request:
// the user's token runs DoAsUser (the user is the approval), anything else
// runs Do (the rules decide). Only the user may Undo.
type Actions interface {
	Do(ctx context.Context, req rules.Request) (rules.Outcome, error)
	DoAsUser(ctx context.Context, req rules.Request) (rules.Outcome, error)
	Undo(ctx context.Context, id string) (rules.Outcome, error)
}

// writeOutcome answers with the outcome and a status saying how it went.
func writeOutcome(w http.ResponseWriter, out rules.Outcome, err error) {
	code := http.StatusOK
	switch {
	case err == nil:
	case errors.Is(err, rules.ErrRefused):
		code = http.StatusForbidden
	case errors.Is(err, rules.ErrUnknown):
		code = http.StatusNotFound
	case errors.Is(err, rules.ErrNoUndo):
		code = http.StatusConflict
	case errors.Is(err, rules.ErrAudit):
		code = http.StatusServiceUnavailable
	case errors.Is(err, rules.ErrFailed):
		code = http.StatusBadGateway
	default:
		code = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(out)
}

// Handler serves the API over src.
func Handler(src Source, opts Options) http.Handler {
	if opts.KeepAlive <= 0 {
		opts.KeepAlive = 25 * time.Second
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, HealthBody)
	})
	mux.HandleFunc("GET /items", func(w http.ResponseWriter, r *http.Request) {
		tab := Tab(r.URL.Query().Get("tab"))
		if !tab.Valid() {
			http.Error(w, "tab must be one of issues, actions, doing, summaries", http.StatusBadRequest)
			return
		}
		items, err := src.Items(r.Context(), tab)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"tab": tab, "items": items})
	})
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		f, err := src.Focus(r.Context(), r.PathValue("id"))
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "no such item", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, f)
	})
	if opts.Actions == nil {
		mux.HandleFunc("POST /actions", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "rules engine not built (#5)", http.StatusNotImplemented)
		})
	} else {
		mux.HandleFunc("POST /actions", func(w http.ResponseWriter, r *http.Request) {
			var req rules.Request
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil {
				http.Error(w, "bad action: "+err.Error(), http.StatusBadRequest)
				return
			}
			var out rules.Outcome
			var err error
			if CallerFrom(r.Context()) == CallerUser {
				out, err = opts.Actions.DoAsUser(r.Context(), req)
			} else {
				out, err = opts.Actions.Do(r.Context(), req)
			}
			writeOutcome(w, out, err)
		})
		mux.HandleFunc("POST /actions/{id}/undo", func(w http.ResponseWriter, r *http.Request) {
			if CallerFrom(r.Context()) != CallerUser {
				// An undo is recorded as the user's own decision.
				writeOutcome(w, rules.Outcome{Decision: rules.Decision{Reason: "Only you can undo an action; the agent pane can ask you to."}}, rules.ErrRefused)
				return
			}
			out, err := opts.Actions.Undo(r.Context(), r.PathValue("id"))
			writeOutcome(w, out, err)
		})
	}
	if opts.Hub == nil {
		opts.Hub = NewHub()
	}
	chatRoutes(mux, src, opts.Chat, opts.Hub)
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		serveEvents(w, r, src, opts.Hub, opts.KeepAlive)
	})
	tokens := map[Caller]string{CallerUser: opts.Token, CallerAgent: opts.AgentToken}
	return guard(mux, tokens, opts.Port, map[string]bool{"/healthz": true})
}

// serveEvents streams src's and hub's events as Server-Sent Events until
// the client goes away or either drops it.
func serveEvents(w http.ResponseWriter, r *http.Request, src Source, hub *Hub, keepAlive time.Duration) {
	rc := http.NewResponseController(w)
	ch, stop := src.Subscribe()
	defer stop()
	hch, hstop := hub.Subscribe()
	defer hstop()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": lasso events\n\n")
	if err := rc.Flush(); err != nil {
		return
	}
	tick := time.NewTicker(keepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			fmt.Fprint(w, ": keep-alive\n\n")
		case ev, ok := <-ch:
			if !ok || writeEvent(w, ev) != nil {
				return
			}
		case ev, ok := <-hch:
			if !ok || writeEvent(w, ev) != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

func writeEvent(w http.ResponseWriter, ev Event) error {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, data)
	return err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
