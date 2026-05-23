package main

import (
	"strings"
	"testing"

	"github.com/anmitsu/go-shlex"
)

// TestShellQuoteRoundTrip verifies that every element of argv survives a
// round trip through `strings.Join` + `shlex.Split` after being shell-quoted.
// This is the same path that `shed exec` argv takes: the ssh client joins
// quoted argv with spaces into a single SSH command string, and the
// gliderlabs/ssh server uses shlex.Split to recover argv.
//
// Known transport limitation (not introduced by shellQuoteArgs): the
// posix-mode anmitsu/go-shlex used by gliderlabs/ssh drops empty tokens, so
// passing an empty-string argv element through `shed exec` is not possible
// regardless of quoting. Empty-arg shellQuoteArg output is still validated
// in TestShellQuoteArgFormat below.
func TestShellQuoteRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{"simple", []string{"echo", "hello"}},
		{"pipe inside arg", []string{"bash", "-c", "echo hello | wc -c"}},
		{"redirect", []string{"bash", "-c", "echo hello > /tmp/t"}},
		{"semicolon", []string{"bash", "-c", "echo hello; echo world"}},
		{"nested double quotes", []string{"bash", "-c", `echo "outer \"inner\" outer"`}},
		{"nested single quote", []string{"bash", "-c", "echo it's working"}},
		{"parens", []string{"bash", "-c", "bun -e \"console.log(1+1)\""}},
		{"url with pipe", []string{"bash", "-c", "curl -fsSL https://bun.sh/install | bash"}},
		{"dollar var", []string{"bash", "-c", "echo $HOSTNAME"}},
		{"backticks", []string{"bash", "-c", "echo `date`"}},
		{"backslash", []string{"bash", "-c", `echo a\nb`}},
		{"spaces inside arg", []string{"echo", "hello world"}},
		{"multiple internal singles", []string{"bash", "-c", "x='1' y='2'"}},
		{"all metacharacters", []string{"bash", "-c", "echo $a | tee /tmp/x && echo 'done' > /tmp/y"}},
		{"already quoted", []string{"bash", "-c", "'literal-single-quoted'"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quoted := shellQuoteArgs(tc.argv)
			if len(quoted) != len(tc.argv) {
				t.Fatalf("shellQuoteArgs length mismatch: got %d, want %d", len(quoted), len(tc.argv))
			}

			joined := strings.Join(quoted, " ")
			// Mirror gliderlabs/ssh's session.go:
			//   cmd, _ := shlex.Split(sess.rawCmd, true)
			// Posix mode = true.
			recovered, err := shlex.Split(joined, true)
			if err != nil {
				t.Fatalf("shlex.Split(%q) error: %v", joined, err)
			}

			if len(recovered) != len(tc.argv) {
				t.Fatalf("recovered length = %d, want %d\n  joined: %s\n  argv:   %#v\n  got:    %#v", len(recovered), len(tc.argv), joined, tc.argv, recovered)
			}
			for i := range tc.argv {
				if recovered[i] != tc.argv[i] {
					t.Errorf("argv[%d] = %q, want %q\n  joined: %s", i, recovered[i], tc.argv[i], joined)
				}
			}
		})
	}
}

// TestShellQuoteArgFormat sanity-checks the single-quote wrapping format.
func TestShellQuoteArgFormat(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"hello", `'hello'`},
		{"", `''`},
		{"a b", `'a b'`},
		{"it's", `'it'\''s'`},
		{`he said "hi"`, `'he said "hi"'`},
		{"pipe|here", `'pipe|here'`},
	}
	for _, tc := range cases {
		got := shellQuoteArg(tc.in)
		if got != tc.want {
			t.Errorf("shellQuoteArg(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
