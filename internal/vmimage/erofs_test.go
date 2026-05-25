package vmimage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMintRootfsErofsIntegration verifies the end-to-end mint flow:
// build a tar.gz layer, install it as a blob, run MintRootfsErofs
// against it (which shells out to docker + the build-tools image),
// and confirm the produced blob is a valid erofs.
//
// Skipped unless both docker and the locally-built
// shed-build-tools:dev image are available, since the integration is
// the point — there's no point running this against a mocked docker.
func TestMintRootfsErofsIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test (docker + shed-build-tools image); skipping in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping erofs mint integration")
	}
	// Confirm shed-build-tools:dev exists locally. CI will populate
	// this via `make build-tools`; locally the user does the same.
	check := exec.Command("docker", "image", "inspect", "shed-build-tools:dev")
	if err := check.Run(); err != nil {
		t.Skip("shed-build-tools:dev not present locally — run `make build-tools` to enable this test")
	}

	dir := t.TempDir()
	if err := EnsureOCILayout(dir); err != nil {
		t.Fatalf("EnsureOCILayout: %v", err)
	}

	// Build a real tar.gz layer with a few small files. mkfs.erofs
	// --tar=f wants regular tar (no compression) since shed merges
	// the layer first; the merger handles gzip extraction so the
	// stored blob is gzip-compressed like every other OCI layer.
	layerBytes := buildTarGzLayer(t, map[string]string{
		"./etc/test.txt":    "hello from mint test\n",
		"./usr/local/bin/x": "fake binary content",
	})
	layerDigest := DigestBytes(layerBytes)
	if _, err := WriteBlob(dir, layerDigest, layerBytes); err != nil {
		t.Fatalf("WriteBlob layer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	erofsDigest, err := MintRootfsErofs(ctx, MintErofsOptions{
		ImagesDir:     dir,
		LayerDigests:  []string{layerDigest},
		BuildToolsRef: "shed-build-tools:dev",
	})
	if err != nil {
		t.Fatalf("MintRootfsErofs: %v", err)
	}
	if !strings.HasPrefix(erofsDigest, DigestPrefix) {
		t.Fatalf("erofs digest = %q, want sha256: prefix", erofsDigest)
	}
	if !BlobExists(dir, erofsDigest) {
		t.Fatalf("BlobExists(%s) = false after mint", erofsDigest)
	}

	// Verify the produced blob is actually an erofs by reading the
	// 32-byte preamble + 4-byte magic.
	blobPath, err := BlobPath(dir, erofsDigest)
	if err != nil {
		t.Fatalf("BlobPath: %v", err)
	}
	f, err := os.Open(blobPath)
	if err != nil {
		t.Fatalf("opening blob: %v", err)
	}
	defer f.Close()
	header := make([]byte, 36)
	if _, err := f.Read(header); err != nil {
		t.Fatalf("reading blob preamble: %v", err)
	}
	// erofs magic 0xE0F5E1E2 (little-endian) lives at offset 1024
	// (start of superblock). Check that instead — easier than parsing.
	if _, err := f.Seek(1024, 0); err != nil {
		t.Fatalf("seeking to superblock: %v", err)
	}
	magic := make([]byte, 4)
	if _, err := f.Read(magic); err != nil {
		t.Fatalf("reading magic: %v", err)
	}
	want := []byte{0xE2, 0xE1, 0xF5, 0xE0}
	for i := range want {
		if magic[i] != want[i] {
			t.Fatalf("erofs magic at offset 1024 = %x, want %x", magic, want)
		}
	}
}

// TestMintRootfsErofsValidatesInputs spot-checks the argument
// validation so a misconfigured caller fails fast rather than running
// docker and getting a confusing failure.
func TestMintRootfsErofsValidatesInputs(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		opts MintErofsOptions
		want string
	}{
		{
			name: "empty ImagesDir",
			opts: MintErofsOptions{LayerDigests: []string{"sha256:x"}, BuildToolsRef: "shed-build-tools:dev"},
			want: "ImagesDir is required",
		},
		{
			name: "no layers",
			opts: MintErofsOptions{ImagesDir: t.TempDir(), BuildToolsRef: "shed-build-tools:dev"},
			want: "at least one layer digest is required",
		},
		{
			name: "no build-tools ref",
			opts: MintErofsOptions{ImagesDir: t.TempDir(), LayerDigests: []string{"sha256:x"}},
			want: "BuildToolsRef is required",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := MintRootfsErofs(ctx, c.opts)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), c.want)
			}
		})
	}
}

// buildTarGzLayer composes a gzipped tar with the given path→contents
// entries. Caller-controlled paths so tests can reproduce specific
// layer shapes (whiteouts, opaque dirs, etc).
func buildTarGzLayer(t *testing.T, files map[string]string) []byte {
	t.Helper()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "layer.tar.gz")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatalf("creating tar: %v", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for path, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     path,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file close: %v", err)
	}
	data, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatalf("read tar: %v", err)
	}
	return data
}
