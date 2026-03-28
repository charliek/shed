package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

var imageCmd = &cobra.Command{
	Use:   "image",
	Short: "Manage VZ images",
	Long:  "Build, list, and manage VZ rootfs images.",
}

var (
	imageBuildFile      string
	imageBuildFrom      string
	imageBuildName      string
	imageBuildTarget    string
	imageBuildSize      string
	imageBuildOutputDir string
	imageBuildForce     bool

	imageDeleteForce bool
	imagePruneForce  bool
	imagePruneDryRun bool
)

var imageBuildCmd = &cobra.Command{
	Use:   "build [context]",
	Short: "Build a VZ rootfs image",
	Long: `Build a VZ rootfs image from a Dockerfile or Docker registry image.

There are two modes:

  Dockerfile mode (default):
    shed image build -f Dockerfile.shed -n myimage .

  Registry mode (--from):
    shed image build --from ghcr.io/charliek/shed-vz-base:v1.0.0 -n myimage

The resulting ext4 image is stored in the images directory and is
immediately available for use with: shed create mydev --image myimage`,
	Args: cobra.MaximumNArgs(1),
	RunE: runImageBuild,
}

var imageListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available VZ images",
	Long:  "List available VZ image variants from server config and auto-discovered images.",
	Args:  cobra.NoArgs,
	RunE:  runImageList,
}

var imageDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a cached VZ image",
	Long:  "Delete a cached VZ rootfs image from the images directory.",
	Args:  cobra.ExactArgs(1),
	RunE:  runImageDelete,
}

var imagePruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove unused cached VZ images",
	Long:  "Remove cached VZ images that are not referenced by config or any existing shed.",
	Args:  cobra.NoArgs,
	RunE:  runImagePrune,
}

func init() {
	imageBuildCmd.Flags().StringVarP(&imageBuildFile, "file", "f", "", "Dockerfile path (default: ./Dockerfile.shed or ./Dockerfile)")
	imageBuildCmd.Flags().StringVar(&imageBuildFrom, "from", "", "Docker image reference to convert directly (skips build)")
	imageBuildCmd.Flags().StringVarP(&imageBuildName, "name", "n", "", "Image variant name (required)")
	imageBuildCmd.Flags().StringVar(&imageBuildTarget, "target", "", "Docker build target stage (Dockerfile mode only)")
	imageBuildCmd.Flags().StringVar(&imageBuildSize, "size", "20G", "Rootfs image size")
	imageBuildCmd.Flags().StringVar(&imageBuildOutputDir, "output-dir", "", "Output directory (default: ~/Library/Application Support/shed/vz/)")
	imageBuildCmd.Flags().BoolVar(&imageBuildForce, "force", false, "Skip base image validation warning")
	_ = imageBuildCmd.MarkFlagRequired("name")

	imageDeleteCmd.Flags().BoolVar(&imageDeleteForce, "force", false, "Skip confirmation prompt")
	imagePruneCmd.Flags().BoolVar(&imagePruneForce, "force", false, "Skip confirmation prompt")
	imagePruneCmd.Flags().BoolVar(&imagePruneDryRun, "dry-run", false, "List candidates without deleting")

	imageCmd.AddCommand(imageBuildCmd)
	imageCmd.AddCommand(imageListCmd)
	imageCmd.AddCommand(imageDeleteCmd)
	imageCmd.AddCommand(imagePruneCmd)
	rootCmd.AddCommand(imageCmd)
}

func runImageBuild(cmd *cobra.Command, args []string) error {
	outputDir := imageBuildOutputDir
	if outputDir == "" {
		outputDir = config.ExpandPath(config.DefaultVZImagesDir)
	}

	if imageBuildFrom != "" {
		return runImageBuildFromRef(cmd.Context(), outputDir)
	}
	return runImageBuildFromDockerfile(cmd.Context(), args, outputDir)
}

func runImageBuildFromRef(ctx context.Context, outputDir string) error {
	fmt.Printf("Converting %s to ext4 rootfs...\n", imageBuildFrom)

	result, err := vmimage.Convert(ctx, vmimage.ConvertOptions{
		DockerRef:     imageBuildFrom,
		Name:          imageBuildName,
		OutputDir:     outputDir,
		RootfsSize:    imageBuildSize,
		ExtractKernel: true,
	})
	if err != nil {
		return fmt.Errorf("conversion failed: %w", err)
	}

	finishImageBuild(outputDir, imageBuildFrom, result.RootfsPath)
	return nil
}

