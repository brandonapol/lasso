package api

// The Host, Origin and bearer-token checks here are adapted from saddle's
// internal/remote (server.go: ReposHandler, bearerToken, hostOnly;
// config.go: loopbackHost; token.go: the file-mode check). lasso copies
// rather than imports them because saddle's packages are internal.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Caller is who an authenticated request comes from, decided by which
// token it carried, never by anything in its body.
type Caller string

// The callers.
const (
	// CallerNone: no token (only open paths like /healthz).
	CallerNone Caller = ""
	// CallerUser: the user's own token file, which the TUI reads and the
	// web view gets through `lasso open`. What it asks for is the user's
	// decision.
	CallerUser Caller = "user"
	// CallerAgent: the agent pane's token, which the daemon makes in memory
	// and hands only to the agent pane. It can never act as the user.
	CallerAgent Caller = "agent"
)

type callerKey struct{}

// CallerFrom returns the caller guard authenticated for this request.
func CallerFrom(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}

// guard refuses requests for a non-loopback Host (DNS rebinding) or from a
// foreign Origin with 403, and requests without a known token or web
// session with 401. Paths open reports true for skip the token check but
// not the Host and Origin checks. The token a request carried decides its
// Caller; a web session (see static.go) is always CallerUser.
func guard(next http.Handler, tokens map[Caller]string, port int, open func(string) bool, web *webAuth) http.Handler {
	origins := map[string]bool{
		"http://127.0.0.1:" + strconv.Itoa(port): true,
		"http://localhost:" + strconv.Itoa(port): true,
	}
	if tokens[CallerUser] != "" && tokens[CallerUser] == tokens[CallerAgent] {
		// One token for both would let the agent pass as the user.
		delete(tokens, CallerAgent)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(hostOnly(r.Host)) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !origins[o] {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if !open(r.URL.Path) {
			caller := authenticate([]byte(bearerToken(r)), tokens)
			ctx := r.Context()
			if caller == CallerNone && web != nil && web.fromSession(r) {
				caller = CallerUser
				ctx = context.WithValue(ctx, sessionKey{}, true)
			}
			if caller == CallerNone {
				w.Header().Set("WWW-Authenticate", `Bearer realm="lasso"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			r = r.WithContext(context.WithValue(ctx, callerKey{}, caller))
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate returns the caller whose token have is, comparing against
// every token in constant time.
func authenticate(have []byte, tokens map[Caller]string) Caller {
	found := CallerNone
	for _, c := range []Caller{CallerUser, CallerAgent} {
		want := []byte(tokens[c])
		if len(want) > 0 && subtle.ConstantTimeCompare(have, want) == 1 {
			found = c
		}
	}
	return found
}

func bearerToken(r *http.Request) string {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LoadToken reads the bearer token at path, creating it (0600, 32 random
// bytes as hex) if it doesn't exist. It refuses a file that group or world
// can read or write.
func LoadToken(path string) (string, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createToken(path)
	}
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by others (mode %v); run chmod 600 on it", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("%s is empty; delete it and lasso will make a new one", path)
	}
	return tok, nil
}

func createToken(path string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	// Write a temp file and hard-link it into place, so a concurrent reader
	// never sees a half-written token and two first runs agree on one.
	f, err := os.CreateTemp(filepath.Dir(path), ".token-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Link(f.Name(), path); errors.Is(err, fs.ErrExist) {
		return LoadToken(path)
	} else if err != nil {
		return "", err
	}
	return tok, nil
}
