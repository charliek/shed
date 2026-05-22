package vmimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// installMultiLayerManifest writes a synthetic OCI manifest with N
// layer tar.gz blobs into imagesDir and tags it. layerEntries is
// indexed by layer position (lowest first); each entry maps tar
// path → file content. Whiteout markers can be expressed by setting
// content to "" and using the .wh.* path convention.
func installMultiLayerManifest(t *testing.T, imagesDir, tagName string, layerEntries []map[string]string) string {
	t.Helper()
	if err := EnsureOCILayout(imagesDir); err != nil {
		t.Fatalf("EnsureOCILayout: %v", err)
	}

	var layerDescs []Descriptor
	var diffIDs []string
	for i, entries := range layerEntries {
		buf := buildLayerTarGz(t, entries)
		digest := DigestBytes(buf)
		if _, err := WriteBlob(imagesDir, digest, buf); err != nil {
			t.Fatalf("WriteBlob layer %d: %v", i, err)
		}
		layerDescs = append(layerDescs, Descriptor{
			MediaType: MediaTypeOCILayer,
			Digest:    digest,
			Size:      int64(len(buf)),
		})
		diffIDs = append(diffIDs, digest)
	}

	cfg := &OCIConfig{
		Architecture: "arm64",
		OS:           "linux",
		RootFS: OCIRootFS{
			Type:    "layers",
			DiffIDs: diffIDs,
		},
	}
	cfgData, err := cfg.MarshalIndent()
	if err != nil {
		t.Fatalf("config marshal: %v", err)
	}
	cfgDigest := DigestBytes(cfgData)
	if _, err := WriteBlob(imagesDir, cfgDigest, cfgData); err != nil {
		t.Fatalf("WriteBlob config: %v", err)
	}

	manifest := &OCIManifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config: Descriptor{
			MediaType: MediaTypeOCIConfig,
			Digest:    cfgDigest,
			Size:      int64(len(cfgData)),
		},
		Layers: layerDescs,
		Annotations: map[string]string{
			AnnotationSchemaVersion: ShedSchemaVersion,
			AnnotationVariant:       tagName,
		},
	}
	manData, err := manifest.MarshalIndent()
	if err != nil {
		t.Fatalf("manifest marshal: %v", err)
	}
	manDigest := DigestBytes(manData)
	if _, err := WriteBlob(imagesDir, manDigest, manData); err != nil {
		t.Fatalf("WriteBlob manifest: %v", err)
	}
	if err := SetTag(imagesDir, tagName, manDigest); err != nil {
		t.Fatalf("SetTag: %v", err)
	}
	return manDigest
}

// buildLayerTarGz turns a map of path→content into a gzip-compressed tar
// blob. Keys starting with ".wh." or containing "/.wh." are emitted as
// whiteout markers (length-0 regular files, since that's the OCI convention).
// Symlinks can be expressed by prefixing content with "symlink:".
func buildLayerTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for name, content := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o644}
		if strings.HasPrefix(content, "symlink:") {
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = strings.TrimPrefix(content, "symlink:")
			hdr.Mode = 0o777
		} else if strings.HasSuffix(name, "/") {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(content))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			if _, err := tw.Write([]byte(content)); err != nil {
				t.Fatalf("write content %s: %v", name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(raw.Bytes()); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return gz.Bytes()
}

// extractMergedTar returns a map of path → content for every regular
// file in the merged tar at path. Whiteout markers should never be
// present; the test fails loudly if any are observed.
func extractMergedTar(t *testing.T, tarPath string) map[string]string {
	t.Helper()
	out := map[string]string{}
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open tar: %v", err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if strings.Contains(name, ".wh.") {
			t.Errorf("whiteout marker leaked into merged tar: %s", name)
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			out[name] = string(data)
		case tar.TypeDir:
			out[name] = "<dir>"
		case tar.TypeSymlink:
			out[name] = "symlink:" + hdr.Linkname
		}
	}
	return out
}

func TestMergeLayersFromManifestNoWhiteouts(t *testing.T) {
	imagesDir := t.TempDir()
	manifestDigest := installMultiLayerManifest(t, imagesDir, "test",
		[]map[string]string{
			// Layer 0 (lowest)
			{"etc/hostname": "shed\n", "usr/bin/sh": "#!/bin/sh\n"},
			// Layer 1
			{"etc/motd": "welcome\n", "usr/bin/sh": "#!/bin/bash\n"}, // overrides sh
		},
	)
	out := filepath.Join(imagesDir, "merged.tar")
	if err := MergeLayersFromManifest(context.Background(), imagesDir, manifestDigest, out); err != nil {
		t.Fatalf("MergeLayersFromManifest: %v", err)
	}
	got := extractMergedTar(t, out)
	want := map[string]string{
		"etc/hostname": "shed\n",
		"etc/motd":     "welcome\n",
		"usr/bin/sh":   "#!/bin/bash\n", // layer 1 wins
	}
	if !mapsEqual(got, want) {
		t.Errorf("merged contents = %v, want %v", got, want)
	}
}

