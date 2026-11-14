package provider

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charliek/shed/internal/remote/guest"
)

// Instance metadata keys. Every shed key begins "shed-". The first three are
// read by the guest's boot unit and are defined in internal/remote/guest; the
// last two are read only by the server. None holds a secret.
const (
	// MetadataShedName is the shed's name.
	MetadataShedName = guest.MetadataShedName
	// MetadataShedID is the shed's permanent ID.
	MetadataShedID = guest.MetadataShedID
	// MetadataAuthorizedKeys is the server's SSH public key, one
	// authorized_keys line per key.
	MetadataAuthorizedKeys = guest.MetadataAuthorizedKeys
	// MetadataShedSpec is the ShedSpec as JSON: what a server needs, beyond
	// the labels, to rebuild the shed's record. Written once, at create.
	MetadataShedSpec = "shed-spec"
	// MetadataHostKeyPin is the one SSH host key the server accepts from
	// this instance, in authorized_keys form with no comment. The server
	// writes it once, at first contact, and it is absent before that. Only
	// the cloud API can change it, which is why the pin lives here and not
	// in a guest attribute.
	MetadataHostKeyPin = "shed-host-key"
)

// MaxMetadataBytes caps the total size of Spec.Metadata, keys plus values.
const MaxMetadataBytes = 4096

// ShedSpecSchema is the current ShedSpec.Schema.
const ShedSpecSchema = 1

// ShedSpec is the JSON document stored under MetadataShedSpec. Together with
// the labels and the cloud's own facts (state, address, disk size, planned
// stop) it is everything adoption needs: a record rebuilt from them equals
// the one that was lost. Readers ignore fields they do not know.
type ShedSpec struct {
	// Schema is ShedSpecSchema.
	Schema int `json:"schema"`
	// CreatedAt is when the shed was created, in UTC. It is the shed's
	// created_at, which the reaper and ownership checks compare; the
	// cloud's own creation time is not used for that.
	CreatedAt time.Time `json:"created_at"`
	// Repo is the repository cloned at create, with any user name and
	// password removed (see StripRepoUserinfo). Empty when none.
	Repo string `json:"repo,omitempty"`
	// Image is the exact image name the instance was created from, never
	// a family or an alias.
	Image string `json:"image"`
	// Size is the named size: SizeStandard, SizeLarge or another name from
	// the server config.
	Size string `json:"size"`
	// Network is the shed's network profile: "open" or "isolated".
	Network string `json:"network"`
	// LandingDir is where interactive logins land. Empty means the home
	// directory.
	LandingDir string `json:"landing_dir,omitempty"`
}

// Encode returns the JSON stored under MetadataShedSpec. The schema is set to
// ShedSpecSchema, the time is converted to UTC and the repo is stripped of
// credentials, so a caller cannot store them by mistake.
func (s ShedSpec) Encode() (string, error) {
	s.Schema = ShedSpecSchema
	s.CreatedAt = s.CreatedAt.UTC()
	s.Repo = StripRepoUserinfo(s.Repo)
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode shed-spec: %w", err)
	}
	return string(data), nil
}

// DecodeShedSpec parses a MetadataShedSpec value. An empty value or a schema
// newer than ShedSpecSchema is an error wrapping ErrInvalid.
func DecodeShedSpec(value string) (ShedSpec, error) {
	if strings.TrimSpace(value) == "" {
		return ShedSpec{}, Errorf(ErrInvalid, "", "metadata %s is empty", MetadataShedSpec)
	}
	var s ShedSpec
	if err := json.Unmarshal([]byte(value), &s); err != nil {
		return ShedSpec{}, &Error{Kind: ErrInvalid, Detail: "metadata " + MetadataShedSpec + " is not valid JSON", Err: err}
	}
	if s.Schema < 1 || s.Schema > ShedSpecSchema {
		return ShedSpec{}, Errorf(ErrInvalid, "", "metadata %s has schema %d, want %d", MetadataShedSpec, s.Schema, ShedSpecSchema)
	}
	return s, nil
}

