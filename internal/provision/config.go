// Package provision handles in-repo provisioning for shed containers.
// It loads configuration from .shed/provision.yaml.
package provision

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"gopkg.in/yaml.v3"
)

// DefaultHookTimeout is the default timeout for hook execution.
const DefaultHookTimeout = 30 * time.Minute

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

// HasAnyHooks returns true if any hooks are configured.
func (c *Config) HasAnyHooks() bool {
	return c.HasInstallHook() || c.HasStartupHook()
}

// LoadConfigFromContainer loads provisioning configuration from within a Docker container.
// It reads the config files via docker exec since the workspace is in the container.
func LoadConfigFromContainer(ctx context.Context, docker *client.Client, containerID, workspacePath string) (*Config, error) {
	shedPath := filepath.Join(workspacePath, ShedProvisionYAML)
	if content, err := readFileFromContainer(ctx, docker, containerID, shedPath); err == nil {
		cfg, err := parseShedConfigContent(content)
		if err != nil {
			return nil, fmt.Errorf("failed to parse %s: %w", ShedProvisionYAML, err)
		}
		return cfg, nil
	}

	// No config found - return empty config (no-op)
	return &Config{
		Env: make(map[string]string),
	}, nil
}

// readFileFromContainer reads a file from within a Docker container.
func readFileFromContainer(ctx context.Context, docker *client.Client, containerID, path string) ([]byte, error) {
	execConfig := container.ExecOptions{
		Cmd:          []string{"cat", path},
		User:         config.ContainerUser,
		AttachStdout: true,
		AttachStderr: true,
	}

	execResp, err := docker.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return nil, err
	}

	attachResp, err := docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{})
	if err != nil {
		return nil, err
	}
	defer attachResp.Close()

	// Read output
	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, attachResp.Reader)
	if err != nil {
		return nil, err
	}

	// Check exit code
	inspectResp, err := docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return nil, err
	}

	if inspectResp.ExitCode != 0 {
		// File doesn't exist or other error
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg == "" {
			errMsg = fmt.Sprintf("command failed with exit code %d", inspectResp.ExitCode)
		}
		return nil, fmt.Errorf("%s (exit code %d)", errMsg, inspectResp.ExitCode)
	}

	return stdout.Bytes(), nil
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
