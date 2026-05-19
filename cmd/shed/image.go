package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"

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
	imageBuildInitrd    string
	imageBuildName      string
	imageBuildTarget    string
	imageBuildSize      string
	imageBuildOutputDir string
	imageBuildPlatform  string
	imageBuildSourceRef string
	imageBuildForce     bool

	imageDeleteForce bool
	imagePruneForce  bool
	imagePruneDryRun bool
)

var imageBuildCmd = &cobra.Command{
	Use:   "build [context]",
	Short: "Build a rootfs image",
	Long: `Build a rootfs image from a Dockerfile.

Example:
  shed image build -f Dockerfile.shed -n myimage .

The target platform is auto-detected from the host OS (linux/amd64 for
Firecracker, linux/arm64 for VZ). The resulting OCI image is stored in
the images directory and is immediately available for use with:
  shed create mydev --image myimage

To consume an image from a registry instead of building locally, use
'shed image pull <ref>' — the old 'shed image build --from <ref>' mode
was removed in this release.`,
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

var imagePushCmd = &cobra.Command{
	Use:   "push <tag-or-digest> <destination-ref>",
	Short: "Push an image to a registry",
	Long: `Push the manifest currently held by a tag (or digest) to a
destination registry reference, byte-perfect: the on-disk layer blobs
are streamed unchanged so any signatures attached to the original
remain valid.

By default this talks to a running shed-server via the HTTP API.
Pass --local (or -c <config>) to read the OCI store directly without
needing a server — useful for CI publish flows and standalone hosts.`,
	Args: cobra.ExactArgs(2),
	RunE: runImagePush,
}

var imagePushLocal bool

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

var (
	imagePullTag      string
	imagePullPlatform string
)

var imagePruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove unused cached images",
	Long:  "Remove cached images that are not referenced by config or any existing shed.",
	Args:  cobra.NoArgs,
	RunE:  runImagePrune,
}

func init() {
	imageBuildCmd.Flags().StringVarP(&imageBuildFile, "file", "f", "", "Dockerfile path (default: ./Dockerfile.shed or ./Dockerfile)")
	imageBuildCmd.Flags().StringVarP(&imageBuildName, "name", "n", "", "Image variant name (required)")
	imageBuildCmd.Flags().StringVar(&imageBuildInitrd, "initramfs", "", "Path to a pre-built shed-overlay initramfs (built via scripts/build-initramfs.sh). Required for images that need to boot through shed's overlayfs assembly.")
	imageBuildCmd.Flags().StringVar(&imageBuildTarget, "target", "", "Docker build target stage (Dockerfile mode only)")
	imageBuildCmd.Flags().StringVar(&imageBuildSize, "size", "20G", "Rootfs image size")
	imageBuildCmd.Flags().StringVar(&imageBuildOutputDir, "output-dir", "", "Output directory (auto-detected based on backend)")
	imageBuildCmd.Flags().StringVar(&imageBuildPlatform, "platform", "", "Target docker platform (linux/arm64 or linux/amd64). Default: linux/arm64 for shed-vz-* targets, linux/amd64 for shed-fc-* targets, else host arch.")
	// --source-ref controls the `io.shed.source-ref` manifest annotation
	// that the server's resolveImage cache lookup compares against. CI
	// publish workflows should pass the final registry ref here (e.g.,
	// `ghcr.io/charliek/shed-vz-full:v0.5.0`) so subsequent `shed create`
	// pulls on a remote host hit the cache instead of re-pulling. When
	// empty, defaults to the local buildx tag (`<prefix><name>:latest`),
	// which is fine for purely-local builds but does NOT match the
	// registry ref the image is later pushed to.
	imageBuildCmd.Flags().StringVar(&imageBuildSourceRef, "source-ref", "", "Override the io.shed.source-ref annotation baked into the manifest (default: <prefix><name>:latest). Pass the final registry ref in CI publish flows so server-side cache lookups match.")
	imageBuildCmd.Flags().BoolVar(&imageBuildForce, "force", false, "Skip base image validation warning")
	_ = imageBuildCmd.MarkFlagRequired("name")

	imageDeleteCmd.Flags().BoolVar(&imageDeleteForce, "force", false, "Skip confirmation prompt")
	imagePruneCmd.Flags().BoolVar(&imagePruneForce, "force", false, "Skip confirmation prompt")
	imagePruneCmd.Flags().BoolVar(&imagePruneDryRun, "dry-run", false, "List candidates without deleting")
	imagePullCmd.Flags().StringVarP(&imagePullTag, "tag", "t", "", "Tag name (default: derived from docker ref)")
	imagePullCmd.Flags().StringVar(&imagePullPlatform, "platform", "", "Platform override for multi-arch images (e.g. linux/arm64). Empty means the backend's native platform.")
	imagePushCmd.Flags().BoolVar(&imagePushLocal, "local", false, "Push from the local OCI store (no shed-server required). Implied when -c is set without -s.")

	imageCmd.AddCommand(imageBuildCmd)
	imageCmd.AddCommand(imageListCmd)
	imageCmd.AddCommand(imageDeleteCmd)
	imageCmd.AddCommand(imagePruneCmd)
	imageCmd.AddCommand(imageInspectCmd)
	imageCmd.AddCommand(imageTagCmd)
	imageCmd.AddCommand(imagePullCmd)
	imageCmd.AddCommand(imagePushCmd)
	rootCmd.AddCommand(imageCmd)
}

