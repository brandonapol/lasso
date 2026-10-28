package daemon

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/audit"
	"github.com/brandonapol/lasso/internal/rules"
	"github.com/brandonapol/lasso/internal/store"
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
}

// route sets the adapter arg from the bot, on a copy of req.
func (d *actions) route(req rules.Request) rules.Request {
	args := maps.Clone(req.Args)
	if args == nil {
		args = map[string]string{}
	}
	delete(args, argAdapter)
	if kind := adapterFor(req.Bot, d.adapters); kind != "" {
		args[argAdapter] = kind
	}
	req.Args = args
	return req
}

// Do runs a request from a bot or the agent pane: only a rule can allow it.
func (d *actions) Do(ctx context.Context, req rules.Request) (rules.Outcome, error) {
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
