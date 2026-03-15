package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/charliek/shed/internal/terminal"
)

// validCredentialName matches names starting with an alphanumeric character,
// followed by alphanumerics, underscores, or hyphens. Credential names are
// used as VirtioFS mount tags (cred-{name}), so they must not contain spaces,
// commas, or other special characters.
var validCredentialName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

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

	// VZ contains Apple Virtualization.framework-specific configuration (macOS only)
	VZ *VZConfig `yaml:"vz,omitempty"`

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

	// NotifyPort is the vsock port for credential change notifications
	NotifyPort uint32 `yaml:"notify_port"`

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

// VZConfig contains Apple Virtualization.framework-specific configuration.
type VZConfig struct {
	// VfkitPath is the path to the vfkit binary
	VfkitPath string `yaml:"vfkit_path"`

	// KernelPath is the path to the decompressed Linux kernel
	KernelPath string `yaml:"kernel_path"`

	// InitrdPath is the path to the initial RAM disk image
	InitrdPath string `yaml:"initrd_path"`

	// BaseRootfs is the path to the base rootfs image (used when no --image is specified)
	BaseRootfs string `yaml:"base_rootfs"`

	// Images maps variant names to rootfs paths for per-shed image selection.
	// Users can reference these with: shed create mydev --image typescript
	Images map[string]string `yaml:"images,omitempty"`

	// InstanceDir is the directory for instance data
	InstanceDir string `yaml:"instance_dir"`

	// SocketDir is the directory for vsock Unix sockets.
	// NOTE: This path must not contain spaces. vfkit URL-encodes socket paths,
	// turning spaces into %20, which causes connection failures.
	SocketDir string `yaml:"socket_dir"`

	// DefaultCPUs is the default number of vCPUs for new VMs
	DefaultCPUs int `yaml:"default_cpus"`

	// DefaultMemoryMB is the default memory in MB for new VMs
	DefaultMemoryMB int `yaml:"default_memory_mb"`

	// DefaultDiskGB is the default disk size in GB for new VMs
	DefaultDiskGB int `yaml:"default_disk_gb"`

	// ConsolePort is the vsock port for console/exec connections
	ConsolePort uint32 `yaml:"console_port"`

	// HealthPort is the vsock port for health checks
	HealthPort uint32 `yaml:"health_port"`

	// NotifyPort is the vsock port for credential change notifications
	NotifyPort uint32 `yaml:"notify_port"`

	// StartTimeout is the timeout for VM startup
	StartTimeout Duration `yaml:"start_timeout"`

	// StopTimeout is the timeout for graceful VM shutdown
	StopTimeout Duration `yaml:"stop_timeout"`
}

