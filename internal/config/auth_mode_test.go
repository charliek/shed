package config

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// captureStderr redirects os.Stderr for the duration of fn and returns
// everything written to it. Used to assert on the auth.mode deprecation
// warning, which is intentionally a direct Fprintln(os.Stderr, ...) rather
// than a mockable logger.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

func TestAuthModeEnforcement(t *testing.T) {
	// AuthEnforced is the open-vs-mtls predicate. Only open mode leaves the
	// HTTP API unauthenticated.
	tests := []struct {
		name            string
		auth            *AuthConfig
		wantMTLSMode    bool
		wantEnforced    bool
		wantHTTPEnforce bool
		wantModeValue   string
	}{
		{"nil auth", nil, false, false, false, AuthModeOpen},
		{"open default", &AuthConfig{}, false, false, false, AuthModeOpen},
		{"explicit open", &AuthConfig{Mode: AuthModeOpen}, false, false, false, AuthModeOpen},
		{"mtls", &AuthConfig{Mode: AuthModeMTLS}, true, true, true, AuthModeMTLS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ServerConfig{Auth: tt.auth}
			if c.MTLSMode() != tt.wantMTLSMode {
				t.Errorf("MTLSMode()=%v, want %v", c.MTLSMode(), tt.wantMTLSMode)
			}
			if c.AuthEnforced() != tt.wantEnforced {
				t.Errorf("AuthEnforced()=%v, want %v", c.AuthEnforced(), tt.wantEnforced)
			}
			if c.HTTPAuthEnforced() != tt.wantHTTPEnforce {
				t.Errorf("HTTPAuthEnforced()=%v, want %v", c.HTTPAuthEnforced(), tt.wantHTTPEnforce)
			}
			if got := c.AuthModeValue(); got != tt.wantModeValue {
				t.Errorf("AuthModeValue()=%q, want %q", got, tt.wantModeValue)
			}
		})
	}
}

// TestRemovedAuthModeIsNotEnforced pins that a hand-built config naming the
// removed token mode (one that never went through a loader) is not treated as
// an enforced mode by any predicate: validateAuth refuses it, and nothing
// below that may quietly read it as either open-with-auth or mtls.
func TestRemovedAuthModeIsNotEnforced(t *testing.T) {
	for _, mode := range []string{AuthModeToken, "secure"} {
		t.Run(mode, func(t *testing.T) {
			c := &ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: mode}}
			if c.MTLSMode() || c.AuthEnforced() || c.HTTPAuthEnforced() {
				t.Errorf("mode %q: MTLSMode=%v AuthEnforced=%v HTTPAuthEnforced=%v, want all false",
					mode, c.MTLSMode(), c.AuthEnforced(), c.HTTPAuthEnforced())
			}
			err := c.validateAuth()
			if err == nil || !strings.Contains(err.Error(), "mtls") {
				t.Errorf("validateAuth() = %v, want a refusal naming mtls", err)
			}
		})
	}
}

// TestEffectiveSSHAuthForcesEnforceInMTLSMode: mtls forces enforce while
// keeping the configured key sources, and must not mutate the underlying
// config. Open mode returns the configured block verbatim.
func TestEffectiveSSHAuthForcesEnforceInMTLSMode(t *testing.T) {
	c := &ServerConfig{Auth: &AuthConfig{
		Mode: AuthModeMTLS,
		SSH:  &SSHAuthConfig{Mode: SSHAuthWarn, GitHubUsers: []string{"charliek"}},
	}}
	eff := c.EffectiveSSHAuth()
	if eff.Mode != SSHAuthEnforce {
		t.Errorf("mtls-mode EffectiveSSHAuth mode = %q, want enforce", eff.Mode)
	}
	if len(eff.GitHubUsers) != 1 || eff.GitHubUsers[0] != "charliek" {
		t.Errorf("key sources not preserved: %+v", eff.GitHubUsers)
	}
	if c.Auth.SSH.Mode != SSHAuthWarn {
		t.Error("EffectiveSSHAuth mutated the underlying config")
	}
	open := &ServerConfig{Auth: &AuthConfig{Mode: AuthModeOpen}}
	if open.EffectiveSSHAuth() != nil {
		t.Error("open EffectiveSSHAuth should be nil when auth.ssh unset")
	}
}