// StripRepoUserinfo removes the whole user-and-password part from a
// repository URL: "https://token@host/o/r.git" and "https://u:p@host/o/r.git"
// both become "https://host/o/r.git". On an ssh:// URL the user is a login
// name, not a credential, so it is kept and only a password is dropped. A
// value with no scheme, such as "git@github.com:o/r.git" or "o/r", is
// returned unchanged.
//
// The backend stores the stripped form in the shed's record as well, so a
// record rebuilt from ShedSpec equals the original.
func StripRepoUserinfo(repo string) string {
	scheme, rest, ok := strings.Cut(repo, "://")
	if !ok {
		return repo
	}
	authority, path, hasPath := strings.Cut(rest, "/")
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return repo
	}
	host := authority[at+1:]
	if s := strings.ToLower(scheme); s == "ssh" || s == "git+ssh" || s == "ssh+git" {
		if user, _, _ := strings.Cut(authority[:at], ":"); user != "" {
			host = user + "@" + host
		}
	}
	out := scheme + "://" + host
	if hasPath {
		out += "/" + path
	}
	return out
}

// Architectures.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// Named sizes every remote server has unless its config replaces them.
const (
	// SizeStandard is the default: 2 vCPUs and 8 GB.
	SizeStandard = "standard"
	// SizeLarge is for heavy builds: 8 vCPUs and 32 GB.
	SizeLarge = "large"
	// DefaultDiskGB is the boot disk size when a create names none.
	DefaultDiskGB = 50
)

// Size is a named machine shape. A create request never names a machine
// type; it names a size, or gives CPUs and memory that ResolveSize maps to
// one.
type Size struct {
	// Name is the size's name in config and in ShedSpec.Size.
	Name string
	// MachineType is the cloud's name for the shape.
	MachineType string
	// CPUs and MemoryMB are what the shape provides; a shed's record
	// reports them.
	CPUs     int
	MemoryMB int
	// Arch is ArchAMD64 or ArchARM64. It must match the image.
	Arch string
}

// DefaultSizes returns the built-in size table for GCE.
func DefaultSizes() []Size {
	return []Size{
		{Name: SizeStandard, MachineType: "e2-standard-2", CPUs: 2, MemoryMB: 8192, Arch: ArchAMD64},
		{Name: SizeLarge, MachineType: "e2-standard-8", CPUs: 8, MemoryMB: 32768, Arch: ArchAMD64},
	}
}

// ResolveSize picks the size for a create request. With cpus and memoryMB
// both zero it returns the size named defaultName. Otherwise it returns the
// smallest size (fewest CPUs, then least memory) that provides at least cpus
// and at least memoryMB; a zero in one of them means "no requirement". When
// nothing fits, or defaultName is not in sizes, the error wraps ErrInvalid
// and names the sizes that exist.
func ResolveSize(sizes []Size, defaultName string, cpus, memoryMB int) (Size, error) {
	if cpus == 0 && memoryMB == 0 {
		for _, s := range sizes {
			if s.Name == defaultName {
				return s, nil
			}
		}
		return Size{}, Errorf(ErrInvalid, "", "default size %q is not defined (sizes: %s)", defaultName, describeSizes(sizes))
	}
	sorted := append([]Size(nil), sizes...)
	sort.SliceStable(sorted, func(a, b int) bool {
		if sorted[a].CPUs != sorted[b].CPUs {
			return sorted[a].CPUs < sorted[b].CPUs
		}
		return sorted[a].MemoryMB < sorted[b].MemoryMB
	})
	for _, s := range sorted {
		if s.CPUs >= cpus && s.MemoryMB >= memoryMB {
			return s, nil
		}
	}
	return Size{}, Errorf(ErrInvalid, "", "no size provides %d CPUs and %d MB (sizes: %s)", cpus, memoryMB, describeSizes(sizes))
}

func describeSizes(sizes []Size) string {
	parts := make([]string, 0, len(sizes))
	for _, s := range sizes {
		parts = append(parts, fmt.Sprintf("%s=%d CPUs/%d MB", s.Name, s.CPUs, s.MemoryMB))
	}
	return strings.Join(parts, ", ")
}

