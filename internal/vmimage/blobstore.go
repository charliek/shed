// Package vmimage's blob store provides a content-addressed image store
// with tag indirection, mirroring how container runtimes manage images.
//
// On-disk layout under {imagesDir}:
//
//	blobs/sha256/<digest>/
//	  rootfs.ext4    mode 0444   (the read-only lower)
//	  kernel         mode 0444   (optional, when extracted)
//	  initrd         mode 0444   (optional, when extracted)
//	  manifest.json  mode 0444
//	  .lock                       (flock for atomic install + GC)
//	tags/
//	  <tag>.json                  ({"digest": "sha256:...", "updated_at": "..."})
//
// Identity: the digest is the sha256 of the produced rootfs.ext4 file. The
// blob is installed atomically via a tmp directory + rename, so partial
// installs never become visible to readers.
//
// Tags are advanced atomically via tmp file + rename. A tag pointing at a
// digest does NOT protect that digest from prune (Docker-style: untag with
// `image rm`, then `image prune`). Only sheds and snapshots protect blobs.
package vmimage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DigestPrefix is the algorithm prefix for image digests. Always "sha256:".
const DigestPrefix = "sha256:"

// ManifestSchemaVersion is the current manifest.json schema version.
const ManifestSchemaVersion = 1

// blobsDir, tagsDir are the subdirectory names under imagesDir.
const (
	blobsDir = "blobs"
	tagsDir  = "tags"
)

// algorithmDir is the per-algorithm subdir under blobs/. Hardcoded to
// "sha256" since that's the only algorithm the on-disk layout supports.
const algorithmDir = "sha256"

// blobFiles lists the files written into a blob directory.
const (
	BlobRootfsFilename   = "rootfs.ext4"
	BlobKernelFilename   = "kernel"
	BlobInitrdFilename   = "initrd"
	BlobManifestFilename = "manifest.json"
	BlobLockFilename     = ".lock"
)

// Manifest describes an image blob. Stored at {blob_dir}/manifest.json.
type Manifest struct {
	SchemaVersion      int       `json:"schema_version"`
	Digest             string    `json:"digest"` // "sha256:..."
	Backend            string    `json:"backend,omitempty"`
	Arch               string    `json:"arch,omitempty"`
	SourceRef          string    `json:"source_ref,omitempty"`        // Docker ref at convert time
	SourceRefDigest    string    `json:"source_ref_digest,omitempty"` // resolved Docker manifest digest
	ShedExtVersion     string    `json:"shed_ext_version,omitempty"`
	KernelSize         int64     `json:"kernel_size,omitempty"`
	InitrdSize         int64     `json:"initrd_size,omitempty"`
	RootfsLogicalSize  int64     `json:"rootfs_logical_size"`
	RootfsPhysicalSize int64     `json:"rootfs_physical_size,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

// Tag represents a named pointer to a blob digest. Stored at tags/<name>.json.
type Tag struct {
	Digest    string    `json:"digest"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Sentinel errors for blob/tag operations.
var (
	// ErrBlobNotFound is returned when a digest is not present in the store.
	ErrBlobNotFound = errors.New("blob not found")

	// ErrTagNotFound is returned when a tag does not exist.
	ErrTagNotFound = errors.New("tag not found")

	// ErrInvalidDigest is returned when a digest string is malformed.
	ErrInvalidDigest = errors.New("invalid digest")

	// ErrInvalidTag is returned when a tag name is unsafe.
	ErrInvalidTag = errors.New("invalid tag name")
)

// BlobsRoot returns {imagesDir}/blobs.
func BlobsRoot(imagesDir string) string {
	return filepath.Join(imagesDir, blobsDir)
}

// TagsRoot returns {imagesDir}/tags.
func TagsRoot(imagesDir string) string {
	return filepath.Join(imagesDir, tagsDir)
}

// BlobDir returns the on-disk directory holding the blob with the given digest.
// digest must be of the form "sha256:<hex>".
func BlobDir(imagesDir, digest string) (string, error) {
	hex, err := digestHex(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(imagesDir, blobsDir, algorithmDir, hex), nil
}

// BlobRootfsPath returns the path to the blob's rootfs.ext4.
func BlobRootfsPath(imagesDir, digest string) (string, error) {
	dir, err := BlobDir(imagesDir, digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, BlobRootfsFilename), nil
}

// BlobManifestPath returns the path to the blob's manifest.json.
func BlobManifestPath(imagesDir, digest string) (string, error) {
	dir, err := BlobDir(imagesDir, digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, BlobManifestFilename), nil
}

// TagPath returns the path to tags/<tag>.json.
func TagPath(imagesDir, tag string) (string, error) {
	if err := ValidateImageName(tag); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidTag, err)
	}
	return filepath.Join(imagesDir, tagsDir, tag+".json"), nil
}

