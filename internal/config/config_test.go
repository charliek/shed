package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContainerName(t *testing.T) {
	tests := []struct {
		name     string
		shedName string
		want     string
	}{
		{"simple", "myapp", "shed-myapp"},
		{"with-hyphen", "my-app", "shed-my-app"},
		{"numbers", "app123", "shed-app123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContainerName(tt.shedName)
			if got != tt.want {
				t.Errorf("ContainerName(%q) = %q, want %q", tt.shedName, got, tt.want)
			}
		})
	}
}

func TestVolumeName(t *testing.T) {
	tests := []struct {
		name     string
		shedName string
		want     string
	}{
		{"simple", "myapp", "shed-myapp-workspace"},
		{"with-hyphen", "my-app", "shed-my-app-workspace"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VolumeName(tt.shedName)
			if got != tt.want {
				t.Errorf("VolumeName(%q) = %q, want %q", tt.shedName, got, tt.want)
			}
		})
	}
}

func TestNewAPIError(t *testing.T) {
	err := NewAPIError(ErrShedNotFound, "Shed 'test' not found")

	if err.Error.Code != ErrShedNotFound {
		t.Errorf("Code = %q, want %q", err.Error.Code, ErrShedNotFound)
	}
	if err.Error.Message != "Shed 'test' not found" {
		t.Errorf("Message = %q, want %q", err.Error.Message, "Shed 'test' not found")
	}
}

func TestServerConfigDefaults(t *testing.T) {
	cfg := DefaultServerConfig()

	if cfg.HTTPPort != 8080 {
		t.Errorf("HTTPPort = %d, want 8080", cfg.HTTPPort)
	}
	if cfg.SSHPort != 2222 {
		t.Errorf("SSHPort = %d, want 2222", cfg.SSHPort)
	}
	if cfg.DefaultImage != "shed-base:latest" {
		t.Errorf("DefaultImage = %q, want %q", cfg.DefaultImage, "shed-base:latest")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
}

func TestServerConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *ServerConfig
		wantErr bool
	}{
		{
			name:    "valid",
			cfg:     &ServerConfig{Name: "test", HTTPPort: 8080, SSHPort: 2222, LogLevel: "info"},
			wantErr: false,
		},
		{
			name:    "missing name",
			cfg:     &ServerConfig{HTTPPort: 8080, SSHPort: 2222, LogLevel: "info"},
			wantErr: true,
		},
		{
			name:    "invalid http port",
			cfg:     &ServerConfig{Name: "test", HTTPPort: 0, SSHPort: 2222, LogLevel: "info"},
			wantErr: true,
		},
		{
			name:    "invalid log level",
			cfg:     &ServerConfig{Name: "test", HTTPPort: 8080, SSHPort: 2222, LogLevel: "invalid"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestClientConfigSaveLoad(t *testing.T) {
	// Create a temp directory for test
	tmpDir, err := os.MkdirTemp("", "shed-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	configPath := filepath.Join(tmpDir, "config.yaml")

	// Create and save config
	cfg := &ClientConfig{
		Servers: map[string]ServerEntry{
			"test-server": {
				Host:     "localhost",
				HTTPPort: 8080,
				SSHPort:  2222,
			},
		},
		DefaultServer: "test-server",
		Sheds:         make(map[string]ShedCache),
	}

	if err := cfg.SaveToPath(configPath); err != nil {
		t.Fatalf("Save() failed: %v", err)
	}

	// Load and verify
	loaded, err := LoadClientConfigFromPath(configPath)
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if loaded.DefaultServer != "test-server" {
		t.Errorf("DefaultServer = %q, want %q", loaded.DefaultServer, "test-server")
	}

	server, err := loaded.GetServer("test-server")
	if err != nil {
		t.Fatalf("GetServer() failed: %v", err)
	}
	if server.Host != "localhost" {
		t.Errorf("Server.Host = %q, want %q", server.Host, "localhost")
	}
}

func TestClientConfigServerOperations(t *testing.T) {
	cfg := &ClientConfig{
		Servers: make(map[string]ServerEntry),
		Sheds:   make(map[string]ShedCache),
	}

	// Add server
	err := cfg.AddServer("server1", ServerEntry{Host: "host1", HTTPPort: 8080, SSHPort: 2222})
	if err != nil {
		t.Fatalf("AddServer() failed: %v", err)
	}

	// Verify it's the default (first server)
	if cfg.DefaultServer != "server1" {
		t.Errorf("DefaultServer = %q, want %q", cfg.DefaultServer, "server1")
	}

	// Add another server
	err = cfg.AddServer("server2", ServerEntry{Host: "host2", HTTPPort: 8080, SSHPort: 2222})
	if err != nil {
		t.Fatalf("AddServer() failed: %v", err)
	}

	// Default should still be server1
	if cfg.DefaultServer != "server1" {
		t.Errorf("DefaultServer = %q, want %q", cfg.DefaultServer, "server1")
	}

	// Try to add duplicate
	err = cfg.AddServer("server1", ServerEntry{Host: "host3", HTTPPort: 8080, SSHPort: 2222})
	if err == nil {
		t.Error("AddServer() should fail for duplicate name")
	}

	// Set default
	err = cfg.SetDefaultServer("server2")
	if err != nil {
		t.Fatalf("SetDefaultServer() failed: %v", err)
	}
	if cfg.DefaultServer != "server2" {
		t.Errorf("DefaultServer = %q, want %q", cfg.DefaultServer, "server2")
	}

	// Remove server
	err = cfg.RemoveServer("server1")
	if err != nil {
		t.Fatalf("RemoveServer() failed: %v", err)
	}

	_, err = cfg.GetServer("server1")
	if err == nil {
		t.Error("GetServer() should fail for removed server")
	}
}

func TestClientConfigShedCache(t *testing.T) {
	cfg := &ClientConfig{
		Servers: make(map[string]ServerEntry),
		Sheds:   make(map[string]ShedCache),
	}

	// Cache a shed
	cfg.CacheShed("myshed", "server1", StatusRunning)

	server, err := cfg.GetShedServer("myshed")
	if err != nil {
		t.Fatalf("GetShedServer() failed: %v", err)
	}
	if server != "server1" {
		t.Errorf("Server = %q, want %q", server, "server1")
	}

	// Remove from cache
	cfg.RemoveShedCache("myshed")

	_, err = cfg.GetShedServer("myshed")
	if err == nil {
		t.Error("GetShedServer() should fail for removed shed")
	}
}
