package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Claude Code writes one file per running session to ~/.claude/sessions:
// <pid>.json, with the session's id, directory, name, whether it is busy,
// and its messaging socket. The file goes when the process exits. lasso
// reads only these files, never the <pid>.<hash>.key files beside them.

// liveSession is one running Claude Code session, from its <pid>.json.
type liveSession struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Status    string `json:"status"` // "idle", "busy", "shell"
	Socket    string `json:"messagingSocketPath"`
	StatusAt  int64  `json:"statusUpdatedAt"` // ms since the epoch
}

// busy reports whether the session is working rather than waiting for input.
func (l liveSession) busy() bool { return l.Status != "" && l.Status != "idle" }

// liveSet is what the sessions directory says.
type liveSet struct {
	// ok is false when the directory isn't there or holds nothing usable
	// (an older Claude Code): liveness then falls back to the guess.
	ok     bool
	byID   map[string]liveSession
	pids   map[int]bool
	byName map[string]int // how many running sessions carry each name
}

// readLive reads the sessions directory. A file whose process is gone (a
// crash left it behind) is ignored.
func readLive(dir string, alive func(int) bool) liveSet {
	ls := liveSet{byID: map[string]liveSession{}, pids: map[int]bool{}, byName: map[string]int{}}
	if dir == "" {
		return ls
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return ls
	}
	for _, p := range paths {
		base := strings.TrimSuffix(filepath.Base(p), ".json")
		pid, err := strconv.Atoi(base)
		if err != nil {
			continue // not <pid>.json
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s liveSession
		if json.Unmarshal(b, &s) != nil || s.SessionID == "" {
			continue
		}
		ls.ok = true // this Claude Code writes them: trust what's here
		if s.PID == 0 {
			s.PID = pid
		}
		if !alive(s.PID) {
			continue
		}
		ls.pids[s.PID] = true
		if old, dup := ls.byID[s.SessionID]; dup && old.StatusAt > s.StatusAt {
			continue
		}
		ls.byID[s.SessionID] = s
		if s.Name != "" {
			ls.byName[s.Name]++
		}
	}
	return ls
}

// pidAlive reports whether a process with this pid exists.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
