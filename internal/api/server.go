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
	// Token is the bearer token every request but /healthz must carry.
	Token string
	// Port is the listener's port; it fixes the one Origin allowed.
	Port int
	// KeepAlive is how often /events sends a comment line to keep the
	// connection open (default 25s).
	KeepAlive time.Duration
	// Actions runs POST /actions (rules check, audit write, adapter call).
	// Nil answers 501.
	Actions Actions
}

// Actions is what POST /actions and POST /actions/{id}/undo call;
// *rules.Engine implements it.
type Actions interface {
	Do(ctx context.Context, req rules.Request) (rules.Outcome, error)
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
			out, err := opts.Actions.Do(r.Context(), req)
			writeOutcome(w, out, err)
		})
		mux.HandleFunc("POST /actions/{id}/undo", func(w http.ResponseWriter, r *http.Request) {
			out, err := opts.Actions.Undo(r.Context(), r.PathValue("id"))
			writeOutcome(w, out, err)
		})
	}
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		serveEvents(w, r, src, opts.KeepAlive)
	})
	return guard(mux, opts.Token, opts.Port, map[string]bool{"/healthz": true})
}

// serveEvents streams src's changes as Server-Sent Events until the client
// goes away or the source drops it.
func serveEvents(w http.ResponseWriter, r *http.Request, src Source, keepAlive time.Duration) {
	rc := http.NewResponseController(w)
	ch, stop := src.Subscribe()
	defer stop()
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
			if !ok {
				return
			}
			data, err := json.Marshal(ev.Data)
			if err != nil {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, data)
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
