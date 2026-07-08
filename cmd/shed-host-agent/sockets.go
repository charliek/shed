package main

import (
	"os"
	"path/filepath"
	"runtime"
)

// The agent's IPC sockets live at fixed, well-known paths: they are the
// program's public interface — the shed-desktop app, `status`, and any future
// tooling all rendezvous here — so they are deliberately NOT configurable in
// the YAML config. (The config file is the daemon's private input; clients
// must be able to find the agent without reading it.) See
// docs/reference/host-agent-ipc.md for the full contract.
//
// SHED_HOST_AGENT_SOCKET_DIR overrides the directory — an escape hatch for
// tests and parallel dev agents, mirroring the desktop app's
// SHED_DESKTOP_HOST_AGENT_SOCKET.

// socketDir returns the fixed directory the agent's sockets live in.
func socketDir() string {
	if d := os.Getenv("SHED_HOST_AGENT_SOCKET_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(userHomeDir(), "Library", "Application Support", "shed")
	}
	// Future Linux hosts: the user runtime dir, else a stable home path.
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "shed")
	}
	return filepath.Join(userHomeDir(), ".local", "share", "shed")
}

// desktopSocketPath is the stateful approval channel: single consumer
// (normally the shed-desktop app) receiving the audit/event stream and
// deciding shed-desktop-policy approvals.
func desktopSocketPath() string { return filepath.Join(socketDir(), "host-agent.sock") }

// statusSocketPath is the read-only status socket: any client gets the
// daemon's LiveStatus JSON self-report per connection.
func statusSocketPath() string { return filepath.Join(socketDir(), "host-agent-status.sock") }
