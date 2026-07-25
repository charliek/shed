package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// infoHandler serves a minimal /api/info, the only endpoint selectAddTransport hits.
func infoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/info" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(config.ServerInfo{
			Name: "test-server", HTTPPort: 8080, SSHPort: 2222, Backend: "vz", AuthMode: config.AuthModeToken,
		})
	})
}

func hostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port from %q: %v", rawURL, err)
	}
	return u.Hostname(), port
}

// freeClosedPort returns a port with nothing listening, so a dial is refused.
func freeClosedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// withCleanAddFlags resets the package-level `shed server add` flags for a
// subtest and restores them afterward (they are globals bound to cobra flags).
func withCleanAddFlags(t *testing.T) {
	t.Helper()
	oHTTPS, oSecure, oPort, oTOFU, oFP, oDefault, oJSON :=
		serverAddHTTPSPort, serverAddSecure, serverAddPort, serverAddTrustTOFU,
		serverAddTLSFingerprint, defaultAddHTTPSPort, jsonFlag
	serverAddHTTPSPort = 0
	serverAddSecure = false
	serverAddPort = 8080
	serverAddTrustTOFU = false
	serverAddTLSFingerprint = ""
	jsonFlag = false
	t.Cleanup(func() {
		serverAddHTTPSPort, serverAddSecure, serverAddPort, serverAddTrustTOFU,
			serverAddTLSFingerprint, defaultAddHTTPSPort, jsonFlag =
			oHTTPS, oSecure, oPort, oTOFU, oFP, oDefault, oJSON
	})
}

func TestSelectAddTransport(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(infoHandler())
	defer tlsSrv.Close()
	tlsHost, tlsPort := hostPort(t, tlsSrv.URL)
	tlsURL := "https://" + net.JoinHostPort(tlsHost, strconv.Itoa(tlsPort))

	plainSrv := httptest.NewServer(infoHandler())
	defer plainSrv.Close()
	plainHost, plainPort := hostPort(t, plainSrv.URL)

	t.Run("explicit --https-port bootstraps TLS", func(t *testing.T) {
		withCleanAddFlags(t)
		serverAddHTTPSPort = tlsPort
		serverAddTrustTOFU = true
		client, apiURL, fp, info, err := selectAddTransport(tlsHost)
		if err != nil {
			t.Fatalf("selectAddTransport: %v", err)
		}
		if client == nil || info == nil || info.Name != "test-server" {
			t.Fatalf("client/info not populated: %+v", info)
		}
		if apiURL != tlsURL {
			t.Errorf("apiURL = %q, want %q", apiURL, tlsURL)
		}
		if fp == "" {
			t.Error("expected a pinned TLS fingerprint")
		}
	})

	t.Run("plain HTTP server uses plain transport", func(t *testing.T) {
		withCleanAddFlags(t)
		serverAddPort = plainPort
		client, apiURL, fp, info, err := selectAddTransport(plainHost)
		if err != nil {
			t.Fatalf("selectAddTransport: %v", err)
		}
		if client == nil || info == nil {
			t.Fatal("expected client + info for a reachable plain server")
		}
		if apiURL != "" || fp != "" {
			t.Errorf("expected plain transport (no apiURL/fingerprint), got apiURL=%q fp=%q", apiURL, fp)
		}
	})

	t.Run("plain refused falls back to the default TLS port", func(t *testing.T) {
		withCleanAddFlags(t)
		serverAddPort = freeClosedPort(t) // nothing listening → connection refused
		defaultAddHTTPSPort = tlsPort     // the fallback target
		serverAddTrustTOFU = true
		client, apiURL, fp, info, err := selectAddTransport(tlsHost)
		if err != nil {
			t.Fatalf("expected TLS fallback to succeed: %v", err)
		}
		if client == nil || info == nil {
			t.Fatal("expected client + info from the fallback")
		}
		if apiURL != tlsURL {
			t.Errorf("apiURL = %q, want TLS fallback %q", apiURL, tlsURL)
		}
		if fp == "" {
			t.Error("expected a pinned fingerprint from the fallback")
		}
	})

	t.Run("--secure bootstraps TLS on the default port", func(t *testing.T) {
		withCleanAddFlags(t)
		serverAddSecure = true
		defaultAddHTTPSPort = tlsPort
		serverAddTrustTOFU = true
		_, apiURL, fp, info, err := selectAddTransport(tlsHost)
		if err != nil {
			t.Fatalf("selectAddTransport: %v", err)
		}
		if info == nil || apiURL != tlsURL || fp == "" {
			t.Errorf("expected TLS bootstrap on default port: apiURL=%q fp=%q info=%v", apiURL, fp, info)
		}
	})

	t.Run("plain server error is surfaced, no TLS fallback", func(t *testing.T) {
		errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer errSrv.Close()
		_, errPort := hostPort(t, errSrv.URL)

		withCleanAddFlags(t)
		serverAddPort = errPort       // reachable, but responds 500 (not unreachable)
		defaultAddHTTPSPort = tlsPort // a working TLS server: a wrong fallback would succeed
		serverAddTrustTOFU = true
		_, apiURL, _, _, err := selectAddTransport("127.0.0.1")
		if err == nil {
			t.Fatal("expected the plain 500 to surface, not a silent TLS fallback")
		}
		if apiURL != "" {
			t.Errorf("must not fall back to TLS on a server error; apiURL=%q", apiURL)
		}
	})
}
