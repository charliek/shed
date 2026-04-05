package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

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
	if cfg.DefaultBackend != BackendDetect {
		t.Errorf("DefaultBackend = %q, want %q", cfg.DefaultBackend, BackendDetect)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
}

func TestResolveBackend(t *testing.T) {
	t.Run("detect on darwin/arm64", func(t *testing.T) {
		got, err := ResolveBackend(BackendDetect, "darwin", "arm64")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != BackendVZ {
			t.Errorf("ResolveBackend = %q, want %q", got, BackendVZ)
		}
	})

	t.Run("detect on linux", func(t *testing.T) {
		got, err := ResolveBackend(BackendDetect, "linux", "amd64")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != BackendFirecracker {
			t.Errorf("ResolveBackend = %q, want %q", got, BackendFirecracker)
		}
	})

	t.Run("detect on unsupported platform", func(t *testing.T) {
		_, err := ResolveBackend(BackendDetect, "windows", "amd64")
		if err == nil {
			t.Fatal("expected error for unsupported platform")
		}
	})

	t.Run("explicit backend passes through", func(t *testing.T) {
		got, err := ResolveBackend(BackendVZ, "linux", "amd64")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != BackendVZ {
			t.Errorf("ResolveBackend = %q, want %q", got, BackendVZ)
		}
	})

	t.Run("docker is not valid", func(t *testing.T) {
		if isValidBackend("docker") {
			t.Error("docker should not be a valid backend")
		}
	})
}

