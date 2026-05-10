package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

var imageCmd = &cobra.Command{
	Use:   "image",
	Short: "Manage rootfs images",
	Long:  "Build, list, and manage rootfs images for VM backends.",
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
	Short: "Build a rootfs image",
	Long: `Build a rootfs image from a Dockerfile or Docker registry image.

There are two modes:

  Dockerfile mode (default):
    shed image build -f Dockerfile.shed -n myimage .

  Registry mode (--from):
    shed image build --from ghcr.io/org/image:v1.0.0 -n myimage

The target platform is auto-detected from the host OS (linux/amd64 for
Firecracker, linux/arm64 for VZ). The resulting ext4 image is stored in
the images directory and is immediately available for use with:
  shed create mydev --image myimage`,
	Args: cobra.MaximumNArgs(1),
	RunE: runImageBuild,
}

var imageListCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List available images",
	Long:    "List available image variants from server config and auto-discovered tags.",
	Args:    cobra.NoArgs,
	RunE:    runImageList,
}

var imageDeleteCmd = &cobra.Command{
	Use:     "rm <name>",
	Aliases: []string{"delete"},
	Short:   "Remove a tag",
	Long: `Remove a tag from the image store. Following the Docker model, the
underlying blob is NOT removed by 'shed image rm' — call 'shed image prune'
to garbage-collect blobs that are no longer referenced.`,
	Args: cobra.ExactArgs(1),
	RunE: runImageDelete,
}

var imageInspectCmd = &cobra.Command{
	Use:   "inspect <tag-or-digest>",
	Short: "Show details about an image",
	Long:  "Inspect a tag or digest, returning the manifest and disk layout.",
	Args:  cobra.ExactArgs(1),
	RunE:  runImageInspect,
}

var imageTagCmd = &cobra.Command{
	Use:   "tag <src-tag-or-digest> <new-tag>",
	Short: "Point a new tag at an existing image",
	Long:  "Create or update a tag to point at the digest currently held by another tag (or a specified digest).",
	Args:  cobra.ExactArgs(2),
	RunE:  runImageTag,
}

var imagePullCmd = &cobra.Command{
	Use:   "pull <docker-ref>",
	Short: "Pull a Docker image into the blob store",
	Long: `Pull a Docker reference, convert it to ext4, install it into the
content-addressed blob store, and advance a tag.

Defaults the tag to the last path segment of the Docker ref minus the version
suffix (e.g. ghcr.io/charliek/shed-vz-experimental:v0.4.0 → 'experimental').`,
	Args: cobra.ExactArgs(1),
	RunE: runImagePull,
}

var imagePullTag string

var imagePruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove unused cached images",
	Long:  "Remove cached images that are not referenced by config or any existing shed.",
	Args:  cobra.NoArgs,
	RunE:  runImagePrune,
}

func init() {
	imageBuildCmd.Flags().StringVarP(&imageBuildFile, "file", "f", "", "Dockerfile path (default: ./Dockerfile.shed or ./Dockerfile)")
	imageBuildCmd.Flags().StringVar(&imageBuildFrom, "from", "", "Docker image reference to convert directly (skips build)")
	imageBuildCmd.Flags().StringVarP(&imageBuildName, "name", "n", "", "Image variant name (required)")
	imageBuildCmd.Flags().StringVar(&imageBuildTarget, "target", "", "Docker build target stage (Dockerfile mode only)")
	imageBuildCmd.Flags().StringVar(&imageBuildSize, "size", "20G", "Rootfs image size")
	imageBuildCmd.Flags().StringVar(&imageBuildOutputDir, "output-dir", "", "Output directory (auto-detected based on backend)")
	imageBuildCmd.Flags().BoolVar(&imageBuildForce, "force", false, "Skip base image validation warning")
	_ = imageBuildCmd.MarkFlagRequired("name")

	imageDeleteCmd.Flags().BoolVar(&imageDeleteForce, "force", false, "Skip confirmation prompt")
	imagePruneCmd.Flags().BoolVar(&imagePruneForce, "force", false, "Skip confirmation prompt")
	imagePruneCmd.Flags().BoolVar(&imagePruneDryRun, "dry-run", false, "List candidates without deleting")
	imagePullCmd.Flags().StringVarP(&imagePullTag, "tag", "t", "", "Tag name (default: derived from docker ref)")

	imageCmd.AddCommand(imageBuildCmd)
	imageCmd.AddCommand(imageListCmd)
	imageCmd.AddCommand(imageDeleteCmd)
	imageCmd.AddCommand(imagePruneCmd)
	imageCmd.AddCommand(imageInspectCmd)
	imageCmd.AddCommand(imageTagCmd)
	imageCmd.AddCommand(imagePullCmd)
	rootCmd.AddCommand(imageCmd)
}

