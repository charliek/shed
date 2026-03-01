package vmutil

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/backend"
)

func TestExitError(t *testing.T) {
	err := &ExitError{Code: 42}
	if err.Error() != "command exited with code 42" {
		t.Errorf("Error() = %q, want %q", err.Error(), "command exited with code 42")
	}
}

func TestNewAgentClient(t *testing.T) {
	// Use a nil dialer since we're just testing construction
	client := NewAgentClient(nil, 1024, 1025, 1026)
	if client == nil {
		t.Fatal("NewAgentClient() returned nil")
	}
	if client.NotifyPort() != 1026 {
		t.Errorf("NotifyPort() = %d, want 1026", client.NotifyPort())
	}
	if client.Dialer() != nil {
		t.Error("Dialer() should be nil when constructed with nil")
	}
}

func TestNopWriteCloser(t *testing.T) {
	var buf []byte
	w := NopWriteCloser(&byteWriter{buf: &buf})

	data := []byte("hello world")
	n, err := w.Write(data)
	if err != nil {
		t.Errorf("Write() error = %v", err)
	}
	if n != len(data) {
		t.Errorf("Write() = %d, want %d", n, len(data))
	}
	if string(buf) != "hello world" {
		t.Errorf("Written data = %q, want %q", buf, "hello world")
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

// pipeDialer implements Dialer using net.Pipe() for in-memory testing.
type pipeDialer struct {
	// handler is called with the server-side connection when Dial() is called.
	handler func(conn net.Conn)
}

func (d *pipeDialer) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	client, server := net.Pipe()
	go d.handler(server)
	return client, nil
}

// errDialer always returns an error.
type errDialer struct {
	err error
}

func (d *errDialer) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	return nil, d.err
}

func TestCheckHealth(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			// Read the health request
			msgType, _, err := agentproto.ReadMessage(conn)
			if err != nil {
				return
			}
			if msgType != agentproto.MsgTypeHealthRequest {
				return
			}
			// Send health response
			agentproto.WriteMessage(conn, agentproto.MsgTypeHealthResponse, nil)
		},
	}

	client := NewAgentClient(dialer, 1024, 1025, 1026)
	err := client.CheckHealth(context.Background())
	if err != nil {
		t.Errorf("CheckHealth() error = %v", err)
	}
}

func TestCheckHealthDialError(t *testing.T) {
	dialer := &errDialer{err: io.ErrClosedPipe}
	client := NewAgentClient(dialer, 1024, 1025, 1026)
	err := client.CheckHealth(context.Background())
	if err == nil {
		t.Error("CheckHealth() expected error when dial fails")
	}
}

func TestCheckHealthBadResponse(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn)
			// Send wrong message type
			agentproto.WriteMessage(conn, agentproto.MsgTypeData, nil)
		},
	}

	client := NewAgentClient(dialer, 1024, 1025, 1026)
	err := client.CheckHealth(context.Background())
	if err == nil {
		t.Error("CheckHealth() expected error for wrong response type")
	}
}

func TestWaitForHealth(t *testing.T) {
	var calls int32
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			n := atomic.AddInt32(&calls, 1)
			if n < 3 {
				// Close connection to simulate unhealthy
				return
			}
			agentproto.ReadMessage(conn)
			agentproto.WriteMessage(conn, agentproto.MsgTypeHealthResponse, nil)
		},
	}

	client := NewAgentClient(dialer, 1024, 1025, 1026)
	err := client.WaitForHealth(context.Background(), 10*time.Second)
	if err != nil {
		t.Errorf("WaitForHealth() error = %v", err)
	}
	if atomic.LoadInt32(&calls) < 3 {
		t.Errorf("expected at least 3 health check attempts, got %d", atomic.LoadInt32(&calls))
	}
}

func TestWaitForHealthTimeout(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			// Always unhealthy — close immediately
			conn.Close()
		},
	}

	client := NewAgentClient(dialer, 1024, 1025, 1026)
	err := client.WaitForHealth(context.Background(), 1*time.Second)
	if err == nil {
		t.Error("WaitForHealth() expected timeout error")
	}
}

