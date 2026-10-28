// Package daemon runs lasso's daemon (lassod) and starts it in the
// background for a client that finds none running (ADR 0001 §1).
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/audit"
	"github.com/brandonapol/lasso/internal/config"
	"github.com/brandonapol/lasso/internal/digest"
	"github.com/brandonapol/lasso/internal/github"
	"github.com/brandonapol/lasso/internal/jev"
	"github.com/brandonapol/lasso/internal/llm"
	"github.com/brandonapol/lasso/internal/rules"
	"github.com/brandonapol/lasso/internal/source"
	"github.com/brandonapol/lasso/internal/store"
	"github.com/brandonapol/lasso/internal/summary"
)

// DBFile is the daemon's database in stateDir.
func DBFile(stateDir string) string { return filepath.Join(stateDir, "lasso.db") }

// AuditFile is the audit log in stateDir.
func AuditFile(stateDir string) string { return filepath.Join(stateDir, "audit.jsonl") }

// backend is the real source and actions, and how to stop them.
type backend struct {
	src     *source.Source
	actions *actions
	repos   []string // configured hot repos, for the web view's header
	stop    func()
}

// startBackend opens the store, builds the bot adapters, the source and
// the rules engine, and starts the source polling. stop ends polling and
// closes the store.
func startBackend(ctx context.Context, opts Options) (*backend, error) {
	cfg := opts.Config
	if cfg == nil {
		c, err := config.Load()
		if err != nil {
			return nil, err
		}
		cfg = &c
	}
	cfgDir, dirErr := config.Dir()
	cfgPath := "~/.config/lasso/config.toml"
	if dirErr == nil {
		cfgPath = filepath.Join(cfgDir, "config.toml")
	}
	rulesPath := opts.RulesPath
	if rulesPath == "" {
		if dirErr != nil {
			return nil, dirErr
		}
		rulesPath = filepath.Join(cfgDir, "rules.toml")
	}
	runner := opts.Runner
	if runner == nil {
		runner = github.CLI{}
	}
	logf := opts.Logf
	if logf == nil {
		logf = log.Printf
	}
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}
	st, err := store.Open(DBFile(opts.StateDir))
	if err != nil {
		return nil, err
	}

	// The adapters report trouble to the source, which is built after
	// them; reports only come from its polling, which starts below.
	var src *source.Source
	adapters := opts.Adapters
	if adapters == nil {
		adapters = buildAdapters(cfg, env, func(kind, msg string) { src.Report(kind, reachable(msg)) })
	}
	byKind := map[string]adapter.Adapter{}
	for _, a := range adapters {
		byKind[a.Kind()] = a
	}
	// Messages held for open sessions live in lasso.db; the adapters
	// reload them now, before any polling, and send them once their
	// sessions close.
	auditLog := audit.Open(AuditFile(opts.StateDir))
	useHeldStore(adapters, heldStore{st: st, log: auditLog, now: time.Now}, logf)
	fail := func(err error) (*backend, error) {
		closeAdapters(adapters, logf)
		_ = st.Close()
		return nil, err
	}

	// The models: Haiku for the digest and sorting, Sonnet for Summaries,
	// Jev to cut first. Each is off without its key; keyNotice says so.
	keyDir := cfgDir
	if dirErr != nil {
		keyDir = ""
	}
	key, keyErr := llm.LoadKey(env, keyDir)
	var haiku, sonnet *llm.Client
	if keyErr == nil {
		if haiku, err = llm.New(llm.Options{
			Key: key, Model: llm.Haiku, BaseURL: opts.AnthropicURL, DailyUSD: cfg.Models.HaikuDailyUSD,
			Spend: llm.StoreSpend{S: st, Feature: featureDigest},
		}); err != nil {
			return fail(err)
		}
		if sonnet, err = llm.New(llm.Options{
			Key: key, Model: llm.Sonnet, BaseURL: opts.AnthropicURL, DailyUSD: cfg.Models.SonnetDailyUSD,
			Spend: llm.StoreSpend{S: st, Feature: featureSummary},
		}); err != nil {
			return fail(err)
		}
	}
	jc := jev.New(cfg.Jev, env, jev.StoreSpend{S: st})
	dg := digest.New(digest.Options{LLM: haiku, Jev: jc})
	fetch := opts.SummaryFetch
	if fetch == nil {
		fetch = summary.GH{Runner: runner, Cache: st}
	}
	sums := summary.New(summary.Options{
		Fetch: fetch, Cache: st, LLM: sonnet, Jev: jc,
		Briefs: func(repo string, since time.Time) []string { return src.Briefs(repo, since) },
	})
	notices := keyNotice(st, keyErr, jc == nil && !cfg.Jev.Disabled, keyDir, time.Now())
	repos := newRepoFinder(byKind, opts.gitRemote)

	suggest := func(now time.Time) ([]rules.Suggestion, error) {
		as, err := st.Answers(now.Add(-rules.SuggestWindow))
		if err != nil {
			return nil, err
		}
		f, err := rules.LoadFile(rulesPath)
		if err != nil {
			return nil, err
		}
		answers := make([]rules.Answer, len(as))
		for i, a := range as {
			answers[i] = rules.Answer{Bot: a.Bot, Action: rules.Action(a.Action), Choice: a.Choice, Repo: a.Repo, At: a.At}
		}
		return rules.Suggest(answers, f, now), nil
	}
	src = source.New(source.Options{
		Repos: cfg.HotRepos, Runner: runner, Cache: st, Cursors: st,
		ConfigPath: cfgPath, Logf: logf, Adapters: adapters, Suggest: suggest,
		Sorter: dg, Digest: dg, Summaries: sums, SessionRepo: repos.find, PromptFacts: askFacts(byKind), Notices: notices,
	})
	acts := &actions{
		eng:      rules.NewEngine(rulesPath, auditLog, executor{adapters: byKind}, time.Now),
		adapters: byKind, answers: st, resolve: src.Resolve, wake: src.Wake, now: time.Now, logf: logf,
		summaries: sums, repos: cfg.HotRepos,
	}

	pctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = src.Run(pctx)
	}()
	if len(cfg.HotRepos) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			summaryLoop(pctx, sums, cfg.HotRepos, src.Wake, logf)
		}()
	}
	watch(pctx, adapters, src, logf)
	return &backend{src: src, actions: acts, repos: cfg.HotRepos, stop: func() {
		cancel()
		wg.Wait()
		// Close the relay session and leader connections; held messages
		// stay in the store for the next start.
		closeAdapters(adapters, logf)
		_ = st.Close()
	}}, nil

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
	// Actions runs POST /actions when Source is set; nil answers 501. When
	// Source is nil, Run builds the real one: the rules engine over
	// RulesPath and StateDir/audit.jsonl, sending through the adapters.
	Actions api.Actions
	// Adapters are the bot adapters for the real source and actions; nil
	// builds them from Config: saddle once [adapters.saddle] token_file
	// is set, Claude Code and grok when their directories exist (or
	// [adapters.claude] / [adapters.grok] enabled says so).
	Adapters []adapter.Adapter
	// RulesPath is rules.toml; empty is config.Dir()/rules.toml.
	RulesPath string
	// AgentToken is the agent pane's API token; empty makes a random one.
	// It is never written to disk.
	AgentToken string
	// Logf gets the real source's log lines; nil logs to stderr.
	Logf func(format string, args ...any)
	// Env reads the environment (API keys, GROK_HOME); nil is os.Getenv.
	Env func(string) string
	// AnthropicURL is the Messages API's base URL; empty is Anthropic's.
	AnthropicURL string
	// SummaryFetch reads a repo's last 24 hours for Summaries; nil is gh.
	SummaryFetch summary.Fetcher
	// gitRemote reads a directory's origin URL; nil runs git (tests).
	gitRemote func(dir string) (string, error)
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

	src, acts := opts.Source, opts.Actions
	var hotRepos []string
	if src == nil {
		b, err := startBackend(ctx, opts)
		if err != nil {
			return fmt.Errorf("lasso serve: %w", err)
		}
		defer b.stop()
		src, acts, hotRepos = b.src, b.actions, b.repos
	}
	agentToken := opts.AgentToken
	if agentToken == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return fmt.Errorf("lasso serve: %w", err)
		}
		agentToken = hex.EncodeToString(buf)
	}
	hub := api.NewHub()
	chat, stopChat, err := startAgentPane(ctx, opts, addr.Port, agentToken, hub)
	if err != nil {
		return fmt.Errorf("lasso serve: %w", err)
	}
	defer stopChat()

	srv := &http.Server{
		Handler: api.Handler(src, api.Options{
			Token: token, AgentToken: agentToken, Port: addr.Port, Actions: acts, Chat: chat, Hub: hub,
			HotRepos: hotRepos,
		}),
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
	if err := srv.Shutdown(sctx); err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		// Something still holds a connection open, such as a client that
		// connected and never sent a request (net/http waits 5s for those).
		// Cut it rather than fail the shutdown.
		_ = srv.Close()
	}
	return nil
}
