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

// RecheckedDialer is the Dialer for an operation that checked a shed's record
// and then connects to its VM through an endpoint named only by the shed's
// name (Firecracker's <name>.vsock, VZ's <name>-<port>.sock). A shed deleted
// and created again under that name between the check and the connect owns
// the endpoint by then, so the connection reaches the newcomer's VM. Each
// connection, once established and before anything is sent on it, is kept
// only if Recheck still finds the incarnation that was checked; otherwise it
// is closed and Recheck's error is returned. A record that is unchanged after
// the connect was unchanged throughout (a deleted incarnation never comes
// back), so the endpoint connected to was the checked shed's VM.
type RecheckedDialer struct {
	Dialer Dialer
	// Recheck returns nil when the shed's record is still the incarnation
	// that was checked, and an error to return otherwise.
	Recheck func() error
	// BeforeDial, when set, runs before each connect, after the caller's
	// check. It is a test seam for replacing the shed in that window.
	BeforeDial func()
}

// CheckedDialer is a Dialer whose connect writes on the connection before it
// returns it (Firecracker's CONNECT handshake). DialChecked runs check once the
// endpoint is connected and before anything is written on it; when check
// returns an error it closes the connection and returns that error.
type CheckedDialer interface {
	DialChecked(ctx context.Context, port uint32, check func() error) (net.Conn, error)
}

// Dial connects through the wrapped Dialer and then rechecks the shed. A
// wrapped CheckedDialer rechecks before its handshake, so the replacement's VM
// is sent nothing at all; any other Dialer sends nothing before it returns.
func (d *RecheckedDialer) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	if d.BeforeDial != nil {
		d.BeforeDial()
	}
	if cd, ok := d.Dialer.(CheckedDialer); ok {
		return cd.DialChecked(ctx, port, d.Recheck)
	}
	conn, err := d.Dialer.Dial(ctx, port)
	if err != nil {
		return nil, err
	}
	if err := d.Recheck(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
