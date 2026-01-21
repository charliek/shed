package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"

	"github.com/charliek/shed/internal/config"
)

// CreateVolume creates a Docker volume for a shed workspace.
func (c *Client) CreateVolume(ctx context.Context, shedName string) error {
	volumeName := config.VolumeName(shedName)

	_, err := c.docker.VolumeCreate(ctx, volume.CreateOptions{
		Name: volumeName,
		Labels: map[string]string{
			config.LabelShed:     "true",
			config.LabelShedName: shedName,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create volume %s: %w", volumeName, err)
	}

	return nil
}

// DeleteVolume deletes a Docker volume for a shed workspace.
func (c *Client) DeleteVolume(ctx context.Context, shedName string) error {
	volumeName := config.VolumeName(shedName)

	// Force removal even if volume is in use
	if err := c.docker.VolumeRemove(ctx, volumeName, true); err != nil {
		return fmt.Errorf("failed to delete volume %s: %w", volumeName, err)
	}

	return nil
}

// VolumeExists checks if a Docker volume exists for a shed.
func (c *Client) VolumeExists(ctx context.Context, shedName string) (bool, error) {
	volumeName := config.VolumeName(shedName)

	// Use filters to check for specific volume
	filterArgs := filters.NewArgs()
	filterArgs.Add("name", volumeName)

	volumes, err := c.docker.VolumeList(ctx, volume.ListOptions{
		Filters: filterArgs,
	})
	if err != nil {
		return false, fmt.Errorf("failed to list volumes: %w", err)
	}

	// Check if the exact volume name exists
	for _, v := range volumes.Volumes {
		if v.Name == volumeName {
			return true, nil
		}
	}

	return false, nil
}
