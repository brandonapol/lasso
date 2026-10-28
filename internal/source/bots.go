package source

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/rules"
)

// Bot adapters (ADR 0001 §4) feed the In progress tab: each adapter's
// sessions are rows there, and what a bot is waiting on the user for
// becomes an ask with a decision card. Trouble reaching a bot is one brief
// item per adapter, the same as a GitHub repo that can't be read.
//
// None of this is stored: after a restart the adapters are read from the
// start again, which gives back everything still waiting (ADR 0001 §2).

// BotsEvery is how often the adapters are read.
const BotsEvery = 30 * time.Second

// RefreshEvery is the longest the source goes without rebuilding its rows,
// so time-based things (line-jumpers, suggestions) move even when nothing
// is polled.
const RefreshEvery = 5 * time.Minute

// kindBotError is the attention kind of a bot lasso can't reach.
const kindBotError attention.Kind = "bot_error"

func botEventID(kind, id string) string         { return "bot:" + kind + ":" + id }
func sessionID(kind, id string) string          { return "session:" + kind + ":" + id }
func botErrorID(kind string) string             { return "bot-error:" + kind }
func suggestionRowID(s rules.Suggestion) string { return s.Item.ID }

// botProblem is why one adapter can't be read.
type botProblem struct {
	since time.Time
	// reported is what the adapter said through Report; polled is the
	// error its last read returned. Either one makes it a problem.
	reported, polled string
}

func (p botProblem) text() string {
	var parts []string
	for _, m := range []string{p.reported, p.polled} {
		if m != "" {
			parts = append(parts, m)
		}
	}
	return strings.Join(parts, "\n")
}

// Report records trouble reaching the adapter kind, as the adapter's own
// Report hook says it; msg "" means it is reachable again. A problem lasts
// as one brief item, keeping the time it started, until it clears. Safe
// from any goroutine.
func (s *Source) Report(kind, msg string) {
	s.botMu.Lock()
	p, had := s.problems[kind]
	if !had {
		p.since = s.opts.Clock.Now()
	}
	changed := p.reported != msg
	p.reported = msg
	if p.text() == "" {
		delete(s.problems, kind)
	} else {
		s.problems[kind] = p
	}
	s.botMu.Unlock()
	if changed {
		if msg == "" {
			s.logf("lasso: %s: reachable again", kind)
		} else {
			s.logf("lasso: %s: %s", kind, msg)
		}
		s.Wake()
	}
}

// Resolve drops what the adapter kind was waiting on for promptID, once it
// has been answered through lasso, without waiting for the next read. Safe
// from any goroutine.
func (s *Source) Resolve(kind, promptID string) {
	s.botMu.Lock()
	for id, e := range s.events {
		if e.Bot == kind && (e.PromptID == promptID || e.ID == promptID) {
			delete(s.events, id)
		}
	}
	s.botMu.Unlock()
	s.Wake()
}

// Wake makes Run rebuild the rows now (after an answer, or when the rules
// or answers behind a suggestion change) rather than at the next poll.
func (s *Source) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// WakeBots makes Run read the bot adapters now rather than at the next
// BotsEvery (a transcript changed). Safe from any goroutine.
func (s *Source) WakeBots() {
	s.botsDue.Store(true)
	s.Wake()
}

// waitsOnUser reports whether an event is the bot stopping for the user.
func waitsOnUser(e adapter.Event) bool {
	return e.Kind == attention.KindPermissionPrompt || e.Kind == attention.KindBotQuestion
}

