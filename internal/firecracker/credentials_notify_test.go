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

	// Create a tar.gz with a file larger than maxCredentialFileSize.
	// The io.LimitReader in extractTarToHost should truncate the output
	// to exactly maxCredentialFileSize bytes.
	oversizedLen := maxCredentialFileSize + 1024
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	if err := tw.WriteHeader(&tar.Header{
		Name: "big.txt",
		Mode: 0600,
		Size: int64(oversizedLen),
	}); err != nil {
		t.Fatal(err)
	}
	// Write oversized data in chunks
	chunk := make([]byte, 32*1024)
	for i := range chunk {
		chunk[i] = 'A'
	}
	written := 0
	for written < oversizedLen {
		n := oversizedLen - written
		if n > len(chunk) {
			n = len(chunk)
		}
		if _, err := tw.Write(chunk[:n]); err != nil {
			t.Fatal(err)
		}
		written += n
	}

	tw.Close()
	gzw.Close()

	if err := extractTarToHost(buf.Bytes(), destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "big.txt"))
	if err != nil {
		t.Fatalf("failed to read extracted file: %v", err)
	}
	if len(got) != maxCredentialFileSize {
		t.Errorf("extracted file size = %d, want %d (truncated to maxCredentialFileSize)", len(got), maxCredentialFileSize)
	}
}

func TestExtractTarToHost_SymlinkEscape(t *testing.T) {
	destDir := mustTempDir(t, "extract-test")
	outsideDir := mustTempDir(t, "outside")

	// Plant a symlink inside destDir that points outside
	if err := os.Symlink(outsideDir, filepath.Join(destDir, "escape")); err != nil {
		t.Fatal(err)
	}

	// Create a tar with a file targeting the symlinked directory
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	content := []byte("should-not-land-outside")
	if err := tw.WriteHeader(&tar.Header{
		Name: "escape/pwned.txt",
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

	// The file should NOT have been written to the outside directory
	if _, err := os.Stat(filepath.Join(outsideDir, "pwned.txt")); !os.IsNotExist(err) {
		t.Errorf("file was written outside destDir via symlink, got err = %v", err)
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
