package vmimage

import "testing"

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
