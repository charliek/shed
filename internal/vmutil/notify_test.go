package vmutil

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
)

// mockNotifyHandler records calls and behavior for testing.
type mockNotifyHandler struct {
	onConnectCalls int32
	onMessageCalls int32
	onConnectErr   error
	lastMsgType    byte
	lastMsgData    []byte
}

func (h *mockNotifyHandler) OnConnect(conn net.Conn) error {
	atomic.AddInt32(&h.onConnectCalls, 1)
	return h.onConnectErr
}

func (h *mockNotifyHandler) OnMessage(msgType byte, data []byte) error {
	atomic.AddInt32(&h.onMessageCalls, 1)
	h.lastMsgType = msgType
	h.lastMsgData = data
	return nil
}

func TestNotifyConnStartStop(t *testing.T) {
	handler := &mockNotifyHandler{}

	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			// Send a message, then hang until connection closes
			agentproto.WriteMessage(conn, agentproto.MsgTypeFileChanged, []byte(`{"credential":"test"}`))
			// Block until closed
			buf := make([]byte, 1)
			conn.Read(buf)
		},
	}

	nc := NewNotifyConn(dialer, 1026, "test-vm")
	nc.Start(context.Background(), handler)

	// Give time for connection and message processing
	time.Sleep(200 * time.Millisecond)

	nc.Stop()

	if atomic.LoadInt32(&handler.onConnectCalls) < 1 {
		t.Error("OnConnect should have been called at least once")
	}
	if atomic.LoadInt32(&handler.onMessageCalls) < 1 {
		t.Error("OnMessage should have been called at least once")
	}
}

func TestNotifyConnReconnectsOnClose(t *testing.T) {
	connectCount := int32(0)

	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			count := atomic.AddInt32(&connectCount, 1)
			if count < 3 {
				// Close immediately to force reconnect
				conn.Close()
				return
			}
			// Third connection: stay open until test finishes
			defer conn.Close()
			buf := make([]byte, 1)
			conn.Read(buf)
		},
	}

	handler := &mockNotifyHandler{}
	nc := NewNotifyConn(dialer, 1026, "test-vm")
	nc.Start(context.Background(), handler)

	// Wait for reconnects — the initial backoff is 1s, so wait enough time.
	deadline := time.After(5 * time.Second)
	for atomic.LoadInt32(&connectCount) < 3 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for reconnects, got %d connections", atomic.LoadInt32(&connectCount))
		case <-time.After(100 * time.Millisecond):
		}
	}

	nc.Stop()

	if c := atomic.LoadInt32(&connectCount); c < 3 {
		t.Errorf("expected at least 3 connections (reconnects), got %d", c)
	}
}

func TestNotifyConnGracefulStop(t *testing.T) {
	dialer := &pipeDialer{
		handler: func(conn net.Conn) {
			defer conn.Close()
			buf := make([]byte, 1)
			conn.Read(buf)
		},
	}

	handler := &mockNotifyHandler{}
	nc := NewNotifyConn(dialer, 1026, "test-vm")
	nc.Start(context.Background(), handler)

	time.Sleep(100 * time.Millisecond)

	// Stop should return promptly (not hang)
	done := make(chan struct{})
	go func() {
		nc.Stop()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not return within 5 seconds")
	}
}
