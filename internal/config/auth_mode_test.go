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

func TestTokenModeAndEnforcement(t *testing.T) {
	// AuthEnforced is the combined open-vs-(token|mtls) predicate; mtls shares
	// every enforced-mode invariant except HTTP bearer-token enforcement (it
	// authenticates the client via certificate instead — see HTTPAuthEnforced).
	tests := []struct {
		name            string
		auth            *AuthConfig
		wantTokenMode   bool
		wantMTLSMode    bool
		wantEnforced    bool
		wantHTTPEnforce bool
	}{
		{"nil auth", nil, false, false, false, false},
		{"open default", &AuthConfig{}, false, false, false, false},
		{"explicit open", &AuthConfig{Mode: AuthModeOpen}, false, false, false, false},
		{"token", &AuthConfig{Mode: AuthModeToken}, true, false, true, true},
		{"mtls", &AuthConfig{Mode: AuthModeMTLS}, false, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ServerConfig{Auth: tt.auth}
			if c.TokenMode() != tt.wantTokenMode {
				t.Errorf("TokenMode()=%v, want %v", c.TokenMode(), tt.wantTokenMode)
			}
			if c.MTLSMode() != tt.wantMTLSMode {
				t.Errorf("MTLSMode()=%v, want %v", c.MTLSMode(), tt.wantMTLSMode)
			}
			if c.AuthEnforced() != tt.wantEnforced {
				t.Errorf("AuthEnforced()=%v, want %v", c.AuthEnforced(), tt.wantEnforced)
			}
			if c.HTTPAuthEnforced() != tt.wantHTTPEnforce {
				t.Errorf("HTTPAuthEnforced()=%v, want %v", c.HTTPAuthEnforced(), tt.wantHTTPEnforce)
			}
		})
	}
}

func TestEffectiveSSHAuthForcesEnforceInTokenMode(t *testing.T) {
	// Token mode forces enforce while keeping the configured key sources, and
	// must not mutate the underlying config.
	c := &ServerConfig{Auth: &AuthConfig{
		Mode: AuthModeToken,
		SSH:  &SSHAuthConfig{Mode: SSHAuthWarn, GitHubUsers: []string{"charliek"}},
	}}
	eff := c.EffectiveSSHAuth()
	if eff.Mode != SSHAuthEnforce {
		t.Errorf("token-mode EffectiveSSHAuth mode = %q, want enforce", eff.Mode)
	}
	if len(eff.GitHubUsers) != 1 || eff.GitHubUsers[0] != "charliek" {
		t.Errorf("key sources not preserved: %+v", eff.GitHubUsers)
	}
	if c.Auth.SSH.Mode != SSHAuthWarn {
		t.Error("EffectiveSSHAuth mutated the underlying config")
	}
	// Open mode returns the configured block verbatim (nil when unset).
	open := &ServerConfig{Auth: &AuthConfig{Mode: AuthModeOpen}}
	if open.EffectiveSSHAuth() != nil {
		t.Error("open EffectiveSSHAuth should be nil when auth.ssh unset")
	}
}

