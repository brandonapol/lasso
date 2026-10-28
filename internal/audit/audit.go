// Package audit is lasso's append-only record of every outward action it or
// a standing rule took for the user (ADR 0001 §2, §6): one JSON object per
// line in ~/.local/state/lasso/audit.jsonl.
//
// The log is written before an action is sent. If the line can't be written,
// the caller must refuse the action, so nothing lasso does goes unrecorded.
// Undo and per-day caps are both read back from this file.
package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/brandonapol/lasso/internal/config"
)

// Results an entry can record. An action writes Started before it is sent,
// then OK or Failed once the adapter answers.
const (
	Started = "started"
	OK      = "ok"
	Failed  = "failed"
)

// Step is one outward call: what kind, against what, with which arguments.
// An entry carries the step it records and, once done, the step that would
// reverse it.
type Step struct {
	Action string            `json:"action"`
	Target string            `json:"target"`
	Args   map[string]string `json:"args,omitempty"`
}

// Entry is one line of the audit log.
type Entry struct {
	Time time.Time `json:"time"`
	// ID ties an action's started line to its result line.
	ID string `json:"id"`
	// Actor is who decided: "user", "rule:<id>" or "agent-pane".
	Actor string `json:"actor"`
	// Bot is the bot the action was taken for, if any.
	Bot    string `json:"bot,omitempty"`
	Action string `json:"action"`
	Target string `json:"target"`
	// Args are scrubbed of secrets before they are written.
	Args   map[string]string `json:"args,omitempty"`
	Result string            `json:"result"`
	// Detail says what happened, in plain words.
	Detail string `json:"detail,omitempty"`
	// Undo is the step that reverses this action, set on the OK line when
	// one exists.
	Undo *Step `json:"undo,omitempty"`
	// UndoOf is the ID of the action this one reverses.
	UndoOf string `json:"undo_of,omitempty"`
}

// Log appends to and reads one audit file.
type Log struct {
	path string
	mu   sync.Mutex
}

// Path is the default audit log: StateDir/audit.jsonl.
func Path() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "audit.jsonl"), nil
}

// Open returns the log at path. Nothing is created until the first Append.
func Open(path string) *Log { return &Log{path: path} }

// Path returns the file the log writes to.
func (l *Log) Path() string { return l.path }

// Append scrubs e's args and writes it as one line, synced to disk. An error
// means the line may not be on disk and the action must not go ahead.
func (l *Log) Append(e Entry) error {
	e.Args = Scrub(e.Args)
	if e.Undo != nil {
		u := *e.Undo
		u.Args = Scrub(u.Args)
		e.Undo = &u
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

// Entries reads the whole log in order. A missing file is an empty log. A
// line that doesn't parse is an error naming its line number: the log is
// for "who did this", so it is never silently skipped.
func (l *Log) Entries() ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("audit %s line %d: %w", l.path, n, err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	return out, nil
}

// CountStarted counts the actions actor started at or after since. Failed
// attempts count too: a per-day cap bounds tries, not successes.
func (l *Log) CountStarted(actor string, since time.Time) (int, error) {
	es, err := l.Entries()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range es {
		if e.Result == Started && e.Actor == actor && !e.Time.Before(since) {
			n++
		}
	}
	return n, nil
}

// Find returns the latest entry for action id, which is its result line
// once one has been written.
func (l *Log) Find(id string) (Entry, bool, error) {
	es, err := l.Entries()
	if err != nil {
		return Entry{}, false, err
	}
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].ID == id {
			return es[i], true, nil
		}
	}
	return Entry{}, false, nil
}

// UndoneBy returns the ID of the action that reversed id, if any.
func (l *Log) UndoneBy(id string) (string, bool, error) {
	es, err := l.Entries()
	if err != nil {
		return "", false, err
	}
	for _, e := range es {
		if e.UndoOf == id && e.Result == OK {
			return e.ID, true, nil
		}
	}
	return "", false, nil
}