// buildContext holds backend-specific settings for image building.
type buildContext struct {
	Prefix        string // Docker tag prefix ("shed-vz-" or "shed-fc-")
	Platform      string // Docker platform ("linux/arm64" or "linux/amd64")
	OutputDir     string // Default output directory
	ExtractKernel bool   // Whether to extract kernel from images
	NeedsInitrd   bool   // Whether to extract initrd (VZ only)
}

// imageBackendContext returns build settings for the current host OS.
// shed image build is a host-local operation — the platform is determined
// by the host, not by server backend selection.
func imageBackendContext() buildContext {
	if runtime.GOOS == "linux" {
		return buildContext{
			Prefix:        "shed-fc-",
			Platform:      vmimage.FirecrackerPlatform,
			OutputDir:     config.DefaultFirecrackerImagesDir,
			ExtractKernel: true,
			NeedsInitrd:   false,
		}
	}
	return buildContext{
		Prefix:        "shed-vz-",
		Platform:      vmimage.DefaultPlatform,
		OutputDir:     config.ExpandPath(config.DefaultVZImagesDir),
		ExtractKernel: true,
		NeedsInitrd:   true,
	}
}

func runImageBuild(cmd *cobra.Command, args []string) error {
	bc := imageBackendContext()

	outputDir := imageBuildOutputDir
	if outputDir == "" {
		outputDir = bc.OutputDir
	}

	if imageBuildFrom != "" {
		return runImageBuildFromRef(cmd.Context(), outputDir, bc.Platform, bc.ExtractKernel, bc.NeedsInitrd)
	}
	return runImageBuildFromDockerfile(cmd.Context(), args, outputDir, bc.Prefix, bc.Platform, bc.ExtractKernel, bc.NeedsInitrd)
}

func runImageBuildFromRef(ctx context.Context, outputDir, platform string, extractKernel, needsInitrd bool) error {
	fmt.Printf("Converting %s to ext4 rootfs...\n", imageBuildFrom)

	digest, err := convertAndInstall(ctx, imageBuildFrom, outputDir, platform, extractKernel, needsInitrd)
	if err != nil {
		return err
	}
	finishImageBuild(outputDir, imageBuildFrom, digest)
	return nil
}

