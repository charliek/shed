package api

import (
	"context"
	"crypto/tls"
	"net"
	"time"
)

// connKey is the context key of the request's connection (ConnContext).
type connKey struct{}

// ConnContext is an http.Server ConnContext: it puts the accepted connection
// on every request's context, so a credential-bus stream can bound how long
// data written to it may stay unacknowledged (limitUnackedData).
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, c)
}

// limitUnackedData asks the kernel to fail the request's TCP connection when
// data written to it stays unacknowledged for d, where the platform has such
// a bound. It does nothing for a request whose server set no ConnContext, or
// whose connection is not TCP.
func limitUnackedData(ctx context.Context, d time.Duration) error {
	c, _ := ctx.Value(connKey{}).(net.Conn)
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return nil
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return err
	}
	return setUnackedTimeout(raw, d)
}
