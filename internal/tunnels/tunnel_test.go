package tunnels

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// assertStopReturns runs tun.Stop() and fails if it doesn't return promptly —
// the regression guard for the teardown hang (issue #223).
func assertStopReturns(t *testing.T, tun *Tunnel) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		tun.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() hung")
	}
}

// startTunnel starts a Tunnel against srv on an ephemeral local port and opens a
// client connection through it (which drives handleConn -> Dial). The client
// conn is returned for the caller to exercise and is closed via t.Cleanup.
func startTunnel(t *testing.T, srv *httptest.Server) (*Tunnel, net.Conn) {
	t.Helper()
	tun := NewTunnel(NewConnectClient(ConnectTarget{Addr: addrOf(srv)}), "myshed", PortMapping{Local: 0, Remote: 8080})
	if err := tun.Start(); err != nil {
		t.Fatalf("start tunnel: %v", err)
	}
	c, err := net.Dial("tcp", tun.LocalAddr().String())
	if err != nil {
		tun.Stop()
		t.Fatalf("dial local listener: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return tun, c
}

// TestTunnelStopUnblocksOpenConnection covers the core hang: an established but
// idle tunneled connection must not wedge Stop(). Before the fix,
// BidirectionalCopy blocked on io.Copy and Stop()'s wg.Wait() never returned.
func TestTunnelStopUnblocksOpenConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n\r\n"))
		buf := make([]byte, 64)
		// Echo one message (proves the pipe is fully wired), then stay idle
		// until the peer closes (Stop closes the conns).
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	tun, c := startTunnel(t, srv)

	// A successful round-trip guarantees handleConn is inside BidirectionalCopy
	// in both directions — exactly the state that used to hang Stop().
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("round-trip: %v", err)
	}

	assertStopReturns(t, tun)
}

// TestTunnelStopUnblocksHangingUpgrade covers the second teardown gap: a backend
// that accepts the connection but never sends 101 wedges Dial's ReadResponse.
// The cancelable upgrade in ConnectClient.Dial must let Stop() tear it down.
func TestTunnelStopUnblocksHangingUpgrade(t *testing.T) {
	accepted := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		once.Do(func() { close(accepted) })
		// Never reply with 101; block until the client closes.
		buf := make([]byte, 64)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	tun, _ := startTunnel(t, srv)
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("backend never saw the upgrade attempt")
	}

	assertStopReturns(t, tun)
}