// Spec is everything a provider needs to create one instance. Build it with
// NewSpec so every provider is handed the same labels and metadata.
//
// What a Spec does not carry comes from the provider's own configuration and
// is the same for every instance: project, zone, network, the network tag
// NetworkTag(env), no service account, and whatever switches the cloud needs
// (on GCE, block-project-ssh-keys and enable-guest-attributes).
type Spec struct {
	// Name is the instance name, InstanceName(env, id).
	Name string
	// Labels are set on the instance; the DiskLabelKeys among them are also
	// set on the boot disk.
	Labels map[string]string
	// Metadata is set on the instance. It is not secret, and at most
	// MaxMetadataBytes in total.
	Metadata map[string]string
	// Size is the machine shape.
	Size Size
	// DiskGB is the boot disk size. The disk is deleted with the instance.
	DiskGB int
	// Image is the exact image to boot.
	Image string
	// MaxRun is the run limit: how long the instance may run after each
	// start before the cloud stops it (never deletes it). Zero means no
	// limit. A provider without Capabilities.EnforcesRunLimit ignores it.
	MaxRun time.Duration
}

// NewSpec builds the Spec for a shed. authorizedKeys are the server's SSH
// public keys in authorized_keys form (one normally, two during a rotation).
// The labels are id.Labels(ShedStateCreating); the metadata holds the shed's
// name, ID, the keys and the encoded ShedSpec, whose Image and Size are
// filled from the arguments.
func NewSpec(id Identity, shed ShedSpec, authorizedKeys []string, size Size, diskGB int, image string, maxRun time.Duration) (Spec, error) {
	shed.Image = image
	shed.Size = size.Name
	encoded, err := shed.Encode()
	if err != nil {
		return Spec{}, err
	}
	keys := make([]string, 0, len(authorizedKeys))
	for _, k := range authorizedKeys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	spec := Spec{
		Name:   id.InstanceName(),
		Labels: id.Labels(ShedStateCreating),
		Metadata: map[string]string{
			MetadataShedName:       id.Name,
			MetadataShedID:         id.ID,
			MetadataAuthorizedKeys: strings.Join(keys, "\n"),
			MetadataShedSpec:       encoded,
		},
		Size:   size,
		DiskGB: diskGB,
		Image:  image,
		MaxRun: maxRun,
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// Validate checks a Spec before anything is sent to a cloud. Providers call
// it first in Create and return its error, which wraps ErrInvalid.
func (s Spec) Validate() error {
	id, err := IdentityFromLabels(s.Labels)
	if err != nil {
		return err
	}
	if err := checkLabels(s.Labels); err != nil {
		return Errorf(ErrInvalid, "create", "%v", err)
	}
	switch {
	case s.Name != id.InstanceName():
		return Errorf(ErrInvalid, "create", "instance name %q does not match its labels (want %q)", s.Name, id.InstanceName())
	case s.Labels[LabelState] != string(ShedStateCreating):
		return Errorf(ErrInvalid, "create", "label %s must be %q at create", LabelState, ShedStateCreating)
	case s.Metadata[MetadataShedName] != id.Name || s.Metadata[MetadataShedID] != id.ID:
		return Errorf(ErrInvalid, "create", "metadata name and id do not match the labels")
	case strings.TrimSpace(s.Metadata[MetadataAuthorizedKeys]) == "":
		return Errorf(ErrInvalid, "create", "metadata %s is empty: the server could never log in", MetadataAuthorizedKeys)
	case s.Size.MachineType == "" || s.Size.Arch == "":
		return Errorf(ErrInvalid, "create", "size %q has no machine type or architecture", s.Size.Name)
	case s.DiskGB <= 0:
		return Errorf(ErrInvalid, "create", "disk size must be positive")
	case s.Image == "":
		return Errorf(ErrInvalid, "create", "image is empty")
	case s.MaxRun < 0:
		return Errorf(ErrInvalid, "create", "run limit is negative")
	}
	if _, err := DecodeShedSpec(s.Metadata[MetadataShedSpec]); err != nil {
		return err
	}
	total := 0
	for k, v := range s.Metadata {
		if !strings.HasPrefix(k, "shed-") {
			return Errorf(ErrInvalid, "create", "metadata key %q does not begin \"shed-\"", k)
		}
		total += len(k) + len(v)
	}
	if total > MaxMetadataBytes {
		return Errorf(ErrInvalid, "create", "metadata is %d bytes, over the %d limit", total, MaxMetadataBytes)
	}
	return nil
}
