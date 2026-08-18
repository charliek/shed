// The rc-parity harness's Go leg — the differential oracle (plan 010 H15).
//
// This is byte-identical in behavior to the retired cmd/shed-machine-rc: it
// calls clirc.Run with ProgName "shed-machine-rc" (the identity the goldens
// were recorded under), so the harness keeps pinning Go↔Rust convergence
// after the shipped component's retirement. The Go hub/engine code lives on
// in internal/ext for sheds; this main exists ONLY so the parity suite can
// build the machine-shaped binary. It ships nowhere.
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
