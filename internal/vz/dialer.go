//go:build darwin
// +build darwin

package vz

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/charliek/shed/internal/vmutil"
)

// Compile-time check that VZDialer implements vmutil.Dialer.
var _ vmutil.Dialer = (*VZDialer)(nil)

// VZDialer implements vmutil.Dialer using direct Unix socket connections.
// Unlike Firecracker's CONNECT/OK handshake, vfkit exposes each vsock port
// as a separate Unix socket.
type VZDialer struct {
	socketDir string
	name      string // VM name, used in socket filenames
}

// NewVZDialer creates a new VZDialer.
func NewVZDialer(socketDir, name string) *VZDialer {
	return &VZDialer{socketDir: socketDir, name: name}
}

// Dial connects to the VM on the given port via a per-port Unix socket.
func (d *VZDialer) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	socketPath := filepath.Join(d.socketDir, fmt.Sprintf("%s-%d.sock", d.name, port))
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", socketPath)
}

// DialService connects to a TCP service inside the VM via the TCP proxy vsock port.
// It performs a CONNECT handshake and returns a raw TCP connection to the target port.
func (d *VZDialer) DialService(ctx context.Context, tcpProxyPort uint32, servicePort uint16) (net.Conn, error) {
	conn, err := d.Dial(ctx, tcpProxyPort)
	if err != nil {
		return nil, fmt.Errorf("dial tcp proxy: %w", err)
	}

	// Context cancellation closes the connection to unblock reads/writes.
	cancelDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-cancelDone:
		}
	}()

	connectCmd := fmt.Sprintf("CONNECT %d\n", servicePort)
	if _, err := conn.Write([]byte(connectCmd)); err != nil {
		close(cancelDone)
		conn.Close()
		return nil, fmt.Errorf("send CONNECT: %w", err)
	}

	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')
	if err != nil {
		close(cancelDone)
		conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}

	close(cancelDone)
	if ctx.Err() != nil {
		conn.Close()
		return nil, ctx.Err()
	}

	response = strings.TrimSpace(response)
	if response != "OK" {
		conn.Close()
		msg := strings.TrimPrefix(response, "ERR ")
		return nil, fmt.Errorf("CONNECT %d failed: %s", servicePort, msg)
	}

	return &vmutil.BufferedConn{Conn: conn, Reader: reader}, nil
}
