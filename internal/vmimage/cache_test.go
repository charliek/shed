package vmimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// makeTarGz writes a minimal tar.gz containing a few files + a symlink
// to outPath. Returns nothing — callers Fatal on errors.
func makeTarGz(t *testing.T, outPath string, entries map[string]string, symlinks map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range entries {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write content: %v", err)
		}
	}
	for name, target := range symlinks {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o777,
			Typeflag: tar.TypeSymlink,
			Linkname: target,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write symlink header: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write tar.gz: %v", err)
	}
}

func TestExtractTarGz(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "layer.tar.gz")
	dst := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeTarGz(t, src,
		map[string]string{
			"etc/hostname":    "shed-test\n",
			"usr/bin/sh":      "#!/bin/sh\n",
			"var/log/app.log": "hello world",
		},
		map[string]string{
			"bin/sh": "../usr/bin/sh",
		},
	)
	if err := extractTarGz(src, dst); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	for path, want := range map[string]string{
		"etc/hostname":    "shed-test\n",
		"usr/bin/sh":      "#!/bin/sh\n",
		"var/log/app.log": "hello world",
	} {
		got, err := os.ReadFile(filepath.Join(dst, path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	target, err := os.Readlink(filepath.Join(dst, "bin/sh"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "../usr/bin/sh" {
		t.Errorf("symlink target = %q, want ../usr/bin/sh", target)
	}
}

func TestExtractTarGzSkipsEscapingPaths(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "evil.tar.gz")
	dst := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Try to escape into the parent directory. extractTarGz must
	// silently skip these (or, at worst, write them under dst).
	makeTarGz(t, src, map[string]string{
		"../escaped":             "should never land outside dst",
		"normal/within/dst.file": "ok",
	}, nil)
	if err := extractTarGz(src, dst); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "escaped")); err == nil {
		t.Errorf("escaped path landed outside dst: %s/escaped exists", tmp)
	}
}

func TestMaterializeNativeLinuxOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("native materializer requires linux (got %s)", runtime.GOOS)
	}
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skipf("mkfs.erofs not installed: %v", err)
	}

	tmp := t.TempDir()
	src := filepath.Join(tmp, "layer.tar.gz")
	out := filepath.Join(tmp, "layer.erofs")
	makeTarGz(t, src, map[string]string{
		"etc/release":  "shed-erofs-smoke\n",
		"etc/hostname": "smoke\n",
	}, nil)

	if err := materializeNativeLinux(context.Background(), src, out); err != nil {
		t.Fatalf("materializeNativeLinux: %v", err)
	}

	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatalf("output erofs is empty")
	}
	// Verify magic bytes — same check the VZ materializer uses.
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var magic [4]byte
	if _, err := f.ReadAt(magic[:], 1024); err != nil {
		t.Fatalf("read magic: %v", err)
	}
	want := [4]byte{0xE2, 0xE1, 0xF5, 0xE0}
	if magic != want {
		t.Errorf("erofs magic = %x, want %x", magic, want)
	}
}

// TestEnsureLowerFromLayerLegacyCacheHit verifies that an existing
// legacy .ext4 cache file is honored without rebuilding (upgrade-in-
// place from v0.5.0 must not force re-materialization of every layer).
func TestEnsureLowerFromLayerLegacyCacheHit(t *testing.T) {
	tmp := t.TempDir()
	if err := EnsureOCILayout(tmp); err != nil {
		t.Fatalf("EnsureOCILayout: %v", err)
	}

	// Write a fake layer blob.
	content := []byte("fake-layer-content")
	digest := DigestBytes(content)
	if _, err := WriteBlob(tmp, digest, content); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}

	// Pre-seed the legacy .ext4 cache path.
	legacyPath, err := CacheLowerPathLegacy(tmp, digest)
	if err != nil {
		t.Fatalf("CacheLowerPathLegacy: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(legacyPath, []byte("legacy-ext4-fake"), 0o444); err != nil {
		t.Fatalf("write legacy cache: %v", err)
	}

	got, err := EnsureLowerFromLayer(context.Background(), tmp, digest, "linux/arm64", "")
	if err != nil {
		t.Fatalf("EnsureLowerFromLayer: %v", err)
	}
	if got != legacyPath {
		t.Errorf("EnsureLowerFromLayer returned %s, want legacy path %s", got, legacyPath)
	}
	// And the new .erofs path should NOT have been written.
	newPath, _ := CacheLowerPath(tmp, digest)
	if _, err := os.Stat(newPath); err == nil {
		t.Errorf("EnsureLowerFromLayer wrote %s even though %s already existed", newPath, legacyPath)
	}
}

