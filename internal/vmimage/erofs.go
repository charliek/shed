// Erofs blob minting for shed images at publish time.
//
// Prior to v0.5.2 the read-only rootfs erofs was built lazily on the
// host at first `shed create` via local mkfs.erofs (see cache.go).
// That coupled the on-disk format to whatever erofs-utils the host
// distro shipped — and erofs-utils 1.7.1 (current Ubuntu noble) has
// a writer bug where `-E force-inode-compact` emits per-inode
// big-pcluster headers without the matching superblock feature flag.
// Resulting filesystems can't be mounted by any kernel.
//
// v0.5.2+: the publisher (this file, called from `shed image build`)
// runs a pinned mkfs.erofs from the shed-build-tools OCI image inside
// a docker container, writes the erofs as a content-addressed blob in
// the local OCI store, and stamps the digest into the manifest's
// io.shed.rootfs.erofs.digest annotation. On `shed image push` the
// blob rides along as a loose sibling (registry.go:PushFromOCILayout
// already walks the annotation list). On pull the host fetches the
// blob and mounts it directly — no local mkfs.erofs. See
// docs/reference/storage-model.md for the full picture.

package vmimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MintErofsOptions controls what MintRootfsErofs invokes on the
// publishing host.
type MintErofsOptions struct {
	// ImagesDir is the OCI store the layer blobs are already
	// installed in and where the new erofs blob will be written.
	ImagesDir string

	// LayerDigests are the OCI manifest's layer digests in OCI order
	// (lowest at index 0). MergeLayers flattens them into a single
	// tarball before feeding mkfs.erofs.
	LayerDigests []string

	// BuildToolsRef is the OCI image carrying mkfs.erofs that will
	// run inside `docker run --rm ...`. Pin to the same tag as the
	// shed-server version being released — see
	// docs/reference/build-tools.md for the versioning model.
	BuildToolsRef string

	// DockerBinary defaults to "docker"; tests / unusual hosts
	// override (e.g. "podman" with a compat wrapper).
	DockerBinary string
}