func TestTokenTTLDefaulting(t *testing.T) {
	if got := (&ServerConfig{}).TokenTTL(); got != DefaultTokenTTL {
		t.Errorf("default TokenTTL=%v, want %v", got, DefaultTokenTTL)
	}
	c := &ServerConfig{Auth: &AuthConfig{TokenTTL: Duration(2 * time.Hour)}}
	if got := c.TokenTTL(); got != 2*time.Hour {
		t.Errorf("TokenTTL=%v, want 2h", got)
	}
}

func TestPreflightAuth(t *testing.T) {
	tests := []struct {
		name    string
		auth    *AuthConfig
		wantErr bool
	}{
		{"open is inert", &AuthConfig{Mode: AuthModeOpen}, false},
		{"nil auth is inert", nil, false},
		{"mtls + github_users ok", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{GitHubUsers: []string{"charliek"}}}, false},
		{"mtls + authorized_keys ok", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{AuthorizedKeys: []string{"ssh-ed25519 AAAA x"}}}, false},
		{"mtls + authorized_keys_file ok", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{AuthorizedKeysFile: "/etc/shed/keys"}}, false},
		{"mtls with no key source fails", &AuthConfig{Mode: AuthModeMTLS}, true},
		{"mtls with empty ssh block fails", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&ServerConfig{Auth: tt.auth}).PreflightAuth()
			if (err != nil) != tt.wantErr {
				t.Errorf("PreflightAuth() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestPlainHTTPEnabled(t *testing.T) {
	// mtls is TLS-only: the plain-HTTP listener is not served (only the
	// pinned-TLS listener faces clients). Open mode serves plain HTTP.
	tests := []struct {
		name string
		cfg  ServerConfig
		want bool
	}{
		{"open serves plain HTTP", ServerConfig{HTTPPort: 8080}, true},
		{"mtls mode is TLS-only", ServerConfig{HTTPPort: 8080, Auth: &AuthConfig{Mode: AuthModeMTLS}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.PlainHTTPEnabled(); got != tt.want {
				t.Errorf("PlainHTTPEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCrossFieldAuthValidation exercises the mtls⟺certs⟺TLS coupling: SSH
// enforce and https_port are mtls-only surfaces, and mtls forbids an explicit
// ssh off/warn override.
func TestCrossFieldAuthValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ServerConfig
		wantErr string // substring the error must name; "" means expect success
	}{
		{
			name:    "open + ssh enforce rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeOpen, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce}}},
			wantErr: "auth.ssh.mode: enforce requires auth.mode: mtls",
		},
		{
			name:    "open + https_port rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8443, Auth: &AuthConfig{Mode: AuthModeOpen}},
			wantErr: "https_port requires auth.mode: mtls",
		},
		{
			name:    "https_port with no auth block rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8443},
			wantErr: "https_port requires auth.mode: mtls",
		},
		{
			name:    "open + ssh warn ok (staging)",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeOpen, SSH: &SSHAuthConfig{Mode: SSHAuthWarn}}},
			wantErr: "",
		},
		{
			name:    "mtls + ssh warn rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{Mode: SSHAuthWarn}}},
			wantErr: "auth.mode: mtls forces auth.ssh.mode: enforce",
		},
		{
			name:    "mtls + ssh off rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{Mode: SSHAuthOff}}},
			wantErr: "auth.mode: mtls forces auth.ssh.mode: enforce",
		},
		{
			name:    "mtls + ssh enforce ok",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce}}},
			wantErr: "",
		},
		{
			name:    "mtls + ssh unset ok (derives enforce)",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{GitHubUsers: []string{"charliek"}}}},
			wantErr: "",
		},
		{
			name:    "mtls + https_port ok",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8443, Auth: &AuthConfig{Mode: AuthModeMTLS}},
			wantErr: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validateAuth()
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

// TestNormalizeAuthModeClientBoundary pins the client half of the wire-compat
// contract with servers older than this one: a 0.9.x token-mode server reports
// "secure" on /api/info, and the CLI's decode must map it to the canonical
// constant, or the HTTP-fallback guard in `shed server add` writes a
// credential-less entry for it. Every other value passes through, so a future
// mode is not eaten.
func TestNormalizeAuthModeClientBoundary(t *testing.T) {
	tests := []struct{ in, want string }{
		{"secure", AuthModeToken},
		{AuthModeToken, AuthModeToken},
		{AuthModeOpen, AuthModeOpen},
		{AuthModeMTLS, AuthModeMTLS},
		{"", ""},
		{"bogus", "bogus"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := NormalizeAuthMode(tt.in); got != tt.want {
				t.Errorf("NormalizeAuthMode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