// digestHex validates a "sha256:<hex>" digest and returns the hex part.
func digestHex(digest string) (string, error) {
	if !strings.HasPrefix(digest, DigestPrefix) {
		return "", fmt.Errorf("%w: missing %s prefix", ErrInvalidDigest, DigestPrefix)
	}
	hex := strings.TrimPrefix(digest, DigestPrefix)
	if len(hex) != 64 {
		return "", fmt.Errorf("%w: expected 64 hex chars, got %d", ErrInvalidDigest, len(hex))
	}
	for _, r := range hex {
		isDigit := r >= '0' && r <= '9'
		isHexLower := r >= 'a' && r <= 'f'
		if !isDigit && !isHexLower {
			return "", fmt.Errorf("%w: non-hex character", ErrInvalidDigest)
		}
	}
	return hex, nil
}

// ShortDigest returns the first 12 hex chars of a digest for display.
func ShortDigest(digest string) string {
	hex, err := digestHex(digest)
	if err != nil {
		return digest
	}
	return DigestPrefix + hex[:12]
}

// HashFile computes the sha256 digest of a file's contents.
// Returns a digest string of the form "sha256:<hex>".
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hashing %s: %w", path, err)
	}
	return DigestPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// BlobInstallSpec describes a blob about to be installed into the store.
type BlobInstallSpec struct {
	// Files is the set of files to place in the blob directory. Keys are
	// the filenames inside the blob dir (e.g. "rootfs.ext4", "kernel"),
	// values are absolute paths to the source files.
	//
	// Files are renamed (moved) into the blob directory when possible
	// (same filesystem); fall back to copy for cross-device. The source
	// paths must be writable by the caller — InstallBlob will not delete
	// them on failure.
	Files map[string]string

	// Manifest is written to manifest.json after files are placed.
	Manifest Manifest
}

