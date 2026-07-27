package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var execSessionFlag string

var consoleCmd = &cobra.Command{
	Use:   "console <name>",
	Short: "Open an interactive console to a shed",
	Long: `Open an interactive SSH console to a shed.

This command replaces the current process with an SSH connection
to the specified shed. For tmux session support, use "shed attach" instead.`,
	Args: cobra.ExactArgs(1),
	RunE: runConsole,
}

var execCmd = &cobra.Command{
	Use:   "exec <name> <command...>",
	Short: "Execute a command in a shed",
	Long: `Execute a command in a shed via SSH.

This command replaces the current process with an SSH connection
that runs the specified command.

Use --session to run the command in the context of an existing tmux session.

Examples:
  shed exec myproj git status                     # Direct execution
  shed exec myproj --session default git status   # Run in tmux session`,
	Args: cobra.MinimumNArgs(2),
	RunE: runExec,
}

func init() {
	execCmd.Flags().StringVarP(&execSessionFlag, "session", "S", "", "Run command in tmux session context")

	rootCmd.AddCommand(consoleCmd)
	rootCmd.AddCommand(execCmd)
}

func runConsole(cmd *cobra.Command, args []string) error {
	name := args[0]
	return sshToShed(name, nil)
}

func runExec(cmd *cobra.Command, args []string) error {
	name := args[0]
	command := args[1:]

	// If --session flag is provided, wrap command in tmux send-keys
	if execSessionFlag != "" {
		if err := config.ValidateSessionName(execSessionFlag); err != nil {
			return fmt.Errorf("invalid session name: %w", err)
		}
		// Use tmux send-keys to run the command in the session
		// This sends the command to the session and presses Enter
		// Escape single quotes by replacing ' with '\'' (end quote, escaped quote, start quote)
		escapedCmd := escapeForShellSingleQuote(strings.Join(command, " "))
		tmuxCmd := fmt.Sprintf("tmux send-keys -t %s '%s' Enter", execSessionFlag, escapedCmd)
		command = []string{"sh", "-c", tmuxCmd}
	}

	return sshToShed(name, command)
}

// escapeForShellSingleQuote escapes a string for safe inclusion in single quotes.
// Single quotes cannot be escaped inside single-quoted strings, so we end the
// single-quoted string, add an escaped single quote, and start a new single-quoted string.
func escapeForShellSingleQuote(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// shellQuoteArg wraps a single argv element in single quotes so it survives
// one round-trip through a shell-style command-line parser (e.g. the SSH
// server's shlex.Split). Used when handing argv to ssh, which joins remaining
// arguments with spaces into a single command string before transmission.
func shellQuoteArg(s string) string {
	return "'" + escapeForShellSingleQuote(s) + "'"
}

// shellQuoteArgs applies shellQuoteArg to each element, returning a new slice
// of the same length.
func shellQuoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuoteArg(a)
	}
	return out
}

// validateAndQuoteArgs single-quotes each argv element (so shell metacharacters
// survive the SSH wire) and rejects argv elements that the SSH/bash downstream
// can't safely round-trip.
//
// Rejected:
//   - Empty elements — the gliderlabs/ssh server uses anmitsu/go-shlex in posix
//     mode, which drops empty quoted tokens; without this guard an empty argv
//     element would silently shift the rest of argv on the server side.
//   - NUL bytes — bash and Go's os/exec both reject embedded NUL with confusing
//     errors; reject early at the CLI so the user sees a clear message. NUL is
//     also the one byte single-quote wrapping can't safely carry through SSH
//     and the server-side `bash -lc` reparse (see internal/sshd/wrap.go).
//
// The single-quote wrap itself survives the server-side `bash -lc` reparse
// because bash treats single-quoted text as literal data — that is the
// security gate that lets `shed exec` preserve argv literally while raw SSH
// gains shell semantics.
func validateAndQuoteArgs(args []string) ([]string, error) {
	for i, a := range args {
		if a == "" {
			return nil, fmt.Errorf("argv[%d] is empty; the SSH transport cannot represent empty arguments (posix shlex drops '' tokens)", i)
		}
		if strings.ContainsRune(a, 0) {
			return nil, fmt.Errorf("argv[%d] contains a NUL byte; not representable over SSH or in POSIX argv", i)
		}
	}
	return shellQuoteArgs(args), nil
}

// ttyFlag returns the ssh PTY flag for a shed session. An interactive shell
// (no command) always requests a PTY. A command exec requests one only when the
// local stdin AND stdout are both terminals, so captured or piped output stays
// 8-bit-clean — a remote PTY's ONLCR line discipline corrupts binary output
// (e.g. `shed exec box -- cat bin > f`). The AND-condition also suppresses the
// "Pseudo-terminal will not be allocated" warning a bare `-t` emits for a
// non-tty stdin. Escape hatch: raw `ssh -tt`/`ssh -T`.
func ttyFlag(hasCommand, stdinTTY, stdoutTTY bool) string {
	if hasCommand && (!stdinTTY || !stdoutTTY) {
		return "-T"
	}
	return "-t"
}

// sshSessionArgs assembles the ssh argv for a shed session: ["ssh", flag], the
// shared connection options (baseSSHArgs, reused from rc.go so options can't
// drift), then the already-quoted remote command (empty for an interactive
// shell). Kept pure so a test can assert flag placement and option reuse; the
// syscall.Exec that consumes it can't be unit-tested.
//
// Unlike rc.go's capture path, no "--" precedes the command: this preserves the
// pre-existing console/exec behavior (ssh treats everything after the
// destination as the command, and the argv is already single-quoted), so adding
// a "--" would change what the server's bash -lc receives.
func sshSessionArgs(flag, name string, entry *config.ServerEntry, quotedCmd []string) []string {
	args := append([]string{"ssh", flag}, baseSSHArgs(name, entry)...)
	return append(args, quotedCmd...)
}

// sshToShed establishes an SSH connection to a shed.
// If command is nil, an interactive shell is opened.
// If command is provided, it is executed on the shed.
func sshToShed(name string, command []string) error {
	// Find the server hosting this shed
	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}

	// Ensure the shed is running (auto-start if stopped)
	client := NewAPIClientFromNamedEntry(serverName, entry, clientConfig.GetCreateTimeout())
	if _, err := ensureRunningShed(client, name); err != nil {
		return err
	}

	if verboseLevel > 0 {
		fmt.Printf("Connecting to %s on %s...\n", name, serverName)
	}

	// Quote the command (if any). validateAndQuoteArgs single-quotes each argv
	// element so pipes, redirects, semicolons, spaces, and nested quotes survive
	// the SSH wire (issues #44 and #48) and rejects empty elements the posix-mode
	// server-side shlex would silently drop.
	var quoted []string
	if len(command) > 0 {
		quoted, err = validateAndQuoteArgs(command)
		if err != nil {
			return err
		}
	}

	// Pick -t (PTY) vs -T (clean 8-bit) by terminal detection — see ttyFlag.
	flag := ttyFlag(len(command) > 0, isStdinTTY(), isStdoutTTY())
	sshArgs := sshSessionArgs(flag, name, entry, quoted)

	// Find ssh binary
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found in PATH: %w", err)
	}

	// Replace current process with ssh
	if err := syscall.Exec(sshPath, sshArgs, os.Environ()); err != nil {
		return fmt.Errorf("failed to exec ssh: %w", err)
	}

	// This should never be reached
	return nil
}
