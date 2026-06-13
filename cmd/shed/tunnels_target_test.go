package main

import (
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestConnectTargetFromEntry(t *testing.T) {
	t.Run("plain entry → host:http_port, no pin/token", func(t *testing.T) {
		entry := &config.ServerEntry{Host: "host.example", HTTPPort: 8080}
		got, err := connectTargetFromEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if got.Addr != "host.example:8080" || got.TLSPin != "" || got.Token != "" {
			t.Errorf("plain entry = %+v, want addr=host.example:8080 no pin/token", got)
		}
	})

	t.Run("https api_url + pin → tls target with creds token", func(t *testing.T) {
		entry := &config.ServerEntry{
			Host: "host.example", HTTPPort: 8080,
			APIURL:             "https://host.example:8443",
			TLSCertFingerprint: "sha256:abc123",
			CredentialsToken:   "shed_credentials_xyz",
		}
		got, err := connectTargetFromEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if got.Addr != "host.example:8443" {
			t.Errorf("addr = %q, want host.example:8443", got.Addr)
		}
		if got.TLSPin != "sha256:abc123" {
			t.Errorf("pin = %q, want sha256:abc123", got.TLSPin)
		}
		if got.Token != "shed_credentials_xyz" {
			t.Errorf("token = %q, want the credentials token", got.Token)
		}
	})

	t.Run("https api_url without a pin fails closed", func(t *testing.T) {
		entry := &config.ServerEntry{
			Host: "host.example", HTTPPort: 8080,
			APIURL: "https://host.example:8443", // no tls_cert_fingerprint
		}
		if _, err := connectTargetFromEntry(entry); err == nil {
			t.Error("an https api_url without a pin must error, not dial unpinned")
		}
	})

	t.Run("https api_url without a port errors upfront", func(t *testing.T) {
		entry := &config.ServerEntry{
			Host: "host.example", HTTPPort: 8080,
			APIURL: "https://host.example", TLSCertFingerprint: "sha256:abc",
		}
		if _, err := connectTargetFromEntry(entry); err == nil {
			t.Error("a port-less https api_url should error in resolution, not at dial time")
		}
	})

	t.Run("non-https api_url falls back to plain", func(t *testing.T) {
		entry := &config.ServerEntry{
			Host: "host.example", HTTPPort: 8080,
			APIURL: "http://host.example:8080",
		}
		got, err := connectTargetFromEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if got.Addr != "host.example:8080" || got.TLSPin != "" {
			t.Errorf("non-https api_url = %+v, want the plain host:port path", got)
		}
	})
}
