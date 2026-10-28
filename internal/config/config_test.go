package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMissingFileIsDefaults(t *testing.T) {
	cfg, err := LoadFile(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Port != 7337 || cfg.Attention.AsksPerDay != 2 || cfg.Models.SonnetDailyUSD != 5 || cfg.Jev.DailyUSD != 0.5 || cfg.Jev.Disabled {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := LoadFile(write(t, `
hot_repos = ["brandonapol/saddle", "brandonapol/lasso"]
[attention]
asks_per_day = 1
quiet_hours = "23:30-07:00"
[api]
port = 8000
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.HotRepos) != 2 || cfg.Attention.AsksPerDay != 1 || cfg.API.Port != 8000 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Models.HaikuDailyUSD != 1 {
		t.Fatalf("unset key lost its default: %+v", cfg.Models)
	}
	start, end, err := cfg.Attention.Quiet()
	if err != nil || start != 23*time.Hour+30*time.Minute || end != 7*time.Hour {
		t.Fatalf("quiet = %v %v %v", start, end, err)
	}
}

func TestUnknownKeyIsNamed(t *testing.T) {
	_, err := LoadFile(write(t, "[attention]\nasks_per_dya = 3\n"))
	if err == nil || !strings.Contains(err.Error(), "attention.asks_per_dya") {
		t.Fatalf("err = %v", err)
	}
}

func TestInvalidValues(t *testing.T) {
	for _, body := range []string{
		`hot_repos = ["saddle"]`,
		"[attention]\nquiet_hours = \"late\"",
		"[api]\nport = 70000",
		"[attention]\nasks_per_day = -1",
	} {
		if _, err := LoadFile(write(t, body)); err == nil {
			t.Errorf("%q: no error", body)
		}
	}
}

func TestSaddleAdapter(t *testing.T) {
	cfg, err := LoadFile(write(t, "[adapters.saddle]\ntoken_file = \"/x/saddle.token\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Adapters.Saddle; s.URL != "http://127.0.0.1:7431" || s.TokenFile != "/x/saddle.token" {
		t.Fatalf("saddle = %+v", s)
	}
	_, err = LoadFile(write(t, "[adapters.saddle]\nurl = \"127.0.0.1:7431\"\n"))
	if err == nil || !strings.Contains(err.Error(), "adapters.saddle.url") {
		t.Fatalf("bad url err = %v", err)
	}
}

func TestLocalAdapters(t *testing.T) {
	home := t.TempDir()
	cfg, err := LoadFile(write(t, "[adapters.grok]\nenabled = false\n[adapters.claude]\ndir = \"~/cc\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Unset: on only when the directory exists.
	dir, on := cfg.Adapters.Claude.Resolve(home, ClaudeDir(home))
	if dir != filepath.Join(home, "cc") || on {
		t.Fatalf("claude = %q %v", dir, on)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, on := cfg.Adapters.Claude.Resolve(home, ClaudeDir(home)); !on {
		t.Fatal("claude off with its directory present")
	}
	// Enabled = false wins over an existing directory.
	gdir := GrokDir(home, func(string) string { return "" })
	if err := os.MkdirAll(gdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if d, on := cfg.Adapters.Grok.Resolve(home, gdir); on || d != gdir {
		t.Fatalf("grok = %q %v", d, on)
	}
	if d := GrokDir(home, func(k string) string { return map[string]string{"GROK_HOME": "/g"}[k] }); d != "/g" {
		t.Fatal(d)
	}
}

func TestXDGDirs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	t.Setenv("XDG_STATE_HOME", "/x/state")
	if d, _ := Dir(); d != "/x/cfg/lasso" {
		t.Fatal(d)
	}
	if d, _ := StateDir(); d != "/x/state/lasso" {
		t.Fatal(d)
	}
}
