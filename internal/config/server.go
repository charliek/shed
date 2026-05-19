package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/charliek/shed/internal/terminal"
	"github.com/charliek/shed/internal/vmimage"
)

// validCredentialName matches names starting with an alphanumeric character,
// followed by alphanumerics, underscores, or hyphens. Credential names are
// used as VirtioFS mount tags (cred-{name}), so they must not contain spaces,
// commas, or other special characters.
var validCredentialName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ServerConfig represents the server-side configuration.
type ServerConfig struct {
	Name         string                 `yaml:"name"`
	HTTPPort     int                    `yaml:"http_port"`
	SSHPort      int                    `yaml:"ssh_port"`
	DefaultImage string                 `yaml:"default_image"`
	Credentials  map[string]MountConfig `yaml:"credentials"`
	EnvFile      string                 `yaml:"env_file"`
	LogLevel     string                 `yaml:"log_level"`
	Terminal     *terminal.Config       `yaml:"terminal"`

	// DefaultBackend specifies the backend type: "vz", "firecracker", or "detect".
	// When set to "detect", the backend is chosen based on the platform
	// (vz on macOS/arm64, firecracker on linux).
	DefaultBackend string `yaml:"default_backend,omitempty"`

	// Firecracker contains Firecracker-specific configuration
	Firecracker *FirecrackerConfig `yaml:"firecracker,omitempty"`

	// VZ contains Apple Virtualization.framework-specific configuration (macOS only)
	VZ *VZConfig `yaml:"vz,omitempty"`

	// Extensions configures which extensions the agent should enable in VMs.
	Extensions *ExtensionsConfig `yaml:"extensions,omitempty"`

	// Git configures git-related behaviour for in-VM clones, including
	// the SSH known_hosts content seeded before `git clone` runs.
	Git *GitConfig `yaml:"git,omitempty"`

	// Loaded environment variables (not from YAML)
	EnvVars map[string]string `yaml:"-"`
}

// ExtensionsConfig configures which extensions the agent should enable.
type ExtensionsConfig struct {
	// Enabled lists the extension namespaces to activate in VMs
	// (e.g., ["ssh-agent", "aws-credentials"]).
	Enabled []string `yaml:"enabled"`
}

// Validate checks that all extension namespaces are valid and unique.
func (e *ExtensionsConfig) Validate() error {
	seen := make(map[string]bool, len(e.Enabled))
	for _, ns := range e.Enabled {
		if ns == "" {
			return fmt.Errorf("extension namespace must not be empty")
		}
		if seen[ns] {
			return fmt.Errorf("duplicate extension namespace: %q", ns)
		}
		seen[ns] = true
		// Reuse the plugin namespace validation rules (printable ASCII, no spaces,
		// max 128 chars, rejects "system:" prefix).
		if err := validateExtensionNamespace(ns); err != nil {
			return fmt.Errorf("invalid extension namespace %q: %w", ns, err)
		}
	}
	return nil
}

// GitConfig configures git behaviour for in-VM clones.
type GitConfig struct {
	// ExtraKnownHosts contains additional lines to append to the in-VM
	// ~/.ssh/known_hosts before `git clone` runs over SSH. Each entry must be
	// a valid known_hosts line (e.g., "github.com ssh-ed25519 AAAAC3..."),
	// typically obtained by running `ssh-keyscan <host>` on a trusted machine.
	// Built-in defaults (currently GitHub's published host keys) are always
	// included; this list extends them.
	ExtraKnownHosts []string `yaml:"extra_known_hosts,omitempty"`
}

// knownHostsKeyTypes lists the SSH key-type tokens accepted in known_hosts
// lines. Used by GitConfig.Validate to reject obviously-malformed entries
// at server startup rather than silently shipping garbage to the VM.
var knownHostsKeyTypes = map[string]bool{
	"ssh-rsa":             true,
	"ssh-ed25519":         true,
	"ssh-dss":             true,
	"ecdsa-sha2-nistp256": true,
	"ecdsa-sha2-nistp384": true,
	"ecdsa-sha2-nistp521": true,
}

// Validate checks that each extra_known_hosts entry is a syntactically valid
// known_hosts line. The check is deliberately simple — it catches typos and
// obvious garbage but does not try to parse the base64 key material.
func (g *GitConfig) Validate() error {
	for i, line := range g.ExtraKnownHosts {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return fmt.Errorf("extra_known_hosts[%d]: line must not be empty", i)
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 3 {
			return fmt.Errorf("extra_known_hosts[%d]: expected at least 3 fields (host, key-type, base64-key), got %d", i, len(fields))
		}
		if !knownHostsKeyTypes[fields[1]] {
			return fmt.Errorf("extra_known_hosts[%d]: unrecognized key type %q (expected one of ssh-rsa, ssh-ed25519, ssh-dss, ecdsa-sha2-nistp256/384/521)", i, fields[1])
		}
	}
	return nil
}

// validateExtensionNamespace checks that a namespace is valid for extension use.
// Rules: non-empty, printable ASCII only, no spaces, max 128 chars, no "system:" prefix.
func validateExtensionNamespace(namespace string) error {
	if len(namespace) > 128 {
		return fmt.Errorf("must be at most 128 characters")
	}
	if strings.HasPrefix(namespace, "system:") {
		return fmt.Errorf("prefix \"system:\" is reserved for internal use")
	}
	for _, r := range namespace {
		if r == ' ' {
			return fmt.Errorf("must not contain spaces")
		}
		if r < 0x20 || r > 0x7E {
			return fmt.Errorf("must contain only printable ASCII characters")
		}
	}
	return nil
}

