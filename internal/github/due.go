package github

import (
	"regexp"
	"strings"
	"time"
)

// ProjectDate is one Projects v2 date field value on an issue.
type ProjectDate struct {
	Field string // the field's name, e.g. "Due"
	Date  string // YYYY-MM-DD
}

// dueFields are the Projects v2 date fields that set a due date, compared
// case-insensitively.
var dueFields = []string{"Due", "Target date"}

// dueLine matches a "Due: YYYY-MM-DD" line in an issue body.
var dueLine = regexp.MustCompile(`(?im)^[ \t]*due:[ \t]*(\d{4}-\d{2}-\d{2})[ \t]*\r?$`)

// ResolveDue picks an issue's due date, first that exists (ADR 0001 §3):
//
//  1. a Projects v2 date field named "Due" or "Target date" (the earliest,
//     when the issue is on several projects);
//  2. its milestone's due_on (milestoneDue, zero when none);
//  3. a "Due: YYYY-MM-DD" line in the body.
//
// The date comes back at midnight UTC.
func ResolveDue(project []ProjectDate, milestoneDue time.Time, body string) (time.Time, DueSource) {
	var best time.Time
	for _, p := range project {
		if !isDueField(p.Field) {
			continue
		}
		d, err := time.Parse(time.DateOnly, p.Date)
		if err != nil {
			continue
		}
		if best.IsZero() || d.Before(best) {
			best = d
		}
	}
	if !best.IsZero() {
		return best, DueProject
	}
	if !milestoneDue.IsZero() {
		return dateOnly(milestoneDue), DueMilestone
	}
	if m := dueLine.FindStringSubmatch(body); m != nil {
		if d, err := time.Parse(time.DateOnly, m[1]); err == nil {
			return d, DueBody
		}
	}
	return time.Time{}, DueNone
}

func isDueField(name string) bool {
	name = strings.TrimSpace(name)
	for _, f := range dueFields {
		if strings.EqualFold(name, f) {
			return true
		}
	}
	return false
}

// dateOnly truncates t to its UTC date. GitHub stores a milestone's due_on
// as a timestamp in the setter's zone (e.g. 2026-11-01T07:00:00Z for
// November 1 in California), so the UTC date is the date the user picked.
func dateOnly(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
