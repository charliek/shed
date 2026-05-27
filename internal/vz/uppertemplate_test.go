//go:build darwin
// +build darwin

package vz

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charliek/shed/internal/version"
)

func TestUpperTemplatePathKeyedBySize(t *testing.T) {
	dir := "/tmp/templates"
	a := upperTemplatePath(dir, 5*1024*1024*1024)
	b := upperTemplatePath(dir, 20*1024*1024*1024)
	if a == b {
		t.Fatalf("templates of different sizes must have distinct paths: %q == %q", a, b)
	}
	if filepath.Dir(a) != dir {
		t.Errorf("template path %q not under %q", a, dir)
	}
}

func TestHasExt4MagicAndValidTemplate(t *testing.T) {
	dir := t.TempDir()
	const size = int64(2048)

	ext4 := make([]byte, size)
	ext4[1080] = 0x53
	ext4[1081] = 0xEF

	tests := []struct {
		name      string
		content   []byte
		write     bool // false => file does not exist
		wantMagic bool
		wantValid bool // validTemplate(path, size)
	}{
		{name: "ext4_magic_present", content: ext4, write: true, wantMagic: true, wantValid: true},
		{name: "zeroed_no_magic", content: make([]byte, size), write: true, wantMagic: false, wantValid: false},
		{name: "too_short_for_magic", content: []byte("hi"), write: true, wantMagic: false, wantValid: false},
		{name: "missing_file", write: false, wantMagic: false, wantValid: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".img")
			if tc.write {
				if err := os.WriteFile(path, tc.content, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := hasExt4Magic(path); got != tc.wantMagic {
				t.Errorf("hasExt4Magic = %v, want %v", got, tc.wantMagic)
			}
			if got := validTemplate(path, size); got != tc.wantValid {
				t.Errorf("validTemplate = %v, want %v", got, tc.wantValid)
			}
		})
	}

	// A correctly-formed template must also be rejected on a size mismatch.
	good := filepath.Join(dir, "good.img")
	if err := os.WriteFile(good, ext4, 0o644); err != nil {
		t.Fatal(err)
	}
	if validTemplate(good, size+1) {
		t.Error("size mismatch must invalidate the template")
	}
}

func TestResolveBuildToolsRef(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })

	tests := []struct {
		name    string
		envRef  string
		ver     string
		wantRef string
	}{
		{name: "env_override_wins", envRef: "shed-build-tools:dev", ver: "v0.5.3", wantRef: "shed-build-tools:dev"},
		// Release binaries embed "X.Y.Z" with no leading v; the ref must
		// still be the v-prefixed published tag (the v0.5.4 regression).
		{name: "release_version_no_v", envRef: "", ver: "0.5.4", wantRef: "ghcr.io/charliek/shed-build-tools:v0.5.4"},
		{name: "release_version_with_v", envRef: "", ver: "v0.5.3", wantRef: "ghcr.io/charliek/shed-build-tools:v0.5.3"},
		{name: "dev_version_none", envRef: "", ver: "dev", wantRef: ""},
		{name: "dirty_version_none", envRef: "", ver: "v0.5.3-2-g493976f-dirty", wantRef: ""},
		{name: "untagged_commit_none", envRef: "", ver: "v0.5.3-5-gabcdef0", wantRef: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHED_BUILD_TOOLS_REF", tc.envRef)
			version.Version = tc.ver
			if got := resolveBuildToolsRef(); got != tc.wantRef {
				t.Errorf("resolveBuildToolsRef() = %q, want %q", got, tc.wantRef)
			}
		})
	}
}

func TestEnsureUpperTemplateNoRef(t *testing.T) {
	// With no build-tools ref, EnsureUpperTemplate must error (caller then
	// falls back to in-guest mkfs) rather than attempting docker.
	if _, err := EnsureUpperTemplate(t.Context(), t.TempDir(), "", 5<<30, ""); err == nil {
		t.Error("expected error when build-tools ref is empty")
	}
}
