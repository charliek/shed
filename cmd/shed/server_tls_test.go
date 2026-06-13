package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/servertls"
)

func TestPinnedTransport(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	pin := servertls.Fingerprint(srv.Certificate().Raw)

	t.Run("empty fingerprint yields default transport", func(t *testing.T) {
		if pinnedTransport("") != nil {
			t.Error("empty fingerprint should yield a nil (default) transport")
		}
	})
	t.Run("correct pin connects", func(t *testing.T) {
		c := &http.Client{Transport: pinnedTransport(pin)}
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatalf("pinned request failed: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})
	t.Run("wrong pin is rejected", func(t *testing.T) {
		c := &http.Client{Transport: pinnedTransport("sha256:" + strings.Repeat("00", 32))}
		if _, err := c.Get(srv.URL); err == nil {
			t.Error("a wrong pin must be rejected")
		}
	})
}

func TestTLSFingerprintEqual(t *testing.T) {
	const fp = "sha256:abcdef0123456789"
	cases := []struct {
		name, expected string
		want           bool
	}{
		{"identical", fp, true},
		{"missing prefix", "abcdef0123456789", true},
		{"uppercase + prefix", "SHA256:ABCDEF0123456789", true},
		{"surrounding whitespace", "  sha256:abcdef0123456789  ", true},
		{"different", "sha256:deadbeef", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := tlsFingerprintEqual(fp, tt.expected); got != tt.want {
				t.Errorf("tlsFingerprintEqual(%q, %q) = %v, want %v", fp, tt.expected, got, tt.want)
			}
		})
	}
}

func TestNormalizeTLSFingerprint(t *testing.T) {
	const want = "sha256:abc123"
	for _, in := range []string{"abc123", "ABC123", "sha256:abc123", " sha256:ABC123 "} {
		if got := normalizeTLSFingerprint(in); got != want {
			t.Errorf("normalizeTLSFingerprint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfirmTLSCert(t *testing.T) {
	const fp = "sha256:abcdef0123456789"

	// Args: (fingerprint, expected, trustOnFirstUse, interactive, jsonMode, firstUse).
	t.Run("matching expected verifies", func(t *testing.T) {
		if err := confirmTLSCert(fp, fp, false, false, false, true); err != nil {
			t.Errorf("matching fingerprint should succeed: %v", err)
		}
	})
	t.Run("mismatch fails closed", func(t *testing.T) {
		if err := confirmTLSCert(fp, "sha256:deadbeef", false, false, false, true); err == nil {
			t.Error("mismatched fingerprint must fail")
		}
	})
	t.Run("non-interactive first use trusts", func(t *testing.T) {
		if err := confirmTLSCert(fp, "", false, false, false, true); err != nil {
			t.Errorf("non-interactive first-use TOFU should succeed: %v", err)
		}
	})
	t.Run("non-interactive rotation refused without trust flag", func(t *testing.T) {
		// firstUse=false: a silent re-pin in a script would let a MITM repin.
		if err := confirmTLSCert(fp, "", false, false, false, false); err == nil {
			t.Error("non-interactive rotation without --trust-on-first-use must fail")
		}
	})
	t.Run("non-interactive rotation accepts with trust flag", func(t *testing.T) {
		if err := confirmTLSCert(fp, "", true, false, false, false); err != nil {
			t.Errorf("--trust-on-first-use should accept a rotation: %v", err)
		}
	})
	t.Run("json mode without trust flag fails closed", func(t *testing.T) {
		if err := confirmTLSCert(fp, "", false, false, true, true); err == nil {
			t.Error("json mode without --tls-fingerprint/--trust-on-first-use must fail")
		}
	})
	t.Run("trust-on-first-use accepts in json mode", func(t *testing.T) {
		if err := confirmTLSCert(fp, "", true, false, true, true); err != nil {
			t.Errorf("--trust-on-first-use should accept: %v", err)
		}
	})
}

func TestIsHTTPSURL(t *testing.T) {
	cases := map[string]bool{
		"https://host:8443": true,
		"HTTPS://host:8443": true,
		"  https://host  ":  true,
		"http://host:8080":  false,
		"":                  false,
		"host:8443":         false,
	}
	for in, want := range cases {
		if got := isHTTPSURL(in); got != want {
			t.Errorf("isHTTPSURL(%q) = %v, want %v", in, got, want)
		}
	}
}
