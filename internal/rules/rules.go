// Package rules holds the user's standing permissions (ADR 0001 §6): what a
// bot may do for the user without asking, and within which bounds.
//
// Rules live in ~/.config/lasso/rules.toml. Matching is plain Go over that
// file. No model ever decides whether a rule allows something. A fixed
// always-ask list (login and auth code, billing, secrets, merging to the
// default branch, deleting branches, force-push, spending money, changes over
// 500 lines) overrides every rule; the user may add to it but not remove from
// it.
//
// Every action a rule allows is written to the audit log before it is sent,
// and refused if that write fails (see Engine).
package rules

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/brandonapol/lasso/internal/config"
)

// Clock returns the current time. Tests pass a fixed one.
type Clock func() time.Time

// Action is a kind of outward action a rule can allow.
type Action string

// The action kinds a rule may name.
const (
	FileIssue    Action = "file_issue"
	CIRetry      Action = "ci_retry"
	FixSmall     Action = "fix_small"
	AnswerPrompt Action = "answer_prompt"
	Comment      Action = "comment"
	Label        Action = "label"
)

// Actions lists every action kind a rule may name.
var Actions = []Action{FileIssue, CIRetry, FixSmall, AnswerPrompt, Comment, Label}

// Valid reports whether a is a known action kind.
func (a Action) Valid() bool { return slices.Contains(Actions, a) }

// Plain says what the action does, in everyday words ("file issues").
func (a Action) Plain() string {
	switch a {
	case FileIssue:
		return "file issues"
	case CIRetry:
		return "re-run failed checks"
	case FixSmall:
		return "push small fixes"
	case AnswerPrompt:
		return "answer its own permission prompts"
	case Comment:
		return "post comments"
	case Label:
		return "add labels"
	}
	return string(a)
}

// Rule is one standing permission.
type Rule struct {
	// ID names the rule in the audit log and in reasons. Unique.
	ID string `toml:"id"`
	// Who is a glob over bot names ("grok-*").
	Who string `toml:"who"`
	// Action is the one kind of action this rule allows.
	Action Action `toml:"action"`
	// Labels, if set, are the only labels the action may carry; it must
	// carry at least one of them.
	Labels []string `toml:"labels,omitempty"`
	// Paths, if set, are globs ("docs/**") every changed file must match.
	Paths []string `toml:"paths,omitempty"`
	// MaxLines, if set, caps the lines changed.
	MaxLines int `toml:"max_lines,omitzero"`
	// PerDay, if set, caps how many times the rule may act per calendar day.
	PerDay int `toml:"per_day,omitzero"`
	// Repos, if set, are globs over owner/name the action must be in.
	Repos []string `toml:"repos,omitempty"`
	// Expires, if set, is when the rule stops applying.
	Expires time.Time `toml:"expires,omitempty"`
}

// AskRule is one entry of the always-ask list: an action matching it is
// never allowed by a rule, however a rule is written.
type AskRule struct {
	ID string `toml:"id"`
	// Why is the plain reason shown when it applies ("it touches billing").
	Why string `toml:"why"`
	// Paths are globs; any changed file matching one triggers it.
	Paths []string `toml:"paths,omitempty"`
	// Effects are side effects the request declares (see Effect).
	Effects []Effect `toml:"effects,omitempty"`
	// OverLines triggers it when more lines than this change.
	OverLines int `toml:"over_lines,omitzero"`
}

// Effect is a side effect an adapter declares on a request, for the
// always-ask list to catch whatever the action kind is.
type Effect string

// Effects the default always-ask list knows.
const (
	MergeDefault Effect = "merge_default_branch"
	DeleteBranch Effect = "delete_branch"
	ForcePush    Effect = "force_push"
	SpendMoney   Effect = "spend_money"
	Secrets      Effect = "secrets"
	AuthChange   Effect = "auth"
)

// DefaultAlwaysAsk is the built-in always-ask list. It applies whatever
// rules.toml says.
func DefaultAlwaysAsk() []AskRule {
	return []AskRule{
		{ID: "auth", Why: "it touches login or sign-in code",
			Effects: []Effect{AuthChange},
			Paths:   []string{"**/*auth*", "**/*auth*/**", "**/*login*", "**/*login*/**", "**/*signin*", "**/*signin*/**"}},
		{ID: "billing", Why: "it touches billing code",
			Paths: []string{"**/billing/**", "**/*billing*"}},
		{ID: "secrets", Why: "it touches secrets or keys",
			Effects: []Effect{Secrets},
			Paths:   []string{"**/.env", "**/.env.*", "**/*.pem", "**/*.key", "**/id_rsa*", "**/id_ed25519*", "**/*secret*", "**/*secret*/**", "**/*credential*", "**/.netrc", "**/.npmrc", "**/.pypirc"}},
		{ID: "merge-default", Why: "it merges into the main branch", Effects: []Effect{MergeDefault}},
		{ID: "delete-branch", Why: "it deletes a branch", Effects: []Effect{DeleteBranch}},
		{ID: "force-push", Why: "it force-pushes, which can throw away work", Effects: []Effect{ForcePush}},
		{ID: "spend-money", Why: "it spends money", Effects: []Effect{SpendMoney}},
		{ID: "big-change", Why: "it changes more than 500 lines", OverLines: 500},
	}
}

