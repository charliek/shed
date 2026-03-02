//go:build darwin
// +build darwin

package vz

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/vmutil"
)

func TestVZDialerImplementsInterface(t *testing.T) {
	var _ vmutil.Dialer = (*VZDialer)(nil)
}

func TestVZDialerConnects(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "vz-dialer-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	name := "test-vm"
	port := uint32(1024)
	socketPath := filepath.Join(tmpDir, "test-vm-1024.sock")

	// Start a listener on the expected socket path
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Failed to create Unix listener: %v", err)
	}
	defer listener.Close()

	dialer := NewVZDialer(tmpDir, name)
	conn, err := dialer.Dial(context.Background(), port)
	if err != nil {
		t.Fatalf("Dial() failed: %v", err)
	}
	conn.Close()
}

func TestVZDialerFailsWhenNoSocket(t *testing.T) {
	dialer := NewVZDialer("/nonexistent/dir", "test-vm")
	_, err := dialer.Dial(context.Background(), 1024)
	if err == nil {
		t.Error("Dial() should fail when no socket exists")
	}
}
