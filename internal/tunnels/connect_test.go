package tunnels

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	const token = "shed_credentials_test"
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
	const token = "shed_credentials_test"
	srv := httptest.NewTLSServer(upgradeEchoHandler(t, token))
	defer srv.Close()

	pin := servertls.Fingerprint(srv.Certificate().Raw)
	// Pinned but no token → the server rejects the upgrade (not 101).
	client := NewConnectClient(ConnectTarget{Addr: addrOf(srv), TLSPin: pin})
	if _, err := client.Dial(context.Background(), "myshed", 8080); err == nil {
		t.Error("a missing credentials token must be rejected by the connect route")
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
