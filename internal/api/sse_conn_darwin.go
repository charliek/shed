//go:build darwin

package api

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// setUnackedTimeout sets TCP_RXT_CONNDROPTIME (seconds): the connection is
// dropped when retransmissions of unacknowledged data have gone on that
// long.
func setUnackedTimeout(raw syscall.RawConn, d time.Duration) error {
	secs := int(d / time.Second)
	if secs < 1 {
		secs = 1
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_RXT_CONNDROPTIME, secs)
	}); err != nil {
		return err
	}
	return serr
}
