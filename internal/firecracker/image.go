//go:build linux

package firecracker

import (
	"errors"
	"fmt"
	"os"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

// ListImages returns available image variants from config and auto-discovery.
func (c *Client) ListImages() ([]config.ImageInfo, error) {
	mgr := vmimage.NewManager(c.cfg)
	images, err := mgr.ListImages()
	if err != nil {
		return nil, err
	}
	return toConfigImageInfos(images), nil
}

// DeleteImage removes a cached image by name.
func (c *Client) DeleteImage(name string) error {
	mgr := vmimage.NewManager(c.cfg)
	err := mgr.DeleteImage(name, c.inUseImageNames)
	return mapSentinelErrors(err)
}

// PruneImages removes cached images not referenced by config or existing sheds.
func (c *Client) PruneImages(dryRun bool) ([]config.ImageInfo, error) {
	mgr := vmimage.NewManager(c.cfg)
	images, err := mgr.PruneImages(dryRun, c.inUseImageNames)
	if err != nil {
		return nil, err
	}
	return toConfigImageInfos(images), nil
}

// inUseImageNames returns image names referenced by existing Firecracker instances.
func (c *Client) inUseImageNames() ([]string, error) {
	return c.inUseImageNamesExcept(nil)
}

// inUseImageNamesExcept returns image names referenced by existing
// Firecracker instances whose names are NOT in skipSheds. Used by Prune's
// dry-run path to simulate the post-instance-delete state so image
// candidates reflect the fleet after instance deletions.
func (c *Client) inUseImageNamesExcept(skipSheds map[string]bool) ([]string, error) {
	instances, err := ListInstances(c.cfg.InstanceDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("listing instances: %w", err)
	}
	var names []string
	for _, inst := range instances {
		if skipSheds[inst] {
			continue
		}
		meta, err := LoadMetadata(c.cfg.InstanceDir, inst)
		if err != nil {
			return nil, fmt.Errorf("reading metadata for %s: %w", inst, err)
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