// pollBots reads every adapter's sessions and new events. An adapter that
// can't be read keeps its last state, so a bot going quiet for a minute
// doesn't drop the asks it has waiting.
func (s *Source) pollBots(ctx context.Context) {
	for _, a := range s.opts.Adapters {
		if ctx.Err() != nil {
			return
		}
		kind := a.Kind()
		sess, err := a.Sessions(ctx)
		var evs []adapter.Event
		var cur adapter.Cursor
		if err == nil {
			s.botMu.Lock()
			since := s.cursors[kind]
			s.botMu.Unlock()
			evs, cur, err = a.Events(ctx, since)
		}
		s.settleBot(kind, err)
		if err == nil {
			s.learnRepos(kind, sess)
		}
		s.botMu.Lock()
		if s.problems[kind].text() != "" {
			s.botMu.Unlock()
			continue
		}
		s.cursors[kind] = cur
		s.sessions[kind] = sess
		fresh := map[string]bool{}
		for i := range evs {
			evs[i].Bot = kind
			e := evs[i]
			if s.opts.Digest != nil && !waitsOnUser(e) {
				continue // the digest speaks for it
			}
			id := botEventID(kind, e.ID)
			s.events[id] = e
			fresh[id] = true
		}
		s.queueDigest(evs)
		s.pruneEvents(kind, sess, fresh)
		s.botMu.Unlock()
	}
}

// learnRepos asks SessionRepo about sessions it hasn't seen. s.botMu must
// not be held: it may run git.
func (s *Source) learnRepos(kind string, sess []adapter.Session) {
	if s.opts.SessionRepo == nil {
		return
	}
	for _, se := range sess {
		key := repoKey(kind, se.ID)
		s.botMu.Lock()
		repo, known := s.sessionRepo[key]
		s.botMu.Unlock()
		if known && (repo != "" || se.PR == "") {
			continue
		}
		repo = s.opts.SessionRepo(kind, se)
		s.botMu.Lock()
		s.sessionRepo[key] = repo
		s.botMu.Unlock()
	}
}

// settleBot records the error the adapter kind's last read returned, or
// clears it. s.botMu must not be held.
func (s *Source) settleBot(kind string, err error) {
	msg := ""
	if err != nil {
		msg = "lasso couldn't read " + kind + ": " + err.Error()
	}
	s.botMu.Lock()
	p, had := s.problems[kind]
	if !had {
		p.since = s.opts.Clock.Now()
	}
	changed := p.polled != msg
	p.polled = msg
	if p.text() == "" {
		delete(s.problems, kind)
	} else {
		s.problems[kind] = p
	}
	s.botMu.Unlock()
	if changed && msg != "" {
		s.logf("lasso: %s", msg)
	}
}

// pruneEvents drops kind's events the bot has moved past: a prompt or
// question whose session is no longer waiting, and anything whose session
// is gone. Events delivered in this read (fresh) are kept. s.botMu must be
// held.
func (s *Source) pruneEvents(kind string, sess []adapter.Session, fresh map[string]bool) {
	state := map[string]adapter.State{}
	for _, se := range sess {
		state[se.ID] = se.State
	}
	for id, e := range s.events {
		if e.Bot != kind || fresh[id] || e.Session == "" {
			continue
		}
		st, ok := state[e.Session]
		waitsOnUser := e.Kind == attention.KindPermissionPrompt || e.Kind == attention.KindBotQuestion
		if !ok || (waitsOnUser && st != adapter.Waiting) {
			delete(s.events, id)
		}
	}
}

// botItems are the bots' attention items: what they wait on the user for,
// and one per adapter that can't be read.
func (s *Source) botItems(ctx context.Context) []attention.Item {
	s.botMu.Lock()
	evs := make([]adapter.Event, 0, len(s.events))
	for _, e := range s.events {
		evs = append(evs, e)
	}
	problems := make(map[string]botProblem, len(s.problems))
	for k, p := range s.problems {
		problems[k] = p
	}
	repos := maps.Clone(s.sessionRepo)
	s.botMu.Unlock()

	var items []attention.Item
	for _, e := range evs {
		it := e.Item()
		it.ID = botEventID(e.Bot, e.ID)
		it.Repo = repos[repoKey(e.Bot, e.Session)]
		it.Level = s.level(ctx, it)
		if it.Level == attention.Ask {
			it.Card = decisionCard(e, attention.Classify(ctx, it, nil).Why)
		}
		items = append(items, it)
	}
	for kind, p := range problems {
		items = append(items, attention.Item{
			ID: botErrorID(kind), Source: kind, Kind: kindBotError, Level: attention.Brief,
			Title: "Can't reach " + kind, Summary: p.text(), Created: p.since,
		})
	}
	return items
}

