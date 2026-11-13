//go:build linux

package locallink

import "syscall"

// agentProcAttr makes the kernel send the agent SIGTERM when the thread that
// started it exits. The link starts the agent from a goroutine locked to its
// thread for the agent's whole life, so that means: when the server dies, by
// any route, its agent goes with it.
func agentProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
