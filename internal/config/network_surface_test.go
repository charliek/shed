package config

import (
	"strings"
	"testing"
)

func TestListenAddrHelpers(t *testing.T) {
	tests := []struct {
		name              string
		cfg               ServerConfig
		wantHTTP, wantSSH string
	}{
		{
			name:     "defaults bind all interfaces",
			cfg:      ServerConfig{HTTPPort: 8080, SSHPort: 2222},
			wantHTTP: ":8080",
			wantSSH:  ":2222",
		},
		{
			name:     "http_bind restricts to loopback",
			cfg:      ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPBind: "127.0.0.1"},
			wantHTTP: "127.0.0.1:8080",
			wantSSH:  ":2222",
		},
		{
			name:     "ssh_bind restricts to a tailnet ip",
			cfg:      ServerConfig{HTTPPort: 8080, SSHPort: 2222, SSHBind: "100.64.0.1"},
			wantHTTP: ":8080",
			wantSSH:  "100.64.0.1:2222",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.HTTPListenAddr(); got != tt.wantHTTP {
				t.Errorf("HTTPListenAddr() = %q, want %q", got, tt.wantHTTP)
			}
			if got := tt.cfg.SSHListenAddr(); got != tt.wantSSH {
				t.Errorf("SSHListenAddr() = %q, want %q", got, tt.wantSSH)
			}
		})
	}
}

func TestValidateNetworkSurface(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ServerConfig
		wantErr bool
	}{
		{"https disabled (0) ok", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 0}, false},
		{"valid https port", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8443}, false},
		{"https out of range", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 70000}, true},
		{"https negative", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: -1}, true},
		{"https collides with http_port", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8080}, true},
		{"https collides with ssh_port", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 2222}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validateNetworkSurface()
			if (err != nil) != tt.wantErr {
				t.Errorf("validateNetworkSurface() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestRejectRemovedAuthKeys(t *testing.T) {
	// The removed-key scan must fail loudly on a stale public_exposure /
	// auth.http.tokens — silently ignoring public_exposure would un-loopback an
	// internet-facing plaintext listener (the v0.6.0 image-key silent-drift bug
	// this pattern exists to prevent).
	tests := []struct {
		name string
		yaml string
		want string // substring the error must contain; "" means expect no error
	}{
		{"clean secure config ok", "name: x\nauth:\n  mode: secure\n", ""},
		{"open config ok", "name: x\nauth:\n  mode: open\n", ""},
		{"no auth block ok", "name: x\n", ""},
		{"public_exposure rejected", "name: x\npublic_exposure: true\n", "public_exposure"},
		{"auth.http.tokens rejected", "name: x\nauth:\n  http:\n    tokens:\n      - {scope: control, token: abc}\n", "auth.http.tokens"},
		{"auth.http.mode rejected", "name: x\nauth:\n  http:\n    mode: enforce\n", "auth.http"},
		{"malformed yaml deferred to typed unmarshal", "name: [unterminated", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rejectRemovedAuthKeys([]byte(tt.yaml))
			if tt.want == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should name %q, got %v", tt.want, err)
			}
		})
	}
}

func TestRejectRemovedNetworkKeys(t *testing.T) {
	// internal_http_port was removed in v0.7.4 — a config still carrying it must
	// fail loudly rather than be silently ignored (it would otherwise imply a
	// loopback bus listener the new binary never starts).
	tests := []struct {
		name string
		yaml string
		want string // substring the error must contain; "" means expect no error
	}{
		{"clean config ok", "name: x\nhttp_port: 8080\n", ""},
		{"no network keys ok", "name: x\n", ""},
		{"internal_http_port rejected", "name: x\ninternal_http_port: 8081\n", "internal_http_port"},
		{"malformed yaml deferred to typed unmarshal", "name: [unterminated", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rejectRemovedNetworkKeys([]byte(tt.yaml))
			if tt.want == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should name %q, got %v", tt.want, err)
			}
		})
	}
}

func TestValidateAuth(t *testing.T) {
	// validateAuth is the config gate that rejects a malformed auth block before
	// the server ever binds — an invalid mode, an empty token, a bad scope, or a
	// malformed GitHub username must fail loudly at load, not silently disable a
	// control the operator believes is on.
	tests := []struct {
		name    string
		auth    *AuthConfig
		wantErr string // substring the error must name; "" means expect success
	}{
		{"nil auth block is allowed", nil, ""},
		{"empty ssh mode defaults ok", &AuthConfig{SSH: &SSHAuthConfig{}}, ""},
		{"ssh off ok", &AuthConfig{SSH: &SSHAuthConfig{Mode: SSHAuthOff}}, ""},
		{"ssh warn ok", &AuthConfig{SSH: &SSHAuthConfig{Mode: SSHAuthWarn}}, ""},
		{"secure + ssh enforce ok", &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce}}, ""},
		{"invalid ssh mode rejected", &AuthConfig{SSH: &SSHAuthConfig{Mode: "enfore"}}, "auth.ssh.mode"},
		{"negative max_auth_tries rejected", &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce, MaxAuthTries: -1}}, "max_auth_tries"},
		{"zero max_auth_tries ok", &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce, MaxAuthTries: 0}}, ""},
		{"valid github user ok", &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce, GitHubUsers: []string{"charliek"}}}, ""},
		{"malformed github user rejected", &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce, GitHubUsers: []string{"bad user!"}}}, "github_users"},
		{"invalid auth.mode rejected", &AuthConfig{Mode: "secur"}, "auth.mode"},
		{"secure auth.mode ok", &AuthConfig{Mode: AuthModeSecure}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: tt.auth}
			err := cfg.validateAuth()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validateAuth() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateAuth() = nil, want error naming %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validateAuth() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestHTTPSListenAddr(t *testing.T) {
	tests := []struct {
		name     string
		cfg      ServerConfig
		wantOn   bool
		wantAddr string
	}{
		{"disabled by default", ServerConfig{HTTPPort: 8080}, false, ""},
		{"all interfaces", ServerConfig{HTTPPort: 8080, HTTPSPort: 8443}, true, ":8443"},
		{"bound to one interface", ServerConfig{HTTPPort: 8080, HTTPSPort: 8443, HTTPBind: "100.64.0.1"}, true, "100.64.0.1:8443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.HTTPSEnabled(); got != tt.wantOn {
				t.Errorf("HTTPSEnabled() = %v, want %v", got, tt.wantOn)
			}
			if got := tt.cfg.HTTPSListenAddr(); got != tt.wantAddr {
				t.Errorf("HTTPSListenAddr() = %q, want %q", got, tt.wantAddr)
			}
		})
	}
}