// FirecrackerConfig contains Firecracker-specific configuration.
type FirecrackerConfig struct {
	// KernelPath is the path to the Linux kernel image
	KernelPath string `yaml:"kernel_path"`

	// BaseRootfs is the path to the base rootfs image or a Docker image reference.
	// Docker refs are lazily pulled and converted to ext4 on first use.
	BaseRootfs string `yaml:"base_rootfs"`

	// Images maps variant names to rootfs paths or Docker image references.
	// Users can reference these with: shed create mydev --image experimental
	// Values can be ext4 file paths or Docker refs (e.g., "ghcr.io/charliek/shed-fc-default:v1.0.0").
	Images map[string]string `yaml:"images,omitempty"`

	// ImagesDir is the directory for converted/discovered ext4 images.
	// Images matching {name}-rootfs.ext4 are auto-discovered as available variants.
	ImagesDir string `yaml:"images_dir,omitempty"`

	// InstanceDir is the directory for instance data
	InstanceDir string `yaml:"instance_dir"`

	// SnapshotsDir is the directory where shed snapshots are stored.
	SnapshotsDir string `yaml:"snapshots_dir,omitempty"`

	// UppersDir is the directory where per-shed writable upper layers
	// (sparse ext4 files) are stored.
	UppersDir string `yaml:"uppers_dir,omitempty"`

	// UpperSizeDefault is the default logical size of the per-shed
	// writable upper. Accepted units: G (GB) and M (MB). Range 1G-100G.
	UpperSizeDefault string `yaml:"upper_size_default,omitempty"`

	// SocketDir is the directory for Firecracker API sockets
	SocketDir string `yaml:"socket_dir"`

	// DefaultCPUs is the default number of vCPUs for new VMs
	DefaultCPUs int `yaml:"default_cpus"`

	// DefaultMemoryMB is the default memory in MB for new VMs
	DefaultMemoryMB int `yaml:"default_memory_mb"`

	// DefaultDiskGB is the default disk size in GB for new VMs
	DefaultDiskGB int `yaml:"default_disk_gb"`

	// VsockBaseCID is the starting CID for vsock allocation
	VsockBaseCID uint32 `yaml:"vsock_base_cid"`

	// ConsolePort is the vsock port for console/exec connections
	ConsolePort uint32 `yaml:"console_port"`

	// NotifyPort is the vsock port for the message channel (health checks, plugins, credentials)
	NotifyPort uint32 `yaml:"notify_port"`

	// StartTimeout is the timeout for VM startup
	StartTimeout Duration `yaml:"start_timeout"`

	// StopTimeout is the timeout for graceful VM shutdown
	StopTimeout Duration `yaml:"stop_timeout"`

	// BridgeName is the name of the Linux bridge for VM networking
	BridgeName string `yaml:"bridge_name"`

	// BridgeCIDR is the CIDR for the bridge network (e.g., "172.30.0.1/24")
	BridgeCIDR string `yaml:"bridge_cidr"`

	// TAPPrefix is the prefix for TAP device names
	TAPPrefix string `yaml:"tap_prefix"`
}

// VZConfig contains Apple Virtualization.framework-specific configuration.
type VZConfig struct {
	// VfkitPath is the path to the vfkit binary
	VfkitPath string `yaml:"vfkit_path"`

	// KernelPath is the path to the decompressed Linux kernel
	KernelPath string `yaml:"kernel_path"`

	// InitrdPath is the path to the initial RAM disk image
	InitrdPath string `yaml:"initrd_path"`

	// BaseRootfs is the path to the base rootfs image (used when no --image is specified)
	BaseRootfs string `yaml:"base_rootfs"`

	// Images maps variant names to rootfs paths or Docker image references.
	// Users can reference these with: shed create mydev --image experimental
	// Values can be ext4 file paths or Docker refs (e.g., "ghcr.io/charliek/shed-vz-default:v1.0.0").
	Images map[string]string `yaml:"images,omitempty"`

	// ImagesDir is the directory for converted/discovered ext4 images.
	// Images matching {name}-rootfs.ext4 are auto-discovered as available variants.
	ImagesDir string `yaml:"images_dir,omitempty"`

	// InstanceDir is the directory for instance data
	InstanceDir string `yaml:"instance_dir"`

	// SnapshotsDir is the directory where shed snapshots are stored.
	SnapshotsDir string `yaml:"snapshots_dir,omitempty"`

	// UppersDir is the directory where per-shed writable upper layers
	// (sparse ext4 files) are stored.
	UppersDir string `yaml:"uppers_dir,omitempty"`

	// UpperSizeDefault is the default logical size of the per-shed
	// writable upper. Accepted units: G (GB) and M (MB). Range 1G-100G.
	UpperSizeDefault string `yaml:"upper_size_default,omitempty"`

	// SocketDir is the directory for vsock Unix sockets.
	// NOTE: This path must not contain spaces. vfkit URL-encodes socket paths,
	// turning spaces into %20, which causes connection failures.
	SocketDir string `yaml:"socket_dir"`

	// DefaultCPUs is the default number of vCPUs for new VMs
	DefaultCPUs int `yaml:"default_cpus"`

	// DefaultMemoryMB is the default memory in MB for new VMs
	DefaultMemoryMB int `yaml:"default_memory_mb"`

	// DefaultDiskGB is the default disk size in GB for new VMs
	DefaultDiskGB int `yaml:"default_disk_gb"`

	// ConsolePort is the vsock port for console/exec connections
	ConsolePort uint32 `yaml:"console_port"`

	// NotifyPort is the vsock port for the message channel (health checks, plugins, credentials)
	NotifyPort uint32 `yaml:"notify_port"`

	// TCPProxyPort is the vsock port for the TCP proxy (used by DialService to reach VM services)
	TCPProxyPort uint32 `yaml:"tcp_proxy_port"`

	// StartTimeout is the timeout for VM startup
	StartTimeout Duration `yaml:"start_timeout"`

	// StopTimeout is the timeout for graceful VM shutdown
	StopTimeout Duration `yaml:"stop_timeout"`
}

// GetImages implements vmimage.ImageConfig.
func (c *VZConfig) GetImages() map[string]string { return c.Images }

// GetImagesDir implements vmimage.ImageConfig.
func (c *VZConfig) GetImagesDir() string { return c.ImagesDir }

// GetBaseRootfs implements vmimage.ImageConfig.
func (c *VZConfig) GetBaseRootfs() string { return c.BaseRootfs }

// GetPlatform implements vmimage.ImageConfig.
func (c *VZConfig) GetPlatform() string { return "linux/arm64" }

// GetExtractKernel implements vmimage.ImageConfig.
func (c *VZConfig) GetExtractKernel() bool { return true }

// GetNeedsInitrd implements vmimage.ImageConfig.
func (c *VZConfig) GetNeedsInitrd() bool { return true }

// DefaultVZConfig returns a VZConfig with default values.
func DefaultVZConfig() *VZConfig {
	return &VZConfig{
		VfkitPath: "vfkit",
		// KernelPath / InitrdPath / BaseRootfs are intentionally left
		// empty by default. Phase A retired the flat-file layout (e.g.
		// {ImagesDir}/default-rootfs.ext4) in favor of the content-
		// addressed blob store at {ImagesDir}/blobs/sha256/<digest>/
		// with tag indirection at {ImagesDir}/tags/<tag>.json. vm.Start
		// reads the kernel from the blob; ResolveBaseRootfs is only
		// consulted when `shed create` runs without --image, in which
		// case the operator must set BaseRootfs explicitly.
		ImagesDir:        ExpandPath(DefaultVZImagesDir),
		InstanceDir:      ExpandPath(DefaultVZImagesDir + "/instances"),
		SnapshotsDir:     ExpandPath(DefaultVZImagesDir + "/snapshots"),
		UppersDir:        ExpandPath(DefaultVZImagesDir + "/uppers"),
		UpperSizeDefault: DefaultUpperSize,
		SocketDir:        ExpandPath("~/.shed/vz/sockets"),
		DefaultCPUs:      2,
		DefaultMemoryMB:  4096,
		DefaultDiskGB:    20,
		ConsolePort:      1024,
		NotifyPort:       1026,
		TCPProxyPort:     1028,
		StartTimeout:     Duration(60 * time.Second),
		StopTimeout:      Duration(10 * time.Second),
	}
}