// buildContext holds backend-specific settings for image building.
//
// All four behavior-shaping fields (Prefix, Platform, ExtractKernel,
// NeedsInitrd) MUST stay in lockstep with each other: VZ is arm64 +
// needs-initrd, FC is amd64 + no-initrd. Splitting their resolution
// (one source for Prefix, another for Platform) is what caused the
// pre-PR #92/#94 bugs where a Linux-runner cross-build of `shed-vz-*`
// got the right --platform but the wrong `shed-fc-` prefix in the
// manifest's source-ref annotation. Keep the per-target table here
// as the single source of truth.
//
// OutputDir is the one field that legitimately follows the host OS:
// it's the on-disk path of the local OCI store, and the user's
// explicit --output-dir always wins anyway.
type buildContext struct {
	Prefix        string // Docker tag prefix ("shed-vz-" or "shed-fc-")
	Platform      string // Docker platform ("linux/arm64" or "linux/amd64")
	OutputDir     string // Default output directory
	ExtractKernel bool   // Whether to extract kernel from images
	NeedsInitrd   bool   // Whether to extract initrd (VZ only)
}

// vzBuildContext returns the VZ-shaped per-target settings. OutputDir is
// filled in by the caller (it follows host OS, not target).
func vzBuildContext() buildContext {
	return buildContext{
		Prefix:        "shed-vz-",
		Platform:      vmimage.DefaultPlatform,
		ExtractKernel: true,
		NeedsInitrd:   true,
	}
}

// fcBuildContext returns the Firecracker-shaped per-target settings.
// OutputDir is filled in by the caller (it follows host OS, not target).
func fcBuildContext() buildContext {
	return buildContext{
		Prefix:        "shed-fc-",
		Platform:      vmimage.FirecrackerPlatform,
		ExtractKernel: true,
		NeedsInitrd:   false,
	}
}

// hostDefaultOutputDir returns the local-OCI-store path appropriate for
// goos. Extracted as a helper so imageBackendContext stays unit-testable
// without mocking runtime.GOOS.
func hostDefaultOutputDir(goos string) string {
	if goos == "linux" {
		return config.DefaultFirecrackerImagesDir
	}
	return config.ExpandPath(config.DefaultVZImagesDir)
}

