package vmutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestPullChangedFilesPathValidation(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"ssh": {
				Source: "/host/.ssh",
				Target: "/home/shed/.ssh",
			},
		},
	}

	nl := &CredentialNotifyListener{
		credentials: serverCfg.Credentials,
		name:        "test-vm",
		// agent is nil — we expect the function to filter out bad paths
		// and return nil when no valid files remain
	}

	// All paths should be rejected: ".." and absolute paths
	err := nl.PullChangedFiles("ssh", []string{
		"../../etc/passwd",
		"/etc/shadow",
		"../../../root/.ssh/id_rsa",
	})
	if err != nil {
		t.Errorf("PullChangedFiles() error = %v, want nil (all paths rejected)", err)
	}
}

func TestPullChangedFilesUnknownCredential(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{},
	}

	nl := &CredentialNotifyListener{
		credentials: serverCfg.Credentials,
		name:        "test-vm",
	}

	err := nl.PullChangedFiles("nonexistent", []string{"file.txt"})
	if err == nil {
		t.Error("PullChangedFiles() expected error for unknown credential")
	}
}

func TestLimitedBufferExceedsMax(t *testing.T) {
	lb := &limitedBuffer{max: 10}

	// Write within limit
	n, err := lb.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != 5 {
		t.Fatalf("Write() = %d, want 5", n)
	}

	// Write that exceeds limit
	_, err = lb.Write([]byte("hello world"))
	if err == nil {
		t.Fatal("Write() expected error when exceeding max")
	}
}

func TestLimitedBufferExactLimit(t *testing.T) {
	lb := &limitedBuffer{max: 10}

	// Write exactly at limit
	n, err := lb.Write([]byte("0123456789"))
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != 10 {
		t.Fatalf("Write() = %d, want 10", n)
	}

	// One more byte should fail
	_, err = lb.Write([]byte("x"))
	if err == nil {
		t.Fatal("Write() expected error when exceeding max")
	}
}

func TestExtractTarToHostSkipsSymlinks(t *testing.T) {
	destDir := t.TempDir()

	// Create tar with a symlink entry
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	// Regular file
	tw.WriteHeader(&tar.Header{
		Name:     "real.txt",
		Mode:     0644,
		Size:     4,
		Typeflag: tar.TypeReg,
	})
	tw.Write([]byte("real"))

	// Symlink (should be skipped)
	tw.WriteHeader(&tar.Header{
		Name:     "link.txt",
		Linkname: "/etc/passwd",
		Typeflag: tar.TypeSymlink,
	})

	tw.Close()
	gzw.Close()

	if err := extractTarToHost(buf.Bytes(), destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	assertFileContent(t, filepath.Join(destDir, "real.txt"), "real")

	// Symlink should not exist
	if _, err := os.Lstat(filepath.Join(destDir, "link.txt")); !os.IsNotExist(err) {
		t.Error("symlink should not have been extracted")
	}
}

func TestExtractTarToHostSkipsOversizedFiles(t *testing.T) {
	destDir := t.TempDir()

	// Build a valid tar where the oversized file has real data so the tar
	// reader can advance past it. We use maxCredentialFileSize+1 bytes of
	// actual data to exceed the limit.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	oversized := maxCredentialFileSize + 1

	// Normal file first
	if err := tw.WriteHeader(&tar.Header{
		Name:     "small.txt",
		Mode:     0644,
		Size:     5,
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("small"))

	// Oversized file with real data
	if err := tw.WriteHeader(&tar.Header{
		Name:     "huge.bin",
		Mode:     0644,
		Size:     int64(oversized),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	// Write the full amount of data to keep the tar valid
	remaining := oversized
	chunk := make([]byte, 32*1024)
	for remaining > 0 {
		n := len(chunk)
		if n > remaining {
			n = remaining
		}
		tw.Write(chunk[:n])
		remaining -= n
	}

	tw.Close()
	gzw.Close()

	if err := extractTarToHost(buf.Bytes(), destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	// Oversized file should not exist
	if _, err := os.Stat(filepath.Join(destDir, "huge.bin")); !os.IsNotExist(err) {
		t.Error("oversized file should not have been extracted")
	}

	assertFileContent(t, filepath.Join(destDir, "small.txt"), "small")
}

// assertFileContent reads a file and checks its content.
func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if string(got) != want {
		t.Errorf("file %s content = %q, want %q", path, got, want)
	}
}

func TestSecurePathFromNotify(t *testing.T) {
	destDir := t.TempDir()

	// Valid path
	p, err := securePath(destDir, "subdir/file.txt")
	if err != nil {
		t.Fatalf("securePath() error = %v", err)
	}
	if p == "" {
		t.Fatal("securePath() returned empty path")
	}

	// Path traversal
	_, err = securePath(destDir, fmt.Sprintf("../../%s", "escape.txt"))
	if err == nil {
		t.Error("securePath() expected error for path traversal")
	}
}
