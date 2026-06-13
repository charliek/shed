package sdk

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func tlsTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCertFingerprintFormat(t *testing.T) {
	fp := certFingerprint([]byte("hello"))
	hexPart, ok := strings.CutPrefix(fp, "sha256:")
	if !ok {
		t.Fatalf("missing sha256: prefix: %q", fp)
	}
	if len(hexPart) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(hexPart))
	}
	sum := sha256.Sum256([]byte("hello"))
	if hexPart != hex.EncodeToString(sum[:]) {
		t.Error("fingerprint hex does not match sha256 of input")
	}
}

func TestWithTLSPinGoodCert(t *testing.T) {
	srv := tlsTestServer(t)
	fp := certFingerprint(srv.Certificate().Raw)

	c := NewHostClient(WithServerURL(srv.URL), WithTLSPin(fp))
	resp, err := c.httpClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("pinned request to the TLS server failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestWithTLSPinWrongCert(t *testing.T) {
	srv := tlsTestServer(t)

	c := NewHostClient(WithServerURL(srv.URL), WithTLSPin("sha256:"+strings.Repeat("00", 32)))
	if _, err := c.httpClient.Get(srv.URL); err == nil {
		t.Error("a wrong pin must reject the connection")
	}
}

func TestWithTLSPinComposesWithHTTPClient(t *testing.T) {
	srv := tlsTestServer(t)
	fp := certFingerprint(srv.Certificate().Raw)

	custom := &http.Client{Timeout: 42 * time.Second}
	// Option order is deliberately pin-before-client to prove order-independence
	// (the pin is applied in NewHostClient's finalize step).
	c := NewHostClient(WithServerURL(srv.URL), WithTLSPin(fp), WithHTTPClient(custom))

	if c.httpClient.Timeout != 42*time.Second {
		t.Errorf("custom client Timeout not preserved: got %v, want 42s", c.httpClient.Timeout)
	}
	if custom.Transport != nil {
		t.Error("WithTLSPin must not mutate the caller's *http.Client")
	}
	resp, err := c.httpClient.Get(srv.URL)
	if err != nil {
		t.Fatalf("pinned request via composed client failed: %v", err)
	}
	resp.Body.Close()
}

func TestNoPinLeavesClientUnchanged(t *testing.T) {
	custom := &http.Client{Timeout: 7 * time.Second}
	c := NewHostClient(WithHTTPClient(custom))
	if c.httpClient != custom {
		t.Error("without WithTLSPin the supplied client should be used as-is")
	}
}
