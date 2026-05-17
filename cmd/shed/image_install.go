package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

// loadServerConfigForImageInstall reads the active server config so
// imageInstall can find images_dir without forcing the user to pass
// it on every invocation. Defined here (not as a method elsewhere)
// because all other CLI commands talk to a running shed-server via
// HTTP; this is the only local-filesystem subcommand.
func loadServerConfigForImageInstall() (*config.ServerConfig, error) {
	return config.LoadServerConfig()
}

// imageInstallCmd is the host-side "I have rootfs/kernel/initrd files
// already on disk; install them into the content-addressed blob store
// and (optionally) advance a tag" subcommand. Replaces the legacy
// scripts/install-blob.sh so the build pipeline can stop reinventing
// the atomic-install protocol in bash.
//
// Operates locally — does NOT talk to a shed-server. The caller is
// expected to have direct filesystem access to images_dir. Used by
// scripts/build-{vz,firecracker}-rootfs.sh and is otherwise rarely
// invoked by hand.
var imageInstallCmd = &cobra.Command{
	Use:   "install --rootfs <path> [flags]",
	Short: "Install a pre-built rootfs into the local blob store",
	Long: `Install a pre-built rootfs.ext4 (plus optional kernel and initrd)
directly into the content-addressed blob store under images_dir, and
optionally advance a named tag to point at the resulting digest.

This is the structural replacement for scripts/install-blob.sh. The
inputs are local files, the output is an atomic blob layout that
'shed image ls' / 'shed image inspect' / 'shed image prune' see.

The digest is computed as sha256(rootfs.ext4). InstallBlob verifies the
hash, holds a per-digest flock, and renames a sibling .tmp directory
into place so partial installs are never visible. The manifest records
backend, arch, source ref, and file sizes.

Typical use:

  shed image install \
    --rootfs ./default-rootfs.ext4 \
    --kernel ./vmlinux \
    --initrd ./initrd.img \
    --tag default \
    --backend firecracker \
    --arch amd64 \
    --source-ref ghcr.io/charliek/shed-fc-default:v1.0.0

When --consume is set, the source files are moved (not copied) into
the blob layout so the build pipeline doesn't leak intermediates.

This command writes directly to images_dir on disk. It does NOT go
through a shed-server, so the invoking user needs write access to the
target directory.`,
	RunE: runImageInstall,
}

var (
	imageInstallRootfs    string
	imageInstallKernel    string
	imageInstallInitrd    string
	imageInstallTag       string
	imageInstallBackend   string
	imageInstallArch      string
	imageInstallSourceRef string
	imageInstallImagesDir string
	imageInstallConsume   bool
)

func init() {
	imageInstallCmd.Flags().StringVar(&imageInstallRootfs, "rootfs", "", "Path to rootfs.ext4 (required)")
	imageInstallCmd.Flags().StringVar(&imageInstallKernel, "kernel", "", "Path to the boot kernel (optional)")
	imageInstallCmd.Flags().StringVar(&imageInstallInitrd, "initrd", "", "Path to the boot initrd (optional)")
	imageInstallCmd.Flags().StringVar(&imageInstallTag, "tag", "", "Tag to advance to the resulting digest (optional)")
	imageInstallCmd.Flags().StringVar(&imageInstallBackend, "backend", "", "Backend recorded in the manifest (vz|firecracker)")
	imageInstallCmd.Flags().StringVar(&imageInstallArch, "arch", "", "Architecture recorded in the manifest (arm64|amd64)")
	imageInstallCmd.Flags().StringVar(&imageInstallSourceRef, "source-ref", "", "Docker source reference recorded in the manifest (optional)")
	imageInstallCmd.Flags().StringVar(&imageInstallImagesDir, "images-dir", "", "Override the images_dir from server config (otherwise reads from server.yaml search path)")
	imageInstallCmd.Flags().BoolVar(&imageInstallConsume, "consume", false, "Move source files into the blob instead of copying (leaves nothing behind)")
	_ = imageInstallCmd.MarkFlagRequired("rootfs")

	imageCmd.AddCommand(imageInstallCmd)
}

