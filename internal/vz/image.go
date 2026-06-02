//go:build darwin

package vz

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/systemprune"
	"github.com/charliek/shed/internal/vmimage"
)

// EnsureImage ensures a resolved image is available as a local ext4 file.
// Returns the path and the digest of the underlying blob (empty when the
// resolved image is a local-path escape hatch rather than a tagged blob).
//
// Method on Client (not a free function) so the manager carries a real
// RefScanner — matches the Firecracker create path and keeps EnsureImage
// behavior symmetric across backends.
func (c *Client) EnsureImage(ctx context.Context, resolved config.ResolvedImage) (path, digest string, err error) {
	mgr := vmimage.NewManager(c.cfg, c.refScanner())
	res, err := mgr.EnsureImage(ctx, vmimage.ResolvedRef{
		Path:      resolved.Path,
		DockerRef: resolved.DockerRef,
		Name:      resolved.Name,
		Digest:    resolved.Digest,
		Policy:    vmimage.PullPolicy(resolved.PullPolicy),
	}, func(stage, msg string) {
		backend.Phase(ctx, stage)
		backend.Status(ctx, msg)
	})
	if err != nil {
		return "", "", err
	}
	return res.Path, res.Digest, nil
}

// ListImages returns available image variants from config and the blob store.
func (c *Client) ListImages() ([]config.ImageInfo, error) {
	mgr := vmimage.NewManager(c.cfg, c.refScanner())
	images, err := mgr.ListImages()
	if err != nil {
		return nil, err
	}
	return toConfigImageInfos(images), nil
}

// InspectImage returns full details for a tag or digest.
func (c *Client) InspectImage(tagOrDigest string) (config.ImageInspectResponse, error) {
	mgr := vmimage.NewManager(c.cfg, c.refScanner())
	info, manifest, err := mgr.InspectImage(tagOrDigest)
	if err != nil {
		return config.ImageInspectResponse{}, mapSentinelErrors(err)
	}
	return config.ImageInspectResponse{
		Image:    toConfigImageInfo(*info),
		Manifest: toConfigManifest(info.Digest, *manifest),
	}, nil
}

// TagImage points newTag at the digest currently held by srcTagOrDigest.
func (c *Client) TagImage(srcTagOrDigest, newTag string) error {
	mgr := vmimage.NewManager(c.cfg, c.refScanner())
	return mapSentinelErrors(mgr.TagImage(srcTagOrDigest, newTag))
}

// PullImage pulls a Docker ref, installs into the blob store, and tags.
// platform is an optional override (e.g. "linux/arm64"); empty means
// the backend's native platform.
func (c *Client) PullImage(ctx context.Context, dockerRef, tag, platform string) (string, error) {
	mgr := vmimage.NewManager(c.cfg, c.refScanner())
	return mgr.PullImage(ctx, dockerRef, tag, platform, func(stage, msg string) {
		backend.Phase(ctx, stage)
		backend.Status(ctx, msg)
	})
}

// PushImage uploads the manifest currently held by tagOrDigest to the
// destination registry ref. Byte-perfect: layer bytes flow from the
// on-disk OCI store.
func (c *Client) PushImage(ctx context.Context, tagOrDigest, dstRef string) error {
	mgr := vmimage.NewManager(c.cfg, c.refScanner())
	return mapSentinelErrors(mgr.PushImage(ctx, tagOrDigest, dstRef, func(stage, msg string) {
		backend.Phase(ctx, stage)
		backend.Status(ctx, msg)
	}))
}

// DeleteImage removes a tag (Docker model). The underlying blob is GC'd
// by PruneImages once nothing references it.
func (c *Client) DeleteImage(name string) error {
	mgr := vmimage.NewManager(c.cfg, c.refScanner())
	return mapSentinelErrors(mgr.DeleteImage(name))
}

// PruneImages removes blobs not protected by any shed/snapshot ref.
func (c *Client) PruneImages(dryRun bool) ([]config.ImageInfo, error) {
	mgr := vmimage.NewManager(c.cfg, c.refScanner())
	images, err := mgr.PruneImages(dryRun)
	if err != nil {
		return nil, err
	}
	return toConfigImageInfos(images), nil
}

// refScanner returns a RefScanner that walks instances and snapshots and
// emits the LowerDigest references that protect blobs from prune.
func (c *Client) refScanner() vmimage.RefScanner {
	return &vzRefScanner{cfg: c.cfg}
}

// refScannerExcept is like refScanner but skips sheds named in the
// provided set. Used by `shed system prune` dry-run to simulate the
// post-instance-delete state for image GC.
func (c *Client) refScannerExcept(skipSheds map[string]bool) vmimage.RefScanner {
	return &vzRefScanner{cfg: c.cfg, skipSheds: skipSheds}
}

type vzRefScanner struct {
	cfg       *config.VZConfig
	skipSheds map[string]bool
}

