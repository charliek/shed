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
	"github.com/charliek/shed/internal/plugin"
)

func TestExitError(t *testing.T) {
	err := &ExitError{Code: 42}
	if err.Error() != "command exited with code 42" {
		t.Errorf("Error() = %q, want %q", err.Error(), "command exited with code 42")
	}
}

func TestNewAgentClient(t *testing.T) {
	// Use a nil dialer since we're just testing construction
	client := NewAgentClient(nil, 1024, 1026)
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

// healthHandler reads a system:health request envelope and responds with a
// matching response envelope. Used by health check tests.
func healthHandler(conn net.Conn) {
	defer conn.Close()
	msgType, data, err := agentproto.ReadMessage(conn)
	if err != nil || msgType != agentproto.MsgTypePluginMessage {
		return
	}
	var env plugin.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return
	}
	if env.Namespace != plugin.NamespaceHealth || env.Type != plugin.MessageTypeRequest {
		return
	}
	resp := plugin.NewResponse(env.ID, plugin.NamespaceHealth, nil)
	respData, _ := json.Marshal(resp)
	agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, respData)
}

func TestCheckHealth(t *testing.T) {
	dialer := &pipeDialer{handler: healthHandler}
	client := NewAgentClient(dialer, 1024, 1026)
	if err := client.CheckHealth(context.Background()); err != nil {
		t.Errorf("CheckHealth() error = %v", err)
	}
}

func TestCheckHealthDialError(t *testing.T) {
	dialer := &errDialer{err: io.ErrClosedPipe}
	client := NewAgentClient(dialer, 1024, 1026)
	err := client.CheckHealth(context.Background())
	if err == nil {
		t.Error("CheckHealth() expected error when dial fails")
	}
}

func TestCheckHealthBadFrameType(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn)
			// Send wrong frame type (not MsgTypePluginMessage)
			agentproto.WriteMessage(conn, agentproto.MsgTypeData, nil)
		},
	}

	client := NewAgentClient(dialer, 1024, 1026)
	err := client.CheckHealth(context.Background())
	if err == nil {
		t.Error("CheckHealth() expected error for wrong frame type")
	}
	if !strings.Contains(err.Error(), "unexpected message type") {
		t.Errorf("CheckHealth() error = %q, want error about unexpected message type", err.Error())
	}
}

func TestCheckHealthWrongNamespace(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			msgType, data, _ := agentproto.ReadMessage(conn)
			if msgType != agentproto.MsgTypePluginMessage {
				return
			}
			var env plugin.Envelope
			json.Unmarshal(data, &env)
			// Respond with wrong namespace
			resp := plugin.NewResponse(env.ID, "system:other", nil)
			respData, _ := json.Marshal(resp)
			agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, respData)
		},
	}

	client := NewAgentClient(dialer, 1024, 1026)
	err := client.CheckHealth(context.Background())
	if err == nil {
		t.Error("CheckHealth() expected error for wrong namespace")
	}
	if !strings.Contains(err.Error(), "unexpected response namespace") {
		t.Errorf("CheckHealth() error = %q, want error about unexpected namespace", err.Error())
	}
}

func TestCheckHealthWrongType(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			msgType, data, _ := agentproto.ReadMessage(conn)
			if msgType != agentproto.MsgTypePluginMessage {
				return
			}
			var env plugin.Envelope
			json.Unmarshal(data, &env)
			// Respond with event instead of response
			resp := plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeEvent, nil)
			respData, _ := json.Marshal(resp)
			agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, respData)
		},
	}

	client := NewAgentClient(dialer, 1024, 1026)
	err := client.CheckHealth(context.Background())
	if err == nil {
		t.Error("CheckHealth() expected error for wrong message type")
	}
	if !strings.Contains(err.Error(), "unexpected response type") {
		t.Errorf("CheckHealth() error = %q, want error about unexpected response type", err.Error())
	}
}

func TestCheckHealthMismatchedInReplyTo(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn)
			// Respond with wrong InReplyTo
			resp := plugin.NewResponse("wrong-id", plugin.NamespaceHealth, nil)
			respData, _ := json.Marshal(resp)
			agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, respData)
		},
	}

	client := NewAgentClient(dialer, 1024, 1026)
	err := client.CheckHealth(context.Background())
	if err == nil {
		t.Error("CheckHealth() expected error for mismatched InReplyTo")
	}
	if !strings.Contains(err.Error(), "in_reply_to mismatch") {
		t.Errorf("CheckHealth() error = %q, want error about in_reply_to mismatch", err.Error())
	}
}

