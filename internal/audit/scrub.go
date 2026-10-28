package audit

import (
	"regexp"
	"strings"
)

// Scrubbed replaces a value Scrub hides.
const Scrubbed = "[scrubbed]"

// maxArg clips long values: the log says what happened, it isn't a copy of
// every comment body.
const maxArg = 500

// secretKey matches argument names whose values are always hidden.
var secretKey = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[_-]?key|private[_-]?key|credential|cookie|authorization|session)`)

// secretValue matches well-known token shapes wherever they appear in a
// value: GitHub, Anthropic/OpenAI-style, Slack and AWS keys, bearer headers,
// PEM blocks.
var secretValue = regexp.MustCompile(`(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{16,}|xox[abprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|(?i:bearer)\s+[A-Za-z0-9._~+/=-]{16,}|-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$))`)

// Scrub returns a copy of args with secret-named keys hidden, token-shaped
// substrings replaced, and long values clipped.
func Scrub(args map[string]string) map[string]string {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]string, len(args))
	for k, v := range args {
		if secretKey.MatchString(k) {
			out[k] = Scrubbed
			continue
		}
		v = secretValue.ReplaceAllString(v, Scrubbed)
		if len(v) > maxArg {
			v = strings.ToValidUTF8(v[:maxArg], "") + "…"
		}
		out[k] = v
	}
	return out
}
