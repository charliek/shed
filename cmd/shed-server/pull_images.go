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

	// Pre-warm the default_image plus every image_aliases ref. Keyed by
	// the ref itself (the io.shed.source-ref identity); duplicate refs
	// (e.g. an alias equal to default_image) converge on one blob and one
	// ref-index entry, so a second EnsureImage is an O(stat) cache hit.
	refs := map[string]string{} // selector label -> ref
	if dr := imgCfg.GetDefaultImage(); dr != "" {
		refs["default_image"] = dr
	}
	for alias, ref := range imgCfg.GetImageAliases() {
		refs[alias] = ref
	}

	// Filter to a specific selector if requested.
	if pullVariant != "" {
		ref, ok := refs[pullVariant]
		if !ok {
			var names []string
			for name := range refs {
				names = append(names, name)
			}
			sort.Strings(names)
			return fmt.Errorf("unknown selector %q; available: %v", pullVariant, names)
		}
		refs = map[string]string{pullVariant: ref}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	mgr := vmimage.NewManager(imgCfg, nil)
	pulled := 0

	var names []string
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		ref := refs[name]
		if !vmimage.IsDockerRef(ref) {
			fmt.Printf("Skipping %s (local path: %s)\n", name, ref)
			continue
		}

		fmt.Printf("Pulling %s (%s)...\n", name, ref)
		_, err := mgr.EnsureImage(ctx, vmimage.ResolvedRef{
			DockerRef: ref,
			Name:      vmimage.DeriveTagFromRef(ref),
		}, func(stage, msg string) {
			fmt.Printf("  [%s] %s\n", stage, msg)
		})
		if err != nil {
			return fmt.Errorf("failed to pull image %s: %w", name, err)
		}
		fmt.Printf("Done: %s\n", name)
		pulled++
	}

	if pulled == 0 {
		fmt.Println("All images already cached.")
	} else {
		fmt.Printf("\n%d image(s) pulled successfully.\n", pulled)
	}

	return nil
}
