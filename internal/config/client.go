package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// ClientConfig represents the CLI-side configuration.
type ClientConfig struct {
	Servers       map[string]ServerEntry `yaml:"servers"`
	DefaultServer string                 `yaml:"default_server"`
	Sheds         map[string]ShedCache   `yaml:"sheds"`

	// Path to config file (not serialized)
	path string `yaml:"-"`
}

// ServerEntry represents a configured server.
type ServerEntry struct {
	Host     string    `yaml:"host"`
	HTTPPort int       `yaml:"http_port"`
	SSHPort  int       `yaml:"ssh_port"`
	AddedAt  time.Time `yaml:"added_at"`
}

// ShedCache caches the location of a shed.
type ShedCache struct {
	Server    string    `yaml:"server"`
	Status    string    `yaml:"status"`
	UpdatedAt time.Time `yaml:"updated_at"`
}

// GetClientConfigDir returns the path to the shed config directory.
func GetClientConfigDir() string {
	return expandPath("~/.shed")
}

// GetClientConfigPath returns the path to the client config file.
func GetClientConfigPath() string {
	return filepath.Join(GetClientConfigDir(), "config.yaml")
}

// GetKnownHostsPath returns the path to the known_hosts file.
func GetKnownHostsPath() string {
	return filepath.Join(GetClientConfigDir(), "known_hosts")
}

// LoadClientConfig loads the client configuration from the default location.
func LoadClientConfig() (*ClientConfig, error) {
	return LoadClientConfigFromPath(GetClientConfigPath())
}

// LoadClientConfigFromPath loads client configuration from a specific path.
func LoadClientConfigFromPath(path string) (*ClientConfig, error) {
	cfg := &ClientConfig{
		Servers: make(map[string]ServerEntry),
		Sheds:   make(map[string]ShedCache),
		path:    path,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Return empty config if file doesn't exist
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Ensure maps are initialized
	if cfg.Servers == nil {
		cfg.Servers = make(map[string]ServerEntry)
	}
	if cfg.Sheds == nil {
		cfg.Sheds = make(map[string]ShedCache)
	}

	cfg.path = path
	return cfg, nil
}

// Save writes the configuration to disk.
func (c *ClientConfig) Save() error {
	return c.SaveToPath(c.path)
}

// SaveToPath writes the configuration to a specific path.
func (c *ClientConfig) SaveToPath(path string) error {
	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write atomically via temp file
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Clean up on failure
		return fmt.Errorf("failed to save config file: %w", err)
	}

	c.path = path
	return nil
}

// AddServer adds a new server to the configuration.
func (c *ClientConfig) AddServer(name string, entry ServerEntry) error {
	if _, exists := c.Servers[name]; exists {
		return fmt.Errorf("server '%s' already exists", name)
	}

	entry.AddedAt = time.Now()
	c.Servers[name] = entry

	// Set as default if it's the first server
	if c.DefaultServer == "" {
		c.DefaultServer = name
	}

	return nil
}

// RemoveServer removes a server from the configuration.
func (c *ClientConfig) RemoveServer(name string) error {
	if _, exists := c.Servers[name]; !exists {
		return fmt.Errorf("server '%s' not found", name)
	}

	delete(c.Servers, name)

	// Clear default if we removed it
	if c.DefaultServer == name {
		c.DefaultServer = ""
		// Set new default to first remaining server
		for serverName := range c.Servers {
			c.DefaultServer = serverName
			break
		}
	}

	// Remove cached sheds for this server
	for shedName, cache := range c.Sheds {
		if cache.Server == name {
			delete(c.Sheds, shedName)
		}
	}

	return nil
}

// GetServer returns a server by name.
func (c *ClientConfig) GetServer(name string) (*ServerEntry, error) {
	entry, exists := c.Servers[name]
	if !exists {
		return nil, fmt.Errorf("server '%s' not found", name)
	}
	return &entry, nil
}

// GetDefaultServer returns the default server entry.
func (c *ClientConfig) GetDefaultServer() (*ServerEntry, string, error) {
	if c.DefaultServer == "" {
		return nil, "", fmt.Errorf("no default server configured")
	}
	entry, err := c.GetServer(c.DefaultServer)
	if err != nil {
		return nil, "", err
	}
	return entry, c.DefaultServer, nil
}

// SetDefaultServer sets the default server.
func (c *ClientConfig) SetDefaultServer(name string) error {
	if _, exists := c.Servers[name]; !exists {
		return fmt.Errorf("server '%s' not found", name)
	}
	c.DefaultServer = name
	return nil
}

// CacheShed caches a shed's location.
func (c *ClientConfig) CacheShed(name string, server string, status string) {
	c.Sheds[name] = ShedCache{
		Server:    server,
		Status:    status,
		UpdatedAt: time.Now(),
	}
}

// GetShedServer returns the server that hosts a shed.
func (c *ClientConfig) GetShedServer(name string) (string, error) {
	cache, exists := c.Sheds[name]
	if !exists {
		return "", fmt.Errorf("shed '%s' not found in cache", name)
	}
	return cache.Server, nil
}

// RemoveShedCache removes a shed from the cache.
func (c *ClientConfig) RemoveShedCache(name string) {
	delete(c.Sheds, name)
}

// AddKnownHost adds an SSH host key to the known_hosts file.
func AddKnownHost(host string, port int, hostKey string) error {
	knownHostsPath := GetKnownHostsPath()

	// Ensure directory exists
	dir := filepath.Dir(knownHostsPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Format the entry
	var entry string
	if port == 22 {
		entry = fmt.Sprintf("%s %s\n", host, hostKey)
	} else {
		entry = fmt.Sprintf("[%s]:%d %s\n", host, port, hostKey)
	}

	// Append to file
	f, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open known_hosts: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("failed to write to known_hosts: %w", err)
	}

	return nil
}

// EnsureConfigDir ensures the config directory exists.
func EnsureConfigDir() error {
	dir := GetClientConfigDir()
	return os.MkdirAll(dir, 0700)
}
