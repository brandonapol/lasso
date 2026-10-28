package rules

import (
	"slices"
	"testing"
)

func TestCommandEffects(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want []Effect
	}{
		{"go test ./...", nil},
		{"git push origin feature", nil},
		{"git push --force origin feature", []Effect{ForcePush}},
		{"git push -uf origin feature", []Effect{ForcePush}},
		{"git push --force-with-lease=main origin main", []Effect{ForcePush}},
		{"cd /tmp && git -C repo push origin +main", []Effect{ForcePush}},
		{"/usr/bin/git push origin :old-branch", []Effect{DeleteBranch}},
		{"git push origin --delete old", []Effect{DeleteBranch}},
		{"git branch -D old; git push -f", []Effect{DeleteBranch, ForcePush}},
		{"git branch -a", nil},
		{"gh pr merge 12 --squash", []Effect{MergeDefault}},
		{"gh -R o/r pr merge 12", []Effect{MergeDefault}},
		{"gh pr view 12", nil},
		{"git log | grep -f patterns", nil},
	} {
		if got := CommandEffects(tc.cmd); !slices.Equal(got, tc.want) {
			t.Errorf("CommandEffects(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}
