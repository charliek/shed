package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/ext/protocol"
)

// DockerBackend resolves Docker registry credentials on the host. The
// (server, shed) pair selects the effective allowlist policy from the
// hierarchical Docker config; serverURL is the Docker registry being requested.
type DockerBackend interface {
	// GetCredentials returns credentials for the given registry, applying the
	// allowlist resolved for (server, shed).
	GetCredentials(ctx context.Context, server, shed, serverURL string) (*DockerCredential, error)

	// ListCredentials returns a map of allowed registry hostnames to usernames
	// for the policy resolved for (server, shed).
	ListCredentials(ctx context.Context, server, shed string) (map[string]string, error)

	// Status returns the allowlist mode and registry count resolved for
	// (server, shed).
	Status(server, shed string) (allowAll bool, registryCount int)
}

type DockerCredential struct {
	ServerURL string
	Username  string
	Secret    string
}

type helperExecutor interface {
	execHelper(ctx context.Context, helperName, serverURL string) (*DockerCredential, error)
}

// dockerConfig represents the relevant parts of ~/.docker/config.json.
type dockerConfig struct {
	CredHelpers map[string]string          `json:"credHelpers"`
	CredsStore  string                     `json:"credsStore"`
	Auths       map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Auth string `json:"auth"` // base64(user:pass)
}

// dockerHelperBackend resolves credentials by reading the host's Docker config
// and shelling out to credential helpers. The allowlist policy is resolved per
// (server, shed) request from cfg rather than fixed at construction.
type dockerHelperBackend struct {
	configPath string
	cfg        DockerConfig
	executor   helperExecutor
	helperDirs []string
	logger     *slog.Logger
}

// normalizeRegistrySet builds a normalized lookup set from a registries list.
func normalizeRegistrySet(registries []string) map[string]bool {
	allowed := make(map[string]bool, len(registries))
	for _, r := range registries {
		allowed[normalizeRegistry(r)] = true
	}
	return allowed
}

// extraHelperDirs are credential-helper install locations that may be absent
// from the inherited PATH — notably when shed-host-agent is started by launchd
// via `brew services`, which provides only a bare PATH
// (/usr/bin:/bin:/usr/sbin:/sbin). Docker Desktop symlinks its helper into
// /usr/local/bin; Homebrew installs into /opt/homebrew/bin (Apple Silicon) or
// /usr/local/bin (Intel).
var extraHelperDirs = []string{"/usr/local/bin", "/opt/homebrew/bin"}

// NewDockerBackend creates a Docker backend that reads from the host's Docker
// credential store. It returns an error only if an explicit config_path is
// specified but cannot be resolved; a missing default config is not an error.
func NewDockerBackend(cfg DockerConfig, logger *slog.Logger) (DockerBackend, error) {
	configPath := cfg.ConfigPath
	if configPath == "" {
		configPath = findDockerConfig()
	} else {
		if strings.HasPrefix(configPath, "~/") {
			home := userHomeDir()
			configPath = filepath.Join(home, configPath[2:])
		}
	}

	if configPath != "" {
		if _, err := os.Stat(configPath); err != nil && cfg.ConfigPath != "" {
			return nil, fmt.Errorf("docker config not found at %s: %w", configPath, err)
		}
	}

	b := &dockerHelperBackend{
		configPath: configPath,
		cfg:        cfg,
		helperDirs: extraHelperDirs,
		logger:     logger,
	}
	b.executor = b // default: real execution

	registryInfo := "none"
	if cfg.AllowAll {
		registryInfo = "all (allow_all)"
	} else if len(cfg.Registries) > 0 {
		registryInfo = strings.Join(cfg.Registries, ", ")
	}

	logger.Info("Docker backend initialized",
		"config", configPath,
		"registries", registryInfo,
	)

	return b, nil
}

