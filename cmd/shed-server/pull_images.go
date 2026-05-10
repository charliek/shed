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

	mgr := vmimage.NewManager(imgCfg, nil)
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

	// Hydrate _base from base_rootfs. With the content-addressed blob
	// store, two tags (_base and a variant) sharing the same Docker ref
	// converge on the same digest after first conversion — so all we
	// need to do is pull/ensure for _base. If a sibling tag already has
	// a matching blob installed, EnsureImage's cache hit makes this an
	// O(stat) operation; otherwise it falls through to a full convert.
	baseRootfs := imgCfg.GetBaseRootfs()
	if vmimage.IsDockerRef(baseRootfs) {
		imagesDir := imgCfg.GetImagesDir()
		// Fast path: another tag in this run already installed the
		// matching digest. Point _base at it directly.
		var sourceTag string
		for name, ref := range imgCfg.GetImages() {
			if ref != baseRootfs || !vmimage.IsDockerRef(ref) {
				continue
			}
			if vmimage.Resolve(imagesDir, name, ref) != "" {
				sourceTag = name
				break
			}
		}

		if sourceTag != "" {
			if err := mgr.TagImage(sourceTag, "_base"); err != nil {
				fmt.Printf("  [warn] tagging _base from %s failed (%v); falling back to full pull\n", sourceTag, err)
			} else {
				fmt.Printf("Done: _base (tagged from %s)\n", sourceTag)
				pulled++
				goto baseDone
			}
		}

		fmt.Printf("Pulling _base (%s)...\n", baseRootfs)
		if _, err := mgr.EnsureImage(ctx, vmimage.ResolvedRef{
			DockerRef: baseRootfs,
			Name:      "_base",
		}, func(stage, msg string) {
			fmt.Printf("  [%s] %s\n", stage, msg)
		}); err != nil {
			return fmt.Errorf("failed to pull base rootfs: %w", err)
		}
		fmt.Println("Done: _base")
		pulled++
	baseDone:
	}

	if pulled == 0 {
		fmt.Println("All images already cached.")
	} else {
		fmt.Printf("\n%d image(s) pulled successfully.\n", pulled)
	}

	return nil
}
