// Package config provides configuration types and loading for shed.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
)

// Sentinel errors for shed and session operations.
var (
	// ErrShedNotFoundSentinel is returned when a shed does not exist.
	ErrShedNotFoundSentinel = errors.New("shed not found")

	// ErrShedAlreadyExistsSentinel is returned when creating a shed that already exists.
	ErrShedAlreadyExistsSentinel = errors.New("shed already exists")

	// ErrShedAlreadyRunningSentinel is returned when starting a shed that is already running.
	ErrShedAlreadyRunningSentinel = errors.New("shed is already running")

	// ErrSessionNotFoundSentinel is returned when a tmux session does not exist.
	ErrSessionNotFoundSentinel = errors.New("session not found")

	// ErrTmuxNotAvailableSentinel is returned when tmux is not installed in the container.
	ErrTmuxNotAvailableSentinel = errors.New("tmux is not available in this container")

	// ErrShedNotRunningSentinel is returned when an operation requires a running shed.
	ErrShedNotRunningSentinel = errors.New("shed is not running")

	// ErrShedNotStoppedSentinel is returned when an operation requires a
	// stopped shed (e.g. shed reset, shed snapshot create).
	ErrShedNotStoppedSentinel = errors.New("shed must be stopped first")

	// ErrUnknownImageSentinel is returned when a requested image variant does not exist.
	ErrUnknownImageSentinel = errors.New("unknown image")

	// ErrImageNotFoundSentinel is returned when a cached image does not exist.
	ErrImageNotFoundSentinel = errors.New("image not found")

	// ErrImageInUseSentinel is returned when trying to delete an image referenced by config.
	ErrImageInUseSentinel = errors.New("image is referenced by config")

	// ErrNotSupportedSentinel is returned when an operation is not supported by a backend.
	ErrNotSupportedSentinel = errors.New("not supported by this backend")

	// ErrSnapshotNotFoundSentinel is returned when a snapshot does not exist.
	ErrSnapshotNotFoundSentinel = errors.New("snapshot not found")

	// ErrSnapshotAlreadyExistsSentinel is returned when creating a snapshot that already exists.
	ErrSnapshotAlreadyExistsSentinel = errors.New("snapshot already exists")

	// ErrSnapshotSourceRunningSentinel is returned when snapshotting a running shed.
	ErrSnapshotSourceRunningSentinel = errors.New("source shed is running; stop it before snapshotting")

	// ErrSnapshotBackendMismatchSentinel is returned when spawning a snapshot on the wrong backend.
	ErrSnapshotBackendMismatchSentinel = errors.New("snapshot backend does not match target")

	// ErrInvalidShedRequestSentinel is the catch-all sentinel for CreateShedRequest
	// field-level validation that maps to HTTP 400 INVALID_REQUEST. Add new
	// field-conflict cases (e.g., --from-snapshot combined with --image or --repo)
	// under this sentinel rather than minting per-conflict sentinels.
	ErrInvalidShedRequestSentinel = errors.New("invalid create-shed request")

	// ErrStopIncompleteSentinel is returned when StopShed asked the VMM to
	// terminate but the recorded PID is still alive after the stop
	// sequence (vm.Stop returned without error). Surfacing this prevents
	// the metadata from advertising status=stopped while a zombie VMM
	// still holds the workspace upper / vsock sockets.
	ErrStopIncompleteSentinel = errors.New("VMM did not exit after stop sequence")

	// ErrZombiePresentSentinel is returned when StartShed sees a non-zero
	// PID in metadata whose process is still alive AND looks like the
	// expected VMM binary (vfkit / firecracker). This protects against
	// silently spawning a second VMM under the same name when metadata
	// got out of sync (partial save / external tampering / server crash
	// between vm.Start and metadata.Save).
	ErrZombiePresentSentinel = errors.New("recorded VMM pid is still alive; refusing to spawn a second instance")

	// ErrNetworkProfileUnsupportedSentinel is returned when a backend cannot
	// provide the requested network profile (VZ offers only "open").
	ErrNetworkProfileUnsupportedSentinel = errors.New("network profile is not supported by this backend")

	// ErrNetworkEnforcementUnavailableSentinel is returned when shed-server
	// cannot install the firewall rules a non-open network profile needs, so
	// a create or start of such a shed is refused rather than run unguarded.
	ErrNetworkEnforcementUnavailableSentinel = errors.New("network enforcement is unavailable")
)

