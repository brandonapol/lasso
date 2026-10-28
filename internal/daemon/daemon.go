// Package daemon runs lasso's daemon (lassod) and starts it in the
// background for a client that finds none running (ADR 0001 §1).
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/config"
	"github.com/brandonapol/lasso/internal/github"
	"github.com/brandonapol/lasso/internal/source"
	"github.com/brandonapol/lasso/internal/store"
)

// DBFile is the daemon's database in stateDir.
func DBFile(stateDir string) string { return filepath.Join(stateDir, "lasso.db") }

// startSource opens the store and starts the real source polling. The
// returned func stops polling and closes the store.
func startSource(ctx context.Context, opts Options) (*source.Source, func(), error) {
	cfg := opts.Config
	if cfg == nil {
		c, err := config.Load()
		if err != nil {
			return nil, nil, err
		}
		cfg = &c
	}
	cfgPath := "~/.config/lasso/config.toml"
	if dir, err := config.Dir(); err == nil {
		cfgPath = filepath.Join(dir, "config.toml")
	}
	runner := opts.Runner
	if runner == nil {
		runner = github.CLI{}
	}
	logf := opts.Logf
	if logf == nil {
		logf = log.Printf
	}
	st, err := store.Open(DBFile(opts.StateDir))
	if err != nil {
		return nil, nil, err
	}
	src := source.New(source.Options{
		Repos: cfg.HotRepos, Runner: runner, Cache: st, Cursors: st,
		ConfigPath: cfgPath, Logf: logf,
	})
	pctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = src.Run(pctx)
	}()
	return src, func() {
		cancel()
		<-done
		_ = st.Close()
	}, nil
}

// Options configures Run.
type Options struct {
	// Port is the loopback port to listen on; 0 picks a free one (tests).
	Port int
	// TokenPath is the bearer token file, created on first run.
	TokenPath string
	// StateDir holds the pidfile.
	StateDir string
	// Source is what the API serves. When nil, Run builds the real one:
	// GitHub's hot repos through the attention rules (internal/source),
	// with its cache and cursors in lasso.db in StateDir.
	Source api.Source
	// Config is used to build the real source; nil loads config.toml.
	Config *config.Config
	// Runner starts gh for the real source; nil is the real gh.
	Runner github.Runner
	// Logf gets the real source's log lines; nil logs to stderr.
	Logf func(format string, args ...any)
	// Ready, if set, is called with the bound address once listening.
	Ready func(addr *net.TCPAddr)
}

// PidFile is the daemon's pidfile in stateDir.
func PidFile(stateDir string) string { return filepath.Join(stateDir, "lasso.pid") }

// Run serves the API on 127.0.0.1 until ctx is done. It refuses to start if
// the token file is readable by others or the port is taken.
func Run(ctx context.Context, opts Options) error {
	token, err := api.LoadToken(opts.TokenPath)
	if err != nil {
		return fmt.Errorf("lasso serve: token: %w", err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port)))
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("lasso serve: port %d on 127.0.0.1 is already in use (another lasso, or something else); "+
			"stop it or set [api] port in config.toml", opts.Port)
	}
	if err != nil {
		return fmt.Errorf("lasso serve: %w", err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().(*net.TCPAddr)

	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return err
	}
	pid := strconv.Itoa(os.Getpid())
	pidPath := PidFile(opts.StateDir)
	if err := os.WriteFile(pidPath, []byte(pid+"\n"), 0o600); err != nil {
		return err
	}
	defer func() {
		// Remove the pidfile only if it is still ours.
		if b, err := os.ReadFile(pidPath); err == nil && strings.TrimSpace(string(b)) == pid {
			_ = os.Remove(pidPath)
		}
	}()

	src := opts.Source
	if src == nil {
		live, stopSource, err := startSource(ctx, opts)
		if err != nil {
			return fmt.Errorf("lasso serve: %w", err)
		}
		defer stopSource()
		src = live
	}

	srv := &http.Server{
		Handler:           api.Handler(src, api.Options{Token: token, Port: addr.Port}),
		ReadHeaderTimeout: 10 * time.Second,
		// Request contexts end with ctx, so open event streams close on
		// shutdown instead of holding it up.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if opts.Ready != nil {
		opts.Ready(addr)
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
