package config

import (
	"strings"
	"testing"
)

func TestListenAddrHelpers(t *testing.T) {
	// bind_address is shared by every listener and defaults to loopback.
	tests := []struct {
		name              string
		cfg               ServerConfig
		wantHTTP, wantSSH string
	}{
		{
			name:     "unset binds loopback on both listeners",
			cfg:      ServerConfig{HTTPPort: 8080, SSHPort: 2222},
			wantHTTP: "127.0.0.1:8080",
			wantSSH:  "127.0.0.1:2222",
		},
		{
			name:     "explicit loopback",
			cfg:      ServerConfig{HTTPPort: 8080, SSHPort: 2222, BindAddress: "127.0.0.1"},
			wantHTTP: "127.0.0.1:8080",
			wantSSH:  "127.0.0.1:2222",
		},
		{
			name:     "tailnet ip binds both listeners",
			cfg:      ServerConfig{HTTPPort: 8080, SSHPort: 2222, BindAddress: "100.64.0.1"},
			wantHTTP: "100.64.0.1:8080",
			wantSSH:  "100.64.0.1:2222",
		},
		{
			name:     "star is all IPv4",
			cfg:      ServerConfig{HTTPPort: 8080, SSHPort: 2222, BindAddress: "*"},
			wantHTTP: "0.0.0.0:8080",
			wantSSH:  "0.0.0.0:2222",
		},
		{
			name:     "double-colon is all interfaces",
			cfg:      ServerConfig{HTTPPort: 8080, SSHPort: 2222, BindAddress: "::"},
			wantHTTP: "[::]:8080",
			wantSSH:  "[::]:2222",
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

func TestValidateBindAddress(t *testing.T) {
	// Open mode is loopback only: no acknowledgment widens it. Only mtls (TLS +
	// client certificates) may bind a routable interface.
	mtlsAuth := &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{GitHubUsers: []string{"charliek"}}}
	tests := []struct {
		name    string
		cfg     ServerConfig
		wantErr string // substring the error must contain; "" means expect no error
	}{
		{"open unset is loopback (ok)", ServerConfig{}, ""},
		{"open explicit loopback ok", ServerConfig{BindAddress: "127.0.0.1"}, ""},
		{"open other 127/8 address ok", ServerConfig{BindAddress: "127.0.0.2"}, ""},
		{"open localhost ok", ServerConfig{BindAddress: "localhost"}, ""},
		{"open ::1 ok", ServerConfig{BindAddress: "::1"}, ""},
		{"explicit open mode on loopback ok", ServerConfig{BindAddress: "127.0.0.1", Auth: &AuthConfig{Mode: AuthModeOpen}}, ""},
		{"open 0.0.0.0 rejected", ServerConfig{BindAddress: "0.0.0.0"}, "mtls"},
		{"open star rejected", ServerConfig{BindAddress: "*"}, "mtls"},
		{"open :: rejected", ServerConfig{BindAddress: "::"}, "mtls"},
		{"open tailnet ip rejected", ServerConfig{BindAddress: "100.64.0.1"}, "mtls"},
		{"open LAN ip rejected", ServerConfig{BindAddress: "192.168.1.20"}, "mtls"},
		{"explicit open mode 0.0.0.0 rejected", ServerConfig{BindAddress: "0.0.0.0", Auth: &AuthConfig{Mode: AuthModeOpen}}, "mtls"},
		{"mtls mode 0.0.0.0 ok", ServerConfig{BindAddress: "0.0.0.0", Auth: mtlsAuth}, ""},
		{"mtls mode :: ok", ServerConfig{BindAddress: "::", Auth: mtlsAuth}, ""},
		{"mtls mode tailnet ip ok", ServerConfig{BindAddress: "100.64.0.1", Auth: mtlsAuth}, ""},
		// A hand-built config naming the removed token mode is not enforced,
		// so it gets the open-mode rule (validateAuth refuses the mode itself).
		{"removed token mode 0.0.0.0 rejected", ServerConfig{BindAddress: "0.0.0.0", Auth: &AuthConfig{Mode: AuthModeToken}}, "mtls"},
		// Format validation runs before the mode gate, so a malformed bind is
		// rejected in every mode (it would otherwise fail cryptically at net.Listen).
		{"malformed ip rejected", ServerConfig{BindAddress: "127.0.0.l"}, "invalid bind_address"},
		{"hostname rejected", ServerConfig{BindAddress: "box.example"}, "invalid bind_address"},
		{"over-long ipv4 rejected", ServerConfig{BindAddress: "0.0.0.0.0"}, "invalid bind_address"},
		{"malformed rejected even in mtls mode", ServerConfig{BindAddress: "nope", Auth: mtlsAuth}, "invalid bind_address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validateBindAddress()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validateBindAddress() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validateBindAddress() = %v, want an error containing %q", err, tt.wantErr)
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
	// auth.http.tokens / token-mode config — silently ignoring public_exposure
	// would un-loopback an
	// internet-facing plaintext listener (the v0.6.0 image-key silent-drift bug
	// this pattern exists to prevent).
	tests := []struct {
		name string
		yaml string
		want string // substring the error must contain; "" means expect no error
	}{
		{"token mode rejected naming mtls", "name: x\nauth:\n  mode: token\n", "use auth.mode: mtls"},
		{"secure alias rejected naming mtls", "name: x\nauth:\n  mode: secure\n", "use auth.mode: mtls"},
		{"token mode with key sources rejected", "name: x\nauth:\n  mode: token\n  ssh:\n    github_users: [charliek]\n", `auth.mode: "token" was removed`},
		{"secure alias error names the spelling used", "name: x\nauth:\n  mode: secure\n", `auth.mode: "secure" was removed`},
		{"open config ok", "name: x\nauth:\n  mode: open\n", ""},
		{"mtls config ok", "name: x\nauth:\n  mode: mtls\n", ""},
		{"misspelled mode left to validateAuth", "name: x\nauth:\n  mode: tokn\n", ""},
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
	// internal_http_port was removed in v0.7.4 and allow_insecure_exposure in
	// v0.10.0 — a config still carrying either must fail loudly rather than be
	// silently ignored (the first would imply a loopback bus listener the new
	// binary never starts; the second an open server reachable off-box).
	tests := []struct {
		name string
		yaml string
		want string // substring the error must contain; "" means expect no error
	}{
		{"clean config ok", "name: x\nhttp_port: 8080\n", ""},
		{"no network keys ok", "name: x\n", ""},
		{"internal_http_port rejected", "name: x\ninternal_http_port: 8081\n", "internal_http_port"},
		{"http_bind rejected (renamed to bind_address)", "name: x\nhttp_bind: 0.0.0.0\n", "bind_address"},
		{"ssh_bind rejected (renamed to bind_address)", "name: x\nssh_bind: 0.0.0.0\n", "bind_address"},
		{"allow_insecure_exposure true rejected", "name: x\nallow_insecure_exposure: true\n", "auth.mode: mtls"},
		{"allow_insecure_exposure false rejected", "name: x\nallow_insecure_exposure: false\n", "allow_insecure_exposure"},
		{"allow_insecure_exposure with a wide bind rejected", "name: x\nbind_address: 0.0.0.0\nallow_insecure_exposure: true\n", "loopback only"},
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
	// the server ever binds — an invalid or removed mode, a bad scope, or a
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
		{"invalid ssh mode rejected", &AuthConfig{SSH: &SSHAuthConfig{Mode: "enfore"}}, "auth.ssh.mode"},
		{"negative max_auth_tries rejected", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce, MaxAuthTries: -1}}, "max_auth_tries"},
		{"zero max_auth_tries ok", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce, MaxAuthTries: 0}}, ""},
		{"valid github user ok", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce, GitHubUsers: []string{"charliek"}}}, ""},
		{"malformed github user rejected", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce, GitHubUsers: []string{"bad user!"}}}, "github_users"},
		{"invalid auth.mode rejected", &AuthConfig{Mode: "secur"}, "must be open or mtls"},
		{"removed token auth.mode rejected", &AuthConfig{Mode: AuthModeToken}, "use auth.mode: mtls"},
		{"removed secure auth.mode rejected", &AuthConfig{Mode: "secure"}, "use auth.mode: mtls"},
		{"open auth.mode ok", &AuthConfig{Mode: AuthModeOpen}, ""},
		{"mtls + ssh enforce ok", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce}}, ""},
		{"mtls auth.mode ok", &AuthConfig{Mode: AuthModeMTLS}, ""},
		{"mtls + invalid ssh mode rejected", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{Mode: "enfore"}}, "auth.ssh.mode"},
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
		{"loopback by default", ServerConfig{HTTPPort: 8080, HTTPSPort: 8443}, true, "127.0.0.1:8443"},
		{"bound to one interface", ServerConfig{HTTPPort: 8080, HTTPSPort: 8443, BindAddress: "100.64.0.1"}, true, "100.64.0.1:8443"},
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

func TestValidateStateDir(t *testing.T) {
	tests := []struct {
		name    string
		dir     string
		wantErr bool
	}{
		{"unset ok", "", false},
		{"absolute ok", "/var/lib/shed-dev/state", false},
		{"home-relative ok", "~/.shed-dev", false},
		{"relative rejected", "state", true},
		{"dot-relative rejected", "./state", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ServerConfig{StateDir: tt.dir}
			err := cfg.validateStateDir()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateStateDir(%q) error = %v, wantErr %v", tt.dir, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "state_dir") {
				t.Errorf("error %q does not name state_dir", err)
			}
		})
	}
}
