//go:build linux
// +build linux

package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/charliek/shed/internal/vmutil"
)

// Compile-time check that FirecrackerDialer implements vmutil.Dialer.
var _ vmutil.Dialer = (*FirecrackerDialer)(nil)

// FirecrackerDialer implements vmutil.Dialer using Firecracker's vsock UDS protocol.
type FirecrackerDialer struct {
	socketPath string
}

// NewFirecrackerDialer creates a new FirecrackerDialer.
func NewFirecrackerDialer(socketPath string) *FirecrackerDialer {
	return &FirecrackerDialer{socketPath: socketPath}
}

// Dial connects to the guest via Firecracker's vsock UDS.
// Firecracker's vsock protocol requires:
// 1. Connect to the Unix domain socket
// 2. Send "CONNECT <port>\n"
// 3. Read response "OK <port>\n"
// 4. Then the connection is bridged to the guest
func (d *FirecrackerDialer) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	// Use context-aware dialing so cancellation unblocks the connect.
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", d.socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to vsock socket: %w", err)
	}

	// Close conn on ctx cancel so handshake I/O is bounded.
	cancelDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-cancelDone:
		}
	}()

	// handshakeFailed closes the cancel goroutine and the connection.
	handshakeFailed := func(fmtStr string, args ...interface{}) (net.Conn, error) {
		close(cancelDone)
		conn.Close()
		return nil, fmt.Errorf(fmtStr, args...)
	}

	connectCmd := fmt.Sprintf("CONNECT %d\n", port)
	if _, err := conn.Write([]byte(connectCmd)); err != nil {
		return handshakeFailed("failed to send CONNECT command: %w", err)
	}

	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')
	if err != nil {
		return handshakeFailed("failed to read CONNECT response: %w", err)
	}

	response = strings.TrimSpace(response)
	if !strings.HasPrefix(response, "OK ") {
		return handshakeFailed("vsock CONNECT failed: %s", response)
	}

	// Handshake succeeded — stop the cancel-close goroutine.
	close(cancelDone)

	// The cancel goroutine may have already selected ctx.Done() and closed
	// conn before observing cancelDone. Check for that race.
	if ctx.Err() != nil {
		conn.Close()
		return nil, ctx.Err()
	}

	return &vmutil.BufferedConn{Conn: conn, Reader: reader}, nil
}
