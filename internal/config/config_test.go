package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
	validConfig := &ServerConfig{
		Name:            "test",
		HTTPPort:        8080,
		SSHPort:         2222,
		LogLevel:        "info",
		DefaultBackend:  BackendDocker,
		EnabledBackends: []string{BackendDocker},
	}

	tests := []struct {
		name    string
		cfg     *ServerConfig
		wantErr bool
	}{
		{
			name:    "valid",
			cfg:     validConfig,
			wantErr: false,
		},
		{
			name:    "missing name",
			cfg:     &ServerConfig{HTTPPort: 8080, SSHPort: 2222, LogLevel: "info", DefaultBackend: BackendDocker, EnabledBackends: []string{BackendDocker}},
			wantErr: true,
		},
		{
			name:    "invalid http port",
			cfg:     &ServerConfig{Name: "test", HTTPPort: 0, SSHPort: 2222, LogLevel: "info", DefaultBackend: BackendDocker, EnabledBackends: []string{BackendDocker}},
			wantErr: true,
		},
		{
			name:    "invalid log level",
			cfg:     &ServerConfig{Name: "test", HTTPPort: 8080, SSHPort: 2222, LogLevel: "invalid", DefaultBackend: BackendDocker, EnabledBackends: []string{BackendDocker}},
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

func TestServerConfigVZPlatformValidation(t *testing.T) {
	cfg := &ServerConfig{
		Name:            "test",
		HTTPPort:        8080,
		SSHPort:         2222,
		LogLevel:        "info",
		DefaultBackend:  BackendVZ,
		EnabledBackends: []string{BackendVZ},
		VZ:              validVZConfig(),
	}

	err := cfg.Validate()
	if runtime.GOOS != "darwin" {
		if err == nil || !strings.Contains(err.Error(), "vz backend is only supported on macOS") {
			t.Fatalf("expected macOS platform validation error, got: %v", err)
		}
		return
	}
	if runtime.GOARCH != "arm64" {
		if err == nil || !strings.Contains(err.Error(), "Apple Silicon (arm64)") {
			t.Fatalf("expected arm64 platform validation error, got: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("expected valid VZ config on darwin/arm64, got: %v", err)
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

func TestValidateShedName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "myapp", false},
		{"valid with hyphen", "my-app", false},
		{"valid with numbers", "app123", false},
		{"valid single char", "a", false},
		{"empty", "", true},
		{"starts with number", "123app", true},
		{"starts with hyphen", "-myapp", true},
		{"ends with hyphen", "myapp-", true},
		{"uppercase", "MyApp", true},
		{"underscores", "my_app", true},
		{"too long", strings.Repeat("a", MaxShedNameLength+1), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateShedName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateShedName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestValidateSessionName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "default", false},
		{"valid with hyphen", "my-session", false},
		{"valid with underscore", "my_session", false},
		{"valid with numbers", "session123", false},
		{"valid uppercase", "MySession", false},
		{"valid mixed", "Claude_Session-1", false},
		{"empty", "", true},
		{"starts with hyphen", "-session", true},
		{"starts with underscore", "_session", true},
		{"too long", strings.Repeat("a", MaxSessionNameLength+1), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSessionName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSessionName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

// validFirecrackerConfig returns a FirecrackerConfig that passes validation.
// It uses /dev/null for file paths since those must exist on disk.
func validFirecrackerConfig() *FirecrackerConfig {
	return &FirecrackerConfig{
		KernelPath:      "/dev/null",
		BaseRootfs:      "/dev/null",
		InstanceDir:     "/tmp/shed-test-instances",
		SocketDir:       "/tmp/shed-test-sockets",
		DefaultCPUs:     2,
		DefaultMemoryMB: 4096,
		DefaultDiskGB:   20,
		VsockBaseCID:    100,
		ConsolePort:     1024,
		HealthPort:      1025,
		NotifyPort:      1026,
		StartTimeout:    Duration(30 * time.Second),
		StopTimeout:     Duration(10 * time.Second),
		BridgeName:      "shed-br0",
		BridgeCIDR:      "172.30.0.1/24",
		TAPPrefix:       "shed-tap",
	}
}

func TestFirecrackerConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*FirecrackerConfig)
		wantErr bool
	}{
		{
			name:    "valid defaults",
			modify:  func(c *FirecrackerConfig) {},
			wantErr: false,
		},
		// Upper bounds
		{
			name:    "cpus at max",
			modify:  func(c *FirecrackerConfig) { c.DefaultCPUs = MaxFirecrackerCPUs },
			wantErr: false,
		},
		{
			name:    "cpus over max",
			modify:  func(c *FirecrackerConfig) { c.DefaultCPUs = MaxFirecrackerCPUs + 1 },
			wantErr: true,
		},
		{
			name:    "memory at max",
			modify:  func(c *FirecrackerConfig) { c.DefaultMemoryMB = MaxFirecrackerMemoryMB },
			wantErr: false,
		},
		{
			name:    "memory over max",
			modify:  func(c *FirecrackerConfig) { c.DefaultMemoryMB = MaxFirecrackerMemoryMB + 1 },
			wantErr: true,
		},
		{
			name:    "disk at max",
			modify:  func(c *FirecrackerConfig) { c.DefaultDiskGB = MaxFirecrackerDiskGB },
			wantErr: false,
		},
		{
			name:    "disk over max",
			modify:  func(c *FirecrackerConfig) { c.DefaultDiskGB = MaxFirecrackerDiskGB + 1 },
			wantErr: true,
		},
		// CID bounds
		{
			name:    "vsock cid at max",
			modify:  func(c *FirecrackerConfig) { c.VsockBaseCID = MaxVsockCID },
			wantErr: false,
		},
		{
			name:    "vsock cid over max",
			modify:  func(c *FirecrackerConfig) { c.VsockBaseCID = MaxVsockCID + 1 },
			wantErr: true,
		},
		{
			name:    "vsock cid below min",
			modify:  func(c *FirecrackerConfig) { c.VsockBaseCID = 2 },
			wantErr: true,
		},
		// Port bounds
		{
			name:    "console port at max",
			modify:  func(c *FirecrackerConfig) { c.ConsolePort = MaxVsockPort },
			wantErr: false,
		},
		{
			name:    "console port over max",
			modify:  func(c *FirecrackerConfig) { c.ConsolePort = MaxVsockPort + 1 },
			wantErr: true,
		},
		{
			name:    "health port over max",
			modify:  func(c *FirecrackerConfig) { c.HealthPort = MaxVsockPort + 1 },
			wantErr: true,
		},
		// Timeout bounds
		{
			name:    "start timeout at min",
			modify:  func(c *FirecrackerConfig) { c.StartTimeout = Duration(MinTimeout) },
			wantErr: false,
		},
		{
			name:    "start timeout below min",
			modify:  func(c *FirecrackerConfig) { c.StartTimeout = Duration(500 * time.Millisecond) },
			wantErr: true,
		},
		{
			name:    "start timeout at max",
			modify:  func(c *FirecrackerConfig) { c.StartTimeout = Duration(MaxTimeout) },
			wantErr: false,
		},
		{
			name:    "start timeout over max",
			modify:  func(c *FirecrackerConfig) { c.StartTimeout = Duration(MaxTimeout + time.Second) },
			wantErr: true,
		},
		{
			name:    "stop timeout below min",
			modify:  func(c *FirecrackerConfig) { c.StopTimeout = Duration(100 * time.Millisecond) },
			wantErr: true,
		},
		{
			name:    "stop timeout over max",
			modify:  func(c *FirecrackerConfig) { c.StopTimeout = Duration(MaxTimeout + time.Minute) },
			wantErr: true,
		},
		{
			name:    "zero timeout allowed",
			modify:  func(c *FirecrackerConfig) { c.StartTimeout = 0; c.StopTimeout = 0 },
			wantErr: false,
		},
		// Path existence
		{
			name:    "kernel path missing",
			modify:  func(c *FirecrackerConfig) { c.KernelPath = "/nonexistent/vmlinux.bin" },
			wantErr: true,
		},
		{
			name:    "rootfs path missing",
			modify:  func(c *FirecrackerConfig) { c.BaseRootfs = "/nonexistent/rootfs.ext4" },
			wantErr: true,
		},
		// Lower bounds still work
		{
			name:    "cpus below min",
			modify:  func(c *FirecrackerConfig) { c.DefaultCPUs = 0 },
			wantErr: true,
		},
		{
			name:    "memory below min",
			modify:  func(c *FirecrackerConfig) { c.DefaultMemoryMB = 64 },
			wantErr: true,
		},
		{
			name:    "disk below min",
			modify:  func(c *FirecrackerConfig) { c.DefaultDiskGB = 0 },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFirecrackerConfig()
			tt.modify(cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// validVZConfig returns a VZConfig that passes validation.
// It uses /dev/null for file paths since those must exist on disk.
func validVZConfig() *VZConfig {
	return &VZConfig{
		VfkitPath:       "vfkit",
		KernelPath:      "/dev/null",
		BaseRootfs:      "/dev/null",
		InstanceDir:     "/tmp/shed-test-vz-instances",
		SocketDir:       "/tmp/shed-test-vz-sockets",
		DefaultCPUs:     2,
		DefaultMemoryMB: 4096,
		DefaultDiskGB:   20,
		ConsolePort:     1024,
		HealthPort:      1025,
		NotifyPort:      1026,
		StartTimeout:    Duration(60 * time.Second),
		StopTimeout:     Duration(10 * time.Second),
	}
}

func TestVZConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*VZConfig)
		wantErr bool
	}{
		{
			name:    "valid defaults",
			modify:  func(c *VZConfig) {},
			wantErr: false,
		},
		// Required fields
		{
			name:    "missing vfkit_path",
			modify:  func(c *VZConfig) { c.VfkitPath = "" },
			wantErr: true,
		},
		{
			name:    "missing kernel_path",
			modify:  func(c *VZConfig) { c.KernelPath = "" },
			wantErr: true,
		},
		{
			name:    "missing base_rootfs",
			modify:  func(c *VZConfig) { c.BaseRootfs = "" },
			wantErr: true,
		},
		{
			name:    "missing instance_dir",
			modify:  func(c *VZConfig) { c.InstanceDir = "" },
			wantErr: true,
		},
		{
			name:    "missing socket_dir",
			modify:  func(c *VZConfig) { c.SocketDir = "" },
			wantErr: true,
		},
		// CPU and memory bounds
		{
			name:    "cpus below min",
			modify:  func(c *VZConfig) { c.DefaultCPUs = 0 },
			wantErr: true,
		},
		{
			name:    "cpus over max",
			modify:  func(c *VZConfig) { c.DefaultCPUs = MaxVZCPUs + 1 },
			wantErr: true,
		},
		{
			name:    "memory below min",
			modify:  func(c *VZConfig) { c.DefaultMemoryMB = 64 },
			wantErr: true,
		},
		{
			name:    "memory over max",
			modify:  func(c *VZConfig) { c.DefaultMemoryMB = MaxVZMemoryMB + 1 },
			wantErr: true,
		},
		{
			name:    "disk below min",
			modify:  func(c *VZConfig) { c.DefaultDiskGB = 0 },
			wantErr: true,
		},
		{
			name:    "disk over max",
			modify:  func(c *VZConfig) { c.DefaultDiskGB = MaxVZDiskGB + 1 },
			wantErr: true,
		},
		// Port validation
		{
			name:    "console port zero",
			modify:  func(c *VZConfig) { c.ConsolePort = 0 },
			wantErr: true,
		},
		{
			name:    "console port over max",
			modify:  func(c *VZConfig) { c.ConsolePort = MaxVsockPort + 1 },
			wantErr: true,
		},
		{
			name:    "health port zero",
			modify:  func(c *VZConfig) { c.HealthPort = 0 },
			wantErr: true,
		},
		{
			name:    "notify port zero",
			modify:  func(c *VZConfig) { c.NotifyPort = 0 },
			wantErr: true,
		},
		{
			name:    "duplicate ports",
			modify:  func(c *VZConfig) { c.ConsolePort = 1024; c.HealthPort = 1024 },
			wantErr: true,
		},
		// Timeout validation
		{
			name:    "start timeout below min",
			modify:  func(c *VZConfig) { c.StartTimeout = Duration(500 * time.Millisecond) },
			wantErr: true,
		},
		{
			name:    "start timeout over max",
			modify:  func(c *VZConfig) { c.StartTimeout = Duration(MaxTimeout + time.Second) },
			wantErr: true,
		},
		{
			name:    "zero timeout allowed",
			modify:  func(c *VZConfig) { c.StartTimeout = 0; c.StopTimeout = 0 },
			wantErr: false,
		},
		// Path existence
		{
			name:    "kernel path missing",
			modify:  func(c *VZConfig) { c.KernelPath = "/nonexistent/vmlinux" },
			wantErr: true,
		},
		{
			name:    "rootfs path missing",
			modify:  func(c *VZConfig) { c.BaseRootfs = "/nonexistent/rootfs.ext4" },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validVZConfig()
			tt.modify(cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDefaultVZConfig(t *testing.T) {
	cfg := DefaultVZConfig()

	if cfg.VfkitPath != "vfkit" {
		t.Errorf("VfkitPath = %q, want %q", cfg.VfkitPath, "vfkit")
	}
	if cfg.DefaultCPUs != 2 {
		t.Errorf("DefaultCPUs = %d, want 2", cfg.DefaultCPUs)
	}
	if cfg.DefaultMemoryMB != 4096 {
		t.Errorf("DefaultMemoryMB = %d, want 4096", cfg.DefaultMemoryMB)
	}
	if cfg.ConsolePort != 1024 {
		t.Errorf("ConsolePort = %d, want 1024", cfg.ConsolePort)
	}
	if cfg.HealthPort != 1025 {
		t.Errorf("HealthPort = %d, want 1025", cfg.HealthPort)
	}
	if cfg.NotifyPort != 1026 {
		t.Errorf("NotifyPort = %d, want 1026", cfg.NotifyPort)
	}
}

func TestMountConfigMatchesExclude(t *testing.T) {
	m := MountConfig{
		Exclude: []string{"*.db", "*.db-shm", "*.db-wal", "log/*", "storage/*"},
	}

	tests := []struct {
		relPath string
		want    bool
	}{
		{"opencode.db", true},
		{"opencode.db-shm", true},
		{"opencode.db-wal", true},
		{"log/output.log", true},
		{"storage/data.bin", true},
		{"auth.json", false},
		{"config.yaml", false},
		{"nested/auth.json", false},
		// Patterns only match the final path component by default
		{"nested/opencode.db", false},
	}

	for _, tt := range tests {
		t.Run(tt.relPath, func(t *testing.T) {
			got := m.MatchesExclude(tt.relPath)
			if got != tt.want {
				t.Errorf("MatchesExclude(%q) = %v, want %v", tt.relPath, got, tt.want)
			}
		})
	}
}

func TestMountConfigMatchesExcludeEmpty(t *testing.T) {
	m := MountConfig{}
	if m.MatchesExclude("anything.db") {
		t.Error("MatchesExclude should return false with no patterns")
	}
}

func TestVZConfigApplyDefaults(t *testing.T) {
	cfg := &VZConfig{}
	cfg.applyDefaults()

	if cfg.NotifyPort != 1026 {
		t.Errorf("NotifyPort = %d after applyDefaults, want 1026", cfg.NotifyPort)
	}

	// Should not overwrite non-zero value
	cfg2 := &VZConfig{NotifyPort: 2000}
	cfg2.applyDefaults()
	if cfg2.NotifyPort != 2000 {
		t.Errorf("NotifyPort = %d after applyDefaults, want 2000", cfg2.NotifyPort)
	}
}
