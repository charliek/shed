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
	"github.com/charliek/shed/internal/vmimage"
)

// EnsureImage ensures a resolved image is available as a local ext4 file.
// Returns the path and the digest of the underlying blob (empty when the
// resolved image is a local-path escape hatch rather than a tagged blob).
func EnsureImage(ctx context.Context, resolved config.ResolvedImage, cfg *config.VZConfig) (path, digest string, err error) {
	mgr := vmimage.NewManager(cfg, nil)
	res, err := mgr.EnsureImage(ctx, vmimage.ResolvedRef{
		Path:      resolved.Path,
		DockerRef: resolved.DockerRef,
		Name:      resolved.Name,
	}, func(stage, msg string) {
		backend.Progress(ctx, stage, msg)
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
		Manifest: toConfigManifest(*manifest),
	}, nil
}

// TagImage points newTag at the digest currently held by srcTagOrDigest.
func (c *Client) TagImage(srcTagOrDigest, newTag string) error {
	mgr := vmimage.NewManager(c.cfg, c.refScanner())
	return mapSentinelErrors(mgr.TagImage(srcTagOrDigest, newTag))
}

// PullImage pulls a Docker ref, installs into the blob store, and tags.
func (c *Client) PullImage(ctx context.Context, dockerRef, tag string) (string, error) {
	mgr := vmimage.NewManager(c.cfg, c.refScanner())
	return mgr.PullImage(ctx, dockerRef, tag, func(stage, msg string) {
		backend.Progress(ctx, stage, msg)
	})
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

func (s *vzRefScanner) ScanRefs() ([]vmimage.Reference, error) {
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
			return nil, fmt.Errorf("reading metadata for %s: %w", inst, err)
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
				log.Printf("Warning: skipping snapshot %q during ref scan: %v", name, err)
				continue
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

// toConfigManifest copies a vmimage.Manifest into the wire shape.
func toConfigManifest(m vmimage.Manifest) config.ImageManifest {
	return config.ImageManifest{
		SchemaVersion:      m.SchemaVersion,
		Digest:             m.Digest,
		Backend:            m.Backend,
		Arch:               m.Arch,
		SourceRef:          m.SourceRef,
		SourceRefDigest:    m.SourceRefDigest,
		ShedExtVersion:     m.ShedExtVersion,
		KernelSize:         m.KernelSize,
		InitrdSize:         m.InitrdSize,
		RootfsLogicalSize:  m.RootfsLogicalSize,
		RootfsPhysicalSize: m.RootfsPhysicalSize,
		CreatedAt:          m.CreatedAt,
	}
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
