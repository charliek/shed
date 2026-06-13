package config

import (
	"strings"
	"testing"
)

func TestListenAddrHelpers(t *testing.T) {
	tests := []struct {
		name                            string
		cfg                             ServerConfig
		wantHTTP, wantSSH, wantInternal string
	}{
		{
			name:         "defaults bind all interfaces",
			cfg:          ServerConfig{HTTPPort: 8080, SSHPort: 2222},
			wantHTTP:     ":8080",
			wantSSH:      ":2222",
			wantInternal: "",
		},
		{
			name:         "http_bind restricts to loopback",
			cfg:          ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPBind: "127.0.0.1"},
			wantHTTP:     "127.0.0.1:8080",
			wantSSH:      ":2222",
			wantInternal: "",
		},
		{
			name:         "ssh_bind restricts to a tailnet ip",
			cfg:          ServerConfig{HTTPPort: 8080, SSHPort: 2222, SSHBind: "100.64.0.1"},
			wantHTTP:     ":8080",
			wantSSH:      "100.64.0.1:2222",
			wantInternal: "",
		},
		{
			name:         "internal_http_port enables loopback internal listener",
			cfg:          ServerConfig{HTTPPort: 8080, SSHPort: 2222, InternalHTTPPort: 8081},
			wantHTTP:     ":8080",
			wantSSH:      ":2222",
			wantInternal: "127.0.0.1:8081",
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
			if got := tt.cfg.InternalHTTPListenAddr(); got != tt.wantInternal {
				t.Errorf("InternalHTTPListenAddr() = %q, want %q", got, tt.wantInternal)
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
		{"split disabled (0) ok", ServerConfig{HTTPPort: 8080, SSHPort: 2222, InternalHTTPPort: 0}, false},
		{"valid offset port", ServerConfig{HTTPPort: 8080, SSHPort: 2222, InternalHTTPPort: 8081}, false},
		{"out of range", ServerConfig{HTTPPort: 8080, SSHPort: 2222, InternalHTTPPort: 70000}, true},
		{"negative", ServerConfig{HTTPPort: 8080, SSHPort: 2222, InternalHTTPPort: -1}, true},
		{"collides with http_port", ServerConfig{HTTPPort: 8080, SSHPort: 2222, InternalHTTPPort: 8080}, true},
		{"collides with ssh_port", ServerConfig{HTTPPort: 8080, SSHPort: 2222, InternalHTTPPort: 2222}, true},
		{"https disabled (0) ok", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 0}, false},
		{"valid https port", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8443}, false},
		{"https out of range", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 70000}, true},
		{"https negative", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: -1}, true},
		{"https collides with http_port", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 8080}, true},
		{"https collides with ssh_port", ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPSPort: 2222}, true},
		{"https collides with internal", ServerConfig{HTTPPort: 8080, SSHPort: 2222, InternalHTTPPort: 8081, HTTPSPort: 8081}, true},
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

func TestPreflightPublicExposure(t *testing.T) {
	// A complete bundle: SSH enforce + HTTP enforce(+token) + TLS + internal bus.
	strongToken := "shed_control_" + strings.Repeat("a", 32) // generated-shape, >= minPublicTokenLen
	full := func() ServerConfig {
		return ServerConfig{
			HTTPPort: 8080, SSHPort: 2222,
			HTTPSPort: 8443, InternalHTTPPort: 8081,
			PublicExposure: true,
			Auth: &AuthConfig{
				SSH:  &SSHAuthConfig{Mode: SSHAuthEnforce, GitHubUsers: []string{"charliek"}},
				HTTP: &HTTPAuthConfig{Mode: HTTPAuthEnforce, Tokens: []HTTPToken{{Scope: TokenScopeControl, Token: strongToken}}},
			},
		}
	}

	t.Run("inert when unset, even on a non-loopback bind", func(t *testing.T) {
		// The tailnet/LAN fleet: public_exposure unset → preflight is a no-op
		// regardless of bind or missing auth.
		cfg := ServerConfig{HTTPPort: 8080, SSHPort: 2222, HTTPBind: "100.64.0.1"}
		if err := cfg.PreflightPublicExposure(); err != nil {
			t.Errorf("unset public_exposure must be inert, got %v", err)
		}
	})
	t.Run("complete bundle passes", func(t *testing.T) {
		cfg := full()
		if err := cfg.PreflightPublicExposure(); err != nil {
			t.Errorf("complete bundle should pass, got %v", err)
		}
	})
	t.Run("plain HTTP forced to loopback under public_exposure", func(t *testing.T) {
		// Even with an all-interfaces http_bind, the plaintext listener must be
		// loopback-only so only the TLS listener faces the network.
		cfg := full()
		cfg.HTTPBind = "" // default: all interfaces
		if got := cfg.HTTPListenAddr(); got != "127.0.0.1:8080" {
			t.Errorf("HTTPListenAddr() under public_exposure = %q, want loopback 127.0.0.1:8080", got)
		}
		// HTTPS still faces the network (honors http_bind, default all interfaces).
		if got := cfg.HTTPSListenAddr(); got != ":8443" {
			t.Errorf("HTTPSListenAddr() = %q, want :8443 (public)", got)
		}
	})

	// Each missing piece is rejected, naming the gap.
	missing := []struct {
		name   string
		mutate func(c *ServerConfig)
		want   string
	}{
		{"no ssh enforce", func(c *ServerConfig) { c.Auth.SSH.Mode = SSHAuthWarn }, "auth.ssh.mode"},
		{"no ssh auth at all", func(c *ServerConfig) { c.Auth.SSH = nil }, "auth.ssh.mode"},
		{"no http enforce", func(c *ServerConfig) { c.Auth.HTTP.Mode = HTTPAuthOff }, "auth.http.mode"},
		{"no http tokens", func(c *ServerConfig) { c.Auth.HTTP.Tokens = nil }, "auth.http.tokens"},
		{"weak http token", func(c *ServerConfig) { c.Auth.HTTP.Tokens = []HTTPToken{{Scope: TokenScopeControl, Token: "x"}} }, "strong auth.http.tokens"},
		{"no tls", func(c *ServerConfig) { c.HTTPSPort = 0 }, "https_port"},
		{"no internal bus", func(c *ServerConfig) { c.InternalHTTPPort = 0 }, "internal_http_port"},
	}
	for _, tt := range missing {
		t.Run(tt.name, func(t *testing.T) {
			cfg := full()
			tt.mutate(&cfg)
			err := cfg.PreflightPublicExposure()
			if err == nil {
				t.Fatalf("missing %q must refuse to start", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should name %q, got %v", tt.want, err)
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
