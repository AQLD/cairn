package redact

import (
	"reflect"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// named values
		{"GITHUB_TOKEN=abc123 make deploy", "GITHUB_TOKEN=[redacted] make deploy"},
		{"export AWS_SECRET_ACCESS_KEY='s3cr3t/val'", "export AWS_SECRET_ACCESS_KEY=[redacted]"},
		{`db_password: "hunter2"`, `db_password: [redacted]`},
		{`{"api_key": "k-123", "user": "bob"}`, `{"api_key": [redacted], "user": "bob"}`},
		{"OPENAI_API_KEY=xyz; echo ok", "OPENAI_API_KEY=[redacted]; echo ok"},
		{"client_secret=abc&scope=read", "client_secret=[redacted]&scope=read"},
		// flags
		{"mysql --password=hunter2 -u root", "mysql --password=[redacted] -u root"},
		{"curl --token abc123 https://x", "curl --token [redacted] https://x"},
		{"tool --api-key k_9 run", "tool --api-key [redacted] run"},
		// headers and bearer tokens
		{`curl -H "Authorization: Bearer abc.def.ghi" x`, `curl -H "Authorization: Bearer [redacted]" x`},
		{"authorization=Basic dXNlcjpwYXNz", "authorization=Basic [redacted]"},
		{"got bearer abcdefghijkl back", "got bearer [redacted] back"},
		// URLs with a password
		{"git clone https://bob:pa55w0rd@github.com/o/r.git", "git clone https://bob:[redacted]@github.com/o/r.git"},
		{"postgres://app:s3cret@db:5432/x", "postgres://app:[redacted]@db:5432/x"},
		// token shapes, wherever they appear
		{"key AKIAIOSFODNN7EXAMPLE here", "key [redacted] here"},
		{"ghp_" + strings.Repeat("a", 36), "[redacted]"},
		{"x github_pat_" + strings.Repeat("B", 40), "x [redacted]"},
		{"glpat-" + strings.Repeat("c", 20), "[redacted]"},
		{"xoxb-123456789012-abcdef", "[redacted]"},
		{"sk-ant-" + strings.Repeat("d", 30), "[redacted]"},
		{"sk-proj-" + strings.Repeat("e", 30) + " used", "[redacted] used"},
		{"AIza" + strings.Repeat("f", 35), "[redacted]"},
		{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "[redacted]"},
		{"a\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\nabc\n-----END RSA PRIVATE KEY-----\nb", "a\n[redacted]\nb"},
		{"-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn (cut by the tail", "[redacted]"},
		// left alone
		{"echo hello world", "echo hello world"},
		{"pytest -q tests/", "pytest -q tests/"},
		{"https://github.com/o/r.git", "https://github.com/o/r.git"},
		{"ssh git@github.com", "ssh git@github.com"},
		{"sk-short", "sk-short"},
		{"", ""},
	} {
		if got := String(tc.in); got != tc.want {
			t.Errorf("String(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestStringIsIdempotent(t *testing.T) {
	for _, s := range []string{"TOKEN=abc", "--password=x", "Authorization: Bearer abcdefghij", "https://u:p@h/"} {
		once := String(s)
		if twice := String(once); twice != once {
			t.Errorf("String twice changed %q to %q", once, twice)
		}
	}
}

func TestArgs(t *testing.T) {
	got := Args([]string{"curl", "--token", "abc123", "-H", "X-Api-Key: k1", "--password=p", "--verbose", "https://u:p@h/x"})
	want := []string{"curl", "--token", Mark, "-H", "X-Api-Key: [redacted]", "--password=[redacted]", "--verbose", "https://u:[redacted]@h/x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Args\n got %q\nwant %q", got, want)
	}
	// a flag followed by another flag has no value to hide
	if got := Args([]string{"tool", "--token", "--verbose"}); got[2] != "--verbose" {
		t.Fatalf("flag after a secret flag: %q", got)
	}
}

func TestAny(t *testing.T) {
	in := map[string]any{
		"command":     []string{"deploy", "--api-key", "k"},
		"stdout_tail": "TOKEN=abc",
		"nested":      map[string]any{"x": []any{"PASSWORD=p", 3}},
		"exit_code":   1,
	}
	got := Any(in).(map[string]any)
	if got["stdout_tail"] != "TOKEN=[redacted]" || got["exit_code"] != 1 ||
		got["command"].([]string)[2] != Mark ||
		got["nested"].(map[string]any)["x"].([]any)[0] != "PASSWORD=[redacted]" {
		t.Fatalf("Any: %#v", got)
	}
	if in["stdout_tail"] != "TOKEN=abc" {
		t.Fatal("Any changed its input")
	}
}
