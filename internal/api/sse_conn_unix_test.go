//go:build linux || darwin

package api

import "syscall"

// smallReceiveBuffer is a net.Dialer Control that shrinks the socket's
// receive buffer, so a subscriber that stops reading stalls the server's
// writes sooner.
func smallReceiveBuffer(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
	}); err != nil {
		return err
	}
	return serr
}
