// OCI manifest, config, and index types for shed's image store. These types
// mirror the OCI image-spec v1 shapes verbatim (so the on-disk blobs can be
// inspected by tools like crane / oras / skopeo) plus a small set of
// shed-specific annotations carried alongside.
//
// References:
//   - https://github.com/opencontainers/image-spec/blob/main/manifest.md
//   - https://github.com/opencontainers/image-spec/blob/main/config.md
//   - https://github.com/opencontainers/image-spec/blob/main/image-index.md
//   - https://github.com/opencontainers/image-spec/blob/main/image-layout.md

package vmimage

import (
	"encoding/json"
	"fmt"
)

// OCI media types used by shed. We standardize on the OCI variants
// (application/vnd.oci.*) rather than the legacy Docker variants — both
// reference implementations interoperate, but OCI is the spec.
const (
	MediaTypeOCIIndex    = "application/vnd.oci.image.index.v1+json"
	MediaTypeOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIConfig   = "application/vnd.oci.image.config.v1+json"
	MediaTypeOCILayer    = "application/vnd.oci.image.layer.v1.tar+gzip"

	// MediaTypeShedKernel and MediaTypeShedInitrd are shed-specific blob
	// types carried alongside the standard rootfs layer. Foreign OCI
	// tools see them as generic blobs and skip them; shed reads them as
	// the kernel/initrd to boot the VM.
	MediaTypeShedKernel = "application/vnd.shed.kernel"
	MediaTypeShedInitrd = "application/vnd.shed.initrd"
)

// Shed-specific manifest annotations. These live alongside standard OCI
// annotations (org.opencontainers.image.*) in the manifest's annotations
// map and on per-descriptor annotations.
const (
	// AnnotationVariant marks the variant of a shed image
	// (e.g. "base", "extensions", "full"). Display-only.
	AnnotationVariant = "io.shed.variant"

	// AnnotationKernelDigest names the blob digest of the kernel
	// associated with this image, when shed extracted one.
	AnnotationKernelDigest = "io.shed.kernel.digest"

	// AnnotationInitrdDigest names the blob digest of the initrd
	// associated with this image, when shed extracted one.
	AnnotationInitrdDigest = "io.shed.initrd.digest"

	// AnnotationSchemaVersion records shed's own manifest schema version
	// alongside OCI's schemaVersion (always 2 for OCI manifests). Lets
	// us bump shed semantics independently of OCI.
	AnnotationSchemaVersion = "io.shed.schema-version"

	// AnnotationSourceRef preserves the registry ref the image was
	// pulled from or built against. Useful for cache freshness checks.
	AnnotationSourceRef = "io.shed.source-ref"

	// AnnotationRootfsLogicalSize records the logical (sparse) byte
	// size of the derived rootfs ext4. Display-only.
	AnnotationRootfsLogicalSize = "io.shed.rootfs.logical-size"
)

// ShedSchemaVersion is the current shed-specific manifest schema version.
// Bumped when the annotation contract changes.
const ShedSchemaVersion = "1"

// MaxLayers caps the number of layers a shed image manifest may list.
// Bounds both the Firecracker drive count (1 upper + N lowers ≤ 17,
// well under FC's 26-drive limit) and overlayfs lowerdir= argument
// length (each layer adds a path; PAGE_SIZE ~4096 caps the total).
const MaxLayers = 16

