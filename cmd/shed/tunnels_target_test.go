package main

import (
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestConnectTargetFromEntry(t *testing.T) {
	t.Run("plain entry → host:http_port, no pin/token", func(t *testing.T) {
		entry := &config.ServerEntry{Host: "host.example", HTTPPort: 8080}
		// The plain-TCP path ignores the token argument — nothing carries it.
		got, err := connectTargetFromEntry(entry, "ignored-on-plain")
		if err != nil {
			t.Fatal(err)
		}
		if got.Addr != "host.example:8080" || got.TLSPin != "" || got.Token != "" {
			t.Errorf("plain entry = %+v, want addr=host.example:8080 no pin/token", got)
		}
	})

	t.Run("https api_url + pin → tls target with caller-supplied token", func(t *testing.T) {
		entry := &config.ServerEntry{
			Host: "host.example", HTTPPort: 8080,
			APIURL:             "https://host.example:8443",
			TLSCertFingerprint: "sha256:abc123",
			ControlToken:       "shed_control_stale", // must be ignored: the caller passes the live token
		}
		got, err := connectTargetFromEntry(entry, "shed_control_live")
		if err != nil {
			t.Fatal(err)
		}
		if got.Addr != "host.example:8443" {
			t.Errorf("addr = %q, want host.example:8443", got.Addr)
		}
		if got.TLSPin != "sha256:abc123" {
			t.Errorf("pin = %q, want sha256:abc123", got.TLSPin)
		}
		// The target carries the caller's live token, not entry.ControlToken —
		// so a refresh-in-flight isn't dropped for the possibly-stale config copy.
		if got.Token != "shed_control_live" {
			t.Errorf("token = %q, want the caller-supplied live token", got.Token)
		}
	})

	t.Run("https api_url without a pin fails closed", func(t *testing.T) {
		entry := &config.ServerEntry{
			Host: "host.example", HTTPPort: 8080,
			APIURL: "https://host.example:8443", // no tls_cert_fingerprint
		}
		if _, err := connectTargetFromEntry(entry, "tok"); err == nil {
			t.Error("an https api_url without a pin must error, not dial unpinned")
		}
	})

	t.Run("https api_url without a port errors upfront", func(t *testing.T) {
		entry := &config.ServerEntry{
			Host: "host.example", HTTPPort: 8080,
			APIURL: "https://host.example", TLSCertFingerprint: "sha256:abc",
		}
		if _, err := connectTargetFromEntry(entry, "tok"); err == nil {
			t.Error("a port-less https api_url should error in resolution, not at dial time")
		}
	})

	t.Run("non-https api_url falls back to plain", func(t *testing.T) {
		entry := &config.ServerEntry{
			Host: "host.example", HTTPPort: 8080,
			APIURL: "http://host.example:8080",
		}
		got, err := connectTargetFromEntry(entry, "ignored-on-plain")
		if err != nil {
			t.Fatal(err)
		}
		if got.Addr != "host.example:8080" || got.TLSPin != "" || got.Token != "" {
			t.Errorf("non-https api_url = %+v, want the plain host:port path with no token", got)
		}
	})
}
