//go:build linux
// +build linux

package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestNewCredentialTransfer(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025)
	serverCfg := &config.ServerConfig{}
	ct := NewCredentialTransfer(vsock, serverCfg)

	if ct.vsock != vsock {
		t.Error("vsock not set correctly")
	}
	if ct.serverCfg != serverCfg {
		t.Error("serverCfg not set correctly")
	}
}

func TestTransferAllNilConfig(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025)
	ct := NewCredentialTransfer(vsock, nil)

	// Nil config should return nil (no-op)
	if err := ct.TransferAll(context.Background()); err != nil {
		t.Errorf("TransferAll(nil config) = %v, want nil", err)
	}
}

func TestTransferAllEmptyCredentials(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025)
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{},
	}
	ct := NewCredentialTransfer(vsock, serverCfg)

	// Empty credentials should return nil (no-op)
	if err := ct.TransferAll(context.Background()); err != nil {
		t.Errorf("TransferAll(empty credentials) = %v, want nil", err)
	}
}

func TestCreateTarArchiveSingleFile(t *testing.T) {
	tmpDir := mustTempDir(t, "cred-test")

	// Create a test file
	testFile := filepath.Join(tmpDir, "test-cred.txt")
	if err := os.WriteFile(testFile, []byte("secret-value"), 0600); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	info, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("failed to stat test file: %v", err)
	}

	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025)
	ct := NewCredentialTransfer(vsock, nil)

	data, err := ct.createTarArchive(testFile, info)
	if err != nil {
		t.Fatalf("createTarArchive() error = %v", err)
	}

	if len(data) == 0 {
		t.Error("createTarArchive() returned empty data")
	}
}

func TestCreateTarArchiveDirectory(t *testing.T) {
	tmpDir := mustTempDir(t, "cred-test")

	// Create a directory with files
	credDir := filepath.Join(tmpDir, "creds")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatalf("failed to create cred dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "key1"), []byte("value1"), 0600); err != nil {
		t.Fatalf("failed to create key1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "key2"), []byte("value2"), 0600); err != nil {
		t.Fatalf("failed to create key2: %v", err)
	}

	info, err := os.Stat(credDir)
	if err != nil {
		t.Fatalf("failed to stat cred dir: %v", err)
	}

	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025)
	ct := NewCredentialTransfer(vsock, nil)

	data, err := ct.createTarArchive(credDir, info)
	if err != nil {
		t.Fatalf("createTarArchive() error = %v", err)
	}

	if len(data) == 0 {
		t.Error("createTarArchive() returned empty data")
	}
}

func TestCreateTarArchiveNonexistentSource(t *testing.T) {
	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025)
	ct := NewCredentialTransfer(vsock, nil)

	// Create a fake FileInfo from a real file, then try to archive a nonexistent path
	tmpDir := mustTempDir(t, "cred-test")
	testFile := filepath.Join(tmpDir, "exists.txt")
	if err := os.WriteFile(testFile, []byte("data"), 0600); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	info, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("failed to stat test file: %v", err)
	}

	// Now remove the file and try to archive
	os.Remove(testFile)

	_, err = ct.createTarArchive(testFile, info)
	if err == nil {
		t.Error("createTarArchive() expected error for nonexistent file, got nil")
	}
}

func TestCreateTarArchiveEmptyDirectory(t *testing.T) {
	tmpDir := mustTempDir(t, "cred-test")

	// Create an empty directory
	emptyDir := filepath.Join(tmpDir, "empty")
	if err := os.MkdirAll(emptyDir, 0700); err != nil {
		t.Fatalf("failed to create empty dir: %v", err)
	}

	info, err := os.Stat(emptyDir)
	if err != nil {
		t.Fatalf("failed to stat empty dir: %v", err)
	}

	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025)
	ct := NewCredentialTransfer(vsock, nil)

	data, err := ct.createTarArchive(emptyDir, info)
	if err != nil {
		t.Fatalf("createTarArchive() error = %v", err)
	}

	// Should produce a valid (but small) tar.gz even for empty dir
	if len(data) == 0 {
		t.Error("createTarArchive() returned empty data for empty directory")
	}
}

func TestMaxBase64CommandSize(t *testing.T) {
	// Verify the constant is reasonable
	if MaxBase64CommandSize <= 0 {
		t.Error("MaxBase64CommandSize should be positive")
	}
	if MaxBase64CommandSize > 1024*1024 {
		t.Errorf("MaxBase64CommandSize = %d, seems too large (> 1MB)", MaxBase64CommandSize)
	}
}
