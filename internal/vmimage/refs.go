package vmimage

// RefKind classifies a reference protecting a blob from prune.
type RefKind string

const (
	// RefKindShed indicates a reference held by an existing shed instance.
	RefKindShed RefKind = "shed"

	// RefKindSnapshot indicates a reference held by a snapshot.
	RefKindSnapshot RefKind = "snapshot"

	// RefKindTag indicates a reference held by a tag. Tags are
	// informational and DO NOT protect a digest from prune (Docker model).
	RefKindTag RefKind = "tag"
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
type RefScanner interface {
	// ScanRefs returns shed + snapshot refs.
	ScanRefs() ([]Reference, error)
}

// ProtectiveRefs reports whether a digest has any shed or snapshot
// references — i.e. is the digest pinned by something prune must protect.
//
// Tag references are NOT protective and are excluded from this check.
func ProtectiveRefs(refs []Reference, digest string) []Reference {
	var out []Reference
	for _, r := range refs {
		if r.Digest != digest {
			continue
		}
		if r.Kind == RefKindTag {
			continue
		}
		out = append(out, r)
	}
	return out
}
