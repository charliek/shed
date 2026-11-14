//go:build linux

package sshlink

import (
	"net"
	"syscall"
)

// quickAck returns a function that tells the kernel to acknowledge what the
// connection has received now, not after its delayed-ACK timer. The link
// calls it after every read from the SSH connection.
//
// Why: sshd turns off Nagle's algorithm only for interactive sessions (ones
// with a terminal), and a link has none, so sshd's side of the connection
// holds back each small packet until the one before it is acknowledged. The
// agent's protocol writes a frame as two small writes, which sshd forwards
// as two small packets. Linux delays the acknowledgement of the first by 40
// ms hoping to send it with data, so without this every probe, every exec
// round trip and every burst of terminal output stalls for 40 ms. Measured
// against OpenSSH 9.6 over a direct TCP connection: median probe 41 ms and
// median "echo" exec 45 ms without, 0.2 ms and 0.9 ms with.
// TestSSHDLatency guards it.
//
// The option is not sticky (the kernel leaves quick-ACK mode again as soon as
// the traffic looks like request and response), which is why it is set after
// each read and not once.
func quickAck(c net.Conn) func() {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return func() {}
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return func() {}
	}
	return func() {
		_ = raw.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)
		})
	}
}
