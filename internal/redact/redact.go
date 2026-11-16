// Package redact removes secrets from text the engine keeps about workspaces
// (the activity log), on a best-effort basis: well-known token shapes and
// values that are named as secrets (TOKEN=…, --password …, Authorization: …,
// https://user:pass@…). It cannot find a secret that looks like anything
// else, so text it has passed is still treated as workspace contents.
package redact

import (
	"regexp"
	"strings"
)

// Mark replaces each secret.
const Mark = "[redacted]"

// secretName matches names that say they hold a secret.
const secretName = `[A-Za-z0-9_.-]*(?:TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|API[_-]?KEY|ACCESS[_-]?KEY|PRIVATE[_-]?KEY|CREDENTIALS?|AUTH[_-]?KEY|CLIENT[_-]?SECRET)[A-Za-z0-9_.-]*`

// value is what follows a secret's name: quoted, or up to a separator. Text
// already redacted matches as itself, so redacting twice changes nothing.
const value = `(?:\[redacted\]|"[^"\n]*"|'[^'\n]*'|[^\s"'&;|,)}\][][^\s"'&;|,)}\]]*)`

var rules = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private keys, whole (a key cut off by a tail still loses the rest)
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`), Mark},
	// Authorization headers and bearer tokens
	{regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?(?:(?:bearer|basic|token|digest)\s+)?)` + value), "${1}" + Mark},
	{regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]{8,}`), "${1}" + Mark},
	// passwords in URLs: scheme://user:password@host
	{regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^/\s:@'"]+:)[^/\s@'"]+@`), "${1}" + Mark + "@"},
	// --token VALUE, --password=VALUE, -p…? (only named flags)
	{regexp.MustCompile(`(?i)(--?` + secretName + `)(=|\s+)` + value), "${1}${2}" + Mark},
	// TOKEN=VALUE, "api_key": "VALUE", password: VALUE
	{regexp.MustCompile(`(?i)(\b` + secretName + `["']?\s*[:=]\s*)` + value), "${1}" + Mark},
	// well-known token shapes
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), Mark},                                // AWS access key IDs
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), Mark},                                 // GitHub tokens
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`), Mark},                               // GitHub fine-grained tokens
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`), Mark},                                   // GitLab tokens
	{regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`), Mark},                              // Slack tokens
	{regexp.MustCompile(`\bsk-(?:ant-|proj-|live-|test-)?[A-Za-z0-9_-]{20,}`), Mark},           // OpenAI, Anthropic, Stripe keys
	{regexp.MustCompile(`\b[rp]k_(?:live|test)_[A-Za-z0-9]{16,}`), Mark},                       // Stripe restricted/publishable
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`), Mark},                                      // Google API keys
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), Mark}, // JWTs
}

// String returns s with the secrets it can recognise replaced by Mark.
func String(s string) string {
	if s == "" {
		return s
	}
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// flagName is a flag that names a secret and takes its value as the next
// argument: --token VALUE.
var flagName = regexp.MustCompile(`(?i)^--?` + secretName + `$`)

// Args redacts a command's arguments one by one, plus the argument after a
// flag that names a secret (["curl", "--token", "abc"]), which String cannot
// see when the arguments are kept apart.
func Args(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if i > 0 && flagName.MatchString(argv[i-1]) && !strings.HasPrefix(a, "-") {
			out[i] = Mark
			continue
		}
		out[i] = String(a)
	}
	return out
}

// Any redacts every string in v: strings, string slices and the values of
// maps and slices, recursively. Other values are returned as they are.
func Any(v any) any {
	switch x := v.(type) {
	case string:
		return String(x)
	case []string:
		return Args(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Any(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = Any(e)
		}
		return out
	}
	return v
}
