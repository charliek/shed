//go:build linux

package api

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// setUnackedTimeout sets TCP_USER_TIMEOUT (milliseconds): the connection
// fails when written data stays unacknowledged that long.
func setUnackedTimeout(raw syscall.RawConn, d time.Duration) error {
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(d.Milliseconds()))
	}); err != nil {
		return err
	}
	return serr
}