func (b *dockerHelperBackend) GetCredentials(ctx context.Context, server, shed, serverURL string) (*DockerCredential, error) {
	normalized := normalizeRegistry(serverURL)

	resolved := b.cfg.Resolve(server, shed)
	if !resolved.AllowAll && !normalizeRegistrySet(resolved.Registries)[normalized] {
		return nil, &dockerError{
			msg:  fmt.Sprintf("registry %q not in allowlist", serverURL),
			code: protocol.DockerCodeNotAllowed,
		}
	}

	cfg, err := b.readConfig()
	if err != nil {
		return nil, &dockerError{
			msg:  fmt.Sprintf("reading docker config: %s", err),
			code: protocol.DockerCodeInternal,
		}
	}

	// Try credHelpers first (per-registry helper).
	// Look up both raw and normalized forms since Docker config may store
	// keys as "https://index.docker.io/v1/" while the guest sends "index.docker.io".
	if helper, ok := lookupConfigMap(cfg.CredHelpers, serverURL, normalized); ok {
		return b.executor.execHelper(ctx, helper, serverURL)
	}

	// Try credsStore (default helper)
	if cfg.CredsStore != "" {
		cred, err := b.executor.execHelper(ctx, cfg.CredsStore, serverURL)
		if err == nil {
			return cred, nil
		}
		b.logger.Debug("default credsStore helper failed, trying auths fallback",
			"helper", cfg.CredsStore, "error", err)
	}

	// Fall back to inline auths
	if auth, ok := lookupConfigMap(cfg.Auths, serverURL, normalized); ok && auth.Auth != "" {
		return decodeInlineAuth(serverURL, auth.Auth)
	}

	return nil, &dockerError{
		msg:  fmt.Sprintf("no credentials found for %q", serverURL),
		code: protocol.DockerCodeNotFound,
	}
}

func (b *dockerHelperBackend) ListCredentials(ctx context.Context, server, shed string) (map[string]string, error) {
	cfg, err := b.readConfig()
	if err != nil {
		return nil, fmt.Errorf("reading docker config: %w", err)
	}

	resolved := b.cfg.Resolve(server, shed)
	allowAll := resolved.AllowAll
	allowed := normalizeRegistrySet(resolved.Registries)

	result := make(map[string]string)

	// Collect from credHelpers
	for serverURL := range cfg.CredHelpers {
		if allowAll || allowed[normalizeRegistry(serverURL)] {
			result[serverURL] = "(credential helper)"
		}
	}

	// Collect from inline auths
	for serverURL, auth := range cfg.Auths {
		if allowAll || allowed[normalizeRegistry(serverURL)] {
			if auth.Auth != "" {
				cred, err := decodeInlineAuth(serverURL, auth.Auth)
				if err == nil {
					result[serverURL] = cred.Username
					continue
				}
			}
			result[serverURL] = "(auth entry)"
		}
	}

	return result, nil
}

func (b *dockerHelperBackend) Status(server, shed string) (bool, int) {
	resolved := b.cfg.Resolve(server, shed)
	return resolved.AllowAll, len(normalizeRegistrySet(resolved.Registries))
}

func (b *dockerHelperBackend) readConfig() (*dockerConfig, error) {
	if b.configPath == "" {
		return &dockerConfig{}, nil
	}

	data, err := os.ReadFile(b.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &dockerConfig{}, nil
		}
		return nil, err
	}

	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", b.configPath, err)
	}

	return &cfg, nil
}

