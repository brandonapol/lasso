package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/adapter/claude"
	"github.com/brandonapol/lasso/internal/adapter/grok"
	"github.com/brandonapol/lasso/internal/adapter/saddle"
	"github.com/brandonapol/lasso/internal/config"
	"github.com/brandonapol/lasso/internal/llm"
	"github.com/brandonapol/lasso/internal/source"
	"github.com/brandonapol/lasso/internal/store"
	"github.com/brandonapol/lasso/internal/summary"
)

// Spend features: each model use has its own daily cap (ADR 0001 §7).
const (
	// featureDigest is the digest's (and the sorter's) Haiku spend, capped
	// at [models] haiku_daily_usd.
	featureDigest = "digest"
	// featureSummary is the Summaries tab's Sonnet spend, capped at
	// [models] sonnet_daily_usd.
	featureSummary = "summary"
)

// summaryEvery is how often the hot repos' summaries are checked. Refresh
// only writes one that is over summary.Interval (an hour) old.
const summaryEvery = 10 * time.Minute

// watchGap is the least time between two "a transcript changed" reads.
const watchGap = 2 * time.Second

// buildAdapters builds the bot adapters config turns on: saddle once its
// token file is set, Claude Code and grok when their directories exist
// (or [adapters.claude] / [adapters.grok] enabled says so). Each one's
// trouble goes to report.
func buildAdapters(cfg *config.Config, env func(string) string, report func(kind, msg string)) []adapter.Adapter {
	var out []adapter.Adapter
	if cfg.Adapters.Saddle.TokenFile != "" {
		out = append(out, saddle.New(cfg.Adapters.Saddle, saddle.Options{Report: func(msg string) { report(saddle.Kind, msg) }}))
	}
	home, _ := os.UserHomeDir()
	if dir, on := cfg.Adapters.Claude.Resolve(home, config.ClaudeDir(home)); on {
		out = append(out, claude.New(claude.Options{Root: dir}))
	}
	if dir, on := cfg.Adapters.Grok.Resolve(home, config.GrokDir(home, env)); on {
		out = append(out, grok.New(grok.Options{Home: dir, Report: func(msg string) { report(grok.Kind, msg) }}))
	}
	return out
}

// reachable turns an adapter's "reachable again" report into the source's
// "" (trouble cleared).
func reachable(msg string) string {
	if strings.HasSuffix(msg, " is reachable again") {
		return ""
	}
	return msg
}

// watcher is an adapter that can say when its files change (Claude Code).
type watcher interface {
	Watch(ctx context.Context) (<-chan struct{}, error)
}

// watch makes src read the bots soon after a watching adapter's files
// change, at most once per watchGap. Adapters without Watch are polled.
func watch(ctx context.Context, adapters []adapter.Adapter, src *source.Source, logf func(string, ...any)) {
	for _, a := range adapters {
		w, ok := a.(watcher)
		if !ok {
			continue
		}
		ch, err := w.Watch(ctx)
		if err != nil {
			logf("lasso: %s: can't watch for changes, reading every %s instead: %v", a.Kind(), source.BotsEvery, err)
			continue
		}
		go func() {
			for range ch {
				src.WakeBots()
				select {
				case <-ctx.Done():
					return
				case <-time.After(watchGap):
				}
			}
		}()
	}
}

// cwder is an adapter that knows where a session works (claude, grok).
type cwder interface {
	Cwd(sessionID string) (string, bool)
}

// repoFinder says which GitHub repo a bot session works in: its PR's repo,
// else its directory's origin remote.
type repoFinder struct {
	adapters map[string]adapter.Adapter
	remote   func(dir string) (string, error)

	mu    sync.Mutex
	byDir map[string]string
}

func newRepoFinder(adapters map[string]adapter.Adapter, remote func(string) (string, error)) *repoFinder {
	if remote == nil {
		remote = gitRemote
	}
	return &repoFinder{adapters: adapters, remote: remote, byDir: map[string]string{}}
}

