package vmimage

// RefKind classifies a reference protecting a blob from prune.
type RefKind string

const (
	// RefKindShed indicates a reference held by an existing shed instance.
	RefKindShed RefKind = "shed"

	// RefKindSnapshot indicates a reference held by a snapshot.
	RefKindSnapshot RefKind = "snapshot"

	// RefKindTag indicates a reference held by a tag. Tags ARE
	// protective from prune (changed in v0.5.8 — pre-v0.5.8 the prune
	// walker followed Docker's "tags don't protect" model, which made
	// `shed image pull X && shed image prune` delete the manifest
	// blob just pulled if no shed pinned it yet, leaving operators
	// with a tag pointing at a missing blob or, worse, silently
	// reverting to a stale locally-cached manifest). To delete a
	// blob a tag points at, the workflow is `shed image rm <tag>`
	// followed by `shed image prune`. See
	// docs/upgrades/v0.5.7-to-v0.5.8.md.
	RefKindTag RefKind = "tag"

	// RefKindPending indicates a reference held by an in-flight
	// `shed create`. The backend writes a `.creating` marker into the
	// instance directory between EnsureImage and meta.Save; the
	// refscanner emits a Pending ref for each fresh marker so prune
	// can't delete the blob the create is about to depend on. Fresh
	// for 1 h (see systemprune.InstanceCreatingMaxAge); stale markers
	// produce no reference and the underlying blob becomes prunable.
	RefKindPending RefKind = "pending-create"
)

// Reference describes one thing that points at a blob digest.
type Reference struct {
	// Digest is the blob digest the reference points at.
	Digest string

	// Kind is the kind of reference (shed, snapshot, tag).
	Kind RefKind

	// Name is the name of the referencing object — shed name, snapshot
	// name, or tag name. Used for human-readable error messages.
	Name string
}

// RefScanner scans the on-disk layout for references that point at blobs.
// Implementations live in the backend packages where the appropriate
// metadata directories are reachable.
//
// Implementations MUST return shed and snapshot references (the protective
// refs that block prune). Tag refs are scanned by the Manager itself.
//
// The `strict` parameter selects how malformed instance metadata is
// handled:
//
//   - strict=false (read paths — ListImages, InspectImage, DiskUsage):
//     skip the broken instance with a warning, return the rest. A
//     `shed list` or `shed system df` shouldn't fail because one
//     instance has a corrupt JSON file somewhere.
//   - strict=true (PruneImages): fail closed. Returning a partial
//     ref set from a destructive caller risks deleting a blob the
//     broken-but-recoverable shed still pinned. The caller surfaces
//     the error to the operator and tells them to fix or `rm -rf`
//     the broken instance before pruning.
//
// Sentinels for "the listing itself succeeded but a known-broken
// instance was skipped" are intentionally not part of the interface
// — implementations log a warning. Callers that want to inspect
// the skipped set should walk instances/* themselves.
type RefScanner interface {
	ScanRefs(strict bool) ([]Reference, error)
}

// ProtectiveRefs reports whether a digest has any protective
// reference — Shed, Snapshot, Pending (in-flight create), or Tag.
//
// As of v0.5.8 tag references are protective: see RefKindTag's doc
// for the rationale. RefScanner implementations still don't emit
// tag refs (tags live in the central store the Manager owns, not in
// per-backend metadata), so the Manager merges them into the ref
// list at the caller sites that need protection (notably
// PruneImages).
func ProtectiveRefs(refs []Reference, digest string) []Reference {
	var out []Reference
	for _, r := range refs {
		if r.Digest != digest {
			continue
		}
		out = append(out, r)
	}
	return out
}
