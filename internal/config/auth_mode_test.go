package config

import (
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
		{"open + http enforce override", &AuthConfig{HTTP: &HTTPAuthConfig{Mode: HTTPAuthEnforce}}, false, true},
		{"open + http off", &AuthConfig{HTTP: &HTTPAuthConfig{Mode: HTTPAuthOff}}, false, false},
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

func TestSecureModeLoopbackBind(t *testing.T) {
	// Secure mode forces the plain-HTTP listener to loopback (the same property
	// public_exposure provided), so the cleartext control plane is never
	// reachable off-box — only the pinned-TLS listener faces the network.
	secure := &ServerConfig{HTTPPort: 8080, Auth: &AuthConfig{Mode: AuthModeSecure}}
	if got := secure.HTTPListenAddr(); got != "127.0.0.1:8080" {
		t.Errorf("secure HTTPListenAddr=%q, want loopback", got)
	}
	open := &ServerConfig{HTTPPort: 8080}
	if got := open.HTTPListenAddr(); got != ":8080" {
		t.Errorf("open HTTPListenAddr=%q, want all-interfaces", got)
	}
}
