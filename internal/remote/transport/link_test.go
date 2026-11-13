package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/charliek/shed/internal/vmutil"
)

// pipeLink is the smallest possible Link. It exists to prove that a Link is
// usable wherever the server takes a vmutil.Dialer.
type pipeLink struct{ closed bool }

func (l *pipeLink) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	if l.closed {
		return nil, ErrClosed
	}
	client, server := net.Pipe()
	go server.Close()
	return client, nil
}

func (l *pipeLink) Close() error {
	l.closed = true
	return nil
}

func TestLinkIsADialer(t *testing.T) {
	var link Link = &pipeLink{}
	var dialer vmutil.Dialer = link
	agent := vmutil.NewAgentClient(dialer, PortConsole, PortNotify)
	if agent.NotifyPort() != 1026 {
		t.Errorf("NotifyPort = %d, want 1026", agent.NotifyPort())
	}
	conn, err := link.Dial(context.Background(), PortConsole)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	conn.Close()
	if err := link.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := link.Dial(context.Background(), PortConsole); !errors.Is(fmt.Errorf("dial: %w", err), ErrClosed) {
		t.Errorf("Dial after Close = %v, want ErrClosed", err)
	}
}

func TestSocketPaths(t *testing.T) {
	tests := []struct {
		port uint32
		name string
		path string
	}{
		{PortConsole, "1024.sock", "/run/shed-agent/1024.sock"},
		{PortNotify, "1026.sock", "/run/shed-agent/1026.sock"},
		{PortTCPProxy, "1028.sock", "/run/shed-agent/1028.sock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SocketName(tt.port); got != tt.name {
				t.Errorf("SocketName = %q, want %q", got, tt.name)
			}
			if got := SocketPath(GuestSocketDir, tt.port); got != tt.path {
				t.Errorf("SocketPath = %q, want %q", got, tt.path)
			}
		})
	}
	if got := SocketPath("/tmp/x/", PortNotify); got != "/tmp/x/1026.sock" {
		t.Errorf("SocketPath with a trailing slash = %q", got)
	}
}

func TestNames(t *testing.T) {
	tests := []struct {
		name, got, want string
	}{
		{"ssh", NameSSH, "ssh"},
		{"local", NameLocal, "local"},
		{"default install policy", InstallIncompatible, "incompatible"},
		{"development install policy", InstallAlways, "always"},
		{"no install", InstallNever, "never"},
		{"agent build", AgentBinaryName("amd64"), "shed-agent-linux-amd64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}