func runImageBuildFromDockerfile(ctx context.Context, args []string, outputDir string) error {
	buildContext := "."
	if len(args) > 0 {
		buildContext = args[0]
	}

	dockerfile := imageBuildFile
	if dockerfile == "" {
		if _, err := os.Stat("Dockerfile.shed"); err == nil {
			dockerfile = "Dockerfile.shed"
		} else if _, err := os.Stat("Dockerfile"); err == nil {
			dockerfile = "Dockerfile"
		} else {
			return fmt.Errorf("no Dockerfile found (tried Dockerfile.shed and Dockerfile)")
		}
	}

	if !imageBuildForce {
		if err := validateBaseImage(dockerfile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
			fmt.Fprintf(os.Stderr, "Use --force to suppress this warning.\n\n")
		}
	}

	dockerTag := fmt.Sprintf("shed-vz-%s:latest", imageBuildName)

	fmt.Printf("Building Docker image %s...\n", dockerTag)
	buildArgs := []string{"buildx", "build", "--platform", vmimage.DefaultPlatform,
		"-t", dockerTag, "--load", "-f", dockerfile}
	if imageBuildTarget != "" {
		buildArgs = append(buildArgs, "--target", imageBuildTarget)
	}
	buildArgs = append(buildArgs, buildContext)

	buildCmd := exec.CommandContext(ctx, "docker", buildArgs...)
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("docker build failed: %w", err)
	}

	fmt.Printf("\nConverting to ext4 rootfs...\n")
	result, err := vmimage.Convert(ctx, vmimage.ConvertOptions{
		DockerRef:     dockerTag,
		Name:          imageBuildName,
		OutputDir:     outputDir,
		RootfsSize:    imageBuildSize,
		ExtractKernel: true,
	})
	if err != nil {
		return fmt.Errorf("conversion failed: %w", err)
	}

	finishImageBuild(outputDir, dockerTag, result.RootfsPath)
	return nil
}

// finishImageBuild writes the source sidecar and prints success output.
func finishImageBuild(outputDir, sourceRef, rootfsPath string) {
	if err := vmimage.WriteSource(outputDir, imageBuildName, sourceRef); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write source sidecar: %v\n", err)
	}
	printSuccess("Built image %q at %s", imageBuildName, rootfsPath)
	fmt.Printf("\nUse it with: shed create mydev --image %s\n", imageBuildName)
}

