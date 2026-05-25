// OCI layer flattening with whiteout application.
//
// `MergeLayersFromManifest` reads the manifest's layers in OCI order
// and produces a single tar archive representing the merged tree
// (i.e., what the kernel overlay driver would see after stacking the
// layers). The output tar preserves uid/gid/mode/mtime from the
// source layer headers so a downstream `mkfs.erofs --tar=f` produces
// an image with faithful inode metadata. This is the host-native
// replacement for the v0.5.0 docker-fallback materializer (which
// did the same work inside a privileged Ubuntu container) and the
// v0.5.1 materializer-VM (which did it inside vfkit).
//
// Algorithm: single-pass, layers walked from HIGHEST OCI index to
// LOWEST. The first entry seen for a given path wins (mirrors "last
// layer applied wins" in forward order). A `path/.wh.name` marker
// suppresses any later (lower-index) entry whose path is `path/name`
// or a descendant of `path/name`. A `path/.wh..wh..opq` marker
// suppresses any later entry strictly under `path/`. Whiteout markers
// themselves are never written to the output tar.

package vmimage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
)

// MergeLayersFromManifest writes a flattened tar of the manifest's
// layers to outTarPath. Returns an error if the manifest is missing,
// has zero layers, or any layer blob fails to decode.
func MergeLayersFromManifest(ctx context.Context, imagesDir, manifestDigest, outTarPath string) error {
	manifest, err := LoadManifestByDigest(imagesDir, manifestDigest)
	if err != nil {
		return fmt.Errorf("loading manifest %s: %w", manifestDigest, err)
	}
	if len(manifest.Layers) == 0 {
		return fmt.Errorf("manifest %s has no layers", manifestDigest)
	}
	layerDigests := make([]string, len(manifest.Layers))
	for i, l := range manifest.Layers {
		layerDigests[i] = l.Digest
	}
	return MergeLayers(ctx, imagesDir, layerDigests, outTarPath)
}

// MergeLayers is the manifest-less variant of MergeLayersFromManifest
// used during image build, where we have the layer digests in hand
// but the shed-annotated manifest hasn't been minted yet. layerDigests
// must be in OCI order (lowest at index 0).
func MergeLayers(ctx context.Context, imagesDir string, layerDigests []string, outTarPath string) error {
	if len(layerDigests) == 0 {
		return fmt.Errorf("MergeLayers: no layers")
	}

	outFile, err := os.Create(outTarPath)
	if err != nil {
		return fmt.Errorf("creating output tar: %w", err)
	}
	defer outFile.Close()
	tw := tar.NewWriter(outFile)
	defer tw.Close()

	st := &flattenState{
		emitted:   make(map[string]bool),
		fileWhs:   make(map[string]int),
		opaqueWhs: make(map[string]int),
	}

	// Walk layers in reverse OCI order so the highest layer's entries
	// are emitted first and shadow earlier-layer entries.
	for layerIdx := len(layerDigests) - 1; layerIdx >= 0; layerIdx-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		layerDigest := layerDigests[layerIdx]
		blobPath, err := BlobPath(imagesDir, layerDigest)
		if err != nil {
			return fmt.Errorf("resolving layer %s: %w", layerDigest, err)
		}
		if err := walkLayerForMerge(ctx, blobPath, layerIdx, st, tw); err != nil {
			return fmt.Errorf("merging layer %d (%s): %w", layerIdx, ShortDigest(layerDigest), err)
		}
	}
	return nil
}

type flattenState struct {
	emitted   map[string]bool // canonicalized path -> true if already written to output
	fileWhs   map[string]int  // canonicalized path -> layer index that whitened it
	opaqueWhs map[string]int  // canonicalized dir   -> layer index that opaqued it
}

