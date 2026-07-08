// shed-machine-rc is the host-side RC Session Convention v2 helper — the sibling of
// shed-ext-rc for native machines (laptops, workstations, tailnet hosts). It creates,
// lists, probes, prompts, and tears down claude remote-control tmux sessions named
// rc-<slug> on the machine it runs on, printing the neutral JSON DTO on stdout so
// orchestrators (shed-remote-agent, shed-mobile) drive it over SSH exactly as they
// drive shed-ext-rc inside a shed. The command dispatch is shared via internal/clirc;
// this main supplies shed-machine-rc's identity and enables the host-only `claude`
// convenience verb.
//
//	shed-machine-rc claude [--name n] [--workdir d] [--skip]   # local auto-mode session
//	shed-machine-rc create --kind claude-rc --name host/foo [--wait] [--prompt-stdin]
//	shed-machine-rc list
//	shed-machine-rc probe --slug abc123
//	shed-machine-rc accept-trust --slug abc123
//	shed-machine-rc prompt --slug abc123 [--session-id <uuid>]   # text on stdin
//	shed-machine-rc kill --slug abc123
package main

import (
	"os"

	"github.com/charliek/shed/internal/ext/clirc"
)

func main() {
	os.Exit(clirc.Run(clirc.Config{
		ProgName:         "shed-machine-rc",
		DefaultCreatedBy: "shed-machine-rc",
		EnableClaudeVerb: true,
	}, os.Args[1:]))
}
