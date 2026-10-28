package source

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/lasso/internal/adapter"
	"github.com/brandonapol/lasso/internal/api"
	"github.com/brandonapol/lasso/internal/attention"
	"github.com/brandonapol/lasso/internal/digest"
	"github.com/brandonapol/lasso/internal/summary"
)

// The bots' chatter goes through the digest (ADR 0001 §7): every event a
// bot adapter delivers is added to the Digester, and only what a bot waits
// on the user for (a permission prompt, a question) is an item of its own.
// The digest's brief points are brief items on the Summaries tab, and each
// session's running "N → k" is shown on its In progress row.
//
// Digesting can call a model, so it runs on its own goroutine (Run starts
// it): polling never waits on Haiku.

// Digester is what the source needs from digest.Digester.
type Digester interface {
	Add(ctx context.Context, ev adapter.Event) *digest.Digest
	Tick(ctx context.Context) []digest.Digest
	Counts() map[string]digest.Count
	OverCap() bool
}

// SummaryReader gives a hot repo's cached summary (summary.Summarizer).
type SummaryReader interface {
	Get(repo string) (summary.Summary, bool, error)
}

// Notice is a fixed brief item the daemon raises once, such as a missing
// API key. At is when it was first raised, so it is new only once.
type Notice struct {
	ID    string
	Title string
	Body  string
	At    time.Time
}

const (
	// kindDigest is the attention kind of a digest point.
	kindDigest attention.Kind = "bot_digest"
	// kindNotice is the attention kind of a Notice.
	kindNotice attention.Kind = "notice"
	// DigestEvery is how often open digest batches are checked for age.
	DigestEvery = time.Minute
	// PointsKept is how long a digest point stays on the Summaries tab
	// (and is offered to the summaries as a bot's work in a repo).
	PointsKept = summary.Window
	// MaxPoints bounds how many digest points are kept.
	MaxPoints = 200
)

func noticeID(id string) string { return "notice:" + id }

// repoKey is a session's key in Source.sessionRepo.
func repoKey(kind, session string) string { return kind + "\x00" + session }
func summaryID(repo string) string {
	return "summary:" + repoID(repo)
}

// point is one brief point the digest wrote about a bot session.
type point struct {
	id, bot, session, repo, text string
	at                           time.Time
	dg                           digest.Count // the batch's events → points
	cutBy                        string
	local, overCap               bool
}

// queueDigest hands events to the digest goroutine. s.botMu must be held.
func (s *Source) queueDigest(evs []adapter.Event) {
	if s.opts.Digest == nil || len(evs) == 0 {
		return
	}
	s.digestQ = append(s.digestQ, evs...)
	select {
	case s.digestWake <- struct{}{}:
	default:
	}
}

// digestLoop feeds queued events to the Digester and keeps the brief
// points it writes, until ctx ends.
func (s *Source) digestLoop(ctx context.Context) {
	if s.opts.Digest == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.digestWake:
		case <-s.opts.Clock.After(DigestEvery):
		}
		s.DigestStep(ctx)
	}
}

// DigestStep digests what is queued and whatever batches are due, keeps
// the brief points, and wakes Run when there are new ones.
func (s *Source) DigestStep(ctx context.Context) {
	s.botMu.Lock()
	evs := s.digestQ
	s.digestQ = nil
	s.botMu.Unlock()
	var out []digest.Digest
	for _, ev := range evs {
		if ctx.Err() != nil {
			return
		}
		if dg := s.opts.Digest.Add(ctx, ev); dg != nil {
			out = append(out, *dg)
		}
	}
	out = append(out, s.opts.Digest.Tick(ctx)...)
	if s.keepPoints(out) {
		s.Wake()
	}
}

// keepPoints records the brief points of digests, and drops points older
// than PointsKept. It reports whether any were added.
func (s *Source) keepPoints(dgs []digest.Digest) bool {
	now := s.opts.Clock.Now()
	s.botMu.Lock()
	defer s.botMu.Unlock()
	added := false
	for _, dg := range dgs {
		kind, session := dg.Bot, dg.Session
		at := dg.To
		if at.IsZero() {
			at = now
		}
		for _, p := range dg.Points {
			if p.Level != attention.Brief {
				continue
			}
			s.pointSeq++
			s.points = append(s.points, point{
				id: "digest:" + strconv.Itoa(s.pointSeq), bot: kind, session: session,
				repo: s.sessionRepo[repoKey(kind, session)], text: p.Text, at: at,
				dg: digest.Count{Events: dg.Events, Points: len(dg.Points)}, cutBy: dg.CutBy,
				local: dg.Local, overCap: dg.OverCap,
			})
			added = true
		}
	}
	cut := 0
	for cut < len(s.points) && now.Sub(s.points[cut].at) > PointsKept {
		cut++
	}
	if extra := len(s.points) - cut - MaxPoints; extra > 0 {
		cut += extra
	}
	s.points = s.points[cut:]
	return added
}

// Briefs returns the digest's brief points about bots working in repo
// since a time, oldest first: summary.Options.Briefs.
func (s *Source) Briefs(repo string, since time.Time) []string {
	s.botMu.Lock()
	defer s.botMu.Unlock()
	var out []string
	for _, p := range s.points {
		if strings.EqualFold(p.repo, repo) && !p.at.Before(since) {
			out = append(out, p.text)
		}
	}
	return out
}

