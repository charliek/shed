// Package tunnels provides SSH port forwarding tunnel management.
package tunnels

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/charliek/shed/internal/config"
)

// Sentinel errors for tunnel configuration.
var (
	// ErrNoTunnelConfig is returned when no tunnel configuration exists for a shed.
	ErrNoTunnelConfig = errors.New("no tunnel config for shed")
	// ErrProfileNotFound is returned when a profile is not found for a shed.
	ErrProfileNotFound = errors.New("profile not found")
)

// TunnelConfig represents the tunnel configuration file.
type TunnelConfig struct {
	SSH   SSHConfig             `yaml:"ssh"`
	Sheds map[string]ShedConfig `yaml:"sheds"`
}

// SSHConfig contains SSH connection settings.
type SSHConfig struct {
	ServerAliveInterval int `yaml:"server_alive_interval"`
	ServerAliveCountMax int `yaml:"server_alive_count_max"`
	ConnectTimeout      int `yaml:"connect_timeout"`
}

// ShedConfig contains tunnel profiles for a shed.
type ShedConfig struct {
	Profiles map[string][]string `yaml:"profiles"`
}

// Validate checks that SSH configuration values are within acceptable ranges.
func (c *SSHConfig) Validate() error {
	if c.ServerAliveInterval < 0 || c.ServerAliveInterval > 3600 {
		return fmt.Errorf("server_alive_interval must be 0-3600, got %d", c.ServerAliveInterval)
	}
	if c.ServerAliveCountMax < 0 || c.ServerAliveCountMax > 100 {
		return fmt.Errorf("server_alive_count_max must be 0-100, got %d", c.ServerAliveCountMax)
	}
	if c.ConnectTimeout < 0 || c.ConnectTimeout > 300 {
		return fmt.Errorf("connect_timeout must be 0-300, got %d", c.ConnectTimeout)
	}
	return nil
}

// PortMapping represents a local:remote port mapping.
type PortMapping struct {
	Local  int `json:"local"`
	Remote int `json:"remote"`
}

// LoadTunnelConfig loads the tunnel configuration from disk.
func LoadTunnelConfig() (*TunnelConfig, error) {
	return LoadTunnelConfigFromPath(config.GetTunnelsConfigPath())
}

// LoadTunnelConfigFromPath loads tunnel configuration from a specific path.
func LoadTunnelConfigFromPath(path string) (*TunnelConfig, error) {
	cfg := &TunnelConfig{
		SSH: SSHConfig{
			ServerAliveInterval: 30,
			ServerAliveCountMax: 3,
			ConnectTimeout:      10,
		},
		Sheds: make(map[string]ShedConfig),
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read tunnel config: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse tunnel config: %w", err)
	}

	// Ensure maps are initialized
	if cfg.Sheds == nil {
		cfg.Sheds = make(map[string]ShedConfig)
	}

	// Validate SSH config
	if err := cfg.SSH.Validate(); err != nil {
		return nil, fmt.Errorf("invalid tunnel config: %w", err)
	}

	return cfg, nil
}

// GetProfiles returns the profile names available for a shed.
func (c *TunnelConfig) GetProfiles(shedName string) []string {
	shedCfg, ok := c.Sheds[shedName]
	if !ok {
		return nil
	}

	profiles := make([]string, 0, len(shedCfg.Profiles))
	for name := range shedCfg.Profiles {
		profiles = append(profiles, name)
	}
	return profiles
}

// GetPortMappings returns the port mappings for a shed and profile.
func (c *TunnelConfig) GetPortMappings(shedName, profileName string) ([]PortMapping, error) {
	shedCfg, ok := c.Sheds[shedName]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoTunnelConfig, shedName)
	}

	portStrs, ok := shedCfg.Profiles[profileName]
	if !ok {
		available := c.GetProfiles(shedName)
		return nil, fmt.Errorf("%w: %q for shed %q. Available: %s",
			ErrProfileNotFound, profileName, shedName, strings.Join(available, ", "))
	}

	return ParsePortMappings(portStrs)
}

// ParsePortMappings parses a list of port mapping strings.
// Supports formats: "3000" (same port both sides) or "3001:3000" (local:remote).
func ParsePortMappings(portStrs []string) ([]PortMapping, error) {
	mappings := make([]PortMapping, 0, len(portStrs))

	for _, s := range portStrs {
		mapping, err := ParsePortMapping(s)
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, mapping)
	}

	return mappings, nil
}

// ParsePortMapping parses a single port mapping string.
func ParsePortMapping(s string) (PortMapping, error) {
	s = strings.TrimSpace(s)

	if strings.Contains(s, ":") {
		parts := strings.SplitN(s, ":", 2)
		local, err := strconv.Atoi(parts[0])
		if err != nil {
			return PortMapping{}, fmt.Errorf("invalid local port %q: %w", parts[0], err)
		}
		remote, err := strconv.Atoi(parts[1])
		if err != nil {
			return PortMapping{}, fmt.Errorf("invalid remote port %q: %w", parts[1], err)
		}
		if local < 1 || local > 65535 {
			return PortMapping{}, fmt.Errorf("local port %d out of range (1-65535)", local)
		}
		if remote < 1 || remote > 65535 {
			return PortMapping{}, fmt.Errorf("remote port %d out of range (1-65535)", remote)
		}
		return PortMapping{Local: local, Remote: remote}, nil
	}

	port, err := strconv.Atoi(s)
	if err != nil {
		return PortMapping{}, fmt.Errorf("invalid port %q: %w", s, err)
	}
	if port < 1 || port > 65535 {
		return PortMapping{}, fmt.Errorf("port %d out of range (1-65535)", port)
	}
	return PortMapping{Local: port, Remote: port}, nil
}

// MergePortMappings merges multiple port mapping slices, with later entries overriding earlier ones.
func MergePortMappings(mappingSlices ...[]PortMapping) []PortMapping {
	seen := make(map[int]PortMapping)
	order := make([]int, 0)

	for _, mappings := range mappingSlices {
		for _, m := range mappings {
			if _, exists := seen[m.Local]; !exists {
				order = append(order, m.Local)
			}
			seen[m.Local] = m
		}
	}

	result := make([]PortMapping, 0, len(seen))
	for _, local := range order {
		result = append(result, seen[local])
	}
	return result
}