func runImageInstall(cmd *cobra.Command, args []string) error {
	if imageInstallRootfs == "" {
		return fmt.Errorf("--rootfs is required")
	}
	if _, err := os.Stat(imageInstallRootfs); err != nil {
		return fmt.Errorf("rootfs not found: %w", err)
	}

	// Resolve images_dir. Explicit --images-dir wins; otherwise read
	// it from the active server config. The build pipeline almost
	// always uses --images-dir because it knows the right path; the
	// fallback is for one-off operator use.
	imagesDir, err := resolveImagesDir(imageInstallImagesDir, imageInstallBackend)
	if err != nil {
		return err
	}

	// Validate tag if one was supplied — fail fast rather than after
	// running Convert + InstallBlob and finding out at SetTag.
	if imageInstallTag != "" {
		if err := vmimage.ValidateImageName(imageInstallTag); err != nil {
			return fmt.Errorf("invalid --tag: %w", err)
		}
	}

	// Compute the digest the InstallBlob will verify against.
	digest, err := vmimage.HashFile(imageInstallRootfs)
	if err != nil {
		return fmt.Errorf("hashing rootfs: %w", err)
	}

	files := map[string]string{vmimage.BlobRootfsFilename: imageInstallRootfs}
	if imageInstallKernel != "" {
		if _, err := os.Stat(imageInstallKernel); err != nil {
			return fmt.Errorf("kernel not found: %w", err)
		}
		files[vmimage.BlobKernelFilename] = imageInstallKernel
	}
	if imageInstallInitrd != "" {
		if _, err := os.Stat(imageInstallInitrd); err != nil {
			return fmt.Errorf("initrd not found: %w", err)
		}
		files[vmimage.BlobInitrdFilename] = imageInstallInitrd
	}

	// If --consume wasn't set, copy each source file into a temp dir
	// so InstallBlob's rename can move *those* into the blob layout
	// rather than the originals.
	if !imageInstallConsume {
		stagingDir, err := os.MkdirTemp("", "shed-image-install-*")
		if err != nil {
			return fmt.Errorf("creating staging dir: %w", err)
		}
		defer os.RemoveAll(stagingDir)
		for name, src := range files {
			dst := filepath.Join(stagingDir, name)
			if err := copyFile(src, dst); err != nil {
				return fmt.Errorf("staging %s: %w", name, err)
			}
			files[name] = dst
		}
	}

	manifest := vmimage.Manifest{
		SchemaVersion: vmimage.ManifestSchemaVersion,
		Digest:        digest,
		Backend:       imageInstallBackend,
		Arch:          imageInstallArch,
		SourceRef:     imageInstallSourceRef,
		CreatedAt:     time.Now().UTC(),
	}
	if fi, err := os.Stat(imageInstallRootfs); err == nil {
		manifest.RootfsLogicalSize = fi.Size()
	}
	if imageInstallKernel != "" {
		if fi, err := os.Stat(imageInstallKernel); err == nil {
			manifest.KernelSize = fi.Size()
		}
	}
	if imageInstallInitrd != "" {
		if fi, err := os.Stat(imageInstallInitrd); err == nil {
			manifest.InitrdSize = fi.Size()
		}
	}

	blobDir, alreadyPresent, err := vmimage.InstallBlob(imagesDir, vmimage.BlobInstallSpec{
		Files:    files,
		Manifest: manifest,
	})
	if err != nil {
		return fmt.Errorf("installing blob: %w", err)
	}

	if imageInstallTag != "" {
		if err := vmimage.SetTag(imagesDir, imageInstallTag, digest); err != nil {
			return fmt.Errorf("advancing tag %q: %w", imageInstallTag, err)
		}
	}

	state := "installed"
	if alreadyPresent {
		state = "already-installed"
	}

	if jsonFlag {
		return outputJSON(map[string]any{
			"status":          "ok",
			"digest":          digest,
			"blob_dir":        blobDir,
			"already_present": alreadyPresent,
			"tag":             imageInstallTag,
		})
	}

	fmt.Printf("Digest: %s\n", digest)
	fmt.Printf("Blob:   %s (%s)\n", blobDir, state)
	if imageInstallTag != "" {
		fmt.Printf("Tag:    %s -> %s\n", imageInstallTag, vmimage.ShortDigest(digest))
	}
	return nil
}

// resolveImagesDir picks the images_dir to install into. Explicit
// --images-dir wins. Otherwise we read the active server config and
// pick the directory for the requested backend (or fall back to the
// non-empty one if only one backend block is populated).
func resolveImagesDir(explicit, backend string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	cfg, err := loadServerConfigForImageInstall()
	if err != nil {
		return "", err
	}
	switch backend {
	case "firecracker":
		if cfg.Firecracker == nil || cfg.Firecracker.ImagesDir == "" {
			return "", fmt.Errorf("server config has no firecracker.images_dir; pass --images-dir explicitly")
		}
		return cfg.Firecracker.ImagesDir, nil
	case "vz":
		if cfg.VZ == nil || cfg.VZ.ImagesDir == "" {
			return "", fmt.Errorf("server config has no vz.images_dir; pass --images-dir explicitly")
		}
		return cfg.VZ.ImagesDir, nil
	case "":
		// Auto-detect: prefer the backend whose config block is
		// populated and whose ImagesDir is non-empty.
		if cfg.Firecracker != nil && cfg.Firecracker.ImagesDir != "" {
			return cfg.Firecracker.ImagesDir, nil
		}
		if cfg.VZ != nil && cfg.VZ.ImagesDir != "" {
			return cfg.VZ.ImagesDir, nil
		}
		return "", fmt.Errorf("server config has no images_dir for either backend; pass --images-dir explicitly")
	default:
		return "", fmt.Errorf("unknown --backend %q (want vz|firecracker)", backend)
	}
}

// copyFile is a small file copier used when --consume isn't set so
// the original input files survive the install. io.Copy is fine here
// since the inputs are local-disk files of bounded size (a couple GB
// at most).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
