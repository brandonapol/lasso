package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/audit"
	"github.com/brandonapol/lasso/internal/rules"
	"github.com/brandonapol/lasso/internal/store"
	"github.com/brandonapol/lasso/internal/summary"
)

// Action args the daemon reads. A request's args go to the executor and,
// scrubbed, to the audit log.
const (
	// argAdapter names the adapter the action goes through. The daemon
	// sets it from the request's bot and overwrites whatever the caller
	// sent, so a request can't steer itself to another bot's adapter.
	argAdapter = "adapter"
	// argOption and argText are the answer to a prompt: a numbered option,
	// or free text for a bot that takes one.
	argOption = "option"
	argText   = "text"
	// argChoice is the user's answer in plain words ("yes", "no"), kept in
	// the answers table for suggested rules.
	argChoice = "choice"
	// argPrompt names the prompt an action other than answer_prompt
	// answers, if any.
	argPrompt = "prompt"
)

// adapterFor returns the adapter kind a bot belongs to: the kind itself
// ("saddle") or a name under it ("grok-scout", "grok:scout").
func adapterFor(bot string, kinds map[string]adapter.Adapter) string {
	for kind := range kinds {
		if bot == kind || strings.HasPrefix(bot, kind+"-") || strings.HasPrefix(bot, kind+":") {
			return kind
		}
	}
	return ""
}

// executor sends each action through its bot's adapter: one call each
// (ADR 0001 §2). It has no inverses: an answer sent to a bot can't be
// taken back.
type executor struct {
	adapters map[string]adapter.Adapter
}

var _ rules.Executor = executor{}

// Execute implements rules.Executor.
func (x executor) Execute(ctx context.Context, step audit.Step) (rules.Done, error) {
	kind := step.Args[argAdapter]
	a, ok := x.adapters[kind]
	if !ok {
		return rules.Done{}, fmt.Errorf("no bot adapter is set up for this bot, so lasso has nothing to send it through")
	}
	switch rules.Action(step.Action) {
	case rules.SendMessage:
		text := strings.TrimSpace(step.Args[argText])
		if text == "" {
			return rules.Done{}, fmt.Errorf("the message is empty")
		}
		session := sessionOf(kind, step.Target)
		if err := a.Send(ctx, session, text); err != nil {
			return rules.Done{}, err
		}
		return rules.Done{Detail: fmt.Sprintf("Sent your message to %s session %s.", kind, session)}, nil
	case rules.AnswerPrompt:
		choice, said, err := choiceOf(step.Args)
		if err != nil {
			return rules.Done{}, err
		}
		if err := a.Answer(ctx, step.Target, choice); err != nil {
			return rules.Done{}, err
		}
		return rules.Done{Detail: fmt.Sprintf("Answered %s's prompt %s with %s.", kind, step.Target, said)}, nil
	}
	return rules.Done{}, fmt.Errorf("lasso can't %s through %s yet", rules.Action(step.Action).Plain(), kind)
}

// Inverse implements rules.Executor.
func (executor) Inverse(audit.Step, rules.Done) (audit.Step, bool) { return audit.Step{}, false }

// choiceOf reads the answer from an action's args, and says it in words.
func choiceOf(args map[string]string) (adapter.Choice, string, error) {
	if o := args[argOption]; o != "" {
		n, err := strconv.Atoi(o)
		if err != nil || n <= 0 {
			return adapter.Choice{}, "", fmt.Errorf("option %q isn't a number from the prompt's list", o)
		}
		return adapter.Choice{Option: n}, "option " + o, nil
	}
	if t := strings.TrimSpace(args[argText]); t != "" {
		return adapter.Choice{Text: t}, "your reply", nil
	}
	return adapter.Choice{}, "", fmt.Errorf("the answer needs an option or some text")
}

// actions is the daemon's api.Actions: it routes each request to its
// bot's adapter, runs it through the rules engine, and, for the user's own
// answers, keeps them for suggested rules.
type actions struct {
	eng      *rules.Engine
	adapters map[string]adapter.Adapter
	answers  interface{ RecordAnswer(store.Answer) error }
	// resolve drops an answered prompt from the views; wake refreshes them.
	resolve func(kind, promptID string)
	wake    func()
	now     func() time.Time
	logf    func(format string, args ...any)
	// summaries writes a hot repo's summary on regenerate_summary.
	summaries interface {
		Regenerate(ctx context.Context, repo string) (summary.Summary, error)
	}
	repos []string // the hot repos
}

// sessionOf is the adapter's own session id for a send's target, which may
// be the In progress row's id ("session:grok:<id>").
func sessionOf(kind, target string) string {
	return strings.TrimPrefix(target, "session:"+kind+":")
}

// requester is an adapter that knows the facts of its own prompts (grok:
// the refused call's tool, rule and paths), for the rules to match on.
type requester interface {
	Request(promptID string, choice adapter.Choice) (rules.Request, error)
}