func runImageBuildFromDockerfile(ctx context.Context, args []string, outputDir, prefix, platform string, extractKernel, needsInitrd bool) error {
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

	dockerTag := fmt.Sprintf("%s%s:latest", prefix, imageBuildName)

	fmt.Printf("Building Docker image %s...\n", dockerTag)
	buildArgs := []string{"buildx", "build", "--platform", platform,
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
	digest, err := convertAndInstall(ctx, dockerTag, outputDir, platform, extractKernel, needsInitrd)
	if err != nil {
		return err
	}
	finishImageBuild(outputDir, dockerTag, digest)
	return nil
}

// convertAndInstall runs Convert, installs the result into the blob
// store at the computed digest, and advances the imageBuildName tag.
// Returns the digest installed.
func convertAndInstall(ctx context.Context, sourceRef, outputDir, platform string, extractKernel, needsInitrd bool) (string, error) {
	result, err := vmimage.Convert(ctx, vmimage.ConvertOptions{
		DockerRef:     sourceRef,
		Name:          imageBuildName,
		OutputDir:     outputDir,
		RootfsSize:    imageBuildSize,
		Platform:      platform,
		ExtractKernel: extractKernel,
		NeedsInitrd:   needsInitrd,
	})
	if err != nil {
		return "", fmt.Errorf("conversion failed: %w", err)
	}
	defer vmimage.CleanupConvert(result)

	rootfsLogical := int64(0)
	if fi, err := os.Stat(result.RootfsPath); err == nil {
		rootfsLogical = fi.Size()
	}
	manifest := vmimage.Manifest{
		SchemaVersion:     vmimage.ManifestSchemaVersion,
		Digest:            result.Digest,
		SourceRef:         sourceRef,
		RootfsLogicalSize: rootfsLogical,
		CreatedAt:         time.Now().UTC(),
	}
	files := map[string]string{vmimage.BlobRootfsFilename: result.RootfsPath}
	if result.KernelPath != "" {
		files[vmimage.BlobKernelFilename] = result.KernelPath
		if fi, err := os.Stat(result.KernelPath); err == nil {
			manifest.KernelSize = fi.Size()
		}
	}
	if result.InitrdPath != "" {
		files[vmimage.BlobInitrdFilename] = result.InitrdPath
		if fi, err := os.Stat(result.InitrdPath); err == nil {
			manifest.InitrdSize = fi.Size()
		}
	}

	if _, _, err := vmimage.InstallBlob(outputDir, vmimage.BlobInstallSpec{
		Files:    files,
		Manifest: manifest,
	}); err != nil {
		return "", fmt.Errorf("installing blob: %w", err)
	}

	if err := vmimage.SetTag(outputDir, imageBuildName, result.Digest); err != nil {
		return "", fmt.Errorf("advancing tag %q: %w", imageBuildName, err)
	}

	return result.Digest, nil
}

// finishImageBuild prints success output. Tag advancement happens inside
// convertAndInstall.
func finishImageBuild(_ /*outputDir*/, sourceRef, digest string) {
	_ = sourceRef
	printSuccess("Built image %q (%s)", imageBuildName, vmimage.ShortDigest(digest))
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
	fmt.Fprintln(w, "NAME\tDIGEST\tSOURCE\tSIZE\tIN USE\tREF")
	for _, img := range resp.Images {
		size := "-"
		if img.SizeBytes > 0 {
			size = formatSize(img.SizeBytes)
		}
		digest := "-"
		if img.Digest != "" {
			digest = vmimage.ShortDigest(img.Digest)
		}
		inUse := "no"
		if img.InUse {
			inUse = "yes"
		}
		ref := img.DockerRef
		if ref == "" {
			ref = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", img.Name, digest, img.Source, size, inUse, ref)
	}
	w.Flush()
	if !jsonFlag && len(resp.Images) > 0 {
		fmt.Println()
		fmt.Println("Use 'shed image rm <name>' to remove a tag, or 'shed image prune' to GC unused blobs.")
	}
	return nil
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
			prompt += fmt.Sprintf(" (%s)", formatSize(targetImage.SizeBytes))
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
		msg += fmt.Sprintf(" (freed %s)", formatSize(targetImage.SizeBytes))
	}
	printSuccess(msg)
	return nil
}

func runImageInspect(_ *cobra.Command, args []string) error {
	entry, _, err := getServerEntry()
	if err != nil {
		return err
	}
	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	resp, err := client.InspectImage(args[0])
	if err != nil {
		return fmt.Errorf("failed to inspect image: %w", err)
	}
	if jsonFlag {
		return outputJSON(resp)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Name:\t%s\n", resp.Image.Name)
	if resp.Image.Tag != "" {
		fmt.Fprintf(w, "Tag:\t%s\n", resp.Image.Tag)
	}
	fmt.Fprintf(w, "Digest:\t%s\n", resp.Image.Digest)
	fmt.Fprintf(w, "Source:\t%s\n", resp.Image.Source)
	if resp.Image.DockerRef != "" {
		fmt.Fprintf(w, "Docker ref:\t%s\n", resp.Image.DockerRef)
	}
	if resp.Image.Path != "" {
		fmt.Fprintf(w, "Path:\t%s\n", resp.Image.Path)
	}
	if resp.Image.SizeBytes > 0 {
		fmt.Fprintf(w, "Size:\t%s\n", formatSize(resp.Image.SizeBytes))
	}
	fmt.Fprintf(w, "In use:\t%v\n", resp.Image.InUse)
	if !resp.Manifest.CreatedAt.IsZero() {
		fmt.Fprintf(w, "Created:\t%s\n", resp.Manifest.CreatedAt.Format("2006-01-02 15:04:05 UTC"))
	}
	w.Flush()
	return nil
}

func runImageTag(_ *cobra.Command, args []string) error {
	entry, _, err := getServerEntry()
	if err != nil {
		return err
	}
	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	if err := client.TagImage(args[0], args[1]); err != nil {
		return fmt.Errorf("failed to tag image: %w", err)
	}
	if jsonFlag {
		return outputJSON(ActionResult{Status: "ok", Action: "tagged", Name: args[1]})
	}
	printSuccess("Tagged %s as %s", args[0], args[1])
	return nil
}

func runImagePull(cmd *cobra.Command, args []string) error {
	dockerRef := args[0]
	tag := imagePullTag
	if tag == "" {
		tag = deriveTagFromRef(dockerRef)
	}
	if err := vmimage.ValidateImageName(tag); err != nil {
		return fmt.Errorf("invalid tag %q: %w", tag, err)
	}

	entry, _, err := getServerEntry()
	if err != nil {
		return err
	}
	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	resp, err := client.PullImage(dockerRef, tag)
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}
	if jsonFlag {
		return outputJSON(resp)
	}
	printSuccess("Pulled %s as tag %q (%s)", dockerRef, resp.Tag, vmimage.ShortDigest(resp.Digest))
	return nil
}

// deriveTagFromRef extracts a sensible default tag from a Docker ref:
// ghcr.io/charliek/shed-vz-experimental:v0.4.0 → "experimental".
func deriveTagFromRef(ref string) string {
	// Strip everything after the last colon (tag/digest).
	name := ref
	if i := strings.LastIndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		// Avoid stripping registry-port colons by checking for '/' after.
		if !strings.Contains(name[i:], "/") {
			name = name[:i]
		}
	}
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	for _, prefix := range []string{"shed-vz-", "shed-fc-"} {
		if strings.HasPrefix(name, prefix) {
			name = strings.TrimPrefix(name, prefix)
			break
		}
	}
	if name == "" {
		name = "default"
	}
	return name
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
				size = formatSize(img.SizeBytes)
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
			fmt.Printf(" (%s)", formatSize(totalSize))
		}
		fmt.Println()
		return nil
	}

	if !imagePruneForce {
		prompt := fmt.Sprintf("Delete %d image(s)", len(dryResp.Deleted))
		if totalSize > 0 {
			prompt += fmt.Sprintf(" (%s)", formatSize(totalSize))
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

	var freedSize int64
	for _, img := range pruneResp.Deleted {
		freedSize += img.SizeBytes
	}
	msg := fmt.Sprintf("Pruned %d image(s)", len(pruneResp.Deleted))
	if freedSize > 0 {
		msg += fmt.Sprintf(" (freed %s)", formatSize(freedSize))
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
			if strings.Contains(trimmed, "shed-vz-") || strings.Contains(trimmed, "shed-fc-") {
				return nil
			}
			return fmt.Errorf("dockerfile does not appear to extend a shed base image (first FROM: %s)", trimmed)
		}
	}
	return nil
}