// TestEffectiveSSHAuthForcesEnforceInMTLSMode mirrors the token-mode case
// above: mtls forces the same enforce posture, since it shares the SSH
// allowlist invariant with token mode.
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
		{"token + github_users ok", &AuthConfig{Mode: AuthModeToken, SSH: &SSHAuthConfig{GitHubUsers: []string{"charliek"}}}, false},
		{"token + authorized_keys ok", &AuthConfig{Mode: AuthModeToken, SSH: &SSHAuthConfig{AuthorizedKeys: []string{"ssh-ed25519 AAAA x"}}}, false},
		{"token + authorized_keys_file ok", &AuthConfig{Mode: AuthModeToken, SSH: &SSHAuthConfig{AuthorizedKeysFile: "/etc/shed/keys"}}, false},
		{"token with no key source fails", &AuthConfig{Mode: AuthModeToken}, true},
		{"token with empty ssh block fails", &AuthConfig{Mode: AuthModeToken, SSH: &SSHAuthConfig{}}, true},
		{"mtls + github_users ok", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{GitHubUsers: []string{"charliek"}}}, false},
		{"mtls + authorized_keys ok", &AuthConfig{Mode: AuthModeMTLS, SSH: &SSHAuthConfig{AuthorizedKeys: []string{"ssh-ed25519 AAAA x"}}}, false},
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
	// Token and mtls modes are both TLS-only: the plain-HTTP listener is not
	// served (only the pinned-TLS listener faces clients). Open mode serves
	// plain HTTP.
	tests := []struct {
		name string
		cfg  ServerConfig
		want bool
	}{
		{"open serves plain HTTP", ServerConfig{HTTPPort: 8080}, true},
		{"token mode is TLS-only", ServerConfig{HTTPPort: 8080, Auth: &AuthConfig{Mode: AuthModeToken}}, false},
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

// TestCrossFieldAuthValidation exercises the token⟺tokens⟺TLS coupling: SSH
// enforce and https_port are token-mode-only surfaces, and token mode forbids
// an explicit ssh off/warn override.
func TestCrossFieldAuthValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ServerConfig
		wantErr string // substring the error must name; "" means expect success
	}{
		{
			name:    "open + ssh enforce rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeOpen, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce}}},
			wantErr: "auth.ssh.mode: enforce requires auth.mode: token",
		},
		{
			name:    "open + https_port rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8443, Auth: &AuthConfig{Mode: AuthModeOpen}},
			wantErr: "https_port requires auth.mode: token",
		},
		{
			name:    "https_port with no auth block rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8443},
			wantErr: "https_port requires auth.mode: token",
		},
		{
			name:    "token + ssh warn rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeToken, SSH: &SSHAuthConfig{Mode: SSHAuthWarn}}},
			wantErr: "auth.mode: token forces auth.ssh.mode: enforce",
		},
		{
			name:    "token + ssh off rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeToken, SSH: &SSHAuthConfig{Mode: SSHAuthOff}}},
			wantErr: "auth.mode: token forces auth.ssh.mode: enforce",
		},
		{
			name:    "open + ssh warn ok (staging)",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeOpen, SSH: &SSHAuthConfig{Mode: SSHAuthWarn}}},
			wantErr: "",
		},
		{
			name:    "token + ssh enforce ok",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeToken, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce}}},
			wantErr: "",
		},
		{
			name:    "token + ssh unset ok (derives enforce)",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeToken, SSH: &SSHAuthConfig{GitHubUsers: []string{"charliek"}}}},
			wantErr: "",
		},
		{
			name:    "token + https_port ok",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8443, Auth: &AuthConfig{Mode: AuthModeToken}},
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

// TestNormalizeAuthModeValue covers the pure secure→token alias mapping used
// by normalizeAuthMode at config load time.
func TestNormalizeAuthModeValue(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		wantNormal    string
		wantDeprecate bool
	}{
		{"empty passes through", "", "", false},
		{"canonical token passes through", AuthModeToken, AuthModeToken, false},
		{"open passes through", AuthModeOpen, AuthModeOpen, false},
		{"mtls passes through unchanged (no alias)", AuthModeMTLS, AuthModeMTLS, false},
		{"legacy secure normalizes to token", "secure", AuthModeToken, true},
		{"invalid value passes through unchanged", "bogus", "bogus", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMode, gotDeprecated := normalizeAuthModeValue(tt.mode)
			if gotMode != tt.wantNormal {
				t.Errorf("normalizeAuthModeValue(%q) mode = %q, want %q", tt.mode, gotMode, tt.wantNormal)
			}
			if gotDeprecated != tt.wantDeprecate {
				t.Errorf("normalizeAuthModeValue(%q) deprecated = %v, want %v", tt.mode, gotDeprecated, tt.wantDeprecate)
			}
		})
	}
}

// TestNormalizeAuthModeWarning verifies the deprecation warning is emitted
// exactly once when the legacy "secure" spelling is normalized, and not at
// all for the canonical "token" (or an unset/open mode).
func TestNormalizeAuthModeWarning(t *testing.T) {
	tests := []struct {
		name      string
		auth      *AuthConfig
		wantMode  string
		wantWarns int
	}{
		{"nil auth: no warning", nil, "", 0},
		{"open: no warning", &AuthConfig{Mode: AuthModeOpen}, AuthModeOpen, 0},
		{"token: no warning", &AuthConfig{Mode: AuthModeToken}, AuthModeToken, 0},
		{"mtls: no warning", &AuthConfig{Mode: AuthModeMTLS}, AuthModeMTLS, 0},
		{"secure: normalizes to token, warns once", &AuthConfig{Mode: "secure"}, AuthModeToken, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &ServerConfig{Auth: tt.auth}
			stderr := captureStderr(t, func() {
				normalizeAuthMode(cfg)
			})
			if tt.auth != nil && cfg.Auth.Mode != tt.wantMode {
				t.Errorf("Auth.Mode = %q, want %q", cfg.Auth.Mode, tt.wantMode)
			}
			gotWarns := strings.Count(stderr, authModeDeprecationWarning)
			if gotWarns != tt.wantWarns {
				t.Errorf("deprecation warning count = %d, want %d (stderr: %q)", gotWarns, tt.wantWarns, stderr)
			}
		})
	}
}
