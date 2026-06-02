package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVZConfigApplyDefaultsImageFields(t *testing.T) {
	home, _ := os.UserHomeDir()
	cfg := &VZConfig{
		DefaultImage: "ghcr.io/charliek/shed-vz-full:v1",
		ImageAliases: map[string]string{
			"local":  "~/shed/base.ext4",
			"remote": "ghcr.io/charliek/shed-vz-base:v1",
		},
	}
	cfg.applyDefaults()

	if cfg.PullPolicy != "missing" {
		t.Errorf("PullPolicy = %q, want missing (default)", cfg.PullPolicy)
	}
	// Docker-ref default_image is left untouched.
	if cfg.DefaultImage != "ghcr.io/charliek/shed-vz-full:v1" {
		t.Errorf("DefaultImage = %q, want unchanged docker ref", cfg.DefaultImage)
	}
	// Local-path alias is tilde-expanded; docker-ref alias is not.
	if want := filepath.Join(home, "shed/base.ext4"); cfg.ImageAliases["local"] != want {
		t.Errorf("ImageAliases[local] = %q, want %q", cfg.ImageAliases["local"], want)
	}
	if cfg.ImageAliases["remote"] != "ghcr.io/charliek/shed-vz-base:v1" {
		t.Errorf("ImageAliases[remote] = %q, want unchanged docker ref", cfg.ImageAliases["remote"])
	}
}

// resolveImageRef is shared by both backends; exercise it through VZConfig.
func TestResolveImageSelectors(t *testing.T) {
	dir := t.TempDir()
	cfg := &VZConfig{
		DefaultImage: "ghcr.io/charliek/shed-vz-full:v0.6.0",
		ImageAliases: map[string]string{
			"extensions": "ghcr.io/charliek/shed-vz-extensions:v0.6.0",
		},
		PullPolicy: "missing",
		ImagesDir:  dir,
	}

	t.Run("empty selector resolves default_image as a ref", func(t *testing.T) {
		got, err := cfg.ResolveImage("")
		if err != nil {
			t.Fatalf("ResolveImage(\"\"): %v", err)
		}
		if got.DockerRef != cfg.DefaultImage {
			t.Errorf("DockerRef = %q, want %q", got.DockerRef, cfg.DefaultImage)
		}
		if got.Path != "" {
			t.Errorf("Path = %q, want empty for an uncached ref", got.Path)
		}
		if got.PullPolicy != "missing" {
			t.Errorf("PullPolicy = %q, want missing", got.PullPolicy)
		}
	})

	t.Run("alias resolves to its underlying ref", func(t *testing.T) {
		got, err := cfg.ResolveImage("extensions")
		if err != nil {
			t.Fatalf("ResolveImage(extensions): %v", err)
		}
		if got.DockerRef != "ghcr.io/charliek/shed-vz-extensions:v0.6.0" {
			t.Errorf("DockerRef = %q, want the aliased ref", got.DockerRef)
		}
	})

	t.Run("bare docker ref used directly", func(t *testing.T) {
		got, err := cfg.ResolveImage("ghcr.io/other/img:v9")
		if err != nil {
			t.Fatalf("ResolveImage(ref): %v", err)
		}
		if got.DockerRef != "ghcr.io/other/img:v9" {
			t.Errorf("DockerRef = %q", got.DockerRef)
		}
	})

	t.Run("absolute path escape hatch", func(t *testing.T) {
		got, err := cfg.ResolveImage("/dev/null")
		if err != nil {
			t.Fatalf("ResolveImage(/dev/null): %v", err)
		}
		if got.Path != "/dev/null" {
			t.Errorf("Path = %q, want /dev/null", got.Path)
		}
	})

	t.Run("local tag label resolves from the blob store", func(t *testing.T) {
		labelDir := t.TempDir()
		want := installCachedBlob(t, labelDir, "mylabel", "ghcr.io/example/custom:v1")
		c := &VZConfig{DefaultImage: "ghcr.io/x/y:v1", ImagesDir: labelDir}
		got, err := c.ResolveImage("mylabel")
		if err != nil {
			t.Fatalf("ResolveImage(mylabel): %v", err)
		}
		if got.Path != want {
			t.Errorf("Path = %q, want %q (resolved from tag)", got.Path, want)
		}
	})

	t.Run("bare word is treated as a docker.io ref (Docker semantics)", func(t *testing.T) {
		// A bare token parses as docker.io/library/<token>, so it resolves
		// to a Docker ref (the pull error, if any, surfaces at create time)
		// rather than failing at config resolution.
		got, err := cfg.ResolveImage("someimage")
		if err != nil {
			t.Fatalf("ResolveImage(someimage): %v", err)
		}
		if got.DockerRef == "" {
			t.Errorf("bare word should resolve to a DockerRef, got %#v", got)
		}
	})

	t.Run("missing local path errors", func(t *testing.T) {
		_, err := cfg.ResolveImage("/nonexistent/rootfs.ext4")
		if err == nil {
			t.Fatal("expected error for a missing local path")
		}
	})

	t.Run("empty selector with no default_image errors", func(t *testing.T) {
		c := &VZConfig{ImagesDir: dir}
		_, err := c.ResolveImage("")
		if err == nil {
			t.Fatal("expected error when no default_image configured")
		}
		if !strings.Contains(err.Error(), "default_image") {
			t.Errorf("error = %q, want mention of default_image", err.Error())
		}
	})
}

