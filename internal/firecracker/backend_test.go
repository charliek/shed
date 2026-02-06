//go:build linux
// +build linux

package firecracker

import (
	"testing"

	"github.com/charliek/shed/internal/backend"
)

func TestNewBackend(t *testing.T) {
	dir := mustTempDir(t, "backend-test")
	cfg := testFirecrackerConfig(dir)

	netMgr, err := NewNetworkManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.TAPPrefix)
	if err != nil {
		t.Fatalf("NewNetworkManager() error = %v", err)
	}

	client := &Client{
		cfg:       cfg,
		serverCfg: nil, // Not needed for this test
		netMgr:    netMgr,
		vms:       make(map[string]*VM),
		usedCIDs:  make(map[uint32]string),
		usedIPs:   make(map[string]string),
	}

	b := NewBackend(client)
	if b == nil {
		t.Fatal("NewBackend() returned nil")
	}

	if b.client != client {
		t.Error("backend client not set correctly")
	}
}

func TestBackendType(t *testing.T) {
	dir := mustTempDir(t, "backend-test")
	cfg := testFirecrackerConfig(dir)

	netMgr, err := NewNetworkManager(cfg.BridgeName, cfg.BridgeCIDR, cfg.TAPPrefix)
	if err != nil {
		t.Fatalf("NewNetworkManager() error = %v", err)
	}

	client := &Client{
		cfg:       cfg,
		serverCfg: nil, // Not needed for this test
		netMgr:    netMgr,
		vms:       make(map[string]*VM),
		usedCIDs:  make(map[uint32]string),
		usedIPs:   make(map[string]string),
	}

	b := NewBackend(client)
	if b.Type() != backend.TypeFirecracker {
		t.Errorf("Type() = %v, want %v", b.Type(), backend.TypeFirecracker)
	}
}

func TestBackendImplementsInterface(t *testing.T) {
	// This is a compile-time check, but let's verify at runtime too
	var _ backend.Backend = (*FirecrackerBackend)(nil)
}

func TestNopWriteCloser(t *testing.T) {
	var buf []byte
	w := &nopWriteCloser{w: &byteWriter{buf: &buf}}

	data := []byte("test data")
	n, err := w.Write(data)
	if err != nil {
		t.Errorf("Write() error = %v", err)
	}
	if n != len(data) {
		t.Errorf("Write() = %d, want %d", n, len(data))
	}

	if err := w.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

// byteWriter is a simple io.Writer for testing
type byteWriter struct {
	buf *[]byte
}

func (w *byteWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}
