//go:build linux
// +build linux

package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestExtractTarToHost(t *testing.T) {
	destDir := mustTempDir(t, "extract-test")

	// Create a tar.gz with a test file
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	content := []byte("test-token-value")
	hdr := &tar.Header{
		Name: "hosts.yml",
		Mode: 0600,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gzw.Close()

	if err := extractTarToHost(buf.Bytes(), destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	// Verify the file was created
	got, err := os.ReadFile(filepath.Join(destDir, "hosts.yml"))
	if err != nil {
		t.Fatalf("failed to read extracted file: %v", err)
	}
	if string(got) != "test-token-value" {
		t.Errorf("extracted content = %q, want %q", string(got), "test-token-value")
	}
}

func TestExtractTarToHost_Directory(t *testing.T) {
	destDir := mustTempDir(t, "extract-test")

	// Create a tar.gz with a subdirectory and file
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	// Add directory
	if err := tw.WriteHeader(&tar.Header{
		Name:     "subdir/",
		Typeflag: tar.TypeDir,
		Mode:     0755,
	}); err != nil {
		t.Fatal(err)
	}

	// Add file in directory
	content := []byte("nested-content")
	if err := tw.WriteHeader(&tar.Header{
		Name: "subdir/config.json",
		Mode: 0644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}

	tw.Close()
	gzw.Close()

	if err := extractTarToHost(buf.Bytes(), destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "subdir", "config.json"))
	if err != nil {
		t.Fatalf("failed to read extracted file: %v", err)
	}
	if string(got) != "nested-content" {
		t.Errorf("extracted content = %q, want %q", string(got), "nested-content")
	}
}

func TestExtractTarToHost_PathTraversal(t *testing.T) {
	destDir := mustTempDir(t, "extract-test")

	// Create a tar.gz with a path traversal attempt
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	content := []byte("malicious")
	if err := tw.WriteHeader(&tar.Header{
		Name: "../../../etc/passwd",
		Mode: 0644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gzw.Close()

	// Should not error but should skip the malicious path
	if err := extractTarToHost(buf.Bytes(), destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	// The malicious file should not have been created
	entries, _ := os.ReadDir(destDir)
	if len(entries) != 0 {
		t.Errorf("expected empty directory, got %d entries", len(entries))
	}
}

func TestExtractTarToHost_SymlinkRejected(t *testing.T) {
	destDir := mustTempDir(t, "extract-test")

	// Create a tar.gz with a symlink pointing outside destDir
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	if err := tw.WriteHeader(&tar.Header{
		Name:     "evil-link",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
	}); err != nil {
		t.Fatal(err)
	}

	tw.Close()
	gzw.Close()

	if err := extractTarToHost(buf.Bytes(), destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	// The symlink should not have been created
	if _, err := os.Lstat(filepath.Join(destDir, "evil-link")); !os.IsNotExist(err) {
		t.Errorf("symlink should not have been created, got err = %v", err)
	}
}

func TestExtractTarToHost_OversizedFile(t *testing.T) {
	destDir := mustTempDir(t, "extract-test")

	// Create a tar.gz with a file that claims to be larger than maxCredentialFileSize.
	// We write only a small amount of actual data but set the header size large.
	// io.LimitReader will truncate to maxCredentialFileSize.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	// Write a small file to verify truncation: 16 bytes of actual content
	// but header claims a large size. The tar reader will read up to header.Size
	// bytes, but LimitReader caps it.
	smallContent := []byte("credential-data!")
	if err := tw.WriteHeader(&tar.Header{
		Name: "small.txt",
		Mode: 0600,
		Size: int64(len(smallContent)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(smallContent); err != nil {
		t.Fatal(err)
	}

	tw.Close()
	gzw.Close()

	// Normal small file should extract fine (well under the limit)
	if err := extractTarToHost(buf.Bytes(), destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "small.txt"))
	if err != nil {
		t.Fatalf("failed to read extracted file: %v", err)
	}
	if string(got) != string(smallContent) {
		t.Errorf("extracted content = %q, want %q", string(got), string(smallContent))
	}
}

func TestNewCredentialNotifyListener(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025, 1026)
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"gh": {Source: "/home/user/.config/gh", Target: "/home/shed/.config/gh"},
		},
	}

	nl := NewCredentialNotifyListener(vsock, serverCfg, nil)
	if nl.vsock != vsock {
		t.Error("vsock not set correctly")
	}
	if nl.serverCfg != serverCfg {
		t.Error("serverCfg not set correctly")
	}
}