// shedNameRegex validates shed names: lowercase alphanumeric and hyphens, starting with a letter.
var shedNameRegex = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$|^[a-z]$`)

// sessionNameRegex validates session names: alphanumeric, underscores, and hyphens.
var sessionNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// MaxShedNameLength is the maximum allowed length for a shed name.
const MaxShedNameLength = 63

// ValidateShedName validates that a shed name is valid.
// Names must be lowercase alphanumeric with hyphens allowed (not at start/end),
// must start with a letter, and must be at most 63 characters.
func ValidateShedName(name string) error {
	if name == "" {
		return fmt.Errorf("shed name cannot be empty")
	}

	if len(name) > MaxShedNameLength {
		return fmt.Errorf("shed name cannot exceed %d characters", MaxShedNameLength)
	}

	if !shedNameRegex.MatchString(name) {
		return fmt.Errorf("shed name must be lowercase alphanumeric with hyphens (not at start/end), starting with a letter")
	}

	return nil
}

// ValidateSnapshotName validates that a snapshot name is valid.
// Snapshot names follow the same rules as shed names.
func ValidateSnapshotName(name string) error {
	if name == "" {
		return fmt.Errorf("snapshot name cannot be empty")
	}

	if len(name) > MaxShedNameLength {
		return fmt.Errorf("snapshot name cannot exceed %d characters", MaxShedNameLength)
	}

	if !shedNameRegex.MatchString(name) {
		return fmt.Errorf("snapshot name must be lowercase alphanumeric with hyphens (not at start/end), starting with a letter")
	}

	return nil
}

// Shed represents a development environment container.
type Shed struct {
	Name        string    `json:"name" yaml:"name"`
	Status      string    `json:"status" yaml:"status"`
	CreatedAt   time.Time `json:"created_at" yaml:"created_at"`
	Repo        string    `json:"repo,omitempty" yaml:"repo,omitempty"`
	ContainerID string    `json:"container_id" yaml:"container_id"`
	Backend     string    `json:"backend,omitempty" yaml:"backend,omitempty"`
	IPAddress   string    `json:"ip_address,omitempty" yaml:"ip_address,omitempty"`
	CPUs        int       `json:"cpus,omitempty" yaml:"cpus,omitempty"`
	MemoryMB    int       `json:"memory_mb,omitempty" yaml:"memory_mb,omitempty"`
	PID         int       `json:"pid,omitempty" yaml:"pid,omitempty"`
	RootfsPath  string    `json:"rootfs_path,omitempty" yaml:"rootfs_path,omitempty"`
	// ProjectMounts are host directories mounted under the home directory
	// (--local-dir / --add-dir), each at /home/shed/<basename>.
	ProjectMounts []MountConfig `json:"project_mounts,omitempty" yaml:"project_mounts,omitempty"`
	// LandingDir is the directory interactive logins land in (a project
	// subdirectory of the home directory, or the home directory itself).
	LandingDir  string `json:"landing_dir,omitempty" yaml:"landing_dir,omitempty"`
	Image       string `json:"image,omitempty" yaml:"image,omitempty"`
	ImageDigest string `json:"image_digest,omitempty" yaml:"image_digest,omitempty"` // pinned manifest digest (sha256:...)
	// FromSnapshot records the snapshot name this shed was spawned from (immediate parent only).
	FromSnapshot string                         `json:"from_snapshot,omitempty" yaml:"from_snapshot,omitempty"`
	LastHealthy  *time.Time                     `json:"last_healthy,omitempty" yaml:"last_healthy,omitempty"` // last heartbeat from agent (VM backends only)
	StartedAt    *time.Time                     `json:"started_at,omitempty" yaml:"started_at,omitempty"`     // agent boot time from heartbeat (VM backends only)
	Extensions   map[string]ExtensionHealthInfo `json:"extensions,omitempty" yaml:"extensions,omitempty"`     // per-extension health (VM backends only)
	// Egress* track Level-1 egress-control state (set when egress is enabled
	// and the shed is assigned a non-empty profile list). EgressPort is the
	// per-shed proxy listener port — stable across stop/start because the
	// guest's injected HTTP_PROXY is baked into the persistent upper.
	EgressProfiles []string `json:"egress_profiles,omitempty" yaml:"egress_profiles,omitempty"`
	EgressPort     int      `json:"egress_port,omitempty" yaml:"egress_port,omitempty"`
	// EgressToken is the per-shed proxy-auth token binding the port to this
	// shed. It is a secret: never serialized to API/CLI responses (json:"-").
	// It lives durably in the per-backend on-disk metadata, not here.
	EgressToken string `json:"-" yaml:"-"`
	// Owner is the service client (auth.clients) that created this shed.
	// Empty for a shed an admin created, and for every shed created before
	// service clients existed: those are admin-only.
	Owner string `json:"owner,omitempty" yaml:"owner,omitempty"`
	// Mounts is the server-mount selection this shed starts with (top-level
	// `mounts:`). A shed created before mount selection existed reports
	// {"mode":"all"}.
	Mounts *MountSelection `json:"mounts,omitempty" yaml:"mounts,omitempty"`
	// NetworkProfile is the shed's network profile ("open" or "isolated"),
	// fixed at create. A shed created before network profiles existed
	// reports "open".
	NetworkProfile string `json:"network_profile,omitempty" yaml:"network_profile,omitempty"`
}

// Network profiles (CreateShedRequest.Network, Shed.NetworkProfile,
// network.default_profile, auth.clients.<name>.policy.network).
const (
	// NetworkProfileOpen is today's network: the guest reaches whatever the
	// host's NAT reaches. The default.
	NetworkProfileOpen = "open"
	// NetworkProfileIsolated gives the guest the internet and nothing else:
	// not the host, other guests, private, CGNAT/tailnet, link-local or
	// multicast ranges, and no IPv6. Firecracker only.
	NetworkProfileIsolated = "isolated"
)

// ValidNetworkProfile reports whether p is a known network profile name.
func ValidNetworkProfile(p string) bool {
	return p == NetworkProfileOpen || p == NetworkProfileIsolated
}

// EffectiveNetworkProfile is the profile a shed with the stored value p runs
// with: empty (metadata written before network profiles existed) is "open".
func EffectiveNetworkProfile(p string) string {
	if p == "" {
		return NetworkProfileOpen
	}
	return p
}

// Mount selection modes (MountSelection.Mode).
const (
	// MountModeAll mounts every configured server mount (the default, and
	// what a shed created before mount selection existed gets).
	MountModeAll = "all"
	// MountModeSelected mounts only the named server mounts; no names means
	// none.
	MountModeSelected = "selected"
)

// MountSelection is a shed's stored choice of server mounts. The mode, not
// the slice, carries the meaning, so a "selected" selection with no names can
// never reload as "every mount".
type MountSelection struct {
	Mode  string   `json:"mode" yaml:"mode"`
	Names []string `json:"names,omitempty" yaml:"names,omitempty"`
}

// Allows reports whether the server mount called name is part of the
// selection. A nil selection (legacy metadata) and mode "all" allow every
// mount; "selected" allows only Names; any other mode allows nothing, so a
// corrupted or future mode fails closed.
func (m *MountSelection) Allows(name string) bool {
	if m == nil {
		return true
	}
	switch m.Mode {
	case MountModeAll:
		return true
	case MountModeSelected:
		for _, n := range m.Names {
			if n == name {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// AllMounts returns a fresh {mode: all} selection.
func AllMounts() *MountSelection { return &MountSelection{Mode: MountModeAll} }

// SelectedMounts returns a {mode: selected} selection of a copy of names
// (nil or empty: no mounts).
func SelectedMounts(names []string) *MountSelection {
	return &MountSelection{Mode: MountModeSelected, Names: append([]string(nil), names...)}
}

// EffectiveMountSelection is the selection a shed with the stored value sel
// runs with: a missing selection (metadata written before mount selection
// existed) is {mode: all}. It returns a copy, never sel itself.
func EffectiveMountSelection(sel *MountSelection) *MountSelection {
	if sel == nil {
		return AllMounts()
	}
	return &MountSelection{Mode: sel.Mode, Names: append([]string(nil), sel.Names...)}
}

// ExtensionHealthInfo is the API-facing extension health for a shed.
type ExtensionHealthInfo struct {
	Guest string `json:"guest"` // "running", "stopped", "failed"
	Host  string `json:"host"`  // "connected", "unreachable", "unknown"
}

// Shed status constants.
const (
	StatusRunning  = "running"
	StatusStopped  = "stopped"
	StatusStarting = "starting"
	StatusError    = "error"
)

// Session represents a tmux session within a shed container.
type Session struct {
	Name        string    `json:"name"`
	ShedName    string    `json:"shed_name"`
	ServerName  string    `json:"server_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	Attached    bool      `json:"attached"`
	WindowCount int       `json:"window_count,omitempty"`
}

