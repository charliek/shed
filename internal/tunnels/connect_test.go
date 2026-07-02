package tunnels

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

// upgradeEchoHandler answers the Connect upgrade with 101, then echoes bytes
// over the hijacked connection. If wantToken is non-empty it requires it.
func upgradeEchoHandler(t *testing.T, wantToken string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if wantToken != "" && r.Header.Get("Authorization") != "Bearer "+wantToken {
			http.Error(w, "missing/invalid token", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Upgrade") != "shed-tcp" {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: shed-tcp\r\nConnection: Upgrade\r\n\r\n"))
		buf := make([]byte, 256)
		n, _ := conn.Read(buf)
		_, _ = conn.Write(buf[:n]) // echo
	}
}

func addrOf(srv *httptest.Server) string {
	return strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://")
}

func roundTrip(t *testing.T, conn io.ReadWriter) {
	t.Helper()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("echo = %q, want ping", got)
	}
}

func TestConnectClientDialTLSPinned(t *testing.T) {
	const token = "shed_control_test"
	srv := httptest.NewTLSServer(upgradeEchoHandler(t, token))
	defer srv.Close()

	pin := servertls.Fingerprint(srv.Certificate().Raw)
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin, Token: token})

	conn, err := client.Dial(context.Background(), "myshed", 8080)
	if err != nil {
		t.Fatalf("Dial over pinned TLS: %v", err)
	}
	defer conn.Close()
	roundTrip(t, conn)
}

func TestConnectClientDialWrongPin(t *testing.T) {
	srv := httptest.NewTLSServer(upgradeEchoHandler(t, ""))
	defer srv.Close()

	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: "sha256:" + strings.Repeat("00", 32)})
	if _, err := client.Dial(context.Background(), "myshed", 8080); err == nil {
		t.Error("a wrong pin must fail the TLS handshake")
	}
}

func TestConnectClientDialMissingToken(t *testing.T) {
	const token = "shed_control_test"
	srv := httptest.NewTLSServer(upgradeEchoHandler(t, token))
	defer srv.Close()

	pin := servertls.Fingerprint(srv.Certificate().Raw)
	// Pinned but no token → the server rejects the upgrade (not 101).
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin})
	if _, err := client.Dial(context.Background(), "myshed", 8080); err == nil {
		t.Error("a missing token must be rejected by the connect route")
	}
}

func TestConnectClientDialPlain(t *testing.T) {
	// No pin, no token: the legacy plain-TCP path, byte-identical.
	srv := httptest.NewServer(upgradeEchoHandler(t, ""))
	defer srv.Close()

	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv)})
	conn, err := client.Dial(context.Background(), "myshed", 8080)
	if err != nil {
		t.Fatalf("Dial plain: %v", err)
	}
	defer conn.Close()
	roundTrip(t, conn)
}

// probeStatusHandler asserts the probe targets the exact connect/0 route and
// returns status. If wantToken is set, a missing/mismatched bearer yields 401
// (mirroring the real auth middleware); otherwise status is returned. A 400 is
// written as the shed INVALID_PORT error envelope, mirroring handleConnect, so
// the probe's error-code check treats it as "authenticated".
func probeStatusHandler(t *testing.T, wantToken string, status int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/sheds/myshed/connect/0" {
			t.Errorf("probe hit %s %s, want GET /api/sheds/myshed/connect/0", r.Method, r.URL.Path)
		}
		if wantToken != "" && r.Header.Get("Authorization") != "Bearer "+wantToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if status == http.StatusBadRequest {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(config.APIError{Error: config.APIErrorDetail{Code: "INVALID_PORT", Message: "port must be 1-65535"}})
			return
		}
		http.Error(w, "probe", status)
	}
}

func TestConnectProbeStatusHandling(t *testing.T) {
	const token = "shed_control_test"
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"400 INVALID_PORT means authenticated", http.StatusBadRequest, false},
		{"401 token missing/invalid", http.StatusUnauthorized, true},
		{"403 scope not accepted / old server", http.StatusForbidden, true},
		{"unexpected 500", http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(probeStatusHandler(t, "", tt.status))
			defer srv.Close()
			pin := servertls.Fingerprint(srv.Certificate().Raw)
			client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin, Token: token})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := client.Probe(ctx, "myshed")
			if tt.wantErr && err == nil {
				t.Errorf("status %d: want error, got nil", tt.status)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("status %d: want nil, got %v", tt.status, err)
			}
		})
	}
}

// TestConnectProbeGeneric400 proves a 400 that is NOT the shed INVALID_PORT
// envelope (e.g. from a proxy or wrong service) fails the probe rather than
// false-passing as "authenticated".
func TestConnectProbeGeneric400(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request from some middlebox", http.StatusBadRequest) // plain text, no shed envelope
	}))
	defer srv.Close()
	pin := servertls.Fingerprint(srv.Certificate().Raw)
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin, Token: "t"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Probe(ctx, "myshed"); err == nil {
		t.Error("a generic 400 (not INVALID_PORT) must fail the probe, not pass")
	}
}

// TestConnectProbeToken proves the probe sends the target token on the connect/0
// route: with the right token the server 400s (pass); without it, 401s (fail).
func TestConnectProbeToken(t *testing.T) {
	const token = "shed_control_test"
	srv := httptest.NewTLSServer(probeStatusHandler(t, token, http.StatusBadRequest))
	defer srv.Close()
	pin := servertls.Fingerprint(srv.Certificate().Raw)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin, Token: token}).Probe(ctx, "myshed"); err != nil {
		t.Errorf("probe with valid token: %v", err)
	}
	if err := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}).Probe(ctx, "myshed"); err == nil {
		t.Error("probe without a token must fail (401)")
	}
}

// TestConnectProbePlain: open mode (no pin, no token) — connect/0 400s → pass.
func TestConnectProbePlain(t *testing.T) {
	srv := httptest.NewServer(probeStatusHandler(t, "", http.StatusBadRequest))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := NewConnectClient(ConnectTarget{Addr: addrOf(srv)}).Probe(ctx, "myshed"); err != nil {
		t.Errorf("plain probe: %v", err)
	}
}

func TestConnectProbeWrongPin(t *testing.T) {
	srv := httptest.NewTLSServer(probeStatusHandler(t, "", http.StatusBadRequest))
	defer srv.Close()
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: "sha256:" + strings.Repeat("00", 32), Token: "t"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Probe(ctx, "myshed"); err == nil {
		t.Error("probe must fail the TLS handshake on a wrong pin")
	}
}

func TestConnectProbeUnreachable(t *testing.T) {
	srv := httptest.NewServer(probeStatusHandler(t, "", http.StatusBadRequest))
	addr := addrOf(srv)
	srv.Close() // nothing listening now
	client := NewConnectClient(ConnectTarget{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Probe(ctx, "myshed"); err == nil {
		t.Error("probe must fail when the server is unreachable")
	}
}

func TestConnectProbeContextDeadline(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // stall until the test unblocks us
	}))
	defer srv.Close()
	defer close(block) // LIFO: runs before srv.Close() so the handler returns and Close() doesn't hang

	pin := servertls.Fingerprint(srv.Certificate().Raw)
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin, Token: "t"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := client.Probe(ctx, "myshed"); err == nil {
		t.Error("probe must fail when the server stalls past the context deadline")
	}
}
