package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

// TestImageBackendContextForHost locks in the per-target table that
// drives `shed image build`. Regressing any of these rows reintroduces
// the cross-build prefix bug (Linux runner producing a `shed-fc-*`
// tag for a `--target shed-vz-*` build, which then corrupts the
// io.shed.source-ref annotation and breaks server-side cache hits).
func TestImageBackendContextForHost(t *testing.T) {
	linuxFCStore := config.DefaultFirecrackerImagesDir
	darwinVZStore := config.ExpandPath(config.DefaultVZImagesDir)

	tests := []struct {
		name          string
		target        string
		goos          string
		wantPrefix    string
		wantPlatform  string
		wantOutputDir string
		wantExtract   bool
		wantInitrd    bool
	}{
		{
			name:          "shed-vz target on linux host (cross-build)",
			target:        "shed-vz-base",
			goos:          "linux",
			wantPrefix:    "shed-vz-",
			wantPlatform:  vmimage.DefaultPlatform, // linux/arm64
			wantOutputDir: linuxFCStore,            // OutputDir follows host OS
			wantExtract:   true,
			wantInitrd:    true,
		},
		{
			name:          "shed-vz target on darwin host (native)",
			target:        "shed-vz-full",
			goos:          "darwin",
			wantPrefix:    "shed-vz-",
			wantPlatform:  vmimage.DefaultPlatform,
			wantOutputDir: darwinVZStore,
			wantExtract:   true,
			wantInitrd:    true,
		},
		{
			name:          "shed-fc target on linux host (native)",
			target:        "shed-fc-full",
			goos:          "linux",
			wantPrefix:    "shed-fc-",
			wantPlatform:  vmimage.FirecrackerPlatform, // linux/amd64
			wantOutputDir: linuxFCStore,
			wantExtract:   true,
			wantInitrd:    false,
		},
		{
			name:          "shed-fc target on darwin host (cross-build)",
			target:        "shed-fc-experimental",
			goos:          "darwin",
			wantPrefix:    "shed-fc-",
			wantPlatform:  vmimage.FirecrackerPlatform,
			wantOutputDir: darwinVZStore, // OutputDir follows host OS
			wantExtract:   true,
			wantInitrd:    false,
		},
		{
			name:          "empty target on darwin → VZ defaults",
			target:        "",
			goos:          "darwin",
			wantPrefix:    "shed-vz-",
			wantPlatform:  vmimage.DefaultPlatform,
			wantOutputDir: darwinVZStore,
			wantExtract:   true,
			wantInitrd:    true,
		},
		{
			name:          "empty target on linux → FC defaults",
			target:        "",
			goos:          "linux",
			wantPrefix:    "shed-fc-",
			wantPlatform:  vmimage.FirecrackerPlatform,
			wantOutputDir: linuxFCStore,
			wantExtract:   true,
			wantInitrd:    false,
		},
		{
			name:          "non-shed target on linux falls through to host default",
			target:        "custom-stage",
			goos:          "linux",
			wantPrefix:    "shed-fc-",
			wantPlatform:  vmimage.FirecrackerPlatform,
			wantOutputDir: linuxFCStore,
			wantExtract:   true,
			wantInitrd:    false,
		},
		{
			name:          "non-shed target on darwin falls through to host default",
			target:        "custom-stage",
			goos:          "darwin",
			wantPrefix:    "shed-vz-",
			wantPlatform:  vmimage.DefaultPlatform,
			wantOutputDir: darwinVZStore,
			wantExtract:   true,
			wantInitrd:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bc := imageBackendContextForHost(tt.target, tt.goos)
			if bc.Prefix != tt.wantPrefix {
				t.Errorf("Prefix = %q, want %q", bc.Prefix, tt.wantPrefix)
			}
			if bc.Platform != tt.wantPlatform {
				t.Errorf("Platform = %q, want %q", bc.Platform, tt.wantPlatform)
			}
			if bc.OutputDir != tt.wantOutputDir {
				t.Errorf("OutputDir = %q, want %q", bc.OutputDir, tt.wantOutputDir)
			}
			if bc.ExtractKernel != tt.wantExtract {
				t.Errorf("ExtractKernel = %v, want %v", bc.ExtractKernel, tt.wantExtract)
			}
			if bc.NeedsInitrd != tt.wantInitrd {
				t.Errorf("NeedsInitrd = %v, want %v", bc.NeedsInitrd, tt.wantInitrd)
			}
		})
	}
}

// TestRecordBuiltImage is the #227 regression guard: a local `shed image build`
// must populate the ref-index (not just the cosmetic tag), so a subsequent
// `shed create --image <sourceRef>` — which resolves through RefIndexGet — sees
// the freshly built manifest instead of a stale digest left by an earlier pull.
// Before the fix, convertAndInstall called SetTag only, so the build was
// invisible to create and the dev loop required a hand-edited refs/<hash>.json.
func TestRecordBuiltImage(t *testing.T) {
	imagesDir := t.TempDir()
	const sourceRef = "ghcr.io/charliek/shed-vz-base:v9.9.9"
	// recordBuiltImage doesn't hash blob bytes; RefIndexGet only requires the
	// referenced blob to exist on disk, so stand in a dummy blob at the path for
	// an arbitrary valid-hex digest.
	digest := "sha256:" + strings.Repeat("a", 64)
	blobPath, err := vmimage.BlobPath(imagesDir, digest)
	if err != nil {
		t.Fatalf("BlobPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	if err := os.WriteFile(blobPath, []byte("manifest"), 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	if err := recordBuiltImage(imagesDir, "base", sourceRef, digest); err != nil {
		t.Fatalf("recordBuiltImage: %v", err)
	}

	got, ok := vmimage.RefIndexGet(imagesDir, sourceRef)
	if !ok {
		t.Fatal("RefIndexGet missed after recordBuiltImage — build did not write the ref-index (#227)")
	}
	if got != digest {
		t.Errorf("RefIndexGet = %q, want %q", got, digest)
	}

	// The cosmetic tag must still advance (pre-existing behavior preserved).
	tag, err := vmimage.GetTag(imagesDir, "base")
	if err != nil {
		t.Fatalf("GetTag: %v", err)
	}
	if tag.Digest != digest {
		t.Errorf("tag digest = %q, want %q", tag.Digest, digest)
	}
}
