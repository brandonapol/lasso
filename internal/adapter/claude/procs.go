package claude

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procs lists the claude processes from /proc. Elsewhere (no /proc) it
// returns none, and liveness rests on transcript times alone.
func procs() ([]Proc, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil
	}
	var out []Proc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(string(bytes.TrimRight(raw, "\x00")), "\x00")
		if !isClaude(args) {
			continue
		}
		cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
		if err != nil {
			continue
		}
		out = append(out, Proc{PID: pid, Cwd: cwd, Args: args})
	}
	return out, nil
}

// isClaude reports whether args are Claude Code itself: the native binary,
// or node running its CLI.
func isClaude(args []string) bool {
	if filepath.Base(args[0]) == "claude" {
		return true
	}
	return len(args) > 1 && strings.HasPrefix(filepath.Base(args[0]), "node") &&
		(filepath.Base(args[1]) == "claude" || strings.Contains(args[1], "claude-code"))
}

// helpers are claude subcommands that hold no session.
var helpers = map[string]bool{
	"daemon": true, "bg-pty-host": true, "mcp": true, "config": true, "doctor": true,
	"update": true, "install": true, "migrate-installer": true, "setup-token": true, "plugin": true,
}

// liveSets turns processes into the sessions they hold: by id when the
// command line names one (--resume, --session-id), else by working
// directory. Claude Code's own helpers (its daemon, its terminal hosts)
// hold no session.
func liveSets(ps []Proc) (byID, byCwd map[string]bool) {
	byID, byCwd = map[string]bool{}, map[string]bool{}
	for _, p := range ps {
		args := p.Args
		if len(args) > 0 && strings.HasPrefix(filepath.Base(args[0]), "node") && len(args) > 1 {
			args = args[1:]
		}
		if len(args) > 1 && helpers[args[1]] {
			continue
		}
		named := false
		for i, a := range args {
			if v, ok := strings.CutPrefix(a, "--resume="); ok {
				byID[v], named = true, true
			}
			if v, ok := strings.CutPrefix(a, "--session-id="); ok {
				byID[v], named = true, true
			}
			if (a == "--resume" || a == "-r" || a == "--session-id") && i+1 < len(args) {
				byID[args[i+1]], named = true, true
			}
		}
		if !named && p.Cwd != "" {
			byCwd[p.Cwd] = true
		}
	}
	return byID, byCwd
}
