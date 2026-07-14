//go:build linux
// +build linux

package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/vmutil"
)

// fakeVsockUDS is a fake Firecracker vsock UDS server. It speaks the stacked
// handshakes that Client.DialService now traverses: first the Firecracker mux
// ("CONNECT <muxPort>\n" → "OK <hostport>\n"), then the guest tcpproxy
// ("CONNECT <servicePort>\n" → tcpProxyResp). If coalesce is set, both response
// lines are written in a single Write to reproduce the coalesced-response case
// that the reader-reuse in vmutil.DialService must survive.
type fakeVsockUDS struct {
	path         string
	ln           net.Listener
	tcpProxyResp string // e.g. "OK\n" or "ERR dial 127.0.0.1:1029: connection refused\n"
	coalesce     bool
	echo         bool

	mu       sync.Mutex
	gotMux   uint32
	gotSvc   uint16
	handshOK bool
}

func newFakeVsockUDS(t *testing.T, tcpProxyResp string, coalesce bool) *fakeVsockUDS {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vsock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	f := &fakeVsockUDS{path: path, ln: ln, tcpProxyResp: tcpProxyResp, coalesce: coalesce}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeVsockUDS) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeVsockUDS) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)

	// Mux handshake.
	muxLine, err := r.ReadString('\n')
	if err != nil {
		return
	}
	muxPort, ok := parseConnect(muxLine)
	if !ok {
		return
	}
	f.mu.Lock()
	f.gotMux = muxPort
	f.mu.Unlock()

	if f.coalesce {
		// Write the mux OK and the tcpproxy response in a single write BEFORE
		// reading the tcpproxy CONNECT, so the client's mux reader buffers the
		// tcpproxy line. The client's subsequent CONNECT write is drained below.
		if _, err := conn.Write([]byte("OK 0\n" + f.tcpProxyResp)); err != nil {
			return
		}
		if _, err := r.ReadString('\n'); err != nil { // tcpproxy CONNECT
			return
		}
	} else {
		if _, err := conn.Write([]byte("OK 0\n")); err != nil {
			return
		}
		svcLine, err := r.ReadString('\n')
		if err != nil {
			return
		}
		svcPort, ok := parseConnect(svcLine)
		if !ok {
			return
		}
		f.mu.Lock()
		f.gotSvc = uint16(svcPort)
		f.mu.Unlock()
		if _, err := conn.Write([]byte(f.tcpProxyResp)); err != nil {
			return
		}
	}

	f.mu.Lock()
	f.handshOK = true
	f.mu.Unlock()

	if !f.echo {
		return
	}
	// Echo remaining bytes back so the caller can prove data flows.
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// parseConnect parses "CONNECT <port>\n".
func parseConnect(line string) (uint32, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "CONNECT ") {
		return 0, false
	}
	var p uint32
	if _, err := fmt.Sscanf(strings.TrimPrefix(line, "CONNECT "), "%d", &p); err != nil {
		return 0, false
	}
	return p, true
}

// TestFirecrackerDialServiceStacked exercises the FirecrackerDialer +
// vmutil.DialService composition — the exact path Client.DialService takes,
// minus the metadata/running-status plumbing — against a fake UDS server that
// speaks both the mux and tcpproxy handshakes.
func TestFirecrackerDialServiceStacked(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		f := newFakeVsockUDS(t, "OK\n", false)
		f.echo = true
		dialer := NewFirecrackerDialer(f.path)
		conn, err := vmutil.DialService(context.Background(), dialer, 1028, 1029)
		if err != nil {
			t.Fatalf("DialService: %v", err)
		}
		defer conn.Close()

		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, 4)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read echo: %v", err)
		}
		if string(got) != "ping" {
			t.Errorf("echo = %q, want ping", got)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.handshOK {
			t.Error("server did not complete stacked handshake")
		}
		if f.gotSvc != 1029 {
			t.Errorf("service port = %d, want 1029", f.gotSvc)
		}
	})

	t.Run("coalesced_ok", func(t *testing.T) {
		// Mux OK and tcpproxy OK arrive in one write; the mux reader buffers
		// the tcpproxy line. DialService must reuse that reader, not re-wrap.
		f := newFakeVsockUDS(t, "OK\n", true)
		dialer := NewFirecrackerDialer(f.path)
		conn, err := vmutil.DialService(context.Background(), dialer, 1028, 1029)
		if err != nil {
			t.Fatalf("DialService (coalesced): %v", err)
		}
		conn.Close()
	})

	t.Run("tcpproxy_err_surfaces", func(t *testing.T) {
		f := newFakeVsockUDS(t, "ERR dial 127.0.0.1:1029: connection refused\n", false)
		dialer := NewFirecrackerDialer(f.path)
		_, err := vmutil.DialService(context.Background(), dialer, 1028, 1029)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("error = %q, want it to contain connection refused", err.Error())
		}
	})

	t.Run("uds_not_present", func(t *testing.T) {
		dialer := NewFirecrackerDialer(filepath.Join(t.TempDir(), "missing.vsock"))
		_, err := vmutil.DialService(context.Background(), dialer, 1028, 1029)
		if err == nil {
			t.Fatal("expected error dialing a missing UDS, got nil")
		}
	})
}