// decisionCard is the card for a bot's ask, in lasso's own words. The
// bot's text is never part of it: it is shown beside the card as quoted
// bot output.
func decisionCard(e adapter.Event, why string) *attention.DecisionCard {
	who := e.Bot
	if e.Session != "" {
		who = fmt.Sprintf("%s (%s)", e.Session, e.Bot)
	}
	c := attention.DecisionCard{WhyAsking: why}
	if e.Kind == attention.KindPermissionPrompt {
		c.Wants = who + " wants your permission before it goes on."
	} else {
		c.Wants = who + " has stopped to ask you something."
	}
	if len(e.Options) > 0 {
		opts := make([]string, len(e.Options))
		for i, o := range e.Options {
			opts[i] = fmt.Sprintf("%d) %s", o.N, o.Label)
		}
		c.Plain = fmt.Sprintf("It is waiting until you pick one of: %s.", strings.Join(opts, "; "))
		c.IfYes = "lasso sends the option you pick, and " + e.Bot + " uses it only if that same question is still waiting."
		c.IfNo = "Nothing is sent, and it keeps waiting."
		c.Recommendation = "Read its words below before picking: lasso doesn't pick for a bot's own question."
	} else {
		c.Plain = "It is waiting for a reply only you can give."
		c.IfYes = "lasso can't answer this one from here yet: reply to it in " + e.Bot + "."
		c.IfNo = "It keeps waiting."
		c.Recommendation = "Reply in " + e.Bot + " when you have a moment."
	}
	pc := attention.PlainCard(c)
	return &pc
}

// askFocus is the focus pane for an item with no row of its own (a bot's
// ask): its decision card, then what to answer with.
func askFocus(it attention.Item, e adapter.Event, isBot bool) api.Focus {
	f := api.Focus{
		Item: api.Item{ID: it.ID, Repo: it.Repo, Title: it.Title, Updated: it.Created},
		Body: it.Summary,
	}
	if c := it.Card; c != nil {
		f.Fields = append(f.Fields,
			api.Field{Name: "Wants", Value: c.Wants},
			api.Field{Name: "Why it's asking", Value: c.WhyAsking},
			api.Field{Name: "In plain words", Value: c.Plain},
			api.Field{Name: "If yes", Value: c.IfYes},
			api.Field{Name: "If no", Value: c.IfNo},
			api.Field{Name: "lasso suggests", Value: c.Recommendation},
		)
		for _, sec := range []api.Section{
			{Label: "What it wants", Body: c.Wants},
			{Label: "Why it's asking", Body: c.WhyAsking},
			{Label: "In plain words", Body: c.Plain},
			{Label: "If yes", Body: c.IfYes},
			{Label: "If no", Body: c.IfNo},
		} {
			if strings.TrimSpace(sec.Body) != "" {
				f.Sections = append(f.Sections, sec)
			}
		}
		f.Take = c.Recommendation
	}
	if text := strings.TrimSpace(it.Summary); text != "" {
		label := "What it said"
		if isBot {
			label = "What " + e.Bot + " said"
		}
		f.Sections = append(f.Sections, api.Section{Label: label, Body: quote(text)})
	}
	if isBot && e.PromptID != "" {
		f.Actions = answerActions(e, it.Repo)
	}
	if isBot {
		f.Fields = append(f.Fields, api.Field{Name: "Bot", Value: e.Bot})
		if e.Session != "" {
			f.Fields = append(f.Fields, api.Field{Name: "Session", Value: e.Session})
		}
		if e.PromptID != "" {
			f.Fields = append(f.Fields, api.Field{Name: "Prompt", Value: e.PromptID})
			opts := make([]string, len(e.Options))
			for i, o := range e.Options {
				opts[i] = strconv.Itoa(o.N) + " " + string(o.Label)
			}
			f.Fields = append(f.Fields, api.Field{Name: "Options", Value: strings.Join(opts, "; ")})
		}
	}
	return f
}

// quote marks every line of a bot's words as a quotation.
func quote(text string) string {
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		lines[i] = "> " + ln
	}
	return strings.Join(lines, "\n")
}

// answerKeys are the keys an ask's options go on: y and n for the first
// yes and no, then letters no other key uses. Digits switch tabs.
var answerKeys = []string{"a", "b", "c", "d", "e", "f", "h", "i"}

