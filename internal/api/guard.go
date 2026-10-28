package api

// The Host, Origin and bearer-token checks here are adapted from saddle's
// internal/remote (server.go: ReposHandler, bearerToken, hostOnly;
// config.go: loopbackHost; token.go: the file-mode check). lasso copies
// rather than imports them because saddle's packages are internal.

import (
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

// guard refuses requests for a non-loopback Host (DNS rebinding) or from a
// foreign Origin with 403, and requests without the token with 401. Paths
// in open skip the token check but not the Host and Origin checks.
func guard(next http.Handler, token string, port int, open map[string]bool) http.Handler {
	origins := map[string]bool{
		"http://127.0.0.1:" + strconv.Itoa(port): true,
		"http://localhost:" + strconv.Itoa(port): true,
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(hostOnly(r.Host)) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !origins[o] {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if !open[r.URL.Path] {
			have := []byte(bearerToken(r))
			if len(want) == 0 || subtle.ConstantTimeCompare(have, want) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="lasso"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
