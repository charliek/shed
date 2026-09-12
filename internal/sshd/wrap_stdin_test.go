package sshd

import (
	"bytes"
	"errors"
	"os/exec"
	"testing"
)

// TestWrapCommandStdinExitCodePropagates pins the far-side shape every plan
// 019 roost bootstrap script depends on (§3.4): roost's own script builders
// (`stream_command`, `path_check_command`, `exec_chain_command`, and every
// install-order script) all run as `/bin/sh -s`, with the script bytes
// arriving on stdin. Over shed's own sshd, the raw SSH command string is
// wrapped as `bash -lc <raw>` (wrapCommand) before it ever reaches a shell —
// so a bootstrap script sent as the command `/bin/sh -s` actually executes as
// `bash -lc '/bin/sh -s'`, with bash itself never touching the script text on
// stdin, only handing the stdin descriptor through to the inner `sh`.
//
// This test proves that wrap does not swallow or remap the inner `sh -s`
// process's exit code: `bash -lc '/bin/sh -s'` must exit with whatever the
// piped script itself exits with. Every bootstrap machine (ProbeMachine,
// InstallMachine, crates/shed-core/src/roost/bootstrap) treats that exit code
// as the `Step::Outcome`'s `exit` field and classifies success/failure on it,
// so a wrap that lost or remapped the code would silently corrupt every
// probe and install outcome without any single golden catching it — this is
// the one place the shape is pinned directly against a real shell.
func TestWrapCommandStdinExitCodePropagates(t *testing.T) {
	argv := wrapCommand("/bin/sh -s")
	wantArgv := []string{"bash", "-lc", "/bin/sh -s"}
	if len(argv) != len(wantArgv) {
		t.Fatalf("wrapCommand(%q) = %#v, want %#v", "/bin/sh -s", argv, wantArgv)
	}
	for i := range wantArgv {
		if argv[i] != wantArgv[i] {
			t.Fatalf("wrapCommand(%q) = %#v, want %#v", "/bin/sh -s", argv, wantArgv)
		}
	}

	cases := []struct {
		name   string
		script string
		want   int
	}{
		{
			name:   "clean success",
			script: "exit 0",
			want:   0,
		},
		{
			name:   "an explicit nonzero exit propagates",
			script: "exit 42",
			want:   42,
		},
		{
			name:   "a failed command's implicit exit propagates",
			script: "false",
			want:   1,
		},
		{
			name:   "the script's LAST command's status wins, not its first",
			script: "true; false; exit 7",
			want:   7,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			//nolint:gosec // argv is the fixed, package-level wrapCommand output; script is a test literal, not user input.
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin = bytes.NewReader([]byte(tc.script))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr

			err := cmd.Run()
			got := exitCodeOf(t, err)
			if got != tc.want {
				t.Fatalf("script %q via %v: exit code = %d, want %d (stderr: %s)",
					tc.script, argv, got, tc.want, stderr.String())
			}
		})
	}
}

// exitCodeOf extracts a process's exit code from exec.Cmd.Run's error, or
// fails the test on any other kind of failure (the shell/sh binaries
// themselves not being found, for instance — a real error this test should
// not paper over).
func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	t.Fatalf("unexpected error running command (not an exit-code failure): %v", err)
	return -1
}
