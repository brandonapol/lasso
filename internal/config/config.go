// Package config loads lasso's user config from ~/.config/lasso/config.toml.
//
// A missing file means defaults. An unknown key is an error that names the
// key, so a typo never silently does nothing.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the whole user config.
type Config struct {
	HotRepos  []string  `toml:"hot_repos"`
	Attention Attention `toml:"attention"`
	Models    Models    `toml:"models"`
	API       API       `toml:"api"`
	Jev       Jev       `toml:"jev"`
	Adapters  Adapters  `toml:"adapters"`
}

// Adapters configures the bot adapters (ADR 0001 §4).
type Adapters struct {
	Saddle Saddle `toml:"saddle"`
	Claude Local  `toml:"claude"`
	Grok   Local  `toml:"grok"`
}

// Local is an adapter that reads a bot's own files on this machine
// (Claude Code's transcripts, grok's sessions).
type Local struct {
	// Enabled turns the adapter on or off. Unset means on when Dir exists.
	Enabled *bool `toml:"enabled"`
	// Dir is where the bot keeps its sessions. Empty is the bot's default:
	// ~/.claude/projects for Claude Code, $GROK_HOME or ~/.grok for grok.
	// A leading ~/ is the home directory.
	Dir string `toml:"dir"`
}

// ClaudeDir is Claude Code's default projects directory under home.
func ClaudeDir(home string) string { return filepath.Join(home, ".claude", "projects") }

// GrokDir is grok's default home: GROK_HOME (read through env), else
// ~/.grok.
func GrokDir(home string, env func(string) string) string {
	if d := env("GROK_HOME"); d != "" {
		return d
	}
	return filepath.Join(home, ".grok")
}

// Resolve says where the adapter reads (Dir, else def, with ~/ expanded)
// and whether it is on: Enabled when set, else whether that directory
// exists.
func (l Local) Resolve(home, def string) (dir string, on bool) {
	dir = l.Dir
	if dir == "" {
		dir = def
	}
	if rest, ok := strings.CutPrefix(dir, "~/"); ok && home != "" {
		dir = filepath.Join(home, rest)
	}
	if l.Enabled != nil {
		return dir, *l.Enabled
	}
	fi, err := os.Stat(dir)
	return dir, err == nil && fi.IsDir()
}

// Saddle is how lasso reaches `saddle remote`.
type Saddle struct {
	// URL is the remote listener. A bare address means its /mcp route
	// (the first repo it serves); give /mcp/<repo> for another.
	URL string `toml:"url"`
	// TokenFile holds a `saddle remote token create` secret. A read token
	// lets lasso look; an admin token also lets it send and answer.
	TokenFile string `toml:"token_file"`
}

// Jev configures TypeSafe's Jev, the cheap decision model lasso uses to cut
// long lists down before a writing model sees them. The key never lives in
// this file: it comes from JEV_TOKEN (or TYPESAFE_API_KEY), as in saddle.
type Jev struct {
	// Disabled turns Jev off even when a key is present.
	Disabled bool `toml:"disabled"`
	// Model overrides the model name (default "jev-latest").
	Model string `toml:"model"`
	// DailyUSD caps Jev spending per day.
	DailyUSD float64 `toml:"daily_usd"`
}

// Attention controls when lasso may interrupt the user (ADR 0001 §5).
type Attention struct {
	AsksPerDay    int    `toml:"asks_per_day"`
	QuietHours    string `toml:"quiet_hours"`
	Notifications bool   `toml:"notifications"`
}

// Models holds the daily spending caps, in US dollars.
type Models struct {
	HaikuDailyUSD  float64 `toml:"haiku_daily_usd"`
	SonnetDailyUSD float64 `toml:"sonnet_daily_usd"`
}

// API is the daemon's local listener.
type API struct {
	Port int `toml:"port"`
}

// Default returns the config used when no file exists.
func Default() Config {
	return Config{
		Attention: Attention{AsksPerDay: 2, QuietHours: "22:00-08:00"},
		Models:    Models{HaikuDailyUSD: 1, SonnetDailyUSD: 5},
		API:       API{Port: 7337},
		Jev:       Jev{DailyUSD: 0.5},
		Adapters:  Adapters{Saddle: Saddle{URL: "http://127.0.0.1:7431"}},
	}
}

// Dir is lasso's config directory: $XDG_CONFIG_HOME/lasso, else ~/.config/lasso.
func Dir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "lasso"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "lasso"), nil
}

// StateDir is where lasso keeps its database and audit log:
// $XDG_STATE_HOME/lasso, else ~/.local/state/lasso.
func StateDir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "lasso"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "lasso"), nil
}

// Load reads config.toml from Dir.
func Load() (Config, error) {
	dir, err := Dir()
	if err != nil {
		return Config{}, err
	}
	return LoadFile(filepath.Join(dir, "config.toml"))
}

// LoadFile reads one config file over the defaults.
func LoadFile(path string) (Config, error) {
	cfg := Default()
	md, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		keys := make([]string, len(undec))
		for i, k := range undec {
			keys[i] = k.String()
		}
		return Config{}, fmt.Errorf("config %s: unknown key %s", path, strings.Join(keys, ", "))
	}
	return cfg, cfg.Validate()
}

// Validate checks values a TOML decode can't.
func (c Config) Validate() error {
	for _, r := range c.HotRepos {
		if owner, name, ok := strings.Cut(r, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return fmt.Errorf("hot_repos: %q is not owner/name", r)
		}
	}
	if c.Attention.AsksPerDay < 0 {
		return fmt.Errorf("attention.asks_per_day: must be 0 or more")
	}
	if c.Attention.QuietHours != "" {
		if _, _, err := c.Attention.Quiet(); err != nil {
			return err
		}
	}
	if c.API.Port < 1 || c.API.Port > 65535 {
		return fmt.Errorf("api.port: %d is not a port", c.API.Port)
	}
	if c.Models.HaikuDailyUSD < 0 || c.Models.SonnetDailyUSD < 0 || c.Jev.DailyUSD < 0 {
		return fmt.Errorf("models: daily caps must be 0 or more")
	}
	if u, err := url.Parse(c.Adapters.Saddle.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("adapters.saddle.url: %q is not an http(s) URL", c.Adapters.Saddle.URL)
	}
	return nil
}

// Quiet parses QuietHours ("22:00-08:00") into offsets from midnight. The end
// may be before the start, meaning the range wraps past midnight.
func (a Attention) Quiet() (start, end time.Duration, err error) {
	from, to, ok := strings.Cut(a.QuietHours, "-")
	if !ok {
		return 0, 0, fmt.Errorf("attention.quiet_hours: %q is not HH:MM-HH:MM", a.QuietHours)
	}
	if start, err = clock(from); err == nil {
		end, err = clock(to)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("attention.quiet_hours: %q is not HH:MM-HH:MM", a.QuietHours)
	}
	return start, end, nil
}

func clock(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}
