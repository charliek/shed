package systemprune

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// InstanceCreatingMarker is the filename written into an instance
// directory while CreateShed is in flight, between `EnsureImage`
// returning a digest and `meta.Save` recording the protective
// reference. Its presence keeps `shed image prune` from deleting the
// blob the in-flight create is about to use — the marker's body is
// the literal lower digest, treated as a protective reference by the
// refscanner.
const InstanceCreatingMarker = ".creating"

// InstanceCreatingMaxAge bounds how long a `.creating` marker is
// honored before the refscanner treats it as crash residue. A correct
// CreateShed (including a full image pull + convert) completes well
// under this threshold; anything older is taken to mean the process
// was killed mid-create and the marker should no longer protect.
//
// Tuned to 1 h: even a slow Docker pull + ext4 conversion finishes in
// 10-20 minutes in practice; 1 h gives plenty of headroom while
// keeping the post-crash window short enough that prune isn't pinned
// for a full day waiting for a marker to expire.
const InstanceCreatingMaxAge = 1 * time.Hour

// PendingCreateRef describes a protective reference recorded by an
// in-flight CreateShed. The refscanner emits one of these per fresh
// `.creating` marker so prune sees the lower digest as referenced.
type PendingCreateRef struct {
	// ShedName is the instance directory name carrying the marker.
	ShedName string
	// LowerDigest is the body of the marker file (a "sha256:..." string).
	LowerDigest string
}

// ScanInstanceCreatingMarkers walks instanceDir for `.creating`
// markers whose mtime is within InstanceCreatingMaxAge of now, and
// returns the lower digests they record. The refscanner adds these
// to its protective-reference set so a concurrent `image prune`
// can't sweep a blob a create is about to depend on.
//
// Stale markers (mtime older than InstanceCreatingMaxAge) are
// skipped: they represent crashed creates whose protection has
// expired. Malformed marker bodies (empty file, no "sha256:"
// prefix) are skipped silently — a marker that doesn't name a
// digest can't protect one.
//
// Returns nil if instanceDir doesn't exist (server hasn't created
// any sheds yet). Read errors on the dir itself are returned so the
// refscanner can fail closed rather than silently dropping
// protection.
func ScanInstanceCreatingMarkers(instanceDir string) ([]PendingCreateRef, error) {
	entries, err := os.ReadDir(instanceDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading instance dir: %w", err)
	}

	cutoff := time.Now().Add(-InstanceCreatingMaxAge)
	var refs []PendingCreateRef
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		markerPath := filepath.Join(instanceDir, entry.Name(), InstanceCreatingMarker)
		info, err := os.Stat(markerPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			// Permission/IO error on the marker itself: skip it
			// rather than fail-closing the whole scan. The shed dir
			// could still have a metadata.json that the refscanner
			// will read separately.
			continue
		}
		if info.ModTime().Before(cutoff) {
			continue // stale marker; protection expired
		}

		body, err := os.ReadFile(markerPath)
		if err != nil {
			continue
		}
		digest := strings.TrimSpace(string(body))
		if !strings.HasPrefix(digest, "sha256:") {
			continue // malformed marker; no digest to protect
		}

		refs = append(refs, PendingCreateRef{
			ShedName:    entry.Name(),
			LowerDigest: digest,
		})
	}
	return refs, nil
}
