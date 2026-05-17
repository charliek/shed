// Package vmimage's tag store provides shed-specific tag indirection
// over the OCI image-layout-v1 blob store defined in ocilayout.go.
//
// Tags live at {imagesDir}/tags/<name>.json and point at OCI image
// manifest digests. A tag pointing at a digest does NOT protect that
// digest from prune (Docker-style: untag with `image rm`, then
// `image prune`). Only sheds and snapshots protect blobs.
//
// This file also hosts the shared low-level primitives (digest
// validation, file locking, atomic file moves) used by ocilayout.go,
// cache.go, and manager.go.

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
	"syscall"
	"time"
)

// DigestPrefix is the algorithm prefix for image digests. Always "sha256:".
const DigestPrefix = "sha256:"

// blobsDir / tagsDir / algorithmDir are the subdirectory names under
// imagesDir. Hardcoded to sha256 since that's the only algorithm the
// on-disk layout supports.
const (
	blobsDir     = "blobs"
	tagsDir      = "tags"
	algorithmDir = "sha256"
)

// Tag represents a named pointer to an OCI manifest digest. Stored at
// tags/<name>.json.
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
		if errors.Is(err, os.ErrNotExist) {
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
		if errors.Is(err, os.ErrNotExist) {
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
		if errors.Is(err, os.ErrNotExist) {
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

// ValidateImageName validates that an image name is safe for filesystem operations.
func ValidateImageName(name string) error {
	if name == "" {
		return fmt.Errorf("image name cannot be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid image name: %q", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("invalid image name: %q (only alphanumerics, '-', '_', '.' allowed)", name)
		}
	}
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

// acquireFileLock acquires an exclusive flock on the given path.
// The lock is automatically released if the process exits or crashes.
func acquireFileLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}

	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}, nil
}

// TryAcquireFileLockBlocking takes an exclusive flock on path and blocks
// until it's available. Use this from tests that need to simulate a
// live conversion holding the lock.
func TryAcquireFileLockBlocking(path string) (func(), error) {
	return acquireFileLock(path)
}

// TryAcquireFileLock attempts a non-blocking exclusive flock on path.
// Returns held=true with a no-op release if the lock file does not exist.
func TryAcquireFileLock(path string) (release func(), held bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return func() {}, true, nil
		}
		return nil, false, err
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to acquire lock: %w", err)
	}

	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}, true, nil
}
