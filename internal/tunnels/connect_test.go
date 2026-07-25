package tunnels

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
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
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin, Token: token}, nil)

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

	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: "sha256:" + strings.Repeat("00", 32)}, nil)
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
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, nil)
	if _, err := client.Dial(context.Background(), "myshed", 8080); err == nil {
		t.Error("a missing token must be rejected by the connect route")
	}
}

func TestConnectClientDialPlain(t *testing.T) {
	// No pin, no token: the legacy plain-TCP path, byte-identical.
	srv := httptest.NewServer(upgradeEchoHandler(t, ""))
	defer srv.Close()

	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv)}, nil)
	conn, err := client.Dial(context.Background(), "myshed", 8080)
	if err != nil {
		t.Fatalf("Dial plain: %v", err)
	}
	defer conn.Close()
	roundTrip(t, conn)
}

// TestConnectClientDialRefreshesOn401 proves a tunnel with a refreshing source
// survives token expiry: the first upgrade (stale token) 401s, the client
// re-mints once and re-dials, and the second upgrade (fresh token) reaches 101.
func TestConnectClientDialRefreshesOn401(t *testing.T) {
	const newTok = "shed_control_new"
	var mints int32
	srv := httptest.NewTLSServer(upgradeEchoHandler(t, newTok)) // only "Bearer new" upgrades
	defer srv.Close()

	pin := servertls.Fingerprint(srv.Certificate().Raw)
	src := clienttoken.New(clienttoken.TokenCredential("shed_control_old", time.Now().Add(24*time.Hour)), func() (clienttoken.Credential, error) {
		atomic.AddInt32(&mints, 1)
		return clienttoken.TokenCredential(newTok, time.Now().Add(24*time.Hour)), nil
	})
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, src)

	conn, err := client.Dial(context.Background(), "myshed", 8080)
	if err != nil {
		t.Fatalf("Dial should succeed after 401 → refresh → re-dial: %v", err)
	}
	defer conn.Close()
	roundTrip(t, conn)
	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Errorf("mints = %d, want 1 (one re-mint on the 401)", got)
	}
}

// TestConnectClientConcurrentDialsCoalesceRefresh drives the real tunnel-layer
// concurrency: N port tunnels share one ConnectClient/source, all 401 with the
// stale token at once, and must coalesce to a single re-mint. Run with -race.
func TestConnectClientConcurrentDialsCoalesceRefresh(t *testing.T) {
	const newTok = "shed_control_new"
	var mints int32
	srv := httptest.NewTLSServer(upgradeEchoHandler(t, newTok))
	defer srv.Close()

	pin := servertls.Fingerprint(srv.Certificate().Raw)
	src := clienttoken.New(clienttoken.TokenCredential("shed_control_old", time.Now().Add(24*time.Hour)), func() (clienttoken.Credential, error) {
		atomic.AddInt32(&mints, 1)
		return clienttoken.TokenCredential(newTok, time.Now().Add(24*time.Hour)), nil
	})
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, src)

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := client.Dial(context.Background(), "myshed", 8080)
			if err != nil {
				t.Errorf("concurrent Dial: %v", err)
				return
			}
			roundTrip(t, conn)
			_ = conn.Close()
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&mints); got != 1 {
		t.Errorf("mints = %d across %d concurrent dials, want 1 (coalesced)", got, n)
	}
}

// TestConnectClientPlainNeverSendsToken: the unpinned/plain path must not put a
// bearer token on the wire even when a token-bearing source is (mis)configured.
func TestConnectClientPlainNeverSendsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("plain path sent Authorization %q; a token must never travel over plaintext", auth)
		}
		http.Error(w, "no upgrade", http.StatusOK) // any non-101; we only care about the header
	}))
	defer srv.Close()

	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv)}, clienttoken.Static("must-not-be-sent"))
	_, _ = client.Dial(context.Background(), "myshed", 8080) // fails (non-101); that's fine
}

// TestConnectClientPlainNeverSendsTokenOnRetry: even the 401 re-mint+re-dial path
// must not put a bearer on the wire for an unpinned target (a refreshable source
// exercises the retry, unlike the Static source above).
func TestConnectClientPlainNeverSendsTokenOnRetry(t *testing.T) {
	var sawAuth int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			atomic.StoreInt32(&sawAuth, 1)
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized) // always 401 → drives refresh + retry
	}))
	defer srv.Close()

	src := clienttoken.New(clienttoken.TokenCredential("old", time.Now().Add(24*time.Hour)), func() (clienttoken.Credential, error) {
		return clienttoken.TokenCredential("new", time.Now().Add(24*time.Hour)), nil
	})
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv)}, src) // plain + refreshable
	_, _ = client.Dial(context.Background(), "myshed", 8080)          // fails after retry; that's fine
	if atomic.LoadInt32(&sawAuth) == 1 {
		t.Error("plain path sent a bearer token on the 401 retry; a token must never travel over plaintext")
	}
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
			client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin, Token: token}, nil)
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
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin, Token: "t"}, nil)
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

	if err := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin, Token: token}, nil).Probe(ctx, "myshed"); err != nil {
		t.Errorf("probe with valid token: %v", err)
	}
	if err := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin}, nil).Probe(ctx, "myshed"); err == nil {
		t.Error("probe without a token must fail (401)")
	}
}

// TestConnectProbePlain: open mode (no pin, no token) — connect/0 400s → pass.
func TestConnectProbePlain(t *testing.T) {
	srv := httptest.NewServer(probeStatusHandler(t, "", http.StatusBadRequest))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := NewConnectClient(ConnectTarget{Addr: addrOf(srv)}, nil).Probe(ctx, "myshed"); err != nil {
		t.Errorf("plain probe: %v", err)
	}
}

func TestConnectProbeWrongPin(t *testing.T) {
	srv := httptest.NewTLSServer(probeStatusHandler(t, "", http.StatusBadRequest))
	defer srv.Close()
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: "sha256:" + strings.Repeat("00", 32), Token: "t"}, nil)
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
	client := NewConnectClient(ConnectTarget{Addr: addr}, nil)
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
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin, Token: "t"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := client.Probe(ctx, "myshed"); err == nil {
		t.Error("probe must fail when the server stalls past the context deadline")
	}
}
