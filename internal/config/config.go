// Package config loads lasso's user config from ~/.config/lasso/config.toml.
//
// A missing file means defaults. An unknown key is an error that names the
// key, so a typo never silently does nothing.
package config

import (
	"errors"
	"fmt"
	"io/fs"
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
