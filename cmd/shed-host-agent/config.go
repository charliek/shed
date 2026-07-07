package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// userHomeDir returns the user's home directory, falling back to /tmp if unavailable.
func userHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("could not determine home directory, using /tmp", "error", err)
		return "/tmp"
	}
	return home
}

// expandTilde expands a leading "~/" in path to the user's home directory.
func expandTilde(path string) string {
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(userHomeDir(), path[2:])
	}
	return path
}

// serverShedKey builds the composite key used to scope per-shed state (the AWS
// credential cache, Touch ID approvals) to a single server, so identical shed
// names on different servers stay isolated.
func serverShedKey(server, shed string) string { return server + "/" + shed }

// Config is the top-level configuration for shed-host-agent.
type Config struct {
	// Server is the legacy single-server URL. It is used only when Discovery
	// is absent (nil). When Discovery is set, this field is ignored.
	Server string `yaml:"server"`
	// Discovery enables multi-server mode: the agent discovers shed servers
	// from shed's own CLI config and brokers for all (or a selected subset) of
	// them from a single process. nil (block omitted) => legacy single-server.
	Discovery *DiscoveryConfig `yaml:"discovery"`
	// ApprovalTimeout is how long a delegated approval decision (an extension
	// with `approval.policy: shed-desktop`) may take before the agent fails
	// closed (deny). Go duration string; default "25s".
	ApprovalTimeout string       `yaml:"approval_timeout"`
	SSH             SSHConfig    `yaml:"ssh"`
	AWS             AWSConfig    `yaml:"aws"`
	Docker          DockerConfig `yaml:"docker"`
	Logging         LogConfig    `yaml:"logging"`
	// Desktop is deprecated and ignored — see DesktopConfig.
	Desktop DesktopConfig `yaml:"desktop"`
}

// DiscoveryConfig selects which shed servers a single agent process watches and
// how it reacts to changes in the discovery source.
type DiscoveryConfig struct {
	// Servers selects which discovered servers to watch: the scalar "all"
	// (default) or a YAML list of server names.
	Servers ServerSelector `yaml:"servers"`
	// Source overrides the shed CLI config path (default ~/.shed/config.yaml).
	Source string `yaml:"source"`
	// Watch controls live reloading of the discovery source:
	// "fsnotify" (default), "poll", or "off".
	Watch string `yaml:"watch"`
	// PollInterval is the reconcile cadence when Watch == "poll" (default 10s).
	PollInterval string `yaml:"poll_interval"`
	// Debounce coalesces rapid fsnotify events when Watch == "fsnotify"
	// (default 500ms).
	Debounce string `yaml:"debounce"`
}

// ServerSelector chooses which discovered servers to watch. It unmarshals from
// either the scalar "all" (every discovered server) or a YAML list of names.
type ServerSelector struct {
	All   bool
	Names []string
}

// UnmarshalYAML accepts either a scalar ("all" or a single name) or a sequence
// of server names.
func (s *ServerSelector) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Value == "" || value.Value == "all" {
			s.All = true
			return nil
		}
		s.Names = []string{value.Value}
		return nil
	case yaml.SequenceNode:
		var names []string
		if err := value.Decode(&names); err != nil {
			return err
		}
		// Keep an explicit empty list non-nil so it is distinguishable from an
		// omitted selector (nil) — `servers: []` means "watch none", not "all".
		if names == nil {
			names = []string{}
		}
		s.Names = names
		return nil
	default:
		return fmt.Errorf("discovery.servers must be \"all\" or a list of server names")
	}
}

// Selected reports whether a discovered server name should be watched. An
// omitted selector (nil Names) selects every server; an explicit empty list
// (`servers: []`) selects none.
func (s ServerSelector) Selected(name string) bool {
	if s.All || s.Names == nil {
		return true
	}
	for _, n := range s.Names {
		if n == name {
			return true
		}
	}
	return false
}

// ServerTarget is a resolved shed server this agent brokers for. Name is the
// identity key (the discovery name, or empty in single-server mode); URL is the
// shed-server broker base URL (http://host:port, or an https api_url).
type ServerTarget struct {
	Name string
	URL  string
	// Token is the credentials-scoped bearer token sent to the shed-server
	// credential bus. Empty when the server isn't token-gated. In secure mode
	// the agent mints this itself over SSH (Phase 4) rather than reading a pasted
	// credentials_token.
	Token string
	// TLSFingerprint pins the server's self-signed TLS cert ("sha256:<hex>")
	// when URL is https. Empty for plain HTTP.
	TLSFingerprint string
	// SSHHost / SSHPort are the server's SSH endpoint, used to mint a credentials
	// token over the _bootstrap channel (sdk/bootstrap.Run). Every shed server has an
	// SSH endpoint (it is how sheds are reached), so this is NOT the open/secure
	// signal — IsSecure (the https scheme) is. SSHPort == 0 only when the discovery
	// entry omitted ssh_port, in which case the agent can't self-mint regardless.
	SSHHost string
	SSHPort int
}