// imageBackendContext returns build settings for the requested target.
//
// Resolution order:
//  1. If target has a `shed-vz-` / `shed-fc-` prefix, return that
//     backend's context. This covers the CI cross-build case (e.g.,
//     building shed-vz-* from a Linux runner) where the host OS and
//     the target backend disagree — without this fork, every
//     non-OutputDir field would silently flip to the host's default.
//  2. Otherwise fall back to the host OS default (vz on macOS, fc on
//     linux). This is the day-to-day local-developer case where
//     `shed image build -n myimg` should infer the backend from the
//     machine running the build.
//
// OutputDir always follows the host OS: it's the location of the
// on-disk OCI store, which has nothing to do with the image's target
// architecture. Callers may override with --output-dir.
func imageBackendContext(target string) buildContext {
	return imageBackendContextForHost(target, runtime.GOOS)
}

// imageBackendContextForHost is the unit-testable inner of
// imageBackendContext. goos is the host operating system identifier
// (matches runtime.GOOS values: "linux", "darwin", ...).
func imageBackendContextForHost(target, goos string) buildContext {
	var bc buildContext
	switch {
	case strings.HasPrefix(target, "shed-vz-"):
		bc = vzBuildContext()
	case strings.HasPrefix(target, "shed-fc-"):
		bc = fcBuildContext()
	default:
		if goos == "linux" {
			bc = fcBuildContext()
		} else {
			bc = vzBuildContext()
		}
	}
	bc.OutputDir = hostDefaultOutputDir(goos)
	return bc
}

func runImageBuild(cmd *cobra.Command, args []string) error {
	bc := imageBackendContext(imageBuildTarget)

	// --platform CLI flag remains an explicit override on top of the
	// target-driven default in bc.Platform (e.g., for non-shed-* targets
	// where the user wants an arch that disagrees with the host).
	platform := imageBuildPlatform
	if platform == "" {
		platform = bc.Platform
	}

	outputDir := imageBuildOutputDir
	if outputDir == "" {
		outputDir = bc.OutputDir
	}

	return runImageBuildFromDockerfile(cmd.Context(), args, outputDir, bc.Prefix, platform, bc.ExtractKernel, bc.NeedsInitrd)
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

	// sourceRef is what gets baked into the manifest's
	// io.shed.source-ref annotation, which the server's resolveImage
	// cache-hit check (internal/config/server.go) compares against
	// the configured `ref:` to decide whether a re-pull is needed.
	// Default to the local buildx tag for ad-hoc developer builds;
	// publish workflows override with the final registry ref so
	// remote `shed create` pulls hit the cache.
	sourceRef := imageBuildSourceRef
	if sourceRef == "" {
		sourceRef = dockerTag
	}

	// Emit buildx output as an OCI image-layout tar so shed's Convert
	// ingests the full layer structure (Dockerfile stages that `FROM`
	// each other share layers in the OCI store, which lights up the
	// SHARED column in `shed image ls`). The legacy --load + docker
	// create/export flatten path collapsed everything to one layer.
	ociTar, err := os.CreateTemp("", "shed-build-*.tar")
	if err != nil {
		return fmt.Errorf("creating build output tempfile: %w", err)
	}
	ociTarPath := ociTar.Name()
	ociTar.Close()
	defer os.Remove(ociTarPath)

	fmt.Printf("Building Docker image %s (OCI output → %s)...\n", dockerTag, ociTarPath)
	buildArgs := []string{
		"buildx", "build", "--platform", platform,
		"--output", "type=oci,dest=" + ociTarPath,
		"-f", dockerfile,
	}
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

	fmt.Printf("\nIngesting OCI layers into shed store...\n")
	digest, err := convertAndInstall(ctx, sourceRef, ociTarPath, outputDir, platform, extractKernel, needsInitrd)
	if err != nil {
		return err
	}
	finishImageBuild(outputDir, sourceRef, digest)
	return nil
}