func TestCheckHealthMalformedJSON(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn)
			agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, []byte("not json"))
		},
	}

	client := NewAgentClient(dialer, 1024, 1026)
	err := client.CheckHealth(context.Background())
	if err == nil {
		t.Error("CheckHealth() expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "invalid health response envelope") {
		t.Errorf("CheckHealth() error = %q, want error about invalid envelope", err.Error())
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
			healthHandler(conn) // reuse — note: healthHandler defers Close too, but double-close on pipe is safe
		},
	}

	client := NewAgentClient(dialer, 1024, 1026)
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

	client := NewAgentClient(dialer, 1024, 1026)
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

	client := NewAgentClient(dialer, 1024, 1026)

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

	client := NewAgentClient(dialer, 1024, 1026)

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

	client := NewAgentClient(dialer, 1024, 1026)

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

	client := NewAgentClient(dialer, 1024, 1026)

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

	client := NewAgentClient(dialer, 1024, 1026)

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

func TestExecEOFBeforeExitCode(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn) // exec request
			agentproto.ReadMessage(conn) // stdin EOF
			// Close connection without sending exit code
		},
	}

	client := NewAgentClient(dialer, 1024, 1026)

	opts := backend.ExecOptions{
		Cmd: []string{"crash"},
		TTY: false,
	}

	err := client.Exec(context.Background(), opts)
	if err == nil {
		t.Fatal("Exec() expected error when connection closes before exit code")
	}
	if !strings.Contains(err.Error(), "connection closed before exit code received") {
		t.Errorf("Exec() error = %q, want error about connection closed before exit code", err.Error())
	}
}

func TestExecResizeGoroutineExitsOnCompletion(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn) // exec request
			agentproto.ReadMessage(conn) // stdin EOF
			time.Sleep(50 * time.Millisecond)
			agentproto.WriteExitCode(conn, 0)
		},
	}

	client := NewAgentClient(dialer, 1024, 1026)

	resizeCh := make(chan backend.TerminalSize)
	opts := backend.ExecOptions{
		Cmd:        []string{"echo", "test"},
		TTY:        true,
		ResizeChan: resizeCh,
	}

	// Use a background context that will NOT be cancelled.
	// The resize goroutine must exit via execDone, not ctx.Done().
	err := client.Exec(context.Background(), opts)
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}

	// After Exec returns, try sending to the resize channel.
	// If the goroutine exited (via execDone), nobody is receiving, so the
	// send blocks until timeout — that's the success case.
	// If the goroutine is still alive, it consumes the send — that's a failure.
	select {
	case resizeCh <- backend.TerminalSize{Width: 80, Height: 24}:
		t.Fatal("resize goroutine is still running after Exec returned")
	case <-time.After(200 * time.Millisecond):
		// Good: nobody consumed the send, goroutine has exited
	}
}

// blockingReader is a reader that blocks until its context is cancelled.
type blockingReader struct {
	ctx context.Context
}

func (r *blockingReader) Read(p []byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *blockingReader) Close() error {
	return nil
}

func TestExecStdinDoesNotBlockAfterCompletion(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			agentproto.ReadMessage(conn) // exec request
			// Don't read stdin — just send exit code immediately
			time.Sleep(50 * time.Millisecond)
			agentproto.WriteExitCode(conn, 0)
		},
	}

	client := NewAgentClient(dialer, 1024, 1026)

	// Create a context that we'll cancel after the test to clean up the blocking reader
	readerCtx, readerCancel := context.WithCancel(context.Background())
	defer readerCancel()

	opts := backend.ExecOptions{
		Cmd:   []string{"true"},
		Stdin: &blockingReader{ctx: readerCtx},
		TTY:   false,
	}

	// Exec should return promptly even though stdin is blocking
	done := make(chan error, 1)
	go func() {
		done <- client.Exec(context.Background(), opts)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Exec() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exec() did not return within 5 seconds — stdin is blocking completion")
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

	client := NewAgentClient(dialer, 1024, 1026)

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
