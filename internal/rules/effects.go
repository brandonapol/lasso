package rules

import (
	"path"
	"slices"
	"strings"
)

// CommandEffects are the always-ask effects a shell command has, read from
// the command itself (an adapter's record of the call a bot wants to run),
// never from the bot's description of it. It reads git and gh the way a
// careful person would skim them: a force push, a branch deleted, a pull
// request merged. It errs towards finding an effect: a false hit only
// means the user is asked.
func CommandEffects(cmd string) []Effect {
	var out []Effect
	add := func(e Effect) {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	for _, seg := range segments(cmd) {
		f := strings.Fields(seg)
		for i, w := range f {
			switch path.Base(strings.Trim(w, `"'`)) {
			case "git":
				gitEffects(f[i+1:], add)
			case "gh":
				if sub := args(f[i+1:]); len(sub) >= 2 && sub[0] == "pr" && sub[1] == "merge" {
					// gh merges into the pull request's base: the main
					// branch, as often as not.
					add(MergeDefault)
				}
			}
		}
	}
	return out
}

// segments splits a command line at its separators (;, &&, ||, |, and new
// lines), so each part is read on its own.
func segments(cmd string) []string {
	return strings.FieldsFunc(cmd, func(r rune) bool { return r == ';' || r == '&' || r == '|' || r == '\n' })
}

// args drops leading options and their values ("-C dir", "-c k=v") from a
// git or gh command, leaving the subcommand and what follows.
func args(f []string) []string {
	for len(f) > 0 && strings.HasPrefix(f[0], "-") {
		if (f[0] == "-C" || f[0] == "-c" || f[0] == "-R" || f[0] == "--repo") && len(f) > 1 {
			f = f[1:]
		}
		f = f[1:]
	}
	return f
}

func gitEffects(f []string, add func(Effect)) {
	f = args(f)
	if len(f) == 0 {
		return
	}
	sub, rest := f[0], f[1:]
	switch sub {
	case "push":
		for _, a := range rest {
			switch {
			case a == "--force", a == "--force-with-lease", a == "--force-if-includes", a == "--mirror",
				strings.HasPrefix(a, "--force-with-lease="),
				shortFlag(a, 'f'):
				add(ForcePush)
			case a == "--delete", shortFlag(a, 'd'):
				add(DeleteBranch)
			case strings.HasPrefix(a, "+"):
				add(ForcePush) // +refspec forces that ref
			case strings.HasPrefix(a, ":") && len(a) > 1:
				add(DeleteBranch) // :branch deletes it on the remote
			}
		}
	case "branch":
		for _, a := range rest {
			if a == "--delete" || shortFlag(a, 'd') || shortFlag(a, 'D') {
				add(DeleteBranch)
			}
		}
	}
}

// shortFlag reports whether a is a bundle of short flags ("-uf") holding c.
func shortFlag(a string, c rune) bool {
	if len(a) < 2 || a[0] != '-' || a[1] == '-' {
		return false
	}
	return strings.ContainsRune(a[1:], c)
}