func TestMergeLayersFromManifestFileWhiteout(t *testing.T) {
	imagesDir := t.TempDir()
	manifestDigest := installMultiLayerManifest(t, imagesDir, "test",
		[]map[string]string{
			// Layer 0
			{"etc/keep": "keep\n", "etc/drop": "drop\n", "etc/drop-dir/inside": "x\n"},
			// Layer 1: deletes etc/drop and etc/drop-dir (recursive)
			{"etc/.wh.drop": "", "etc/.wh.drop-dir": ""},
		},
	)
	out := filepath.Join(imagesDir, "merged.tar")
	if err := MergeLayersFromManifest(context.Background(), imagesDir, manifestDigest, out); err != nil {
		t.Fatalf("MergeLayersFromManifest: %v", err)
	}
	got := extractMergedTar(t, out)
	if _, ok := got["etc/drop"]; ok {
		t.Errorf("etc/drop should have been whitened but is present")
	}
	if _, ok := got["etc/drop-dir/inside"]; ok {
		t.Errorf("etc/drop-dir/inside should be whitened by recursive .wh.drop-dir")
	}
	if got["etc/keep"] != "keep\n" {
		t.Errorf("etc/keep should be kept; got %q", got["etc/keep"])
	}
}

func TestMergeLayersFromManifestOpaqueDir(t *testing.T) {
	imagesDir := t.TempDir()
	manifestDigest := installMultiLayerManifest(t, imagesDir, "test",
		[]map[string]string{
			// Layer 0: populate /var/cache
			{"var/cache/a.dat": "a", "var/cache/b.dat": "b", "etc/keep": "keep"},
			// Layer 1: opaque marker AND its own var/cache/c.dat
			{"var/cache/.wh..wh..opq": "", "var/cache/c.dat": "c"},
		},
	)
	out := filepath.Join(imagesDir, "merged.tar")
	if err := MergeLayersFromManifest(context.Background(), imagesDir, manifestDigest, out); err != nil {
		t.Fatalf("MergeLayersFromManifest: %v", err)
	}
	got := extractMergedTar(t, out)
	if _, ok := got["var/cache/a.dat"]; ok {
		t.Errorf("var/cache/a.dat should be opaque-whitened from layer 0")
	}
	if _, ok := got["var/cache/b.dat"]; ok {
		t.Errorf("var/cache/b.dat should be opaque-whitened from layer 0")
	}
	if got["var/cache/c.dat"] != "c" {
		t.Errorf("var/cache/c.dat should survive (added in layer 1); got %q", got["var/cache/c.dat"])
	}
	if got["etc/keep"] != "keep" {
		t.Errorf("etc/keep should survive; got %q", got["etc/keep"])
	}
}

func TestMergeLayersFromManifestReaddAfterWhiteout(t *testing.T) {
	imagesDir := t.TempDir()
	manifestDigest := installMultiLayerManifest(t, imagesDir, "test",
		[]map[string]string{
			// Layer 0: foo and bar
			{"foo": "old", "bar/inside": "old"},
			// Layer 1: whiteout foo and bar
			{".wh.foo": "", ".wh.bar": ""},
			// Layer 2: re-add foo (file) and bar/inside (file)
			{"foo": "new", "bar/inside": "new"},
		},
	)
	out := filepath.Join(imagesDir, "merged.tar")
	if err := MergeLayersFromManifest(context.Background(), imagesDir, manifestDigest, out); err != nil {
		t.Fatalf("MergeLayersFromManifest: %v", err)
	}
	got := extractMergedTar(t, out)
	if got["foo"] != "new" {
		t.Errorf("foo should be from layer 2 (new); got %q", got["foo"])
	}
	if got["bar/inside"] != "new" {
		t.Errorf("bar/inside should be from layer 2 (new); got %q", got["bar/inside"])
	}
}

func TestMergeLayersFromManifestSymlinks(t *testing.T) {
	imagesDir := t.TempDir()
	manifestDigest := installMultiLayerManifest(t, imagesDir, "test",
		[]map[string]string{
			{"usr/bin/sh": "binary", "bin": "symlink:usr/bin"},
		},
	)
	out := filepath.Join(imagesDir, "merged.tar")
	if err := MergeLayersFromManifest(context.Background(), imagesDir, manifestDigest, out); err != nil {
		t.Fatalf("MergeLayersFromManifest: %v", err)
	}
	got := extractMergedTar(t, out)
	if got["bin"] != "symlink:usr/bin" {
		t.Errorf("bin should be a symlink to usr/bin; got %q", got["bin"])
	}
	if got["usr/bin/sh"] != "binary" {
		t.Errorf("usr/bin/sh content wrong; got %q", got["usr/bin/sh"])
	}
}

func TestMergeLayersFromManifestZeroLayers(t *testing.T) {
	imagesDir := t.TempDir()
	// installMultiLayerManifest doesn't support 0 layers cleanly,
	// so handcraft a manifest with empty layers slice.
	if err := EnsureOCILayout(imagesDir); err != nil {
		t.Fatalf("EnsureOCILayout: %v", err)
	}
	cfgData, _ := (&OCIConfig{Architecture: "arm64", OS: "linux"}).MarshalIndent()
	cfgDigest := DigestBytes(cfgData)
	_, _ = WriteBlob(imagesDir, cfgDigest, cfgData)
	man := &OCIManifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config:        Descriptor{Digest: cfgDigest, Size: int64(len(cfgData)), MediaType: MediaTypeOCIConfig},
	}
	manData, _ := man.MarshalIndent()
	digest := DigestBytes(manData)
	_, _ = WriteBlob(imagesDir, digest, manData)

	out := filepath.Join(imagesDir, "merged.tar")
	err := MergeLayersFromManifest(context.Background(), imagesDir, digest, out)
	if err == nil {
		t.Error("expected error for zero-layer manifest, got nil")
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}
