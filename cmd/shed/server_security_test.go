package main

import (
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestSecurityLabel(t *testing.T) {
	tests := []struct {
		name  string
		entry config.ServerEntry
		want  string
	}{
		{
			name:  "secure: https api_url with pinned cert",
			entry: config.ServerEntry{APIURL: "https://localhost:8443", TLSCertFingerprint: "sha256:abc"},
			want:  "secure",
		},
		{
			name:  "unpinned: https api_url, no fingerprint",
			entry: config.ServerEntry{APIURL: "https://localhost:8443"},
			want:  "unpinned",
		},
		{
			name:  "open: no api_url (plain http via host:port)",
			entry: config.ServerEntry{Host: "mini2", HTTPPort: 8080},
			want:  "open",
		},
		{
			name:  "open: explicit http api_url",
			entry: config.ServerEntry{APIURL: "http://mini2:8080"},
			want:  "open",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := securityLabel(tt.entry); got != tt.want {
				t.Errorf("securityLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHTTPSPortFromAPIURL(t *testing.T) {
	tests := []struct {
		name   string
		apiURL string
		want   int
	}{
		{name: "explicit https port", apiURL: "https://localhost:8443", want: 8443},
		{name: "https without port defaults to 443", apiURL: "https://example.com", want: 443},
		{name: "plain http yields 0", apiURL: "http://mini2:8080", want: 0},
		{name: "empty yields 0", apiURL: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := httpsPortFromAPIURL(tt.apiURL); got != tt.want {
				t.Errorf("httpsPortFromAPIURL(%q) = %d, want %d", tt.apiURL, got, tt.want)
			}
		})
	}
}
