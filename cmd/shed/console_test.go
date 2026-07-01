package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/anmitsu/go-shlex"

	"github.com/charliek/shed/internal/config"
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

// TestValidateAndQuoteArgs verifies that validateAndQuoteArgs single-quotes
// non-empty argv but rejects empty elements (which posix shlex drops, silently
// shifting argv on the server side).
func TestValidateAndQuoteArgs(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		got, err := validateAndQuoteArgs([]string{"bash", "-c", "echo hi"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{`'bash'`, `'-c'`, `'echo hi'`}
		if len(got) != len(want) {
			t.Fatalf("length = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("rejects empty at index 0", func(t *testing.T) {
		_, err := validateAndQuoteArgs([]string{"", "hello"})
		if err == nil {
			t.Fatal("expected error for empty argv[0], got nil")
		}
		if !strings.Contains(err.Error(), "argv[0] is empty") {
			t.Errorf("error = %q, want it to mention argv[0]", err.Error())
		}
	})

	t.Run("rejects empty in middle", func(t *testing.T) {
		_, err := validateAndQuoteArgs([]string{"echo", "", "hello"})
		if err == nil {
			t.Fatal("expected error for empty argv[1], got nil")
		}
		if !strings.Contains(err.Error(), "argv[1] is empty") {
			t.Errorf("error = %q, want it to mention argv[1]", err.Error())
		}
	})

	t.Run("empty slice is fine (caller's len check guards entry)", func(t *testing.T) {
		got, err := validateAndQuoteArgs(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d args, want 0", len(got))
		}
	})
}

// TestValidateAndQuoteArgsNULRejection verifies that argv elements containing
// a NUL byte are rejected with a clear error. NUL is the one byte that
// single-quote wrapping can't safely round-trip: Go's os/exec rejects it with
// a confusing error, and the server-side `bash -lc` reparse can't carry it
// either. Reject early at the CLI so the user sees the failure pointed at the
// offending argv index.
func TestValidateAndQuoteArgsNULRejection(t *testing.T) {
	t.Run("NUL in argv[0]", func(t *testing.T) {
		_, err := validateAndQuoteArgs([]string{"echo\x00", "hello"})
		if err == nil {
			t.Fatal("expected error for NUL in argv[0], got nil")
		}
		if !strings.Contains(err.Error(), "argv[0]") || !strings.Contains(err.Error(), "NUL") {
			t.Errorf("error = %q, want it to mention argv[0] and NUL", err.Error())
		}
	})

	t.Run("NUL in middle argv", func(t *testing.T) {
		_, err := validateAndQuoteArgs([]string{"echo", "a\x00b", "hello"})
		if err == nil {
			t.Fatal("expected error for NUL in argv[1], got nil")
		}
		if !strings.Contains(err.Error(), "argv[1]") || !strings.Contains(err.Error(), "NUL") {
			t.Errorf("error = %q, want it to mention argv[1] and NUL", err.Error())
		}
	})

	t.Run("NUL at end of argv", func(t *testing.T) {
		_, err := validateAndQuoteArgs([]string{"echo", "hello", "\x00"})
		if err == nil {
			t.Fatal("expected error for NUL in last argv, got nil")
		}
		if !strings.Contains(err.Error(), "argv[2]") || !strings.Contains(err.Error(), "NUL") {
			t.Errorf("error = %q, want it to mention argv[2] and NUL", err.Error())
		}
	})
}

// TestShellQuoteBashRoundTrip is the load-bearing security audit: it proves
// that single-quote-wrapped argv survives a real bash reparse with argv
// preserved literally, even when individual elements contain shell
// metacharacters (`$(…)`, backticks, `${…}`, semicolons, pipes, mixed
// quotes, backslashes, newlines, UTF-8).
//
// The PR-D contract: the server-side `bash -lc <raw>` wrap interprets shell
// metacharacters in raw SSH commands, but bash treats single-quoted text as
// LITERAL data — so `shed exec`'s single-quote-wrapped argv stays argv after
// bash sees it. If that invariant ever broke, raw SSH could no longer be
// trusted to wrap with bash safely.
//
// This test invokes the real /bin/bash (skips cleanly when bash isn't on
// PATH) using a NUL-separated printf marker pattern, then splits stdout on
// NUL to recover argv. Each case below is a string bash would normally
// expand or interpret if not single-quoted; the assertion is that the
// recovered string equals the input.
func TestShellQuoteBashRoundTrip(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH; skipping round-trip test")
	}

	cases := []struct {
		name string
		argv []string
	}{
		{"command substitution literal", []string{"echo", "$(rm -rf /)"}},
		{"backtick literal", []string{"echo", "`whoami`"}},
		{"parameter expansion literal", []string{"echo", "${HOME}"}},
		{"mixed quotes", []string{"echo", `it's a "test"`}},
		{"backslash before newline (line continuation)", []string{"echo", "a\\\nb"}},
		{"embedded newline", []string{"echo", "a\nb"}},
		{"UTF-8", []string{"echo", "héllo 🎉"}},
		{"semicolons and pipes", []string{"echo", "a;b|c"}},
		{"dollar var literal", []string{"echo", "$HOME"}},
		{"plain ASCII", []string{"echo", "hello"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quoted, err := validateAndQuoteArgs(tc.argv)
			if err != nil {
				t.Fatalf("validateAndQuoteArgs(%#v): %v", tc.argv, err)
			}

			// Construct a bash script that takes the quoted argv as a
			// space-joined string of literal-single-quoted tokens, then
			// runs `for a in <quoted-tokens>; do printf '%s\0' "$a"; done`
			// so we get a NUL-separated stream of literal argv elements
			// from bash's own parser. This is the exact path `bash -lc`
			// will take server-side: parse the joined string as a shell
			// command, with single-quoted tokens preserved as literal
			// data.
			joined := strings.Join(quoted, " ")
			script := "for a in " + joined + "; do printf '%s\\0' \"$a\"; done"

			cmd := exec.Command(bash, "-c", script)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("bash -c failed: %v\nscript: %s\nstderr: %s", err, script, stderr.String())
			}

			out := stdout.Bytes()
			// printf '%s\0' emits a trailing NUL after each element; the
			// final byte is NUL, so split and drop the empty trailing
			// element rather than counting NULs.
			if len(out) == 0 || out[len(out)-1] != 0 {
				t.Fatalf("expected NUL-terminated stream; got %q", out)
			}
			parts := bytes.Split(out[:len(out)-1], []byte{0})
			recovered := make([]string, 0, len(parts))
			for _, p := range parts {
				recovered = append(recovered, string(p))
			}

			if len(recovered) != len(tc.argv) {
				t.Fatalf("recovered %d argv, want %d\n  argv:      %#v\n  quoted:    %s\n  recovered: %#v\n  stderr:    %s",
					len(recovered), len(tc.argv), tc.argv, joined, recovered, stderr.String())
			}
			for i := range tc.argv {
				if recovered[i] != tc.argv[i] {
					t.Errorf("argv[%d] = %q, want %q (joined=%q)",
						i, recovered[i], tc.argv[i], joined)
				}
			}
		})
	}
}

