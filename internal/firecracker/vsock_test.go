//go:build linux
// +build linux

package firecracker

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
)

func TestNewVsockClient(t *testing.T) {
	client := NewVsockClient("/tmp/test.vsock", 1024, 1025)

	if client.socketPath != "/tmp/test.vsock" {
		t.Errorf("socketPath = %v, want /tmp/test.vsock", client.socketPath)
	}
	if client.consolePort != 1024 {
		t.Errorf("consolePort = %v, want 1024", client.consolePort)
	}
	if client.healthPort != 1025 {
		t.Errorf("healthPort = %v, want 1025", client.healthPort)
	}
}

func TestNewVsockClient_DifferentValues(t *testing.T) {
	tests := []struct {
		name        string
		socketPath  string
		consolePort uint32
		healthPort  uint32
	}{
		{"default", "/var/lib/shed/sockets/test.vsock", 1024, 1025},
		{"custom path", "/tmp/firecracker/vm1.vsock", 1024, 1025},
		{"different ports", "/run/firecracker.vsock", 2000, 2001},
		{"same ports", "/tmp/test.vsock", 1000, 1000}, // technically valid
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewVsockClient(tt.socketPath, tt.consolePort, tt.healthPort)

			if client.socketPath != tt.socketPath {
				t.Errorf("socketPath = %v, want %v", client.socketPath, tt.socketPath)
			}
			if client.consolePort != tt.consolePort {
				t.Errorf("consolePort = %v, want %v", client.consolePort, tt.consolePort)
			}
			if client.healthPort != tt.healthPort {
				t.Errorf("healthPort = %v, want %v", client.healthPort, tt.healthPort)
			}
		})
	}
}

func TestDialWithContext_NonexistentSocket(t *testing.T) {
	client := NewVsockClient("/tmp/nonexistent-socket-path.sock", 1024, 1025)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := client.dialWithContext(ctx, 1024)
	if err == nil {
		t.Fatal("dialWithContext() expected error for nonexistent socket, got nil")
	}
	if !strings.Contains(err.Error(), "failed to connect to vsock socket") {
		t.Errorf("dialWithContext() error = %v, want 'failed to connect to vsock socket'", err)
	}
}

func TestDialWithContext_ContextCancelled(t *testing.T) {
	// Create a unix socket that accepts but never responds with "OK"
	tmpDir := mustTempDir(t, "vsock-test")
	socketPath := tmpDir + "/test.sock"

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	// Accept connections but don't respond
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Read and discard but never respond
			buf := make([]byte, 256)
			_, _ = conn.Read(buf)
			// Hold connection open
			time.Sleep(10 * time.Second)
			conn.Close()
		}
	}()

	client := NewVsockClient(socketPath, 1024, 1025)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err = client.dialWithContext(ctx, 1024)
	if err == nil {
		t.Fatal("dialWithContext() expected error for cancelled context, got nil")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") && !strings.Contains(err.Error(), "canceled") {
		t.Errorf("dialWithContext() error = %v, want context error", err)
	}
}

func TestDialWithContext_BadResponse(t *testing.T) {
	// Create a socket that responds with an error
	tmpDir := mustTempDir(t, "vsock-test")
	socketPath := tmpDir + "/test.sock"

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Read CONNECT command
			buf := make([]byte, 256)
			_, _ = conn.Read(buf)
			// Respond with error
			_, _ = conn.Write([]byte("ERR invalid port\n"))
			conn.Close()
		}
	}()

	client := NewVsockClient(socketPath, 1024, 1025)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = client.dialWithContext(ctx, 1024)
	if err == nil {
		t.Fatal("dialWithContext() expected error for bad response, got nil")
	}
	if !strings.Contains(err.Error(), "vsock CONNECT failed") {
		t.Errorf("dialWithContext() error = %v, want 'vsock CONNECT failed'", err)
	}
}

func TestDialWithContext_Success(t *testing.T) {
	// Create a socket that responds with OK
	tmpDir := mustTempDir(t, "vsock-test")
	socketPath := tmpDir + "/test.sock"

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Read CONNECT command
			buf := make([]byte, 256)
			_, _ = conn.Read(buf)
			// Respond with OK
			_, _ = conn.Write([]byte("OK 1024\n"))
			// Keep connection open briefly
			time.Sleep(100 * time.Millisecond)
			conn.Close()
		}
	}()

	client := NewVsockClient(socketPath, 1024, 1025)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := client.dialWithContext(ctx, 1024)
	if err != nil {
		t.Fatalf("dialWithContext() error = %v", err)
	}
	defer conn.Close()

	// Connection should be a vsockConn wrapping the underlying connection
	if _, ok := conn.(*vsockConn); !ok {
		t.Errorf("dialWithContext() returned %T, want *vsockConn", conn)
	}
}

func TestCheckHealth_NonexistentSocket(t *testing.T) {
	client := NewVsockClient("/tmp/nonexistent.sock", 1024, 1025)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := client.CheckHealth(ctx)
	if err == nil {
		t.Fatal("CheckHealth() expected error for nonexistent socket, got nil")
	}
}

func TestCheckHealth_Success(t *testing.T) {
	// Create a mock health endpoint
	tmpDir := mustTempDir(t, "vsock-test")
	socketPath := tmpDir + "/test.sock"

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to create listener: %v", err)
	}
	defer listener.Close()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Read CONNECT command
			buf := make([]byte, 256)
			_, _ = conn.Read(buf)
			// Respond with OK for vsock handshake
			_, _ = conn.Write([]byte("OK 1025\n"))

			// Read health request message
			_, _, _ = agentproto.ReadMessage(conn)
			// Send health response
			_ = agentproto.WriteMessage(conn, agentproto.MsgTypeHealthResponse, nil)
			conn.Close()
		}
	}()

	client := NewVsockClient(socketPath, 1024, 1025)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = client.CheckHealth(ctx)
	if err != nil {
		t.Fatalf("CheckHealth() error = %v", err)
	}
}

func TestWaitForHealth_Timeout(t *testing.T) {
	// Use a nonexistent socket so health checks always fail
	client := NewVsockClient("/tmp/nonexistent.sock", 1024, 1025)

	ctx := context.Background()
	err := client.WaitForHealth(ctx, 1*time.Second)
	if err == nil {
		t.Fatal("WaitForHealth() expected error on timeout, got nil")
	}
}

func TestVsockConnCloseWrite(t *testing.T) {
	// Test CloseWrite on a connection that supports it
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// net.Pipe doesn't support CloseWrite, so this should return nil gracefully
	vc := &vsockConn{Conn: client}
	err := vc.CloseWrite()
	if err != nil {
		t.Errorf("CloseWrite() on net.Pipe = %v, want nil", err)
	}
}