func TestEnsureLowerFromLayerErofsCacheHit(t *testing.T) {
	tmp := t.TempDir()
	if err := EnsureOCILayout(tmp); err != nil {
		t.Fatalf("EnsureOCILayout: %v", err)
	}

	content := []byte("fake-erofs-content")
	digest := DigestBytes(content)
	if _, err := WriteBlob(tmp, digest, content); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}

	// Pre-seed the new .erofs cache path.
	erofsPath, err := CacheLowerPath(tmp, digest)
	if err != nil {
		t.Fatalf("CacheLowerPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(erofsPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(erofsPath, []byte("erofs-pre-seeded"), 0o444); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	got, err := EnsureLowerFromLayer(context.Background(), tmp, digest, "linux/arm64", "")
	if err != nil {
		t.Fatalf("EnsureLowerFromLayer: %v", err)
	}
	if got != erofsPath {
		t.Errorf("returned %s, want %s", got, erofsPath)
	}
}

func TestCacheLowerExistsBothFormats(t *testing.T) {
	tmp := t.TempDir()
	if err := EnsureOCILayout(tmp); err != nil {
		t.Fatalf("EnsureOCILayout: %v", err)
	}
	digest := DigestBytes([]byte("test"))

	if CacheLowerExists(tmp, digest) {
		t.Error("CacheLowerExists with no files should return false")
	}

	// Seed legacy .ext4 only.
	legacy, _ := CacheLowerPathLegacy(tmp, digest)
	_ = os.MkdirAll(filepath.Dir(legacy), 0o755)
	_ = os.WriteFile(legacy, []byte("x"), 0o444)
	if !CacheLowerExists(tmp, digest) {
		t.Error("CacheLowerExists should accept legacy .ext4")
	}
	_ = os.Remove(legacy)

	// Seed only .erofs.
	erofs, _ := CacheLowerPath(tmp, digest)
	_ = os.WriteFile(erofs, []byte("x"), 0o444)
	if !CacheLowerExists(tmp, digest) {
		t.Error("CacheLowerExists should accept new .erofs")
	}
}

func TestRegisterMaterializerHook(t *testing.T) {
	// Capture and restore so other tests don't see our injected hook.
	prev := materializerHook
	defer RegisterMaterializer(prev)

	called := false
	RegisterMaterializer(func(ctx context.Context, blobPath, outputPath, platform string) error {
		called = true
		return nil
	})

	if materializerHook == nil {
		t.Fatal("RegisterMaterializer did not install the hook")
	}
	if err := materializerHook(context.Background(), "/tmp/x", "/tmp/y", "linux/arm64"); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if !called {
		t.Error("hook closure never ran")
	}
}

func TestValidLowerSize(t *testing.T) {
	cases := map[string]bool{
		"20G":   true,
		"500M":  true,
		"1K":    true,
		"0":     false,
		"":      false,
		"20":    true,
		"abc":   false,
		"20Gb":  false,
		"-1G":   false,
		"20.5G": false,
	}
	for in, want := range cases {
		got := validLowerSize.MatchString(in)
		if got != want {
			t.Errorf("validLowerSize(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestExtractTarGzPlainTar(t *testing.T) {
	// Some registries serve uncompressed layer tarballs; extractTarGz
	// must accept those too (it Seeks back to 0 after a failed gzip
	// detection).
	tmp := t.TempDir()
	src := filepath.Join(tmp, "layer.tar")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "etc/foo", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("hdr: %v", err)
	}
	if _, err := tw.Write([]byte("bar")); err != nil {
		t.Fatalf("write: %v", err)
	}
	tw.Close()
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write tar: %v", err)
	}

	dst := filepath.Join(tmp, "extract")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := extractTarGz(src, dst); err != nil {
		t.Fatalf("extractTarGz on plain tar: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "etc/foo"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(got), "bar") {
		t.Errorf("content = %q, want contains 'bar'", got)
	}
}
