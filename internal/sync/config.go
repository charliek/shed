// Package sync handles client-side file synchronization to shed containers.
// It supports syncing files from the user's local machine to remote containers
// using SSH+tar for transfer and preserving file permissions.
package sync

import (
	"errors"
	"fmt"
	"os"

	"github.com/charliek/shed/internal/config"
	"gopkg.in/yaml.v3"
)

// Sentinel errors for sync operations.
var (
	// ErrProfileNotFound is returned when a profile is not found.
	ErrProfileNotFound = errors.New("profile not found")
	// ErrFeatureNotFound is returned when a feature is not found.
	ErrFeatureNotFound = errors.New("feature not found")
)

// Config holds the sync configuration from ~/.shed/sync.yaml.
type Config struct {
	// Features is a map of feature name to feature configuration
	Features map[string]Feature `yaml:"features"`
	// Profiles is a map of profile name to profile configuration
	Profiles map[string]Profile `yaml:"profiles"`
}

// Feature represents a self-contained sync unit with paths and hooks.
type Feature struct {
	// Description is a human-readable description of the feature
	Description string `yaml:"description,omitempty"`
	// Paths is the list of path mappings for this feature
	Paths []PathMapping `yaml:"paths"`
	// PostSync is a list of commands to run after syncing this feature
	PostSync []PostSyncHook `yaml:"postSync,omitempty"`
}

// PathMapping defines a source-to-target file mapping.
type PathMapping struct {
	// Source is the local path (supports ~ expansion)
	Source string `yaml:"source"`
	// Target is the remote path in the container
	Target string `yaml:"target"`
	// Include is an optional glob pattern to filter files (e.g., "*.pem")
	Include string `yaml:"include,omitempty"`
}

// PostSyncHook defines a command to run after syncing.
type PostSyncHook struct {
	// Run is the command to execute in the container
	Run string `yaml:"run"`
}

// Profile defines which features are enabled together.
type Profile struct {
	// Features is the list of feature names to enable
	Features []string `yaml:"features"`
}

// DefaultConfigPath returns the default sync config path.
func DefaultConfigPath() string {
	return config.GetSyncConfigPath()
}

// LoadConfig loads sync configuration from the default path.
func LoadConfig() (*Config, error) {
	return LoadConfigFromPath(DefaultConfigPath())
}

// LoadConfigFromPath loads sync configuration from a specific path.
func LoadConfigFromPath(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Return empty config if file doesn't exist
			return &Config{
				Features: make(map[string]Feature),
				Profiles: make(map[string]Profile),
			}, nil
		}
		return nil, fmt.Errorf("failed to read sync config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse sync config: %w", err)
	}

	// Initialize maps if nil
	if cfg.Features == nil {
		cfg.Features = make(map[string]Feature)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]Profile)
	}

	// Expand paths
	for name, feature := range cfg.Features {
		for i := range feature.Paths {
			feature.Paths[i].Source = config.ExpandPath(feature.Paths[i].Source)
		}
		cfg.Features[name] = feature
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid sync config: %w", err)
	}

	return &cfg, nil
}

// GetProfile returns a profile by name.
func (c *Config) GetProfile(name string) (*Profile, error) {
	profile, ok := c.Profiles[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrProfileNotFound, name)
	}
	return &profile, nil
}

// GetFeature returns a feature by name.
func (c *Config) GetFeature(name string) (*Feature, error) {
	feature, ok := c.Features[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrFeatureNotFound, name)
	}
	return &feature, nil
}

// GetFeaturesForProfile returns all features enabled by a profile.
func (c *Config) GetFeaturesForProfile(profileName string) (map[string]Feature, error) {
	profile, err := c.GetProfile(profileName)
	if err != nil {
		return nil, err
	}

	features := make(map[string]Feature)
	for _, featureName := range profile.Features {
		feature, err := c.GetFeature(featureName)
		if err != nil {
			return nil, fmt.Errorf("profile %q references unknown feature %q", profileName, featureName)
		}
		features[featureName] = *feature
	}

	return features, nil
}

// HasDefaultProfile returns true if a default profile exists.
func (c *Config) HasDefaultProfile() bool {
	_, ok := c.Profiles["default"]
	return ok
}

// GetDefaultProfile returns "default" if the profile exists, or empty string otherwise.
func (c *Config) GetDefaultProfile() string {
	if _, err := c.GetProfile("default"); err == nil {
		return "default"
	}
	return ""
}

// IsEmpty returns true if no features or profiles are configured.
func (c *Config) IsEmpty() bool {
	return len(c.Features) == 0 && len(c.Profiles) == 0
}

// Validate checks that the configuration is valid.
func (c *Config) Validate() error {
	// Validate features
	for featureName, feature := range c.Features {
		// Warn about empty features (no paths)
		if len(feature.Paths) == 0 {
			return fmt.Errorf("feature %q has no paths defined", featureName)
		}

		// Validate path mappings have required fields
		for i, pm := range feature.Paths {
			if pm.Source == "" {
				return fmt.Errorf("feature %q path %d: source cannot be empty", featureName, i+1)
			}
			if pm.Target == "" {
				return fmt.Errorf("feature %q path %d: target cannot be empty", featureName, i+1)
			}
		}
	}

	// Check that all profile feature references are valid
	for profileName, profile := range c.Profiles {
		for _, featureName := range profile.Features {
			if _, ok := c.Features[featureName]; !ok {
				return fmt.Errorf("profile %q references unknown feature %q", profileName, featureName)
			}
		}
	}

	return nil
}
