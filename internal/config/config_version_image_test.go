package config

import (
	"strings"
	"testing"

	"github.com/charliek/shed/internal/version"
	"github.com/charliek/shed/internal/vmimage"
)

// setVersion swaps the package-global build version for the duration of a
// test, restoring it on cleanup. Tests in this package run sequentially, so a
// global swap is safe.
func setVersion(t *testing.T, v string) {
	t.Helper()
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })
	version.Version = v
}

func TestExpandImageVersion(t *testing.T) {
	t.Run("no token is identity", func(t *testing.T) {
		setVersion(t, "0.6.2")
		const in = "ghcr.io/charliek/shed-vz-full:v0.5.1"
		if got := expandImageVersion(in); got != in {
			t.Errorf("expandImageVersion(%q) = %q, want unchanged", in, got)
		}
	})

	t.Run("release build expands token", func(t *testing.T) {
		setVersion(t, "0.6.2")
		got := expandImageVersion("ghcr.io/charliek/shed-vz-full:${shed.version}")
		if want := "ghcr.io/charliek/shed-vz-full:v0.6.2"; got != want {
			t.Errorf("expandImageVersion = %q, want %q", got, want)
		}
	})

	t.Run("dev build leaves token in place", func(t *testing.T) {
		setVersion(t, "dev")
		const in = "ghcr.io/charliek/shed-vz-full:${shed.version}"
		if got := expandImageVersion(in); got != in {
			t.Errorf("expandImageVersion(%q) = %q, want token left for Validate to reject", in, got)
		}
	})
}

func TestApplyDefaultsTokenExpansion(t *testing.T) {
	setVersion(t, "0.6.2")
	cfg := &VZConfig{
		DefaultImage: "ghcr.io/charliek/shed-vz-full:${shed.version}",
		ImageAliases: map[string]string{
			"base": "ghcr.io/charliek/shed-vz-base:${shed.version}",
		},
	}
	cfg.applyDefaults()

	if want := "ghcr.io/charliek/shed-vz-full:v0.6.2"; cfg.DefaultImage != want {
		t.Errorf("DefaultImage = %q, want %q", cfg.DefaultImage, want)
	}
	if want := "ghcr.io/charliek/shed-vz-base:v0.6.2"; cfg.ImageAliases["base"] != want {
		t.Errorf("ImageAliases[base] = %q, want %q", cfg.ImageAliases["base"], want)
	}
	if err := cfg.Validate(); err != nil {
		// Validate touches more than images; only assert no token error.
		if got := err.Error(); containsToken(got) {
			t.Errorf("Validate() reported an unexpanded token after expansion: %v", err)
		}
	}
}

func TestApplyDefaultsSynthesizesDefaultOnReleaseBuild(t *testing.T) {
	setVersion(t, "0.6.2")

	t.Run("vz", func(t *testing.T) {
		cfg := &VZConfig{} // no image refs configured at all
		cfg.applyDefaults()
		if want := "ghcr.io/charliek/shed-vz-full:v0.6.2"; cfg.DefaultImage != want {
			t.Errorf("synthesized DefaultImage = %q, want %q", cfg.DefaultImage, want)
		}
		assertAliasPair(t, cfg.ImageAliases, "vz")
	})

	t.Run("firecracker", func(t *testing.T) {
		cfg := &FirecrackerConfig{}
		cfg.applyDefaults()
		if want := "ghcr.io/charliek/shed-fc-full:v0.6.2"; cfg.DefaultImage != want {
			t.Errorf("synthesized DefaultImage = %q, want %q", cfg.DefaultImage, want)
		}
		assertAliasPair(t, cfg.ImageAliases, "fc")
	})
}

func TestApplyDefaultsNoSynthesisOnDevBuild(t *testing.T) {
	setVersion(t, "dev")
	cfg := &VZConfig{}
	cfg.applyDefaults()
	if cfg.DefaultImage != "" {
		t.Errorf("dev build should not synthesize a default_image, got %q", cfg.DefaultImage)
	}
	if len(cfg.ImageAliases) != 0 {
		t.Errorf("dev build should not synthesize aliases, got %v", cfg.ImageAliases)
	}
}

// An explicitly-set default_image is never overwritten by synthesis.
func TestApplyDefaultsKeepsExplicitDefault(t *testing.T) {
	setVersion(t, "0.6.2")
	cfg := &VZConfig{DefaultImage: "ghcr.io/charliek/shed-vz-full:v0.5.1"}
	cfg.applyDefaults()
	if want := "ghcr.io/charliek/shed-vz-full:v0.5.1"; cfg.DefaultImage != want {
		t.Errorf("DefaultImage = %q, want the explicit ref untouched", cfg.DefaultImage)
	}
}