// Session constants.
const (
	// DefaultSessionName is the name used when no session is specified.
	DefaultSessionName = "default"

	// MaxSessionNameLength is the maximum allowed length for a session name.
	MaxSessionNameLength = 63
)

// ValidateSessionName validates that a session name is valid.
// Names must be alphanumeric with underscores and hyphens allowed,
// must start with an alphanumeric character, and must be at most 63 characters.
func ValidateSessionName(name string) error {
	if name == "" {
		return fmt.Errorf("session name cannot be empty")
	}

	if len(name) > MaxSessionNameLength {
		return fmt.Errorf("session name cannot exceed %d characters", MaxSessionNameLength)
	}

	if !sessionNameRegex.MatchString(name) {
		return fmt.Errorf("session name must be alphanumeric with underscores and hyphens (not at start), starting with alphanumeric")
	}

	return nil
}

// ServerInfo is returned by GET /api/info.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	SSHPort int    `json:"ssh_port"`
	// HTTPPort is the plain-HTTP port (open mode). Omitted in token mode, which
	// serves no plain HTTP — clients use the HTTPS endpoint there.
	HTTPPort int    `json:"http_port,omitempty"`
	Backend  string `json:"backend"`
	// AuthMode is the server's auth.mode, so `shed server add` knows whether
	// to bootstrap an HTTP token (or, in mtls mode, a client certificate)
	// over SSH. WIRE CONTRACT: on /api/info token mode travels as the legacy
	// "secure" spelling — released clients gate their bootstrap on that exact
	// string (LegacyWireAuthMode) — and the decode boundary normalizes it back
	// to "token" (NormalizeAuthMode, applied in the CLI's GetInfo), so
	// in-process consumers only ever see "open", "token", or "mtls".
	// Reported on the unauthenticated /api/info in open/token mode — the mode
	// is observable from behavior anyway.
	AuthMode string `json:"auth_mode,omitempty"`
	// DefaultImage is the resolved default_image for the active backend
	// (after ${shed.version} expansion / version synthesis at load). Exposed
	// so clients can see which image a `shed create` without --image will
	// use — useful when the ref is synthesized and never written in config.
	DefaultImage string `json:"default_image,omitempty"`

	// HTTPSPort is the pinned-TLS listener port in token mode (auth.mode:
	// token), so a client adding a token-mode server can learn the TLS
	// endpoint. 0/omitted in open mode (no HTTPS listener).
	HTTPSPort int `json:"https_port,omitempty"`

	// CAFingerprint is the "sha256:<hex>" pin of the internal CA that signs
	// client certificates, reported only in mtls mode. It is operator
	// visibility, not a trust anchor: a client never needs it (it authenticates
	// the SERVER by the tls_cert_fingerprint pin and proves itself with the cert
	// the SSH bootstrap handed it), but an operator comparing two servers, or
	// confirming a CA rotation landed, does. Omitted in open/token mode.
	CAFingerprint string `json:"ca_fingerprint,omitempty"`
	// CANotAfter is the client CA's expiry (RFC 3339), reported only in mtls
	// mode. Rotating the CA invalidates every issued client certificate
	// fleet-wide and must be scheduled, so the deadline is worth surfacing
	// somewhere a human or a monitor can read it. Omitted in open/token mode.
	//
	// A string rather than time.Time so the field can be omitted when unset —
	// encoding/json's omitempty does not apply to struct values.
	CANotAfter string `json:"ca_not_after,omitempty"`

	// Features advertises server capability tokens (e.g. "overview") for
	// endpoint discovery, so a client learns which endpoints and behaviors
	// this server supports without probing each one. The same set
	// is mirrored in the GET /api/overview server block. The token list is owned
	// by internal/api (serverFeatures); older clients decode it as an empty slice.
	Features []string `json:"features,omitempty"`
}

// FeatureMountSelection is the /api/info feature token for the create
// request's "mounts" field. internal/api advertises it (api.FeatureMountSelection
// is this constant); the CLI refuses to send --mounts to a server without it,
// since an older server would ignore the field and mount everything.
const FeatureMountSelection = "mount_selection"

// FeatureNetworkProfiles is the /api/info feature token for the create
// request's "network" field (advertised by a Firecracker server). The CLI
// refuses to send --network isolated to a server without it, since an older
// server would ignore the field and create an open shed.
const FeatureNetworkProfiles = "network_profiles"

// HasFeature reports whether the server advertises the feature token f.
func (i *ServerInfo) HasFeature(f string) bool {
	if i == nil {
		return false
	}
	return slices.Contains(i.Features, f)
}

// SSHHostKeyResponse is returned by GET /api/ssh-host-key.
type SSHHostKeyResponse struct {
	HostKey string `json:"host_key"`
}

// ShedsResponse is returned by GET /api/sheds.
type ShedsResponse struct {
	Sheds []Shed `json:"sheds"`
}

// SessionsResponse is returned by GET /api/sheds/{name}/sessions and GET /api/sessions.
type SessionsResponse struct {
	Sessions []Session `json:"sessions"`
	Warnings []string  `json:"warnings,omitempty"`
}

