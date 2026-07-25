package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ClientConfig represents the CLI-side configuration.
type ClientConfig struct {
	Servers       map[string]ServerEntry `yaml:"servers"`
	DefaultServer string                 `yaml:"default_server"`
	Sheds         map[string]ShedCache   `yaml:"sheds"`
	CreateTimeout time.Duration          `yaml:"create_timeout,omitempty"`

	// Path to config file (not serialized)
	path string `yaml:"-"`
}

// GetCreateTimeout returns the configured create timeout or the default
// (10 minutes). One per-manifest mkfs.erofs over a merged tree is
// single-digit seconds even on cold cache; only the first-pull from
// the registry adds notable wallclock, and 10 min is enough headroom
// even for a slow network on a multi-GB image. Cache hits on
// subsequent creates are sub-second.
func (c *ClientConfig) GetCreateTimeout() time.Duration {
	if c.CreateTimeout > 0 {
		return c.CreateTimeout
	}
	return 10 * time.Minute
}

// ServerEntry represents a configured server.
type ServerEntry struct {
	Host     string    `yaml:"host"`
	HTTPPort int       `yaml:"http_port,omitempty"`
	SSHPort  int       `yaml:"ssh_port"`
	AddedAt  time.Time `yaml:"added_at"`
	// ControlToken is the bearer token sent on control-plane HTTP requests
	// (CLI, desktop). CredentialsToken is sent by the host-agent for the
	// credential bus. Both optional; empty when the server isn't token-gated.
	ControlToken     string `yaml:"control_token,omitempty"`
	CredentialsToken string `yaml:"credentials_token,omitempty"`
	// ControlTokenExpiresAt is when ControlToken (bootstrap-minted, short-TTL)
	// expires, so the CLI can transparently re-mint before/after expiry. Zero
	// for a legacy static token or an open server.
	ControlTokenExpiresAt time.Time `yaml:"control_token_expires_at,omitempty"`
	// APIURL, when set, overrides the scheme+host+port for the control plane
	// (e.g. https://host:8443). Empty = plain http://Host:HTTPPort (legacy).
	APIURL string `yaml:"api_url,omitempty"`
	// TLSCertFingerprint pins the server's self-signed TLS cert as
	// "sha256:<hex>", captured at `shed server add`. When set, the client
	// verifies the presented cert against it (no CA needed).
	TLSCertFingerprint string `yaml:"tls_cert_fingerprint,omitempty"`
	// AuthMode records the credential shape the server issued at the last
	// bootstrap: AuthModeToken (a bearer token) or AuthModeMTLS (a client
	// certificate).
	//
	// ABSENT MEANS TOKEN. Every entry written before client-certificate support
	// existed has no auth_mode key, and those are all token/open servers — so an
	// empty value must never be read as "unknown, go find out". It is a CACHE of
	// what the server last said, not a setting: a bootstrap that comes back in
	// the other mode rewrites it (see the mode-flip path in cmd/shed/client.go),
	// which is what lets an operator switch a server's auth.mode without every
	// client needing to be re-added.
	AuthMode string `yaml:"auth_mode,omitempty"`
	// ClientCertFile / ClientKeyFile locate this entry's client certificate and
	// its private key under the creds dir (see ServerCredsDir). Paths rather
	// than inline PEM: the key must live in a 0600 file of its own, not inside a
	// config the user hand-edits, copies between machines, and pastes into
	// issues.
	ClientCertFile string `yaml:"client_cert_file,omitempty"`
	ClientKeyFile  string `yaml:"client_key_file,omitempty"`
	// ClientCertExpiresAt is when ClientCertFile's certificate expires, so the
	// client can re-enroll before a request races expiry — the mtls counterpart
	// of ControlTokenExpiresAt. Cached here so the proactive check costs no file
	// read; the certificate itself remains the authority.
	ClientCertExpiresAt time.Time `yaml:"client_cert_expires_at,omitempty"`
}

// IsMTLS reports whether this entry last bootstrapped a client certificate.
func (e *ServerEntry) IsMTLS() bool { return e.AuthMode == AuthModeMTLS }

// BaseURL returns the control-plane base URL for the entry: APIURL when set
// (it carries scheme+host+port), else the legacy plain http://Host:HTTPPort.
//
// The host:port is joined with net.JoinHostPort, not printf: Host may be an
// IPv6 literal ("::1"), and "http://::1:8080" is not a URL any client can parse.
// The bracketed form is what every http.Client, url.Parse, and SSH-first
// open-mode add downstream of this needs.
func (e *ServerEntry) BaseURL() string {
	if e.APIURL != "" {
		return strings.TrimRight(e.APIURL, "/")
	}
	return "http://" + net.JoinHostPort(e.Host, strconv.Itoa(e.HTTPPort))
}

