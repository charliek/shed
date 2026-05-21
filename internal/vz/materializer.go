//go:build darwin
// +build darwin

// Materializer VM: a one-shot vfkit invocation that converts an OCI
// layer tar.gz blob into a read-only erofs image. The VM boots a shed
// kernel + initramfs whose `shed.mode=materialize` cmdline branch
// (initramfs/init) streams the gzipped tar through
// `mkfs.erofs --tar=f` in a single pass and powers off. shed-server
// reaps the process and reads the resulting .erofs back from the host
// filesystem (the output device is a virtio-blk-backed file).
//
// Why streaming: the v0.5.1 first cut extracted the tar into a tmpfs
// inside the VM, then walked the tree with mkfs.erofs. For 1+ GiB
// compressed Ubuntu base layers that overran the 5-min per-VM timeout
// and forced a docker fallback. Streaming collapses the two passes
// into one and drops the in-guest working set from ~3-4 GiB to a few
// hundred MiB. Requires erofs-utils 1.7+; the initramfs base
// (initramfs/Dockerfile) bumps to debian:trixie-slim for 1.8.6.
//
// This replaces the legacy `docker run ubuntu:24.04 mkfs.ext4`
// pipeline on Mac. The docker pipeline ran apt-get inside the
// container on every materialize, hanging Docker Desktop under load
// (issue #99). The materializer VM has zero Docker dependency: it
// reuses the same kernel + initrd that shed-server boots regular VMs
// with, so the only new artifact in the cache is the .erofs output.
//
// Returns ErrMaterializerUnavailable when no cached shed image
// provides a kernel + initrd to boot the materializer VM. The
// dispatcher in internal/vmimage/cache.go falls back to docker in
// that case. See ResolveMaterializerBootBlobs for the scan that
// makes the first-pull case docker-free (commit 12 of v0.5.1).

package vz

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/vmimage"
)

// MaterializerOpts configures a one-shot materializer VM.
type MaterializerOpts struct {
	// VfkitPath is the path to the vfkit binary.
	VfkitPath string

	// KernelPath / InitrdPath are blob paths resolved from the shed
	// manifest. Use ResolveMaterializerBootBlobs to pick them from any
	// locally-cached shed image.
	KernelPath string
	InitrdPath string

	// InputBlobPath is the host path to the layer tar.gz blob. Passed
	// as a read-only virtio-blk device; the in-guest initramfs runs
	// `tar -xzf <device>` directly (no filesystem layer in between).
	// This avoids needing virtio-fs / 9p modules in the initramfs.
	InputBlobPath string

	// InputDigest is the layer digest being materialized (for logging).
	InputDigest string

	// OutputPath is the host path where the resulting erofs image
	// should land. Passed to vfkit as a virtio-blk device backing file;
	// the in-guest initramfs writes mkfs.erofs output to /dev/vda.
	OutputPath string

	// CPUs and MemoryMiB sizing knobs. Defaults: 2 CPUs / 2048 MiB. The
	// materializer is I/O bound (tar extract + lz4 compress); more
	// memory mainly helps the tmpfs work area for big layers.
	CPUs      int
	MemoryMiB int

	// Timeout caps the total wall-clock budget for the materialize
	// (extract + mkfs.erofs + shutdown). Defaults to 5 minutes.
	Timeout time.Duration

	// ConsoleLogPath, if non-empty, captures vfkit + guest console
	// output for post-mortem debugging. The file is appended to,
	// matching the regular vm.Start pattern. If empty, output goes to
	// the bit bucket.
	ConsoleLogPath string
}