// answerActions are an ask's options, each an answer_prompt the user's key
// sends as is. args.choice says the answer in plain words for suggested
// rules: "yes", "no", or the option.
func answerActions(e adapter.Event, repo string) []api.Action {
	var out []api.Action
	used := map[string]bool{}
	next := 0
	for _, o := range e.Options {
		label := strings.TrimSpace(string(o.Label))
		choice := choiceWord(label)
		key := ""
		switch {
		case choice == "yes" && !used["y"]:
			key = "y"
		case choice == "no" && !used["n"]:
			key = "n"
		case next < len(answerKeys):
			key = answerKeys[next]
			next++
		default:
			continue
		}
		used[key] = true
		if choice == "" {
			choice = "option " + strconv.Itoa(o.N)
		}
		out = append(out, api.Action{Key: key, Label: label, Kind: string(rules.AnswerPrompt), Request: &rules.Request{
			Bot: botName(e.Bot, e.Session), Action: rules.AnswerPrompt, Repo: repo, Target: e.PromptID,
			Args: map[string]string{"option": strconv.Itoa(o.N), "choice": choice},
		}})
	}
	return out
}

// choiceWord is "yes" or "no" for an option that plainly says so, else "".
func choiceWord(label string) string {
	w := strings.ToLower(strings.TrimSpace(label))
	end := strings.IndexFunc(w, func(r rune) bool { return r < 'a' || r > 'z' })
	if end >= 0 {
		w = w[:end]
	}
	switch w {
	case "yes", "y", "allow", "approve", "ok", "proceed", "continue":
		return "yes"
	case "no", "n", "deny", "reject", "cancel", "stop":
		return "no"
	}
	return ""
}

// botName is the name the rules match "who" against for a session of the
// adapter kind, as the agent pane names it: "grok-scout" for kind grok and
// session scout. The daemon routes it back to that adapter by its prefix.
func botName(kind, session string) string {
	switch {
	case session == "":
		return kind
	case session == kind, strings.HasPrefix(session, kind+"-"), strings.HasPrefix(session, kind+":"):
		return session
	}
	return kind + "-" + session
}

// botRows lays out the sessions and adapter problems on In progress.
func (s *Source) botRows(add func(row)) {
	counts := s.sessionCounts()
	s.botMu.Lock()
	defer s.botMu.Unlock()
	for kind, p := range s.problems {
		add(row{group: 0, Focus: api.Focus{
			Item: api.Item{ID: botErrorID(kind), Tab: api.TabDoing, Title: "Can't reach " + kind, Status: kind + " unreachable", Updated: p.since},
			Fields: []api.Field{
				{Name: "What happened", Value: p.text()},
				{Name: "Since", Value: p.since.UTC().Format(time.RFC3339)},
			},
			Sections: []api.Section{
				{Label: "What happened", Body: p.text()},
				{Label: "What lasso does", Body: "lasso keeps showing what it last read from " + kind + " and tries again every " + BotsEvery.String() + "."},
			},
			Body: "lasso keeps showing what it last read from " + kind + " and tries again every " + BotsEvery.String() + ".",
		}})
	}
	kinds := sortedKeys(s.sessions)
	seq := 0
	for _, kind := range kinds {
		for _, se := range s.sessions[kind] {
			seq++
			fields := []api.Field{
				{Name: "Bot", Value: kind},
				{Name: "State", Value: sessionStatus(se.State)},
			}
			if se.PR != "" {
				fields = append(fields, api.Field{Name: "PR", Value: se.PR})
			}
			send := "yes"
			if !se.Writable {
				send = "no: lasso can only read this one"
			}
			fields = append(fields, api.Field{Name: "Can lasso send to it", Value: send})
			status := sessionStatus(se.State)
			if c, ok := counts[se.ID]; ok {
				status += " · " + c.String()
				fields = append(fields, api.Field{Name: "Messages → points", Value: c.String() + " (what the digest kept to tell you)"})
			}
			repo := s.sessionRepo[repoKey(kind, se.ID)]
			if repo != "" {
				fields = append(fields, api.Field{Name: "Repo", Value: repo})
			}
			now := "It is " + sessionStatus(se.State) + "."
			if c, ok := counts[se.ID]; ok {
				now += " Of its last " + c.String() + " messages, the digest kept the ones on the right of the arrow to tell you."
			}
			secs := []api.Section{{Label: "Now", Body: now}}
			if se.PR != "" {
				secs = append(secs, api.Section{Label: "Pull request", Body: se.PR})
			}
			if !se.Writable {
				secs = append(secs, api.Section{Label: "Sending", Body: "lasso can only read this session, not send to it."})
			}
			add(row{group: 3, seq: seq, Focus: api.Focus{
				Item:     api.Item{ID: sessionID(kind, se.ID), Tab: api.TabDoing, Repo: repo, Title: se.Title, Status: status},
				Fields:   fields,
				Sections: secs,
			}})
		}
	}
}