func walkLayerForMerge(ctx context.Context, blobPath string, layerIdx int, st *flattenState, tw *tar.Writer) error {
	f, err := os.Open(blobPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		// Some registries serve uncompressed tar blobs. Rewind and read raw.
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			return fmt.Errorf("layer not gzip and rewind failed: %w", err)
		}
		return walkLayerTarReader(ctx, f, layerIdx, st, tw)
	}
	defer gz.Close()
	return walkLayerTarReader(ctx, gz, layerIdx, st, tw)
}

func walkLayerTarReader(ctx context.Context, r io.Reader, layerIdx int, st *flattenState, tw *tar.Writer) error {
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar header: %w", err)
		}

		name := canonicalizeTarPath(hdr.Name)
		if name == "" {
			continue
		}
		if path.IsAbs(name) || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
			continue
		}

		dir, base := path.Split(name)
		dir = strings.TrimSuffix(dir, "/")

		// Whiteout markers: never emit. Record so they suppress earlier layers.
		if base == opaqueMarker {
			st.opaqueWhs[dir] = layerIdx
			continue
		}
		if strings.HasPrefix(base, whiteoutPrefix) {
			target := strings.TrimPrefix(base, whiteoutPrefix)
			targetPath := target
			if dir != "" {
				targetPath = path.Join(dir, target)
			}
			st.fileWhs[targetPath] = layerIdx
			continue
		}

		// Shadowed by a later layer's emission, file whiteout, or opaque dir?
		if st.emitted[name] {
			continue
		}
		if isShadowedByFileWhiteout(name, layerIdx, st.fileWhs) {
			continue
		}
		if isShadowedByOpaqueDir(name, layerIdx, st.opaqueWhs) {
			continue
		}

		// Emit with canonicalized name (no leading "./"). Directories keep
		// the trailing slash that the tar format prefers. Let tar.Writer
		// pick the format (USTAR for short paths, PAX only when forced
		// by length); mkfs.erofs's tar parser is happier with USTAR.
		newHdr := *hdr
		newHdr.Name = name
		if hdr.Typeflag == tar.TypeDir {
			newHdr.Name = name + "/"
		}

		if err := tw.WriteHeader(&newHdr); err != nil {
			return fmt.Errorf("writing tar header for %s: %w", name, err)
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			if _, err := io.Copy(tw, tr); err != nil {
				return fmt.Errorf("copying content for %s: %w", name, err)
			}
		}
		st.emitted[name] = true
	}
}

// canonicalizeTarPath strips leading "./" and trailing slashes, returning
// the empty string for paths that should be ignored (".", "./", "").
func canonicalizeTarPath(raw string) string {
	name := strings.TrimPrefix(raw, "./")
	name = strings.TrimSuffix(name, "/")
	if name == "" || name == "." {
		return ""
	}
	return name
}

// isShadowedByFileWhiteout returns true if any ancestor (or the path
// itself) was whitened by a `.wh.foo` marker at a layer above layerIdx.
// A whiteout on "foo" suppresses "foo" and any "foo/bar..." descendant.
func isShadowedByFileWhiteout(name string, layerIdx int, fileWhs map[string]int) bool {
	for cur := name; cur != "" && cur != "."; cur = parentPath(cur) {
		if wl, ok := fileWhs[cur]; ok && wl > layerIdx {
			return true
		}
	}
	return false
}

// isShadowedByOpaqueDir returns true if any strict ancestor of name was
// marked opaque (`.wh..wh..opq`) by a layer above layerIdx. The path
// itself is NOT shadowed by its own opaque marker — only its children.
func isShadowedByOpaqueDir(name string, layerIdx int, opaqueWhs map[string]int) bool {
	for cur := parentPath(name); cur != "" && cur != "."; cur = parentPath(cur) {
		if ol, ok := opaqueWhs[cur]; ok && ol > layerIdx {
			return true
		}
	}
	return false
}

// parentPath returns the directory of name without a trailing slash.
// "foo/bar" → "foo"; "foo" → "."; "" → "".
func parentPath(name string) string {
	if name == "" || name == "." {
		return ""
	}
	d := path.Dir(name)
	return d
}
