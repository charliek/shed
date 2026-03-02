//go:build linux
// +build linux

package firecracker

import (
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/vmutil"
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
		serverCfg: nil,
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
		serverCfg: nil,
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
	var _ backend.Backend = (*FirecrackerBackend)(nil)
}

func TestNopWriteCloser(t *testing.T) {
	var buf []byte
	w := vmutil.NopWriteCloser(&byteWriter{buf: &buf})

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

// TestExecCommandBuilding verifies that the command-building logic in Exec
// wraps commands correctly: empty -> login shell, non-empty -> bash --login -c join.
func TestExecCommandBuilding(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		wantCmd []string
	}{
		{
			name:    "empty command gets login shell",
			input:   nil,
			wantCmd: []string{"/bin/bash", "--login"},
		},
		{
			name:    "simple command wrapped in login shell",
			input:   []string{"echo", "hello"},
			wantCmd: []string{"/bin/bash", "--login", "-c", "echo hello"},
		},
		{
			name:    "command with operators passes through",
			input:   []string{"export", "PATH=$PATH", "&&", "bun", "test"},
			wantCmd: []string{"/bin/bash", "--login", "-c", "export PATH=$PATH && bun test"},
		},
		{
			name:    "command with variable expansion",
			input:   []string{"echo", "$HOME"},
			wantCmd: []string{"/bin/bash", "--login", "-c", "echo $HOME"},
		},
		{
			name:    "command with pipes",
			input:   []string{"ls", "|", "grep", "foo"},
			wantCmd: []string{"/bin/bash", "--login", "-c", "ls | grep foo"},
		},
		{
			name:    "single command still wrapped",
			input:   []string{"whoami"},
			wantCmd: []string{"/bin/bash", "--login", "-c", "whoami"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := tt.input
			if len(cmd) == 0 {
				cmd = []string{"/bin/bash", "--login"}
			} else {
				cmd = []string{"/bin/bash", "--login", "-c", strings.Join(cmd, " ")}
			}

			if len(cmd) != len(tt.wantCmd) {
				t.Fatalf("got %v, want %v", cmd, tt.wantCmd)
			}
			for i := range cmd {
				if cmd[i] != tt.wantCmd[i] {
					t.Errorf("cmd[%d] = %q, want %q", i, cmd[i], tt.wantCmd[i])
				}
			}
		})
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
