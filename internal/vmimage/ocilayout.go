// OCI image-layout-v1 store on disk.
//
// Layout under {imagesDir}:
//
//	oci-layout                          {"imageLayoutVersion":"1.0.0"}
//	index.json                          OCI image-index, lists installed manifests
//	blobs/sha256/<hex>                  FILE: OCI blob (manifest / config / layer tar.gz / kernel / initrd)
//	cache/sha256/<hex>.ext4             FILE: derived ext4 for the layer with that digest (not part of the OCI store)
//	tags/<name>.json                    shed-specific tag indirection ({"digest":"sha256:...","updated_at":"..."})
//	uppers/<shed>/upper.ext4            per-shed writable upper (unchanged)
//	instances/<shed>/metadata.json      per-shed metadata (unchanged)
//	snapshots/...                       (unchanged)
//
// `crane validate --remote=false {imagesDir}` should accept the layout —
// the shed-specific `tags/` and `cache/` directories are siblings, never
// inside `blobs/`, so foreign tools see a vanilla OCI image layout.

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
	"strings"
)

const (
	ociLayoutFile = "oci-layout"
	ociIndexFile  = "index.json"
)

// EnsureOCILayout creates the OCI image-layout markers at imagesDir
// if they don't already exist. Idempotent.
func EnsureOCILayout(imagesDir string) error {
	if imagesDir == "" {
		return errors.New("imagesDir is empty")
	}
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		return fmt.Errorf("creating images dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(imagesDir, blobsDir, algorithmDir), 0o755); err != nil {
		return fmt.Errorf("creating blobs dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(imagesDir, "cache", algorithmDir), 0o755); err != nil {
		return fmt.Errorf("creating cache dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(imagesDir, tagsDir), 0o755); err != nil {
		return fmt.Errorf("creating tags dir: %w", err)
	}

	markerPath := filepath.Join(imagesDir, ociLayoutFile)
	if _, err := os.Stat(markerPath); errors.Is(err, os.ErrNotExist) {
		marker, _ := json.Marshal(OCILayoutMarker{ImageLayoutVersion: CurrentOCILayoutVersion})
		if err := atomicWriteFile(markerPath, marker, 0o644); err != nil {
			return fmt.Errorf("writing oci-layout: %w", err)
		}
	}

	indexPath := filepath.Join(imagesDir, ociIndexFile)
	if _, err := os.Stat(indexPath); errors.Is(err, os.ErrNotExist) {
		idx := &OCIIndex{}
		data, err := idx.MarshalIndent()
		if err != nil {
			return fmt.Errorf("marshalling empty index: %w", err)
		}
		if err := atomicWriteFile(indexPath, data, 0o644); err != nil {
			return fmt.Errorf("writing index.json: %w", err)
		}
	}

	return nil
}

// BlobPath returns the on-disk path of a blob keyed by digest.
// digest must be of the form "sha256:<hex>".
func BlobPath(imagesDir, digest string) (string, error) {
	hex, err := digestHex(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(imagesDir, blobsDir, algorithmDir, hex), nil
}

// BlobExists reports whether a blob is present in the store.
func BlobExists(imagesDir, digest string) bool {
	path, err := BlobPath(imagesDir, digest)
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return true
}

// BlobSize returns the on-disk size of a blob, or 0 if missing.
func BlobSize(imagesDir, digest string) int64 {
	path, err := BlobPath(imagesDir, digest)
	if err != nil {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// WriteBlob installs `data` as a blob in the store. The provided
// expectedDigest is verified against sha256(data). Returns the blob path.
// Idempotent: if the blob is already present, returns success without
// rewriting.
func WriteBlob(imagesDir, expectedDigest string, data []byte) (string, error) {
	if err := EnsureOCILayout(imagesDir); err != nil {
		return "", err
	}
	hex, err := digestHex(expectedDigest)
	if err != nil {
		return "", err
	}
	actual := DigestBytes(data)
	if actual != expectedDigest {
		return "", fmt.Errorf("%w: provided %s but content hashes to %s",
			ErrInvalidDigest, expectedDigest, actual)
	}
	path := filepath.Join(imagesDir, blobsDir, algorithmDir, hex)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	lockPath := filepath.Join(imagesDir, blobsDir, algorithmDir, "."+hex+".lock")
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return "", fmt.Errorf("locking blob %s: %w", expectedDigest, err)
	}
	defer unlock()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := atomicWriteFile(path, data, 0o444); err != nil {
		return "", fmt.Errorf("writing blob %s: %w", expectedDigest, err)
	}
	return path, nil
}

// WriteBlobFromFile installs the contents of srcPath as a blob in the
// store. The STAGED bytes are hashed (not the source) so a concurrent
// writer to srcPath can't end up with the staged content stored under
// a stale digest. If consume is true, srcPath is renamed into the
// staging path (saving a copy); otherwise it is copied and srcPath is
// preserved.
func WriteBlobFromFile(imagesDir, srcPath string, consume bool) (digest, blobPath string, err error) {
	if err := EnsureOCILayout(imagesDir); err != nil {
		return "", "", err
	}

	// Stage first under a non-content-addressed temp name.
	algoDir := filepath.Join(imagesDir, blobsDir, algorithmDir)
	if err := os.MkdirAll(algoDir, 0o755); err != nil {
		return "", "", fmt.Errorf("creating blobs dir: %w", err)
	}
	tmpFile, err := os.CreateTemp(algoDir, ".staging-*.tmp")
	if err != nil {
		return "", "", fmt.Errorf("creating tmp blob: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			os.Remove(tmpPath)
		}
	}()

	if consume {
		if err := moveOrCopyFile(srcPath, tmpPath); err != nil {
			return "", "", fmt.Errorf("staging blob: %w", err)
		}
	} else {
		if err := copyFile(srcPath, tmpPath); err != nil {
			return "", "", fmt.Errorf("staging blob: %w", err)
		}
	}

	// Hash the staged bytes (not srcPath) — that's what's actually
	// going to live under blobs/sha256/<hex>.
	digest, err = HashFile(tmpPath)
	if err != nil {
		return "", "", fmt.Errorf("hashing staged blob: %w", err)
	}
	hex, _ := digestHex(digest)
	final := filepath.Join(algoDir, hex)
	if _, err := os.Stat(final); err == nil {
		// Already installed; staged copy is redundant.
		return digest, final, nil
	}
	lockPath := filepath.Join(algoDir, "."+hex+".lock")
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return "", "", fmt.Errorf("locking blob %s: %w", digest, err)
	}
	defer unlock()
	if _, err := os.Stat(final); err == nil {
		return digest, final, nil
	}
	if err := os.Chmod(tmpPath, 0o444); err != nil {
		return "", "", fmt.Errorf("chmod tmp blob: %w", err)
	}
	if err := fsyncFile(tmpPath); err != nil {
		return "", "", fmt.Errorf("fsync tmp blob: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return "", "", fmt.Errorf("renaming blob into place: %w", err)
	}
	cleanupTmp = false
	if err := fsyncDir(algoDir); err != nil {
		return "", "", fmt.Errorf("fsync blobs dir: %w", err)
	}
	return digest, final, nil
}

// ReadBlob reads a blob's bytes. Returns ErrBlobNotFound if missing.
func ReadBlob(imagesDir, digest string) ([]byte, error) {
	path, err := BlobPath(imagesDir, digest)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, digest)
		}
		return nil, fmt.Errorf("reading blob %s: %w", digest, err)
	}
	return data, nil
}

// OpenBlob opens a blob for streaming reads. Returns ErrBlobNotFound if missing.
func OpenBlob(imagesDir, digest string) (*os.File, error) {
	path, err := BlobPath(imagesDir, digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, digest)
		}
		return nil, fmt.Errorf("opening blob %s: %w", digest, err)
	}
	return f, nil
}

