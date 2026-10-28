package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// runPage is how many recent runs are read per repo. The GraphQL queries
// read the first 100 open issues and PRs and 50 open milestones; hot repos
// are expected to stay under that.
const runPage = 30

// splitRepo splits "owner/name".
func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repo %q: want owner/name", repo)
	}
	return owner, name, nil
}

const issuesQuery = `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    issues(states: OPEN, first: 100, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number title body url updatedAt
        labels(first: 20) { nodes { name } }
        assignees(first: 10) { nodes { login } }
        milestone { title dueOn }
        projectItems(first: 10) {
          nodes {
            fieldValues(first: 30) {
              nodes {
                ... on ProjectV2ItemFieldDateValue {
                  date
                  field { ... on ProjectV2FieldCommon { name } }
                }
              }
            }
          }
        }
      }
    }
    pullRequests(states: OPEN, first: 100, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number title url updatedAt isDraft headRefName reviewDecision
        author { login }
        commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
      }
    }
  }
}`

type gqlIssues struct {
	Repository *struct {
		Issues struct {
			Nodes []struct {
				Number    int       `json:"number"`
				Title     string    `json:"title"`
				Body      string    `json:"body"`
				URL       string    `json:"url"`
				UpdatedAt time.Time `json:"updatedAt"`
				Labels    struct {
					Nodes []struct {
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
				Assignees struct {
					Nodes []struct {
						Login string `json:"login"`
					} `json:"nodes"`
				} `json:"assignees"`
				Milestone *struct {
					Title string     `json:"title"`
					DueOn *time.Time `json:"dueOn"`
				} `json:"milestone"`
				ProjectItems struct {
					Nodes []struct {
						FieldValues struct {
							Nodes []struct {
								Date  string `json:"date"`
								Field struct {
									Name string `json:"name"`
								} `json:"field"`
							} `json:"nodes"`
						} `json:"fieldValues"`
					} `json:"nodes"`
				} `json:"projectItems"`
			} `json:"nodes"`
		} `json:"issues"`
		PullRequests struct {
			Nodes []struct {
				Number         int       `json:"number"`
				Title          string    `json:"title"`
				URL            string    `json:"url"`
				UpdatedAt      time.Time `json:"updatedAt"`
				IsDraft        bool      `json:"isDraft"`
				HeadRefName    string    `json:"headRefName"`
				ReviewDecision string    `json:"reviewDecision"`
				Author         *struct {
					Login string `json:"login"`
				} `json:"author"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup *struct {
								State string `json:"state"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"nodes"`
		} `json:"pullRequests"`
	} `json:"repository"`
}

// IssuesAndPRs returns repo's open issues, with due dates resolved, and its
// open PRs. A *PartialError means some fields (usually project dates)
// didn't come back; the rest is returned with it.
func (c *Client) IssuesAndPRs(ctx context.Context, repo string) ([]Issue, []PR, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, nil, err
	}
	var data gqlIssues
	err = c.graphql(ctx, issuesQuery, map[string]string{"owner": owner, "name": name}, &data)
	var partial *PartialError
	if err != nil && !errors.As(err, &partial) {
		return nil, nil, fmt.Errorf("%s issues: %w", repo, err)
	}
	if data.Repository == nil {
		return nil, nil, fmt.Errorf("%s issues: repository not found", repo)
	}
	var issues []Issue
	for _, n := range data.Repository.Issues.Nodes {
		is := Issue{Repo: repo, Number: n.Number, Title: n.Title, Body: n.Body, URL: n.URL, Updated: n.UpdatedAt}
		for _, l := range n.Labels.Nodes {
			is.Labels = append(is.Labels, l.Name)
		}
		for _, a := range n.Assignees.Nodes {
			is.Assignees = append(is.Assignees, a.Login)
		}
		var milestoneDue time.Time
		if n.Milestone != nil {
			is.Milestone = n.Milestone.Title
			if n.Milestone.DueOn != nil {
				milestoneDue = *n.Milestone.DueOn
			}
		}
		var dates []ProjectDate
		for _, item := range n.ProjectItems.Nodes {
			for _, v := range item.FieldValues.Nodes {
				if v.Date != "" {
					dates = append(dates, ProjectDate{Field: v.Field.Name, Date: v.Date})
				}
			}
		}
		is.Due, is.DueSource = ResolveDue(dates, milestoneDue, n.Body)
		issues = append(issues, is)
	}
	var prs []PR
	for _, n := range data.Repository.PullRequests.Nodes {
		pr := PR{
			Repo: repo, Number: n.Number, Title: n.Title, URL: n.URL, Updated: n.UpdatedAt,
			Draft: n.IsDraft, Branch: n.HeadRefName, Review: ReviewState(strings.ToLower(n.ReviewDecision)),
		}
		if n.Author != nil {
			pr.Author = n.Author.Login
		}
		if len(n.Commits.Nodes) > 0 && n.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
			pr.Checks = checkState(n.Commits.Nodes[0].Commit.StatusCheckRollup.State)
		}
		prs = append(prs, pr)
	}
	if partial != nil {
		return issues, prs, fmt.Errorf("%s issues: %w", repo, partial)
	}
	return issues, prs, nil
}

func checkState(s string) CheckState {
	switch s {
	case "SUCCESS":
		return ChecksPassing
	case "FAILURE", "ERROR":
		return ChecksFailing
	case "PENDING", "EXPECTED":
		return ChecksPending
	}
	return ChecksNone
}

const milestonesQuery = `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    milestones(states: OPEN, first: 50, orderBy: {field: DUE_DATE, direction: ASC}) {
      nodes {
        number title url dueOn
        open: issues(states: OPEN) { totalCount }
        closed: issues(states: CLOSED) { totalCount }
      }
    }
  }
}`

type gqlMilestones struct {
	Repository *struct {
		Milestones struct {
			Nodes []struct {
				Number int        `json:"number"`
				Title  string     `json:"title"`
				URL    string     `json:"url"`
				DueOn  *time.Time `json:"dueOn"`
				Open   struct {
					TotalCount int `json:"totalCount"`
				} `json:"open"`
				Closed struct {
					TotalCount int `json:"totalCount"`
				} `json:"closed"`
			} `json:"nodes"`
		} `json:"milestones"`
	} `json:"repository"`
}

// Milestones returns repo's open milestones with their open and closed
// issue counts.
func (c *Client) Milestones(ctx context.Context, repo string) ([]Milestone, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	var data gqlMilestones
	err = c.graphql(ctx, milestonesQuery, map[string]string{"owner": owner, "name": name}, &data)
	var partial *PartialError
	if err != nil && !errors.As(err, &partial) {
		return nil, fmt.Errorf("%s milestones: %w", repo, err)
	}
	if data.Repository == nil {
		return nil, fmt.Errorf("%s milestones: repository not found", repo)
	}
	var out []Milestone
	for _, n := range data.Repository.Milestones.Nodes {
		m := Milestone{Repo: repo, Number: n.Number, Title: n.Title, URL: n.URL, Open: n.Open.TotalCount, Closed: n.Closed.TotalCount}
		if n.DueOn != nil {
			m.Due = dateOnly(*n.DueOn)
		}
		out = append(out, m)
	}
	if partial != nil {
		return out, fmt.Errorf("%s milestones: %w", repo, partial)
	}
	return out, nil
}

type restRuns struct {
	WorkflowRuns []struct {
		ID           int64     `json:"id"`
		Name         string    `json:"name"`
		HeadBranch   string    `json:"head_branch"`
		Status       string    `json:"status"`
		Conclusion   string    `json:"conclusion"`
		RunAttempt   int       `json:"run_attempt"`
		RunStartedAt time.Time `json:"run_started_at"`
		UpdatedAt    time.Time `json:"updated_at"`
		HTMLURL      string    `json:"html_url"`
	} `json:"workflow_runs"`
}

// Runs returns repo's most recent Actions runs, newest first. Failing jobs
// and steps aren't filled in; see FailedJobs.
func (c *Client) Runs(ctx context.Context, repo string) ([]Run, error) {
	if _, _, err := splitRepo(repo); err != nil {
		return nil, err
	}
	body, err := c.get(ctx, fmt.Sprintf("repos/%s/actions/runs?per_page=%d", repo, runPage))
	if err != nil {
		return nil, fmt.Errorf("%s runs: %w", repo, err)
	}
	var data restRuns
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("%s runs: %w", repo, err)
	}
	var out []Run
	for _, r := range data.WorkflowRuns {
		run := Run{
			Repo: repo, ID: r.ID, Attempt: r.RunAttempt, Workflow: r.Name, Branch: r.HeadBranch,
			Status: r.Status, Conclusion: r.Conclusion, Started: r.RunStartedAt, URL: r.HTMLURL,
		}
		// The runs API has no completion time; a completed run's last
		// update is when it finished.
		if r.Status == "completed" {
			run.Finished = r.UpdatedAt
		}
		out = append(out, run)
	}
	return out, nil
}

// Job is one job of a run, with its steps in order.
type Job struct {
	Name       string
	Conclusion string
	Steps      []Step
}

// Step is one step of a job.
type Step struct {
	Name       string
	Conclusion string
}

// Jobs returns the jobs of one attempt of a run.
func (c *Client) Jobs(ctx context.Context, repo string, runID int64, attempt int) ([]Job, error) {
	if _, _, err := splitRepo(repo); err != nil {
		return nil, err
	}
	if attempt < 1 {
		attempt = 1
	}
	body, err := c.get(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=100", repo, runID, attempt))
	if err != nil {
		return nil, fmt.Errorf("%s run %d jobs: %w", repo, runID, err)
	}
	var data struct {
		Jobs []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			Steps      []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("%s run %d jobs: %w", repo, runID, err)
	}
	out := make([]Job, 0, len(data.Jobs))
	for _, j := range data.Jobs {
		job := Job{Name: j.Name, Conclusion: j.Conclusion}
		for _, s := range j.Steps {
			job.Steps = append(job.Steps, Step{Name: s.Name, Conclusion: s.Conclusion})
		}
		out = append(out, job)
	}
	return out, nil
}

// failedConclusions are the job and step conclusions that count as failing.
var failedConclusions = map[string]bool{"failure": true, "timed_out": true}

// FailingStep names the first failing job and its first failing step, for
// the focus pane. step is empty when the job failed outside any step (a
// timeout or a runner that never started); both are empty when no job
// failed.
func FailingStep(jobs []Job) (job, step string) {
	for _, j := range jobs {
		if !failedConclusions[j.Conclusion] {
			continue
		}
		for _, s := range j.Steps {
			if failedConclusions[s.Conclusion] {
				return j.Name, s.Name
			}
		}
		return j.Name, ""
	}
	return "", ""
}
