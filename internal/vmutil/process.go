package vmutil

import (
	"errors"
	"syscall"
)

// IsProcessAlive reports whether the given pid refers to a process the
// caller can signal (i.e., a live process that may or may not be owned
// by the caller).
//
// The check uses signal 0, which performs the kernel's send-signal
// permission validation but does not actually deliver a signal:
//   - nil  → process exists and the caller may signal it.
//   - EPERM → process exists but the caller lacks permission; treated
//     as alive (the process is still there).
//   - ESRCH → no such process; treated as dead.
//   - anything else → conservatively treated as dead so callers don't
//     loop on transient kernel errors.
//
// pid <= 0 is treated as not-alive: 0 means "the current process group"
// to syscall.Kill on Unix and is never a real VMM pid in our metadata;
// negatives are likewise invalid.
//
// This helper is intended for "did the VMM actually exit?" liveness
// checks at stop/start boundaries. It does NOT verify that the pid
// belongs to the expected program — callers concerned about PID reuse
// should follow up with the backend-specific check (isVfkitProcess /
// isFirecrackerProcess).
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}
