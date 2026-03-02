// Package vmutil provides shared VM agent communication code used by both
// Firecracker and VZ backends.
package vmutil

import (
	"context"
	"net"
)

// Dialer opens connections to specific ports on a VM.
// Firecracker implements this with CONNECT/OK handshake over a single UDS.
// VZ implements this with direct per-port Unix socket connections.
type Dialer interface {
	Dial(ctx context.Context, port uint32) (net.Conn, error)
}