// DefaultVZConfig returns a VZConfig with default values.
func DefaultVZConfig() *VZConfig {
	return &VZConfig{
		VfkitPath:       "vfkit",
		KernelPath:      ExpandPath("~/Library/Application Support/shed/vz/vmlinux"),
		InitrdPath:      ExpandPath("~/Library/Application Support/shed/vz/initrd.img"),
		BaseRootfs:      ExpandPath("~/Library/Application Support/shed/vz/default-rootfs.ext4"),
		InstanceDir:     ExpandPath("~/Library/Application Support/shed/vz/instances"),
		SocketDir:       ExpandPath("~/.shed/vz/sockets"),
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

// applyDefaults fills in zero-valued fields with defaults and expands ~ in paths.
func (c *VZConfig) applyDefaults() {
	if c.NotifyPort == 0 {
		c.NotifyPort = 1026
	}

	// Expand ~ in paths
	c.KernelPath = ExpandPath(c.KernelPath)
	c.InitrdPath = ExpandPath(c.InitrdPath)
	c.BaseRootfs = ExpandPath(c.BaseRootfs)
	c.InstanceDir = ExpandPath(c.InstanceDir)
	c.SocketDir = ExpandPath(c.SocketDir)

	// Expand ~ in image paths
	for name, path := range c.Images {
		c.Images[name] = ExpandPath(path)
	}
}

// Validate checks that the VZ configuration is valid.
func (c *VZConfig) Validate() error {
	if c.VfkitPath == "" {
		return fmt.Errorf("vfkit_path is required")
	}
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

	// Validate CPU and memory bounds
	if c.DefaultCPUs < 1 {
		return fmt.Errorf("vz: default_cpus must be at least 1")
	}
	if c.DefaultCPUs > MaxVZCPUs {
		return fmt.Errorf("vz: default_cpus must be at most %d", MaxVZCPUs)
	}
	if c.DefaultMemoryMB < 128 {
		return fmt.Errorf("vz: default_memory_mb must be at least 128")
	}
	if c.DefaultMemoryMB > MaxVZMemoryMB {
		return fmt.Errorf("vz: default_memory_mb must be at most %d", MaxVZMemoryMB)
	}
	if c.DefaultDiskGB < 1 {
		return fmt.Errorf("vz: default_disk_gb must be at least 1")
	}
	if c.DefaultDiskGB > MaxVZDiskGB {
		return fmt.Errorf("vz: default_disk_gb must be at most %d", MaxVZDiskGB)
	}

	// Validate vsock ports
	if c.ConsolePort == 0 {
		return fmt.Errorf("vz: console_port must be set")
	}
	if c.ConsolePort > MaxVsockPort {
		return fmt.Errorf("vz: console_port must be at most %d", MaxVsockPort)
	}
	if c.HealthPort == 0 {
		return fmt.Errorf("vz: health_port must be set")
	}
	if c.HealthPort > MaxVsockPort {
		return fmt.Errorf("vz: health_port must be at most %d", MaxVsockPort)
	}
	if c.NotifyPort == 0 {
		return fmt.Errorf("vz: notify_port must be set")
	}
	if c.NotifyPort > MaxVsockPort {
		return fmt.Errorf("vz: notify_port must be at most %d", MaxVsockPort)
	}
	if c.ConsolePort == c.HealthPort || c.ConsolePort == c.NotifyPort || c.HealthPort == c.NotifyPort {
		return fmt.Errorf("vz: console_port, health_port, and notify_port must all be different")
	}

	// Validate timeouts
	startTimeout := time.Duration(c.StartTimeout)
	if startTimeout != 0 {
		if startTimeout < MinTimeout {
			return fmt.Errorf("vz: start_timeout must be at least %s", MinTimeout)
		}
		if startTimeout > MaxTimeout {
			return fmt.Errorf("vz: start_timeout must be at most %s", MaxTimeout)
		}
	}
	stopTimeout := time.Duration(c.StopTimeout)
	if stopTimeout != 0 {
		if stopTimeout < MinTimeout {
			return fmt.Errorf("vz: stop_timeout must be at least %s", MinTimeout)
		}
		if stopTimeout > MaxTimeout {
			return fmt.Errorf("vz: stop_timeout must be at most %s", MaxTimeout)
		}
	}

	// Validate kernel, initrd, and rootfs paths exist
	if _, err := os.Stat(c.KernelPath); err != nil {
		return fmt.Errorf("vz: kernel_path does not exist: %s", c.KernelPath)
	}
	if c.InitrdPath != "" {
		if _, err := os.Stat(c.InitrdPath); err != nil {
			return fmt.Errorf("vz: initrd_path does not exist: %s", c.InitrdPath)
		}
	}
	if _, err := os.Stat(c.BaseRootfs); err != nil {
		return fmt.Errorf("vz: base_rootfs does not exist: %s", c.BaseRootfs)
	}

	// Validate image variant paths exist
	for name, path := range c.Images {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("vz: image %q path does not exist: %s", name, path)
		}
	}

	return nil
}

// ResolveImage resolves an image name to a rootfs path using the Images map.
// It checks named variants first, then absolute paths as an escape hatch.
func (c *VZConfig) ResolveImage(image string) (string, error) {
	if path, ok := c.Images[image]; ok {
		return path, nil
	}
	expanded := ExpandPath(image)
	if filepath.IsAbs(expanded) {
		if _, err := os.Stat(expanded); err != nil {
			return "", fmt.Errorf("image path does not exist: %s", expanded)
		}
		return expanded, nil
	}
	available := make([]string, 0, len(c.Images))
	for name := range c.Images {
		available = append(available, name)
	}
	sort.Strings(available)
	if len(available) > 0 {
		return "", fmt.Errorf("unknown image %q; available variants: %s", image, strings.Join(available, ", "))
	}
	return "", fmt.Errorf("unknown image %q; no image variants configured (set vz.images in server config)", image)
}

// Firecracker validation upper bounds.
const (
	MaxFirecrackerCPUs            = 32
	MaxFirecrackerMemoryMB        = 256 * 1024 // 256 GB
	MaxFirecrackerDiskGB          = 1024       // 1 TB
	MaxVsockCID            uint32 = 65535
	MaxVsockPort           uint32 = 65535
	MinTimeout                    = 1 * time.Second
	MaxTimeout                    = 30 * time.Minute
)

// VZ validation upper bounds (decoupled from Firecracker).
const (
	MaxVZCPUs     = 32
	MaxVZMemoryMB = 256 * 1024 // 256 GB
	MaxVZDiskGB   = 1024       // 1 TB
)

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
		NotifyPort:      1026,
		StartTimeout:    Duration(30 * time.Second),
		StopTimeout:     Duration(10 * time.Second),
		BridgeName:      "shed-br0",
		BridgeCIDR:      "172.30.0.1/24",
		TAPPrefix:       "shed-tap",
	}
}

