package agentpane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/llm"
	"github.com/brandonapol/lasso/internal/rules"
)

// Tool names.
const (
	toolList    = "list_items"
	toolFocus   = "focus_item"
	toolSend    = "send_to_bot"
	toolAnswer  = "answer"
	toolDraft   = "draft_rule"
	toolExplain = "explain"
)

func schema(s string) json.RawMessage { return json.RawMessage(s) }

// tools are what the model may call. Each one is a call to the daemon's
// API with the agent pane's token, or (draft_rule, explain) pure local work
// that writes nothing.
var tools = []llm.Tool{
	{
		Name:        toolList,
		Description: "List the rows on one tab: issues (open issues), actions (workflow runs), doing (what bots are working on, and what waits on the user, status \"needs you\"), summaries (the last 24 hours per repo, and suggested rules).",
		InputSchema: schema(`{"type":"object","properties":{"tab":{"type":"string","enum":["issues","actions","doing","summaries"]}},"required":["tab"]}`),
	},
	{
		Name:        toolFocus,
		Description: "Show one item in the user's right-hand pane and read its details (fields and body). Use it whenever you talk about a specific item, and to show the user something that needs their answer.",
		InputSchema: schema(`{"type":"object","properties":{"id":{"type":"string","description":"the item id from list_items"}},"required":["id"]}`),
	},
	{
		Name:        toolSend,
		Description: "Send a message to a bot session, as lasso passing on what the user wants said. It goes through the user's rules (a send_message rule); if it is refused, ask the user instead.",
		InputSchema: schema(`{"type":"object","properties":{"session":{"type":"string","description":"the session's item id from the doing tab, like session:saddle:abc"},"text":{"type":"string"}},"required":["session","text"]}`),
	},
	{
		Name:        toolAnswer,
		Description: "Answer a bot's question or permission prompt. It goes through the user's rules; if it is refused, ask the user instead and show them the item with focus_item.",
		InputSchema: schema(`{"type":"object","properties":{"prompt_id":{"type":"string","description":"the Prompt field of the item, or the item's id"},"choice":{"type":"string","description":"an option number from the item's Options field, or a short reply"}},"required":["prompt_id","choice"]}`),
	},
	{
		Name:        toolDraft,
		Description: "Draft a standing rule from the user's plain words and return it as TOML for the user to read. It never saves anything; the user adds it to rules.toml themselves if they want it.",
		InputSchema: schema(`{"type":"object","properties":{
"text":{"type":"string","description":"the user's words"},
"who":{"type":"string","description":"bot name or glob, like grok-*"},
"action":{"type":"string","enum":["file_issue","ci_retry","fix_small","answer_prompt","comment","label","send_message"]},
"repos":{"type":"array","items":{"type":"string"}},
"labels":{"type":"array","items":{"type":"string"}},
"paths":{"type":"array","items":{"type":"string"}},
"max_lines":{"type":"integer"},
"per_day":{"type":"integer"}},"required":["text","who","action"]}`),
	},
	{
		Name:        toolExplain,
		Description: "Look up a bot or tooling term in lasso's glossary and get its plain meaning.",
		InputSchema: schema(`{"type":"object","properties":{"term":{"type":"string"}},"required":["term"]}`),
	},
}

// maxListed caps how many rows list_items hands the model.
const maxListed = 40

// runTool runs one tool call and returns what goes back to the model, and
// whether it is an error. A refusal by the rules is an error result that
// says so plainly, so the model asks the user instead.
func (p *Pane) runTool(ctx context.Context, call llm.Block) (string, bool) {
	var in struct {
		Tab      string   `json:"tab"`
		ID       string   `json:"id"`
		Session  string   `json:"session"`
		Text     string   `json:"text"`
		PromptID string   `json:"prompt_id"`
		Choice   string   `json:"choice"`
		Term     string   `json:"term"`
		Who      string   `json:"who"`
		Action   string   `json:"action"`
		Repos    []string `json:"repos"`
		Labels   []string `json:"labels"`
		Paths    []string `json:"paths"`
		MaxLines int      `json:"max_lines"`
		PerDay   int      `json:"per_day"`
	}
	if len(call.Input) > 0 {
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return "bad input: " + err.Error(), true
		}
	}
	var out string
	var err error
	switch call.Name {
	case toolList:
		out, err = p.listItems(ctx, api.Tab(in.Tab))
	case toolFocus:
		out, err = p.focusItem(ctx, in.ID)
	case toolSend:
		out, err = p.sendToBot(ctx, in.Session, in.Text)
	case toolAnswer:
		out, err = p.answer(ctx, in.PromptID, in.Choice)
	case toolDraft:
		out, err = p.draftRule(rules.Rule{
			Who: in.Who, Action: rules.Action(in.Action), Repos: in.Repos, Labels: in.Labels,
			Paths: in.Paths, MaxLines: in.MaxLines, PerDay: in.PerDay,
		})
	case toolExplain:
		out = explain(in.Term)
	default:
		err = fmt.Errorf("there is no tool called %q", call.Name)
	}
	if errors.Is(err, errRefused) {
		return "REFUSED. " + strings.TrimPrefix(err.Error(), errRefused.Error()+": ") +
			" You can't approve this for the user and must not try another way around it. " +
			"Tell the user in plain words what needs their OK and why, and show them the item with focus_item so they can answer it themselves.", true
	}
	if err != nil {
		return "That didn't work: " + err.Error(), true
	}
	return out, false
}