func TestExecSimpleCommand(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()

			// Read exec request
			msgType, data, err := agentproto.ReadMessage(conn)
			if err != nil {
				return
			}
			if msgType != agentproto.MsgTypeExecRequest {
				return
			}

			var req agentproto.ExecRequest
			json.Unmarshal(data, &req)

			// Read stdin EOF
			agentproto.ReadMessage(conn)

			// Send output
			agentproto.WriteData(conn, []byte("hello from vm"))

			// Send exit code
			agentproto.WriteExitCode(conn, 0)
		},
	}

	client := NewAgentClient(dialer, 1024, 1025, 1026)

	var output strings.Builder
	opts := backend.ExecOptions{
		Cmd:    []string{"echo", "hello"},
		Stdout: NopWriteCloser(&output),
		TTY:    false,
	}

	err := client.Exec(context.Background(), opts)
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if output.String() != "hello from vm" {
		t.Errorf("output = %q, want %q", output.String(), "hello from vm")
	}
}

func TestExecNonZeroExit(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn) // exec request
			agentproto.ReadMessage(conn) // stdin EOF
			agentproto.WriteExitCode(conn, 1)
		},
	}

	client := NewAgentClient(dialer, 1024, 1025, 1026)

	opts := backend.ExecOptions{
		Cmd: []string{"false"},
		TTY: false,
	}

	err := client.Exec(context.Background(), opts)
	if err == nil {
		t.Fatal("Exec() expected error for non-zero exit")
	}
	exitErr, ok := err.(*ExitError)
	if !ok {
		t.Fatalf("expected *ExitError, got %T", err)
	}
	if exitErr.Code != 1 {
		t.Errorf("exit code = %d, want 1", exitErr.Code)
	}
}

func TestExecWithStdin(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn) // exec request

			// Read stdin data frames until EOF
			var received []byte
			for {
				msgType, data, err := agentproto.ReadMessage(conn)
				if err != nil {
					break
				}
				if msgType == agentproto.MsgTypeStdinEOF {
					break
				}
				if msgType == agentproto.MsgTypeData {
					received = append(received, data...)
				}
			}

			// Echo back what we received
			agentproto.WriteData(conn, received)
			agentproto.WriteExitCode(conn, 0)
		},
	}

	client := NewAgentClient(dialer, 1024, 1025, 1026)

	var output strings.Builder
	opts := backend.ExecOptions{
		Cmd:    []string{"cat"},
		Stdin:  io.NopCloser(strings.NewReader("test input")),
		Stdout: NopWriteCloser(&output),
		TTY:    false,
	}

	err := client.Exec(context.Background(), opts)
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if output.String() != "test input" {
		t.Errorf("output = %q, want %q", output.String(), "test input")
	}
}

func TestExecContextCancellation(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn) // exec request
			agentproto.ReadMessage(conn) // stdin EOF
			// Never send exit code — simulate a hang
			select {}
		},
	}

	client := NewAgentClient(dialer, 1024, 1025, 1026)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	opts := backend.ExecOptions{
		Cmd: []string{"sleep", "999"},
		TTY: false,
	}

	err := client.Exec(ctx, opts)
	if err == nil {
		t.Fatal("Exec() expected context error")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("Exec() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestExecResizeGoroutineExitsOnContextCancel(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn) // exec request
			agentproto.ReadMessage(conn) // stdin EOF
			// Send exit code after a short delay
			time.Sleep(50 * time.Millisecond)
			agentproto.WriteExitCode(conn, 0)
		},
	}

	client := NewAgentClient(dialer, 1024, 1025, 1026)

	resizeCh := make(chan backend.TerminalSize)
	opts := backend.ExecOptions{
		Cmd:        []string{"echo", "test"},
		TTY:        true,
		ResizeChan: resizeCh,
	}

	// The resize goroutine should exit when exec completes.
	// If the fix works, the goroutine observes ctx.Done() and exits.
	err := client.Exec(context.Background(), opts)
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
}

func TestExecWorkingDir(t *testing.T) {
	var receivedReq agentproto.ExecRequest

	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			_, data, _ := agentproto.ReadMessage(conn)
			json.Unmarshal(data, &receivedReq)
			agentproto.ReadMessage(conn) // stdin EOF
			agentproto.WriteExitCode(conn, 0)
		},
	}

	client := NewAgentClient(dialer, 1024, 1025, 1026)

	// Test default working dir
	opts := backend.ExecOptions{
		Cmd: []string{"ls"},
		TTY: false,
	}
	if err := client.Exec(context.Background(), opts); err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if receivedReq.WorkingDir != "/workspace" {
		t.Errorf("default WorkingDir = %q, want %q", receivedReq.WorkingDir, "/workspace")
	}

	// Test custom working dir
	opts.WorkingDir = "/tmp"
	if err := client.Exec(context.Background(), opts); err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if receivedReq.WorkingDir != "/tmp" {
		t.Errorf("custom WorkingDir = %q, want %q", receivedReq.WorkingDir, "/tmp")
	}
}
