package config

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// ClientConfig represents the CLI-side configuration.
type ClientConfig struct {
	Servers       map[string]ServerEntry `yaml:"servers"`
	DefaultServer string                 `yaml:"default_server"`
	Sheds         map[string]ShedCache   `yaml:"sheds"`
	CreateTimeout time.Duration          `yaml:"create_timeout,omitempty"`

	// updateMu serializes Update against itself inside THIS process, so the
	// read-modify-write it performs on the in-memory maps is never interleaved
	// with another goroutine's.
	//
	// It is deliberately narrow: it guards mutation, not reading. The CLI keeps
	// its own package-level `configMu` (cmd/shed/client.go) around the paths
	// that READ the shared clientConfig maps — the server-name lookups the
	// `--all` fan-out performs concurrently — and routes every write through
	// one helper that takes configMu and then calls Update. That is what makes
	// reads and writes mutually exclusive; this mutex only guarantees that
	// Update itself is atomic for any other holder of the same *ClientConfig.
	//
	// Nothing outside Update takes it, so it can never be held while the config
	// FILE lock is being acquired by an unrelated path (see Update's ordering
	// note).
	updateMu sync.Mutex

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

// UsesTLS reports whether this entry's control plane is HTTPS — which, for a
// shed-server, means the server enforces auth (token or mtls). Open mode serves
// plain HTTP and has no HTTPS listener at all, so this is the one durable
// signal in a stored entry that separates "this server wants a credential" from
// "this server wants nothing".
//
// Either marker alone is enough: `shed server add` writes both an https api_url
// and a tls_cert_fingerprint for a secure server, but an entry is user-editable
// and a half-filled one still points at a listener that will demand a
// credential.
func (e *ServerEntry) UsesTLS() bool {
	return e.TLSCertFingerprint != "" ||
		strings.HasPrefix(strings.ToLower(e.APIURL), "https://")
}

// NeedsEnrollment reports whether this entry holds NO credential it could
// present to a server that demands one, and has enough information (host + ssh
// port) to obtain one over the SSH `_bootstrap` channel.
//
// It exists because "no credential recorded" is ambiguous in a stored entry and
// the two readings need opposite handling:
//
//   - an OPEN server legitimately has no credential, and must never pay an SSH
//     round-trip to discover that. It is plain HTTP, so UsesTLS is false.
//   - a SECURE server's entry that lost its credential fields must enroll. That
//     is a routine condition during an upgrade, not a corner case: a pre-mtls
//     client that loads and re-saves config.yaml silently drops every key its
//     ServerEntry struct does not know (auth_mode, client_cert_file,
//     client_key_file, client_cert_expires_at), leaving exactly this shape —
//     an https entry with nothing to present. Without enrollment such an entry
//     fails forever, because the client has no credential to be rejected and so
//     never reaches its reactive re-mint.
//
// A legacy static token (a control_token with no expiry) is NOT this case: it
// has something to present and is deliberately never re-minted.
func (e *ServerEntry) NeedsEnrollment() bool {
	if !e.UsesTLS() || e.Host == "" || e.SSHPort == 0 {
		return false
	}
	if e.IsMTLS() {
		return e.ClientCertFile == "" || e.ClientKeyFile == ""
	}
	return e.ControlToken == ""
}

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

// clientConfigLockName is the basename of the advisory lock file that
// serializes whole-file client-config updates ACROSS PROCESSES.
//
// It is a SIBLING of config.yaml — derived from the config path, so a config
// loaded from an arbitrary location (LoadClientConfigFromPath) locks beside
// itself rather than against some other tree's file — and deliberately NOT the
// credential store's `%lock/` directory: config locking must not couple to the
// creds Store's root or lifecycle, and the two locks are taken together in a
// pinned order (see cmd/shed/client.go's persist paths).
//
// The "%" prefix keeps it collision-proof against anything else this directory
// holds: every other name here is a fixed English basename (config.yaml,
// known_hosts, tunnels.yaml, creds/, logs/), and no shed-server name reaches
// this level at all.
const clientConfigLockName = "%config.lock"

// lockClientConfig takes the exclusive advisory lock guarding whole-file
// updates to the config at path, and returns the release function.
//
// It is held across the WHOLE read-modify-write of Update — the fresh load, the
// mutation, and the rename — because that is the only span in which "merge my
// change into what is currently on disk" means anything. SaveToPath serializes
// the ENTIRE snapshot, so two unsynchronized processes do not merge: the second
// rename discards everything the first wrote.
//
// The lock FILE is created if absent and then NEVER removed, for the same
// inode-stability reason as sdk/creds' per-server locks: deleting it out from
// under a process that holds it lets the next locker create a fresh inode and
// lock THAT instead — two "exclusive" holders, the classic broken-mutex shape.
// An empty file costs nothing to leave behind.
func lockClientConfig(path string) (func(), error) {
	if path == "" {
		return nil, errors.New("client config: no path to lock")
	}
	dir := filepath.Dir(path)
	// The config directory may not exist yet — a first `shed server add` writes
	// into a bare home. SaveToPath creates it for the same reason; the lock has
	// to exist BEFORE the save, so it creates it too.
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create config directory: %w", err)
	}
	lockPath := filepath.Join(dir, clientConfigLockName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open config lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock config %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// Update applies mutate to the client configuration as a locked
// read-modify-write against what is CURRENTLY on disk, and is the only
// supported way to change a config that other processes may also be changing.
//
// The problem it solves is that SaveToPath writes the whole document: a `shed
// list` that loaded the config a minute ago and then refreshes its shed cache
// renames its entire stale snapshot over whatever a concurrent `shed server
// add` — or a credential re-mint — just committed. Last writer wins, and what
// it wins with is everything, including the fields it never touched.
//
// mutate cannot fail, by type. That is the whole reason this signature is not
// `func(*ClientConfig) error`: the mutation is applied TWICE (see
// UpdateChecked), with the file write in between, so a mutation that could fail
// on its second application would report an error for a change already durably
// committed. Anything that needs to say no is a precondition, and preconditions
// belong in UpdateChecked's check — which runs once, on the fresh snapshot,
// before anything is written.
func (c *ClientConfig) Update(mutate func(*ClientConfig)) error {
	return c.UpdateChecked(nil, mutate)
}

// UpdateChecked is Update with a precondition evaluated under the lock, against
// the config as it is on disk at that instant.
//
// It exists because "validate, then update" is not a safe shape when another
// process may be writing the same file: a check performed before the lock is
// taken reads a snapshot that can be stale by the time the write lands — two
// concurrent `shed server add`s for one name both see "not taken", and both
// succeed. The check has to happen inside the locked section, against the fresh
// snapshot, and it has to happen exactly once.
//
// The sequence is:
//
//  1. take updateMu (in-process) and then the config file lock (cross-process,
//     blocking; ORDER: the creds lock, when a caller needs both, is taken by
//     that caller BEFORE this — see cmd/shed/client.go — and nothing here ever
//     takes a creds lock);
//  2. load a FRESH snapshot from the receiver's path (a missing file is the
//     empty config, exactly as LoadClientConfigFromPath treats it);
//  3. run check on that snapshot; a non-nil error aborts with NOTHING written
//     and the receiver untouched;
//  4. run mutate on the snapshot and save it;
//  5. run the SAME mutate on the receiver.
//
// Step 5 is a re-application rather than a wholesale replacement on purpose:
// concurrent readers of the in-memory config hold live map values, and swapping
// the maps out from under them would make an unrelated entry momentarily
// vanish. It also means mutate must be a deterministic function of the config
// it is handed — one that stamps its own time.Now() leaves the file and the
// running process holding rows that differ (which is why CacheShedAt exists
// beside CacheShed, and why AddServer must not be called from here).
//
// check is evaluated ONLY on the fresh snapshot. The receiver is this process's
// view of that same file, so re-checking it would either be redundant or, in
// the one case where the two disagree, would fail after the write committed.
func (c *ClientConfig) UpdateChecked(check func(*ClientConfig) error, mutate func(*ClientConfig)) error {
	if mutate == nil {
		return errors.New("client config: Update requires a mutation")
	}
	if c.path == "" {
		return errors.New("client config: this configuration has no path to save to")
	}

	c.updateMu.Lock()
	defer c.updateMu.Unlock()

	unlock, err := lockClientConfig(c.path)
	if err != nil {
		return err
	}
	defer unlock()

	fresh, err := LoadClientConfigFromPath(c.path)
	if err != nil {
		return err
	}
	if check != nil {
		if err := check(fresh); err != nil {
			return err
		}
	}
	mutate(fresh)
	if err := fresh.SaveToPath(c.path); err != nil {
		return err
	}
	mutate(c)
	return nil
}

// Save writes the configuration to disk.
//
// It is the UNSYNCHRONIZED writer: it renames this whole snapshot over the file,
// discarding anything another process committed since this one loaded. Reach for
// Update instead for any mutation the CLI performs; Save survives for the
// first-write paths that own the file outright and for tests.
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

	// Write atomically via a temp file whose name is unique to this attempt.
	//
	// A shared "<path>.tmp" is a second, unlocked rendezvous point: two writers
	// pick the same name, interleave their bytes in it, and one of them renames
	// the other's half-written document into place — an atomic rename of
	// corrupt content. Pid + a random component makes collision practically
	// impossible even between two writers that are not otherwise serialized
	// (Update's lock covers the CLI's own writers; this covers everything else,
	// including a crash-orphaned name being reused). Orphans left by a crash
	// between create and rename are accepted; the rename-failure cleanup below
	// is what handles the ordinary case.
	tmpPath := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), rand.Uint64())
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		// WriteFile creates the file before it can fail on the write itself, so
		// the failure path has to clean up too — otherwise every full-disk or
		// interrupted save leaves another uniquely-named orphan next to the
		// config, and unique names mean nothing ever reclaims them.
		os.Remove(tmpPath)
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
//
// Like CacheShed, it is NOT safe as an Update mutation: it stamps AddedAt with
// time.Now() (two applications, two instants) and it can FAIL — and an Update
// mutation that fails on the second application fails after the file has
// already been written, turning a successful add into a reported error. Callers
// under Update validate first and then assign a fully-formed entry; see
// cmd/shed/server_add.go:saveAddedServer.
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

	// Clear default if we removed it, promoting the alphabetically first
	// remaining server. Sorted rather than "whatever the map yields first":
	// Update applies a mutation to two snapshots (the fresh on-disk one and the
	// caller's in-memory one) and a map-order pick would hand them different
	// defaults, so the file and the running process would disagree about which
	// server is now default.
	if c.DefaultServer == name {
		c.DefaultServer = ""
		for _, serverName := range slices.Sorted(maps.Keys(c.Servers)) {
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

// CacheShed caches a shed's location, stamped now.
//
// It is NOT safe inside an Update mutation: Update runs the mutation twice (the
// fresh on-disk snapshot, then the caller's in-memory one) and the two calls
// would stamp two different instants, so the file and the running process would
// hold rows that differ in a field neither ever meant to disagree on. Use
// CacheShedAt with a timestamp taken once, outside the mutation.
func (c *ClientConfig) CacheShed(name string, server string, status string) {
	c.CacheShedAt(name, server, status, time.Now())
}

// CacheShedAt is CacheShed with the timestamp supplied by the caller, which is
// what makes a cache write a deterministic function of its inputs and therefore
// usable as an Update mutation.
func (c *ClientConfig) CacheShedAt(name string, server string, status string, at time.Time) {
	c.Sheds[name] = ShedCache{
		Server:    server,
		Status:    status,
		UpdatedAt: at,
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
