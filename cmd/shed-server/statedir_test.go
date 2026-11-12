package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestServerStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Unset keeps today's location: /etc/shed as root, ~/.shed otherwise.
	wantDefault := filepath.Join(home, ".shed")
	if os.Getuid() == 0 {
		wantDefault = "/etc/shed"
	}

	tests := []struct {
		name string
		cfg  *config.ServerConfig
		want string
	}{
		{name: "nil config uses the default", cfg: nil, want: wantDefault},
		{name: "unset uses the default", cfg: &config.ServerConfig{}, want: wantDefault},
		{name: "absolute is used as is", cfg: &config.ServerConfig{StateDir: "/var/lib/shed-dev/state"}, want: "/var/lib/shed-dev/state"},
		{name: "home-relative is expanded", cfg: &config.ServerConfig{StateDir: "~/.shed-dev"}, want: filepath.Join(home, ".shed-dev")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serverStateDir(tt.cfg); got != tt.want {
				t.Errorf("serverStateDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The TLS cert and the client CA follow state_dir, so a second server on the
// same host never resolves to the first one's identity files.
func TestIdentityPathsFollowStateDir(t *testing.T) {
	cfg := &config.ServerConfig{StateDir: "/var/lib/shed-dev/state"}
	dir := serverStateDir(cfg)

	cert, key := tlsCertPaths(cfg, dir)
	if cert != "/var/lib/shed-dev/state/tls_cert.pem" || key != "/var/lib/shed-dev/state/tls_key.pem" {
		t.Errorf("tlsCertPaths = %q, %q", cert, key)
	}
	caCert, caKey := caCertPaths(dir)
	if caCert != "/var/lib/shed-dev/state/ca_cert.pem" || caKey != "/var/lib/shed-dev/state/ca_key.pem" {
		t.Errorf("caCertPaths = %q, %q", caCert, caKey)
	}

	// tls_cert_file / tls_key_file still override the state dir.
	cfg.TLSCertFile, cfg.TLSKeyFile = "/etc/custom/cert.pem", "/etc/custom/key.pem"
	cert, key = tlsCertPaths(cfg, dir)
	if cert != "/etc/custom/cert.pem" || key != "/etc/custom/key.pem" {
		t.Errorf("tlsCertPaths with overrides = %q, %q", cert, key)
	}
}