// find implements source.Options.SessionRepo.
func (f *repoFinder) find(kind string, se adapter.Session) string {
	if r := githubRepo(se.PR); r != "" {
		return r
	}
	c, ok := f.adapters[kind].(cwder)
	if !ok {
		return ""
	}
	dir, ok := c.Cwd(se.ID)
	if !ok {
		return ""
	}
	f.mu.Lock()
	r, seen := f.byDir[dir]
	f.mu.Unlock()
	if seen {
		return r
	}
	if url, err := f.remote(dir); err == nil {
		r = githubRepo(url)
	}
	f.mu.Lock()
	f.byDir[dir] = r
	f.mu.Unlock()
	return r
}

// gitRemote is dir's origin URL.
func gitRemote(dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin").Output()
	return strings.TrimSpace(string(out)), err
}

// githubRepo is the owner/name in a GitHub URL (a PR link, or an https or
// ssh remote), or "".
func githubRepo(u string) string {
	i := strings.Index(u, "github.com")
	if i < 0 {
		return ""
	}
	rest := u[i+len("github.com"):]
	if rest == "" || (rest[0] != '/' && rest[0] != ':') {
		return ""
	}
	parts := strings.Split(strings.Trim(rest[1:], "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
}

// summaryLoop keeps each hot repo's summary fresh until ctx ends: Refresh
// writes one only when the cached one is over an hour old.
func summaryLoop(ctx context.Context, sum *summary.Summarizer, repos []string, wake func(), logf func(string, ...any)) {
	for {
		for _, repo := range repos {
			if ctx.Err() != nil {
				return
			}
			if _, err := sum.Refresh(ctx, repo); err != nil && ctx.Err() == nil {
				logf("lasso: summary of %s: %v", repo, err)
			}
		}
		wake()
		select {
		case <-ctx.Done():
			return
		case <-time.After(summaryEvery):
		}
	}
}

// noticeTTL keeps a notice's first time for as long as lasso.db lives.
const noticeTTL = 10 * 365 * 24 * time.Hour

// keyNotice is the one brief item that says which features are off for a
// missing key (Anthropic, Jev) and how to turn them on, or nil when none
// are. Its time is the first time it was raised, kept in the store, so it
// is new only once.
func keyNotice(st *store.Store, keyErr error, jevOff bool, cfgDir string, now time.Time) []source.Notice {
	var missing, body []string
	if keyErr != nil {
		missing = append(missing, "Anthropic")
		why := "There's no Anthropic API key"
		if !errors.Is(keyErr, llm.ErrNoKey) {
			why = "The Anthropic API key can't be used (" + keyErr.Error() + ")"
		}
		sorting := "- sorting what no rule covers: it is all kept as worth knowing.\n"
		if !jevOff {
			sorting = "- sorting what no rule covers, when Jev can't: it is kept as worth knowing.\n"
		}
		body = append(body, why+", so these are off:\n"+
			"- the bot digest's wording: bots' messages are counted instead (\"212 new messages from grok-scout\");\n"+
			"- Summaries written by a model: each repo gets lasso's counts only;\n"+
			sorting+
			"To turn them on, set ANTHROPIC_API_KEY where the daemon starts, or put the key in "+
			filepath.Join(cfgDir, llm.KeyFile)+" (chmod 600 it), then restart the daemon.")
	}
	if jevOff {
		missing = append(missing, "Jev")
		body = append(body, "There's no Jev key, so lasso doesn't cut long lists down before a model reads them: "+
			"Haiku and Sonnet read everything, which costs more of their daily caps.\n"+
			"To turn it on, set JEV_TOKEN (or TYPESAFE_API_KEY) where the daemon starts, then restart the daemon. "+
			"To stop this note without one, set disabled = true under [jev] in config.toml.")
	}
	if len(missing) == 0 {
		return nil
	}
	id := "keys:" + strings.ToLower(strings.Join(missing, "+"))
	at := now
	key := "notice:" + id
	if b, ok, err := st.CacheGet(key, now); err == nil && ok {
		if t, err := time.Parse(time.RFC3339Nano, string(b)); err == nil {
			at = t
		}
	} else {
		_ = st.CachePut(key, []byte(now.UTC().Format(time.RFC3339Nano)), now, noticeTTL)
	}
	return []source.Notice{{
		ID:    id,
		Title: fmt.Sprintf("Some features are off: no %s key", strings.Join(missing, " or ")),
		Body:  strings.Join(body, "\n\n"),
		At:    at,
	}}
}
