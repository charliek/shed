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
	type dialResult struct {
		conn net.Conn
		err  error
	}
	result := make(chan dialResult, 1)
	done := make(chan struct{})

	go func() {
		conn, err := net.Dial("unix", d.socketPath)
		if err != nil {
			select {
			case result <- dialResult{nil, fmt.Errorf("failed to connect to vsock socket: %w", err)}:
			case <-done:
			}
			return
		}

		connectCmd := fmt.Sprintf("CONNECT %d\n", port)
		if _, err := conn.Write([]byte(connectCmd)); err != nil {
			conn.Close()
			select {
			case result <- dialResult{nil, fmt.Errorf("failed to send CONNECT command: %w", err)}:
			case <-done:
			}
			return
		}

		reader := bufio.NewReader(conn)
		response, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			select {
			case result <- dialResult{nil, fmt.Errorf("failed to read CONNECT response: %w", err)}:
			case <-done:
			}
			return
		}

		response = strings.TrimSpace(response)
		if !strings.HasPrefix(response, "OK ") {
			conn.Close()
			select {
			case result <- dialResult{nil, fmt.Errorf("vsock CONNECT failed: %s", response)}:
			case <-done:
			}
			return
		}

		wrapped := &vsockConn{Conn: conn, reader: reader}
		select {
		case result <- dialResult{wrapped, nil}:
		case <-done:
			_ = conn.Close()
		}
	}()

	select {
	case r := <-result:
		return r.conn, r.err
	case <-ctx.Done():
		close(done)
		select {
		case r := <-result:
			if r.conn != nil {
				_ = r.conn.Close()
			}
		default:
		}
		return nil, ctx.Err()
	}
}

// vsockConn wraps a net.Conn with a buffered reader for the initial handshake.
type vsockConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *vsockConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// CloseWrite forwards CloseWrite if the underlying connection supports it.
func (c *vsockConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