// RunMaterializer launches a one-shot vfkit VM that materializes an
// OCI layer tar.gz into an erofs image. Blocks until the VM exits.
// Returns nil on success, an error on exit-status != 0 or vfkit
// failure.
//
// Returns vmimage.ErrMaterializerUnavailable if the materializer
// cannot run because no kernel/initrd was provided (caller should
// resolve the boot blobs first, or fall back to the docker pipeline).
func RunMaterializer(ctx context.Context, opts MaterializerOpts) error {
	if opts.KernelPath == "" || opts.InitrdPath == "" {
		return fmt.Errorf("%w: kernel and initrd paths are required", vmimage.ErrMaterializerUnavailable)
	}
	if opts.InputBlobPath == "" || opts.OutputPath == "" {
		return fmt.Errorf("RunMaterializer: input blob path and output path are required")
	}
	if opts.VfkitPath == "" {
		opts.VfkitPath = "vfkit"
	}
	if opts.CPUs <= 0 {
		opts.CPUs = 2
	}
	if opts.MemoryMiB <= 0 {
		// The in-guest pipeline extracts the uncompressed tar tree to
		// tmpfs (busybox tar parses the GNU PAX/LongLink extensions
		// that erofs-utils 1.8.6's tar parser chokes on). Ubuntu base
		// images decompress to 3-4 GiB; 6 GiB covers it with room for
		// mkfs.erofs's working buffers. APFS doesn't actually commit
		// unused guest memory so this is a paper allocation.
		opts.MemoryMiB = 6144
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Minute
	}

	// Sanity-check the kernel + initrd before we shell out — vfkit's
	// error messages on missing files are cryptic.
	for _, p := range []string{opts.VfkitPath, opts.KernelPath, opts.InitrdPath} {
		if p == opts.VfkitPath {
			if _, err := exec.LookPath(p); err == nil {
				continue
			}
		}
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("materializer prerequisite missing (%s): %w", p, err)
		}
	}
	if _, err := os.Stat(opts.InputBlobPath); err != nil {
		return fmt.Errorf("materializer input blob missing: %w", err)
	}
	// The output path must exist as a regular file backing virtio-blk.
	// vfkit will not create it. Caller (EnsureLowerFromLayer) creates a
	// staging tempfile so this should always succeed.
	if _, err := os.Stat(opts.OutputPath); err != nil {
		return fmt.Errorf("materializer output file missing: %w", err)
	}

	// Pre-allocate the output file. mkfs.erofs writes the erofs
	// image into the device but never grows it; the file must be at
	// least as large as the final image. Use 2× the input blob's
	// compressed size as a safe upper bound — lz4's compression
	// ratio is typically worse than gzip's, so the erofs result can
	// exceed the tar.gz size for low-entropy layers. Cap at 8 GiB.
	// Sparse-allocated — APFS only physically commits the bytes
	// mkfs.erofs actually writes.
	inputInfo, ierr := os.Stat(opts.InputBlobPath)
	if ierr != nil {
		return fmt.Errorf("statting materializer input: %w", ierr)
	}
	outputSize := int64(float64(inputInfo.Size()) * 2.0)
	if outputSize < 64*1024*1024 {
		outputSize = 64 * 1024 * 1024 // floor at 64 MiB for tiny layers
	}
	if outputSize > 8*1024*1024*1024 {
		outputSize = 8 * 1024 * 1024 * 1024
	}
	// Round up to 512-byte block boundary (VZ requirement).
	outputSize = ((outputSize + 511) / 512) * 512
	if err := ensureSparseSize(opts.OutputPath, outputSize); err != nil {
		return fmt.Errorf("sizing materializer output: %w", err)
	}

	// VZ rejects virtio-blk image files whose size isn't a multiple of
	// 512 bytes ("Invalid disk image. The disk image format is not
	// recognized."). The OCI blob is an immutable content-addressed
	// file we can't pad in place, and we don't want to mutate it
	// anyway. Stage a padded copy in the same dir as the output (same
	// volume — APFS clonefile is O(1) sparse on most paths). The
	// in-guest `tar -xzf` reads the gzip stream and stops at end-of-
	// stream; trailing zero padding is ignored by gzip.
	stagedInput, stagedErr := stagePaddedBlob(opts.InputBlobPath, opts.OutputPath+".input")
	if stagedErr != nil {
		return fmt.Errorf("staging padded input blob: %w", stagedErr)
	}
	defer os.Remove(stagedInput)

	cmdline := strings.Join([]string{
		"console=hvc0",
		"shed.mode=materialize",
		"shed.input-device=/dev/vdb",
		"shed.output=/dev/vda",
		"shed.output-format=erofs",
	}, " ")

	args := []string{
		"--cpus", fmt.Sprintf("%d", opts.CPUs),
		"--memory", fmt.Sprintf("%d", opts.MemoryMiB),
		"--kernel", opts.KernelPath,
		"--initrd", opts.InitrdPath,
		"--kernel-cmdline", cmdline,
		// /dev/vda: the staging output file. Guest writes mkfs.erofs
		// output here.
		"--device", "virtio-blk,path=" + opts.OutputPath,
		// /dev/vdb: the input layer tar.gz blob (read-only, staged copy
		// padded to 512-byte alignment for VZ). Guest's initramfs does
		// `tar -xzf /dev/vdb` directly — no filesystem mount in
		// between, so we don't depend on virtio-fs or 9p kernel
		// modules being loadable from the initramfs.
		"--device", "virtio-blk,path=" + stagedInput + ",readonly",
	}
	// Console serial — gives us boot logs if the materializer panics in
	// the initramfs. vfkit's virtio-serial device wants logFilePath=
	// (NOT a bare "stdio" subkey, which vfkit rejects as
	// "operation not supported by device").
	consoleLog := opts.ConsoleLogPath
	if consoleLog == "" {
		consoleLog = opts.OutputPath + ".console.log"
	}
	args = append(args, "--device", fmt.Sprintf("virtio-serial,logFilePath=%s", consoleLog))

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, opts.VfkitPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	// Note: guest console output is captured via virtio-serial's
	// logFilePath= above, not via vfkit's stdout.

	start := time.Now()
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("materializer VM timed out after %s: %s", opts.Timeout, strings.TrimSpace(stderr.String()))
		}
		return fmt.Errorf("materializer VM failed: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	log.Printf("vz materializer: completed in %s (digest=%s)", time.Since(start), vmimage.ShortDigest(opts.InputDigest))

	// Verify the output looks like an erofs image. Magic bytes at
	// offset 1024: 0xE2 0xE1 0xF5 0xE0.
	if err := verifyErofs(opts.OutputPath); err != nil {
		return fmt.Errorf("materializer VM produced invalid erofs at %s: %w", opts.OutputPath, err)
	}
	return nil
}