func TestServerConfigValidation(t *testing.T) {
	// Use the platform-appropriate backend for a valid config
	validBackend, validBackendCfg := platformTestBackend(t)

	validConfig := &ServerConfig{
		Name:           "test",
		HTTPPort:       8080,
		SSHPort:        2222,
		LogLevel:       "info",
		DefaultBackend: validBackend,
		Firecracker:    validBackendCfg.fc,
		VZ:             validBackendCfg.vz,
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
			cfg:     &ServerConfig{HTTPPort: 8080, SSHPort: 2222, LogLevel: "info", DefaultBackend: validBackend, Firecracker: validBackendCfg.fc, VZ: validBackendCfg.vz},
			wantErr: true,
		},
		{
			name:    "invalid http port",
			cfg:     &ServerConfig{Name: "test", HTTPPort: 0, SSHPort: 2222, LogLevel: "info", DefaultBackend: validBackend, Firecracker: validBackendCfg.fc, VZ: validBackendCfg.vz},
			wantErr: true,
		},
		{
			name:    "invalid log level",
			cfg:     &ServerConfig{Name: "test", HTTPPort: 8080, SSHPort: 2222, LogLevel: "invalid", DefaultBackend: validBackend, Firecracker: validBackendCfg.fc, VZ: validBackendCfg.vz},
			wantErr: true,
		},
		{
			name:    "docker backend rejected",
			cfg:     &ServerConfig{Name: "test", HTTPPort: 8080, SSHPort: 2222, LogLevel: "info", DefaultBackend: "docker"},
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

// backendConfigs holds platform-specific backend configs for tests.
type backendConfigs struct {
	fc *FirecrackerConfig
	vz *VZConfig
}

// platformTestBackend returns the backend type and config appropriate for the current platform.
func platformTestBackend(t *testing.T) (string, backendConfigs) {
	t.Helper()
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return BackendVZ, backendConfigs{vz: validVZConfig()}
	case runtime.GOOS == "linux":
		return BackendFirecracker, backendConfigs{fc: validFirecrackerConfig()}
	default:
		t.Skipf("no backend available for %s/%s", runtime.GOOS, runtime.GOARCH)
		return "", backendConfigs{}
	}
}

func TestServerConfigVZPlatformValidation(t *testing.T) {
	cfg := &ServerConfig{
		Name:           "test",
		HTTPPort:       8080,
		SSHPort:        2222,
		LogLevel:       "info",
		DefaultBackend: BackendVZ,
		VZ:             validVZConfig(),
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
			name:    "notify port zero",
			modify:  func(c *VZConfig) { c.NotifyPort = 0 },
			wantErr: true,
		},
		{
			name:    "duplicate ports",
			modify:  func(c *VZConfig) { c.ConsolePort = 1026; c.NotifyPort = 1026 },
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
	if cfg.NotifyPort != 1026 {
		t.Errorf("NotifyPort = %d, want 1026", cfg.NotifyPort)
	}
}

func TestCredentialMountTag(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"claude", "cred-claude"},
		{"git_ssh", "cred-git_ssh"},
		{"gh", "cred-gh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CredentialMountTag(tt.name)
			if got != tt.want {
				t.Errorf("CredentialMountTag(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestCredentialNameValidation(t *testing.T) {
	tests := []struct {
		name  string
		valid bool
	}{
		{"claude", true},
		{"git_ssh", true},
		{"git-config", true},
		{"MyApp123", true},
		{"a", true},
		{"has space", false},
		{"has,comma", false},
		{"has.dot", false},
		{"has/slash", false},
		{"-leading-hyphen", false},
		{"_leading-underscore", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validCredentialName.MatchString(tt.name)
			if got != tt.valid {
				t.Errorf("validCredentialName.MatchString(%q) = %v, want %v", tt.name, got, tt.valid)
			}
		})
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
		{"log", true},
		{"storage", true},
		{"log/sub/deep/file", true},
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

func TestCredentialSourceMustBeDirectory(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file and a directory for testing
	filePath := filepath.Join(tmpDir, "gitconfig")
	if err := os.WriteFile(filePath, []byte("[user]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	dirPath := filepath.Join(tmpDir, "ssh")
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a symlink to the directory
	symlinkPath := filepath.Join(tmpDir, "ssh-link")
	if err := os.Symlink(dirPath, symlinkPath); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		source    string
		wantError bool
	}{
		{
			name:      "directory_source_accepted",
			source:    dirPath,
			wantError: false,
		},
		{
			name:      "file_source_rejected",
			source:    filePath,
			wantError: true,
		},
		{
			name:      "nonexistent_source_accepted",
			source:    filepath.Join(tmpDir, "does-not-exist"),
			wantError: false,
		},
		{
			name:      "symlink_to_directory_accepted",
			source:    symlinkPath,
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend, bcfg := platformTestBackend(t)
			cfgYAML := "name: test-server\ndefault_backend: " + backend + "\ncredentials:\n  testcred:\n    source: " + tt.source + "\n    target: /home/shed/.test\n"
			if bcfg.vz != nil {
				cfgYAML += "vz:\n  vfkit_path: vfkit\n  kernel_path: /dev/null\n  base_rootfs: /dev/null\n  instance_dir: /tmp/test-instances\n  socket_dir: /tmp/test-sockets\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  console_port: 1024\n  notify_port: 1026\n"
			}
			if bcfg.fc != nil {
				cfgYAML += "firecracker:\n  kernel_path: /tmp/vmlinux\n  base_rootfs: /tmp/test-rootfs.ext4\n  instance_dir: /tmp/test-instances\n  images_dir: /tmp/test-images\n"
			}
			cfgPath := filepath.Join(t.TempDir(), "server.yaml")
			if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadServerConfigFromPath(cfgPath)
			if tt.wantError && err == nil {
				t.Error("expected error for file source, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantError && err != nil && !strings.Contains(err.Error(), "not a directory") {
				t.Errorf("error should mention 'not a directory', got: %v", err)
			}
		})
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

func TestVZConfigApplyDefaultsExpandsImagePaths(t *testing.T) {
	cfg := &VZConfig{
		Images: map[string]string{
			"base":    "~/shed/base.ext4",
			"default": "/absolute/path.ext4",
		},
	}
	cfg.applyDefaults()

	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "shed/base.ext4")
	if cfg.Images["base"] != want {
		t.Errorf("Images[base] = %q, want %q", cfg.Images["base"], want)
	}
	if cfg.Images["default"] != "/absolute/path.ext4" {
		t.Errorf("Images[default] = %q, want %q", cfg.Images["default"], "/absolute/path.ext4")
	}
}

func TestVZConfigResolveImage(t *testing.T) {
	cfg := &VZConfig{
		Images: map[string]string{
			"base":         "/dev/null",
			"default":      "/dev/null",
			"experimental": "/dev/null",
		},
	}

	t.Run("named variant", func(t *testing.T) {
		resolved, err := cfg.ResolveImage("base")
		if err != nil {
			t.Fatalf("ResolveImage(base) error = %v", err)
		}
		if resolved.Path != "/dev/null" {
			t.Errorf("ResolveImage(base).Path = %q, want /dev/null", resolved.Path)
		}
	})

	t.Run("absolute path exists", func(t *testing.T) {
		resolved, err := cfg.ResolveImage("/dev/null")
		if err != nil {
			t.Fatalf("ResolveImage(/dev/null) error = %v", err)
		}
		if resolved.Path != "/dev/null" {
			t.Errorf("ResolveImage(/dev/null).Path = %q, want /dev/null", resolved.Path)
		}
	})

	t.Run("absolute path missing", func(t *testing.T) {
		_, err := cfg.ResolveImage("/nonexistent/file.ext4")
		if err == nil {
			t.Fatal("ResolveImage should fail for missing absolute path")
		}
		if !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("error = %q, want 'does not exist'", err.Error())
		}
	})

	t.Run("unknown variant", func(t *testing.T) {
		_, err := cfg.ResolveImage("rust")
		if err == nil {
			t.Fatal("ResolveImage should fail for unknown variant")
		}
		if !strings.Contains(err.Error(), "unknown image") {
			t.Errorf("error = %q, want 'unknown image'", err.Error())
		}
		if !strings.Contains(err.Error(), "base") {
			t.Errorf("error should list available variants, got: %q", err.Error())
		}
	})

	t.Run("tilde path expands", func(t *testing.T) {
		// ~/.. should be expanded and treated as an absolute path
		resolved, err := cfg.ResolveImage("~/../../dev/null")
		if err != nil {
			t.Fatalf("ResolveImage(~/../../dev/null) error = %v", err)
		}
		if !filepath.IsAbs(resolved.Path) {
			t.Errorf("expected absolute path, got %q", resolved.Path)
		}
	})

	t.Run("empty images map", func(t *testing.T) {
		emptyCfg := &VZConfig{Images: map[string]string{}}
		_, err := emptyCfg.ResolveImage("anything")
		if err == nil {
			t.Fatal("ResolveImage should fail with empty images map")
		}
		if !strings.Contains(err.Error(), "no image variants configured") {
			t.Errorf("error = %q, want 'no image variants configured'", err.Error())
		}
	})
}

func TestVZConfigValidateImages(t *testing.T) {
	t.Run("valid image paths", func(t *testing.T) {
		cfg := validVZConfig()
		cfg.Images = map[string]string{
			"base": "/dev/null",
		}
		err := cfg.Validate()
		if err != nil {
			t.Errorf("Validate() error = %v, want nil", err)
		}
	})

	t.Run("invalid image path", func(t *testing.T) {
		cfg := validVZConfig()
		cfg.Images = map[string]string{
			"base": "/nonexistent/rootfs.ext4",
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate() should fail for missing image path")
		}
		if !strings.Contains(err.Error(), "image \"base\" path does not exist") {
			t.Errorf("error = %q, want image path error", err.Error())
		}
	})

	t.Run("docker ref skips validation", func(t *testing.T) {
		cfg := validVZConfig()
		cfg.Images = map[string]string{
			"custom": "ghcr.io/charliek/shed-vz-custom:v1.0.0",
		}
		err := cfg.Validate()
		if err != nil {
			t.Errorf("Validate() error = %v, want nil (Docker refs should skip path validation)", err)
		}
	})

	t.Run("docker ref base_rootfs skips validation", func(t *testing.T) {
		cfg := validVZConfig()
		cfg.BaseRootfs = "ghcr.io/charliek/shed-vz-default:v1.0.0"
		err := cfg.Validate()
		if err != nil {
			t.Errorf("Validate() error = %v, want nil (Docker ref base_rootfs should skip validation)", err)
		}
	})

	t.Run("mixed docker refs and paths", func(t *testing.T) {
		cfg := validVZConfig()
		cfg.Images = map[string]string{
			"local":  "/dev/null",
			"remote": "ghcr.io/charliek/shed-vz-default:v1.0.0",
		}
		err := cfg.Validate()
		if err != nil {
			t.Errorf("Validate() error = %v, want nil", err)
		}
	})
}

func TestVZConfigResolveImageDockerRef(t *testing.T) {
	t.Run("docker ref returns DockerRef field", func(t *testing.T) {
		cfg := &VZConfig{
			Images:    map[string]string{"default": "ghcr.io/charliek/shed-vz-default:v1.0.0"},
			ImagesDir: t.TempDir(),
		}
		resolved, err := cfg.ResolveImage("default")
		if err != nil {
			t.Fatalf("ResolveImage error = %v", err)
		}
		if resolved.DockerRef != "ghcr.io/charliek/shed-vz-default:v1.0.0" {
			t.Errorf("DockerRef = %q, want ghcr.io/charliek/shed-vz-default:v1.0.0", resolved.DockerRef)
		}
		if resolved.Path != "" {
			t.Errorf("Path = %q, want empty for uncached Docker ref", resolved.Path)
		}
		if resolved.Name != "default" {
			t.Errorf("Name = %q, want default", resolved.Name)
		}
	})

	t.Run("cached docker ref returns Path", func(t *testing.T) {
		dir := t.TempDir()
		// Create cached rootfs and source sidecar
		rootfsPath := filepath.Join(dir, "default-rootfs.ext4")
		os.WriteFile(rootfsPath, []byte("fake"), 0644)
		sourceFile := filepath.Join(dir, "default-rootfs.ext4.source")
		os.WriteFile(sourceFile, []byte("ghcr.io/charliek/shed-vz-default:v1.0.0\n"), 0644)

		cfg := &VZConfig{
			Images:    map[string]string{"default": "ghcr.io/charliek/shed-vz-default:v1.0.0"},
			ImagesDir: dir,
		}
		resolved, err := cfg.ResolveImage("default")
		if err != nil {
			t.Fatalf("ResolveImage error = %v", err)
		}
		if resolved.Path != rootfsPath {
			t.Errorf("Path = %q, want %q", resolved.Path, rootfsPath)
		}
		if resolved.DockerRef != "" {
			t.Errorf("DockerRef = %q, want empty for cached image", resolved.DockerRef)
		}
	})

	t.Run("stale cache triggers re-pull", func(t *testing.T) {
		dir := t.TempDir()
		rootfsPath := filepath.Join(dir, "default-rootfs.ext4")
		os.WriteFile(rootfsPath, []byte("fake"), 0644)
		sourceFile := filepath.Join(dir, "default-rootfs.ext4.source")
		os.WriteFile(sourceFile, []byte("ghcr.io/charliek/shed-vz-default:v1.0.0\n"), 0644)

		cfg := &VZConfig{
			Images:    map[string]string{"default": "ghcr.io/charliek/shed-vz-default:v2.0.0"},
			ImagesDir: dir,
		}
		resolved, err := cfg.ResolveImage("default")
		if err != nil {
			t.Fatalf("ResolveImage error = %v", err)
		}
		if resolved.DockerRef != "ghcr.io/charliek/shed-vz-default:v2.0.0" {
			t.Errorf("DockerRef = %q, want v2.0.0 (stale cache should trigger re-pull)", resolved.DockerRef)
		}
	})

	t.Run("auto-discover from ImagesDir", func(t *testing.T) {
		dir := t.TempDir()
		rootfsPath := filepath.Join(dir, "custom-rootfs.ext4")
		os.WriteFile(rootfsPath, []byte("fake"), 0644)

		cfg := &VZConfig{
			Images:    map[string]string{},
			ImagesDir: dir,
		}
		resolved, err := cfg.ResolveImage("custom")
		if err != nil {
			t.Fatalf("ResolveImage error = %v", err)
		}
		if resolved.Path != rootfsPath {
			t.Errorf("Path = %q, want %q (auto-discovered)", resolved.Path, rootfsPath)
		}
	})

	t.Run("config takes precedence over discovery", func(t *testing.T) {
		dir := t.TempDir()
		// Create both a discovered file and a config entry
		os.WriteFile(filepath.Join(dir, "base-rootfs.ext4"), []byte("discovered"), 0644)

		cfg := &VZConfig{
			Images:    map[string]string{"base": "/dev/null"},
			ImagesDir: dir,
		}
		resolved, err := cfg.ResolveImage("base")
		if err != nil {
			t.Fatalf("ResolveImage error = %v", err)
		}
		if resolved.Path != "/dev/null" {
			t.Errorf("Path = %q, want /dev/null (config should take precedence)", resolved.Path)
		}
	})
}

func TestFirecrackerConfigResolveImage(t *testing.T) {
	cfg := &FirecrackerConfig{
		Images: map[string]string{
			"base":         "/dev/null",
			"default":      "/dev/null",
			"experimental": "/dev/null",
		},
	}

	t.Run("named variant", func(t *testing.T) {
		resolved, err := cfg.ResolveImage("base")
		if err != nil {
			t.Fatalf("ResolveImage(base) error = %v", err)
		}
		if resolved.Path != "/dev/null" {
			t.Errorf("ResolveImage(base).Path = %q, want /dev/null", resolved.Path)
		}
	})

	t.Run("absolute path exists", func(t *testing.T) {
		resolved, err := cfg.ResolveImage("/dev/null")
		if err != nil {
			t.Fatalf("ResolveImage(/dev/null) error = %v", err)
		}
		if resolved.Path != "/dev/null" {
			t.Errorf("ResolveImage(/dev/null).Path = %q, want /dev/null", resolved.Path)
		}
	})

	t.Run("unknown variant", func(t *testing.T) {
		_, err := cfg.ResolveImage("rust")
		if err == nil {
			t.Fatal("ResolveImage should fail for unknown variant")
		}
		if !strings.Contains(err.Error(), "unknown image") {
			t.Errorf("error = %q, want 'unknown image'", err.Error())
		}
		if !strings.Contains(err.Error(), "base") {
			t.Errorf("error should list available variants, got: %q", err.Error())
		}
	})

	t.Run("empty images map", func(t *testing.T) {
		emptyCfg := &FirecrackerConfig{Images: map[string]string{}}
		_, err := emptyCfg.ResolveImage("anything")
		if err == nil {
			t.Fatal("ResolveImage should fail with empty images map")
		}
		if !strings.Contains(err.Error(), "no image variants configured") {
			t.Errorf("error = %q, want 'no image variants configured'", err.Error())
		}
		if !strings.Contains(err.Error(), "firecracker.images") {
			t.Errorf("error should mention firecracker.images, got: %q", err.Error())
		}
	})
}

func TestFirecrackerConfigResolveImageDockerRef(t *testing.T) {
	t.Run("docker ref returns DockerRef field", func(t *testing.T) {
		cfg := &FirecrackerConfig{
			Images:    map[string]string{"default": "ghcr.io/charliek/shed-fc-default:v1.0.0"},
			ImagesDir: t.TempDir(),
		}
		resolved, err := cfg.ResolveImage("default")
		if err != nil {
			t.Fatalf("ResolveImage error = %v", err)
		}
		if resolved.DockerRef != "ghcr.io/charliek/shed-fc-default:v1.0.0" {
			t.Errorf("DockerRef = %q, want ghcr.io/charliek/shed-fc-default:v1.0.0", resolved.DockerRef)
		}
		if resolved.Path != "" {
			t.Errorf("Path = %q, want empty for uncached Docker ref", resolved.Path)
		}
	})

	t.Run("cached docker ref returns Path", func(t *testing.T) {
		dir := t.TempDir()
		rootfsPath := filepath.Join(dir, "default-rootfs.ext4")
		os.WriteFile(rootfsPath, []byte("fake"), 0644)
		sourceFile := filepath.Join(dir, "default-rootfs.ext4.source")
		os.WriteFile(sourceFile, []byte("ghcr.io/charliek/shed-fc-default:v1.0.0\n"), 0644)

		cfg := &FirecrackerConfig{
			Images:    map[string]string{"default": "ghcr.io/charliek/shed-fc-default:v1.0.0"},
			ImagesDir: dir,
		}
		resolved, err := cfg.ResolveImage("default")
		if err != nil {
			t.Fatalf("ResolveImage error = %v", err)
		}
		if resolved.Path != rootfsPath {
			t.Errorf("Path = %q, want %q", resolved.Path, rootfsPath)
		}
	})

	t.Run("auto-discover from ImagesDir", func(t *testing.T) {
		dir := t.TempDir()
		rootfsPath := filepath.Join(dir, "custom-rootfs.ext4")
		os.WriteFile(rootfsPath, []byte("fake"), 0644)

		cfg := &FirecrackerConfig{
			Images:    map[string]string{},
			ImagesDir: dir,
		}
		resolved, err := cfg.ResolveImage("custom")
		if err != nil {
			t.Fatalf("ResolveImage error = %v", err)
		}
		if resolved.Path != rootfsPath {
			t.Errorf("Path = %q, want %q (auto-discovered)", resolved.Path, rootfsPath)
		}
	})
}

func TestFirecrackerConfigResolveBaseRootfs(t *testing.T) {
	t.Run("local path", func(t *testing.T) {
		cfg := &FirecrackerConfig{BaseRootfs: "/var/lib/shed/firecracker/base-rootfs.ext4"}
		resolved := cfg.ResolveBaseRootfs()
		if resolved.Path != "/var/lib/shed/firecracker/base-rootfs.ext4" {
			t.Errorf("Path = %q", resolved.Path)
		}
		if resolved.DockerRef != "" {
			t.Errorf("DockerRef = %q, want empty", resolved.DockerRef)
		}
	})

	t.Run("docker ref", func(t *testing.T) {
		cfg := &FirecrackerConfig{
			BaseRootfs: "ghcr.io/charliek/shed-fc-default:v1.0.0",
			ImagesDir:  t.TempDir(),
		}
		resolved := cfg.ResolveBaseRootfs()
		if resolved.DockerRef != "ghcr.io/charliek/shed-fc-default:v1.0.0" {
			t.Errorf("DockerRef = %q", resolved.DockerRef)
		}
		if resolved.Name != "_base" {
			t.Errorf("Name = %q, want _base", resolved.Name)
		}
	})
}

func TestFirecrackerConfigValidateDockerRef(t *testing.T) {
	t.Run("docker ref in BaseRootfs passes validation", func(t *testing.T) {
		cfg := validFirecrackerConfig()
		cfg.BaseRootfs = "ghcr.io/charliek/shed-fc-default:v1.0.0"
		err := cfg.Validate()
		if err != nil {
			t.Errorf("Validate() error = %v, want nil (Docker refs should skip path validation)", err)
		}
	})

	t.Run("docker ref in Images skips validation", func(t *testing.T) {
		cfg := validFirecrackerConfig()
		cfg.Images = map[string]string{
			"custom": "ghcr.io/charliek/shed-fc-custom:v1.0.0",
		}
		err := cfg.Validate()
		if err != nil {
			t.Errorf("Validate() error = %v, want nil", err)
		}
	})

	t.Run("invalid image path fails validation", func(t *testing.T) {
		cfg := validFirecrackerConfig()
		cfg.Images = map[string]string{
			"base": "/nonexistent/rootfs.ext4",
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate() should fail for missing image path")
		}
		if !strings.Contains(err.Error(), "image \"base\" path does not exist") {
			t.Errorf("error = %q, want image path error", err.Error())
		}
	})

	t.Run("kernel_path validation deferred with Docker refs", func(t *testing.T) {
		cfg := validFirecrackerConfig()
		cfg.KernelPath = "/nonexistent/vmlinux"
		cfg.BaseRootfs = "ghcr.io/charliek/shed-fc-base:v1.0.0"
		err := cfg.Validate()
		if err != nil {
			t.Errorf("Validate() error = %v, want nil (kernel_path check deferred for Docker refs)", err)
		}
	})

	t.Run("kernel_path validated without Docker refs", func(t *testing.T) {
		cfg := validFirecrackerConfig()
		cfg.KernelPath = "/nonexistent/vmlinux"
		err := cfg.Validate()
		if err == nil {
			t.Fatal("Validate() should fail for missing kernel_path without Docker refs")
		}
		if !strings.Contains(err.Error(), "kernel_path does not exist") {
			t.Errorf("error = %q, want kernel_path error", err.Error())
		}
	})
}
