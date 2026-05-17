// shed image save / shed image load support.
//
// SaveImage exports the reachable OCI subtree (manifest + config +
// layers + sibling kernel/initrd) as a single tar stream readable by
// other OCI-aware tools (`crane`, `oras`, `skopeo`). LoadImage ingests
// such a tar into the local OCI store, deduping shared blobs by digest.

package vmimage

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// SaveImage writes the OCI subtree reachable from tagOrDigest as a tar
// stream to out. The stream contains:
//
//	oci-layout
//	index.json          (one entry pointing at the saved manifest)
//	blobs/sha256/<hex>  for manifest, config, every layer, and kernel/initrd
//
// Foreign tools that accept "OCI image layout in a tar" (crane, skopeo,
// oras) can consume the stream directly.
func (m *Manager) SaveImage(tagOrDigest string, out io.Writer) error {
	imagesDir := m.cfg.GetImagesDir()
	digest, tagName, err := m.resolveTagOrDigest(tagOrDigest)
	if err != nil {
		return err
	}
	manifest, err := LoadManifestByDigest(imagesDir, digest)
	if err != nil {
		return err
	}

	// Collect the set of blob digests reachable from the manifest.
	reachable := map[string]bool{
		digest:                true,
		manifest.Config.Digest: true,
	}
	for _, layer := range manifest.Layers {
		reachable[layer.Digest] = true
	}
	if d := manifest.ShedKernelDigest(); d != "" {
		reachable[d] = true
	}
	if d := manifest.ShedInitrdDigest(); d != "" {
		reachable[d] = true
	}

	tw := tar.NewWriter(out)
	defer tw.Close()

	// oci-layout
	marker, _ := json.Marshal(OCILayoutMarker{ImageLayoutVersion: CurrentOCILayoutVersion})
	if err := writeTarFile(tw, "oci-layout", marker, 0o644); err != nil {
		return err
	}

	// index.json — single-manifest index pointing at the saved image.
	idx := &OCIIndex{
		SchemaVersion: 2,
		MediaType:     MediaTypeOCIIndex,
		Manifests: []Descriptor{{
			MediaType: MediaTypeOCIManifest,
			Digest:    digest,
			Size:      BlobSize(imagesDir, digest),
			Annotations: map[string]string{
				"org.opencontainers.image.ref.name": tagName,
			},
		}},
	}
	idxData, err := idx.MarshalIndent()
	if err != nil {
		return fmt.Errorf("marshalling index: %w", err)
	}
	if err := writeTarFile(tw, "index.json", idxData, 0o644); err != nil {
		return err
	}

	// Stream every reachable blob.
	for blobDigest := range reachable {
		data, err := ReadBlob(imagesDir, blobDigest)
		if err != nil {
			return fmt.Errorf("reading blob %s: %w", blobDigest, err)
		}
		hex, _ := digestHex(blobDigest)
		if err := writeTarFile(tw, filepath.Join("blobs", "sha256", hex), data, 0o444); err != nil {
			return err
		}
	}
	return nil
}

// LoadImage reads a tar produced by SaveImage (or any compatible OCI
// image-layout-tar) and ingests it into the local store. Returns the
// list of manifest digests added.
//
// Behavior:
//   - Every blob in the tar is content-verified against its filename
//     digest before being written.
//   - Manifests listed in the input index.json are recorded in the local
//     index. ref-name annotations are translated to local tags (if not
//     already taken).
//   - Layer ext4 files are NOT materialized — call EnsureImage afterwards
//     to populate the cache.
func (m *Manager) LoadImage(in io.Reader) ([]string, error) {
	imagesDir := m.cfg.GetImagesDir()
	if err := EnsureOCILayout(imagesDir); err != nil {
		return nil, err
	}

	tr := tar.NewReader(in)
	pendingBlobs := make(map[string][]byte)
	var indexBytes []byte

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar next: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		buf := new(bytes.Buffer)
		if _, err := io.Copy(buf, tr); err != nil {
			return nil, fmt.Errorf("reading %s: %w", hdr.Name, err)
		}
		name := hdr.Name
		switch {
		case name == "oci-layout":
			// We don't strictly need to inspect — the local layout is
			// already initialized via EnsureOCILayout.
			continue
		case name == "index.json":
			indexBytes = buf.Bytes()
		case strings.HasPrefix(name, "blobs/sha256/"):
			hex := strings.TrimPrefix(name, "blobs/sha256/")
			if hex == "" {
				continue
			}
			digest := DigestPrefix + hex
			if computed := DigestBytes(buf.Bytes()); computed != digest {
				return nil, fmt.Errorf("blob %s content does not match filename digest (got %s)", digest, computed)
			}
			pendingBlobs[digest] = buf.Bytes()
		}
	}

	if indexBytes == nil {
		return nil, errors.New("input tar has no index.json")
	}
	idx, err := ParseIndex(indexBytes)
	if err != nil {
		return nil, err
	}

	// Persist blobs.
	for digest, data := range pendingBlobs {
		if _, err := WriteBlob(imagesDir, digest, data); err != nil {
			return nil, fmt.Errorf("writing blob %s: %w", digest, err)
		}
	}

	// Record manifests in the local index and translate ref-name
	// annotations into local tags (when free).
	var added []string
	for _, manifestDesc := range idx.Manifests {
		added = append(added, manifestDesc.Digest)
		if err := indexUpsert(imagesDir, manifestDesc); err != nil {
			return nil, fmt.Errorf("updating index for %s: %w", manifestDesc.Digest, err)
		}
		if manifestDesc.Annotations == nil {
			continue
		}
		refName := manifestDesc.Annotations["org.opencontainers.image.ref.name"]
		if refName == "" {
			continue
		}
		if err := ValidateImageName(refName); err != nil {
			continue
		}
		if _, err := GetTag(imagesDir, refName); err == nil {
			// Tag already exists — don't clobber.
			continue
		}
		_ = SetTag(imagesDir, refName, manifestDesc.Digest)
	}
	return added, nil
}

// writeTarFile is the common helper for adding a regular file to a tar
// writer with sane defaults.
func writeTarFile(tw *tar.Writer, name string, data []byte, mode int64) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     mode,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header for %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("tar body for %s: %w", name, err)
	}
	return nil
}