// ImageInfo describes an available image variant.
//
// Storage model is content-addressed: Digest pins the underlying blob,
// Tag is the optional human-readable name (matches Name for tagged
// images, empty for dangling blobs).
type ImageInfo struct {
	Name      string `json:"name"`
	Path      string `json:"path,omitempty"`
	DockerRef string `json:"docker_ref,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Source    string `json:"source"` // "config", "user", or "dangling"
	Cached    bool   `json:"cached"`
	Digest    string `json:"digest,omitempty"` // "sha256:..." digest of the blob
	Tag       string `json:"tag,omitempty"`    // tag name pointing at this blob
	InUse     bool   `json:"in_use,omitempty"` // protected by a shed/snapshot reference
	// Alias is the friendly image_aliases key (e.g. "base"), set only for
	// config-sourced images; empty for user-pulled or dangling blobs.
	Alias string `json:"alias,omitempty"`
	// IsDefault is true for the config image whose ref is default_image.
	IsDefault bool `json:"is_default,omitempty"`
	// BootOnly is true when the image was pulled without layer tarballs
	// (boots fine; `shed image push` needs a `--with-layers` re-pull first).
	BootOnly bool `json:"boot_only,omitempty"`
}

// ImageInspectResponse is returned by GET /api/images/{tag-or-digest}.
type ImageInspectResponse struct {
	Image    ImageInfo     `json:"image"`
	Manifest ImageManifest `json:"manifest"`
}

// ImageManifest mirrors vmimage.OCIManifest for the wire format.
// Shape parallels the OCI image manifest spec; foreign tools (crane,
// oras, skopeo) inspecting shed's store see this same JSON on disk
// at blobs/sha256/<manifest-digest>.
type ImageManifest struct {
	Digest        string            `json:"digest"`
	SchemaVersion int               `json:"schema_version"`
	MediaType     string            `json:"media_type,omitempty"`
	Config        ImageDescriptor   `json:"config"`
	Layers        []ImageDescriptor `json:"layers,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
	// Convenience fields lifted from annotations for backwards
	// compatibility with consumers that read SourceRef directly.
	SourceRef         string `json:"source_ref,omitempty"`
	Variant           string `json:"variant,omitempty"`
	KernelDigest      string `json:"kernel_digest,omitempty"`
	InitrdDigest      string `json:"initrd_digest,omitempty"`
	RootfsLogicalSize int64  `json:"rootfs_logical_size,omitempty"`
}