func TestResolveBaseRootfsReturnsDefaultImage(t *testing.T) {
	cfg := &VZConfig{
		DefaultImage: "ghcr.io/charliek/shed-vz-full:v0.6.0",
		PullPolicy:   "always",
		ImagesDir:    t.TempDir(),
	}
	got, err := cfg.ResolveBaseRootfs()
	if err != nil {
		t.Fatalf("ResolveBaseRootfs: %v", err)
	}
	if got.DockerRef != cfg.DefaultImage {
		t.Errorf("DockerRef = %q, want %q", got.DockerRef, cfg.DefaultImage)
	}
	if got.Name == "_base" {
		t.Error("Name must not be the removed internal _base tag")
	}
	if got.PullPolicy != "always" {
		t.Errorf("PullPolicy = %q, want always", got.PullPolicy)
	}

	empty := &VZConfig{ImagesDir: t.TempDir()}
	if _, err := empty.ResolveBaseRootfs(); err == nil {
		t.Error("ResolveBaseRootfs with no default_image should error")
	}
}

func TestPullPolicyValidation(t *testing.T) {
	for _, tc := range []struct {
		policy  string
		wantErr bool
	}{
		{"", false},
		{"missing", false},
		{"always", false},
		{"never", false},
		{"sometimes", true},
	} {
		cfg := validVZConfig()
		cfg.PullPolicy = tc.policy
		err := cfg.Validate()
		if tc.wantErr && err == nil {
			t.Errorf("pull_policy %q: expected validation error", tc.policy)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("pull_policy %q: unexpected error %v", tc.policy, err)
		}
	}
}

func TestValidateAliasPaths(t *testing.T) {
	t.Run("docker-ref alias skips path validation", func(t *testing.T) {
		cfg := validVZConfig()
		cfg.ImageAliases = map[string]string{"custom": "ghcr.io/charliek/shed-vz-custom:v1"}
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
	})
	t.Run("missing local-path alias fails", func(t *testing.T) {
		cfg := validVZConfig()
		cfg.ImageAliases = map[string]string{"local": "/nonexistent/rootfs.ext4"}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "image_aliases") {
			t.Errorf("Validate() = %v, want image_aliases path error", err)
		}
	})
}

func TestRejectRemovedImageKeys(t *testing.T) {
	for _, tc := range []struct {
		name    string
		yaml    string
		wantErr bool
	}{
		{
			name:    "base_rootfs rejected",
			yaml:    "name: s\nvz:\n  base_rootfs: ghcr.io/x/y:v1\n",
			wantErr: true,
		},
		{
			name:    "images map rejected",
			yaml:    "name: s\nfirecracker:\n  images:\n    full: ghcr.io/x/y:v1\n",
			wantErr: true,
		},
		{
			name:    "new keys accepted",
			yaml:    "name: s\nvz:\n  default_image: ghcr.io/x/y:v1\n  pull_policy: missing\n",
			wantErr: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "server.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadServerConfigFromPath(p)
			gotRejected := err != nil && strings.Contains(err.Error(), "removed in v0.6.0")
			if tc.wantErr && !gotRejected {
				t.Errorf("expected removed-key rejection, got err=%v", err)
			}
			if !tc.wantErr && gotRejected {
				t.Errorf("unexpected removed-key rejection: %v", err)
			}
		})
	}
}