// digestItems are the kept digest points and the notices, as brief items.
func (s *Source) digestItems() []attention.Item {
	s.botMu.Lock()
	defer s.botMu.Unlock()
	var items []attention.Item
	for _, p := range s.points {
		items = append(items, attention.Item{
			ID: p.id, Source: p.bot, Repo: p.repo, Bot: p.session, Kind: kindDigest, Level: attention.Brief,
			Title: p.text, Created: p.at,
		})
	}
	for _, n := range s.opts.Notices {
		items = append(items, attention.Item{
			ID: noticeID(n.ID), Source: "lasso", Kind: kindNotice, Level: attention.Brief,
			Title: n.Title, Summary: n.Body, Created: n.At,
		})
	}
	return items
}

// digestRows puts the digest points and notices on the Summaries tab.
func (s *Source) digestRows(add func(row)) {
	for i, n := range s.opts.Notices {
		add(row{group: 0, seq: -len(s.opts.Notices) + i, Focus: api.Focus{
			Item: api.Item{ID: noticeID(n.ID), Tab: api.TabSummaries, Title: n.Title, Status: "setup", Updated: n.At},
			Body: n.Body,
		}})
	}
	s.botMu.Lock()
	defer s.botMu.Unlock()
	for _, p := range s.points {
		status := "from " + p.bot
		fields := []api.Field{
			{Name: "Bot", Value: p.bot},
			{Name: "Session", Value: p.session},
			{Name: "Messages → points", Value: p.dg.String()},
		}
		switch {
		case p.overCap:
			status = "counted: digest cap reached"
			fields = append(fields, api.Field{Name: "Written by", Value: "lasso (the digest's daily spending cap is reached; it is [models] haiku_daily_usd)"})
		case p.local:
			fields = append(fields, api.Field{Name: "Written by", Value: "lasso (no model available)"})
		default:
			fields = append(fields, api.Field{Name: "Written by", Value: "the digest model, after " + p.cutBy + " picked what mattered"})
		}
		if p.repo != "" {
			fields = append(fields, api.Field{Name: "Repo", Value: p.repo})
		}
		add(row{group: 2, at: p.at, Focus: api.Focus{
			Item:   api.Item{ID: p.id, Tab: api.TabSummaries, Repo: p.repo, Title: p.text, Status: status, Updated: p.at},
			Fields: fields,
		}})
	}
}

// summaryRows puts each hot repo's summary on the Summaries tab.
func (s *Source) summaryRows(add func(row)) {
	if s.opts.Summaries == nil {
		return
	}
	for i, repo := range s.opts.Repos {
		sum, ok, err := s.opts.Summaries.Get(repo)
		r := row{group: 1, seq: i, Focus: api.Focus{Item: api.Item{ID: summaryID(repo), Tab: api.TabSummaries, Repo: repo}}}
		switch {
		case err != nil:
			r.Title, r.Status = repo+": the summary can't be read", "error"
			r.Body = "lasso couldn't read its saved summary of " + repo + ": " + err.Error() + "\n\nIt writes a new one within the hour."
		case !ok:
			r.Title, r.Status = repo+": no summary yet", "writing"
			r.Body = "lasso writes a summary of each hot repo's last 24 hours once an hour. The first one for " + repo + " is on its way."
		default:
			r.Focus = summaryFocus(sum, r.Focus)
		}
		add(r)
	}
}

func summaryFocus(sum summary.Summary, f api.Focus) api.Focus {
	f.Title = sum.Repo + ": the last 24 hours"
	f.Updated = sum.WrittenAt
	c := sum.Counts
	f.Fields = []api.Field{
		{Name: "Pull requests merged", Value: strconv.Itoa(c.Merged)},
		{Name: "Issues closed", Value: strconv.Itoa(c.Closed)},
		{Name: "Issues opened", Value: strconv.Itoa(c.Opened)},
		{Name: "Releases", Value: strconv.Itoa(c.Releases)},
		{Name: "Checks still failing", Value: strconv.Itoa(c.RedRuns)},
	}
	if sum.Who != "" {
		f.Fields = append(f.Fields, api.Field{Name: "Who did the work", Value: sum.Who})
	}
	written := sum.WrittenAt.UTC().Format(time.RFC3339)
	switch {
	case sum.OverCap:
		f.Status = "counts only: cap reached"
		f.Fields = append(f.Fields,
			api.Field{Name: "Written by", Value: "lasso, at " + written},
			api.Field{Name: "Why counts only", Value: "Today's spending cap for summaries is reached ([models] sonnet_daily_usd), " +
				"so lasso wrote the counts itself. The model writes again tomorrow, or raise the cap."})
	case sum.Local:
		f.Status = "counts only"
		f.Fields = append(f.Fields, api.Field{Name: "Written by", Value: "lasso, at " + written})
		if sum.Note != "" {
			f.Fields = append(f.Fields, api.Field{Name: "Why counts only", Value: sum.Note})
		}
	default:
		f.Status = "written by " + sum.Model
		f.Fields = append(f.Fields, api.Field{Name: "Written by", Value: sum.Model + ", at " + written})
	}
	f.Fields = append(f.Fields, api.Field{Name: "New summary", Value: "press g (at most once an hour on its own)"})
	var b strings.Builder
	b.WriteString(sum.Paragraph)
	if len(sum.Changes) > 0 {
		b.WriteString("\n\nBiggest changes:")
		for _, ch := range sum.Changes {
			fmt.Fprintf(&b, "\n- %s %s", ch.Title, ch.URL)
		}
	}
	f.Body = b.String()
	return f
}

// sessionCount is a session's "N → k" from the digest, if it has one.
func (s *Source) sessionCounts() map[string]digest.Count {
	if s.opts.Digest == nil {
		return nil
	}
	return s.opts.Digest.Counts()
}