// IsSecure reports whether the server is reached over https — the authoritative
// local signal that it runs in secure mode (tokens ⟺ TLS ⟺ secure) and that the
// agent should self-mint a token. Scheme, not TLSFingerprint presence, is the
// signal: it matches the SDK's applyTLSPin / shed's isHTTPSURL idiom and tolerates
// a hand-edited config that keeps https but drops the pin.
func (t ServerTarget) IsSecure() bool {
	return strings.HasPrefix(strings.ToLower(t.URL), "https://")
}

// ResolveTargets computes the desired set of servers to watch. In single-server
// mode (Discovery nil) it returns a single unnamed target from Config.Server,
// ignoring the discovered list. In discovery mode it filters discovered
// servers by the selector and dedups by name.
func (c Config) ResolveTargets(discovered []ServerTarget) []ServerTarget {
	if c.Discovery == nil {
		// Single-server mode: one unnamed target. An empty name keeps the
		// `server` field out of audit logs and desktop events (omitempty), and
		// scopes per-shed state under "/<shed>" — there is only one server.
		return []ServerTarget{{Name: "", URL: c.Server}}
	}
	out := make([]ServerTarget, 0, len(discovered))
	seen := make(map[string]bool, len(discovered))
	for _, t := range discovered {
		if !c.Discovery.Servers.Selected(t.Name) || seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		out = append(out, t)
	}
	return out
}

// DesktopConfig is deprecated and ignored. The approval channel is always on:
// its socket lives at a fixed path (see sockets.go), and the approval budget
// moved to the top-level `approval_timeout`. The struct remains only so
// LoadConfig can warn when an old config still sets these keys.
type DesktopConfig struct {
	// All pointers so an explicitly-set value (even a zero/empty one) is
	// distinguishable from an omitted key — warnDeprecatedDesktopKeys warns on
	// presence, not on a non-zero value.
	//
	// Deprecated: ignored. The approval channel is always on.
	Enabled *bool `yaml:"enabled"`
	// Deprecated: ignored. The socket path is fixed (see desktopSocketPath).
	SocketPath *string `yaml:"socket_path"`
	// Deprecated: ignored. Use the top-level `approval_timeout` instead.
	TimeoutMS *int `yaml:"timeout_ms"`
}

// DockerConfig controls the Docker registry credential handler behavior. The
// top-level fields are the defaults; Servers carries per-server (and per-shed)
// overrides layered over them. ConfigPath is process-global.
type DockerConfig struct {
	Registries []string       `yaml:"registries"`  // registry hostnames to allow (default)
	AllowAll   bool           `yaml:"allow_all"`   // bypass allowlist (default)
	ConfigPath string         `yaml:"config_path"` // override Docker config.json path
	Approval   ApprovalConfig `yaml:"approval"`
	// Servers holds per-server overrides, each optionally with per-shed nesting.
	Servers map[string]DockerServerConfig `yaml:"servers"`
}

// DockerServerConfig holds per-server Docker overrides. AllowAll is a pointer so
// an unset value inherits the default rather than forcing false.
type DockerServerConfig struct {
	Registries []string                    `yaml:"registries"`
	AllowAll   *bool                       `yaml:"allow_all"`
	Sheds      map[string]DockerShedConfig `yaml:"sheds"`
}

// DockerShedConfig holds per-shed Docker overrides.
type DockerShedConfig struct {
	Registries []string `yaml:"registries"`
	AllowAll   *bool    `yaml:"allow_all"`
}

// ResolvedDocker is the effective Docker policy for a single (server, shed) pair.
type ResolvedDocker struct {
	Registries []string
	AllowAll   bool
}