// DeleteBlob removes a blob from the store. Caller is responsible for
// refcount checks. Returns ErrBlobNotFound if missing.
func DeleteBlob(imagesDir, digest string) error {
	path, err := BlobPath(imagesDir, digest)
	if err != nil {
		return err
	}
	hex, _ := digestHex(digest)
	lockPath := filepath.Join(imagesDir, blobsDir, algorithmDir, "."+hex+".lock")
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return fmt.Errorf("locking blob %s: %w", digest, err)
	}
	defer unlock()

	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrBlobNotFound, digest)
		}
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("removing blob: %w", err)
	}
	// Intentionally NOT unlinking lockPath here — doing so before the
	// deferred unlock() runs would let a concurrent caller create a
	// fresh lock file on a different inode and acquire it while we
	// still hold flock() on the old inode, allowing overlapping
	// delete/write operations.
	return nil
}

// ListBlobs returns all installed blob digests.
func ListBlobs(imagesDir string) ([]string, error) {
	dir := filepath.Join(imagesDir, blobsDir, algorithmDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading blobs dir: %w", err)
	}
	var digests []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(name, ".tmp") {
			continue
		}
		digest := DigestPrefix + name
		if _, err := digestHex(digest); err != nil {
			continue
		}
		digests = append(digests, digest)
	}
	return digests, nil
}

