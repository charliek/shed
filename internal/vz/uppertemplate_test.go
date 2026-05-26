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
	size := int64(2048)

	// A file with the ext4 magic (0x53 0xEF) at offset 1080.
	good := filepath.Join(dir, "good.img")
	buf := make([]byte, size)
	buf[1080] = 0x53
	buf[1081] = 0xEF
	if err := os.WriteFile(good, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasExt4Magic(good) {
		t.Error("expected ext4 magic to be detected")
	}
	if !validTemplate(good, size) {
		t.Error("expected good file to be a valid template")
	}
	if validTemplate(good, size+1) {
		t.Error("size mismatch must invalidate the template")
	}

	// A file without the magic.
	bad := filepath.Join(dir, "bad.img")
	if err := os.WriteFile(bad, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasExt4Magic(bad) {
		t.Error("did not expect ext4 magic in a zeroed file")
	}

	// A short file (no byte at offset 1080).
	short := filepath.Join(dir, "short.img")
	if err := os.WriteFile(short, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasExt4Magic(short) {
		t.Error("short file cannot carry the magic")
	}

	if validTemplate(filepath.Join(dir, "missing.img"), size) {
		t.Error("missing file is not a valid template")
	}
}

func TestResolveBuildToolsRef(t *testing.T) {
	t.Setenv("SHED_BUILD_TOOLS_REF", "shed-build-tools:dev")
	if got := resolveBuildToolsRef(); got != "shed-build-tools:dev" {
		t.Errorf("env override not honored: got %q", got)
	}

	t.Setenv("SHED_BUILD_TOOLS_REF", "")
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })

	version.Version = "v0.5.3"
	if got := resolveBuildToolsRef(); got != "ghcr.io/charliek/shed-build-tools:v0.5.3" {
		t.Errorf("release version: got %q", got)
	}

	for _, dev := range []string{"dev", "v0.5.3-2-g493976f-dirty", "v0.5.3-5-gabcdef0"} {
		version.Version = dev
		if got := resolveBuildToolsRef(); got != "" {
			t.Errorf("dev/dirty version %q should yield no ref, got %q", dev, got)
		}
	}
}

func TestEnsureUpperTemplateNoRef(t *testing.T) {
	// With no build-tools ref, EnsureUpperTemplate must error (caller
	// then falls back to in-guest mkfs) rather than attempting docker.
	if _, err := EnsureUpperTemplate(t.Context(), t.TempDir(), "", 5<<30, ""); err == nil {
		t.Error("expected error when build-tools ref is empty")
	}
}