func runImageList(_ *cobra.Command, _ []string) error {
	entry, _, err := getServerEntry()
	if err != nil {
		return err
	}

	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	resp, err := client.ListImages()
	if err != nil {
		return fmt.Errorf("failed to list images: %w", err)
	}

	if jsonFlag {
		return outputJSON(resp)
	}

	if len(resp.Images) == 0 {
		fmt.Println("No images available.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSOURCE\tSIZE\tCACHED\tREF")
	for _, img := range resp.Images {
		size := "-"
		if img.SizeBytes > 0 {
			size = formatBytes(img.SizeBytes)
		}
		cached := "no"
		if img.Cached {
			cached = "yes"
		}
		ref := img.DockerRef
		if ref == "" {
			ref = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", img.Name, img.Source, size, cached, ref)
	}
	w.Flush()
	return nil
}

func formatBytes(b int64) string {
	const gb = 1024 * 1024 * 1024
	const mb = 1024 * 1024
	if b >= gb {
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	}
	return fmt.Sprintf("%.0f MB", float64(b)/float64(mb))
}

func runImageDelete(_ *cobra.Command, args []string) error {
	name := args[0]

	if jsonFlag && !imageDeleteForce {
		return fmt.Errorf("--force is required when using --json")
	}

	entry, _, err := getServerEntry()
	if err != nil {
		return err
	}

	client := NewAPIClientFromEntry(entry, DefaultTimeout)

	// Fetch image info for confirmation prompt and success output
	var targetImage *config.ImageInfo
	if !imageDeleteForce {
		resp, err := client.ListImages()
		if err != nil {
			return fmt.Errorf("failed to list images: %w", err)
		}
		for i := range resp.Images {
			if resp.Images[i].Name == name {
				targetImage = &resp.Images[i]
				break
			}
		}
	}

	if !imageDeleteForce {
		prompt := fmt.Sprintf("Delete image %q", name)
		if targetImage != nil && targetImage.SizeBytes > 0 {
			prompt += fmt.Sprintf(" (%s)", formatBytes(targetImage.SizeBytes))
		}
		prompt += "? [y/N] "
		if !confirmAction(prompt) {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	if err := client.DeleteImage(name); err != nil {
		return fmt.Errorf("failed to delete image: %w", err)
	}

	if jsonFlag {
		result := ActionResult{
			Status: "ok",
			Action: "deleted",
			Name:   name,
		}
		if targetImage != nil {
			result.Details = targetImage
		}
		return outputJSON(result)
	}

	msg := fmt.Sprintf("Deleted image %s", name)
	if targetImage != nil && targetImage.SizeBytes > 0 {
		msg += fmt.Sprintf(" (freed %s)", formatBytes(targetImage.SizeBytes))
	}
	printSuccess(msg)
	return nil
}

func runImagePrune(_ *cobra.Command, _ []string) error {
	if jsonFlag && !imagePruneForce {
		return fmt.Errorf("--force is required when using --json")
	}

	entry, _, err := getServerEntry()
	if err != nil {
		return err
	}

	client := NewAPIClientFromEntry(entry, DefaultTimeout)

	// First do a dry run to see candidates
	dryResp, err := client.PruneImages(true)
	if err != nil {
		return fmt.Errorf("failed to check unused images: %w", err)
	}

	if len(dryResp.Deleted) == 0 {
		if jsonFlag {
			return outputJSON(config.PruneImagesResponse{Deleted: []config.ImageInfo{}})
		}
		fmt.Println("No unused images to prune.")
		return nil
	}

	// Compute total size across all candidates
	var totalSize int64
	for _, img := range dryResp.Deleted {
		totalSize += img.SizeBytes
	}

	// Show candidates table in non-JSON mode
	if !jsonFlag {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSIZE\tREF")
		for _, img := range dryResp.Deleted {
			size := "-"
			if img.SizeBytes > 0 {
				size = formatBytes(img.SizeBytes)
			}
			ref := img.DockerRef
			if ref == "" {
				ref = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", img.Name, size, ref)
		}
		w.Flush()
		fmt.Println()
	}

	if imagePruneDryRun {
		if jsonFlag {
			return outputJSON(config.PruneImagesResponse{Deleted: dryResp.Deleted})
		}
		fmt.Printf("Would prune %d image(s)", len(dryResp.Deleted))
		if totalSize > 0 {
			fmt.Printf(" (%s)", formatBytes(totalSize))
		}
		fmt.Println()
		return nil
	}

	if !imagePruneForce {
		prompt := fmt.Sprintf("Delete %d image(s)", len(dryResp.Deleted))
		if totalSize > 0 {
			prompt += fmt.Sprintf(" (%s)", formatBytes(totalSize))
		}
		prompt += "? [y/N] "
		if !confirmAction(prompt) {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	// Execute the prune
	pruneResp, err := client.PruneImages(false)
	if err != nil {
		return fmt.Errorf("failed to prune images: %w", err)
	}

	if jsonFlag {
		return outputJSON(pruneResp)
	}

	msg := fmt.Sprintf("Pruned %d image(s)", len(pruneResp.Deleted))
	if totalSize > 0 {
		msg += fmt.Sprintf(" (freed %s)", formatBytes(totalSize))
	}
	printSuccess(msg)
	return nil
}

func validateBaseImage(dockerfile string) error {
	data, err := os.ReadFile(dockerfile)
	if err != nil {
		return nil
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(trimmed), "FROM ") {
			if strings.Contains(trimmed, "shed-vz-") {
				return nil
			}
			return fmt.Errorf("dockerfile does not appear to extend a shed base image (first FROM: %s)", trimmed)
		}
	}
	return nil
}