// ResolveMaterializerBootBlobs picks a kernel + initrd from any
// locally-cached shed image with the relevant annotations. Two-pass:
//
//  1. Tagged images (fast path on second+ pull).
//  2. Every manifest descriptor in index.json (catches the
//     just-pulled but not-yet-tagged manifest mid-EnsureLowerFromLayer
//     loop — see registry.go:391-435 for the pull order proof).
//
// Returns vmimage.ErrMaterializerUnavailable if neither pass finds a
// candidate; the caller falls back to docker. Until the first shed
// image's manifest blob and kernel/initrd blobs land on disk, no
// materializer VM is possible.
func ResolveMaterializerBootBlobs(imagesDir string) (kernelPath, initrdPath string, err error) {
	// Pass 1: tagged images.
	if tags, terr := vmimage.ListTags(imagesDir); terr == nil {
		for _, tag := range tags {
			t, err := vmimage.GetTag(imagesDir, tag)
			if err != nil {
				continue
			}
			if kp, ip, ok := tryManifestForBootBlobs(imagesDir, t.Digest); ok {
				return kp, ip, nil
			}
		}
	}
	// Pass 2: every manifest descriptor in index.json. Catches the
	// just-pulled manifest before its tag is committed (first-pull case).
	if digests, ierr := vmimage.IndexManifestDigests(imagesDir); ierr == nil {
		for dgst := range digests {
			if kp, ip, ok := tryManifestForBootBlobs(imagesDir, dgst); ok {
				return kp, ip, nil
			}
		}
	}
	return "", "", fmt.Errorf("%w: no cached shed image provides a kernel + initrd", vmimage.ErrMaterializerUnavailable)
}

