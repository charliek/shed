//go:build darwin

package vz

import (
	"context"
	"errors"
	"os"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

// EnsureImage ensures a resolved image is available as a local ext4 file.
// If the image is already a local path, it returns that path directly.
// If it's a Docker reference, it pulls and converts to ext4, caching the result.
func EnsureImage(ctx context.Context, resolved config.ResolvedImage, cfg *config.VZConfig) (string, error) {
	mgr := vmimage.NewManager(cfg)
	return mgr.EnsureImage(ctx, vmimage.ResolvedRef{
		Path:      resolved.Path,
		DockerRef: resolved.DockerRef,
		Name:      resolved.Name,
	}, func(stage, msg string) {
		backend.Progress(ctx, stage, msg)
	})
}

// ListImages returns available image variants from config and auto-discovery in ImagesDir.
func (c *Client) ListImages() ([]config.ImageInfo, error) {
	mgr := vmimage.NewManager(c.cfg)
	images, err := mgr.ListImages()
	if err != nil {
		return nil, err
	}
	return toConfigImageInfos(images), nil
}

// DeleteImage removes a cached image by name.
// It deletes the ext4 rootfs and source sidecar but NOT the lock file.
func (c *Client) DeleteImage(name string) error {
	mgr := vmimage.NewManager(c.cfg)
	err := mgr.DeleteImage(name, c.inUseImageNames)
	return mapSentinelErrors(err)
}

// PruneImages removes cached images not referenced by config or existing sheds.
// If dryRun is true, returns candidates without deleting.
func (c *Client) PruneImages(dryRun bool) ([]config.ImageInfo, error) {
	mgr := vmimage.NewManager(c.cfg)
	images, err := mgr.PruneImages(dryRun, c.inUseImageNames)
	if err != nil {
		return nil, err
	}
	return toConfigImageInfos(images), nil
}

// inUseImageNames returns image names referenced by existing VZ instances.
func (c *Client) inUseImageNames() ([]string, error) {
	return c.inUseImageNamesExcept(nil)
}

// inUseImageNamesExcept returns image names referenced by existing VZ
// instances whose names are NOT in skipSheds. Used by Prune's dry-run path
// to simulate the post-instance-delete state so image candidates reflect
// the fleet after instance deletions.
//
// Malformed metadata on a single instance is treated as "cannot confirm
// image usage" — we skip-and-warn rather than failing the whole scan,
// matching ListSheds' behavior. The upstream Manager.PruneImages still
// fails-closed when its closure errors, so the protection is intact:
// we only contribute names we could verify.
func (c *Client) inUseImageNamesExcept(skipSheds map[string]bool) ([]string, error) {
	instances, err := ListInstances(c.cfg.InstanceDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var names []string
	for _, inst := range instances {
		if skipSheds[inst] {
			continue
		}
		meta, err := LoadMetadata(c.cfg.InstanceDir, inst)
		if err != nil {
			// Only the race-condition case (instance removed between
			// ListInstances and LoadMetadata) is safe to skip silently.
			// Any other error (malformed JSON, I/O failure, permission
			// denied) means we cannot confirm in-use images and must
			// fail closed, not fail open — otherwise prune might
			// reclaim an image that's actually referenced.
			if errors.Is(err, ErrInstanceNotFound) {
				continue
			}
			return nil, err
		}
		if meta.Image != "" {
			names = append(names, meta.Image)
		}
	}
	return names, nil
}

// toConfigImageInfos converts vmimage.ImageInfo slice to config.ImageInfo slice.
// These types must stay in sync — see vmimage.ImageInfo.
func toConfigImageInfos(images []vmimage.ImageInfo) []config.ImageInfo {
	result := make([]config.ImageInfo, len(images))
	for i, img := range images {
		result[i] = config.ImageInfo{
			Name:      img.Name,
			Path:      img.Path,
			DockerRef: img.DockerRef,
			SizeBytes: img.SizeBytes,
			Source:    img.Source,
			Cached:    img.Cached,
		}
	}
	return result
}

// mapSentinelErrors maps vmimage sentinel errors to config sentinel errors.
func mapSentinelErrors(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, vmimage.ErrImageNotFound) {
		return config.ErrImageNotFoundSentinel
	}
	if errors.Is(err, vmimage.ErrImageInUse) {
		return config.ErrImageInUseSentinel
	}
	return err
}
