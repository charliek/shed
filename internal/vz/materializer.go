//go:build darwin
// +build darwin

// Materializer VM: a one-shot vfkit invocation that converts an OCI
// layer tar.gz blob into a read-only erofs image. The VM boots a shed
// kernel + initramfs whose `shed.mode=materialize` cmdline branch
// (initramfs/init) extracts the layer and runs mkfs.erofs natively,
// then powers off. shed-server reaps the process and reads the
// resulting .erofs back from the host filesystem (the output device is
// a virtio-blk-backed file).
//
// This replaces the legacy `docker run ubuntu:24.04 mkfs.ext4`
// pipeline on Mac. The docker pipeline ran apt-get inside the
// container on every materialize, hanging Docker Desktop under load
// (issue #99). The materializer VM has zero Docker dependency: it
// reuses the same kernel + initrd that shed-server boots regular VMs
// with, so the only new artifact in the cache is the .erofs output.
//
// Falls back via ErrMaterializerUnavailable when no shed kernel is
// cached locally — that's the first-pull case, where the dispatcher in
// internal/vmimage/cache.go drops back to the docker pipeline. Once
// any shed image is cached the materializer VM takes over for
// subsequent layers.

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

	// InputBlobsDir is the host path shared into the VM read-only via
	// VirtioFS. Inside the VM the materializer mounts it at /input;
	// the layer tar.gz lives at /input/sha256/<hex>. Typical value:
	// "{imagesDir}/blobs".
	InputBlobsDir string

	// InputDigest is the layer digest to materialize ("sha256:..."). The
	// initramfs reads this from shed.input= on the cmdline and locates
	// the blob inside the shared dir.
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
	if opts.InputBlobsDir == "" || opts.InputDigest == "" || opts.OutputPath == "" {
		return fmt.Errorf("RunMaterializer: input dir, input digest, and output path are required")
	}
	if opts.VfkitPath == "" {
		opts.VfkitPath = "vfkit"
	}
	if opts.CPUs <= 0 {
		opts.CPUs = 2
	}
	if opts.MemoryMiB <= 0 {
		opts.MemoryMiB = 2048
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
	if _, err := os.Stat(opts.InputBlobsDir); err != nil {
		return fmt.Errorf("materializer input dir missing: %w", err)
	}
	// The output path must exist as a regular file backing virtio-blk.
	// vfkit will not create it. Caller (EnsureLowerFromLayer) creates a
	// staging tempfile so this should always succeed.
	if _, err := os.Stat(opts.OutputPath); err != nil {
		return fmt.Errorf("materializer output file missing: %w", err)
	}

	// Pre-allocate the output file. erofs needs enough space to land
	// its output; truncating to a generous size + sparse holes is
	// cheap on APFS / ext4. Skip if the file is already non-empty
	// (caller may have sized it).
	if err := ensureSparseSize(opts.OutputPath, 2*1024*1024*1024); err != nil {
		return fmt.Errorf("sizing materializer output: %w", err)
	}

	cmdline := strings.Join([]string{
		"console=hvc0",
		"shed.mode=materialize",
		"shed.input=" + opts.InputDigest,
		"shed.output=/dev/vda",
		"shed.output-format=erofs",
	}, " ")

	args := []string{
		"--cpus", fmt.Sprintf("%d", opts.CPUs),
		"--memory", fmt.Sprintf("%d", opts.MemoryMiB),
		"--kernel", opts.KernelPath,
		"--initrd", opts.InitrdPath,
		"--kernel-cmdline", cmdline,
		// Output device: virtio-blk-backed by the staging file. Guest
		// writes mkfs.erofs output here.
		"--device", "virtio-blk,path=" + opts.OutputPath,
		// Read-only VirtioFS share of the blobs directory. The guest
		// reads the layer tar.gz directly from the host store.
		"--device", fmt.Sprintf("virtio-fs,sharedDir=%s,mountTag=blobs", opts.InputBlobsDir),
		// Console serial — gives us boot logs if the materializer
		// panics in the initramfs.
		"--device", "virtio-serial,stdio",
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, opts.VfkitPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if opts.ConsoleLogPath != "" {
		// Use the log file as stdout so the guest serial console gets
		// captured. Stderr stays buffered so we can include it in the
		// error message.
		f, err := os.OpenFile(opts.ConsoleLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return fmt.Errorf("opening materializer console log: %w", err)
		}
		defer f.Close()
		cmd.Stdout = f
	} else {
		cmd.Stdout = io.Discard
	}

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
// locally-cached shed image with the relevant annotations. Returns
// vmimage.ErrMaterializerUnavailable if no candidate image is cached
// yet (caller falls back to the docker pipeline).
func ResolveMaterializerBootBlobs(imagesDir string) (kernelPath, initrdPath string, err error) {
	tags, terr := vmimage.ListTags(imagesDir)
	if terr != nil {
		return "", "", fmt.Errorf("listing tags: %w", terr)
	}
	// Try every tag — pick the first that has both kernel + initrd
	// annotations and where both blobs are on disk.
	for _, tag := range tags {
		t, err := vmimage.GetTag(imagesDir, tag)
		if err != nil {
			continue
		}
		m, err := vmimage.LoadManifestByDigest(imagesDir, t.Digest)
		if err != nil {
			continue
		}
		kd := m.ShedKernelDigest()
		id := m.ShedInitrdDigest()
		if kd == "" || id == "" {
			continue
		}
		kp, kerr := vmimage.BlobPath(imagesDir, kd)
		if kerr != nil {
			continue
		}
		ip, ierr := vmimage.BlobPath(imagesDir, id)
		if ierr != nil {
			continue
		}
		if _, err := os.Stat(kp); err != nil {
			continue
		}
		if _, err := os.Stat(ip); err != nil {
			continue
		}
		return kp, ip, nil
	}
	return "", "", fmt.Errorf("%w: no cached shed image provides a kernel + initrd", vmimage.ErrMaterializerUnavailable)
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
		// blobPath is {imagesDir}/blobs/sha256/<hex>. We pass the
		// parent of sha256/ as the share root so the guest sees a
		// stable directory layout regardless of which shed image we
		// picked for boot.
		blobsDir := filepath.Dir(filepath.Dir(blobPath))
		hex := filepath.Base(blobPath)
		digest := "sha256:" + hex
		return RunMaterializer(ctx, MaterializerOpts{
			VfkitPath:      vfkitPath,
			KernelPath:     kernel,
			InitrdPath:     initrd,
			InputBlobsDir:  blobsDir,
			InputDigest:    digest,
			OutputPath:     outputPath,
			ConsoleLogPath: consoleLogPath,
		})
	}
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
