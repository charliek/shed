package vmutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestCreateTarArchiveSingleFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cred-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a test file
	testFile := filepath.Join(tmpDir, "token")
	if err := os.WriteFile(testFile, []byte("secret-token"), 0600); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	ct := &CredentialTransfer{}
	info, _ := os.Stat(testFile)
	tarData, err := ct.createTarArchive(testFile, info, nil)
	if err != nil {
		t.Fatalf("createTarArchive() error = %v", err)
	}

	// Verify tar contents
	entries := extractTarEntries(t, tarData)
	if len(entries) != 1 {
		t.Fatalf("expected 1 tar entry, got %d", len(entries))
	}
	if entries[0].name != "token" {
		t.Errorf("entry name = %q, want %q", entries[0].name, "token")
	}
	if string(entries[0].data) != "secret-token" {
		t.Errorf("entry data = %q, want %q", entries[0].data, "secret-token")
	}
}

func TestCreateTarArchiveDirectory(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cred-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a directory with files
	credDir := filepath.Join(tmpDir, "ghcreds")
	os.MkdirAll(filepath.Join(credDir, "subdir"), 0755)
	os.WriteFile(filepath.Join(credDir, "hosts.yml"), []byte("github.com:\n  token: abc"), 0600)
	os.WriteFile(filepath.Join(credDir, "subdir", "config"), []byte("setting=value"), 0600)

	ct := &CredentialTransfer{}
	info, _ := os.Stat(credDir)
	tarData, err := ct.createTarArchive(credDir, info, nil)
	if err != nil {
		t.Fatalf("createTarArchive() error = %v", err)
	}

	entries := extractTarEntries(t, tarData)
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 file entries, got %d", len(entries))
	}

	// Check that we find both files
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.name] = true
	}
	if !names["hosts.yml"] {
		t.Error("expected hosts.yml in tar archive")
	}
	if !names[filepath.Join("subdir", "config")] {
		t.Error("expected subdir/config in tar archive")
	}
}

func TestCreateTarArchiveWithExclude(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cred-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a directory with files — some should be excluded
	credDir := filepath.Join(tmpDir, "opencode")
	os.MkdirAll(filepath.Join(credDir, "log"), 0755)
	os.MkdirAll(filepath.Join(credDir, "storage"), 0755)
	os.WriteFile(filepath.Join(credDir, "auth.json"), []byte(`{"token":"abc"}`), 0600)
	os.WriteFile(filepath.Join(credDir, "opencode.db"), []byte("sqlitedata"), 0600)
	os.WriteFile(filepath.Join(credDir, "opencode.db-shm"), []byte("shm"), 0600)
	os.WriteFile(filepath.Join(credDir, "opencode.db-wal"), []byte("wal"), 0600)
	os.WriteFile(filepath.Join(credDir, "log", "output.log"), []byte("logdata"), 0600)
	os.WriteFile(filepath.Join(credDir, "storage", "data.bin"), []byte("bindata"), 0600)

	exclude := []string{"*.db", "*.db-shm", "*.db-wal", "log/*", "storage/*"}

	ct := &CredentialTransfer{}
	info, _ := os.Stat(credDir)
	tarData, err := ct.createTarArchive(credDir, info, exclude)
	if err != nil {
		t.Fatalf("createTarArchive() error = %v", err)
	}

	entries := extractTarEntries(t, tarData)
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.name] = true
	}

	// auth.json should be included
	if !names["auth.json"] {
		t.Error("expected auth.json in tar archive")
	}

	// Excluded files should NOT be present
	for _, excluded := range []string{"opencode.db", "opencode.db-shm", "opencode.db-wal", "log/output.log", "storage/data.bin"} {
		if names[excluded] {
			t.Errorf("expected %s to be excluded from tar archive", excluded)
		}
	}
}

func TestMatchesExclude(t *testing.T) {
	patterns := []string{"*.db", "*.db-shm", "*.db-wal", "log/*"}

	tests := []struct {
		relPath string
		want    bool
	}{
		{"opencode.db", true},
		{"test.db-shm", true},
		{"test.db-wal", true},
		{"log/output.log", true},
		{"auth.json", false},
		{"config.yaml", false},
	}

	for _, tt := range tests {
		t.Run(tt.relPath, func(t *testing.T) {
			got := matchesExclude(tt.relPath, patterns)
			if got != tt.want {
				t.Errorf("matchesExclude(%q) = %v, want %v", tt.relPath, got, tt.want)
			}
		})
	}
}

func TestMatchesExcludeEmpty(t *testing.T) {
	if matchesExclude("anything.db", nil) {
		t.Error("matchesExclude should return false with nil patterns")
	}
	if matchesExclude("anything.db", []string{}) {
		t.Error("matchesExclude should return false with empty patterns")
	}
}

