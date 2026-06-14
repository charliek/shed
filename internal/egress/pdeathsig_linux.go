//go:build linux

package egress

import (
	"os/exec"
	"syscall"
)

// setPdeathsig asks the kernel to SIGTERM the proxy child if shed-server (its
// parent) dies, so an ungraceful shed-server exit never leaves an orphaned
// proxy holding listener ports. Linux-only; other platforms rely on the
// proxy's getppid() orphan-poller.
func setPdeathsig(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}