// InstallBlob atomically installs a new blob into the store at the digest
// recorded in spec.Manifest.Digest. If the digest is already installed,
// returns the existing path and (true, nil) for "already present".
//
// Atomicity: writes to {imagesDir}/blobs/sha256/<digest>.tmp/, fsyncs files,
// then renames the directory into place. Concurrent installers serialize
// via a flock on the destination's `.lock` file inside the algorithm
// directory.
//
// On success, all source files in spec.Files have been moved (or copied)
// into the blob directory and may no longer exist at their original paths.
func InstallBlob(imagesDir string, spec BlobInstallSpec) (blobDir string, alreadyPresent bool, err error) {
	if err := validateInstallSpec(spec); err != nil {
		return "", false, err
	}

	finalDir, err := BlobDir(imagesDir, spec.Manifest.Digest)
	if err != nil {
		return "", false, err
	}

	// Coordinate concurrent installers (and prune) via a sibling lock
	// keyed on the digest. The lock is created under algorithmDir, NOT
	// inside the blob dir itself, since the blob dir doesn't exist yet.
	algoDir := filepath.Dir(finalDir)
	if err := os.MkdirAll(algoDir, 0o755); err != nil {
		return "", false, fmt.Errorf("creating blob algorithm dir: %w", err)
	}
	hex, _ := digestHex(spec.Manifest.Digest) // already validated
	lockPath := filepath.Join(algoDir, "."+hex+".lock")
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return "", false, fmt.Errorf("locking blob %s: %w", spec.Manifest.Digest, err)
	}
	defer unlock()

	// Re-check after acquiring the lock — another process may have just
	// finished installing the same blob.
	if BlobExists(imagesDir, spec.Manifest.Digest) {
		return finalDir, true, nil
	}

	// Stage into a sibling tmp dir. Sibling-not-child guarantees the
	// rename is atomic (same parent dir) and the partial install is
	// never reachable through finalDir.
	tmpDir := finalDir + ".tmp"
	if err := os.RemoveAll(tmpDir); err != nil {
		return "", false, fmt.Errorf("clearing stale tmp blob dir: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", false, fmt.Errorf("creating tmp blob dir: %w", err)
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			os.RemoveAll(tmpDir)
		}
	}()

	for name, srcPath := range spec.Files {
		dst := filepath.Join(tmpDir, name)
		if err := moveOrCopyFile(srcPath, dst); err != nil {
			return "", false, fmt.Errorf("placing %s in blob: %w", name, err)
		}
		if err := os.Chmod(dst, 0o444); err != nil {
			return "", false, fmt.Errorf("chmod %s: %w", name, err)
		}
		if err := fsyncFile(dst); err != nil {
			return "", false, fmt.Errorf("fsync %s: %w", name, err)
		}
	}

	manifestData, err := json.MarshalIndent(spec.Manifest, "", "  ")
	if err != nil {
		return "", false, fmt.Errorf("marshalling manifest: %w", err)
	}
	manifestPath := filepath.Join(tmpDir, BlobManifestFilename)
	if err := os.WriteFile(manifestPath, manifestData, 0o444); err != nil {
		return "", false, fmt.Errorf("writing manifest: %w", err)
	}
	if err := fsyncFile(manifestPath); err != nil {
		return "", false, fmt.Errorf("fsync manifest: %w", err)
	}

	if err := fsyncDir(tmpDir); err != nil {
		return "", false, fmt.Errorf("fsync tmp blob dir: %w", err)
	}

	if err := os.Rename(tmpDir, finalDir); err != nil {
		return "", false, fmt.Errorf("renaming blob dir into place: %w", err)
	}
	cleanupTmp = false

	if err := fsyncDir(algoDir); err != nil {
		// The rename has already taken effect; if the parent fsync
		// fails we'd lose the install on crash. Surface the error;
		// caller can re-install (it's idempotent).
		return "", false, fmt.Errorf("fsync blobs dir: %w", err)
	}

	return finalDir, false, nil
}

// validateInstallSpec checks an InstallBlob input.
//
// Critically, it re-hashes the rootfs file and confirms the result
// matches spec.Manifest.Digest. The blob store's whole guarantee — that
// `blobs/sha256/<digest>/rootfs.ext4` actually has that sha256 — would
// fall apart if a caller passed a stale or wrong digest. Hashing here
// is one extra read per install; that's an acceptable cost for the
// "core invariant always holds" property.
func validateInstallSpec(spec BlobInstallSpec) error {
	if _, err := digestHex(spec.Manifest.Digest); err != nil {
		return err
	}
	if spec.Manifest.SchemaVersion == 0 {
		return fmt.Errorf("manifest schema_version must be set")
	}
	rootfsPath, ok := spec.Files[BlobRootfsFilename]
	if !ok {
		return fmt.Errorf("install spec missing required %q file", BlobRootfsFilename)
	}
	actualDigest, err := HashFile(rootfsPath)
	if err != nil {
		return fmt.Errorf("hashing rootfs %q: %w", rootfsPath, err)
	}
	if actualDigest != spec.Manifest.Digest {
		return fmt.Errorf("%w: manifest says %s but rootfs hashes to %s",
			ErrInvalidDigest, spec.Manifest.Digest, actualDigest)
	}
	return nil
}

// BlobExists reports whether the blob with the given digest is present.
func BlobExists(imagesDir, digest string) bool {
	dir, err := BlobDir(imagesDir, digest)
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, BlobRootfsFilename)); err != nil {
		return false
	}
	return true
}

// LoadManifest reads and parses manifest.json for a blob.
func LoadManifest(imagesDir, digest string) (*Manifest, error) {
	path, err := BlobManifestPath(imagesDir, digest)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, digest)
		}
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	return &m, nil
}

// ListBlobs returns all installed digests.
func ListBlobs(imagesDir string) ([]string, error) {
	dir := filepath.Join(imagesDir, blobsDir, algorithmDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading blobs dir: %w", err)
	}
	var digests []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip dotfiles (locks, tmp staging dirs, etc.)
		if strings.HasPrefix(name, ".") {
			continue
		}
		// Skip .tmp staging dirs that crashed mid-install.
		if strings.HasSuffix(name, ".tmp") {
			continue
		}
		// Validate it looks like a hex digest before reporting.
		digest := DigestPrefix + name
		if _, err := digestHex(digest); err != nil {
			continue
		}
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	return digests, nil
}

