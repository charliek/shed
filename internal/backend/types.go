// Package backend provides the abstraction layer for different execution backends.
package backend

import "io"

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
