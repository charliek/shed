package main

import (
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
