package vmimage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsDockerRef(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want bool
	}{
		// Valid Docker refs
		{name: "registry with tag", s: "ghcr.io/charliek/shed-vz-base:v1.0.0", want: true},
		{name: "registry with latest", s: "ghcr.io/charliek/shed-vz-base:latest", want: true},
		{name: "simple image with tag", s: "ubuntu:24.04", want: true},
		{name: "simple image latest", s: "ubuntu:latest", want: true},
		{name: "bare image name", s: "ubuntu", want: true},
		{name: "localhost registry", s: "localhost:5000/myimage:v1", want: true},
		{name: "company registry", s: "registry.company.com/shed-custom:latest", want: true},
		{name: "digest ref", s: "ghcr.io/charliek/shed-vz-base@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", want: true},
		{name: "nested path", s: "registry.co/org/team/image:v1", want: true},

		// Filesystem paths (not Docker refs)
		{name: "absolute path", s: "/var/lib/shed/rootfs.ext4", want: false},
		{name: "home dir path", s: "~/Library/Application Support/shed/vz/default-rootfs.ext4", want: false},
		{name: "relative path", s: "./my-image.ext4", want: false},
		{name: "empty string", s: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsDockerRef(tt.s)
			if got != tt.want {
				t.Errorf("IsDockerRef(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestRootfsFilename(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "default", want: "default-rootfs.ext4"},
		{name: "base", want: "base-rootfs.ext4"},
		{name: "my-custom", want: "my-custom-rootfs.ext4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RootfsFilename(tt.name)
			if got != tt.want {
				t.Errorf("RootfsFilename(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestSourceFilename(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "default", want: "default-rootfs.ext4.source"},
		{name: "base", want: "base-rootfs.ext4.source"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SourceFilename(tt.name)
			if got != tt.want {
				t.Errorf("SourceFilename(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestCheckCache(t *testing.T) {
	t.Run("hit", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "default-rootfs.ext4"), []byte("data"), 0644)
		os.WriteFile(filepath.Join(dir, "default-rootfs.ext4.source"), []byte("ghcr.io/test:v1\n"), 0644)

		got := CheckCache(dir, "default", "ghcr.io/test:v1")
		want := filepath.Join(dir, "default-rootfs.ext4")
		if got != want {
			t.Errorf("CheckCache() = %q, want %q", got, want)
		}
	})

	t.Run("miss no file", func(t *testing.T) {
		dir := t.TempDir()
		if got := CheckCache(dir, "default", "ghcr.io/test:v1"); got != "" {
			t.Errorf("CheckCache() = %q, want empty", got)
		}
	})

	t.Run("stale source", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "default-rootfs.ext4"), []byte("data"), 0644)
		os.WriteFile(filepath.Join(dir, "default-rootfs.ext4.source"), []byte("ghcr.io/test:v1\n"), 0644)

		if got := CheckCache(dir, "default", "ghcr.io/test:v2"); got != "" {
			t.Errorf("CheckCache() = %q, want empty (stale source)", got)
		}
	})

	t.Run("no sidecar file", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "default-rootfs.ext4"), []byte("data"), 0644)

		if got := CheckCache(dir, "default", "ghcr.io/test:v1"); got != "" {
			t.Errorf("CheckCache() = %q, want empty (no sidecar)", got)
		}
	})
}

func TestWriteSource(t *testing.T) {
	dir := t.TempDir()
	err := WriteSource(dir, "default", "ghcr.io/test:v1")
	if err != nil {
		t.Fatalf("WriteSource() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "default-rootfs.ext4.source"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	if string(data) != "ghcr.io/test:v1\n" {
		t.Errorf("source file content = %q, want %q", string(data), "ghcr.io/test:v1\n")
	}
}