func TestExtractTarToHost(t *testing.T) {
	destDir, err := os.MkdirTemp("", "extract-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(destDir)

	// Create a tar archive in memory
	tarData := createTestTar(t, map[string]string{
		"file1.txt":        "content1",
		"subdir/file2.txt": "content2",
	})

	if err := extractTarToHost(tarData, destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	// Verify extracted files
	data1, err := os.ReadFile(filepath.Join(destDir, "file1.txt"))
	if err != nil {
		t.Fatalf("Failed to read extracted file1.txt: %v", err)
	}
	if string(data1) != "content1" {
		t.Errorf("file1.txt = %q, want %q", data1, "content1")
	}

	data2, err := os.ReadFile(filepath.Join(destDir, "subdir", "file2.txt"))
	if err != nil {
		t.Fatalf("Failed to read extracted subdir/file2.txt: %v", err)
	}
	if string(data2) != "content2" {
		t.Errorf("subdir/file2.txt = %q, want %q", data2, "content2")
	}
}

func TestExtractTarToHostRejectsPathTraversal(t *testing.T) {
	destDir, err := os.MkdirTemp("", "extract-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(destDir)

	// Create a tar with a path traversal attempt
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	tw.WriteHeader(&tar.Header{
		Name: "../../etc/passwd",
		Mode: 0644,
		Size: 4,
	})
	tw.Write([]byte("evil"))
	tw.Close()
	gzw.Close()

	// Should not error but should skip the bad entry
	if err := extractTarToHost(buf.Bytes(), destDir); err != nil {
		t.Fatalf("extractTarToHost() error = %v", err)
	}

	// Nothing should have been written to destDir
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("Failed to read destDir: %v", err)
	}
	if len(entries) > 0 {
		t.Errorf("path traversal file should not have been extracted, found: %v", entries)
	}
}

func TestSudoPrefix(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/etc/ssh", "sudo "},
		{"/usr/local/bin", "sudo "},
		{"/var/lib/data", "sudo "},
		{"/home/user/.ssh", ""},
		{"/workspace/data", ""},
	}
	for _, tt := range tests {
		got := sudoPrefix(tt.path)
		if got != tt.want {
			t.Errorf("sudoPrefix(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestCredentialTransferAllNoCredentials(t *testing.T) {
	ct := &CredentialTransfer{
		serverCfg: &config.ServerConfig{
			Credentials: make(map[string]config.MountConfig),
		},
	}
	err := ct.TransferAll(context.TODO())
	if err != nil {
		t.Errorf("TransferAll() with no credentials should return nil, got %v", err)
	}
}

func TestCredentialTransferAllNilConfig(t *testing.T) {
	ct := &CredentialTransfer{
		serverCfg: nil,
	}
	err := ct.TransferAll(context.TODO())
	if err != nil {
		t.Errorf("TransferAll() with nil config should return nil, got %v", err)
	}
}

func TestSecurePath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "secure-path-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Valid path
	path, err := securePath(tmpDir, "file.txt")
	if err != nil {
		t.Errorf("securePath() error = %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("securePath() returned relative path: %s", path)
	}

	// Path traversal should fail
	_, err = securePath(tmpDir, "../../../etc/passwd")
	if err == nil {
		t.Error("securePath() should fail for path traversal")
	}
}

func TestLimitedBuffer(t *testing.T) {
	lb := &limitedBuffer{max: 10}

	_, err := lb.Write([]byte("hello"))
	if err != nil {
		t.Errorf("Write() error = %v", err)
	}

	_, err = lb.Write([]byte("world!"))
	if err == nil {
		t.Error("Write() should fail when exceeding max size")
	}
}

// --- helpers ---

type tarEntry struct {
	name string
	data []byte
	dir  bool
}

func extractTarEntries(t *testing.T, tarData []byte) []tarEntry {
	t.Helper()
	gzr, err := gzip.NewReader(bytes.NewReader(tarData))
	if err != nil {
		t.Fatalf("gzip.NewReader error: %v", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	var entries []tarEntry
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next() error: %v", err)
		}

		entry := tarEntry{name: header.Name, dir: header.Typeflag == tar.TypeDir}
		if !entry.dir {
			data, _ := io.ReadAll(tr)
			entry.data = data
		}
		entries = append(entries, entry)
	}
	return entries
}

func createTestTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for name, content := range files {
		// Create parent directories
		dir := filepath.Dir(name)
		if dir != "." {
			tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     dir + "/",
				Mode:     0755,
			})
		}

		tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		})
		tw.Write([]byte(content))
	}

	tw.Close()
	gzw.Close()
	return buf.Bytes()
}
