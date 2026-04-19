package main

import (
	"context"
	"fmt"
	"os/signal"
	"sort"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

var (
	pullVariant string
)

var pullImagesCmd = &cobra.Command{
	Use:   "pull-images",
	Short: "Pre-pull configured VM images",
	Long: `Pull and convert Docker images to ext4 rootfs for the configured backend.

This pre-caches images so the first 'shed create' doesn't wait for image conversion.
Uses the images configured in server.yaml for the active backend (VZ or Firecracker).`,
	RunE: runPullImages,
}

func init() {
	pullImagesCmd.Flags().StringVar(&pullVariant, "variant", "", "pull a specific image variant only")
}

func runPullImages(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	var imgCfg vmimage.ImageConfig
	switch cfg.DefaultBackend {
	case config.BackendFirecracker:
		if cfg.Firecracker == nil {
			return fmt.Errorf("firecracker config is required when backend is firecracker")
		}
		imgCfg = cfg.Firecracker
	case config.BackendVZ:
		if cfg.VZ == nil {
			return fmt.Errorf("vz config is required when backend is vz")
		}
		imgCfg = cfg.VZ
	default:
		return fmt.Errorf("unsupported backend: %s", cfg.DefaultBackend)
	}

	images := imgCfg.GetImages()

	// Filter to specific variant if requested
	if pullVariant != "" {
		ref, ok := images[pullVariant]
		if !ok {
			var names []string
			for name := range images {
				names = append(names, name)
			}
			sort.Strings(names)
			return fmt.Errorf("unknown variant %q; available: %v", pullVariant, names)
		}
		images = map[string]string{pullVariant: ref}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	mgr := vmimage.NewManager(imgCfg)
	pulled := 0

	// Sort variant names for deterministic output
	var names []string
	for name := range images {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		ref := images[name]
		if !vmimage.IsDockerRef(ref) {
			fmt.Printf("Skipping %s (local path: %s)\n", name, ref)
			continue
		}

		fmt.Printf("Pulling %s (%s)...\n", name, ref)
		_, err := mgr.EnsureImage(ctx, vmimage.ResolvedRef{
			DockerRef: ref,
			Name:      name,
		}, func(stage, msg string) {
			fmt.Printf("  [%s] %s\n", stage, msg)
		})
		if err != nil {
			return fmt.Errorf("failed to pull image %s: %w", name, err)
		}
		fmt.Printf("Done: %s\n", name)
		pulled++
	}

	// Hydrate _base from base_rootfs. If base_rootfs shares a Docker ref
	// with any cached variant in the full config, hardlink _base to that
	// variant (zero extra disk). Otherwise pull a fresh copy. This makes
	// `shed create` (no --image) immediate after `pull-images`, which
	// previously skipped _base whenever the ref matched a variant.
	baseRootfs := imgCfg.GetBaseRootfs()
	if vmimage.IsDockerRef(baseRootfs) {
		imagesDir := imgCfg.GetImagesDir()
		var linkFrom string
		for name, ref := range imgCfg.GetImages() {
			if ref != baseRootfs || !vmimage.IsDockerRef(ref) {
				continue
			}
			if vmimage.CheckCache(imagesDir, name, ref) != "" {
				linkFrom = name
				break
			}
		}

		linked := false
		if linkFrom != "" {
			if err := vmimage.LinkCachedImage(imagesDir, linkFrom, "_base", baseRootfs); err != nil {
				fmt.Printf("  [warn] hardlink of _base to %s failed (%v); falling back to full pull\n", linkFrom, err)
			} else {
				fmt.Printf("Done: _base (linked to %s)\n", linkFrom)
				pulled++
				linked = true
			}
		}

		if !linked {
			fmt.Printf("Pulling _base (%s)...\n", baseRootfs)
			_, err := mgr.EnsureImage(ctx, vmimage.ResolvedRef{
				DockerRef: baseRootfs,
				Name:      "_base",
			}, func(stage, msg string) {
				fmt.Printf("  [%s] %s\n", stage, msg)
			})
			if err != nil {
				return fmt.Errorf("failed to pull base rootfs: %w", err)
			}
			fmt.Println("Done: _base")
			pulled++
		}
	}

	if pulled == 0 {
		fmt.Println("All images already cached.")
	} else {
		fmt.Printf("\n%d image(s) pulled successfully.\n", pulled)
	}

	return nil
}