// execHelper shells out to docker-credential-<helper> get with serverURL on stdin.
func (b *dockerHelperBackend) execHelper(ctx context.Context, helperName, serverURL string) (*DockerCredential, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	bin := "docker-credential-" + helperName
	helperPath, err := lookHelperPath(bin, b.helperDirs)
	if err != nil {
		// This is the most common cause of a silent "no credentials found":
		// the helper exists but isn't on the inherited (e.g. launchd) PATH.
		// Log loudly so it's diagnosable.
		b.logger.Warn("credential helper not found on PATH",
			"helper", bin, "searched", b.helperDirs, "error", err)
		return nil, &dockerError{
			msg:  fmt.Sprintf("%s not found: %s", bin, err),
			code: protocol.DockerCodeHelperFailed,
		}
	}

	cmd := exec.CommandContext(ctx, helperPath, "get")
	cmd.Stdin = strings.NewReader(serverURL)
	// Augment PATH so any tools the helper itself shells out to are also found
	// under a bare launchd PATH. Base on os.Environ() to preserve HOME etc.
	cmd.Env = augmentPATH(os.Environ(), b.helperDirs)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// A located helper that exits non-zero is expected control flow: for
		// credsStore, GetCredentials falls back to inline auths, so keep this at
		// Debug to avoid noise (and to keep potentially-sensitive helper stderr
		// out of default logs). The "not found on PATH" case above — the cause
		// of the silent bug — is what we log loudly. Stderr is logged locally
		// for diagnostics but never propagated to the guest.
		b.logger.Debug("credential helper failed", "helper", bin, "error", err, "stderr", strings.TrimSpace(stderr.String()))
		return nil, &dockerError{
			msg:  fmt.Sprintf("%s failed: %s", bin, err),
			code: protocol.DockerCodeHelperFailed,
		}
	}

	var cred struct {
		ServerURL string `json:"ServerURL"`
		Username  string `json:"Username"`
		Secret    string `json:"Secret"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &cred); err != nil {
		return nil, &dockerError{
			msg:  fmt.Sprintf("parsing %s output: %s", bin, err),
			code: protocol.DockerCodeHelperFailed,
		}
	}

	return &DockerCredential{
		ServerURL: cred.ServerURL,
		Username:  cred.Username,
		Secret:    cred.Secret,
	}, nil
}

// lookHelperPath resolves a credential-helper binary to an absolute path. It
// first honors the inherited PATH via exec.LookPath, then falls back to
// well-known install locations that may be missing from a bare launchd PATH.
//
// Resolving to an absolute path is required because exec.Command resolves a bare
// binary name against the current process's PATH at construction time —
// augmenting cmd.Env afterward does not affect that lookup.
func lookHelperPath(bin string, extraDirs []string) (string, error) {
	if p, err := exec.LookPath(bin); err == nil {
		return p, nil
	}
	for _, dir := range extraDirs {
		candidate := filepath.Join(dir, bin)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%q not found on PATH or in %s", bin, strings.Join(extraDirs, ", "))
}

// augmentPATH returns a copy of env with extraDirs appended to its PATH entry
// (skipping any already present). If env has no PATH entry, one is added. Basing
// the result on os.Environ() at the call site preserves other variables (e.g.
// HOME) that credential helpers rely on.
func augmentPATH(env, extraDirs []string) []string {
	out := make([]string, len(env))
	copy(out, env)

	// exec.Cmd.Env uses the last value for a duplicate key, so augment the last
	// PATH entry if the environment happens to contain more than one.
	last := -1
	for i, kv := range out {
		if strings.HasPrefix(kv, "PATH=") {
			last = i
		}
	}
	if last == -1 {
		return append(out, "PATH="+strings.Join(extraDirs, string(os.PathListSeparator)))
	}

	dirs := filepath.SplitList(out[last][len("PATH="):])
	seen := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		seen[d] = true
	}
	for _, d := range extraDirs {
		if !seen[d] {
			dirs = append(dirs, d)
			seen[d] = true
		}
	}
	out[last] = "PATH=" + strings.Join(dirs, string(os.PathListSeparator))
	return out
}

// findDockerConfig returns the path to the Docker config.json, checking
// $DOCKER_CONFIG first, then falling back to ~/.docker/config.json.
func findDockerConfig() string {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		p := filepath.Join(dir, "config.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	home := userHomeDir()
	p := filepath.Join(home, ".docker", "config.json")
	if _, err := os.Stat(p); err == nil {
		return p
	}

	return ""
}

// normalizeRegistry strips protocol prefix and trailing slashes to produce
// a canonical hostname for allowlist matching.
func normalizeRegistry(s string) string {
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, "/v1")
	s = strings.TrimSuffix(s, "/v2")
	return s
}

// lookupConfigMap searches a Docker config map using both the raw and normalized
// registry key, handling the mismatch between Docker's stored keys (e.g.
// "https://index.docker.io/v1/") and the guest-provided hostname ("index.docker.io").
func lookupConfigMap[V any](m map[string]V, raw, normalized string) (V, bool) {
	if v, ok := m[raw]; ok {
		return v, true
	}
	if raw != normalized {
		if v, ok := m[normalized]; ok {
			return v, true
		}
	}
	// Try normalizing the map keys to match
	for k, v := range m {
		if normalizeRegistry(k) == normalized {
			return v, true
		}
	}
	var zero V
	return zero, false
}

func decodeInlineAuth(serverURL, encoded string) (*DockerCredential, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decoding auth for %s: %w", serverURL, err)
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid auth format for %s", serverURL)
	}

	return &DockerCredential{
		ServerURL: serverURL,
		Username:  parts[0],
		Secret:    parts[1],
	}, nil
}

type dockerError struct {
	msg  string
	code string
}

func (e *dockerError) Error() string { return e.msg }