func (s *vzRefScanner) ScanRefs(strict bool) ([]vmimage.Reference, error) {
	var refs []vmimage.Reference

	instances, err := ListInstances(s.cfg.InstanceDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("listing instances: %w", err)
	}
	for _, inst := range instances {
		if s.skipSheds[inst] {
			continue
		}
		meta, err := LoadMetadata(s.cfg.InstanceDir, inst)
		if err != nil {
			if errors.Is(err, ErrInstanceNotFound) || errors.Is(err, ErrLegacyMetadata) {
				continue
			}
			// strict=true (prune): fail closed. A broken metadata could
			// be pinning the only protective reference to a blob the
			// operator might want to recover; let prune refuse and
			// point the operator at the broken dir.
			if strict {
				return nil, fmt.Errorf("invalid metadata on shed %q: %w (remove the directory at %s/%s/ before retrying)", inst, err, s.cfg.InstanceDir, inst)
			}
			// strict=false (read paths): warn-and-skip. Matches ListSheds
			// and DiskUsage so one corrupt instance doesn't break the
			// whole listing.
			log.Printf("Warning: skipping shed %q with invalid metadata during ref scan: %v", inst, err)
			continue
		}
		if meta.LowerDigest != "" {
			refs = append(refs, vmimage.Reference{
				Digest: meta.LowerDigest,
				Kind:   vmimage.RefKindShed,
				Name:   meta.Name,
			})
		}
	}

	if s.cfg.SnapshotsDir != "" {
		names, err := listSnapshotNames(s.cfg.SnapshotsDir)
		if err != nil {
			return nil, fmt.Errorf("listing snapshots: %w", err)
		}
		for _, name := range names {
			snap, err := loadSnapshot(s.cfg.SnapshotsDir, name)
			if err != nil {
				// Fail-closed: if we can't read a snapshot, we don't
				// know which lower digest it pins, and silently
				// skipping would let prune delete a blob the
				// snapshot still references.
				return nil, fmt.Errorf("reading snapshot %s during ref scan: %w", name, err)
			}
			if snap.LowerDigest != "" {
				refs = append(refs, vmimage.Reference{
					Digest: snap.LowerDigest,
					Kind:   vmimage.RefKindSnapshot,
					Name:   snap.Name,
				})
			}
		}
	}

	// In-flight creates. The .creating marker carries the lower
	// digest the create is about to use; treat as protective for up
	// to InstanceCreatingMaxAge so a concurrent `image prune`
	// can't delete the blob between EnsureImage and meta.Save.
	pending, err := systemprune.ScanInstanceCreatingMarkers(s.cfg.InstanceDir)
	if err != nil {
		return nil, fmt.Errorf("scanning in-flight create markers: %w", err)
	}
	for _, p := range pending {
		if s.skipSheds[p.ShedName] {
			continue
		}
		refs = append(refs, vmimage.Reference{
			Digest: p.LowerDigest,
			Kind:   vmimage.RefKindPending,
			Name:   p.ShedName,
		})
	}

	return refs, nil
}

// toConfigImageInfo copies a single vmimage.ImageInfo into the wire shape.
func toConfigImageInfo(img vmimage.ImageInfo) config.ImageInfo {
	return config.ImageInfo{
		Name:      img.Name,
		Path:      img.Path,
		DockerRef: img.DockerRef,
		SizeBytes: img.SizeBytes,
		Source:    img.Source,
		Cached:    img.Cached,
		Digest:    img.Digest,
		Tag:       img.Tag,
		InUse:     img.InUse,
	}
}

// toConfigImageInfos converts vmimage.ImageInfo slice to config.ImageInfo slice.
func toConfigImageInfos(images []vmimage.ImageInfo) []config.ImageInfo {
	result := make([]config.ImageInfo, len(images))
	for i, img := range images {
		result[i] = toConfigImageInfo(img)
	}
	return result
}

// toConfigManifest copies a vmimage.OCIManifest into the wire shape.
// digest is the manifest's content digest (the same value that's
// recorded in tags/<name>.json).
func toConfigManifest(digest string, m vmimage.OCIManifest) config.ImageManifest {
	out := config.ImageManifest{
		Digest:        digest,
		SchemaVersion: m.SchemaVersion,
		MediaType:     m.MediaType,
		Config: config.ImageDescriptor{
			MediaType: m.Config.MediaType,
			Digest:    m.Config.Digest,
			Size:      m.Config.Size,
		},
		Annotations:  m.Annotations,
		SourceRef:    m.ShedSourceRef(),
		Variant:      m.ShedVariant(),
		KernelDigest: m.ShedKernelDigest(),
		InitrdDigest: m.ShedInitrdDigest(),
	}
	for _, layer := range m.Layers {
		out.Layers = append(out.Layers, config.ImageDescriptor{
			MediaType:   layer.MediaType,
			Digest:      layer.Digest,
			Size:        layer.Size,
			Annotations: layer.Annotations,
		})
	}
	return out
}

// mapSentinelErrors maps vmimage sentinel errors to config sentinel errors.
func mapSentinelErrors(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, vmimage.ErrImageNotFound) || errors.Is(err, vmimage.ErrTagNotFound) || errors.Is(err, vmimage.ErrBlobNotFound) {
		return config.ErrImageNotFoundSentinel
	}
	if errors.Is(err, vmimage.ErrImageInUse) {
		return config.ErrImageInUseSentinel
	}
	return err
}
