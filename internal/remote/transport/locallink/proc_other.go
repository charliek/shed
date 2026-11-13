//go:build !linux

package locallink

import "syscall"

func agentProcAttr() *syscall.SysProcAttr { return nil }
