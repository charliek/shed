package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/vmimage"
)

// installCachedBlob installs a synthetic OCI image into imagesDir under
// the given tag with the given source-ref annotation. Returns the path
// to the manifest's prebuilt rootfs erofs blob — the path that config
// resolution surfaces to callers as ResolvedImage.Path (v0.5.2+).
func installCachedBlob(t *testing.T, imagesDir, tag, sourceRef string) string {
	t.Helper()
	body := []byte("fake-" + tag)
	digest, err := vmimage.InstallSyntheticImage(imagesDir, tag, sourceRef, body, nil, nil)
	if err != nil {
		t.Fatalf("InstallSyntheticImage: %v", err)
	}
	manifest, err := vmimage.LoadManifestByDigest(imagesDir, digest)
	if err != nil {
		t.Fatalf("LoadManifestByDigest: %v", err)
	}
	erofsDigest := manifest.ShedRootfsErofsDigest()
	if erofsDigest == "" {
		t.Fatalf("synthetic manifest %s missing %s annotation — InstallSyntheticImage should populate it", digest, vmimage.AnnotationRootfsErofsDigest)
	}
	path, err := vmimage.BlobPath(imagesDir, erofsDigest)
	if err != nil {
		t.Fatalf("BlobPath(erofs): %v", err)
	}
	return path
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
		DefaultImage:    "/dev/null",
		InstanceDir:     "/tmp/shed-test-instances",
		SnapshotsDir:    "/tmp/shed-test-snapshots",
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
			modify:  func(c *FirecrackerConfig) { c.DefaultImage = "/nonexistent/rootfs.ext4" },
			wantErr: true,
		},
		{
			// Empty base_rootfs is allowed under the content-addressed
			// model — see Validate header.
			name:    "empty base_rootfs is allowed",
			modify:  func(c *FirecrackerConfig) { c.DefaultImage = "" },
			wantErr: false,
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
		DefaultImage:    "/dev/null",
		InstanceDir:     "/tmp/shed-test-vz-instances",
		SnapshotsDir:    "/tmp/shed-test-vz-snapshots",
		SocketDir:       "/tmp/shed-test-vz-sockets",
		DefaultCPUs:     2,
		DefaultMemoryMB: 4096,
		DefaultDiskGB:   20,
		ConsolePort:     1024,
		NotifyPort:      1026,
		TCPProxyPort:    1028,
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
			// kernel_path is optional under Phase B — vm.Start prefers
			// the kernel inside the blob.
			name:    "missing kernel_path is allowed",
			modify:  func(c *VZConfig) { c.KernelPath = "" },
			wantErr: false,
		},
		{
			// base_rootfs is now optional under the content-addressed
			// model — empty is valid; create-without-image errors later
			// in the CreateShed path with a clear message.
			name:    "missing base_rootfs is allowed",
			modify:  func(c *VZConfig) { c.DefaultImage = "" },
			wantErr: false,
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
			name:    "tcp proxy port zero",
			modify:  func(c *VZConfig) { c.TCPProxyPort = 0 },
			wantErr: true,
		},
		{
			name:    "tcp proxy port over max",
			modify:  func(c *VZConfig) { c.TCPProxyPort = MaxVsockPort + 1 },
			wantErr: true,
		},
		{
			name:    "duplicate console and notify ports",
			modify:  func(c *VZConfig) { c.ConsolePort = 1026; c.NotifyPort = 1026 },
			wantErr: true,
		},
		{
			name:    "duplicate console and tcp proxy ports",
			modify:  func(c *VZConfig) { c.TCPProxyPort = c.ConsolePort },
			wantErr: true,
		},
		{
			name:    "duplicate notify and tcp proxy ports",
			modify:  func(c *VZConfig) { c.TCPProxyPort = c.NotifyPort },
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
			modify:  func(c *VZConfig) { c.DefaultImage = "/nonexistent/rootfs.ext4" },
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
	if cfg.TCPProxyPort != 1028 {
		t.Errorf("TCPProxyPort = %d, want 1028", cfg.TCPProxyPort)
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
				cfgYAML += "vz:\n  vfkit_path: vfkit\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: /tmp/test-instances\n  socket_dir: /tmp/test-sockets\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  console_port: 1024\n  notify_port: 1026\n"
			}
			if bcfg.fc != nil {
				cfgYAML += "firecracker:\n  kernel_path: /dev/null\n  default_image: /dev/null\n  instance_dir: /tmp/test-instances\n  socket_dir: /tmp/test-sockets\n  default_cpus: 2\n  default_memory_mb: 4096\n  default_disk_gb: 20\n  vsock_base_cid: 100\n  console_port: 1024\n  notify_port: 1026\n  bridge_name: shed-br0\n  bridge_cidr: 172.30.0.1/24\n  tap_prefix: shed-tap\n"
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

	if cfg.TCPProxyPort != 1028 {
		t.Errorf("TCPProxyPort = %d after applyDefaults, want 1028", cfg.TCPProxyPort)
	}

	// Should not overwrite non-zero values
	cfg2 := &VZConfig{NotifyPort: 2000, TCPProxyPort: 2002}
	cfg2.applyDefaults()
	if cfg2.NotifyPort != 2000 {
		t.Errorf("NotifyPort = %d after applyDefaults, want 2000", cfg2.NotifyPort)
	}
	if cfg2.TCPProxyPort != 2002 {
		t.Errorf("TCPProxyPort = %d after applyDefaults, want 2002", cfg2.TCPProxyPort)
	}
}
