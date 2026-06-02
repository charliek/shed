package vmimage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PullPolicy controls how EnsureImage reconciles a configured Docker ref
// against the local content-addressed store at create time. It mirrors the
// Docker/Podman vocabulary so existing operator intuition transfers.
type PullPolicy string

const (
	// PullMissing uses the cached ref if present, pulling only when absent.
	// This is the default and keeps the create hot path O(1) and offline.
	PullMissing PullPolicy = "missing"
	// PullAlways always contacts the registry and pulls, bypassing the cache.
	PullAlways PullPolicy = "always"
	// PullNever uses the cached ref if present and errors on a miss; it never
	// contacts the registry.
	PullNever PullPolicy = "never"
)

// ErrPullDisabled is returned when pull_policy=never and the configured ref
// is not present in the local store. Callers should surface this as a
// client-actionable error (4xx), not an internal failure.
var ErrPullDisabled = errors.New("image not present locally and pull_policy=never")

// ParsePullPolicy validates a configured pull_policy string. An empty value
// defaults to PullMissing.
func ParsePullPolicy(s string) (PullPolicy, error) {
	switch PullPolicy(s) {
	case "":
		return PullMissing, nil
	case PullMissing, PullAlways, PullNever:
		return PullPolicy(s), nil
	default:
		return "", fmt.Errorf("invalid pull_policy %q (want one of: missing, always, never)", s)
	}
}

// DeriveTagFromRef reduces a Docker ref to a short, filesystem-safe cosmetic
// name (e.g. ghcr.io/charliek/shed-vz-full:v0.5.9 -> "full"). It is NOT a
// unique identity — multiple ref versions collapse to the same name — so it
// is only used as a lock-adjacent label. Resolution identity is the full ref
// via the ref-index.
func DeriveTagFromRef(ref string) string {
	name := ref
	if i := strings.LastIndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		if !strings.Contains(name[i:], "/") {
			name = name[:i]
		}
	}
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	for _, prefix := range []string{"shed-vz-", "shed-fc-"} {
		if strings.HasPrefix(name, prefix) {
			name = strings.TrimPrefix(name, prefix)
			break
		}
	}
	if name == "" {
		name = "default"
	}
	return name
}

const refIndexSubdir = "refs"

type refIndexEntry struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

func refIndexKey(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])
}

func refIndexPath(imagesDir, ref string) string {
	return filepath.Join(imagesDir, refIndexSubdir, refIndexKey(ref)+".json")
}

// RefIndexPut records ref -> manifest digest in the sidecar ref-index. It is
// the final commit step of a successful pull/build: callers MUST invoke it
// only after the manifest, config, layers, kernel/initrd/erofs, and
// index.json are durable, so a crash can never leave the index pointing at
// an incomplete digest. The write itself is atomic (temp+rename).
func RefIndexPut(imagesDir, ref, digest string) error {
	if imagesDir == "" || ref == "" || digest == "" {
		return fmt.Errorf("RefIndexPut: imagesDir, ref, and digest are required")
	}
	dir := filepath.Join(imagesDir, refIndexSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating ref-index dir: %w", err)
	}
	data, err := json.Marshal(refIndexEntry{Ref: ref, Digest: digest})
	if err != nil {
		return err
	}
	if err := atomicWriteFile(refIndexPath(imagesDir, ref), data, 0o644); err != nil {
		return err
	}
	return fsyncDir(dir)
}

// RefIndexGet returns the cached manifest digest for ref as a VALIDATED hit:
// the sidecar entry exists, its stored ref matches the lookup ref (guarding
// against a sha256 key collision), and the manifest blob is still present. On
// any validation failure it removes the stale entry and reports a miss
// (ok=false), so the caller's pull_policy then governs.
//
// Identity here is the ref the image was PULLED BY (the index key), NOT the
// manifest's io.shed.source-ref annotation — those legitimately differ when
// the configured ref is a mutable tag (:latest), a digest pin, or a mirror
// host, while the annotation records the immutable publish ref. Validating
// against the annotation would delete the entry EnsureImage just wrote.
//
// Blob-completeness beyond the manifest digest (erofs/kernel/initrd) is
// validated by resolveManifestLower at the call site.
func RefIndexGet(imagesDir, ref string) (digest string, ok bool) {
	if imagesDir == "" || ref == "" {
		return "", false
	}
	path := refIndexPath(imagesDir, ref)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var entry refIndexEntry
	if err := json.Unmarshal(data, &entry); err != nil || entry.Digest == "" || entry.Ref != ref {
		_ = os.Remove(path)
		return "", false
	}
	if !BlobExists(imagesDir, entry.Digest) {
		_ = os.Remove(path)
		return "", false
	}
	return entry.Digest, true
}

// FindDigestBySourceRef scans installed manifests for one whose
// io.shed.source-ref equals ref, returning its digest. This is a READ-ONLY,
// O(N-manifests) fallback for the cold paths (rm, prune protection) where a
// ref may not have a sidecar entry yet (e.g. images present from before the
// ref-index existed). It deliberately does NOT write the sidecar: the create
// hot path resolves sidecar-only, so a manifest left behind by `shed image
// rm` (blob persists until prune, Docker model) must not be silently
// re-cached for create — `rm` then `create` should re-pull.
func FindDigestBySourceRef(imagesDir, ref string) (digest string, ok bool) {
	if imagesDir == "" || ref == "" {
		return "", false
	}
	indexed, err := IndexManifestDigests(imagesDir)
	if err != nil {
		return "", false
	}
	for d := range indexed {
		if !BlobExists(imagesDir, d) {
			continue
		}
		manifest, err := LoadManifestByDigest(imagesDir, d)
		if err != nil {
			continue
		}
		if manifest.ShedSourceRef() == ref {
			return d, true
		}
	}
	return "", false
}

// RefIndexReverse returns a digest -> ref map built from all sidecar entries
// (first entry wins when several refs share a digest). Read-only; used by
// `ls`/`inspect` so an image is displayed by the ref it was pulled by, not the
// manifest's publish-time io.shed.source-ref (which differs for mutable tags,
// digest pins, and mirrors).
func RefIndexReverse(imagesDir string) map[string]string {
	out := map[string]string{}
	dir := filepath.Join(imagesDir, refIndexSubdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var entry refIndexEntry
		if err := json.Unmarshal(data, &entry); err != nil || entry.Digest == "" || entry.Ref == "" {
			continue
		}
		// Skip entries whose manifest blob is gone so a broken/half-pruned
		// image isn't displayed under a stale ref.
		if !BlobExists(imagesDir, entry.Digest) {
			continue
		}
		if _, ok := out[entry.Digest]; !ok {
			out[entry.Digest] = entry.Ref
		}
	}
	return out
}

// RefIndexDeleteByDigest removes every ref-index entry pointing at digest.
// Called by prune/rm after a manifest blob is deleted so a later resolve
// can't hit a dangling entry. Best-effort: scan errors are logged, not fatal.
func RefIndexDeleteByDigest(imagesDir, digest string) {
	dir := filepath.Join(imagesDir, refIndexSubdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var entry refIndexEntry
		if err := json.Unmarshal(data, &entry); err != nil {
			continue
		}
		if entry.Digest == digest {
			_ = os.Remove(p)
		}
	}
}