// MintRootfsErofs flattens layerDigests, runs mkfs.erofs inside the
// build-tools container, and installs the resulting erofs as a
// content-addressed blob in imagesDir. Returns the blob digest
// (sha256:...) — caller stamps this into the manifest's
// io.shed.rootfs.erofs.digest annotation.
//
// The mkfs.erofs invocation pins:
//
//   - `-b 4096`: erofs block size. Host page sizes vary (4 KiB on
//     Linux/amd64, 16 KiB on Apple Silicon); fixing the block size
//     keeps the same erofs mountable in both contexts. The guest
//     kernel expects 4 KiB.
//   - `-z lz4`: lz4 compression. Random-read friendly, ~50% on-disk
//     reduction vs raw, decompression is cheap enough that the boot
//     path doesn't measurably slow down.
//   - `-E force-inode-compact`: 32-byte inodes (vs the default
//     64-byte extended layout). Saves disk for image rootfs's
//     thousands of files. The 1.7.x writer bug that motivated this
//     whole change was an interaction between this flag and big
//     pcluster headers; mkfs.erofs 1.8+ (which ships in
//     shed-build-tools) fixes it.
//   - `-T 0`: zeroes the per-inode mtime field. Without this the
//     erofs digest would vary by clock skew on the build host,
//     breaking content addressing for byte-for-byte identical
//     rootfs content.
//
// Changing any of these flags changes the produced digest. The
// shed-build-tools image is versioned in lockstep with shed-server
// so a flag change rides a known release.
func MintRootfsErofs(ctx context.Context, opts MintErofsOptions) (string, error) {
	if opts.ImagesDir == "" {
		return "", errors.New("MintRootfsErofs: ImagesDir is required")
	}
	if len(opts.LayerDigests) == 0 {
		return "", errors.New("MintRootfsErofs: at least one layer digest is required")
	}
	if opts.BuildToolsRef == "" {
		return "", errors.New("MintRootfsErofs: BuildToolsRef is required (e.g. ghcr.io/charliek/shed-build-tools:vX.Y.Z)")
	}
	docker := opts.DockerBinary
	if docker == "" {
		docker = "docker"
	}
	if _, err := exec.LookPath(docker); err != nil {
		return "", fmt.Errorf("MintRootfsErofs: %s not on PATH: %w", docker, err)
	}

	// Flatten layers into a single tar so mkfs.erofs can ingest via
	// --tar=f. Stage in a fresh tempdir so the container's bind
	// mounts don't see anything else.
	stagingDir, err := os.MkdirTemp("", "shed-mint-erofs-*")
	if err != nil {
		return "", fmt.Errorf("creating staging dir: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	tarPath := filepath.Join(stagingDir, "rootfs.tar")
	if err := MergeLayers(ctx, opts.ImagesDir, opts.LayerDigests, tarPath); err != nil {
		return "", fmt.Errorf("flattening layers into tar: %w", err)
	}

	// Output path the container will write to.
	erofsName := "rootfs.erofs"
	erofsPath := filepath.Join(stagingDir, erofsName)

	// Mount the staging dir into /shed/work inside the container.
	// Read-only would be nicer for the tar input, but mkfs.erofs
	// needs to write the output into the same mount, and splitting
	// into two mounts on the same parent dir hits docker's path
	// overlap rejection.
	dockerArgs := []string{
		"run",
		"--rm",
		// Run as the host's UID so the output erofs ends up
		// readable to non-root callers (CI runs as a normal user).
		"-u", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", stagingDir + ":/shed/work",
		"-w", "/shed/work",
		opts.BuildToolsRef,
		// Default entrypoint is mkfs.erofs (see build-tools/Dockerfile).
		"--quiet",
		"-b", "4096",
		"-z", "lz4",
		"-E", "force-inode-compact",
		"-T", "0",
		"--tar=f",
		"/shed/work/" + erofsName,
		"/shed/work/rootfs.tar",
	}
	cmd := exec.CommandContext(ctx, docker, dockerArgs...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr // mkfs.erofs prints stats to stdout — include in failure output
	if err := cmd.Run(); err != nil {
		out := strings.TrimSpace(stderr.String())
		if out == "" {
			out = "(no output)"
		}
		return "", fmt.Errorf("mkfs.erofs via %s %s: %w\n%s", docker, opts.BuildToolsRef, err, out)
	}

	// Hash the produced erofs and install it as a content-addressed
	// blob. Streaming the hash and the install separately is fine
	// because the staging file isn't going anywhere until the
	// surrounding tempdir is removed.
	digest, err := sha256File(erofsPath)
	if err != nil {
		return "", fmt.Errorf("hashing minted erofs: %w", err)
	}
	digestStr := DigestPrefix + digest

	if BlobExists(opts.ImagesDir, digestStr) {
		// Identical content already in the store (likely a sibling
		// variant whose layers + flags collapsed to the same blob —
		// or a retry). No-op; reuse the existing blob.
		return digestStr, nil
	}

	dstPath, err := BlobPath(opts.ImagesDir, digestStr)
	if err != nil {
		return "", fmt.Errorf("resolving destination blob path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return "", fmt.Errorf("creating blob dir: %w", err)
	}
	// Try a rename first (same filesystem fast path); fall back to
	// copy if the staging dir lives on a different mount than the
	// blob store (`/tmp` vs `/var/lib/shed/...`).
	if err := os.Rename(erofsPath, dstPath); err != nil {
		if copyErr := copyFile(erofsPath, dstPath); copyErr != nil {
			return "", fmt.Errorf("installing erofs blob: rename=%v copy=%w", err, copyErr)
		}
	}
	// 0o444 mirrors how WriteBlob installs blobs — read-only so
	// pruning is the only intentional remove path.
	if err := os.Chmod(dstPath, 0o444); err != nil {
		return "", fmt.Errorf("chmod erofs blob: %w", err)
	}

	return digestStr, nil
}

// sha256File streams a file's contents through SHA-256 and returns
// the hex digest (without the "sha256:" prefix).
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
