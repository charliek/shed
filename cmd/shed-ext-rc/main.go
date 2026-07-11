// shed-ext-rc is the guest-side RC Session Convention v2 helper. It runs inside a
// shed and is invoked over SSH by orchestrators (shed-remote-agent, shed-desktop,
// the shed CLI) to create, list, probe, prompt, and tear down remote-control tmux
// sessions named rc-<slug>. It prints the neutral JSON DTO on stdout and writes
// diagnostics to stderr; exit codes carry domain outcomes (see internal/clirc).
//
// The subcommands are one-shot (each does its tmux work locally and exits) EXCEPT
// `serve`, which runs the resident rc activity hub: a small loopback HTTP server
// (127.0.0.1:1029) that watches the shed's rc sessions and streams live activity.
// The hub is spawned on demand (`create` ensures it; the server proxy can start it)
// via `serve --detach`, binds the port as its lock, and self-exits when idle. The
// command dispatch lives in internal/ext/clirc, shared with the host-side sibling
// shed-machine-rc — this main only supplies shed-ext-rc's identity.
//
//	shed-ext-rc create --kind claude-rc --name my-shed/foo [--wait] [--prompt-stdin]
//	shed-ext-rc list
//	shed-ext-rc capabilities
//	shed-ext-rc probe --slug abc123
//	shed-ext-rc accept-trust --slug abc123
//	shed-ext-rc prompt --slug abc123 [--session-id <uuid>]   # text on stdin
//	shed-ext-rc kill --slug abc123
//	shed-ext-rc serve [--detach | --foreground]              # run the rc activity hub
package main

import (
	"os"

	"github.com/charliek/shed/internal/ext/clirc"
)

func main() {
	os.Exit(clirc.Run(clirc.Config{
		ProgName:         "shed-ext-rc",
		DefaultCreatedBy: "shed-ext-rc",
	}, os.Args[1:]))
}