// TestTTYFlag pins the PTY-flag decision: an interactive shell (no command)
// always requests a PTY; a command exec requests one only when stdin AND stdout
// are terminals, so captured/piped output stays 8-bit-clean (a remote PTY's
// ONLCR line discipline corrupts binary output).
func TestTTYFlag(t *testing.T) {
	tests := []struct {
		name                            string
		hasCommand, stdinTTY, stdoutTTY bool
		want                            string
	}{
		{"interactive, both tty", false, true, true, "-t"},
		{"interactive, stdout redirected", false, true, false, "-t"},
		{"interactive, stdin piped", false, false, true, "-t"},
		{"interactive, neither tty", false, false, false, "-t"},
		{"exec, both tty (vim)", true, true, true, "-t"},
		{"exec, stdout redirected (cat bin > f)", true, true, false, "-T"},
		{"exec, stdin piped (echo | cmd)", true, false, true, "-T"},
		{"exec, neither tty (scripted)", true, false, false, "-T"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ttyFlag(tt.hasCommand, tt.stdinTTY, tt.stdoutTTY); got != tt.want {
				t.Errorf("ttyFlag(hasCommand=%v, stdinTTY=%v, stdoutTTY=%v) = %q, want %q",
					tt.hasCommand, tt.stdinTTY, tt.stdoutTTY, got, tt.want)
			}
		})
	}
}

// TestSSHSessionArgs covers the call-site wiring that ttyFlag alone can't: the
// flag lands immediately after "ssh", the shared baseSSHArgs connection options
// are reused (so they can't drift), and the quoted command is appended after the
// host. The real syscall.Exec that consumes this argv is untestable.
func TestSSHSessionArgs(t *testing.T) {
	entry := &config.ServerEntry{Host: "example.com", SSHPort: 2222}

	t.Run("exec places flag after ssh and appends all command args in order", func(t *testing.T) {
		// A multi-element quoted command: it must be appended verbatim, in order,
		// after the host — no reordering and no "--" inserted.
		cmd := []string{"'bash'", "'-c'", "'echo hi'"}
		got := sshSessionArgs("-T", "box", entry, cmd)
		if len(got) < 3 || got[0] != "ssh" || got[1] != "-T" {
			t.Fatalf("expected argv to start with [ssh -T ...], got %v", got)
		}
		joined := strings.Join(got, " ")
		for _, want := range []string{"-p 2222", "UserKnownHostsFile=", "StrictHostKeyChecking=yes", "box@example.com"} {
			if !strings.Contains(joined, want) {
				t.Errorf("argv missing shared option %q: %v", want, got)
			}
		}
		// The command occupies exactly the final len(cmd) elements, preceded by the host.
		tail := got[len(got)-len(cmd):]
		for i := range cmd {
			if tail[i] != cmd[i] {
				t.Errorf("command arg[%d] = %q, want %q (full argv %v)", i, tail[i], cmd[i], got)
			}
		}
		if got[len(got)-len(cmd)-1] != "box@example.com" {
			t.Errorf("host must immediately precede the command args: %v", got)
		}
	})

	t.Run("interactive omits the command and ends at the host", func(t *testing.T) {
		got := sshSessionArgs("-t", "box", entry, nil)
		if got[0] != "ssh" || got[1] != "-t" {
			t.Fatalf("expected [ssh -t ...], got %v", got)
		}
		if got[len(got)-1] != "box@example.com" {
			t.Errorf("interactive argv should end at the host, got %v", got)
		}
	})
}