// route sets the adapter arg from the bot, on a copy of req. For an answer
// to a prompt whose adapter knows its facts, the adapter's paths and args
// replace what the caller said, so a request can't describe itself into a
// rule it doesn't fit.
func (d *actions) route(req rules.Request) rules.Request {
	args := maps.Clone(req.Args)
	if args == nil {
		args = map[string]string{}
	}
	delete(args, argAdapter)
	kind := adapterFor(req.Bot, d.adapters)
	if kind != "" {
		args[argAdapter] = kind
	}
	if rq, ok := d.adapters[kind].(requester); ok && req.Action == rules.AnswerPrompt {
		if choice, _, err := choiceOf(args); err == nil {
			if facts, err := rq.Request(req.Target, choice); err == nil {
				req.Paths, req.Lines = facts.Paths, facts.Lines
				req.Effects = append(slices.Clone(req.Effects), facts.Effects...)
				for k, v := range facts.Args {
					if k != argAdapter {
						args[k] = v
					}
				}
			}
		}
	}
	req.Args = args
	return req
}

// actRegenerate is the request kind for a new summary of a hot repo (the
// TUI's g key): {"action": "regenerate_summary", "repo": "owner/name"}.
// It isn't an outward action, so it doesn't go through the rules engine
// or the audit log; the summaries' daily cap bounds what it can spend.
const actRegenerate rules.Action = "regenerate_summary"

// regenerate writes a new summary of req's repo now.
func (d *actions) regenerate(ctx context.Context, req rules.Request) (rules.Outcome, error) {
	repo := strings.TrimSpace(req.Repo)
	if repo == "" {
		repo = strings.TrimSpace(req.Target)
	}
	repo = strings.TrimPrefix(repo, "summary:")
	if !strings.Contains(repo, "/") {
		repo = strings.Replace(repo, ":", "/", 1) // the Summaries row id's form
	}
	hot := slices.ContainsFunc(d.repos, func(r string) bool { return strings.EqualFold(r, repo) })
	if d.summaries == nil || !hot {
		return rules.Outcome{Decision: rules.Decision{Reason: fmt.Sprintf("%q isn't one of your hot repos, so lasso doesn't summarise it.", repo)}}, rules.ErrRefused
	}
	dec := rules.Decision{Allowed: true, Reason: "You asked for a new summary."}
	// Finish even if the caller hangs up: the summary is kept either way.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	sum, err := d.summaries.Regenerate(wctx, repo)
	if d.wake != nil {
		d.wake()
	}
	if err != nil {
		return rules.Outcome{Decision: dec, Detail: "It failed: " + err.Error()}, fmt.Errorf("%w: %v", rules.ErrFailed, err)
	}
	detail := "Wrote a new summary of " + repo + "."
	switch {
	case sum.OverCap:
		detail = "Today's spending cap for summaries is reached, so the new summary of " + repo + " is lasso's counts only."
	case sum.Local:
		detail = "Wrote a new summary of " + repo + ", counts only: " + sum.Note
	}
	return rules.Outcome{Decision: dec, Detail: detail}, nil
}

// Do runs a request from a bot or the agent pane: only a rule can allow it.
func (d *actions) Do(ctx context.Context, req rules.Request) (rules.Outcome, error) {
	if req.Action == actRegenerate {
		return d.regenerate(ctx, req)
	}
	req = d.route(req)
	out, err := d.eng.Do(ctx, req)
	if err == nil {
		d.answered(req, false)
	}
	return out, err
}

// DoAsUser runs a request the user made: no rule is matched, it is audited
// with actor "user", and an answer is kept in the answers table.
func (d *actions) DoAsUser(ctx context.Context, req rules.Request) (rules.Outcome, error) {
	if req.Action == actRegenerate {
		return d.regenerate(ctx, req)
	}
	req = d.route(req)
	out, err := d.eng.DoAsUser(ctx, req)
	if err == nil {
		d.answered(req, true)
	}
	return out, err
}

// Undo reverses an action (the API lets only the user ask).
func (d *actions) Undo(ctx context.Context, id string) (rules.Outcome, error) {
	return d.eng.Undo(ctx, id)
}

// answered records what a done action answered, and drops the prompt it
// answered from the views.
func (d *actions) answered(req rules.Request, byUser bool) {
	prompt := req.Args[argPrompt]
	if req.Action == rules.AnswerPrompt {
		prompt = req.Target
	}
	if prompt == "" {
		return
	}
	if byUser && d.answers != nil {
		choice := strings.TrimSpace(req.Args[argChoice])
		if choice == "" {
			if req.Action == rules.AnswerPrompt {
				_, choice, _ = choiceOf(req.Args)
			} else {
				choice = "yes" // the user approved the bot's request
			}
		}
		err := d.answers.RecordAnswer(store.Answer{
			PromptID: prompt, Bot: req.Bot, Action: string(req.Action), Choice: choice, Repo: req.Repo, At: d.now(),
		})
		if err != nil && d.logf != nil {
			// The action is done; only the suggestion it may feed is lost.
			d.logf("lasso: couldn't keep your answer to %s: %v", prompt, err)
		}
	}
	if kind := req.Args[argAdapter]; kind != "" && req.Action == rules.AnswerPrompt && d.resolve != nil {
		d.resolve(kind, prompt)
	}
	if d.wake != nil {
		d.wake()
	}
}
