package config

import "testing"

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
