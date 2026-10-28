package rules

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/attention"
)

// Suggestion thresholds (ADR 0001 §6): the same answer 3 times in 7 days.
const (
	SuggestAfter  = 3
	SuggestWindow = 7 * 24 * time.Hour
	// DraftPerDay and DraftLifetime bound a drafted rule, so accepting a
	// suggestion never grants an open-ended permission.
	DraftPerDay   = 5
	DraftLifetime = 30 * 24 * time.Hour
)

// Answer is one answer the user gave a bot (the store's answers table).
type Answer struct {
	Bot    string
	Action Action
	// Choice is the answer; only "yes" answers can become a rule, since a
	// rule can only allow.
	Choice string
	Repo   string
	At     time.Time
}

// Suggestion is an offer to turn repeated answers into a rule. Nothing is
// written until the user confirms it (see Confirm).
type Suggestion struct {
	Rule Rule `json:"rule"`
	// TOML is the drafted rule as it would be added to rules.toml.
	TOML string `json:"toml"`
	// Count is how many matching answers were seen in the window.
	Count int `json:"count"`
	// Item is the brief item that offers it.
	Item attention.Item `json:"item"`
}

// Suggest looks at answers and, for each bot and action the user said yes
// to SuggestAfter or more times in the SuggestWindow before now, drafts a
// rule, unless f already has a live rule for that bot and action. It has
// no side effects.
func Suggest(answers []Answer, f File, now time.Time) []Suggestion {
	type key struct {
		bot    string
		action Action
	}
	groups := map[key][]Answer{}
	for _, a := range answers {
		if !isYes(a.Choice) || !a.Action.Valid() || a.Bot == "" {
			continue
		}
		if a.At.After(now) || now.Sub(a.At) > SuggestWindow {
			continue
		}
		k := key{a.Bot, a.Action}
		groups[k] = append(groups[k], a)
	}
	var out []Suggestion
	for k, as := range groups {
		if len(as) < SuggestAfter || f.hasLiveRule(k.bot, k.action, now) {
			continue
		}
		r := draft(k.bot, k.action, as, f, now)
		block, err := Encode(File{Rules: []Rule{r}})
		if err != nil {
			continue
		}
		out = append(out, Suggestion{
			Rule: r, TOML: string(block), Count: len(as),
			Item: attention.Item{
				ID:      "rule-suggestion:" + r.ID,
				Source:  "lasso",
				Repo:    r.repoOrEmpty(),
				Bot:     k.bot,
				Kind:    attention.KindRuleSuggestion,
				Level:   attention.Brief,
				Created: now,
				Title:   fmt.Sprintf("Let %s %s without asking?", k.bot, k.action.Plain()),
				Summary: fmt.Sprintf("You said yes %d times this week. lasso can make that a rule, at most %d a day, for 30 days. Nothing changes until you confirm.", len(as), DraftPerDay),
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rule.ID < out[j].Rule.ID })
	return out
}

// Confirm writes a drafted rule to the rules file at p. Call it only after
// the user has seen the draft and said yes.
func Confirm(p string, r Rule) error { return Append(p, r) }

func isYes(choice string) bool {
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "yes", "y", "allow", "approve", "approved", "ok":
		return true
	}
	return false
}

func (f File) hasLiveRule(bot string, action Action, now time.Time) bool {
	for _, r := range f.Rules {
		if r.Action == action && globMatch(r.Who, bot) && (r.Expires.IsZero() || now.Before(r.Expires)) {
			return true
		}
	}
	return false
}

var idUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// draft builds a narrow rule from the answers: this bot exactly, this
// action, the one repo if they all came from it, a daily cap and an expiry.
func draft(bot string, action Action, as []Answer, f File, now time.Time) Rule {
	base := strings.Trim(idUnsafe.ReplaceAllString(strings.ToLower(bot+"-"+string(action)), "-"), "-")
	id := base
	for n := 2; f.hasID(id); n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	r := Rule{
		ID: id, Who: bot, Action: action, PerDay: DraftPerDay,
		Expires: now.Add(DraftLifetime).UTC().Truncate(time.Second),
	}
	repo := as[0].Repo
	for _, a := range as[1:] {
		if a.Repo != repo {
			repo = ""
			break
		}
	}
	if repo != "" {
		r.Repos = []string{repo}
	}
	return r
}

func (f File) hasID(id string) bool {
	for _, r := range f.Rules {
		if r.ID == id {
			return true
		}
	}
	return false
}

func (r Rule) repoOrEmpty() string {
	if len(r.Repos) == 1 {
		return r.Repos[0]
	}
	return ""
}
