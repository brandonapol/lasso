package github

import "time"

// DueSource says where an issue's due date came from (ADR 0001 §3).
type DueSource string

// The due-date sources, in order of precedence.
const (
	DueNone      DueSource = ""
	DueProject   DueSource = "project"   // a Projects v2 date field named "Due" or "Target date"
	DueMilestone DueSource = "milestone" // the milestone's due_on
	DueBody      DueSource = "body"      // a "Due: YYYY-MM-DD" line in the body
)

// Issue is an open issue in a hot repo.
type Issue struct {
	Repo      string
	Number    int
	Title     string
	Body      string
	URL       string
	Labels    []string
	Assignees []string
	Milestone string
	Updated   time.Time
	// Due is the due date at midnight UTC, zero when there is none.
	Due       time.Time
	DueSource DueSource
}

// CheckState is a PR's combined checks state.
type CheckState string

// The check states, from the head commit's status check rollup.
const (
	ChecksNone    CheckState = ""
	ChecksPending CheckState = "pending"
	ChecksPassing CheckState = "success"
	ChecksFailing CheckState = "failure"
)

// ReviewState is a PR's review decision.
type ReviewState string

// The review states.
const (
	ReviewNone             ReviewState = ""
	ReviewRequired         ReviewState = "review_required"
	ReviewApproved         ReviewState = "approved"
	ReviewChangesRequested ReviewState = "changes_requested"
)

// PR is an open pull request in a hot repo.
type PR struct {
	Repo    string
	Number  int
	Title   string
	URL     string
	Author  string
	Branch  string
	Draft   bool
	Checks  CheckState
	Review  ReviewState
	Updated time.Time
}

// Run is one GitHub Actions workflow run.
type Run struct {
	Repo     string
	ID       int64
	Attempt  int
	Workflow string
	Branch   string
	// Status is queued, in_progress, completed, and so on.
	Status string
	// Conclusion is set once Status is completed: success, failure, ...
	Conclusion string
	// FailingJob and FailingStep name the first failed job and its first
	// failed step, for a failed run (see FailingStep).
	FailingJob  string
	FailingStep string
	Started     time.Time
	// Finished is zero until the run completes.
	Finished time.Time
	URL      string
}

// InProgress reports whether the run hasn't completed yet.
func (r Run) InProgress() bool { return r.Status != "completed" }

// Failed reports whether the run completed without succeeding in a way
// worth a failing step: failure, timed out or startup failure.
func (r Run) Failed() bool {
	switch r.Conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

// Milestone is an open milestone in a hot repo.
type Milestone struct {
	Repo   string
	Number int
	Title  string
	URL    string
	// Due is the due date at midnight UTC, zero when there is none.
	Due    time.Time
	Open   int
	Closed int
}
