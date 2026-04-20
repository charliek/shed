// Package backend provides the abstraction layer for different execution backends.
package backend

import (
	"io"
	"time"
)

// PruneOptions controls Backend.Prune. If all scope flags are false,
// the caller should treat it as the default scope (images + instances +
// orphans). `Logs` is opt-in and never part of the default.
type PruneOptions struct {
	// Scope selectors (additive; callers may enable any combination).
	Images    bool
	Instances bool
	Logs      bool
	Orphans   bool

	// DryRun returns candidates without mutating anything.
	DryRun bool

	// Until filters stopped instances by mtime(metadata.json): only prune
	// instances whose mtime is older than now - Until. Zero means "any age"
	// (prune every stopped instance).
	Until time.Duration

	// LogTailBytes is the size console.log is truncated to when Logs is true.
	// Zero uses the backend default (5 MiB).
	LogTailBytes int64
}

// ExecOptions contains options for executing a command in a shed.
type ExecOptions struct {
	// Cmd is the command to execute. If empty, defaults to the shed's shell.
	Cmd []string

	// Stdin, Stdout, Stderr are the I/O streams.
	Stdin  io.ReadCloser
	Stdout io.WriteCloser
	Stderr io.WriteCloser

	// TTY indicates whether to allocate a pseudo-TTY.
	TTY bool

	// Env contains additional environment variables.
	Env []string

	// WorkingDir overrides the default working directory (/workspace).
	WorkingDir string

	// InitialSize is the initial terminal size (if TTY is true).
	InitialSize *TerminalSize

	// ResizeChan receives terminal resize events.
	ResizeChan <-chan TerminalSize
}

// TerminalSize represents terminal dimensions.
type TerminalSize struct {
	Width  uint
	Height uint
}