// applyDefaults fills in zero-valued fields with defaults and expands ~ in paths.
func (c *VZConfig) applyDefaults() {
	if c.NotifyPort == 0 {
		c.NotifyPort = 1026
	}
	if c.TCPProxyPort == 0 {
		c.TCPProxyPort = 1028
	}

	// Expand ~ in paths
	c.KernelPath = ExpandPath(c.KernelPath)
	c.InitrdPath = ExpandPath(c.InitrdPath)
	c.BaseRootfs = ExpandPath(c.BaseRootfs)
	c.ImagesDir = ExpandPath(c.ImagesDir)
	c.InstanceDir = ExpandPath(c.InstanceDir)
	c.SnapshotsDir = ExpandPath(c.SnapshotsDir)
	c.SocketDir = ExpandPath(c.SocketDir)

	// Default ImagesDir if not set
	if c.ImagesDir == "" {
		c.ImagesDir = ExpandPath(DefaultVZImagesDir)
	}

	// Default SnapshotsDir to ImagesDir/snapshots if unset
	if c.SnapshotsDir == "" {
		c.SnapshotsDir = filepath.Join(c.ImagesDir, "snapshots")
	}

	// Default UppersDir to ImagesDir/uppers if unset.
	c.UppersDir = ExpandPath(c.UppersDir)
	if c.UppersDir == "" {
		c.UppersDir = filepath.Join(c.ImagesDir, "uppers")
	}

	if c.UpperSizeDefault == "" {
		c.UpperSizeDefault = DefaultUpperSize
	}

	// Expand ~ in image paths (only for filesystem paths, not Docker refs)
	for name, val := range c.Images {
		if !vmimage.IsDockerRef(val) {
			c.Images[name] = ExpandPath(val)
		}
	}
}

// Validate checks that the VZ configuration is valid.
func (c *VZConfig) Validate() error {
	if c.VfkitPath == "" {
		return fmt.Errorf("vfkit_path is required")
	}
	// kernel_path is optional under Phase B: vm.Start prefers the
	// kernel inside the blob (blobs/<digest>/kernel, written by
	// `shed image install --kernel ...`), so the legacy fallback path
	// is only consulted when a blob arrives without a kernel.
	// base_rootfs is similarly optional — see below.
	if c.InstanceDir == "" {
		return fmt.Errorf("instance_dir is required")
	}
	if c.SnapshotsDir == "" {
		return fmt.Errorf("snapshots_dir is required")
	}
	if c.SocketDir == "" {
		return fmt.Errorf("socket_dir is required")
	}

	// Validate CPU and memory bounds
	if c.DefaultCPUs < 1 {
		return fmt.Errorf("vz: default_cpus must be at least 1")
	}
	if c.DefaultCPUs > MaxVZCPUs {
		return fmt.Errorf("vz: default_cpus must be at most %d", MaxVZCPUs)
	}
	if c.DefaultMemoryMB < 128 {
		return fmt.Errorf("vz: default_memory_mb must be at least 128")
	}
	if c.DefaultMemoryMB > MaxVZMemoryMB {
		return fmt.Errorf("vz: default_memory_mb must be at most %d", MaxVZMemoryMB)
	}
	if c.DefaultDiskGB < 1 {
		return fmt.Errorf("vz: default_disk_gb must be at least 1")
	}
	if c.DefaultDiskGB > MaxVZDiskGB {
		return fmt.Errorf("vz: default_disk_gb must be at most %d", MaxVZDiskGB)
	}

	// Validate vsock ports
	if c.ConsolePort == 0 {
		return fmt.Errorf("vz: console_port must be set")
	}
	if c.ConsolePort > MaxVsockPort {
		return fmt.Errorf("vz: console_port must be at most %d", MaxVsockPort)
	}
	if c.NotifyPort == 0 {
		return fmt.Errorf("vz: notify_port must be set")
	}
	if c.NotifyPort > MaxVsockPort {
		return fmt.Errorf("vz: notify_port must be at most %d", MaxVsockPort)
	}
	if c.TCPProxyPort == 0 {
		return fmt.Errorf("vz: tcp_proxy_port must be set")
	}
	if c.TCPProxyPort > MaxVsockPort {
		return fmt.Errorf("vz: tcp_proxy_port must be at most %d", MaxVsockPort)
	}
	if c.ConsolePort == c.NotifyPort || c.ConsolePort == c.TCPProxyPort || c.NotifyPort == c.TCPProxyPort {
		return fmt.Errorf("vz: console_port, notify_port, and tcp_proxy_port must all be different")
	}

	// Validate timeouts
	startTimeout := time.Duration(c.StartTimeout)
	if startTimeout != 0 {
		if startTimeout < MinTimeout {
			return fmt.Errorf("vz: start_timeout must be at least %s", MinTimeout)
		}
		if startTimeout > MaxTimeout {
			return fmt.Errorf("vz: start_timeout must be at most %s", MaxTimeout)
		}
	}
	stopTimeout := time.Duration(c.StopTimeout)
	if stopTimeout != 0 {
		if stopTimeout < MinTimeout {
			return fmt.Errorf("vz: stop_timeout must be at least %s", MinTimeout)
		}
		if stopTimeout > MaxTimeout {
			return fmt.Errorf("vz: stop_timeout must be at most %s", MaxTimeout)
		}
	}

	if c.UpperSizeDefault != "" {
		if _, err := ParseUpperSize(c.UpperSizeDefault); err != nil {
			return fmt.Errorf("vz: upper_size_default: %w", err)
		}
	}

	// Defer kernel/initrd/rootfs path validation when every configured
	// source is a Docker ref (files are extracted during first image
	// conversion) or when kernel_path is empty (Phase B: vm.Start
	// reads the kernel from the blob). The previous hasAnyDockerRef
	// gate was too loose — a mix of local + remote sources still
	// needs the legacy kernel/initrd for the local-spawn path.
	if c.KernelPath != "" && !allSourcesAreDockerRefs(c.BaseRootfs, c.Images) {
		if _, err := os.Stat(c.KernelPath); err != nil {
			return fmt.Errorf("vz: kernel_path does not exist: %s", c.KernelPath)
		}
		if c.InitrdPath != "" {
			if _, err := os.Stat(c.InitrdPath); err != nil {
				return fmt.Errorf("vz: initrd_path does not exist: %s", c.InitrdPath)
			}
		}
	}

	// base_rootfs path-existence check only applies when configured as
	// a local path. An empty value is allowed (see Validate header).
	if c.BaseRootfs != "" && !vmimage.IsDockerRef(c.BaseRootfs) {
		if _, err := os.Stat(c.BaseRootfs); err != nil {
			return fmt.Errorf("vz: base_rootfs does not exist: %s", c.BaseRootfs)
		}
	}

	// Validate image variant paths exist (skip Docker refs)
	for name, val := range c.Images {
		if !vmimage.IsDockerRef(val) {
			if _, err := os.Stat(val); err != nil {
				return fmt.Errorf("vz: image %q path does not exist: %s", name, val)
			}
		}
	}

	return nil
}