// ReadIndex returns the parsed top-level OCI index.
func ReadIndex(imagesDir string) (*OCIIndex, error) {
	path := filepath.Join(imagesDir, ociIndexFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &OCIIndex{}, nil
		}
		return nil, fmt.Errorf("reading index.json: %w", err)
	}
	return ParseIndex(data)
}

// IndexManifestDigests reads index.json and returns the set of
// manifest digests recorded there. This is the cheap way to discover
// which blobs are manifests without probe-reading every blob.
//
// Returns an empty set if index.json is missing or unreadable —
// callers should treat that as "no known manifests" and may fall back
// to a more expensive scan if they require completeness.
func IndexManifestDigests(imagesDir string) (map[string]bool, error) {
	idx, err := ReadIndex(imagesDir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(idx.Manifests))
	for _, m := range idx.Manifests {
		if m.Digest == "" {
			continue
		}
		out[m.Digest] = true
	}
	return out, nil
}

// IndexRemoveByDigest drops a manifest entry from index.json. Called by
// PruneImages after deleting a manifest blob so foreign tools don't see
// a stale descriptor pointing at a missing blob.
func IndexRemoveByDigest(imagesDir, digest string) error {
	lockPath := filepath.Join(imagesDir, ".index.lock")
	unlock, err := acquireFileLock(lockPath)
	if err != nil {
		return fmt.Errorf("locking index: %w", err)
	}
	defer unlock()

	idx, err := ReadIndex(imagesDir)
	if err != nil {
		return err
	}
	out := idx.Manifests[:0]
	for _, m := range idx.Manifests {
		if m.Digest == digest {
			continue
		}
		out = append(out, m)
	}
	idx.Manifests = out
	return WriteIndex(imagesDir, idx)
}

// WriteIndex atomically writes the top-level OCI index.
func WriteIndex(imagesDir string, idx *OCIIndex) error {
	if err := EnsureOCILayout(imagesDir); err != nil {
		return err
	}
	data, err := idx.MarshalIndent()
	if err != nil {
		return fmt.Errorf("marshalling index: %w", err)
	}
	return atomicWriteFile(filepath.Join(imagesDir, ociIndexFile), data, 0o644)
}

// DigestBytes computes the sha256 digest of an in-memory byte slice,
// formatted as "sha256:<hex>".
func DigestBytes(data []byte) string {
	h := sha256.Sum256(data)
	return DigestPrefix + hex.EncodeToString(h[:])
}

// DigestReader streams a reader through sha256, returning the digest
// of the consumed bytes. Reader is fully drained.
func DigestReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return DigestPrefix + hex.EncodeToString(h.Sum(nil)), n, nil
}

// atomicWriteFile writes data to path atomically via tmp + rename.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Chmod(mode); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// copyFile copies src to dst.
func copyFile(src, dst string) error {
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
	return out.Close()
}
