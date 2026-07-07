// shed-ext-rc is the guest-side RC Session Convention v2 helper. It runs inside a
// shed and is invoked over SSH by orchestrators (shed-remote-agent, shed-desktop,
// the shed CLI) to create, list, probe, prompt, and tear down remote-control tmux
// sessions named rc-<slug>. It prints the neutral JSON DTO on stdout and writes
// diagnostics to stderr; exit codes carry domain outcomes (see internal/clirc).
//
// This is a one-shot CLI, not a daemon. All tmux work happens locally in the shed.
// The command dispatch lives in internal/clirc, shared with the host-side sibling
// shed-machine-rc — this main only supplies shed-ext-rc's identity.
//
//	shed-ext-rc create --kind claude-rc --name my-shed/foo [--wait] [--prompt-stdin]
//	shed-ext-rc list
//	shed-ext-rc probe --slug abc123
//	shed-ext-rc accept-trust --slug abc123
//	shed-ext-rc prompt --slug abc123 [--session-id <uuid>]   # text on stdin
//	shed-ext-rc kill --slug abc123
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