// ResolvedImage represents the result of resolving an image name.
// Either Path (ext4 already exists locally) or DockerRef (needs pull + conversion) is set.
type ResolvedImage struct {
	// Path is set when the ext4 image already exists on disk.
	Path string

	// DockerRef is set when the image needs to be pulled and converted.
	DockerRef string

	// Name is the variant name, used for caching (e.g., "default" → "default-rootfs.ext4").
	Name string

	// Digest is set when Path came from a tag in the content-addressed
	// blob store. Empty for the legacy hardcoded-path escape hatch
	// (where the caller can't tell us a digest). Carries the digest
	// forward so EnsureImage doesn't have to re-do tag lookup — that
	// second lookup was both awkward and racey (tag could advance
	// between resolve and ensure).
	Digest string
}

// resolveImage is the shared implementation for image name resolution.
// configKey is used in error messages (e.g., "vz.images" or "firecracker.images").
//
// Resolution order:
//  1. Look up in images map — if value is a Docker ref, check the
//     blob-store tag cache first
//  2. Auto-discover existing tag in {imagesDir}/tags/
//  3. Absolute path escape hatch
//  4. Error with available variants
func resolveImage(images map[string]string, imagesDir, image, configKey string) (ResolvedImage, error) {
	if val, ok := images[image]; ok {
		if vmimage.IsDockerRef(val) {
			// ResolveTag returns the digest too — carry it through
			// so EnsureImage doesn't have to re-do tag lookup
			// (which races against concurrent `shed image pull`).
			digest, cached, err := vmimage.ResolveTag(imagesDir, image)
			if err == nil {
				manifest, mErr := vmimage.LoadManifestByDigest(imagesDir, digest)
				if mErr != nil && errors.Is(mErr, vmimage.ErrLegacyBundledBlob) {
					return ResolvedImage{}, mErr
				}
				if mErr == nil && manifest.ShedSourceRef() == val {
					return ResolvedImage{Path: cached, Name: image, Digest: digest}, nil
				}
			} else if errors.Is(err, vmimage.ErrLegacyBundledBlob) {
				return ResolvedImage{}, err
			}
			return ResolvedImage{DockerRef: val, Name: image}, nil
		}
		return ResolvedImage{Path: val, Name: image}, nil
	}

	// Auto-discover by tag in the blob store.
	if imagesDir != "" {
		digest, discovered, err := vmimage.ResolveTag(imagesDir, image)
		if err == nil {
			return ResolvedImage{Path: discovered, Name: image, Digest: digest}, nil
		}
		if errors.Is(err, vmimage.ErrLegacyBundledBlob) {
			return ResolvedImage{}, err
		}
	}

	// Absolute path escape hatch
	expanded := ExpandPath(image)
	if filepath.IsAbs(expanded) {
		if _, err := os.Stat(expanded); err != nil {
			if os.IsNotExist(err) {
				return ResolvedImage{}, fmt.Errorf("%w: image path does not exist: %q", ErrUnknownImageSentinel, expanded)
			}
			return ResolvedImage{}, fmt.Errorf("failed to stat image path %q: %w", expanded, err)
		}
		return ResolvedImage{Path: expanded, Name: image}, nil
	}

	// Not found — build error with available variants
	available := availableImageVariants(images, imagesDir)
	if len(available) > 0 {
		return ResolvedImage{}, fmt.Errorf("%w %q; available variants: %s", ErrUnknownImageSentinel, image, strings.Join(available, ", "))
	}
	return ResolvedImage{}, fmt.Errorf("%w %q; no image variants configured (set %s in server config)", ErrUnknownImageSentinel, image, configKey)
}

// resolveBaseRootfs is the shared implementation for base rootfs resolution.
func resolveBaseRootfs(baseRootfs, imagesDir string) ResolvedImage {
	if vmimage.IsDockerRef(baseRootfs) {
		if cached := vmimage.Resolve(imagesDir, "_base", baseRootfs); cached != "" {
			return ResolvedImage{Path: cached, Name: "_base"}
		}
		return ResolvedImage{DockerRef: baseRootfs, Name: "_base"}
	}
	return ResolvedImage{Path: baseRootfs, Name: "_base"}
}

// availableImageVariants returns a sorted list of image names known to
// the system: every key in the Images config map plus every tag present
// in the blob store. The synthetic "_base" tag is excluded.
func availableImageVariants(images map[string]string, imagesDir string) []string {
	seen := make(map[string]bool)
	for name := range images {
		seen[name] = true
	}
	if imagesDir != "" {
		tags, err := vmimage.ListTags(imagesDir)
		if err == nil {
			for _, name := range tags {
				if name != "" && name != "_base" {
					seen[name] = true
				}
			}
		}
	}
	available := make([]string, 0, len(seen))
	for name := range seen {
		available = append(available, name)
	}
	sort.Strings(available)
	return available
}

// ResolveImage resolves an image name to a local ext4 path or Docker reference.
func (c *VZConfig) ResolveImage(image string) (ResolvedImage, error) {
	return resolveImage(c.Images, c.ImagesDir, image, "vz.images")
}

// ResolveBaseRootfs resolves the base rootfs (used when no --image flag is specified).
func (c *VZConfig) ResolveBaseRootfs() ResolvedImage {
	return resolveBaseRootfs(c.BaseRootfs, c.ImagesDir)
}

// Firecracker validation upper bounds.
const (
	MaxFirecrackerCPUs            = 32
	MaxFirecrackerMemoryMB        = 256 * 1024 // 256 GB
	MaxFirecrackerDiskGB          = 1024       // 1 TB
	MaxVsockCID            uint32 = 65535
	MaxVsockPort           uint32 = 65535
	MinTimeout                    = 1 * time.Second
	MaxTimeout                    = 30 * time.Minute
)

