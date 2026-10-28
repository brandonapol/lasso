// Package github is lasso's read-only GitHub source (ADR 0001 §3).
//
// It polls each hot repo through the gh CLI: GraphQL for issues, PRs and
// milestones, REST with ETags for Actions runs and their jobs. Nothing in
// this package writes to GitHub.
package github

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// GH is the GitHub CLI. Only this file starts it (see TestNoDirectGHExec).
const GH = "gh"

// Runner runs gh with args and returns its stdout. On failure it still
// returns what gh printed to stdout, since `gh api -i` prints the response
// headers of a 304 and then exits non-zero.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// CLI is the Runner that starts the real gh, in Dir when set. It is the one
// place lasso starts gh.
type CLI struct {
	Dir string
}

// Run runs gh with args.
func (c CLI) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, GH, args...)
	cmd.Dir = c.Dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("gh %s: %w: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}
