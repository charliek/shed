package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/charliek/shed/internal/terminal"
)

// ServerConfig represents the server-side configuration.
type ServerConfig struct {
	Name         string                 `yaml:"name"`
	HTTPPort     int                    `yaml:"http_port"`
	SSHPort      int                    `yaml:"ssh_port"`
	DefaultImage string                 `yaml:"default_image"`
	Credentials  map[string]MountConfig `yaml:"credentials"`
	EnvFile      string                 `yaml:"env_file"`
	LogLevel     string                 `yaml:"log_level"`
	Terminal     *terminal.Config       `yaml:"terminal"`

	// EnabledBackends specifies the backend types this server supports.
	EnabledBackends []string `yaml:"enabled_backends,omitempty"`

	// DefaultBackend specifies the default backend type: "docker" or "firecracker"
	DefaultBackend string `yaml:"default_backend,omitempty"`

	// Backend is deprecated (use default_backend instead).
	Backend string `yaml:"backend,omitempty"`

	// Firecracker contains Firecracker-specific configuration
	Firecracker *FirecrackerConfig `yaml:"firecracker,omitempty"`

	// Loaded environment variables (not from YAML)
	EnvVars map[string]string `yaml:"-"`
}

// FirecrackerConfig contains Firecracker-specific configuration.
type FirecrackerConfig struct {
	// KernelPath is the path to the Linux kernel image
	KernelPath string `yaml:"kernel_path"`

	// BaseRootfs is the path to the base rootfs image
	BaseRootfs string `yaml:"base_rootfs"`

	// InstanceDir is the directory for instance data
	InstanceDir string `yaml:"instance_dir"`

	// SocketDir is the directory for Firecracker API sockets
	SocketDir string `yaml:"socket_dir"`

	// DefaultCPUs is the default number of vCPUs for new VMs
	DefaultCPUs int `yaml:"default_cpus"`

	// DefaultMemoryMB is the default memory in MB for new VMs
	DefaultMemoryMB int `yaml:"default_memory_mb"`

	// DefaultDiskGB is the default disk size in GB for new VMs
	DefaultDiskGB int `yaml:"default_disk_gb"`

	// VsockBaseCID is the starting CID for vsock allocation
	VsockBaseCID uint32 `yaml:"vsock_base_cid"`

	// ConsolePort is the vsock port for console/exec connections
	ConsolePort uint32 `yaml:"console_port"`

	// HealthPort is the vsock port for health checks
	HealthPort uint32 `yaml:"health_port"`

	// StartTimeout is the timeout for VM startup
	StartTimeout Duration `yaml:"start_timeout"`

	// StopTimeout is the timeout for graceful VM shutdown
	StopTimeout Duration `yaml:"stop_timeout"`

	// BridgeName is the name of the Linux bridge for VM networking
	BridgeName string `yaml:"bridge_name"`

	// BridgeCIDR is the CIDR for the bridge network (e.g., "172.30.0.1/24")
	BridgeCIDR string `yaml:"bridge_cidr"`

	// TAPPrefix is the prefix for TAP device names
	TAPPrefix string `yaml:"tap_prefix"`
}

// Duration is a wrapper around time.Duration for YAML marshaling
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler for Duration
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	duration, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(duration)
	return nil
}

// MarshalYAML implements yaml.Marshaler for Duration
func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

// Duration returns the time.Duration value
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// DefaultFirecrackerConfig returns a FirecrackerConfig with default values.
func DefaultFirecrackerConfig() *FirecrackerConfig {
	return &FirecrackerConfig{
		KernelPath:      "/var/lib/shed/firecracker/vmlinux.bin",
		BaseRootfs:      "/var/lib/shed/firecracker/base-rootfs.ext4",
		InstanceDir:     "/var/lib/shed/firecracker/instances",
		SocketDir:       "/var/run/shed/firecracker",
		DefaultCPUs:     2,
		DefaultMemoryMB: 4096,
		DefaultDiskGB:   20,
		VsockBaseCID:    100,
		ConsolePort:     1024,
		HealthPort:      1025,
		StartTimeout:    Duration(30 * time.Second),
		StopTimeout:     Duration(10 * time.Second),
		BridgeName:      "shed-br0",
		BridgeCIDR:      "172.30.0.1/24",
		TAPPrefix:       "shed-tap",
	}
}