// DefaultVZImagesDir is the default directory for VZ rootfs images.
const DefaultVZImagesDir = "~/Library/Application Support/shed/vz"

// VZ validation upper bounds (decoupled from Firecracker).
const (
	MaxVZCPUs     = 32
	MaxVZMemoryMB = 256 * 1024 // 256 GB
	MaxVZDiskGB   = 1024       // 1 TB
)

// Duration is a wrapper around time.Duration for YAML marshaling
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler for Duration
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	duration, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(duration)
	return nil
}

// MarshalYAML implements yaml.Marshaler for Duration
func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

// Duration returns the time.Duration value
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// DefaultFirecrackerImagesDir is the default directory for Firecracker rootfs images.
// This is a subdirectory of /var/lib/shed/firecracker/ to avoid mixing image files
// with the kernel, instance directories, and sockets that already live there.
const DefaultFirecrackerImagesDir = "/var/lib/shed/firecracker/images"

// GetImages implements vmimage.ImageConfig.
func (c *FirecrackerConfig) GetImages() map[string]string { return c.Images }

// GetImagesDir implements vmimage.ImageConfig.
func (c *FirecrackerConfig) GetImagesDir() string { return c.ImagesDir }

// GetBaseRootfs implements vmimage.ImageConfig.
func (c *FirecrackerConfig) GetBaseRootfs() string { return c.BaseRootfs }

// GetPlatform implements vmimage.ImageConfig.
func (c *FirecrackerConfig) GetPlatform() string { return "linux/amd64" }

// GetExtractKernel implements vmimage.ImageConfig.
func (c *FirecrackerConfig) GetExtractKernel() bool { return true }

// GetNeedsInitrd implements vmimage.ImageConfig.
//
// Both VZ and Firecracker boot through the shed-overlay initramfs
// (it owns overlayfs assembly + pivot_root), so an initrd blob must
// be installed alongside every shed image regardless of backend.
func (c *FirecrackerConfig) GetNeedsInitrd() bool { return true }

// ResolveImage resolves an image name to a local ext4 path or Docker reference.
func (c *FirecrackerConfig) ResolveImage(image string) (ResolvedImage, error) {
	return resolveImage(c.Images, c.ImagesDir, image, "firecracker.images")
}

// ResolveBaseRootfs resolves the base rootfs (used when no --image flag is specified).
func (c *FirecrackerConfig) ResolveBaseRootfs() ResolvedImage {
	return resolveBaseRootfs(c.BaseRootfs, c.ImagesDir)
}

// DefaultFirecrackerConfig returns a FirecrackerConfig with default values.
func DefaultFirecrackerConfig() *FirecrackerConfig {
	return &FirecrackerConfig{
		// KernelPath and BaseRootfs are intentionally left empty by
		// default — Phase A retired the flat-file layout (the previous
		// /var/lib/shed/firecracker/base-rootfs.ext4 default) in favor
		// of the content-addressed blob store under
		// {ImagesDir}/blobs/sha256/<digest>/, and Phase B made the
		// in-blob kernel the canonical source. Operators who want the
		// legacy fallbacks set them explicitly in server.yaml.
		ImagesDir:        DefaultFirecrackerImagesDir,
		InstanceDir:      "/var/lib/shed/firecracker/instances",
		SnapshotsDir:     "/var/lib/shed/firecracker/snapshots",
		UppersDir:        "/var/lib/shed/firecracker/uppers",
		UpperSizeDefault: DefaultUpperSize,
		SocketDir:        "/var/run/shed/firecracker",
		DefaultCPUs:      2,
		DefaultMemoryMB:  4096,
		DefaultDiskGB:    20,
		VsockBaseCID:     100,
		ConsolePort:      1024,
		NotifyPort:       1026,
		StartTimeout:     Duration(30 * time.Second),
		StopTimeout:      Duration(10 * time.Second),
		BridgeName:       "shed-br0",
		BridgeCIDR:       "172.30.0.1/24",
		TAPPrefix:        "shed-tap",
	}
}

// MountConfig represents a bind mount configuration.
type MountConfig struct {
	Source   string   `yaml:"source"`
	Target   string   `yaml:"target"`
	ReadOnly bool     `yaml:"readonly"`
	Exclude  []string `yaml:"exclude,omitempty"`
}

// MatchesExclude reports whether the given relative path matches any of the
// mount's exclude patterns. Patterns use filepath.Match glob syntax.
func (m MountConfig) MatchesExclude(relPath string) bool {
	return MatchesExcludePatterns(relPath, m.Exclude)
}

// MatchesExcludePatterns reports whether relPath matches any of the given glob
// patterns. Patterns like "dir/*" also match the directory itself and deeply
// nested paths (e.g., "dir/sub/deep/file").
func MatchesExcludePatterns(relPath string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, relPath); matched {
			return true
		}
		if dir, ok := strings.CutSuffix(pattern, "/*"); ok {
			if relPath == dir || strings.HasPrefix(relPath, dir+"/") {
				return true
			}
		}
	}
	return false
}

// DefaultServerConfig returns a ServerConfig with default values.
func DefaultServerConfig() *ServerConfig {
	return &ServerConfig{
		Name:           "shed-server",
		HTTPPort:       8080,
		SSHPort:        2222,
		Credentials:    make(map[string]MountConfig),
		LogLevel:       "info",
		Terminal:       terminal.DefaultConfig(),
		EnvVars:        make(map[string]string),
		DefaultBackend: BackendDetect,
	}
}

// LoadServerConfig loads server configuration from standard locations.
// It checks in order: ./server.yaml, ~/.config/shed/server.yaml, /etc/shed/server.yaml
func LoadServerConfig() (*ServerConfig, error) {
	return LoadServerConfigFromPath("")
}