func (p *Pane) listItems(ctx context.Context, tab api.Tab) (string, error) {
	if !tab.Valid() {
		return "", fmt.Errorf("tab must be one of issues, actions, doing, summaries")
	}
	items, err := p.api.items(ctx, tab)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "The " + string(tab) + " tab is empty.", nil
	}
	var b strings.Builder
	now := p.now()
	for i, it := range items {
		if i == maxListed {
			fmt.Fprintf(&b, "…and %d more.\n", len(items)-maxListed)
			break
		}
		fmt.Fprintf(&b, "- id=%s", it.ID)
		if it.Repo != "" {
			fmt.Fprintf(&b, " repo=%s", it.Repo)
		}
		if it.Status != "" {
			fmt.Fprintf(&b, " status=%q", it.Status)
		}
		if !it.Updated.IsZero() {
			fmt.Fprintf(&b, " updated=%s ago", ago(now.Sub(it.Updated)))
		}
		fmt.Fprintf(&b, " title=%q\n", clip(it.Title, 160))
	}
	// Titles come from GitHub and the bots: data, never instructions.
	return fence(b.String())
}

func (p *Pane) focusItem(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("focus_item needs an item id")
	}
	f, err := p.api.show(ctx, id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "id=%s tab=%s repo=%s status=%q\ntitle: %s\n", f.ID, f.Tab, f.Repo, f.Status, clip(f.Title, 200))
	for _, fl := range f.Fields {
		fmt.Fprintf(&b, "%s: %s\n", fl.Name, clip(fl.Value, 600))
	}
	if f.Body != "" {
		b.WriteString("\n" + clip(f.Body, 4000) + "\n")
	}
	body, err := fence(b.String())
	if err != nil {
		return "", err
	}
	return "It's now showing in the user's right-hand pane.\n" + body, nil
}

// botName is the name the rules match "who" against for a session of the
// adapter kind: "grok-scout" for kind grok and session scout. The daemon
// routes it back to that adapter by its prefix.
func botName(kind, session string) string {
	switch {
	case session == "":
		return kind
	case session == kind, strings.HasPrefix(session, kind+"-"), strings.HasPrefix(session, kind+":"):
		return session
	}
	return kind + "-" + session
}

func (p *Pane) sendToBot(ctx context.Context, session, text string) (string, error) {
	kind, id, ok := strings.Cut(strings.TrimPrefix(session, "session:"), ":")
	if !strings.HasPrefix(session, "session:") || !ok || kind == "" || id == "" {
		return "", fmt.Errorf("session must be a session id from the doing tab, like session:saddle:abc")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("there's nothing to send")
	}
	// The session's repo, as lasso worked it out, so a rule limited to
	// some repos can match. A session not on the tab yet just has none.
	var repo string
	if f, err := p.api.focus(ctx, session); err == nil {
		repo = f.Repo
	}
	out, err := p.api.act(ctx, rules.Request{
		Bot: botName(kind, id), Action: rules.SendMessage, Repo: repo, Target: id,
		Args: map[string]string{"text": text},
	})
	if err != nil {
		return "", err
	}
	return done(out), nil
}

