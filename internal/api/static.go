package api

// The web view (ADR 0001 §1, #14): the daemon serves the embedded static
// files, and a browser signs in by trading a one-time code for a session
// cookie.
//
// The flow: `lasso open` reads the user's token, asks POST /session/code
// for a code (bearer token only), and opens /login?code=…. That page sets
// an HttpOnly, SameSite=Strict, host-only cookie and redirects to /, so the
// code, which is spent by then, is the only secret that ever sits in a URL;
// the token never does. A session acts as CallerUser, never CallerAgent.
//
// A cookie is sent by the browser on its own, so the guard takes one only
// when the request is plainly the web view's own: Sec-Fetch-Site, when the
// browser sends it, must be same-origin (or none, for a typed URL), and a
// request that changes something must carry an allowed Origin.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/lasso/web"
)

const (
	// codeTTL is how long a one-time code from POST /session/code lasts.
	codeTTL = 60 * time.Second
	// sessionTTL is how long a web session lasts. Sessions live in the
	// daemon's memory, so a restart ends them too; `lasso open` again.
	sessionTTL = 7 * 24 * time.Hour
	// maxCodes and maxSessions bound what the daemon keeps; the oldest
	// goes first.
	maxCodes    = 16
	maxSessions = 64
)

// sessionKey marks a request authenticated by a web session cookie.
type sessionKey struct{}

// webAuth holds the outstanding one-time codes and the live web sessions,
// both keyed by the SHA-256 of the secret so a map lookup can't leak it by
// timing.
type webAuth struct {
	mu       sync.Mutex
	now      func() time.Time
	cookie   string
	codes    map[string]time.Time // hash → expiry
	sessions map[string]time.Time // hash → expiry
}

func newWebAuth(port int, now func() time.Time) *webAuth {
	if now == nil {
		now = time.Now
	}
	return &webAuth{
		now: now,
		// Cookies ignore ports, so the name carries it: two daemons on two
		// ports don't overwrite each other's session.
		cookie:   "lasso_session_" + strconv.Itoa(port),
		codes:    map[string]time.Time{},
		sessions: map[string]time.Time{},
	}
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// add stores a new secret in m with expiry exp, dropping expired entries
// and then the oldest past limit. w.mu must be held.
func (w *webAuth) add(m map[string]time.Time, limit int, exp time.Time) (string, error) {
	now := w.now()
	for k, e := range m {
		if !now.Before(e) {
			delete(m, k)
		}
	}
	for len(m) >= limit {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return m[keys[i]].Before(m[keys[j]]) })
		delete(m, keys[0])
	}
	s, err := randomSecret()
	if err != nil {
		return "", err
	}
	m[hashSecret(s)] = exp
	return s, nil
}

// mintCode returns a new one-time code.
func (w *webAuth) mintCode() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.add(w.codes, maxCodes, w.now().Add(codeTTL))
}

// redeem spends code and, if it was live, returns a new session id.
func (w *webAuth) redeem(code string) (string, bool) {
	if code == "" {
		return "", false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	h := hashSecret(code)
	exp, ok := w.codes[h]
	delete(w.codes, h) // spent either way
	if !ok || !w.now().Before(exp) {
		return "", false
	}
	id, err := w.add(w.sessions, maxSessions, w.now().Add(sessionTTL))
	if err != nil {
		return "", false
	}
	return id, true
}

// end forgets the session id.
func (w *webAuth) end(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.sessions, hashSecret(id))
}

// fromSession reports whether r carries a live session cookie in a way the
// web view itself would send it.
func (w *webAuth) fromSession(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		// same-site counts as foreign here: another port on 127.0.0.1 is
		// the same site, and cookies ignore ports.
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") == "" {
		// The guard already refused a foreign Origin; a change must also
		// prove it came from a page on one of ours.
		return false
	}
	c, err := r.Cookie(w.cookie)
	if err != nil || c.Value == "" {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	h := hashSecret(c.Value)
	exp, ok := w.sessions[h]
	if ok && !w.now().Before(exp) {
		delete(w.sessions, h)
		ok = false
	}
	return ok
}

// openPath reports whether path skips the token check: the health check,
// the sign-in exchange, and the web view's static files, none of which
// hold anything private.
func openPath(path string) bool {
	switch path {
	case "/healthz", "/login", "/":
		return true
	}
	return strings.HasPrefix(path, "/web/")
}

// pageHeaders are sent with the web view's own files.
func pageHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' https://fonts.googleapis.com; "+
		"font-src https://fonts.gstatic.com; connect-src 'self'; img-src 'self'; form-action 'self'; "+
		"frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-cache")
}

// webRoutes adds the web view's pages and its sign-in endpoints.
func webRoutes(mux *http.ServeMux, wa *webAuth) {
	files := web.FS()
	static := http.StripPrefix("/web/", http.FileServerFS(files))
	mux.HandleFunc("GET /web/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r) // no directory listings
			return
		}
		pageHeaders(w.Header())
		static.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		b, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		pageHeaders(w.Header())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
	// POST /session/code mints a one-time code. Only the user's bearer
	// token may: not the agent pane, and not a web session (one session
	// shouldn't be able to spawn more).
	mux.HandleFunc("POST /session/code", func(w http.ResponseWriter, r *http.Request) {
		if CallerFrom(r.Context()) != CallerUser || viaSession(r.Context()) {
			http.Error(w, "only your token can sign a browser in", http.StatusForbidden)
			return
		}
		code, err := wa.mintCode()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"code": code, "expires_in": int(codeTTL / time.Second)})
	})
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		pageHeaders(w.Header())
		w.Header().Set("Cache-Control", "no-store")
		id, ok := wa.redeem(r.URL.Query().Get("code"))
		if !ok {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("That sign-in link has expired or was already used. Run `lasso open` again.\n"))
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     wa.cookie,
			Value:    id,
			Path:     "/",
			MaxAge:   int(sessionTTL / time.Second),
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(wa.cookie); err == nil {
			wa.end(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: wa.cookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(http.StatusNoContent)
	})
}

// viaSession reports whether the request was authenticated by a web
// session cookie rather than a bearer token.
func viaSession(ctx context.Context) bool {
	v, _ := ctx.Value(sessionKey{}).(bool)
	return v
}