// tryManifestForBootBlobs returns paths to the kernel + initrd blobs
// annotated on the given manifest digest, if both blobs exist on disk.
// Returns ok=false for non-manifest blobs, manifests without shed
// annotations, or manifests whose blobs aren't fully cached.
func tryManifestForBootBlobs(imagesDir, manifestDigest string) (kernelPath, initrdPath string, ok bool) {
	m, err := vmimage.LoadManifestByDigest(imagesDir, manifestDigest)
	if err != nil {
		return "", "", false
	}
	kd := m.ShedKernelDigest()
	id := m.ShedInitrdDigest()
	if kd == "" || id == "" {
		return "", "", false
	}
	kp, kerr := vmimage.BlobPath(imagesDir, kd)
	if kerr != nil {
		return "", "", false
	}
	ip, ierr := vmimage.BlobPath(imagesDir, id)
	if ierr != nil {
		return "", "", false
	}
	if _, err := os.Stat(kp); err != nil {
		return "", "", false
	}
	if _, err := os.Stat(ip); err != nil {
		return "", "", false
	}
	return kp, ip, true
}

// MaterializerHook returns a vmimage.MaterializerFunc closure suitable
// for vmimage.RegisterMaterializer. The closure resolves the boot
// blobs at call time so a freshly-pulled image is picked up without
// restarting the server.
func MaterializerHook(imagesDir, vfkitPath, consoleLogPath string) vmimage.MaterializerFunc {
	return func(ctx context.Context, blobPath, outputPath, _ string) (err error) {
		kernel, initrd, rerr := ResolveMaterializerBootBlobs(imagesDir)
		if rerr != nil {
			return rerr
		}
		// blobPath is {imagesDir}/blobs/sha256/<hex>. We pass it
		// directly as the read-only input virtio-blk device — the
		// guest reads `tar -xzf /dev/vdb` from the blob.
		hex := filepath.Base(blobPath)
		digest := "sha256:" + hex
		return RunMaterializer(ctx, MaterializerOpts{
			VfkitPath:      vfkitPath,
			KernelPath:     kernel,
			InitrdPath:     initrd,
			InputBlobPath:  blobPath,
			InputDigest:    digest,
			OutputPath:     outputPath,
			ConsoleLogPath: consoleLogPath,
		})
	}
}

// stagePaddedBlob copies srcPath to a new file at dstPath padded to the
// next 512-byte block boundary. VZ requires virtio-blk image files to
// be exact multiples of 512 bytes — content-addressed OCI tar.gz blobs
// almost never are. Returns the staged path on success; caller is
// responsible for removing it. Uses APFS clonefile when available
// (instantaneous + sparse) and falls back to a regular copy otherwise.
func stagePaddedBlob(srcPath, dstPath string) (string, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()
	srcInfo, err := src.Stat()
	if err != nil {
		return "", err
	}
	dst, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		_ = os.Remove(dstPath)
		return "", fmt.Errorf("copy: %w", err)
	}
	// Pad up to the next 512-byte boundary.
	const blockSize = 512
	size := srcInfo.Size()
	padded := ((size + blockSize - 1) / blockSize) * blockSize
	if padded == size {
		padded = size + blockSize // ensure at least one trailing block; harmless
	}
	if err := dst.Truncate(padded); err != nil {
		_ = os.Remove(dstPath)
		return "", fmt.Errorf("truncate: %w", err)
	}
	return dstPath, nil
}

// ensureSparseSize grows path to at least sizeBytes via truncate.
// Idempotent: a file already at-or-above the target size is left alone.
func ensureSparseSize(path string, sizeBytes int64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Size() >= sizeBytes {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(sizeBytes)
}

// verifyErofs spot-checks that path's magic bytes match erofs.
// Reference: fs/erofs/internal.h EROFS_SUPER_MAGIC_V1 = 0xE0F5E1E2 LE
// at offset 1024.
func verifyErofs(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf [4]byte
	if _, err := f.ReadAt(buf[:], 1024); err != nil {
		return fmt.Errorf("reading magic: %w", err)
	}
	want := [4]byte{0xE2, 0xE1, 0xF5, 0xE0}
	if buf != want {
		return fmt.Errorf("magic mismatch: got %x want %x", buf, want)
	}
	return nil
}
