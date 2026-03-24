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

// Flags for image build
var (
	imageBuildFile      string
	imageBuildFrom      string
	imageBuildName      string
	imageBuildTarget    string
	imageBuildSize      string
	imageBuildOutputDir string
	imageBuildForce     bool
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

func init() {
	imageBuildCmd.Flags().StringVarP(&imageBuildFile, "file", "f", "", "Dockerfile path (default: ./Dockerfile.shed or ./Dockerfile)")
	imageBuildCmd.Flags().StringVar(&imageBuildFrom, "from", "", "Docker image reference to convert directly (skips build)")
	imageBuildCmd.Flags().StringVarP(&imageBuildName, "name", "n", "", "Image variant name (required)")
	imageBuildCmd.Flags().StringVar(&imageBuildTarget, "target", "", "Docker build target stage (Dockerfile mode only)")
	imageBuildCmd.Flags().StringVar(&imageBuildSize, "size", "20G", "Rootfs image size")
	imageBuildCmd.Flags().StringVar(&imageBuildOutputDir, "output-dir", "", "Output directory (default: ~/Library/Application Support/shed/vz/)")
	imageBuildCmd.Flags().BoolVar(&imageBuildForce, "force", false, "Skip base image validation warning")
	_ = imageBuildCmd.MarkFlagRequired("name")

	imageCmd.AddCommand(imageBuildCmd)
	imageCmd.AddCommand(imageListCmd)
	rootCmd.AddCommand(imageCmd)
}

func runImageBuild(cmd *cobra.Command, args []string) error {
	if imageBuildName == "" {
		return fmt.Errorf("--name is required")
	}

	outputDir := imageBuildOutputDir
	if outputDir == "" {
		outputDir = config.ExpandPath("~/Library/Application Support/shed/vz")
	}

	if imageBuildFrom != "" {
		return runImageBuildFromRef(cmd.Context(), outputDir)
	}
	return runImageBuildFromDockerfile(cmd.Context(), args, outputDir)
}

// runImageBuildFromRef converts a Docker registry image to ext4.
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

	// Write source sidecar for cache invalidation
	sourceFile := outputDir + "/" + vmimage.SourceFilename(imageBuildName)
	_ = os.WriteFile(sourceFile, []byte(imageBuildFrom+"\n"), 0644)

	printSuccess("Built image %q at %s", imageBuildName, result.RootfsPath)
	fmt.Printf("\nUse it with: shed create mydev --image %s\n", imageBuildName)
	return nil
}

// runImageBuildFromDockerfile builds from a Dockerfile then converts to ext4.
func runImageBuildFromDockerfile(ctx context.Context, args []string, outputDir string) error {
	// Determine build context
	buildContext := "."
	if len(args) > 0 {
		buildContext = args[0]
	}

	// Resolve Dockerfile path
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

	// Validate base image (warn if not extending shed base)
	if !imageBuildForce {
		if err := validateBaseImage(dockerfile); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
			fmt.Fprintf(os.Stderr, "Use --force to suppress this warning.\n\n")
		}
	}

	dockerTag := fmt.Sprintf("shed-vz-%s:latest", imageBuildName)

	// Build Docker image
	fmt.Printf("Building Docker image %s...\n", dockerTag)
	buildArgs := []string{"buildx", "build", "--platform", "linux/arm64",
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

	// Convert to ext4
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

	// Write source sidecar
	sourceFile := outputDir + "/" + vmimage.SourceFilename(imageBuildName)
	_ = os.WriteFile(sourceFile, []byte(dockerTag+"\n"), 0644)

	printSuccess("Built image %q at %s", imageBuildName, result.RootfsPath)
	fmt.Printf("\nUse it with: shed create mydev --image %s\n", imageBuildName)
	return nil
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

	w := newTabWriter()
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

func newTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
}

func formatBytes(b int64) string {
	const gb = 1024 * 1024 * 1024
	const mb = 1024 * 1024
	if b >= gb {
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	}
	return fmt.Sprintf("%.0f MB", float64(b)/float64(mb))
}

// validateBaseImage checks if a Dockerfile extends a shed base image.
func validateBaseImage(dockerfile string) error {
	data, err := os.ReadFile(dockerfile)
	if err != nil {
		return nil // Can't read, skip validation
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToUpper(trimmed), "FROM ") {
			if strings.Contains(trimmed, "shed-vz-") {
				return nil // Looks like it extends a shed base
			}
			// First FROM doesn't reference shed base
			return fmt.Errorf("dockerfile does not appear to extend a shed base image (first FROM: %s)", trimmed)
		}
	}
	return nil
}