// ImageDescriptor is the wire-format counterpart of vmimage.Descriptor.
type ImageDescriptor struct {
	MediaType   string            `json:"media_type"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ImageTagRequest is the body of POST /api/images/tag.
type ImageTagRequest struct {
	Source string `json:"source"` // tag name or digest
	Target string `json:"target"` // new tag name
}

// ImagePullRequest is the body of POST /api/images/pull.
type ImagePullRequest struct {
	DockerRef string `json:"docker_ref"`
	Tag       string `json:"tag"`
	// Platform is an optional override (e.g. "linux/arm64"). Empty
	// means the server-side backend's native platform.
	Platform string `json:"platform,omitempty"`
	// WithLayers pulls the full image (layer tarballs included). Default
	// (false) pulls boot-only — config + kernel + initrd + erofs only —
	// which the host boots from without the layers. Set true (the CLI's
	// --with-layers) when the image will be re-pushed.
	WithLayers bool `json:"with_layers,omitempty"`
}

// ImagePushRequest is the body of POST /api/images/push.
type ImagePushRequest struct {
	// Source is the local tag or digest to push.
	Source string `json:"source"`
	// Destination is the registry reference (e.g. "ghcr.io/org/repo:v1").
	Destination string `json:"destination"`
}

// ImagePushResponse is the response of POST /api/images/push.
type ImagePushResponse struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// ImagePullResponse is the response of POST /api/images/pull.
type ImagePullResponse struct {
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
}

// ImagesResponse is returned by GET /api/images.
type ImagesResponse struct {
	Images []ImageInfo `json:"images"`
}

// PruneImagesResponse is returned by POST /api/images/prune.
type PruneImagesResponse struct {
	Deleted []ImageInfo `json:"deleted"`
}

// DiskSize captures both apparent (logical) and allocated (physical) bytes.
// PhysicalBytes comes from stat.Blocks * 512. On APFS and other reflink-capable
// filesystems, a file's st_blocks counts cloned-but-unmodified extents against
// every referencing file, so summing PhysicalBytes across files that share
// extents (via clonefile, FICLONE, or hardlinks) overcounts the actual on-disk
// usage. This is accepted for v1 and surfaced in DiskUsage.Notes.
type DiskSize struct {
	LogicalBytes  int64 `json:"logical_bytes"`
	PhysicalBytes int64 `json:"physical_bytes"`
}

// FileEntry describes a single file with its size and classification.
type FileEntry struct {
	Path string   `json:"path"`
	Size DiskSize `json:"size"`
	// Kind is one of: "rootfs" | "console_log" | "kernel" | "initrd" |
	// "lock" | "tmp" | "source" | "metadata" | "snapshot_orphan".
	Kind string `json:"kind,omitempty"`
}

// ImageDiskEntry is the df view of a cached image variant, carrying both
// logical and physical bytes. Kept separate from ImageInfo so /api/images
// wire format stays stable.
type ImageDiskEntry struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	DockerRef string   `json:"docker_ref,omitempty"`
	Size      DiskSize `json:"size"`
}

// ShedDiskEntry describes one shed's per-instance disk footprint.
type ShedDiskEntry struct {
	Name       string      `json:"name"`
	Status     string      `json:"status"`
	Image      string      `json:"image,omitempty"`
	Rootfs     FileEntry   `json:"rootfs"`
	ConsoleLog *FileEntry  `json:"console_log,omitempty"` // nil for Firecracker
	OtherFiles []FileEntry `json:"other_files,omitempty"`
	Total      DiskSize    `json:"total"`
}

// DiskUsageTotals aggregates bytes across df sections.
type DiskUsageTotals struct {
	Images    DiskSize `json:"images"` // includes kernel + initrd
	Sheds     DiskSize `json:"sheds"`
	Snapshots DiskSize `json:"snapshots"`
	Orphans   DiskSize `json:"orphans"`
	All       DiskSize `json:"all"`
}

// DiskUsage is the payload returned by GET /api/system/df.
type DiskUsage struct {
	ServerName  string    `json:"server_name"`
	Backend     string    `json:"backend"` // "vz" | "firecracker" | "none"
	GeneratedAt time.Time `json:"generated_at"`

	Images []ImageDiskEntry `json:"images"`
	Kernel *FileEntry       `json:"kernel,omitempty"`
	Initrd *FileEntry       `json:"initrd,omitempty"` // VZ only

	Sheds     []ShedDiskEntry     `json:"sheds"`
	Snapshots []SnapshotDiskEntry `json:"snapshots,omitempty"`
	Orphans   []FileEntry         `json:"orphans"`

	Totals DiskUsageTotals `json:"totals"`

	// Notes carries advisory caveats (APFS overcount, hardlink double-count, etc.).
	Notes []string `json:"notes,omitempty"`
}

// DiskUsageOrError is one entry in a multi-server SystemDFResponse.
// Exactly one of Usage or Error is populated.
type DiskUsageOrError struct {
	ServerName string     `json:"server_name"`
	Usage      *DiskUsage `json:"usage,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// SystemDFResponse is the client-side aggregation of per-server df results
// produced by `shed system df --all`. Never returned by the API directly.
type SystemDFResponse struct {
	Servers []DiskUsageOrError `json:"servers"`
}

// PrunedItem describes one file or object removed (or proposed for removal)
// by `shed system prune`.
type PrunedItem struct {
	// Kind is one of: "image" | "rootfs" | "console_log" | "metadata" |
	// "instance" | "lock" | "tmp" | "source" | "snapshot_orphan".
	Kind string `json:"kind"`
	Path string `json:"path,omitempty"`
	// Name is the shed or image name when applicable.
	Name string `json:"name,omitempty"`
	// Action is "deleted" or "truncated".
	Action string `json:"action"`
	// Freed is the bytes attributed to this item. For clones or hardlinks
	// the physical count reflects attribution, not necessarily reclamation —
	// see PruneReport.Notes.
	Freed DiskSize `json:"freed"`
	// Reason is a human-readable justification (e.g. "stopped 5d ago").
	Reason string `json:"reason,omitempty"`
}

// SkippedItem is an entity the prune pass inspected but left alone, with a
// short reason. Examples: running shed; stopped but too recent; lock held
// by an in-flight conversion; malformed metadata.
type SkippedItem struct {
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason"`
}

// PruneReportTotals summarizes what the prune pass did (or would do).
type PruneReportTotals struct {
	Freed DiskSize `json:"freed"`
	Items int      `json:"items"`
}

// PruneReport is the payload returned by POST /api/system/prune.
type PruneReport struct {
	DryRun     bool              `json:"dry_run"`
	ServerName string            `json:"server_name"`
	Scope      []string          `json:"scope"`
	Until      string            `json:"until"`
	Items      []PrunedItem      `json:"items"`
	Skipped    []SkippedItem     `json:"skipped,omitempty"`
	Notes      []string          `json:"notes,omitempty"`
	Totals     PruneReportTotals `json:"totals"`
}

// PruneReportOrError is one entry in a multi-server aggregated response.
type PruneReportOrError struct {
	ServerName string       `json:"server_name"`
	Report     *PruneReport `json:"report,omitempty"`
	Error      string       `json:"error,omitempty"`
}

// SystemPruneResponse is the client-side aggregation of per-server prune
// results from `shed system prune --all`. Never returned by the API directly.
type SystemPruneResponse struct {
	Servers []PruneReportOrError `json:"servers"`
}

// CreateShedRequest is the request body for POST /api/sheds.
type CreateShedRequest struct {
	Name        string `json:"name"`
	Repo        string `json:"repo,omitempty"`
	Image       string `json:"image,omitempty"`
	NoProvision bool   `json:"no_provision,omitempty"`

	// Backend specifies which backend to use ("firecracker" or "vz").
	// If empty, uses the server's configured backend.
	Backend string `json:"backend,omitempty"`

	// CPUs specifies the number of vCPUs (firecracker/vz only)
	CPUs int `json:"cpus,omitempty"`

	// MemoryMB specifies the memory in MB (firecracker/vz only)
	MemoryMB int `json:"memory_mb,omitempty"`

	// LocalDir mounts a host directory under the home directory (at
	// /home/shed/<basename>) and makes it the landing directory.
	// Mutually exclusive with Repo.
	LocalDir string `json:"local_dir,omitempty"`

	// AddDirs mounts additional host directories under the home directory
	// (each at /home/shed/<basename>) as reference siblings of LocalDir.
	// Only valid together with LocalDir.
	AddDirs []string `json:"add_dirs,omitempty"`

	// Egress assigns Level-1 egress-control profiles to this shed (composed,
	// first-match). Empty inherits the server `egress.default`; ["off"]
	// disables egress for this shed even when a default is set.
	Egress []string `json:"egress,omitempty"`

	// FromSnapshot spawns the shed from a snapshot's rootfs instead of a base image.
	// Mutually exclusive with Image and Repo. Provisioning steps (repo clone, install
	// hook, first-time auto-sync) are skipped because the snapshot is already provisioned.
	FromSnapshot string `json:"from_snapshot,omitempty"`

	// UpperSizeBytes is the logical size of the per-shed writable upper.
	// Zero falls back to the backend's upper_size_default config value.
	UpperSizeBytes int64 `json:"upper_size_bytes,omitempty"`

	// Mounts selects the server mounts (top-level `mounts:`) the shed gets:
	// omitted (nil) = every server mount, as before; [] = none; names =
	// exactly those. A service client may name only its policy's
	// server_mounts, and omitting it means exactly those.
	Mounts *[]string `json:"mounts,omitempty"`

	// Network is the shed's network profile ("open" or "isolated"; omitted =
	// the server's network.default_profile, or a service client's pinned
	// policy.network). The API server resolves it and writes the effective
	// profile back here before the backend runs, so a backend always sees an
	// explicit value from the API.
	Network string `json:"network,omitempty"`

	// Owner and ResolvedMounts are set by the API server after
	// authorization (json:"-": no caller can set them on the wire). Owner is
	// the service client creating the shed ("" for an admin);
	// ResolvedMounts is the stored mount selection. A backend called with a
	// nil ResolvedMounts (a non-API caller) stores {mode: all}.
	Owner          string          `json:"-"`
	ResolvedMounts *MountSelection `json:"-"`
}

// SnapshotSchemaVersion is the current snapshot schema version. Bumped
// from 1 to 2 with the introduction of LowerDigest tracking.
const SnapshotSchemaVersion = 2

// Snapshot represents a captured rootfs that can be used to spawn new sheds.
type Snapshot struct {
	// Version is the snapshot schema version (current: SnapshotSchemaVersion).
	Version int `json:"version"`

	// Name is the unique snapshot identifier within a server.
	Name string `json:"name"`

	// Backend is "vz" or "firecracker"; only matching backends can spawn from this snapshot.
	Backend string `json:"backend"`

	// SourceShed is the shed this snapshot was created from. May reference a deleted shed.
	SourceShed string `json:"source_shed,omitempty"`

	// SourceImage is the image variant the source shed was created from (provenance hint).
	SourceImage string `json:"source_image,omitempty"`

	// SourceLocalDirs are the host directories the source shed mounted
	// (--local-dir / --add-dir); hint only, not bound at spawn.
	SourceLocalDirs []string `json:"source_local_dirs,omitempty"`

	// Comment is an optional user-supplied note attached at create time.
	Comment string `json:"comment,omitempty"`

	// CreatedAt is when the snapshot was captured.
	CreatedAt time.Time `json:"created_at"`

	// SizeBytes is the apparent (logical) size of the snapshot rootfs.
	SizeBytes int64 `json:"size_bytes,omitempty"`

	// LowerDigest is the digest of the lower (base) image the source shed
	// was created from, in the form "sha256:...". Snapshots count toward
	// the lower's refcount: pruning a digest pinned by a snapshot is
	// refused. Empty for snapshots created before schema v2.
	LowerDigest string `json:"lower_digest,omitempty"`

	// LowerCached reports whether the lower digest's blob is currently
	// installed in the local image store. Computed at read time, never
	// persisted (the on-disk value is recomputed on each load). When
	// false, `shed create --from-snapshot` will fail until the lower
	// image is pulled or rebuilt; surfaced by `shed snapshot info`.
	LowerCached bool `json:"lower_cached,omitempty"`
}

// SnapshotCreateRequest is the request body for POST /api/snapshots.
type SnapshotCreateRequest struct {
	Name       string `json:"name"`
	SourceShed string `json:"source_shed"`
	Comment    string `json:"comment,omitempty"`
}

// SnapshotsResponse is returned by GET /api/snapshots.
type SnapshotsResponse struct {
	Snapshots []Snapshot `json:"snapshots"`
}

// SnapshotCreateResponse is returned by POST /api/snapshots. It wraps the
// created snapshot together with any non-fatal warnings emitted during the
// operation (e.g., source shed used --local-dir so workspace contents are
// not captured). Wire format is intentionally distinct from the Snapshot
// type so warnings can grow without disturbing snapshot.json on disk.
type SnapshotCreateResponse struct {
	Snapshot *Snapshot `json:"snapshot"`
	Warnings []string  `json:"warnings,omitempty"`
}

// SnapshotDiskEntry describes one snapshot's disk footprint for `shed system df`.
// OtherFiles holds metadata sidecars (snapshot.json) so callers can sum the
// total footprint without hardcoding a per-snapshot file count.
type SnapshotDiskEntry struct {
	Name       string      `json:"name"`
	SourceShed string      `json:"source_shed,omitempty"`
	Rootfs     FileEntry   `json:"rootfs"`
	OtherFiles []FileEntry `json:"other_files,omitempty"`
	Total      DiskSize    `json:"total"`
}

// APIError represents an error response from the API.
type APIError struct {
	Error APIErrorDetail `json:"error"`
}

// APIErrorDetail contains the error code and message.
type APIErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// NewAPIError creates a new APIError with the given code and message.
func NewAPIError(code, message string) APIError {
	return APIError{
		Error: APIErrorDetail{
			Code:    code,
			Message: message,
		},
	}
}

// Error codes for API responses.
const (
	ErrShedNotFound       = "SHED_NOT_FOUND"
	ErrShedAlreadyExists  = "SHED_ALREADY_EXISTS"
	ErrShedAlreadyRunning = "SHED_ALREADY_RUNNING"
	ErrShedAlreadyStopped = "SHED_ALREADY_STOPPED"
	ErrShedNotStopped     = "SHED_NOT_STOPPED"
	ErrInvalidShedName    = "INVALID_SHED_NAME"
	ErrInvalidRepoURL     = "INVALID_REPO_URL"
	ErrCloneFailed        = "CLONE_FAILED"
	ErrBackendNotEnabled  = "BACKEND_NOT_ENABLED"
	ErrUnknownImage       = "UNKNOWN_IMAGE"
	ErrImageNotFound      = "IMAGE_NOT_FOUND"
	ErrImageInUse         = "IMAGE_IN_USE"
	ErrBackendError       = "BACKEND_ERROR"
	ErrInternalError      = "INTERNAL_ERROR"
	ErrSessionNotFound    = "SESSION_NOT_FOUND"
	ErrInvalidSessionName = "INVALID_SESSION_NAME"
	ErrTmuxNotAvailable   = "TMUX_NOT_AVAILABLE"
	ErrInvalidLocalDir    = "INVALID_LOCAL_DIR"
	ErrInvalidRequest     = "INVALID_REQUEST"

	// ErrShedNotRunning (409) is the exec endpoint's "the shed must be
	// running" refusal. Exec never auto-starts a shed. Other routes keep
	// SHED_ALREADY_STOPPED for the same condition.
	ErrShedNotRunning = "SHED_NOT_RUNNING"
	// ErrExecLimit (429) means the shed already runs
	// exec.max_concurrent_per_shed HTTPS execs.
	ErrExecLimit = "EXEC_LIMIT"
	// ErrRequestTooLarge (413) means a request body or its decoded stdin is
	// over the server's limit.
	ErrRequestTooLarge = "REQUEST_TOO_LARGE"
	// ErrPolicyViolation (403) means a service client's create request
	// breaks its auth.clients policy; the message names the rule.
	ErrPolicyViolation = "POLICY_VIOLATION"
	// ErrForbidden (403) means the caller's credential may not reach this
	// route (a scope mismatch, or a route outside a service client's
	// allowlist).
	ErrForbidden = "FORBIDDEN"
	// ErrUnauthorized (401) means no valid credential was presented.
	ErrUnauthorized = "UNAUTHORIZED"
	// ErrInvalidNetworkProfile (400) means a create request's network is not
	// a known profile.
	ErrInvalidNetworkProfile = "INVALID_NETWORK_PROFILE"
	// ErrNetworkProfileUnsupported (422) means the backend cannot provide the
	// requested profile (VZ offers only "open").
	ErrNetworkProfileUnsupported = "NETWORK_PROFILE_UNSUPPORTED"
	// ErrNetworkEnforcementUnavailable (503) means the server could not
	// install the firewall rules a non-open profile needs.
	ErrNetworkEnforcementUnavailable = "NETWORK_ENFORCEMENT_UNAVAILABLE"

	ErrSnapshotNotFound        = "SNAPSHOT_NOT_FOUND"
	ErrSnapshotAlreadyExists   = "SNAPSHOT_ALREADY_EXISTS"
	ErrSnapshotSourceRunning   = "SNAPSHOT_SOURCE_RUNNING"
	ErrSnapshotBackendMismatch = "SNAPSHOT_BACKEND_MISMATCH"
	ErrInvalidSnapshotName     = "INVALID_SNAPSHOT_NAME"

	ErrProfileNotFound = "PROFILE_NOT_FOUND"
	ErrProfileReserved = "PROFILE_RESERVED" // name collides with a config/reserved profile
	ErrProfileInUse    = "PROFILE_IN_USE"   // referenced by one or more sheds
)

// Backend type constants for Shed.Backend field.
const (
	BackendFirecracker = "firecracker"
	BackendVZ          = "vz"
	BackendDetect      = "detect"
)

// SSH auth modes for SSHAuthConfig.Mode.
const (
	SSHAuthOff     = "off"     // accept all keys (legacy default)
	SSHAuthWarn    = "warn"    // log would-deny attempts, but still accept
	SSHAuthEnforce = "enforce" // reject keys not in the allowlist
)

// Auth modes for AuthConfig.Mode — the secure-by-default switch.
// "secure" is the pre-rename spelling of AuthModeToken, kept as a deprecated
// alias that config load normalizes to AuthModeToken (see normalizeAuthMode
// in server.go) — downstream code only ever observes AuthModeToken.
const (
	AuthModeOpen  = "open"  // default: no enforcement (tailnet/LAN posture)
	AuthModeToken = "token" // SSH allowlist + HTTP tokens + TLS, all enforced
	// AuthModeMTLS: the client credential is a short-lived certificate issued
	// over the SSH bootstrap channel; no bearer tokens exist in this mode.
	// Shares every other token-mode invariant (SSH allowlist enforce, TLS-only,
	// https_port default) — see AuthEnforced in server.go.
	AuthModeMTLS = "mtls"
)

// AuthConfig configures authentication. The headline control is Mode (the
// open|token|mtls switch). The SSH sub-block carries key sources and the
// advanced SSH mode override. HTTP bearer-token enforcement is derived purely
// from token mode — there is no HTTP sub-block.
type AuthConfig struct {
	// Mode is open | token | mtls (default open; the deprecated "secure"
	// spelling is normalized to "token" at config load, with one startup
	// deprecation warning). token derives: SSH allowlist enforce, HTTP
	// bearer-token enforce, and TLS on (the server serves the TLS listener
	// only). mtls derives the same SSH-allowlist-enforce and TLS-only posture,
	// but the client credential is a short-lived certificate rather than a
	// bearer token. Both require at least one SSH key source.
	Mode string `yaml:"mode,omitempty"`
	// TokenTTL is the lifetime of a bootstrap-minted HTTP token (default 24h).
	TokenTTL Duration `yaml:"token_ttl,omitempty"`
	// SSH configures the SSH public-key allowlist (key sources + advanced mode).
	SSH *SSHAuthConfig `yaml:"ssh,omitempty"`
	// Clients are the service clients (mtls mode only): orchestrators that
	// authenticate with a certificate from `shed-server client issue` and may
	// create, run commands in and delete only their own sheds, within their
	// policy. Keyed by client name.
	Clients map[string]ServiceClientConfig `yaml:"clients,omitempty"`
}

// DefaultClientCertMaxTTL is auth.clients.<name>.cert_max_ttl when unset.
const DefaultClientCertMaxTTL = 90 * 24 * time.Hour

// ServiceClientConfig is one service client under auth.clients. Unknown keys are
// refused at config load (rejectUnknownClientKeys), so a typo in a security
// policy fails loudly.
type ServiceClientConfig struct {
	// CertMaxTTL is the longest certificate `client issue` may sign for this
	// client (default 90 days).
	CertMaxTTL Duration `yaml:"cert_max_ttl,omitempty"`
	// Policy bounds what the client may create.
	Policy ServiceClientPolicy `yaml:"policy"`
}

// ServiceClientPolicy is a service client's create policy.
type ServiceClientPolicy struct {
	// NamePrefix is required: every shed the client creates must start
	// with it.
	NamePrefix string `yaml:"name_prefix"`
	// MaxSheds is the most sheds the client may own at once, running or
	// stopped (at least 1).
	MaxSheds int `yaml:"max_sheds"`
	// MaxAge is how old an owned shed may get before the server deletes it.
	MaxAge Duration `yaml:"max_age"`
	// ServerMounts names the top-level mounts the client's sheds may get
	// (default none). A create that omits mounts gets exactly these.
	ServerMounts []string `yaml:"server_mounts,omitempty"`
	// Images are the image aliases the client may ask for; empty means the
	// server default only.
	Images []string `yaml:"images,omitempty"`
	// Caps bound the resources of one shed, compared after defaults.
	Caps ServiceClientCaps `yaml:"caps,omitempty"`
	// Network is the network profile every shed of this client gets
	// ("open" or "isolated"; default isolated). A create that names another
	// profile is refused.
	Network string `yaml:"network,omitempty"`
}

// NetworkProfile is the client's pinned network profile: policy.network, or
// "isolated" when unset.
func (p ServiceClientPolicy) NetworkProfile() string {
	if p.Network == "" {
		return NetworkProfileIsolated
	}
	return p.Network
}

// NetworkConfig is the server-level network block. Every field is optional;
// an absent block is the open default.
type NetworkConfig struct {
	// DefaultProfile is the profile a create that names none gets ("open",
	// the default, or "isolated"; non-open requires Firecracker).
	DefaultProfile string `yaml:"default_profile,omitempty"`
	// AllowCIDRs are IPv4 ranges an isolated shed may reach although they
	// fall in the blocked ranges (a LAN registry, say). They apply to
	// forwarded traffic only, never to the host itself.
	AllowCIDRs []string `yaml:"allow_cidrs,omitempty"`
}

// ServiceClientCaps are optional per-shed resource caps (0 = no cap).
type ServiceClientCaps struct {
	CPUs     int `yaml:"cpus,omitempty"`
	MemoryMB int `yaml:"memory_mb,omitempty"`
}

// CertMaxTTLValue is cert_max_ttl, or DefaultClientCertMaxTTL when unset.
func (c ServiceClientConfig) CertMaxTTLValue() time.Duration {
	if c.CertMaxTTL > 0 {
		return c.CertMaxTTL.Duration()
	}
	return DefaultClientCertMaxTTL
}

// ExecConfig bounds the HTTPS exec endpoint (POST /api/sheds/{name}/exec).
// Every field is optional; a zero value takes its default (see the
// ServerConfig.Exec* accessors in server.go).
type ExecConfig struct {
	// MaxStdinBytes caps a request's decoded stdin (default 1 MiB).
	MaxStdinBytes int64 `yaml:"max_stdin_bytes,omitempty"`
	// MaxTimeout is the largest timeout_seconds a request may ask for
	// (default 1h, at least 1s). A larger request is refused, not clamped.
	MaxTimeout Duration `yaml:"max_timeout,omitempty"`
	// MaxConcurrentPerShed caps the HTTPS execs running in one shed at once
	// (default 8); one more is refused with 429 EXEC_LIMIT.
	MaxConcurrentPerShed int `yaml:"max_concurrent_per_shed,omitempty"`
}

// AuditConfig turns on and shapes shed-server's audit log (<state>/audit.jsonl).
// The log is on whenever auth.clients is set, whatever Enabled says.
type AuditConfig struct {
	// Enabled turns the audit log on for a server with no service clients
	// (it then records HTTPS execs; admin API requests are never logged).
	Enabled bool `yaml:"enabled,omitempty"`
	// ExecArgv is "" (the default: an exec.end line records argv[0] and the
	// argument count) or "full" (the whole argv, which may carry prompts or
	// secrets).
	ExecArgv string `yaml:"exec_argv,omitempty"`
}

// AuditExecArgvFull is audit.exec_argv's one non-default value.
const AuditExecArgvFull = "full"

// ExecRequest is the body of POST /api/sheds/{name}/exec. Argv runs as given
// (no shell wrap) as the shed user; a caller who wants a login shell passes
// ["bash","-lc",...]. Stdin is standard base64.
type ExecRequest struct {
	Argv           []string          `json:"argv"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	MaxOutputBytes int64             `json:"max_output_bytes,omitempty"`
}

// Exec stream frame types: each NDJSON line of a 200 exec response carries
// one of these in "t".
const (
	ExecFrameStdout = "stdout"
	ExecFrameStderr = "stderr"
	ExecFrameExit   = "exit"
)

// ExecDataFrame is one chunk of a command's stdout or stderr, base64-encoded.
type ExecDataFrame struct {
	T string `json:"t"`
	D string `json:"d"`
}

// ExecExitFrame is the last line of an exec stream. A stream that ends
// without one means the result was lost (the command may or may not have
// finished). Code is 124 with TimedOut on a timeout; Truncated means output
// past max_output_bytes was discarded (the command still ran to its exit).
type ExecExitFrame struct {
	T          string `json:"t"`
	Code       int    `json:"code"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
}

// SSHAuthConfig configures the SSH public-key allowlist. Identity comes from
// the offered key (the username still selects the shed), GitHub-style.
type SSHAuthConfig struct {
	// Mode is off | warn | enforce (default off). off accepts all keys
	// (legacy); warn logs would-deny attempts but accepts; enforce rejects
	// keys not in the allowlist.
	Mode string `yaml:"mode,omitempty"`
	// AuthorizedKeys are inline OpenSSH authorized_keys lines.
	AuthorizedKeys []string `yaml:"authorized_keys,omitempty"`
	// AuthorizedKeysFile is a path to an authorized_keys-format file.
	AuthorizedKeysFile string `yaml:"authorized_keys_file,omitempty"`
	// GitHubUsers seeds the allowlist from https://github.com/<user>.keys,
	// cached to disk and failing closed to the last-known-good cache.
	GitHubUsers []string `yaml:"github_users,omitempty"`
	// GitHubRefresh is how often to re-fetch GitHub keys (default 1h).
	GitHubRefresh Duration `yaml:"github_refresh,omitempty"`
	// MaxAuthTries caps public-key attempts per connection (0 = shed default 10).
	// Raise it for clients whose agent holds many keys (1Password, Secretive) so
	// the allowlisted key is tried before the server gives up.
	MaxAuthTries int `yaml:"max_auth_tries,omitempty"`
}

// githubUsernamePattern guards the URL built from github_users. GitHub
// usernames are alphanumeric with single internal hyphens, 1–39 chars; this
// also blocks path traversal (no '/' or '.').
var githubUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

// ValidGitHubUsername reports whether s is a syntactically valid GitHub
// username (and, by construction, safe to interpolate into the .keys URL).
func ValidGitHubUsername(s string) bool { return githubUsernamePattern.MatchString(s) }

// HomePath is the shed user's home directory inside the VM. Repos, --local-dir,
// and --add-dir mounts all live under here, and interactive logins land here by
// default (or in a project subdirectory when one is present).
const HomePath = "/home/shed"

// ShedUser is the guest's login user and primary group (UID 1000), owner of HomePath.
const ShedUser = "shed"

// CredentialMountTag returns the VirtioFS/9P mount tag for a credential share.
// Tags use the format "cred-{name}" to avoid collisions with project mounts.
func CredentialMountTag(name string) string {
	return "cred-" + name
}

// maxMountTagLen is the maximum length of a virtio-fs mount tag (the guest's
// virtio_fs device tag field is 36 bytes).
const maxMountTagLen = 36

// ProjectMountTag returns a stable, unique, virtio-fs-safe mount tag for a
// project mount (--local-dir / --add-dir) identified by its guest-dir basename.
// The tag is "proj-<sanitized>-<hash>": the hash keeps tags distinct even when
// two different basenames sanitize to the same prefix, and the whole tag is
// capped at maxMountTagLen.
func ProjectMountTag(basename string) string {
	const prefix = "proj-"
	sum := sha256.Sum256([]byte(basename))
	suffix := hex.EncodeToString(sum[:])[:8]
	san := sanitizeMountTag(basename)
	// Budget: len(prefix) + len(san) + len("-") + len(suffix) <= maxMountTagLen.
	maxSan := maxMountTagLen - len(prefix) - 1 - len(suffix)
	if len(san) > maxSan {
		san = san[:maxSan]
	}
	return prefix + san + "-" + suffix
}

// sanitizeMountTag replaces every byte outside [A-Za-z0-9_-] with '_'.
func sanitizeMountTag(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			// keep
		default:
			b[i] = '_'
		}
	}
	return string(b)
}