// MountConfig represents a bind mount configuration.
type MountConfig struct {
	Source   string   `yaml:"source"`
	Target   string   `yaml:"target"`
	ReadOnly bool     `yaml:"readonly"`
	Exclude  []string `yaml:"exclude,omitempty"`
}

// MatchesExclude reports whether the given relative path matches any of the
// mount's exclude patterns. Patterns use filepath.Match glob syntax.
func (m MountConfig) MatchesExclude(relPath string) bool {
	return MatchesExcludePatterns(relPath, m.Exclude)
}

// MatchesExcludePatterns reports whether relPath matches any of the given glob
// patterns. Patterns like "dir/*" also match the directory itself and deeply
// nested paths (e.g., "dir/sub/deep/file").
func MatchesExcludePatterns(relPath string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, relPath); matched {
			return true
		}
		if dir, ok := strings.CutSuffix(pattern, "/*"); ok {
			if relPath == dir || strings.HasPrefix(relPath, dir+"/") {
				return true
			}
		}
	}
	return false
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
		// Credential names must be safe for use as VirtioFS mount tags
		if !validCredentialName.MatchString(name) {
			return nil, fmt.Errorf("credential name %q must contain only alphanumeric characters, underscores, and hyphens", name)
		}

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

		// Source paths with commas break vfkit VirtioFS device arguments
		if strings.Contains(source, ",") {
			return nil, fmt.Errorf("credential %q source path must not contain commas: %s", name, source)
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

	if cfg.Firecracker != nil {
		cfg.Firecracker.applyDefaults()
	}

	if cfg.VZ != nil {
		cfg.VZ.applyDefaults()
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
		return fmt.Errorf("invalid default_backend: %s (must be docker, firecracker, or vz)", c.DefaultBackend)
	}

	enabled := make(map[string]bool, len(c.EnabledBackends))
	for _, backend := range c.EnabledBackends {
		if !isValidBackend(backend) {
			return fmt.Errorf("invalid enabled_backends entry: %s (must be docker, firecracker, or vz)", backend)
		}
		enabled[backend] = true
	}

	if !enabled[c.DefaultBackend] {
		return fmt.Errorf("default_backend %q must be in enabled_backends", c.DefaultBackend)
	}

	if runtime.GOOS != "linux" && enabled[BackendFirecracker] {
		return fmt.Errorf("firecracker backend is only supported on linux")
	}

	if runtime.GOOS != "darwin" && enabled[BackendVZ] {
		return fmt.Errorf("vz backend is only supported on macOS")
	}
	if runtime.GOARCH != "arm64" && enabled[BackendVZ] {
		return fmt.Errorf("vz backend currently supports macOS Apple Silicon (arm64) only")
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

	// Validate VZ config if enabled
	if enabled[BackendVZ] {
		if c.VZ == nil {
			return fmt.Errorf("vz configuration is required when backend is enabled")
		}
		if err := c.VZ.Validate(); err != nil {
			return fmt.Errorf("vz config: %w", err)
		}
	}

	return nil
}

func isValidBackend(backend string) bool {
	return backend == BackendDocker || backend == BackendFirecracker || backend == BackendVZ
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

// applyDefaults fills in zero-valued fields with defaults so existing configs
// without newer fields (e.g. notify_port) continue to work.
func (c *FirecrackerConfig) applyDefaults() {
	if c.NotifyPort == 0 {
		c.NotifyPort = 1026
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

	// Validate CPU and memory bounds
	if c.DefaultCPUs < 1 {
		return fmt.Errorf("default_cpus must be at least 1")
	}
	if c.DefaultCPUs > MaxFirecrackerCPUs {
		return fmt.Errorf("default_cpus must be at most %d", MaxFirecrackerCPUs)
	}
	if c.DefaultMemoryMB < 128 {
		return fmt.Errorf("default_memory_mb must be at least 128")
	}
	if c.DefaultMemoryMB > MaxFirecrackerMemoryMB {
		return fmt.Errorf("default_memory_mb must be at most %d", MaxFirecrackerMemoryMB)
	}
	if c.DefaultDiskGB < 1 {
		return fmt.Errorf("default_disk_gb must be at least 1")
	}
	if c.DefaultDiskGB > MaxFirecrackerDiskGB {
		return fmt.Errorf("default_disk_gb must be at most %d", MaxFirecrackerDiskGB)
	}

	// Validate vsock ports
	if c.ConsolePort == 0 {
		return fmt.Errorf("console_port must be set")
	}
	if c.ConsolePort > MaxVsockPort {
		return fmt.Errorf("console_port must be at most %d", MaxVsockPort)
	}
	if c.HealthPort == 0 {
		return fmt.Errorf("health_port must be set")
	}
	if c.HealthPort > MaxVsockPort {
		return fmt.Errorf("health_port must be at most %d", MaxVsockPort)
	}
	if c.NotifyPort == 0 {
		return fmt.Errorf("notify_port must be set")
	}
	if c.NotifyPort > MaxVsockPort {
		return fmt.Errorf("notify_port must be at most %d", MaxVsockPort)
	}
	if c.ConsolePort == c.HealthPort || c.ConsolePort == c.NotifyPort || c.HealthPort == c.NotifyPort {
		return fmt.Errorf("console_port, health_port, and notify_port must all be different")
	}

	if c.VsockBaseCID < 3 {
		return fmt.Errorf("vsock_base_cid must be at least 3 (0-2 are reserved)")
	}
	if c.VsockBaseCID > MaxVsockCID {
		return fmt.Errorf("vsock_base_cid must be at most %d", MaxVsockCID)
	}

	// Validate timeouts
	startTimeout := time.Duration(c.StartTimeout)
	if startTimeout != 0 {
		if startTimeout < MinTimeout {
			return fmt.Errorf("start_timeout must be at least %s", MinTimeout)
		}
		if startTimeout > MaxTimeout {
			return fmt.Errorf("start_timeout must be at most %s", MaxTimeout)
		}
	}
	stopTimeout := time.Duration(c.StopTimeout)
	if stopTimeout != 0 {
		if stopTimeout < MinTimeout {
			return fmt.Errorf("stop_timeout must be at least %s", MinTimeout)
		}
		if stopTimeout > MaxTimeout {
			return fmt.Errorf("stop_timeout must be at most %s", MaxTimeout)
		}
	}

	// Validate kernel and rootfs paths exist
	if _, err := os.Stat(c.KernelPath); err != nil {
		return fmt.Errorf("kernel_path does not exist: %s", c.KernelPath)
	}
	if _, err := os.Stat(c.BaseRootfs); err != nil {
		return fmt.Errorf("base_rootfs does not exist: %s", c.BaseRootfs)
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