// DeleteBlob removes a blob from the store. Caller is responsible for
// refcount checks; DeleteBlob always proceeds if the digest exists.
//
// Acquires the same lock InstallBlob uses, serializing against installs
// of the same digest. Returns ErrBlobNotFound if the blob is missing.
func DeleteBlob(imagesDir, digest string) error {
	dir, err := BlobDir(imagesDir, digest)
	if err != nil {
		return err
	}
	hex, _ := digestHex(digest)
	algoDir := filepath.Dir(dir)
	lockPath := filepath.Join(algoDir, "."+hex+".lock")
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return fmt.Errorf("locking blob %s: %w", digest, err)
	}
	defer unlock()

	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrBlobNotFound, digest)
		}
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing blob: %w", err)
	}
	// Best-effort cleanup of the lock file. Removing it is safe here
	// because we still hold the open fd's flock; concurrent waiters
	// will queue on the inode's lock until our fd closes via unlock().
	_ = os.Remove(lockPath)
	return nil
}

// SetTag atomically points <tag> at <digest>. The digest does not need
// to exist (caller's responsibility — useful for record-only flows like
// recording the original ref of a snapshot whose blob has been pruned).
func SetTag(imagesDir, tag, digest string) error {
	if err := ValidateImageName(tag); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTag, err)
	}
	if _, err := digestHex(digest); err != nil {
		return err
	}

	dir := filepath.Join(imagesDir, tagsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating tags dir: %w", err)
	}

	final := filepath.Join(dir, tag+".json")
	data, err := json.MarshalIndent(Tag{
		Digest:    digest,
		UpdatedAt: time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling tag: %w", err)
	}

	// Use os.CreateTemp so concurrent SetTag callers each get their own
	// scratch file. A shared `final + ".tmp"` would let two writers
	// truncate and rename the same file, defeating the atomic-update
	// guarantee.
	tmpFile, err := os.CreateTemp(dir, "."+tag+".*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating tag tmp: %w", err)
	}
	tmp := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmp)
		return fmt.Errorf("writing tag tmp: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("closing tag tmp: %w", err)
	}
	if err := fsyncFile(tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("fsync tag tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("renaming tag into place: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("fsync tags dir: %w", err)
	}
	return nil
}

// GetTag returns the digest a tag currently points at.
// Returns ErrTagNotFound if the tag file is missing.
func GetTag(imagesDir, tag string) (Tag, error) {
	if err := ValidateImageName(tag); err != nil {
		return Tag{}, fmt.Errorf("%w: %v", ErrInvalidTag, err)
	}
	path := filepath.Join(imagesDir, tagsDir, tag+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Tag{}, fmt.Errorf("%w: %s", ErrTagNotFound, tag)
		}
		return Tag{}, fmt.Errorf("reading tag: %w", err)
	}
	var t Tag
	if err := json.Unmarshal(data, &t); err != nil {
		return Tag{}, fmt.Errorf("parsing tag: %w", err)
	}
	if _, err := digestHex(t.Digest); err != nil {
		return Tag{}, fmt.Errorf("tag %q has invalid digest: %w", tag, err)
	}
	return t, nil
}

// DeleteTag removes a tag file. Returns ErrTagNotFound if missing.
func DeleteTag(imagesDir, tag string) error {
	if err := ValidateImageName(tag); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTag, err)
	}
	path := filepath.Join(imagesDir, tagsDir, tag+".json")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrTagNotFound, tag)
		}
		return fmt.Errorf("removing tag: %w", err)
	}
	return nil
}

// ListTags returns all tag names currently present in the store.
func ListTags(imagesDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(imagesDir, tagsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading tags dir: %w", err)
	}
	var tags []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip tmp + dotfiles.
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		tags = append(tags, strings.TrimSuffix(name, ".json"))
	}
	sort.Strings(tags)
	return tags, nil
}

// moveOrCopyFile moves src to dst, falling back to copy when rename fails
// (e.g. cross-device). On a successful copy, the source is removed.
func moveOrCopyFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	// Best-effort cleanup of the source on copy fallback.
	_ = os.Remove(src)
	return nil
}

func fsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func fsyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