// LoadServerConfigFromPath loads server configuration from a specific path.
// If path is empty, it searches standard locations.
func LoadServerConfigFromPath(path string) (*ServerConfig, error) {
	cfg := DefaultServerConfig()

	var configPath string
	if path != "" {
		configPath = ExpandPath(path)
	} else {
		// Search standard locations
		locations := []string{
			"./server.yaml",
			ExpandPath("~/.config/shed/server.yaml"),
			"/etc/shed/server.yaml",
		}

		for _, loc := range locations {
			if _, err := os.Stat(loc); err == nil {
				configPath = loc
				break
			}
		}

		if configPath == "" {
			return cfg, nil // Return defaults if no config found
		}
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", configPath, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", configPath, err)
	}

	// Apply defaults for zero values
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = 8080
	}
	if cfg.SSHPort == 0 {
		cfg.SSHPort = 2222
	}
	// DefaultImage is unused for VM backends (they use BaseRootfs),
	// but keep the field for potential future use.
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.Terminal == nil {
		cfg.Terminal = terminal.DefaultConfig()
	}

	cfg.normalizeBackends()

	// Expand and validate paths in credentials
	for name, mount := range cfg.Credentials {
		// Credential names must be safe for use as VirtioFS mount tags
		if !validCredentialName.MatchString(name) {
			return nil, fmt.Errorf("credential name %q must contain only alphanumeric characters, underscores, and hyphens", name)
		}

		source := filepath.Clean(ExpandPath(mount.Source))
		target := filepath.Clean(mount.Target)

		// Source must be an absolute path
		if !filepath.IsAbs(source) {
			return nil, fmt.Errorf("credential %q source must be an absolute path: %s", name, mount.Source)
		}

		// Target must be an absolute path
		if !filepath.IsAbs(target) {
			return nil, fmt.Errorf("credential %q target must be an absolute path: %s", name, mount.Target)
		}

		// Source paths with commas break vfkit VirtioFS device arguments
		if strings.Contains(source, ",") {
			return nil, fmt.Errorf("credential %q source path must not contain commas: %s", name, source)
		}

		// Credential sources must be directories. Single-file credentials are
		// no longer supported — use shed sync for individual files instead.
		// Missing sources are OK (skipped at runtime with a warning).
		if info, err := os.Stat(source); err == nil && !info.IsDir() {
			return nil, fmt.Errorf(
				"credential %q source %s is a file, not a directory; "+
					"only directory credentials are supported — "+
					"use 'shed sync' for single-file configs like .gitconfig",
				name, source,
			)
		}

		mount.Source = source
		mount.Target = target
		cfg.Credentials[name] = mount
	}

	// Load environment file if specified
	if cfg.EnvFile != "" {
		envPath := ExpandPath(cfg.EnvFile)
		envVars, err := loadEnvFile(envPath)
		if err != nil {
			// Log warning but don't fail if env file is missing
			fmt.Fprintf(os.Stderr, "Warning: failed to load env file %s: %v\n", envPath, err)
		} else {
			cfg.EnvVars = envVars
		}
	}

	if cfg.Firecracker != nil {
		cfg.Firecracker.applyDefaults()
	}

	if cfg.VZ != nil {
		cfg.VZ.applyDefaults()
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// LoadServerConfigForCLI loads server config the same way as
// LoadServerConfigFromPath but skips the host/backend OS coupling
// validation. Used by CLI commands like `shed image push --local`
// and `shed image build` that read the OCI store via the backend's
// config block but never actually start a VM. For example, the
// publish-images.yaml workflow runs on a Linux runner, builds VZ
// images via `--target shed-vz-*`, and pushes them via `shed image
// push --local -c <config-with-vz-block>`. The strict validator
// rejects that combination ("vz backend is only supported on
// macOS") even though we're never going to boot a VM there.
//
// CALLERS THAT START A VM (or accept arbitrary HTTP traffic that
// will start a VM) MUST use LoadServerConfigFromPath instead — the
// OS coupling check matters for them.
func LoadServerConfigForCLI(path string) (*ServerConfig, error) {
	cfg, err := loadServerConfigForCLI(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.ValidateNoHostCoupling(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadServerConfigForCLI replicates the YAML-read + defaults path of
// LoadServerConfigFromPath without invoking the strict Validate at
// the end. Kept private; callers go through LoadServerConfigForCLI
// (which adds ValidateNoHostCoupling).
func loadServerConfigForCLI(path string) (*ServerConfig, error) {
	cfg := DefaultServerConfig()

	var configPath string
	if path != "" {
		configPath = ExpandPath(path)
	} else {
		locations := []string{
			"./server.yaml",
			ExpandPath("~/.config/shed/server.yaml"),
			"/etc/shed/server.yaml",
		}
		for _, loc := range locations {
			if _, err := os.Stat(loc); err == nil {
				configPath = loc
				break
			}
		}
		if configPath == "" {
			return cfg, nil
		}
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", configPath, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", configPath, err)
	}

	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = 8080
	}
	if cfg.SSHPort == 0 {
		cfg.SSHPort = 2222
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.Terminal == nil {
		cfg.Terminal = terminal.DefaultConfig()
	}

	cfg.normalizeBackends()

	if cfg.Firecracker != nil {
		cfg.Firecracker.applyDefaults()
	}
	if cfg.VZ != nil {
		cfg.VZ.applyDefaults()
	}

	return cfg, nil
}

// ValidateNoHostCoupling runs the parts of Validate that aren't tied to
// the host OS/arch. Used by CLI commands that only need to read the
// OCI store (image build, image push --local), where the strict
// "vz backend only on macOS" / "firecracker backend only on linux"
// checks would block cross-platform image operations from CI runners
// or developers cross-publishing images. Callers that actually start
// a VM MUST call Validate instead.
func (c *ServerConfig) ValidateNoHostCoupling() error {
	if c.Name == "" {
		return fmt.Errorf("server name is required")
	}
	if c.HTTPPort < 0 || c.HTTPPort > 65535 {
		return fmt.Errorf("invalid http_port: %d", c.HTTPPort)
	}
	if c.SSHPort < 0 || c.SSHPort > 65535 {
		return fmt.Errorf("invalid ssh_port: %d", c.SSHPort)
	}

	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[c.LogLevel] {
		return fmt.Errorf("invalid log_level: %s (must be debug, info, warn, or error)", c.LogLevel)
	}

	if c.DefaultBackend == "" {
		return fmt.Errorf("default_backend is required")
	}

	if !isValidBackend(c.DefaultBackend) {
		return fmt.Errorf("invalid default_backend: %s (must be firecracker, vz, or detect)", c.DefaultBackend)
	}

	// Resolve detect to the actual backend (no OS coupling check below).
	if c.DefaultBackend == BackendDetect {
		resolved, err := ResolveBackend(BackendDetect, runtime.GOOS, runtime.GOARCH)
		if err != nil {
			return fmt.Errorf("default_backend: %w", err)
		}
		c.DefaultBackend = resolved
	}

	// Per-backend config block presence is still validated (you can't
	// point at a vz: store if the block is absent), but the full per-
	// backend Validate is skipped — it asserts things like 'firecracker
	// requires this kernel path' that don't apply to image-only flows.
	switch c.DefaultBackend {
	case BackendFirecracker:
		if c.Firecracker == nil {
			return fmt.Errorf("firecracker configuration is required when backend is firecracker")
		}
	case BackendVZ:
		if c.VZ == nil {
			return fmt.Errorf("vz configuration is required when backend is vz")
		}
	}
	return nil
}

// Validate checks that the configuration is valid.
func (c *ServerConfig) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("server name is required")
	}
	if c.HTTPPort < 1 || c.HTTPPort > 65535 {
		return fmt.Errorf("invalid http_port: %d", c.HTTPPort)
	}
	if c.SSHPort < 1 || c.SSHPort > 65535 {
		return fmt.Errorf("invalid ssh_port: %d", c.SSHPort)
	}

	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[c.LogLevel] {
		return fmt.Errorf("invalid log_level: %s (must be debug, info, warn, or error)", c.LogLevel)
	}

	if c.DefaultBackend == "" {
		return fmt.Errorf("default_backend is required")
	}

	if !isValidBackend(c.DefaultBackend) {
		return fmt.Errorf("invalid default_backend: %s (must be firecracker, vz, or detect)", c.DefaultBackend)
	}

	// Resolve detect to the actual backend
	if c.DefaultBackend == BackendDetect {
		resolved, err := ResolveBackend(BackendDetect, runtime.GOOS, runtime.GOARCH)
		if err != nil {
			return fmt.Errorf("default_backend: %w", err)
		}
		c.DefaultBackend = resolved
	}

	if runtime.GOOS != "linux" && c.DefaultBackend == BackendFirecracker {
		return fmt.Errorf("firecracker backend is only supported on linux")
	}

	if runtime.GOOS != "darwin" && c.DefaultBackend == BackendVZ {
		return fmt.Errorf("vz backend is only supported on macOS")
	}
	if runtime.GOARCH != "arm64" && c.DefaultBackend == BackendVZ {
		return fmt.Errorf("vz backend currently supports macOS Apple Silicon (arm64) only")
	}

	// Validate Firecracker config if enabled
	if c.DefaultBackend == BackendFirecracker {
		if c.Firecracker == nil {
			return fmt.Errorf("firecracker configuration is required when backend is firecracker")
		}
		if err := c.Firecracker.Validate(); err != nil {
			return fmt.Errorf("firecracker config: %w", err)
		}
	}

	// Validate VZ config if enabled
	if c.DefaultBackend == BackendVZ {
		if c.VZ == nil {
			return fmt.Errorf("vz configuration is required when backend is vz")
		}
		if err := c.VZ.Validate(); err != nil {
			return fmt.Errorf("vz config: %w", err)
		}
	}

	// Validate extension config if present
	if c.Extensions != nil {
		if err := c.Extensions.Validate(); err != nil {
			return fmt.Errorf("extensions config: %w", err)
		}
	}

	// Validate git config if present
	if c.Git != nil {
		if err := c.Git.Validate(); err != nil {
			return fmt.Errorf("git config: %w", err)
		}
	}

	return nil
}

func isValidBackend(backend string) bool {
	return backend == BackendFirecracker || backend == BackendVZ || backend == BackendDetect
}

// ResolveBackend resolves a backend string to a concrete backend type.
// When backend is "detect", it selects based on the platform:
// darwin/arm64 → vz, linux → firecracker.
func ResolveBackend(backend, goos, goarch string) (string, error) {
	if backend != BackendDetect {
		return backend, nil
	}
	switch {
	case goos == "darwin" && goarch == "arm64":
		return BackendVZ, nil
	case goos == "linux":
		return BackendFirecracker, nil
	default:
		return "", fmt.Errorf("cannot auto-detect backend for %s/%s: set default_backend explicitly to 'vz' or 'firecracker'", goos, goarch)
	}
}

func (c *ServerConfig) normalizeBackends() {
	if c.DefaultBackend == "" {
		c.DefaultBackend = BackendDetect
	}
}

// applyDefaults fills in zero-valued fields with defaults so existing configs
// without newer fields (e.g. notify_port) continue to work.
func (c *FirecrackerConfig) applyDefaults() {
	if c.NotifyPort == 0 {
		c.NotifyPort = 1026
	}

	// Default ImagesDir if not set
	if c.ImagesDir == "" {
		c.ImagesDir = DefaultFirecrackerImagesDir
	}

	// Default SnapshotsDir if not set (sibling of InstanceDir)
	if c.SnapshotsDir == "" {
		c.SnapshotsDir = "/var/lib/shed/firecracker/snapshots"
	}

	c.UppersDir = ExpandPath(c.UppersDir)
	if c.UppersDir == "" {
		c.UppersDir = "/var/lib/shed/firecracker/uppers"
	}

	if c.UpperSizeDefault == "" {
		c.UpperSizeDefault = DefaultUpperSize
	}

	// Default KernelPath to ImagesDir/vmlinux (extracted from published images)
	if c.KernelPath == "" {
		c.KernelPath = c.ImagesDir + "/vmlinux"
	}

	// Expand ~ in image paths (only for filesystem paths, not Docker refs)
	for name, val := range c.Images {
		if !vmimage.IsDockerRef(val) {
			c.Images[name] = ExpandPath(val)
		}
	}
}

// Validate checks that the Firecracker configuration is valid.
func (c *FirecrackerConfig) Validate() error {
	// kernel_path and base_rootfs are both optional under the Phase B
	// content-addressed model: vm.Start reads the kernel from the
	// blob, and ResolveBaseRootfs is only called when `shed create`
	// runs without --image (handled separately by CreateShed). The
	// legacy path-based fields remain as fallbacks; an empty value
	// means "rely on the blob."
	if c.InstanceDir == "" {
		return fmt.Errorf("instance_dir is required")
	}
	if c.SnapshotsDir == "" {
		return fmt.Errorf("snapshots_dir is required")
	}
	if c.SocketDir == "" {
		return fmt.Errorf("socket_dir is required")
	}

	// Validate CPU and memory bounds
	if c.DefaultCPUs < 1 {
		return fmt.Errorf("default_cpus must be at least 1")
	}
	if c.DefaultCPUs > MaxFirecrackerCPUs {
		return fmt.Errorf("default_cpus must be at most %d", MaxFirecrackerCPUs)
	}
	if c.DefaultMemoryMB < 128 {
		return fmt.Errorf("default_memory_mb must be at least 128")
	}
	if c.DefaultMemoryMB > MaxFirecrackerMemoryMB {
		return fmt.Errorf("default_memory_mb must be at most %d", MaxFirecrackerMemoryMB)
	}
	if c.DefaultDiskGB < 1 {
		return fmt.Errorf("default_disk_gb must be at least 1")
	}
	if c.DefaultDiskGB > MaxFirecrackerDiskGB {
		return fmt.Errorf("default_disk_gb must be at most %d", MaxFirecrackerDiskGB)
	}

	// Validate vsock ports
	if c.ConsolePort == 0 {
		return fmt.Errorf("console_port must be set")
	}
	if c.ConsolePort > MaxVsockPort {
		return fmt.Errorf("console_port must be at most %d", MaxVsockPort)
	}
	if c.NotifyPort == 0 {
		return fmt.Errorf("notify_port must be set")
	}
	if c.NotifyPort > MaxVsockPort {
		return fmt.Errorf("notify_port must be at most %d", MaxVsockPort)
	}
	if c.ConsolePort == c.NotifyPort {
		return fmt.Errorf("console_port and notify_port must be different")
	}

	if c.VsockBaseCID < 3 {
		return fmt.Errorf("vsock_base_cid must be at least 3 (0-2 are reserved)")
	}
	if c.VsockBaseCID > MaxVsockCID {
		return fmt.Errorf("vsock_base_cid must be at most %d", MaxVsockCID)
	}

	// Validate timeouts
	startTimeout := time.Duration(c.StartTimeout)
	if startTimeout != 0 {
		if startTimeout < MinTimeout {
			return fmt.Errorf("start_timeout must be at least %s", MinTimeout)
		}
		if startTimeout > MaxTimeout {
			return fmt.Errorf("start_timeout must be at most %s", MaxTimeout)
		}
	}
	stopTimeout := time.Duration(c.StopTimeout)
	if stopTimeout != 0 {
		if stopTimeout < MinTimeout {
			return fmt.Errorf("stop_timeout must be at least %s", MinTimeout)
		}
		if stopTimeout > MaxTimeout {
			return fmt.Errorf("stop_timeout must be at most %s", MaxTimeout)
		}
	}

	// Defer kernel/rootfs path validation when every configured source
	// is a Docker ref (files are extracted during first image
	// conversion) or when kernel_path is empty (Phase B: vm.Start
	// reads the kernel from the blob). The previous hasAnyDockerRef
	// gate was too loose — a mix of local + remote sources still
	// needs the legacy kernel/initrd for the local-spawn path.
	if c.KernelPath != "" && !allSourcesAreDockerRefs(c.BaseRootfs, c.Images) {
		if _, err := os.Stat(c.KernelPath); err != nil {
			return fmt.Errorf("kernel_path does not exist: %s", c.KernelPath)
		}
	}

	// Validate base rootfs path exists when configured as a local
	// path. An empty value is allowed (see Validate header).
	if c.BaseRootfs != "" && !vmimage.IsDockerRef(c.BaseRootfs) {
		if _, err := os.Stat(c.BaseRootfs); err != nil {
			return fmt.Errorf("base_rootfs does not exist: %s", c.BaseRootfs)
		}
	}

	// Validate image variant paths exist (skip Docker refs)
	for name, val := range c.Images {
		if !vmimage.IsDockerRef(val) {
			if _, err := os.Stat(val); err != nil {
				return fmt.Errorf("image %q path does not exist: %s", name, val)
			}
		}
	}

	// Validate network configuration
	if c.BridgeName == "" {
		return fmt.Errorf("bridge_name is required")
	}
	if c.BridgeCIDR == "" {
		return fmt.Errorf("bridge_cidr is required")
	}

	// Validate CIDR format
	_, _, err := net.ParseCIDR(c.BridgeCIDR)
	if err != nil {
		return fmt.Errorf("invalid bridge_cidr %q: %w", c.BridgeCIDR, err)
	}

	if c.TAPPrefix == "" {
		return fmt.Errorf("tap_prefix is required")
	}

	if c.UpperSizeDefault != "" {
		if _, err := ParseUpperSize(c.UpperSizeDefault); err != nil {
			return fmt.Errorf("firecracker: upper_size_default: %w", err)
		}
	}

	return nil
}

// UpperSize bounds. The plan caps configurable upper size at
// 1G-100G; values outside this range almost always indicate a config
// typo and should fail-fast at validation time.
const (
	MinUpperSizeBytes int64 = 1 * 1024 * 1024 * 1024
	MaxUpperSizeBytes int64 = 100 * 1024 * 1024 * 1024
)

// DefaultUpperSize is the fallback upper-layer size when the config
// omits upper_size_default. Plan-aligned at 5 GB sparse — a working
// guess for typical "build a project" workloads.
const DefaultUpperSize = "5G"

// ParseUpperSize accepts a human-friendly suffix (G, M, or bare bytes)
// and returns the size in bytes. Validates the range against
// MinUpperSizeBytes/MaxUpperSizeBytes.
//
// Pre-checks that n*mul won't overflow int64 — otherwise a value like
// "10000000000G" would silently wrap to a tiny positive number and
// slip past the range bounds.
func ParseUpperSize(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("upper size is empty")
	}
	mul := int64(1)
	digits := s
	switch unit := s[len(s)-1]; unit {
	case 'G', 'g':
		mul = 1024 * 1024 * 1024
		digits = s[:len(s)-1]
	case 'M', 'm':
		mul = 1024 * 1024
		digits = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid upper size %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("upper size must be positive: %q", s)
	}
	// Reject inputs that would overflow before the bound checks see
	// them. Anything > MaxUpperSizeBytes/mul is already out of range,
	// so we can fail with the same range error.
	if n > MaxUpperSizeBytes/mul {
		return 0, fmt.Errorf("upper size must be at most %dG, got %q", MaxUpperSizeBytes/(1024*1024*1024), s)
	}
	bytes := n * mul
	if bytes < MinUpperSizeBytes {
		return 0, fmt.Errorf("upper size must be at least %dG, got %q", MinUpperSizeBytes/(1024*1024*1024), s)
	}
	if bytes > MaxUpperSizeBytes {
		return 0, fmt.Errorf("upper size must be at most %dG, got %q", MaxUpperSizeBytes/(1024*1024*1024), s)
	}
	return bytes, nil
}

// ExpandPath expands ~ to the user's home directory.
func ExpandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// loadEnvFile loads environment variables from a file.
// Each line should be in the format KEY=value.
func loadEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	envVars := make(map[string]string)
	lines := strings.Split(string(data), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split on first =
		idx := strings.Index(line, "=")
		if idx == -1 {
			continue
		}

		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])

		// Remove quotes if present
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		envVars[key] = value
	}

	return envVars, nil
}

// allSourcesAreDockerRefs returns true when the kernel/initrd validation
// can be safely skipped: either base_rootfs is empty (the optional-Phase-B
// case) or it's a Docker ref, AND every entry in images is either a Docker
// ref or empty. A single local-path source means the legacy kernel/initrd
// files must still exist to support spawning from it.
func allSourcesAreDockerRefs(baseRootfs string, images map[string]string) bool {
	if baseRootfs != "" && !vmimage.IsDockerRef(baseRootfs) {
		return false
	}
	for _, val := range images {
		if val != "" && !vmimage.IsDockerRef(val) {
			return false
		}
	}
	return true
}
