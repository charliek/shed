//go:build linux

package api

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestLimitUnackedDataSetsTheUserTimeout: a connection put on the context by
// ConnContext is given TCP_USER_TIMEOUT; a context with none is left alone.
func TestLimitUnackedDataSetsTheUserTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
			defer c.Close()
			time.Sleep(time.Second)
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := limitUnackedData(context.Background(), 40*time.Second); err != nil {
		t.Fatalf("no connection on the context: %v", err)
	}
	if err := limitUnackedData(ConnContext(context.Background(), c), 40*time.Second); err != nil {
		t.Fatal(err)
	}
	raw, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		got, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT)
	}); err != nil || gerr != nil {
		t.Fatal(err, gerr)
	}
	if got != 40000 {
		t.Fatalf("TCP_USER_TIMEOUT = %d ms, want 40000", got)
	}
}
