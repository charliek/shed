package config

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charliek/shed/internal/redact"
	"github.com/charliek/shed/internal/remote/provider"
)

var (
	envVarNameRegex     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	remoteSizeNameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	machineTypeRegex    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// applyDefaults fills the zero-valued fields of a remote block. It never
// sets environment, state_dir, default_image or agent_dir: the first and
// third are required, the other two default relative to the server's state
// directory (StateDirPath, AgentDirPath).
func (c *RemoteConfig) applyDefaults() {
	if c.Provider == "" {
		c.Provider = RemoteProviderGCE
	}
	if c.Transport == "" {
		c.Transport = RemoteTransportSSH
	}
	if c.DefaultSize == "" {
		c.DefaultSize = provider.SizeStandard
	}
	if len(c.Sizes) == 0 {
		c.Sizes = make(map[string]RemoteSizeConfig)
		for _, s := range provider.DefaultSizes() {
			c.Sizes[s.Name] = RemoteSizeConfig{MachineType: s.MachineType, CPUs: s.CPUs, MemoryMB: s.MemoryMB, Arch: s.Arch}
		}
	}
	for name, s := range c.Sizes {
		if s.Arch == "" {
			s.Arch = provider.ArchAMD64
			c.Sizes[name] = s
		}
	}
	if c.DiskGB == 0 {
		c.DiskGB = provider.DefaultDiskGB
	}
	if c.Limits.MaxRunning == 0 {
		c.Limits.MaxRunning = DefaultRemoteMaxRunning
	}
	if c.Limits.MaxSheds == 0 {
		c.Limits.MaxSheds = DefaultRemoteMaxSheds
	}
	if c.MaxRunDuration == 0 {
		c.MaxRunDuration = Duration(DefaultRemoteMaxRunDuration)
	}
	if c.AgentInstall == "" {
		c.AgentInstall = RemoteAgentInstallIncompatible
	}
	if c.SSH.ClientKeyEnv == "" {
		c.SSH.ClientKeyEnv = DefaultRemoteSSHClientKeyEnv
	}
	if c.SSH.ClientKeyFileEnv == "" {
		c.SSH.ClientKeyFileEnv = DefaultRemoteSSHClientKeyFileEnv
	}
	if c.Firewall.Mode == "" {
		c.Firewall.Mode = RemoteFirewallStatic
	}
	if c.Firewall.Mode == RemoteFirewallStatic && len(c.Firewall.SourceRanges) == 0 && c.Firewall.SourceRangesEnv == "" {
		c.Firewall.SourceRangesEnv = DefaultRemoteFirewallSourceRangesEnv
	}
	if c.Provider == RemoteProviderGCE {
		if c.GCE == nil {
			c.GCE = &RemoteGCEConfig{}
		}
		g := c.GCE
		setDefault := func(field *string, def string) {
			if *field == "" {
				*field = def
			}
		}
		setDefault(&g.ProjectEnv, DefaultRemoteGCEProjectEnv)
		setDefault(&g.ZoneEnv, DefaultRemoteGCEZoneEnv)
		setDefault(&g.NetworkEnv, DefaultRemoteGCENetworkEnv)
		setDefault(&g.SubnetEnv, DefaultRemoteGCESubnetEnv)
		setDefault(&g.CredentialsEnv, DefaultRemoteGCECredentialsEnv)
		setDefault(&g.CredentialsFileEnv, DefaultRemoteGCECredentialsFileEnv)
	}
}

// Validate checks a remote block after applyDefaults. It reads nothing from
// the environment: whether the *_env variables are set is Resolve's job, at
// server start-up, so a config loads on a machine that has none of them.
func (c *RemoteConfig) Validate() error {
	switch c.Provider {
	case RemoteProviderGCE:
	case RemoteProviderFake:
		if !c.AllowTestDrivers {
			return fmt.Errorf("provider: %s is a test driver; set allow_test_drivers: true to use it", RemoteProviderFake)
		}
	default:
		return fmt.Errorf("invalid provider: %q (must be %s or %s)", c.Provider, RemoteProviderGCE, RemoteProviderFake)
	}
	switch c.Transport {
	case RemoteTransportSSH:
	case RemoteTransportLocal:
		if !c.AllowTestDrivers {
			return fmt.Errorf("transport: %s is a test driver; set allow_test_drivers: true to use it", RemoteTransportLocal)
		}
	default:
		return fmt.Errorf("invalid transport: %q (must be %s or %s)", c.Transport, RemoteTransportSSH, RemoteTransportLocal)
	}

	if c.Environment == "" {
		return fmt.Errorf("environment is required")
	}
	if !provider.ValidEnv(c.Environment) {
		return fmt.Errorf("invalid environment: %q (1 to 20 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen)", c.Environment)
	}

	for _, d := range []struct{ key, value string }{{"state_dir", c.StateDir}, {"agent_dir", c.AgentDir}} {
		if d.value != "" && !filepath.IsAbs(ExpandPath(d.value)) {
			return fmt.Errorf("%s must be an absolute path or start with ~/ (got %q)", d.key, d.value)
		}
	}

	if c.DefaultImage == "" {
		return fmt.Errorf("default_image is required")
	}
	if err := validateRemoteImageName("default_image", c.DefaultImage); err != nil {
		return err
	}
	for alias, name := range c.ImageAliases {
		if alias == "" || strings.ContainsAny(alias, " \t\n/") {
			return fmt.Errorf("invalid image_aliases name %q", alias)
		}
		if err := validateRemoteImageName("image_aliases."+alias, name); err != nil {
			return err
		}
	}

	if err := c.validateSizes(); err != nil {
		return err
	}
	if c.DiskGB < RemoteMinDiskGB || c.DiskGB > RemoteMaxDiskGB {
		return fmt.Errorf("invalid disk_gb: %d (must be %d to %d)", c.DiskGB, RemoteMinDiskGB, RemoteMaxDiskGB)
	}

	if c.Limits.MaxRunning < 1 {
		return fmt.Errorf("invalid limits.max_running: %d (must be at least 1)", c.Limits.MaxRunning)
	}
	if c.Limits.MaxSheds < 1 {
		return fmt.Errorf("invalid limits.max_sheds: %d (must be at least 1)", c.Limits.MaxSheds)
	}
	if c.Limits.MaxRunning > c.Limits.MaxSheds {
		return fmt.Errorf("limits.max_running (%d) must not exceed limits.max_sheds (%d)", c.Limits.MaxRunning, c.Limits.MaxSheds)
	}
	if d := c.MaxRunDuration.Duration(); d < RemoteMinRunDuration || d > RemoteMaxRunDuration {
		return fmt.Errorf("invalid max_run_duration: %s (must be %s to %s)", d, RemoteMinRunDuration, RemoteMaxRunDuration)
	}

	switch c.AgentInstall {
	case RemoteAgentInstallIncompatible, RemoteAgentInstallAlways, RemoteAgentInstallNever:
	default:
		return fmt.Errorf("invalid agent_install: %q (must be %s, %s or %s)", c.AgentInstall,
			RemoteAgentInstallIncompatible, RemoteAgentInstallAlways, RemoteAgentInstallNever)
	}

	if c.Transport == RemoteTransportSSH {
		if err := validateEnvPair("ssh.client_key_env", c.SSH.ClientKeyEnv, "ssh.client_key_file_env", c.SSH.ClientKeyFileEnv); err != nil {
			return err
		}
	}
	if err := c.validateFirewall(); err != nil {
		return err
	}
	if c.Provider == RemoteProviderGCE {
		if c.GCE == nil {
			return fmt.Errorf("gce block is required with provider: %s", RemoteProviderGCE)
		}
		g := c.GCE
		for _, v := range []struct{ key, name string }{
			{"gce.project_env", g.ProjectEnv}, {"gce.zone_env", g.ZoneEnv},
			{"gce.network_env", g.NetworkEnv}, {"gce.subnet_env", g.SubnetEnv},
		} {
			if !envVarNameRegex.MatchString(v.name) {
				return fmt.Errorf("invalid %s: %q is not an environment variable name", v.key, v.name)
			}
		}
		if err := validateEnvPair("gce.credentials_env", g.CredentialsEnv, "gce.credentials_file_env", g.CredentialsFileEnv); err != nil {
			return err
		}
	}
	return nil
}

func validateRemoteImageName(key, name string) error {
	if name == "" || strings.ContainsAny(name, " \t\n") {
		return fmt.Errorf("invalid %s: %q (a cloud image name, or %s<name> for an image family)", key, name, provider.ImagePrefixFamily)
	}
	return nil
}

func validateEnvPair(key, name, fileKey, fileName string) error {
	if !envVarNameRegex.MatchString(name) {
		return fmt.Errorf("invalid %s: %q is not an environment variable name", key, name)
	}
	if !envVarNameRegex.MatchString(fileName) {
		return fmt.Errorf("invalid %s: %q is not an environment variable name", fileKey, fileName)
	}
	if name == fileName {
		return fmt.Errorf("%s and %s must name different variables (both are %q)", key, fileKey, name)
	}
	return nil
}

func (c *RemoteConfig) validateSizes() error {
	if len(c.Sizes) == 0 {
		return fmt.Errorf("sizes must list at least one size")
	}
	for name, s := range c.Sizes {
		if !remoteSizeNameRegex.MatchString(name) {
			return fmt.Errorf("invalid sizes name %q (lowercase letters, digits and hyphens, starting with a letter)", name)
		}
		if !machineTypeRegex.MatchString(s.MachineType) {
			return fmt.Errorf("invalid sizes.%s.machine_type: %q", name, s.MachineType)
		}
		if s.CPUs < 1 {
			return fmt.Errorf("invalid sizes.%s.cpus: %d (must be at least 1)", name, s.CPUs)
		}
		if s.MemoryMB < 1 {
			return fmt.Errorf("invalid sizes.%s.memory_mb: %d (must be at least 1)", name, s.MemoryMB)
		}
		if s.Arch != provider.ArchAMD64 && s.Arch != provider.ArchARM64 {
			return fmt.Errorf("invalid sizes.%s.arch: %q (must be %s or %s)", name, s.Arch, provider.ArchAMD64, provider.ArchARM64)
		}
	}
	if _, ok := c.Sizes[c.DefaultSize]; !ok {
		return fmt.Errorf("default_size %q is not one of sizes (%s)", c.DefaultSize, strings.Join(c.sizeNames(), ", "))
	}
	return nil
}

func (c *RemoteConfig) validateFirewall() error {
	f := c.Firewall
	// One service is one voice, however many URLs name it: two entries on
	// one host would let whoever runs that host supply the whole agreement
	// auto mode needs before it rewrites the rule.
	echoHosts := make(map[string]bool)
	for _, u := range f.EchoURLs {
		parsed, err := url.Parse(u)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
			return fmt.Errorf("invalid firewall.echo_urls entry %q (must be an https URL)", redact.String(u))
		}
		host := EchoHost(parsed)
		// A host name outside ASCII has a second spelling (its punycode
		// form) that the comparison below would count as another
		// service. Only the ASCII form is accepted, so one service has
		// one name.
		if !isASCII(host) {
			return fmt.Errorf("invalid firewall.echo_urls entry %q: write the host name in its ASCII (punycode) form", redact.String(u))
		}
		if echoHosts[host] {
			return fmt.Errorf("firewall.echo_urls names host %s more than once; each entry must be a different, independently run service", host)
		}
		echoHosts[host] = true
	}
	notStatic := func() error {
		if len(f.SourceRanges) > 0 {
			return fmt.Errorf("firewall.source_ranges is only used with firewall.mode: %s", RemoteFirewallStatic)
		}
		if f.SourceRangesEnv != "" {
			return fmt.Errorf("firewall.source_ranges_env is only used with firewall.mode: %s", RemoteFirewallStatic)
		}
		return nil
	}
	switch f.Mode {
	case RemoteFirewallStatic:
		if len(f.EchoURLs) > 0 {
			return fmt.Errorf("firewall.echo_urls is only used with firewall.mode: %s", RemoteFirewallAuto)
		}
		switch {
		case len(f.SourceRanges) > 0 && f.SourceRangesEnv != "":
			return fmt.Errorf("firewall.source_ranges and firewall.source_ranges_env are both set; set exactly one")
		case len(f.SourceRanges) == 0 && f.SourceRangesEnv == "":
			return fmt.Errorf("firewall.mode: %s needs firewall.source_ranges or firewall.source_ranges_env", RemoteFirewallStatic)
		case f.SourceRangesEnv != "":
			if !envVarNameRegex.MatchString(f.SourceRangesEnv) {
				return fmt.Errorf("invalid firewall.source_ranges_env: %q is not an environment variable name", f.SourceRangesEnv)
			}
		default:
			if _, err := ParseFirewallSources("firewall.source_ranges", f.SourceRanges, f.AcknowledgeAny); err != nil {
				return err
			}
		}
	case RemoteFirewallAuto:
		if len(f.EchoURLs) < 2 {
			return fmt.Errorf("firewall.mode: %s needs at least two firewall.echo_urls", RemoteFirewallAuto)
		}
		if f.AcknowledgeAny {
			return fmt.Errorf("firewall.acknowledge_any is not used with firewall.mode: %s", RemoteFirewallAuto)
		}
		if err := notStatic(); err != nil {
			return err
		}
	case RemoteFirewallAny:
		if !f.AcknowledgeAny {
			return fmt.Errorf("firewall.mode: %s opens every shed's SSH port to every address; set firewall.acknowledge_any: true to accept that", RemoteFirewallAny)
		}
		if len(f.EchoURLs) > 0 {
			return fmt.Errorf("firewall.echo_urls is only used with firewall.mode: %s", RemoteFirewallAuto)
		}
		if err := notStatic(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid firewall.mode: %q (must be %s, %s or %s)", f.Mode, RemoteFirewallStatic, RemoteFirewallAuto, RemoteFirewallAny)
	}
	return nil
}

// EchoHost is the name an echo URL's service is counted under: its host name,
// lower-cased, without the port and without a trailing dot. Two URLs with the
// same EchoHost are one service.
func EchoHost(u *url.URL) string {
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// notPublicIPv4 is the IPv4 space that is never a public source address:
// "this network", the private ranges, carrier-grade NAT, loopback,
// link-local, the IETF protocol assignments, the benchmarking range,
// multicast and the reserved top. A shed's own address is in one of the
// private ranges, so a rule that admits one of these may admit sheds to each
// other. The documentation ranges (192.0.2.0/24, 198.51.100.0/24,
// 203.0.113.0/24) are not listed: nothing routes them, and they are what
// this repository's examples and tests use for "a public address".
var notPublicIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// PublicIPv4 reports whether addr could be a server's public address: an
// IPv4 address outside every range of notPublicIPv4. It is the one
// definition of "public" for the firewall check: what a static source range
// may name without an acknowledgement, and what auto mode may believe from
// an echo service and point the rule at.
func PublicIPv4(addr netip.Addr) bool {
	if !addr.Is4() {
		return false
	}
	for _, np := range notPublicIPv4 {
		if np.Contains(addr) {
			return false
		}
	}
	return true
}

// ParseFirewallSources checks a list of firewall source ranges and returns
// them in canonical form (the network address and its prefix length), sorted
// and without repeats. Each entry must be an IPv4 CIDR. A range wider than a
// /24, 0.0.0.0/0 included, and a range in private or otherwise non-public
// space are refused unless acknowledged. An empty list is
// an error. key names where the list came from, for the message; an entry is
// quoted only as the operator wrote it.
func ParseFirewallSources(key string, entries []string, acknowledged bool) ([]string, error) {
	seen := make(map[netip.Prefix]bool)
	var out []netip.Prefix
	for _, raw := range entries {
		s := strings.TrimSpace(raw)
		prefix, err := netip.ParsePrefix(s)
		if err != nil || !prefix.Addr().Is4() {
			return nil, fmt.Errorf("invalid %s entry %q (must be an IPv4 CIDR such as 203.0.113.7/32)", key, raw)
		}
		prefix = prefix.Masked()
		if prefix.Bits() < RemoteFirewallMinPrefix && !acknowledged {
			return nil, fmt.Errorf("%s entry %q is wider than a /%d; name the server's own address, or set firewall.acknowledge_any: true to accept a range that wide", key, raw, RemoteFirewallMinPrefix)
		}
		if !acknowledged {
			for _, np := range notPublicIPv4 {
				if prefix.Overlaps(np) {
					return nil, fmt.Errorf("%s entry %q is not public address space (it is within %s); sheds have private addresses, so a rule that admits a private range can admit one shed to another's SSH port. Name the server's public address, or set firewall.acknowledge_any: true to accept the range", key, raw, np)
				}
			}
		}
		if !seen[prefix] {
			seen[prefix] = true
			out = append(out, prefix)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists no source range", key)
	}
	sort.Slice(out, func(i, k int) bool {
		if c := out[i].Addr().Compare(out[k].Addr()); c != 0 {
			return c < 0
		}
		return out[i].Bits() < out[k].Bits()
	})
	list := make([]string, len(out))
	for i, p := range out {
		list[i] = p.String()
	}
	return list, nil
}

// resolveFirewallSources returns static mode's expected source ranges: the
// config's own list, or the comma-separated value of the variable
// source_ranges_env names. set is false when the variable is unset or blank.
func (c *RemoteConfig) resolveFirewallSources(getenv func(string) string) (sources []string, set bool, err error) {
	f := c.Firewall
	if len(f.SourceRanges) > 0 {
		sources, err = ParseFirewallSources("remote.firewall.source_ranges", f.SourceRanges, f.AcknowledgeAny)
		return sources, true, err
	}
	value := strings.TrimSpace(getenv(f.SourceRangesEnv))
	if value == "" {
		return nil, false, fmt.Errorf("environment variable %s (remote.firewall.source_ranges_env) is not set; it must hold the firewall rule's source ranges, comma-separated", f.SourceRangesEnv)
	}
	var entries []string
	for _, e := range strings.Split(value, ",") {
		if strings.TrimSpace(e) != "" {
			entries = append(entries, e)
		}
	}
	sources, err = ParseFirewallSources("environment variable "+f.SourceRangesEnv+" (remote.firewall.source_ranges_env)", entries, f.AcknowledgeAny)
	return sources, true, err
}

func (c *RemoteConfig) sizeNames() []string {
	names := make([]string, 0, len(c.Sizes))
	for name := range c.Sizes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SizeList returns remote.sizes as the provider's type, ordered by name.
func (c *RemoteConfig) SizeList() []provider.Size {
	out := make([]provider.Size, 0, len(c.Sizes))
	for _, name := range c.sizeNames() {
		s := c.Sizes[name]
		out = append(out, provider.Size{Name: name, MachineType: s.MachineType, CPUs: s.CPUs, MemoryMB: s.MemoryMB, Arch: s.Arch})
	}
	return out
}

// ResolveSize picks the size for a create request's cpus and memory_mb. Both
// zero gives default_size; otherwise the smallest size that provides at
// least both (a zero in one means "no requirement"). When nothing fits the
// error wraps ErrInvalidShedRequestSentinel (400 INVALID_REQUEST) and names
// the sizes that exist. The shed's record reports the returned size's CPUs
// and MemoryMB, and client caps apply to them.
func (c *RemoteConfig) ResolveSize(cpus, memoryMB int) (provider.Size, error) {
	if cpus < 0 || memoryMB < 0 {
		return provider.Size{}, fmt.Errorf("%w: cpus and memory_mb must not be negative", ErrInvalidShedRequestSentinel)
	}
	size, err := provider.ResolveSize(c.SizeList(), c.DefaultSize, cpus, memoryMB)
	if err != nil {
		return provider.Size{}, fmt.Errorf("%w: %w", ErrInvalidShedRequestSentinel, err)
	}
	return size, nil
}

// ResolveDiskGB turns a create's upper_size_bytes into the boot disk size in
// whole GB, rounding up. Zero gives disk_gb. A size outside RemoteMinDiskGB
// to RemoteMaxDiskGB wraps ErrInvalidShedRequestSentinel.
func (c *RemoteConfig) ResolveDiskGB(upperSizeBytes int64) (int, error) {
	if upperSizeBytes == 0 {
		return c.DiskGB, nil
	}
	const gb = int64(1) << 30
	if upperSizeBytes < 0 || upperSizeBytes > int64(RemoteMaxDiskGB)*gb {
		return 0, fmt.Errorf("%w: disk size must be %d to %d GB on the remote backend", ErrInvalidShedRequestSentinel, RemoteMinDiskGB, RemoteMaxDiskGB)
	}
	n := int((upperSizeBytes + gb - 1) / gb)
	if n < RemoteMinDiskGB {
		return 0, fmt.Errorf("%w: disk size must be %d to %d GB on the remote backend", ErrInvalidShedRequestSentinel, RemoteMinDiskGB, RemoteMaxDiskGB)
	}
	return n, nil
}

// ResolveImage maps a create's image to a cloud image name: empty gives
// default_image, an image_aliases name gives its value. Anything else wraps
// ErrUnknownImageSentinel: a create never names a cloud image directly.
func (c *RemoteConfig) ResolveImage(image string) (string, error) {
	if image == "" {
		return c.DefaultImage, nil
	}
	if name, ok := c.ImageAliases[image]; ok {
		return name, nil
	}
	aliases := make([]string, 0, len(c.ImageAliases))
	for a := range c.ImageAliases {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	return "", fmt.Errorf("%w: %q (image aliases: %s)", ErrUnknownImageSentinel, image, strings.Join(aliases, ", "))
}

// RunLimit is the run limit a shed gets: the larger of max_run_duration and
// the owning service client's max_age (zero for an admin's shed).
func (c *RemoteConfig) RunLimit(clientMaxAge time.Duration) time.Duration {
	return max(c.MaxRunDuration.Duration(), clientMaxAge)
}

// StateDirPath is where the record cache and delete markers live: state_dir
// when set, otherwise "remote" under serverStateDir.
func (c *RemoteConfig) StateDirPath(serverStateDir string) string {
	if c.StateDir != "" {
		return ExpandPath(c.StateDir)
	}
	return filepath.Join(serverStateDir, "remote")
}

// AgentDirPath is where the installable agent builds live: agent_dir when
// set, otherwise "agent" under serverStateDir.
func (c *RemoteConfig) AgentDirPath(serverStateDir string) string {
	if c.AgentDir != "" {
		return ExpandPath(c.AgentDir)
	}
	return filepath.Join(serverStateDir, "agent")
}

// Resolve reads everything the remote backend takes from the environment.
// The server calls it once at start-up, after the config has loaded and
// validated; loading a config never calls it. getenv is os.Getenv when nil.
//
// With provider: gce it needs the project, zone, network and subnet
// variables and exactly one of the credentials pair; with transport: ssh,
// exactly one of the client key pair; with firewall.mode: static and
// firewall.source_ranges_env, that variable. An error names the variable
// (never a secret's value). A test driver needs none of its half.
func (c *RemoteConfig) Resolve(getenv func(string) string) (*RemoteRuntime, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	rt := &RemoteRuntime{}
	if c.Provider == RemoteProviderGCE {
		if c.GCE == nil {
			return nil, fmt.Errorf("remote.gce block is required with provider: %s", RemoteProviderGCE)
		}
		for _, v := range []struct {
			key, name string
			dst       *string
		}{
			{"remote.gce.project_env", c.GCE.ProjectEnv, &rt.Project},
			{"remote.gce.zone_env", c.GCE.ZoneEnv, &rt.Zone},
			{"remote.gce.network_env", c.GCE.NetworkEnv, &rt.Network},
			{"remote.gce.subnet_env", c.GCE.SubnetEnv, &rt.Subnet},
		} {
			value := strings.TrimSpace(getenv(v.name))
			if value == "" {
				return nil, fmt.Errorf("environment variable %s (%s) is not set", v.name, v.key)
			}
			*v.dst = value
		}
		creds, err := resolveEnvPair(getenv, "remote.gce.credentials_env", c.GCE.CredentialsEnv, "remote.gce.credentials_file_env", c.GCE.CredentialsFileEnv)
		if err != nil {
			return nil, err
		}
		rt.Credentials = creds
	}
	if c.Transport == RemoteTransportSSH {
		key, err := resolveEnvPair(getenv, "remote.ssh.client_key_env", c.SSH.ClientKeyEnv, "remote.ssh.client_key_file_env", c.SSH.ClientKeyFileEnv)
		if err != nil {
			return nil, err
		}
		rt.SSHClientKey = key
	}
	if c.Firewall.Mode == RemoteFirewallStatic {
		sources, set, err := c.resolveFirewallSources(getenv)
		// A test provider has no real rule to compare with, so it may be
		// given no ranges; a list that is given must still be valid.
		if err != nil && (set || c.Provider == RemoteProviderGCE) {
			return nil, err
		}
		rt.FirewallSources = sources
	}
	return rt, nil
}

// resolveEnvPair returns the secret a *_env / *_file_env pair names: the
// base64-decoded value of the first variable, or the content of the file the
// second names. Exactly one of the two variables must be set.
func resolveEnvPair(getenv func(string) string, key, name, fileKey, fileName string) ([]byte, error) {
	value := strings.TrimSpace(getenv(name))
	path := strings.TrimSpace(getenv(fileName))
	switch {
	case value == "" && path == "":
		return nil, fmt.Errorf("neither environment variable %s (%s) nor %s (%s) is set; set exactly one", name, key, fileName, fileKey)
	case value != "" && path != "":
		return nil, fmt.Errorf("environment variables %s (%s) and %s (%s) are both set; set exactly one", name, key, fileName, fileKey)
	case value != "":
		// Tolerate the line wrapping `base64` adds by default.
		compact := strings.Join(strings.Fields(value), "")
		data, err := base64.StdEncoding.DecodeString(compact)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(compact)
		}
		if err != nil {
			return nil, fmt.Errorf("environment variable %s (%s) is not valid base64", name, key)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, fmt.Errorf("environment variable %s (%s) decodes to nothing", name, key)
		}
		return data, nil
	default:
		data, err := os.ReadFile(ExpandPath(path))
		if err != nil {
			return nil, fmt.Errorf("reading the file environment variable %s (%s) names: %w", fileName, fileKey, err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, fmt.Errorf("the file environment variable %s (%s) names is empty", fileName, fileKey)
		}
		return data, nil
	}
}

// validateRemote is the remote half of Validate: the block itself, then the
// server-level settings the remote backend cannot honour. Each is refused at
// load so it never reaches a create.
func (c *ServerConfig) validateRemote() error {
	if c.Remote == nil {
		return fmt.Errorf("remote configuration is required when backend is remote")
	}
	if c.Firecracker != nil {
		return fmt.Errorf("a firecracker: block is not allowed with default_backend: %s (one server runs one backend)", BackendRemote)
	}
	if c.VZ != nil {
		return fmt.Errorf("a vz: block is not allowed with default_backend: %s (one server runs one backend)", BackendRemote)
	}
	if err := c.Remote.Validate(); err != nil {
		return fmt.Errorf("remote config: %w", err)
	}
	if c.EnvFile != "" {
		return fmt.Errorf("env_file is not supported by the %s backend (a cloud shed gets no host environment); remove it", BackendRemote)
	}
	if len(c.Mounts) > 0 {
		return fmt.Errorf("mounts are not supported by the %s backend (a cloud shed cannot mount the server's directories); remove the mounts block", BackendRemote)
	}
	if c.Egress != nil && c.Egress.Enabled {
		return fmt.Errorf("egress.enabled is not supported by the %s backend", BackendRemote)
	}
	if c.Network != nil && len(c.Network.AllowCIDRs) > 0 {
		return fmt.Errorf("network.allow_cidrs is not supported by the %s backend (isolation there is by network topology)", BackendRemote)
	}
	if c.Extensions != nil && len(c.Extensions.Enabled) > 0 {
		return fmt.Errorf("extensions.enabled is not supported by the %s backend", BackendRemote)
	}
	if c.Auth != nil {
		for name, cc := range c.Auth.Clients {
			if len(cc.Policy.ServerMounts) > 0 {
				return fmt.Errorf("auth.clients.%s.policy.server_mounts is not supported by the %s backend", name, BackendRemote)
			}
		}
		if len(c.Auth.Clients) > 0 && c.Remote.Firewall.AdmitsAny() {
			if c.Remote.Firewall.Mode == RemoteFirewallAny {
				return fmt.Errorf("remote.firewall.mode: %s is not allowed with auth.clients (one client's shed could reach another's SSH port)", RemoteFirewallAny)
			}
			return fmt.Errorf("remote.firewall.acknowledge_any is not allowed with auth.clients (one client's shed could reach another's SSH port)")
		}
	}
	// A client's images must be remote image aliases. Checked here as well
	// as in Validate so the image-only loader refuses the same configs.
	return c.validateClientImages()
}