// convertAndInstall runs the OCI Convert flow against `imagesDir`,
// which writes the manifest+config+layer+kernel+initrd blobs into the
// OCI layout and materializes a derived ext4 in the cache, then
// advances the imageBuildName tag to the new manifest digest.
//
// sourceRef is recorded verbatim in the manifest's io.shed.source-ref
// annotation; pass the final registry ref here when this image will be
// pushed, otherwise the server's resolveImage cache check (which
// compares `manifest.ShedSourceRef() == cfg.ref`) will miss on every
// subsequent pull. Returns the manifest digest.
func convertAndInstall(ctx context.Context, sourceRef, ociArchivePath, imagesDir, platform string, extractKernel, needsInitrd bool) (string, error) {
	if err := vmimage.ValidateImageName(imageBuildName); err != nil {
		return "", fmt.Errorf("invalid image name %q: %w", imageBuildName, err)
	}

	result, err := vmimage.Convert(ctx, vmimage.ConvertOptions{
		OCIArchivePath:   ociArchivePath,
		DockerRef:        sourceRef,
		Name:             imageBuildName,
		ImagesDir:        imagesDir,
		RootfsSize:       imageBuildSize,
		Platform:         platform,
		ExtractKernel:    extractKernel,
		NeedsInitrd:      needsInitrd,
		InitrdSourcePath: imageBuildInitrd,
	})
	if err != nil {
		return "", fmt.Errorf("conversion failed: %w", err)
	}
	if err := vmimage.SetTag(imagesDir, imageBuildName, result.ManifestDigest); err != nil {
		return "", fmt.Errorf("advancing tag %q: %w", imageBuildName, err)
	}
	return result.ManifestDigest, nil
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

	// `shed image rm` removes only the tag; the underlying blob persists
	// until `shed image prune` reclaims it (Docker model). Reporting
	// "freed X" here would mislead operators about real disk recovery.
	printSuccess("Removed image tag %s (run `shed image prune` to reclaim disk)", name)
	_ = targetImage
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
	if resp.Manifest.Variant != "" {
		fmt.Fprintf(w, "Variant:\t%s\n", resp.Manifest.Variant)
	}
	if resp.Manifest.SourceRef != "" {
		fmt.Fprintf(w, "Source ref:\t%s\n", resp.Manifest.SourceRef)
	}
	if len(resp.Manifest.Layers) > 0 {
		fmt.Fprintf(w, "Layers:\t%d\n", len(resp.Manifest.Layers))
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
	resp, err := client.PullImage(dockerRef, tag, imagePullPlatform)
	if err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}
	if jsonFlag {
		return outputJSON(resp)
	}
	printSuccess("Pulled %s as tag %q (%s)", dockerRef, resp.Tag, vmimage.ShortDigest(resp.Digest))
	return nil
}

func runImagePush(_ *cobra.Command, args []string) error {
	source := args[0]
	dest := args[1]

	// Local mode: --local explicit, or -c <config> passed without
	// -s <server>. Otherwise talk to a running shed-server.
	useLocal := imagePushLocal || (configFlag != "" && serverFlag == "")
	if useLocal {
		mgr, err := loadLocalManager()
		if err != nil {
			return err
		}
		if err := mgr.PushImage(context.Background(), source, dest, func(stage, msg string) {
			fmt.Printf("  %s: %s\n", stage, msg)
		}); err != nil {
			return fmt.Errorf("failed to push image: %w", err)
		}
		printSuccess("Pushed %s → %s", source, dest)
		return nil
	}

	entry, _, err := getServerEntry()
	if err != nil {
		return err
	}
	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	resp, err := client.PushImage(source, dest)
	if err != nil {
		return fmt.Errorf("failed to push image: %w", err)
	}
	if jsonFlag {
		return outputJSON(resp)
	}
	printSuccess("Pushed %s → %s", resp.Source, resp.Destination)
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
	// --json + --dry-run is always safe (read-only). Only require
	// --force when --json is combined with an execute pass, matching
	// the system-prune ergonomics.
	if jsonFlag && !imagePruneForce && !imagePruneDryRun {
		return fmt.Errorf("--force is required when combining --json with an execute pass (add --dry-run to preview)")
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
