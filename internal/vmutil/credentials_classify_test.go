package vmutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestClassifyCredentialsDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	creds := map[string]config.MountConfig{
		"ssh": {Source: tmpDir, Target: "/home/shed/.ssh", ReadOnly: true},
	}

	dirCreds, fileCreds := ClassifyCredentials(creds)

	if len(dirCreds) != 1 {
		t.Fatalf("expected 1 dir credential, got %d", len(dirCreds))
	}
	if _, ok := dirCreds["ssh"]; !ok {
		t.Error("expected 'ssh' in dirCreds")
	}
	if len(fileCreds) != 0 {
		t.Errorf("expected 0 file credentials, got %d", len(fileCreds))
	}
}

func TestClassifyCredentialsSingleFile(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(tmpFile, []byte("[user]\n"), 0644); err != nil {
		t.Fatal(err)
	}

	creds := map[string]config.MountConfig{
		"git": {Source: tmpFile, Target: "/home/shed/.gitconfig", ReadOnly: true},
	}

	dirCreds, fileCreds := ClassifyCredentials(creds)

	if len(dirCreds) != 0 {
		t.Errorf("expected 0 dir credentials, got %d", len(dirCreds))
	}
	if len(fileCreds) != 1 {
		t.Fatalf("expected 1 file credential, got %d", len(fileCreds))
	}
	if _, ok := fileCreds["git"]; !ok {
		t.Error("expected 'git' in fileCreds")
	}
}

func TestClassifyCredentialsMissing(t *testing.T) {
	creds := map[string]config.MountConfig{
		"gone": {Source: "/nonexistent/path", Target: "/home/shed/.gone"},
	}

	dirCreds, fileCreds := ClassifyCredentials(creds)

	if len(dirCreds) != 0 {
		t.Errorf("expected 0 dir credentials, got %d", len(dirCreds))
	}
	if len(fileCreds) != 0 {
		t.Errorf("expected 0 file credentials, got %d", len(fileCreds))
	}
}

func TestClassifyCredentialsMixed(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(tmpFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	creds := map[string]config.MountConfig{
		"dir_cred":  {Source: tmpDir, Target: "/home/shed/.dir"},
		"file_cred": {Source: tmpFile, Target: "/home/shed/.file"},
		"missing":   {Source: "/nonexistent", Target: "/home/shed/.missing"},
	}

	dirCreds, fileCreds := ClassifyCredentials(creds)

	if len(dirCreds) != 1 {
		t.Fatalf("expected 1 dir credential, got %d", len(dirCreds))
	}
	if _, ok := dirCreds["dir_cred"]; !ok {
		t.Error("expected 'dir_cred' in dirCreds")
	}
	if len(fileCreds) != 1 {
		t.Fatalf("expected 1 file credential, got %d", len(fileCreds))
	}
	if _, ok := fileCreds["file_cred"]; !ok {
		t.Error("expected 'file_cred' in fileCreds")
	}
}

func TestClassifyCredentialsSymlinkToDir(t *testing.T) {
	tmpDir := t.TempDir()
	symlinkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(tmpDir, symlinkDir); err != nil {
		t.Fatal(err)
	}

	creds := map[string]config.MountConfig{
		"linked": {Source: symlinkDir, Target: "/home/shed/.linked"},
	}

	dirCreds, fileCreds := ClassifyCredentials(creds)

	if len(dirCreds) != 1 {
		t.Fatalf("expected 1 dir credential (symlink to dir), got %d", len(dirCreds))
	}
	if len(fileCreds) != 0 {
		t.Errorf("expected 0 file credentials, got %d", len(fileCreds))
	}
}

func TestHasWritableCredentials(t *testing.T) {
	tests := []struct {
		name     string
		creds    map[string]config.MountConfig
		expected bool
	}{
		{
			name:     "empty",
			creds:    map[string]config.MountConfig{},
			expected: false,
		},
		{
			name: "readonly_only",
			creds: map[string]config.MountConfig{
				"git": {ReadOnly: true},
			},
			expected: false,
		},
		{
			name: "writable",
			creds: map[string]config.MountConfig{
				"config": {ReadOnly: false},
			},
			expected: true,
		},
		{
			name: "mixed",
			creds: map[string]config.MountConfig{
				"ro": {ReadOnly: true},
				"rw": {ReadOnly: false},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasWritableCredentials(tt.creds)
			if got != tt.expected {
				t.Errorf("HasWritableCredentials() = %v, want %v", got, tt.expected)
			}
		})
	}
}