// File is rules.toml.
type File struct {
	// AlwaysAsk adds to DefaultAlwaysAsk; it can't remove from it.
	AlwaysAsk []AskRule `toml:"always_ask,omitempty"`
	Rules     []Rule    `toml:"rules,omitempty"`
}

// Path is the default rules file: config.Dir()/rules.toml.
func Path() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "rules.toml"), nil
}

// Load reads rules.toml from config.Dir.
func Load() (File, error) {
	p, err := Path()
	if err != nil {
		return File{}, err
	}
	return LoadFile(p)
}

// LoadFile reads one rules file. A missing file means no rules (the
// always-ask list still applies). An unknown key is an error naming it, so a
// typo never silently widens or narrows a rule.
func LoadFile(p string) (File, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	f, err := Parse(b)
	if err != nil {
		return File{}, fmt.Errorf("rules %s: %w", p, err)
	}
	return f, nil
}

// Parse decodes and validates rules TOML.
func Parse(b []byte) (File, error) {
	var f File
	md, err := toml.Decode(string(b), &f)
	if err != nil {
		return File{}, err
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		keys := make([]string, len(undec))
		for i, k := range undec {
			keys[i] = k.String()
		}
		return File{}, fmt.Errorf("unknown key %s", strings.Join(keys, ", "))
	}
	return f, f.Validate()
}

// Validate checks what a TOML decode can't.
func (f File) Validate() error {
	ids := map[string]bool{}
	for i, r := range f.Rules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
		if ids[r.ID] {
			return fmt.Errorf("rules[%d]: id %q is used twice", i, r.ID)
		}
		ids[r.ID] = true
	}
	for i, a := range f.AlwaysAsk {
		if a.ID == "" {
			return fmt.Errorf("always_ask[%d]: id is required", i)
		}
		if len(a.Paths) == 0 && len(a.Effects) == 0 && a.OverLines == 0 {
			return fmt.Errorf("always_ask[%d] %s: needs paths, effects or over_lines", i, a.ID)
		}
		if a.OverLines < 0 {
			return fmt.Errorf("always_ask[%d] %s: over_lines must be 0 or more", i, a.ID)
		}
		for _, g := range a.Paths {
			if err := checkGlob(g); err != nil {
				return fmt.Errorf("always_ask[%d] %s: %w", i, a.ID, err)
			}
		}
	}
	return nil
}

// Validate checks one rule.
func (r Rule) Validate() error {
	if r.ID == "" {
		return errors.New("id is required")
	}
	if r.Who == "" {
		return fmt.Errorf("%s: who is required (a bot name or glob like grok-*)", r.ID)
	}
	if _, err := path.Match(r.Who, ""); err != nil {
		return fmt.Errorf("%s: who %q is not a valid glob", r.ID, r.Who)
	}
	if !r.Action.Valid() {
		return fmt.Errorf("%s: action %q is not one of %s", r.ID, r.Action, actionList())
	}
	if r.MaxLines < 0 || r.PerDay < 0 {
		return fmt.Errorf("%s: max_lines and per_day must be 0 or more", r.ID)
	}
	for _, g := range r.Paths {
		if err := checkGlob(g); err != nil {
			return fmt.Errorf("%s: %w", r.ID, err)
		}
	}
	for _, g := range r.Repos {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("%s: repo %q is not a valid glob", r.ID, g)
		}
	}
	return nil
}

func actionList() string {
	s := make([]string, len(Actions))
	for i, a := range Actions {
		s[i] = string(a)
	}
	return strings.Join(s, ", ")
}

// Encode renders f as TOML. Comments in the file it came from are not kept;
// use Append to add a rule without touching the rest of the file.
func Encode(f File) ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Save writes f to p, replacing it. It refuses to write anything that
// doesn't load back to the same rules.
func Save(p string, f File) error {
	if err := f.Validate(); err != nil {
		return err
	}
	b, err := Encode(f)
	if err != nil {
		return err
	}
	back, err := Parse(b)
	if err != nil || len(back.Rules) != len(f.Rules) || len(back.AlwaysAsk) != len(f.AlwaysAsk) {
		return fmt.Errorf("rules: encoded file doesn't load back (%v)", err)
	}
	return writeAtomic(p, b)
}

// Append adds r to the rules file at p by appending a [[rules]] table to
// the file's text, so the user's comments and ordering stay exactly as they
// were. The result is checked to load with every old rule plus r before it
// replaces the file.
func Append(p string, r Rule) error {
	if err := r.Validate(); err != nil {
		return err
	}
	old, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	before, err := Parse(old)
	if err != nil {
		return fmt.Errorf("rules %s: %w", p, err)
	}
	block, err := Encode(File{Rules: []Rule{r}})
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.Write(old)
	if len(old) > 0 {
		if !bytes.HasSuffix(old, []byte("\n")) {
			buf.WriteByte('\n')
		}
		buf.WriteByte('\n')
	}
	buf.Write(block)
	after, err := Parse(buf.Bytes())
	if err != nil {
		return fmt.Errorf("rules %s: adding %s would break the file: %w", p, r.ID, err)
	}
	if len(after.Rules) != len(before.Rules)+1 {
		return fmt.Errorf("rules %s: adding %s didn't add exactly one rule", p, r.ID)
	}
	return writeAtomic(p, buf.Bytes())
}

// writeAtomic replaces p with b via a temp file and rename, so a crash never
// leaves half a rules file.
func writeAtomic(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".rules-*.toml")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	defer tmp.Close()
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(p); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