func sessionStatus(st adapter.State) string {
	switch st {
	case adapter.Running:
		return "working"
	case adapter.Idle:
		return "idle"
	case adapter.Waiting:
		return "waiting on you"
	case adapter.Done:
		return "done, waiting to be added"
	}
	return string(st)
}

// botEvent returns the event behind an item id, if it is a bot's.
func (s *Source) botEvent(id string) (adapter.Event, bool) {
	s.botMu.Lock()
	defer s.botMu.Unlock()
	e, ok := s.events[id]
	return e, ok
}

// suggestionItems asks for suggested rules. A suggestion that stands keeps
// the draft and time it was first offered with (only its count moves), so
// it doesn't look new, or change its expiry, on every refresh.
func (s *Source) suggestionItems(now time.Time) ([]attention.Item, []rules.Suggestion) {
	if s.opts.Suggest == nil {
		return nil, nil
	}
	sugs, err := s.opts.Suggest(now)
	if err != nil {
		if msg := err.Error(); msg != s.suggestErr {
			s.suggestErr = msg
			s.logf("lasso: suggested rules: %s", msg)
		}
		return nil, nil
	}
	s.suggestErr = ""
	live := map[string]bool{}
	var items []attention.Item
	for i := range sugs {
		id := sugs[i].Item.ID
		live[id] = true
		if first, ok := s.suggested[id]; ok {
			first.Count, first.Item.Summary = sugs[i].Count, sugs[i].Item.Summary
			sugs[i] = first
		}
		s.suggested[id] = sugs[i]
		items = append(items, sugs[i].Item)
	}
	for id := range s.suggested {
		if !live[id] {
			delete(s.suggested, id)
		}
	}
	return items, sugs
}

// suggestionRows puts each suggested rule on the Summaries tab.
func suggestionRows(sugs []rules.Suggestion, add func(row)) {
	sort.Slice(sugs, func(i, j int) bool { return sugs[i].Rule.ID < sugs[j].Rule.ID })
	for i, sg := range sugs {
		r := sg.Rule
		fields := []api.Field{
			{Name: "Who", Value: r.Who},
			{Name: "May", Value: r.Action.Plain()},
			{Name: "Times you said yes this week", Value: strconv.Itoa(sg.Count)},
			{Name: "At most", Value: fmt.Sprintf("%d a day", r.PerDay)},
			{Name: "Until", Value: r.Expires.Format(time.DateOnly)},
		}
		if len(r.Repos) > 0 {
			fields = slices.Insert(fields, 2, api.Field{Name: "In", Value: strings.Join(r.Repos, ", ")})
		}
		add(row{group: 0, seq: i, Focus: api.Focus{
			Item: api.Item{
				ID: suggestionRowID(sg), Tab: api.TabSummaries, Repo: sg.Item.Repo,
				Title: sg.Item.Title, Status: "suggested rule", Updated: sg.Item.Created, Body: sg.Item.Summary,
			},
			Fields: fields,
			Sections: []api.Section{
				{Label: "Why", Body: sg.Item.Summary},
				{Label: "The rule it would add to rules.toml", Body: "```\n" + strings.TrimSpace(sg.TOML) + "\n```"},
			},
			Body: sg.Item.Summary + "\n\nThe rule it would add to rules.toml:\n\n" + sg.TOML,
		}})
	}
}
