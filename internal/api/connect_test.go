package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// testBackend implements the subset of backend.Backend needed for Connect API tests.
// Methods not used by the handler panic to catch unexpected calls.
type testBackend struct {
	backend.Backend // embed to satisfy interface; unused methods panic
	dialFunc        func(ctx context.Context, shedName string, port uint16) (net.Conn, error)
}

func (b *testBackend) DialService(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
	return b.dialFunc(ctx, shedName, port)
}

func TestHandleConnectInvalidPort(t *testing.T) {
	srv := newTestServer()
	router := srv.Router()

	tests := []struct {
		name string
		port string
		code int
	}{
		{"zero", "0", http.StatusBadRequest},
		{"negative", "-1", http.StatusBadRequest},
		{"too large", "99999", http.StatusBadRequest},
		{"non-numeric", "abc", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/sheds/test/connect/"+tt.port, nil)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)

			if w.Code != tt.code {
				t.Errorf("status = %d, want %d", w.Code, tt.code)
			}
		})
	}
}

func TestHandleConnectShedNotFound(t *testing.T) {
	be := &testBackend{
		dialFunc: func(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotFoundSentinel, shedName)
		},
	}
	srv := NewServer(be, &config.ServerConfig{Name: "test", HTTPPort: 8080}, "", nil, nil)
	router := srv.Router()

	r := httptest.NewRequest(http.MethodGet, "/api/sheds/nonexistent/connect/8080", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleConnectShedNotRunning(t *testing.T) {
	be := &testBackend{
		dialFunc: func(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
			return nil, fmt.Errorf("%w: %s", config.ErrShedNotRunningSentinel, shedName)
		},
	}
	srv := NewServer(be, &config.ServerConfig{Name: "test", HTTPPort: 8080}, "", nil, nil)
	router := srv.Router()

	r := httptest.NewRequest(http.MethodGet, "/api/sheds/stopped/connect/8080", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// TestHandleConnectSuccess verifies the /api/sheds/{name}/connect/{port}
// HTTP upgrade handshake and bidirectional byte flow through
// vmutil.BidirectionalCopy.
//
// Known flake: this test has intermittently timed out on Linux CI (Go 1.24.x)
// when the server's io.Copy(pipe, TCP) goroutine inside BidirectionalCopy
// exits before propagating bytes — leaving the peer copy goroutine blocked
// forever on pipe.Read, because net.Pipe has no CloseWrite so the EOF
// signal from the first goroutine's closeWrite call is a no-op. Production
// code uses real TCP / vsock conns (both support CloseWrite), so this is a
// test-setup artifact, not a shed regression.
//
// The SetDeadline calls below cap any recurrence at ~5 seconds with a clear
// "i/o timeout" error instead of a 10-minute test timeout. If this flake
// recurs frequently, consider hardening BidirectionalCopy to fully Close
// the peer when one direction exits (careful: changes production half-close
// semantics for long-lived tunnels).
func TestHandleConnectSuccess(t *testing.T) {
	// Create a backend that returns a pipe for DialService.
	serverConn, clientSideConn := net.Pipe()
	t.Cleanup(func() { _ = clientSideConn.Close() })

	be := &testBackend{
		dialFunc: func(ctx context.Context, shedName string, port uint16) (net.Conn, error) {
			if shedName != "myvm" || port != 8080 {
				return nil, fmt.Errorf("unexpected: %s:%d", shedName, port)
			}
			return serverConn, nil
		},
	}
	srv := NewServer(be, &config.ServerConfig{Name: "test", HTTPPort: 8080}, "", nil, nil)

	// Start a real HTTP server (httptest.NewRecorder doesn't support Hijacker).
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)

	// Make a raw TCP connection to the test server.
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set tcp deadline: %v", err)
	}

	// Send the upgrade request.
	req := "GET /api/sheds/myvm/connect/8080 HTTP/1.1\r\n" +
		"Host: " + ts.Listener.Addr().String() + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: shed-tcp\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Read the response.
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}
	if resp.Header.Get("Upgrade") != "shed-tcp" {
		t.Errorf("Upgrade header = %q, want %q", resp.Header.Get("Upgrade"), "shed-tcp")
	}

	// Verify bidirectional data flow. Writing 20 bytes fits in the kernel
	// send buffer, so the Write does not block and we can call both
	// synchronously — avoids a scheduling race the prior goroutine form had.
	testData := "hello through tunnel"
	if _, err := conn.Write([]byte(testData)); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}

	if err := clientSideConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set pipe deadline: %v", err)
	}
	received := make([]byte, len(testData))
	n, err := io.ReadFull(clientSideConn, received)
	if err != nil {
		t.Fatalf("read from VM side: %v", err)
	}
	if string(received[:n]) != testData {
		t.Errorf("received = %q, want %q", string(received[:n]), testData)
	}
}