// Resolve layers Docker overrides for a (server, shed) pair, most specific wins:
// top-level defaults -> Servers[server] -> Servers[server].Sheds[shed]. A
// non-nil Registries list replaces (does not merge) the inherited list.
func (d DockerConfig) Resolve(server, shed string) ResolvedDocker {
	r := ResolvedDocker{Registries: d.Registries, AllowAll: d.AllowAll}
	sv, ok := d.Servers[server]
	if !ok {
		return r
	}
	if sv.Registries != nil {
		r.Registries = sv.Registries
	}
	if sv.AllowAll != nil {
		r.AllowAll = *sv.AllowAll
	}
	if sc, ok := sv.Sheds[shed]; ok {
		if sc.Registries != nil {
			r.Registries = sc.Registries
		}
		if sc.AllowAll != nil {
			r.AllowAll = *sc.AllowAll
		}
	}
	return r
}

// AWS credential vending modes. Mode selects how the agent obtains the creds it
// vends for a shed:
//
//	assume-role (default) — sts:AssumeRole from SourceProfile into the resolved
//	                        role, vending the short-lived STS session.
//	passthrough           — vend SourceProfile's existing session credentials
//	                        directly (no AssumeRole), for SSO/SAML setups where
//	                        no assumable role exists. Any resolved role is ignored.
//
// An empty mode means assume-role (back-compat).
const (
	AWSModeAssumeRole  = "assume-role"
	AWSModePassthrough = "passthrough"
)

// AWSConfig controls the AWS credential handler behavior. The top-level fields
// are the defaults; Servers carries per-server (and per-shed) overrides layered
// over them. SourceProfile and CacheRefreshBefore are process-global and are not
// overridable per server.
type AWSConfig struct {
	SourceProfile      string         `yaml:"source_profile"`
	DefaultRole        string         `yaml:"default_role"`
	Mode               string         `yaml:"mode"` // "" (=assume-role) | assume-role | passthrough
	SessionDuration    string         `yaml:"session_duration"`
	CacheRefreshBefore string         `yaml:"cache_refresh_before"`
	Approval           ApprovalConfig `yaml:"approval"`
	// Sheds is the removed global per-shed override map. It is parsed only so
	// Validate can reject configs that still set it with a migration message; it
	// no longer affects resolution. Use Servers.<name>.sheds.<name> instead.
	Sheds map[string]ShedAWSConfig `yaml:"sheds"`
	// Servers holds per-server overrides, each optionally with per-shed nesting.
	Servers map[string]AWSServerConfig `yaml:"servers"`
}

// AWSServerConfig holds per-server AWS overrides.
type AWSServerConfig struct {
	DefaultRole     string                   `yaml:"default_role"`
	Mode            string                   `yaml:"mode"`
	SessionDuration string                   `yaml:"session_duration"`
	Sheds           map[string]ShedAWSConfig `yaml:"sheds"`
}

// ShedAWSConfig holds per-shed AWS overrides.
type ShedAWSConfig struct {
	Role            string `yaml:"role"`
	Mode            string `yaml:"mode"`
	SessionDuration string `yaml:"session_duration"`
}

// ResolvedAWS is the effective AWS policy for a single (server, shed) pair.
type ResolvedAWS struct {
	Role            string // assumed-role ARN ("" => no role configured)
	Mode            string // AWSModeAssumeRole | AWSModePassthrough (never "")
	SessionDuration string // "" => fall back to the backend default
}

// Resolve layers AWS overrides for a (server, shed) pair, most specific wins:
// top-level defaults -> Servers[server] -> Servers[server].Sheds[shed]. Role,
// Mode, and SessionDuration each layer independently; an empty Mode means "no
// override" while layering and is normalized to AWSModeAssumeRole at the end
// (so a child that only sets a role under a passthrough parent stays passthrough).
func (a AWSConfig) Resolve(server, shed string) ResolvedAWS {
	r := ResolvedAWS{Role: a.DefaultRole, Mode: a.Mode, SessionDuration: a.SessionDuration}
	if sv, ok := a.Servers[server]; ok {
		if sv.DefaultRole != "" {
			r.Role = sv.DefaultRole
		}
		if sv.Mode != "" {
			r.Mode = sv.Mode
		}
		if sv.SessionDuration != "" {
			r.SessionDuration = sv.SessionDuration
		}
		if sc, ok := sv.Sheds[shed]; ok {
			if sc.Role != "" {
				r.Role = sc.Role
			}
			if sc.Mode != "" {
				r.Mode = sc.Mode
			}
			if sc.SessionDuration != "" {
				r.SessionDuration = sc.SessionDuration
			}
		}
	}
	r.Mode = normalizeAWSMode(r.Mode)
	return r
}

