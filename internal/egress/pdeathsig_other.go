//go:build !linux

package egress

import "os/exec"

// setPdeathsig is a no-op off Linux (no PR_SET_PDEATHSIG equivalent on macOS).
// The proxy's getppid() orphan-poller covers parent-death teardown there.
func setPdeathsig(cmd *exec.Cmd) {}
