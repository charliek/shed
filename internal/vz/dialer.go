//go:build darwin
// +build darwin

package vz

import (
	"context"
	"fmt"
	"net"
	"path/filepath"

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