// normalizeAWSMode maps an empty mode to the assume-role default.
func normalizeAWSMode(mode string) string {
	if mode == "" {
		return AWSModeAssumeRole
	}
	return mode
}

// Enabled reports whether the AWS handler should start at all: true if any
// resolution path selects passthrough mode or configures a non-empty role. An
// explicit assume-role with no role anywhere is "AWS off" (false).
func (a AWSConfig) Enabled() bool {
	if a.Mode == AWSModePassthrough || a.DefaultRole != "" {
		return true
	}
	for _, sv := range a.Servers {
		if sv.Mode == AWSModePassthrough || sv.DefaultRole != "" {
			return true
		}
		for _, s := range sv.Sheds {
			if s.Mode == AWSModePassthrough || s.Role != "" {
				return true
			}
		}
	}
	return false
}

// Approval policy values. Each credential extension (ssh/aws/docker) selects one
// via its `approval.policy`. An empty/omitted policy means PolicyDenyAll — the
// safe default, so a misconfigured extension fails closed rather than open.
const (
	PolicyDenyAll              = "deny-all"               // reject every request
	PolicyApproveAll           = "approve-all"            // allow every request (allowlist/role still applies)
	PolicyBiometrics           = "biometrics"             // native Touch ID, biometrics only (SSH only)
	PolicyBiometricsOrPassword = "biometrics-or-password" // native Touch ID + Apple Watch / password fallback (SSH only)
	PolicyShedDesktop          = "shed-desktop"           // delegate the decision to the shed-desktop app
)

// SSHConfig controls the SSH agent handler behavior.
type SSHConfig struct {
	Mode     string         `yaml:"mode"` // "agent-forward", "local-keys", or "" (auto)
	Approval ApprovalConfig `yaml:"approval"`
}

// ApprovalConfig controls how a credential extension's requests are approved.
// Policy is the single decision (see the Policy* constants). Scope and SessionTTL
// apply ONLY to the native biometric policies (they cache a Touch ID approval);
// under shed-desktop the app owns scope/ttl, and under deny-all/approve-all they
// are unused.
type ApprovalConfig struct {
	Policy     string `yaml:"policy"`
	Scope      string `yaml:"scope"`       // "per-request" | "per-session" | "per-shed" (biometric policies)
	SessionTTL string `yaml:"session_ttl"` // e.g., "4h" (biometric policies)
}

// EffectivePolicy returns the configured policy, defaulting an empty value to
// PolicyDenyAll (fail-closed).
func (a ApprovalConfig) EffectivePolicy() string {
	if a.Policy == "" {
		return PolicyDenyAll
	}
	return a.Policy
}

// resolveAllowPassword reports whether a native biometric policy may fall back
// to Apple Watch / account password (LAPolicyDeviceOwnerAuthentication) — true
// for "biometrics-or-password" so approval works in clamshell mode and on Macs
// without a sensor; false for "biometrics" (Touch ID only).
func resolveAllowPassword(policy string) bool {
	return policy != PolicyBiometrics
}

// LogConfig controls audit logging.
type LogConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	home := userHomeDir()
	return Config{
		Server:          "http://localhost:8080",
		ApprovalTimeout: "25s",
		SSH: SSHConfig{
			// Policy is intentionally empty here => deny-all (fail closed) unless
			// the config sets one. Scope/SessionTTL are the defaults a native
			// biometric policy uses when the config omits them.
			Approval: ApprovalConfig{
				Scope:      "per-session",
				SessionTTL: "4h",
			},
		},
		AWS: AWSConfig{
			SourceProfile:      "default",
			SessionDuration:    "1h",
			CacheRefreshBefore: "5m",
		},
		Logging: LogConfig{
			Enabled: true,
			Path:    filepath.Join(home, ".local", "share", "shed", "extensions-audit.log"),
		},
	}
}

// LoadConfig reads and parses the config file, applying defaults for missing fields.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	path = expandTilde(path)

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading config: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config: %w", err)
	}

	cfg.Logging.Path = expandTilde(cfg.Logging.Path)
	warnDeprecatedDesktopKeys(cfg.Desktop)

	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid config: %w", err)
	}

	// Apply discovery defaults and expand its source path (only when the
	// discovery block is present — its absence means legacy single-server).
	if cfg.Discovery != nil {
		cfg.Discovery.applyDefaults()
	}

	return cfg, nil
}