// Descriptor is the OCI content-descriptor pointing at a blob by digest.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// OCIManifest is an OCI image manifest. The shed-on-disk shape is the
// standard OCI shape; consumers can inspect it via `crane manifest`.
type OCIManifest struct {
	SchemaVersion int               `json:"schemaVersion"` // always 2
	MediaType     string            `json:"mediaType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// OCIConfig is the OCI image config. Shed populates only the fields it
// uses; the rest are omitted via omitempty so foreign tools see a
// well-formed config with no surprises.
type OCIConfig struct {
	Architecture string          `json:"architecture"` // "arm64" or "amd64"
	OS           string          `json:"os"`           // "linux"
	Created      string          `json:"created,omitempty"`
	Author       string          `json:"author,omitempty"`
	RootFS       OCIRootFS       `json:"rootfs"`
	History      []OCIHistory    `json:"history,omitempty"`
	Config       OCIConfigConfig `json:"config,omitempty"`
}

// OCIRootFS is the rootfs section of the OCI image config.
type OCIRootFS struct {
	Type    string   `json:"type"`     // "layers"
	DiffIDs []string `json:"diff_ids"` // sha256 digests of uncompressed layer tars
}

// OCIHistory is the per-layer history entry, optional but conventional.
type OCIHistory struct {
	Created    string `json:"created,omitempty"`
	CreatedBy  string `json:"created_by,omitempty"`
	Comment    string `json:"comment,omitempty"`
	EmptyLayer bool   `json:"empty_layer,omitempty"`
}

// OCIConfigConfig is the runtime config block. Shed doesn't run images
// like a container, so we leave most fields empty.
type OCIConfigConfig struct {
	Env        []string `json:"Env,omitempty"`
	Entrypoint []string `json:"Entrypoint,omitempty"`
	Cmd        []string `json:"Cmd,omitempty"`
	WorkingDir string   `json:"WorkingDir,omitempty"`
}

// OCIIndex is the top-level image index stored at {imagesDir}/index.json.
// Shed lists every installed manifest here with a ref-name annotation
// (org.opencontainers.image.ref.name) so foreign tools can enumerate
// the store with `crane catalog dir:<imagesDir>`.
type OCIIndex struct {
	SchemaVersion int               `json:"schemaVersion"` // always 2
	MediaType     string            `json:"mediaType,omitempty"`
	Manifests     []Descriptor      `json:"manifests"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// OCILayoutMarker is the content of the {imagesDir}/oci-layout file.
type OCILayoutMarker struct {
	ImageLayoutVersion string `json:"imageLayoutVersion"`
}

// CurrentOCILayoutVersion is the OCI image-layout version shed writes.
const CurrentOCILayoutVersion = "1.0.0"

// MarshalIndent emits OCI-compliant JSON with stable indentation.
func (m *OCIManifest) MarshalIndent() ([]byte, error) {
	if m.SchemaVersion == 0 {
		m.SchemaVersion = 2
	}
	if m.MediaType == "" {
		m.MediaType = MediaTypeOCIManifest
	}
	return json.MarshalIndent(m, "", "  ")
}

// MarshalIndent emits OCI-compliant JSON with stable indentation.
func (c *OCIConfig) MarshalIndent() ([]byte, error) {
	if c.RootFS.Type == "" {
		c.RootFS.Type = "layers"
	}
	if c.OS == "" {
		c.OS = "linux"
	}
	return json.MarshalIndent(c, "", "  ")
}

// MarshalIndent emits OCI-compliant JSON with stable indentation.
func (i *OCIIndex) MarshalIndent() ([]byte, error) {
	if i.SchemaVersion == 0 {
		i.SchemaVersion = 2
	}
	if i.MediaType == "" {
		i.MediaType = MediaTypeOCIIndex
	}
	if i.Manifests == nil {
		i.Manifests = []Descriptor{}
	}
	return json.MarshalIndent(i, "", "  ")
}

// ParseManifest decodes an OCI manifest blob.
func ParseManifest(data []byte) (*OCIManifest, error) {
	var m OCIManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if m.SchemaVersion != 2 {
		return nil, fmt.Errorf("unsupported manifest schemaVersion %d (want 2)", m.SchemaVersion)
	}
	if len(m.Layers) > MaxLayers {
		return nil, fmt.Errorf("manifest has %d layers (max %d)", len(m.Layers), MaxLayers)
	}
	return &m, nil
}

// ParseConfig decodes an OCI image config blob.
func ParseConfig(data []byte) (*OCIConfig, error) {
	var c OCIConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &c, nil
}

// ParseIndex decodes an OCI index blob.
func ParseIndex(data []byte) (*OCIIndex, error) {
	var i OCIIndex
	if err := json.Unmarshal(data, &i); err != nil {
		return nil, fmt.Errorf("parsing index: %w", err)
	}
	if i.SchemaVersion != 0 && i.SchemaVersion != 2 {
		return nil, fmt.Errorf("unsupported index schemaVersion %d (want 2)", i.SchemaVersion)
	}
	return &i, nil
}

// ShedSourceRef returns the manifest's recorded source ref annotation,
// or "" if unset.
func (m *OCIManifest) ShedSourceRef() string {
	if m == nil || m.Annotations == nil {
		return ""
	}
	return m.Annotations[AnnotationSourceRef]
}

// ShedVariant returns the manifest's recorded variant annotation,
// or "" if unset.
func (m *OCIManifest) ShedVariant() string {
	if m == nil || m.Annotations == nil {
		return ""
	}
	return m.Annotations[AnnotationVariant]
}

// ShedKernelDigest returns the kernel blob digest carried in annotations,
// or "" if no kernel is associated with this image.
func (m *OCIManifest) ShedKernelDigest() string {
	if m == nil || m.Annotations == nil {
		return ""
	}
	return m.Annotations[AnnotationKernelDigest]
}

// ShedInitrdDigest returns the initrd blob digest carried in annotations,
// or "" if no initrd is associated with this image.
func (m *OCIManifest) ShedInitrdDigest() string {
	if m == nil || m.Annotations == nil {
		return ""
	}
	return m.Annotations[AnnotationInitrdDigest]
}
