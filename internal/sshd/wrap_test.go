package sshd

import (
	"reflect"
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
