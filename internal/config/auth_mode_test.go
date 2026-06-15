package config

import (
	"strings"
	"testing"
	"time"
)

func TestSecureAndEnforcement(t *testing.T) {
	tests := []struct {
		name            string
		auth            *AuthConfig
		wantSecure      bool
		wantHTTPEnforce bool
	}{
		{"nil auth", nil, false, false},
		{"open default", &AuthConfig{}, false, false},
		{"explicit open", &AuthConfig{Mode: AuthModeOpen}, false, false},
		{"secure", &AuthConfig{Mode: AuthModeSecure}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &ServerConfig{Auth: tt.auth}
			if c.Secure() != tt.wantSecure {
				t.Errorf("Secure()=%v, want %v", c.Secure(), tt.wantSecure)
			}
			if c.HTTPAuthEnforced() != tt.wantHTTPEnforce {
				t.Errorf("HTTPAuthEnforced()=%v, want %v", c.HTTPAuthEnforced(), tt.wantHTTPEnforce)
			}
		})
	}
}

func TestEffectiveSSHAuthForcesEnforceInSecure(t *testing.T) {
	// Secure mode forces enforce while keeping the configured key sources, and
	// must not mutate the underlying config.
	c := &ServerConfig{Auth: &AuthConfig{
		Mode: AuthModeSecure,
		SSH:  &SSHAuthConfig{Mode: SSHAuthWarn, GitHubUsers: []string{"charliek"}},
	}}
	eff := c.EffectiveSSHAuth()
	if eff.Mode != SSHAuthEnforce {
		t.Errorf("secure EffectiveSSHAuth mode = %q, want enforce", eff.Mode)
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

func TestTokenTTLDefaulting(t *testing.T) {
	if got := (&ServerConfig{}).TokenTTL(); got != DefaultTokenTTL {
		t.Errorf("default TokenTTL=%v, want %v", got, DefaultTokenTTL)
	}
	c := &ServerConfig{Auth: &AuthConfig{TokenTTL: Duration(2 * time.Hour)}}
	if got := c.TokenTTL(); got != 2*time.Hour {
		t.Errorf("TokenTTL=%v, want 2h", got)
	}
}

func TestPreflightSecure(t *testing.T) {
	tests := []struct {
		name    string
		auth    *AuthConfig
		wantErr bool
	}{
		{"open is inert", &AuthConfig{Mode: AuthModeOpen}, false},
		{"nil auth is inert", nil, false},
		{"secure + github_users ok", &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{GitHubUsers: []string{"charliek"}}}, false},
		{"secure + authorized_keys ok", &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{AuthorizedKeys: []string{"ssh-ed25519 AAAA x"}}}, false},
		{"secure + authorized_keys_file ok", &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{AuthorizedKeysFile: "/etc/shed/keys"}}, false},
		{"secure with no key source fails", &AuthConfig{Mode: AuthModeSecure}, true},
		{"secure with empty ssh block fails", &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&ServerConfig{Auth: tt.auth}).PreflightSecure()
			if (err != nil) != tt.wantErr {
				t.Errorf("PreflightSecure() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestPlainHTTPEnabled(t *testing.T) {
	// Secure mode is TLS-only: the plain-HTTP listener is not served (only the
	// pinned-TLS listener faces clients). Open mode serves plain HTTP.
	secure := &ServerConfig{HTTPPort: 8080, Auth: &AuthConfig{Mode: AuthModeSecure}}
	if secure.PlainHTTPEnabled() {
		t.Error("secure mode should not serve the plain-HTTP listener")
	}
	open := &ServerConfig{HTTPPort: 8080}
	if !open.PlainHTTPEnabled() {
		t.Error("open mode should serve the plain-HTTP listener")
	}
}

// TestCrossFieldAuthValidation exercises the secure⟺tokens⟺TLS coupling: SSH
// enforce and https_port are secure-mode-only surfaces, and secure forbids an
// explicit ssh off/warn override.
func TestCrossFieldAuthValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ServerConfig
		wantErr string // substring the error must name; "" means expect success
	}{
		{
			name:    "open + ssh enforce rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeOpen, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce}}},
			wantErr: "auth.ssh.mode: enforce requires auth.mode: secure",
		},
		{
			name:    "open + https_port rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8443, Auth: &AuthConfig{Mode: AuthModeOpen}},
			wantErr: "https_port requires auth.mode: secure",
		},
		{
			name:    "https_port with no auth block rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8443},
			wantErr: "https_port requires auth.mode: secure",
		},
		{
			name:    "secure + ssh warn rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{Mode: SSHAuthWarn}}},
			wantErr: "auth.mode: secure forces auth.ssh.mode: enforce",
		},
		{
			name:    "secure + ssh off rejected",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{Mode: SSHAuthOff}}},
			wantErr: "auth.mode: secure forces auth.ssh.mode: enforce",
		},
		{
			name:    "open + ssh warn ok (staging)",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeOpen, SSH: &SSHAuthConfig{Mode: SSHAuthWarn}}},
			wantErr: "",
		},
		{
			name:    "secure + ssh enforce ok",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{Mode: SSHAuthEnforce}}},
			wantErr: "",
		},
		{
			name:    "secure + ssh unset ok (derives enforce)",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, Auth: &AuthConfig{Mode: AuthModeSecure, SSH: &SSHAuthConfig{GitHubUsers: []string{"charliek"}}}},
			wantErr: "",
		},
		{
			name:    "secure + https_port ok",
			cfg:     ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8443, Auth: &AuthConfig{Mode: AuthModeSecure}},
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