// ShedCache caches the location of a shed.
type ShedCache struct {
	Server    string    `yaml:"server"`
	Status    string    `yaml:"status"`
	UpdatedAt time.Time `yaml:"updated_at"`
}

// GetClientConfigDir returns the path to the shed config directory.
func GetClientConfigDir() string {
	return ExpandPath("~/.shed")
}

// GetClientConfigPath returns the path to the client config file.
func GetClientConfigPath() string {
	return filepath.Join(GetClientConfigDir(), "config.yaml")
}

// GetKnownHostsPath returns the path to the known_hosts file.
func GetKnownHostsPath() string {
	return filepath.Join(GetClientConfigDir(), "known_hosts")
}

// GetTunnelsConfigPath returns the path to the tunnels config file.
func GetTunnelsConfigPath() string {
	return filepath.Join(GetClientConfigDir(), "tunnels.yaml")
}

// GetTunnelStatePath returns the path to the tunnel state file.
func GetTunnelStatePath() string {
	return filepath.Join(GetClientConfigDir(), "tunnel-state.json")
}

// GetTunnelLogDir returns the directory holding background tunnel daemon logs.
func GetTunnelLogDir() string {
	return filepath.Join(GetClientConfigDir(), "logs")
}

// GetTunnelLogPath returns the log path for a shed's background tunnel daemon.
// shedName is escaped so a name containing path separators can't traverse out
// of the log directory (the daemon opens this path with O_TRUNC).
func GetTunnelLogPath(shedName string) string {
	return filepath.Join(GetTunnelLogDir(), "tunnel-"+url.PathEscape(shedName)+".log")
}

// GetSyncConfigPath returns the path to the sync config file.
func GetSyncConfigPath() string {
	return filepath.Join(GetClientConfigDir(), "sync.yaml")
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

// KnownHostLine renders the exact ~/.shed/known_hosts line for an endpoint, in
// OpenSSH's syntax: a bare hostname on port 22, the bracketed "[host]:port"
// form otherwise.
//
// It is exported because it is also the IDENTITY of a line: RemoveKnownHost
// matches on it, and `shed server add` keeps it around so a failed add can undo
// exactly the line it wrote and nothing else.
func KnownHostLine(host string, port int, hostKey string) string {
	if port == 22 {
		return host + " " + strings.TrimSpace(hostKey)
	}
	return fmt.Sprintf("[%s]:%d %s", host, port, strings.TrimSpace(hostKey))
}

// AddKnownHost adds an SSH host key to the known_hosts file.
func AddKnownHost(host string, port int, hostKey string) error {
	knownHostsPath := GetKnownHostsPath()

	// Ensure directory exists
	dir := filepath.Dir(knownHostsPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Append to file
	f, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open known_hosts: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(KnownHostLine(host, port, hostKey) + "\n"); err != nil {
		return fmt.Errorf("failed to write to known_hosts: %w", err)
	}

	return nil
}

// RemoveKnownHost deletes ONE line from ~/.shed/known_hosts: the exact line
// AddKnownHost would have written for this endpoint and key.
//
// It exists so a `shed server add` that pins a host key and then fails — an
// unauthorized SSH key, a verification that does not answer, a duplicate name —
// can leave the file exactly as it found it. Pinning a key for a server that was
// never added is quiet residue: it silently pre-trusts that endpoint for the
// next attempt, which is precisely the decision the operator was in the middle
// of making.
//
// The match is on the whole rendered line, so an entry the user (or an earlier
// successful add) put there is never touched — not even for the same host with a
// different key, and not a "@cert-authority"/"@revoked" marked line, whose
// leading token makes it a different line. Only the LAST occurrence is removed:
// the line this call is undoing is the one most recently appended.
//
// A missing file, or a file with no such line, is not an error — the caller is
// undoing something it may never have done.
func RemoveKnownHost(host string, port int, hostKey string) error {
	path := GetKnownHostsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read known_hosts %s: %w", path, err)
	}

	want := KnownHostLine(host, port, hostKey)
	lines := strings.Split(string(data), "\n")
	drop := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == want {
			drop = i
		}
	}
	if drop < 0 {
		return nil
	}
	lines = append(lines[:drop], lines[drop+1:]...)

	out := strings.Join(lines, "\n")
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if err := atomicWriteFile(path, []byte(out), 0600); err != nil {
		return fmt.Errorf("failed to rewrite known_hosts %s: %w", path, err)
	}
	return nil
}

// EnsureConfigDir ensures the config directory exists.
func EnsureConfigDir() error {
	dir := GetClientConfigDir()
	return os.MkdirAll(dir, 0700)
}