// answer finds the ask that prompt (a prompt id or an item id) belongs to,
// and sends the choice through the rules as that bot's answer.
func (p *Pane) answer(ctx context.Context, prompt, choice string) (string, error) {
	prompt, choice = strings.TrimSpace(prompt), strings.TrimSpace(choice)
	if prompt == "" || choice == "" {
		return "", fmt.Errorf("answer needs a prompt id and a choice")
	}
	f, err := p.findAsk(ctx, prompt)
	if err != nil {
		return "", err
	}
	kind, session, promptID := field(f, "Bot"), field(f, "Session"), field(f, "Prompt")
	if kind == "" || promptID == "" {
		return "", fmt.Errorf("%s isn't a bot prompt lasso can answer", f.ID)
	}
	args := map[string]string{}
	if n, err := strconv.Atoi(choice); err == nil && n > 0 {
		args["option"] = choice
	} else {
		args["text"] = choice
	}
	// What the prompt would do, when its item says, goes on the request so
	// the always-ask list sees it: answering yes to "may I merge to main?"
	// is a merge to main.
	var effects []rules.Effect
	for _, e := range splitList(field(f, fieldEffects)) {
		effects = append(effects, rules.Effect(e))
	}
	out, err := p.api.act(ctx, rules.Request{
		Bot: botName(kind, session), Action: rules.AnswerPrompt, Repo: f.Repo, Target: promptID, Args: args,
		Effects: effects, Paths: splitList(field(f, fieldPaths)),
	})
	if err != nil {
		return "", err
	}
	return done(out), nil
}

// findAsk returns the In progress item for prompt: the item with that id,
// or the ask whose Prompt field is prompt.
func (p *Pane) findAsk(ctx context.Context, prompt string) (api.Focus, error) {
	if f, err := p.api.focus(ctx, prompt); err == nil {
		return f, nil
	}
	items, err := p.api.items(ctx, api.TabDoing)
	if err != nil {
		return api.Focus{}, err
	}
	for _, it := range items {
		f, err := p.api.focus(ctx, it.ID)
		if err == nil && field(f, "Prompt") == prompt {
			return f, nil
		}
	}
	return api.Focus{}, fmt.Errorf("no question or prompt with id %q is waiting; list the doing tab to find it", prompt)
}

// Focus fields an ask may carry about what answering it would do. The pane
// forwards them on the answer, so the always-ask list applies.
const (
	fieldEffects = "Effects" // comma-separated rules.Effect values
	fieldPaths   = "Paths"   // comma-separated repo-relative files
)

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func field(f api.Focus, name string) string {
	for _, fl := range f.Fields {
		if fl.Name == name {
			return fl.Value
		}
	}
	return ""
}

func done(out rules.Outcome) string {
	s := "Done"
	if out.Decision.Rule != "" {
		s += ", allowed by the rule " + out.Decision.Rule
	}
	s += "."
	if out.Detail != "" {
		s += " " + out.Detail
	}
	return s
}

var nonID = regexp.MustCompile(`[^a-z0-9]+`)

// draftRule checks a rule and renders it as TOML. It writes nothing: the
// pane has no way to save a rule, and only the user can add one.
func (p *Pane) draftRule(r rules.Rule) (string, error) {
	r.ID = strings.Trim(nonID.ReplaceAllString(strings.ToLower(r.Who+"-"+string(r.Action)), "-"), "-")
	if r.PerDay == 0 {
		r.PerDay = rules.DraftPerDay
	}
	r.Expires = p.now().Add(rules.DraftLifetime).UTC().Truncate(time.Second)
	if err := r.Validate(); err != nil {
		return "", err
	}
	b, err := rules.Encode(rules.File{Rules: []rules.Rule{r}})
	if err != nil {
		return "", err
	}
	return "Draft only; nothing was saved. Show it to the user as it is, and say that if they want it they add it to rules.toml themselves. " +
		fmt.Sprintf("It lets %s %s, at most %d times a day, until %s.\n\n```toml\n%s```",
			r.Who, r.Action.Plain(), r.PerDay, r.Expires.Format("2 Jan 2006"), b), nil
}

// explain looks term up in the glossary.
func explain(term string) string {
	t := strings.ToLower(strings.TrimSpace(term))
	for _, e := range attention.Glossary {
		if strings.ToLower(e.Jargon) == t {
			return fmt.Sprintf("%q means: %s. Said in a sentence: %q → %q.", e.Jargon, e.Gloss, e.Jargon, e.Plain)
		}
	}
	return fmt.Sprintf("%q isn't in lasso's glossary. Explain it from context in plain words, and say that you're guessing if you are.", term)
}

// fence marks text from GitHub and the bots as data.
func fence(s string) (string, error) { return adapter.Untrusted(s).Fenced() }

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// ago says a duration the way a person would.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
