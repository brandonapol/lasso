package rules

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// Request is one outward action a bot wants lasso to take. Adapters fill it
// in from facts they read (the diff, the labels), never from the bot's own
// description of what it is doing.
type Request struct {
	// Bot is the bot name rules match "who" against.
	Bot    string `json:"bot"`
	Action Action `json:"action"`
	// Repo is owner/name.
	Repo string `json:"repo,omitempty"`
	// Target is what the action touches ("owner/name#12", a run URL).
	Target string `json:"target"`
	// Labels are the labels the action carries or adds.
	Labels []string `json:"labels,omitempty"`
	// Paths are the repo-relative files it changes.
	Paths []string `json:"paths,omitempty"`
	// Lines is how many lines it changes.
	Lines int `json:"lines,omitempty"`
	// Effects are side effects the adapter knows the action has.
	Effects []Effect `json:"effects,omitempty"`
	// Args go to the executor and, scrubbed, to the audit log.
	Args map[string]string `json:"args,omitempty"`
}

// Decision is the matcher's answer.
type Decision struct {
	Allowed bool `json:"allowed"`
	// Rule is the id of the rule that allowed it, or of the always-ask entry
	// that stopped it. Empty when no rule applied.
	Rule string `json:"rule,omitempty"`
	// Reason says why, in plain words.
	Reason string `json:"reason"`
}

// Usage counts how many times a rule has acted since a time. The engine
// reads it from the audit log.
type Usage func(ruleID string, since time.Time) (int, error)

// Match decides whether f lets req go ahead without asking, at time now.
// The always-ask list is checked first and wins over every rule. Then rules
// are tried in file order; the first that covers req allows it. If none
// does, the reason comes from the first rule for this bot and action, so the
// user sees the nearest miss.
//
// used may be nil when no rule has a per_day cap.
func (f File) Match(req Request, now time.Time, used Usage) Decision {
	if !req.Action.Valid() {
		return Decision{Reason: fmt.Sprintf("%q isn't an action lasso knows", req.Action)}
	}
	if req.Bot == "" {
		return Decision{Reason: "the request doesn't say which bot it's for"}
	}
	for _, a := range append(DefaultAlwaysAsk(), f.AlwaysAsk...) {
		if why, hit := a.hits(req); hit {
			return Decision{Rule: a.ID, Reason: "Always ask: " + why + "."}
		}
	}
	var nearest string
	for _, r := range f.Rules {
		if r.Action != req.Action || !globMatch(r.Who, req.Bot) {
			continue
		}
		miss, count := r.covers(req, now, used)
		if miss == "" {
			return Decision{Allowed: true, Rule: r.ID, Reason: r.allowedReason(req, count)}
		}
		if nearest == "" {
			nearest = fmt.Sprintf("Rule %s doesn't cover it: %s.", r.ID, miss)
		}
	}
	if nearest != "" {
		return Decision{Reason: nearest}
	}
	return Decision{Reason: fmt.Sprintf("No rule lets %s %s.", req.Bot, req.Action.Plain())}
}

// hits reports whether a stops req, and says why.
func (a AskRule) hits(req Request) (string, bool) {
	for _, e := range a.Effects {
		if slices.Contains(req.Effects, e) {
			return a.why(), true
		}
	}
	if a.OverLines > 0 && req.Lines > a.OverLines {
		return fmt.Sprintf("%s (%d lines)", a.why(), req.Lines), true
	}
	for _, p := range req.Paths {
		for _, g := range a.Paths {
			// Case-folded: Auth/ and auth/ both ask.
			if globMatch(strings.ToLower(g), strings.ToLower(p)) {
				return fmt.Sprintf("%s (%s)", a.why(), p), true
			}
		}
	}
	return "", false
}

func (a AskRule) why() string {
	if a.Why != "" {
		return a.Why
	}
	return "it matches the always-ask entry " + a.ID
}

// covers returns "" when r allows req, else the first condition it fails in
// plain words. count is how many times r has acted today.
func (r Rule) covers(req Request, now time.Time, used Usage) (miss string, count int) {
	if !r.Expires.IsZero() && !now.Before(r.Expires) {
		return "it expired " + r.Expires.Format("2 Jan 2006"), 0
	}
	if len(r.Repos) > 0 && !slices.ContainsFunc(r.Repos, func(g string) bool { return globMatch(g, req.Repo) }) {
		if req.Repo == "" {
			return "it only covers some repos, and this action names none", 0
		}
		return fmt.Sprintf("it doesn't cover the repo %s", req.Repo), 0
	}
	if len(r.Labels) > 0 {
		if len(req.Labels) == 0 {
			return fmt.Sprintf("it needs one of the labels %s", strings.Join(r.Labels, ", ")), 0
		}
		for _, l := range req.Labels {
			if !slices.Contains(r.Labels, l) {
				return fmt.Sprintf("the label %q isn't one it allows", l), 0
			}
		}
	}
	if len(r.Paths) > 0 {
		if len(req.Paths) == 0 {
			return "it only covers some files, and this action names none", 0
		}
		for _, p := range req.Paths {
			if !slices.ContainsFunc(r.Paths, func(g string) bool { return globMatch(g, p) }) {
				return fmt.Sprintf("it doesn't cover the file %s", p), 0
			}
		}
	}
	if r.MaxLines > 0 && req.Lines > r.MaxLines {
		return fmt.Sprintf("it allows at most %d changed lines and this is %d", r.MaxLines, req.Lines), 0
	}
	if r.PerDay > 0 {
		if used == nil {
			return "today's count can't be checked", 0
		}
		n, err := used(r.ID, startOfDay(now))
		if err != nil {
			return "today's count can't be read from the audit log", 0
		}
		if n >= r.PerDay {
			return fmt.Sprintf("it has already acted %d of %d times today", n, r.PerDay), n
		}
		count = n
	}
	return "", count
}

func (r Rule) allowedReason(req Request, count int) string {
	s := fmt.Sprintf("Rule %s lets %s %s", r.ID, req.Bot, req.Action.Plain())
	if r.PerDay > 0 {
		s += fmt.Sprintf(" (this is %d of %d today)", count+1, r.PerDay)
	}
	return s + "."
}

// startOfDay is midnight at the start of t's day, in t's location.
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// checkGlob reports a malformed glob.
func checkGlob(g string) error {
	for _, seg := range strings.Split(g, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("glob %q is malformed", g)
		}
	}
	return nil
}

// globMatch matches name against a slash-separated glob in which each
// segment is a path.Match pattern and "**" matches any number of segments,
// including none.
func globMatch(glob, name string) bool {
	return matchSegs(strings.Split(glob, "/"), strings.Split(name, "/"))
}

func matchSegs(gs, ns []string) bool {
	for len(gs) > 0 {
		if gs[0] == "**" {
			for i := 0; i <= len(ns); i++ {
				if matchSegs(gs[1:], ns[i:]) {
					return true
				}
			}
			return false
		}
		if len(ns) == 0 {
			return false
		}
		if ok, err := path.Match(gs[0], ns[0]); err != nil || !ok {
			return false
		}
		gs, ns = gs[1:], ns[1:]
	}
	return len(ns) == 0
}