// MountConfig represents a bind mount configuration.
type MountConfig struct {
	Source   string `yaml:"source"`
	Target   string `yaml:"target"`
	ReadOnly bool   `yaml:"readonly"`
}

// DefaultServerConfig returns a ServerConfig with default values.
func DefaultServerConfig() *ServerConfig {
	return &ServerConfig{
		Name:            "shed-server",
		HTTPPort:        8080,
		SSHPort:         2222,
		DefaultImage:    "shed-base:latest",
		Credentials:     make(map[string]MountConfig),
		LogLevel:        "info",
		Terminal:        terminal.DefaultConfig(),
		EnvVars:         make(map[string]string),
		DefaultBackend:  BackendDocker,
		EnabledBackends: []string{BackendDocker},
	}
}

// LoadServerConfig loads server configuration from standard locations.
// It checks in order: ./server.yaml, ~/.config/shed/server.yaml, /etc/shed/server.yaml
func LoadServerConfig() (*ServerConfig, error) {
	return LoadServerConfigFromPath("")
}

// LoadServerConfigFromPath loads server configuration from a specific path.
// If path is empty, it searches standard locations.
func LoadServerConfigFromPath(path string) (*ServerConfig, error) {
	cfg := DefaultServerConfig()

	var configPath string
	if path != "" {
		configPath = ExpandPath(path)
	} else {
		// Search standard locations
		locations := []string{
			"./server.yaml",
			ExpandPath("~/.config/shed/server.yaml"),
			"/etc/shed/server.yaml",
		}

		for _, loc := range locations {
			if _, err := os.Stat(loc); err == nil {
				configPath = loc
				break
			}
		}

		if configPath == "" {
			return cfg, nil // Return defaults if no config found
		}
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", configPath, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", configPath, err)
	}

	// Apply defaults for zero values
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = 8080
	}
	if cfg.SSHPort == 0 {
		cfg.SSHPort = 2222
	}
	if cfg.DefaultImage == "" {
		cfg.DefaultImage = "shed-base:latest"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.Terminal == nil {
		cfg.Terminal = terminal.DefaultConfig()
	}

	cfg.normalizeBackends()

	// Expand and validate paths in credentials
	for name, mount := range cfg.Credentials {
		source := filepath.Clean(ExpandPath(mount.Source))
		target := filepath.Clean(mount.Target)

		// Source must be an absolute path
		if !filepath.IsAbs(source) {
			return nil, fmt.Errorf("credential %q source must be an absolute path: %s", name, mount.Source)
		}

		// Target must be an absolute path
		if !filepath.IsAbs(target) {
			return nil, fmt.Errorf("credential %q target must be an absolute path: %s", name, mount.Target)
		}

		mount.Source = source
		mount.Target = target
		cfg.Credentials[name] = mount
	}

	// Load environment file if specified
	if cfg.EnvFile != "" {
		envPath := ExpandPath(cfg.EnvFile)
		envVars, err := loadEnvFile(envPath)
		if err != nil {
			// Log warning but don't fail if env file is missing
			fmt.Fprintf(os.Stderr, "Warning: failed to load env file %s: %v\n", envPath, err)
		} else {
			cfg.EnvVars = envVars
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate checks that the configuration is valid.
func (c *ServerConfig) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("server name is required")
	}
	if c.HTTPPort < 1 || c.HTTPPort > 65535 {
		return fmt.Errorf("invalid http_port: %d", c.HTTPPort)
	}
	if c.SSHPort < 1 || c.SSHPort > 65535 {
		return fmt.Errorf("invalid ssh_port: %d", c.SSHPort)
	}

	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[c.LogLevel] {
		return fmt.Errorf("invalid log_level: %s (must be debug, info, warn, or error)", c.LogLevel)
	}

	if len(c.EnabledBackends) == 0 {
		return fmt.Errorf("enabled_backends must include at least one backend")
	}

	if c.DefaultBackend == "" {
		return fmt.Errorf("default_backend is required")
	}

	if !isValidBackend(c.DefaultBackend) {
		return fmt.Errorf("invalid default_backend: %s (must be docker or firecracker)", c.DefaultBackend)
	}

	enabled := make(map[string]bool, len(c.EnabledBackends))
	for _, backend := range c.EnabledBackends {
		if !isValidBackend(backend) {
			return fmt.Errorf("invalid enabled_backends entry: %s (must be docker or firecracker)", backend)
		}
		enabled[backend] = true
	}

	if !enabled[c.DefaultBackend] {
		return fmt.Errorf("default_backend %q must be in enabled_backends", c.DefaultBackend)
	}

	if runtime.GOOS != "linux" && enabled[BackendFirecracker] {
		return fmt.Errorf("firecracker backend is only supported on linux")
	}

	// Validate Firecracker config if enabled
	if enabled[BackendFirecracker] {
		if c.Firecracker == nil {
			return fmt.Errorf("firecracker configuration is required when backend is enabled")
		}
		if err := c.Firecracker.Validate(); err != nil {
			return fmt.Errorf("firecracker config: %w", err)
		}
	}

	return nil
}

func isValidBackend(backend string) bool {
	return backend == BackendDocker || backend == BackendFirecracker
}

func (c *ServerConfig) normalizeBackends() {
	if c.DefaultBackend == "" && c.Backend != "" {
		c.DefaultBackend = c.Backend
	}
	if c.DefaultBackend == "" {
		c.DefaultBackend = BackendDocker
	}
	if len(c.EnabledBackends) == 0 {
		if c.Backend != "" {
			c.EnabledBackends = []string{c.Backend}
		} else {
			c.EnabledBackends = []string{c.DefaultBackend}
		}
	}
}

// Validate checks that the Firecracker configuration is valid.
func (c *FirecrackerConfig) Validate() error {
	// Validate paths exist
	if c.KernelPath == "" {
		return fmt.Errorf("kernel_path is required")
	}
	if c.BaseRootfs == "" {
		return fmt.Errorf("base_rootfs is required")
	}
	if c.InstanceDir == "" {
		return fmt.Errorf("instance_dir is required")
	}
	if c.SocketDir == "" {
		return fmt.Errorf("socket_dir is required")
	}

	// Validate CPU and memory minimums
	if c.DefaultCPUs < 1 {
		return fmt.Errorf("default_cpus must be at least 1")
	}
	if c.DefaultMemoryMB < 128 {
		return fmt.Errorf("default_memory_mb must be at least 128")
	}

	// Validate vsock ports
	if c.ConsolePort == 0 {
		return fmt.Errorf("console_port must be set")
	}
	if c.HealthPort == 0 {
		return fmt.Errorf("health_port must be set")
	}
	if c.ConsolePort == c.HealthPort {
		return fmt.Errorf("console_port and health_port must be different")
	}

	if c.VsockBaseCID < 3 {
		return fmt.Errorf("vsock_base_cid must be at least 3 (0-2 are reserved)")
	}

	// Validate network configuration
	if c.BridgeName == "" {
		return fmt.Errorf("bridge_name is required")
	}
	if c.BridgeCIDR == "" {
		return fmt.Errorf("bridge_cidr is required")
	}

	// Validate CIDR format
	_, _, err := net.ParseCIDR(c.BridgeCIDR)
	if err != nil {
		return fmt.Errorf("invalid bridge_cidr %q: %w", c.BridgeCIDR, err)
	}

	if c.TAPPrefix == "" {
		return fmt.Errorf("tap_prefix is required")
	}

	return nil
}

// ExpandPath expands ~ to the user's home directory.
func ExpandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// loadEnvFile loads environment variables from a file.
// Each line should be in the format KEY=value.
func loadEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	envVars := make(map[string]string)
	lines := strings.Split(string(data), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split on first =
		idx := strings.Index(line, "=")
		if idx == -1 {
			continue
		}

		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])

		// Remove quotes if present
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		envVars[key] = value
	}

	return envVars, nil
}
