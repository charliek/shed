// Package provision handles in-repo provisioning for shed containers.
// It loads configuration from .shed/provision.yaml.
package provision

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultHookTimeout is the default timeout for hook execution.
const DefaultHookTimeout = 30 * time.Minute

// Default environment variables set by shed in containers/VMs.
const (
	EnvShedContainer = "SHED_CONTAINER"
	EnvShedName      = "SHED_NAME"
	EnvShedWorkspace = "SHED_WORKSPACE"
)

// Log file paths in the container/VM.
const (
	LogDir      = "/var/log/shed"
	InstallLog  = "/var/log/shed/install.log"
	StartupLog  = "/var/log/shed/startup.log"
	ShutdownLog = "/var/log/shed/shutdown.log"
	SyncLog     = "/var/log/shed/sync.log"
)

// HookType identifies which type of hook is being run.
type HookType string

const (
	HookTypeInstall  HookType = "install"
	HookTypeStartup  HookType = "startup"
	HookTypeShutdown HookType = "shutdown"
)

// HookError represents an error from a failed hook execution.
type HookError struct {
	HookType   HookType
	ExitCode   int
	LogFile    string
	LastOutput string
	Err        error // Optional underlying error
}

func (e *HookError) Error() string {
	return fmt.Sprintf("%s hook failed (exit code %d)", e.HookType, e.ExitCode)
}

// Unwrap returns the underlying error for errors.Is/As compatibility.
func (e *HookError) Unwrap() error {
	return e.Err
}

// Config holds provisioning configuration loaded from a repository.
type Config struct {
	// Hooks contains paths to provisioning scripts
	Hooks HooksConfig `yaml:"hooks,omitempty" json:"hooks,omitempty"`
	// Env contains additional environment variables to pass to hooks
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	// Timeout is the maximum time allowed for hook execution
	Timeout time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// GetTimeout returns the configured timeout or the default.
func (c *Config) GetTimeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultHookTimeout
}

// HooksConfig contains paths to provisioning hook scripts.
type HooksConfig struct {
	// Install is the path to the install script (runs once on create)
	Install string `yaml:"install,omitempty"`
	// Startup is the path to the startup script (runs on every start)
	Startup string `yaml:"startup,omitempty"`
	// Shutdown is the path to the shutdown script (runs before stop)
	Shutdown string `yaml:"shutdown,omitempty"`
}

// ShedProvisionYAML is the name of the shed provision config file.
const ShedProvisionYAML = ".shed/provision.yaml"

// LoadConfig loads provisioning configuration from a workspace directory.
// It looks for .shed/provision.yaml and returns an empty config (no-op) if not found.
func LoadConfig(workspacePath string) (*Config, error) {
	shedPath := filepath.Join(workspacePath, ShedProvisionYAML)
	if cfg, err := loadShedConfig(shedPath); err == nil {
		return cfg, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to load %s: %w", ShedProvisionYAML, err)
	}

	// No config found - return empty config (no-op)
	return &Config{
		Env: make(map[string]string),
	}, nil
}

// loadShedConfig loads a .shed/provision.yaml file.
func loadShedConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	if cfg.Env == nil {
		cfg.Env = make(map[string]string)
	}

	return &cfg, nil
}

// HasInstallHook returns true if an install hook is configured.
func (c *Config) HasInstallHook() bool {
	return c != nil && c.Hooks.Install != ""
}

// HasStartupHook returns true if a startup hook is configured.
func (c *Config) HasStartupHook() bool {
	return c != nil && c.Hooks.Startup != ""
}

// HasShutdownHook returns true if a shutdown hook is configured.
func (c *Config) HasShutdownHook() bool {
	return c != nil && c.Hooks.Shutdown != ""
}

// HasAnyHooks returns true if any hooks are configured.
func (c *Config) HasAnyHooks() bool {
	return c.HasInstallHook() || c.HasStartupHook() || c.HasShutdownHook()
}

// parseShedConfigContent parses .shed/provision.yaml content.
func parseShedConfigContent(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Env == nil {
		cfg.Env = make(map[string]string)
	}
	return &cfg, nil
}

// ParseConfigContent parses .shed/provision.yaml content from raw bytes.
// This is exported for use by backends that read config via their own mechanism.
func ParseConfigContent(data []byte) (*Config, error) {
	return parseShedConfigContent(data)
}
