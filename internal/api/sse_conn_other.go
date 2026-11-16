//go:build !linux && !darwin

package api

import (
	"syscall"
	"time"
)

// setUnackedTimeout does nothing where the platform has no bound on
// unacknowledged data; the stream's write deadline still applies.
func setUnackedTimeout(syscall.RawConn, time.Duration) error { return nil }
