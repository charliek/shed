// Test helpers for installing synthetic OCI images. Not in a _test.go
// file so cross-package tests (internal/vz, internal/firecracker,
// internal/config) can reuse them without duplicating the fixture
// boilerplate.

package vmimage

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// InstallSyntheticImage writes a faux OCI image into imagesDir using
// the provided rootfsContent bytes as the layer content. Tags the
// resulting manifest at tagName.
//
// The synthetic image consists of:
//   - one layer blob (the raw rootfsContent — not actually gzipped, but
//     stored at a sha256-keyed blob path so the OCI shape is correct)
//   - one config blob (minimal OCI image config)
//   - one manifest blob with the standard annotations
//   - optional kernel + initrd blobs referenced by manifest annotations
//   - a tag pointing at the manifest digest
//
// Returns the manifest digest. Used by tests that previously called
// InstallBlob with synthetic content.
func InstallSyntheticImage(imagesDir, tagName, sourceRef string, rootfsContent, kernelContent, initrdContent []byte) (string, error) {
	if err := EnsureOCILayout(imagesDir); err != nil {
		return "", err
	}

	// Layer blob (placeholder — real images store tar.gz here; for tests
	// the bytes are opaque content addressed by their sha256).
	layerDigest := DigestBytes(rootfsContent)
	if _, err := WriteBlob(imagesDir, layerDigest, rootfsContent); err != nil {
		return "", fmt.Errorf("writing layer blob: %w", err)
	}

	// Materialize a fake lower-image in the cache so callers that
	// expect a ready-to-boot image see CacheLowerExists() returning
	// true.
	cachePath, err := CacheLowerPath(imagesDir, layerDigest)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return "", err
	}
	// Layer cache is content-addressed; if the file is already in
	// place from an earlier synthetic install with the same content,
	// leave it alone (it's 0o444 and would otherwise refuse a rewrite).
	if _, statErr := os.Stat(cachePath); statErr != nil {
		if err := os.WriteFile(cachePath, rootfsContent, 0o444); err != nil {
			return "", fmt.Errorf("writing synthetic lower cache: %w", err)
		}
	}

	annotations := map[string]string{
		AnnotationSchemaVersion: ShedSchemaVersion,
		AnnotationVariant:       tagName,
		AnnotationSourceRef:     sourceRef,
	}

	if len(kernelContent) > 0 {
		kd := DigestBytes(kernelContent)
		if _, err := WriteBlob(imagesDir, kd, kernelContent); err != nil {
			return "", fmt.Errorf("writing kernel blob: %w", err)
		}
		annotations[AnnotationKernelDigest] = kd
	}
	if len(initrdContent) > 0 {
		id := DigestBytes(initrdContent)
		if _, err := WriteBlob(imagesDir, id, initrdContent); err != nil {
			return "", fmt.Errorf("writing initrd blob: %w", err)
		}
		annotations[AnnotationInitrdDigest] = id
	}

	cfg := &OCIConfig{
		Architecture: "arm64",
		OS:           "linux",
		Created:      time.Now().UTC().Format(time.RFC3339Nano),
		RootFS: OCIRootFS{
			Type:    "layers",
			DiffIDs: []string{layerDigest},
		},
	}
	cfgData, err := cfg.MarshalIndent()
	if err != nil {
		return "", err
	}
	configDigest := DigestBytes(cfgData)
	if _, err := WriteBlob(imagesDir, configDigest, cfgData); err != nil {
		return "", fmt.Errorf("writing config blob: %w", err)
	}

	manifest := &OCIManifest{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIManifest,
		Config: Descriptor{
			MediaType: MediaTypeOCIConfig,
			Digest:    configDigest,
			Size:      int64(len(cfgData)),
		},
		Layers: []Descriptor{{
			MediaType: MediaTypeOCILayer,
			Digest:    layerDigest,
			Size:      int64(len(rootfsContent)),
		}},
		Annotations: annotations,
	}
	manData, err := manifest.MarshalIndent()
	if err != nil {
		return "", err
	}
	manifestDigest := DigestBytes(manData)
	if _, err := WriteBlob(imagesDir, manifestDigest, manData); err != nil {
		return "", fmt.Errorf("writing manifest blob: %w", err)
	}

	// Pre-populate the manifest-digest-keyed cache lower so tests that
	// expect a ready-to-boot image skip the EnsureLowerFromManifest path
	// (which would try to flatten the synthetic non-tar layer content).
	mfCachePath, err := CacheLowerPath(imagesDir, manifestDigest)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(mfCachePath); statErr != nil {
		if err := os.MkdirAll(filepath.Dir(mfCachePath), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(mfCachePath, rootfsContent, 0o444); err != nil {
			return "", fmt.Errorf("writing synthetic manifest cache: %w", err)
		}
	}

	// Record the manifest in index.json so ListImages / PruneImages
	// can enumerate it without probe-reading every blob (matches the
	// behavior of the production Convert / PullToOCILayout paths).
	if err := indexUpsert(imagesDir, Descriptor{
		MediaType: MediaTypeOCIManifest,
		Digest:    manifestDigest,
		Size:      int64(len(manData)),
		Annotations: map[string]string{
			"org.opencontainers.image.ref.name": tagName,
		},
	}); err != nil {
		return "", fmt.Errorf("updating index for synthetic image: %w", err)
	}

	if tagName != "" {
		if err := SetTag(imagesDir, tagName, manifestDigest); err != nil {
			return "", fmt.Errorf("setting tag: %w", err)
		}
	}

	return manifestDigest, nil
}

// SyntheticDigestFromBytes returns a sha256 digest of the given content
// formatted as "sha256:<hex>". Used by tests that need a deterministic
// digest without writing to disk.
func SyntheticDigestFromBytes(content []byte) string {
	return DigestBytes(content)
}

// SyntheticDigestFromString is a convenience wrapper for tests using
// string literals to generate stable digests.
func SyntheticDigestFromString(s string) string {
	return DigestBytes([]byte(s))
}

// HexDigest returns the hex portion of a digest, panicking on malformed
// input. For test use only.
func HexDigest(digest string) string {
	h, err := digestHex(digest)
	if err != nil {
		panic(err)
	}
	_ = hex.EncodedLen(0) // keep import for older Go versions; harmless
	return h
}
