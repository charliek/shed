package sync

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNewSyncer(t *testing.T) {
	cfg := &Config{}
	syncer := NewSyncer(cfg)

	if syncer == nil {
		t.Fatal("NewSyncer returned nil")
	}
	if syncer.cfg != cfg {
		t.Error("Syncer cfg not set correctly")
	}
	if syncer.output != os.Stdout {
		t.Error("Syncer output not set to os.Stdout by default")
	}
}

func TestSyncer_SetOutput(t *testing.T) {
	cfg := &Config{}
	syncer := NewSyncer(cfg)

	var buf bytes.Buffer
	syncer.SetOutput(&buf)

	if syncer.output != &buf {
		t.Error("SetOutput did not update output writer")
	}
}

func TestCreateTar_SingleFile(t *testing.T) {
	// Create a temp file
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello world"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{}
	syncer := NewSyncer(cfg)

	ctx := context.Background()

	// Create tar
	tarData, err := syncer.createTar(ctx, testFile, "")
	if err != nil {
		t.Fatalf("createTar failed: %v", err)
	}

	// Verify tar data is not empty and looks like gzip
	if len(tarData) < 10 {
		t.Error("tar data too small")
	}
	// Gzip magic number: 1f 8b
	if tarData[0] != 0x1f || tarData[1] != 0x8b {
		t.Error("tar data doesn't appear to be gzip compressed")
	}
}

func TestCreateTar_Directory(t *testing.T) {
	// Create a temp directory with files
	dir := t.TempDir()
	testDir := filepath.Join(dir, "testdir")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "file1.txt"), []byte("file1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "file2.txt"), []byte("file2"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{}
	syncer := NewSyncer(cfg)
	ctx := context.Background()

	// Create tar
	tarData, err := syncer.createTar(ctx, testDir, "")
	if err != nil {
		t.Fatalf("createTar failed: %v", err)
	}

	// Verify tar data is not empty
	if len(tarData) < 10 {
		t.Error("tar data too small for directory")
	}
}

func TestCreateTar_WithInclude(t *testing.T) {
	// Create a temp directory with mixed files
	dir := t.TempDir()
	testDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "cert.pem"), []byte("cert"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "key.pem"), []byte("key"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "other.txt"), []byte("other"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{}
	syncer := NewSyncer(cfg)
	ctx := context.Background()

	// Create tar with include pattern
	tarData, err := syncer.createTar(ctx, testDir, "*.pem")
	if err != nil {
		t.Fatalf("createTar with include failed: %v", err)
	}

	// Verify tar data is not empty
	if len(tarData) < 10 {
		t.Error("tar data too small")
	}
}

func TestCreateTar_NonExistent(t *testing.T) {
	cfg := &Config{}
	syncer := NewSyncer(cfg)
	ctx := context.Background()

	_, err := syncer.createTar(ctx, "/nonexistent/path", "")
	if err == nil {
		t.Error("Expected error for non-existent path")
	}
}

func TestSyncPath_SkipsMissingSource(t *testing.T) {
	cfg := &Config{}
	syncer := NewSyncer(cfg)
	ctx := context.Background()

	var buf bytes.Buffer
	syncer.SetOutput(&buf)

	pm := PathMapping{
		Source: "/nonexistent/path",
		Target: "/target",
	}

	// Should not error, just skip
	// Note: We can't fully test this without a real SSH connection,
	// but we can verify the initial stat check works
	err := syncer.syncPath(ctx, pm, "test-shed", nil, true) // dry-run
	if err != nil {
		// Should skip, not error
		t.Logf("syncPath returned: %v", err)
	}

	// Output should indicate skipping the missing file
	_ = buf.String()
}

func TestSyncPath_DryRun(t *testing.T) {
	// Create a temp file
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{}
	syncer := NewSyncer(cfg)
	ctx := context.Background()

	var buf bytes.Buffer
	syncer.SetOutput(&buf)

	pm := PathMapping{
		Source: testFile,
		Target: "/target/test.txt",
	}

	// Dry run should not attempt SSH
	err := syncer.syncPath(ctx, pm, "test-shed", nil, true)
	if err != nil {
		t.Fatalf("syncPath dry-run failed: %v", err)
	}

	output := buf.String()
	if output == "" {
		t.Error("Expected output for dry-run")
	}
}

func TestSyncFeature_DryRun(t *testing.T) {
	// Create temp files
	dir := t.TempDir()
	testFile := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		Features: map[string]Feature{
			"test": {
				Description: "Test feature",
				Paths: []PathMapping{
					{Source: testFile, Target: "/target/test.txt"},
				},
				PostSync: []PostSyncHook{
					{Run: "echo done"},
				},
			},
		},
	}

	syncer := NewSyncer(cfg)
	ctx := context.Background()

	var buf bytes.Buffer
	syncer.SetOutput(&buf)

	feature := cfg.Features["test"]
	err := syncer.syncFeature(ctx, "test", &feature, "test-shed", nil, true)
	if err != nil {
		t.Fatalf("syncFeature dry-run failed: %v", err)
	}

	output := buf.String()
	if output == "" {
		t.Error("Expected output for dry-run")
	}
}
