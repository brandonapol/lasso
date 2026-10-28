package github

import (
	"testing"
	"time"
)

func date(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestResolveDuePrecedence(t *testing.T) {
	msDue := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	body := "Some text.\n\nDue: 2026-12-01\n"
	tests := []struct {
		name      string
		project   []ProjectDate
		milestone time.Time
		body      string
		want      string
		source    DueSource
	}{
		{"project beats milestone and body", []ProjectDate{{"Due", "2026-11-05"}}, msDue, body, "2026-11-05", DueProject},
		{"target date field counts", []ProjectDate{{"Target date", "2026-11-06"}}, msDue, body, "2026-11-06", DueProject},
		{"field name is case-insensitive", []ProjectDate{{"target Date", "2026-11-07"}}, time.Time{}, "", "2026-11-07", DueProject},
		{"earliest project date wins", []ProjectDate{{"Due", "2026-11-09"}, {"Target date", "2026-11-08"}}, time.Time{}, "", "2026-11-08", DueProject},
		{"other date fields are ignored", []ProjectDate{{"Start date", "2026-10-01"}}, msDue, body, "2026-11-20", DueMilestone},
		{"milestone beats body", nil, msDue, body, "2026-11-20", DueMilestone},
		{"body when nothing else", nil, time.Time{}, body, "2026-12-01", DueBody},
		{"body line is case-insensitive and may be indented", nil, time.Time{}, "  due: 2026-12-02\r\nmore", "2026-12-02", DueBody},
		{"due mid-line doesn't count", nil, time.Time{}, "we said Due: 2026-12-03 maybe", "", DueNone},
		{"bad project date falls through", []ProjectDate{{"Due", "soon"}}, time.Time{}, body, "2026-12-01", DueBody},
		{"none", nil, time.Time{}, "nothing here", "", DueNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, src := ResolveDue(tt.project, tt.milestone, tt.body)
			if src != tt.source {
				t.Fatalf("source = %q, want %q", src, tt.source)
			}
			if tt.want == "" {
				if !got.IsZero() {
					t.Fatalf("due = %v, want none", got)
				}
				return
			}
			if !got.Equal(date(tt.want)) {
				t.Fatalf("due = %v, want %s", got, tt.want)
			}
		})
	}
}

func TestMilestoneDueUsesUTCDate(t *testing.T) {
	// GitHub returns "November 1" set from California as 07:00Z.
	got, _ := ResolveDue(nil, time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC), "")
	if !got.Equal(date("2026-11-01")) {
		t.Fatalf("due = %v", got)
	}
}