// Validate checks each extension's approval.policy is a recognized value for
// that extension. An empty policy is allowed (it means deny-all). SSH alone
// supports the native biometric policies; AWS/Docker support only
// deny-all/approve-all/shed-desktop (native per-request Touch ID is a poor fit
// for background credential refresh / per-pull).
func (c Config) Validate() error {
	sshAllowed := []string{PolicyDenyAll, PolicyApproveAll, PolicyBiometrics, PolicyBiometricsOrPassword, PolicyShedDesktop}
	credAllowed := []string{PolicyDenyAll, PolicyApproveAll, PolicyShedDesktop}
	if err := validatePolicy("ssh", c.SSH.Approval.Policy, sshAllowed); err != nil {
		return err
	}
	if err := validatePolicy("aws", c.AWS.Approval.Policy, credAllowed); err != nil {
		return err
	}
	if err := validatePolicy("docker", c.Docker.Approval.Policy, credAllowed); err != nil {
		return err
	}
	if err := c.AWS.validate(); err != nil {
		return err
	}
	if _, err := c.ApprovalTimeoutDuration(); err != nil {
		return err
	}
	return nil
}

// ApprovalTimeoutDuration parses approval_timeout, requiring a positive Go
// duration. An empty value falls back to the 25s default (so a config that
// omits the key is valid).
func (c Config) ApprovalTimeoutDuration() (time.Duration, error) {
	v := c.ApprovalTimeout
	if v == "" {
		v = "25s"
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("approval_timeout %q is not a valid duration: %w", c.ApprovalTimeout, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("approval_timeout %q must be positive", c.ApprovalTimeout)
	}
	return d, nil
}

// warnDeprecatedDesktopKeys logs a warning for each deprecated desktop.* key
// that is still set in the config. They are ignored: the approval channel is
// always on at a fixed socket path, and the approval budget is the top-level
// approval_timeout.
func warnDeprecatedDesktopKeys(d DesktopConfig) {
	if d.Enabled != nil {
		slog.Warn("config: `desktop.enabled` is deprecated and ignored — the approval channel is always on")
	}
	if d.SocketPath != nil {
		slog.Warn("config: `desktop.socket_path` is deprecated and ignored — the socket path is fixed",
			"path", desktopSocketPath())
	}
	if d.TimeoutMS != nil {
		slog.Warn("config: `desktop.timeout_ms` is deprecated and ignored — use the top-level `approval_timeout`",
			"timeout_ms", *d.TimeoutMS)
	}
}

func validatePolicy(provider, policy string, allowed []string) error {
	if policy == "" {
		return nil // empty => deny-all
	}
	for _, a := range allowed {
		if policy == a {
			return nil
		}
	}
	return fmt.Errorf("%s.approval.policy %q is not one of %s", provider, policy, strings.Join(allowed, ", "))
}

// validate checks AWS-specific config: the removed global aws.sheds map is
// rejected with migration guidance (fail-closed — silently ignoring it could
// over-grant by falling back to default_role), and every mode field must be a
// known value.
func (a AWSConfig) validate() error {
	if len(a.Sheds) > 0 {
		return fmt.Errorf("aws.sheds was removed; move entries under aws.servers.<server>.sheds.<shed>")
	}
	if err := validateMode("aws.mode", a.Mode); err != nil {
		return err
	}
	for name, sv := range a.Servers {
		if err := validateMode(fmt.Sprintf("aws.servers.%s.mode", name), sv.Mode); err != nil {
			return err
		}
		for shed, sc := range sv.Sheds {
			if err := validateMode(fmt.Sprintf("aws.servers.%s.sheds.%s.mode", name, shed), sc.Mode); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateMode allows an empty value (meaning assume-role) or a known mode.
func validateMode(field, mode string) error {
	switch mode {
	case "", AWSModeAssumeRole, AWSModePassthrough:
		return nil
	default:
		return fmt.Errorf("%s %q is not one of %s, %s", field, mode, AWSModeAssumeRole, AWSModePassthrough)
	}
}

// DefaultDiscoverySource is the shed CLI client config the agent discovers
// servers from when discovery is enabled without an explicit source.
const DefaultDiscoverySource = "~/.shed/config.yaml"

// applyDefaults fills unset discovery fields and expands ~ in the source path.
func (d *DiscoveryConfig) applyDefaults() {
	if d.Source == "" {
		d.Source = DefaultDiscoverySource
	}
	d.Source = expandTilde(d.Source)
	if d.Watch == "" {
		d.Watch = "fsnotify"
	}
	if d.PollInterval == "" {
		d.PollInterval = "10s"
	}
	if d.Debounce == "" {
		d.Debounce = "500ms"
	}
}
