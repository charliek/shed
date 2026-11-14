package sshd

import (
	"github.com/charliek/shed/internal/redact"
	"reflect"
	"strings"
	"testing"
)

// TestWrapCommand locks the contract `wrapCommand` exposes: empty raw
// returns nil (agent boots into its default `{bash, --login}`), every
// non-empty raw is fed verbatim to `bash -lc` with no transformation,
// because every shell-metacharacter interpretation belongs to bash.
//
// What this test *does not* exercise: bash's own parse of the resulting
// `-lc` argument. That happens guest-side and is covered by the
// integration suite (tests/integration/test_exec_shell.py).
func TestWrapCommand(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "empty raw → nil (agent default login shell fires)",
			raw:  "",
			want: nil,
		},
		{
			name: "plain command preserved verbatim",
			raw:  "echo hi",
			want: []string{"bash", "-lc", "echo hi"},
		},
		{
			name: "dollar-var preserved literally for bash to expand",
			raw:  "echo $HOME",
			want: []string{"bash", "-lc", "echo $HOME"},
		},
		{
			name: "embedded single quote preserved",
			raw:  `echo 'it'\''s'`,
			want: []string{"bash", "-lc", `echo 'it'\''s'`},
		},
		{
			name: "leading and trailing whitespace preserved",
			raw:  "   echo hi   ",
			want: []string{"bash", "-lc", "   echo hi   "},
		},
		{
			name: "embedded newline preserved",
			raw:  "echo a\necho b",
			want: []string{"bash", "-lc", "echo a\necho b"},
		},
		{
			name: "command substitution preserved",
			raw:  "echo $(hostname)",
			want: []string{"bash", "-lc", "echo $(hostname)"},
		},
		{
			name: "backtick substitution preserved",
			raw:  "echo `hostname`",
			want: []string{"bash", "-lc", "echo `hostname`"},
		},
		{
			name: "parameter expansion preserved",
			raw:  "echo ${FOO:-default}",
			want: []string{"bash", "-lc", "echo ${FOO:-default}"},
		},
		{
			name: "literal NUL passes through wrap (CLI quoter rejects upstream)",
			raw:  "echo \x00",
			want: []string{"bash", "-lc", "echo \x00"},
		},
		{
			name: "pipes and semicolons preserved",
			raw:  "a; b | c && d",
			want: []string{"bash", "-lc", "a; b | c && d"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapCommand(tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("wrapCommand(%q) = %#v, want %#v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestWrappedCommandRedacts: the "Executing in shed" log line prints
// redact.Args of the wrapped command. The command is one string the shed CLI
// built by single-quoting each argv element, so an apostrophe in a URL
// password arrives as '\”; the password must not reach the log either way.
func TestWrappedCommandRedacts(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"plain password", `'git' 'clone' 'https://user:s3cr3t-token@git.example/o/r.git'`},
		{"apostrophe in the password", `'git' 'clone' 'https://user:s3cr3t'\''token@git.example/o/r.git'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logged := strings.Join(redact.Args(wrapCommand(tt.raw)), " ")
			if strings.Contains(logged, "s3cr3t") || strings.Contains(logged, "token") {
				t.Errorf("the logged command carries the password: %s", logged)
			}
			if !strings.Contains(logged, "https://REDACTED@git.example/o/r.git") {
				t.Errorf("the logged command lost the URL: %s", logged)
			}
		})
	}
}