func TestValidateRejectsUnexpandedToken(t *testing.T) {
	setVersion(t, "dev")
	cfg := &VZConfig{
		VfkitPath:    "vfkit", // set so Validate reaches the token check
		DefaultImage: "ghcr.io/charliek/shed-vz-full:${shed.version}",
	}
	cfg.applyDefaults() // token survives on a dev build
	err := cfg.Validate()
	if err == nil || !containsToken(err.Error()) {
		t.Fatalf("Validate() = %v, want an error naming the unexpanded token", err)
	}

	fc := &FirecrackerConfig{
		ImageAliases: map[string]string{"base": "ghcr.io/charliek/shed-fc-base:${shed.version}"},
		// fill required fields so we reach the token check / fail only on it
		InstanceDir: "/tmp/i", SnapshotsDir: "/tmp/s", SocketDir: "/tmp/k",
		DefaultCPUs: 2, DefaultMemoryMB: 1024, DefaultDiskGB: 10,
		ConsolePort: 1024, NotifyPort: 1026, VsockBaseCID: 100,
	}
	fc.applyDefaults()
	if err := fc.Validate(); err == nil || !containsToken(err.Error()) {
		t.Fatalf("FC Validate() = %v, want an error naming the unexpanded token", err)
	}
}

// Regression for prune protection: after load, the resolved default and
// aliases must be concrete Docker refs so vmimage's reachability walk
// (configuredRefs -> IsDockerRef) protects their blobs. A synthesized or
// token-expanded ref that didn't satisfy IsDockerRef would be silently
// dropped from prune protection and GC'd out from under a fresh create.
func TestSynthesizedRefsAreProtectableDockerRefs(t *testing.T) {
	setVersion(t, "0.6.2")
	cfg := &VZConfig{
		DefaultImage: "ghcr.io/charliek/shed-vz-full:${shed.version}",
	}
	cfg.applyDefaults()

	if !vmimage.IsDockerRef(cfg.GetDefaultImage()) {
		t.Errorf("GetDefaultImage() = %q is not a Docker ref; prune would not protect it", cfg.GetDefaultImage())
	}
	for name, ref := range cfg.GetImageAliases() {
		if !vmimage.IsDockerRef(ref) {
			t.Errorf("alias %q = %q is not a Docker ref; prune would not protect it", name, ref)
		}
	}
}

// assertAliasPair: a release build synthesizes the two variants, base and
// full. The extensions variant was removed in v0.11 (plan 039) and has no alias.
func assertAliasPair(t *testing.T, aliases map[string]string, backend string) {
	t.Helper()
	want := map[string]string{
		"base": "ghcr.io/charliek/shed-" + backend + "-base:v0.6.2",
		"full": "ghcr.io/charliek/shed-" + backend + "-full:v0.6.2",
	}
	if len(aliases) != len(want) {
		t.Fatalf("aliases = %v, want %v", aliases, want)
	}
	for k, v := range want {
		if aliases[k] != v {
			t.Errorf("alias %q = %q, want %q", k, aliases[k], v)
		}
	}
}

// --image extensions on a release build resolves exactly as any other
// unknown word does (a bare Docker Hub ref that is not a shed image), never to
// a shed-<backend>-extensions image: the extensions variant was removed in
// v0.11 (plan 039 §3.12) and no alias names it.
func TestRetiredExtensionsAliasIsUnknown(t *testing.T) {
	setVersion(t, "0.6.2")
	for _, backend := range []string{"vz", "fc"} {
		t.Run(backend, func(t *testing.T) {
			var resolve func(string) (ResolvedImage, error)
			var aliases map[string]string
			if backend == "vz" {
				cfg := &VZConfig{ImagesDir: t.TempDir()}
				cfg.applyDefaults()
				resolve, aliases = cfg.ResolveImage, cfg.ImageAliases
			} else {
				cfg := &FirecrackerConfig{ImagesDir: t.TempDir()}
				cfg.applyDefaults()
				resolve, aliases = cfg.ResolveImage, cfg.ImageAliases
			}
			if _, ok := aliases["extensions"]; ok {
				t.Fatalf("aliases = %v, want no extensions alias", aliases)
			}
			got, err := resolve("extensions")
			if err != nil {
				t.Fatalf("ResolveImage(extensions): %v", err)
			}
			unknown, err := resolve("nosuchvariant")
			if err != nil {
				t.Fatalf("ResolveImage(nosuchvariant): %v", err)
			}
			if got.Path != "" || unknown.Path != "" {
				t.Fatalf("paths = %q, %q; want none for an unknown word", got.Path, unknown.Path)
			}
			if want := strings.Replace(unknown.DockerRef, "nosuchvariant", "extensions", 1); got.DockerRef != want {
				t.Errorf("ResolveImage(extensions).DockerRef = %q, want %q (as an unknown word resolves)", got.DockerRef, want)
			}
			if strings.Contains(got.DockerRef, "charliek/shed-") {
				t.Errorf("ResolveImage(extensions).DockerRef = %q names a shed image", got.DockerRef)
			}
		})
	}
}

func containsToken(s string) bool {
	return strings.Contains(s, shedVersionToken)
}
