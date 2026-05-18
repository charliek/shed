// `shed image history` support.
//
// Walks an OCI manifest's layer list, attributes each layer back to
// (a) the originating shed variant when the layer was produced by us,
// (b) the source registry ref when it was pulled from outside, or
// (c) "unknown" when there's no annotation/history.

package vmimage

import (
	"fmt"
	"time"
)

// LayerInfo describes one layer in an image's history.
type LayerInfo struct {
	Index       int               `json:"index"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	MediaType   string            `json:"media_type"`
	CreatedAt   time.Time         `json:"created_at,omitempty"`
	CreatedBy   string            `json:"created_by,omitempty"`
	Comment     string            `json:"comment,omitempty"`
	Variant     string            `json:"variant,omitempty"`
	EmptyLayer  bool              `json:"empty_layer,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ImageHistory loads the manifest + config for tagOrDigest and returns
// per-layer history in display order (layer[0] = base, layer[N-1] = top).
func (m *Manager) ImageHistory(tagOrDigest string) ([]LayerInfo, error) {
	imagesDir := m.cfg.GetImagesDir()
	digest, _, err := m.resolveTagOrDigest(tagOrDigest)
	if err != nil {
		return nil, err
	}
	manifest, err := LoadManifestByDigest(imagesDir, digest)
	if err != nil {
		return nil, err
	}
	cfg, err := LoadConfigByDigest(imagesDir, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("loading config blob: %w", err)
	}
	variant := manifest.ShedVariant()

	out := make([]LayerInfo, 0, len(manifest.Layers))
	historyIdx := 0
	for i, layer := range manifest.Layers {
		info := LayerInfo{
			Index:       i,
			Digest:      layer.Digest,
			Size:        layer.Size,
			MediaType:   layer.MediaType,
			Variant:     variant,
			Annotations: layer.Annotations,
		}
		// OCI image config history entries are 1:1 with layers, skipping
		// empty_layer entries. Walk in parallel.
		for historyIdx < len(cfg.History) && cfg.History[historyIdx].EmptyLayer {
			historyIdx++
		}
		if historyIdx < len(cfg.History) {
			h := cfg.History[historyIdx]
			info.CreatedBy = h.CreatedBy
			info.Comment = h.Comment
			info.EmptyLayer = h.EmptyLayer
			if t, err := time.Parse(time.RFC3339Nano, h.Created); err == nil {
				info.CreatedAt = t
			}
			historyIdx++
		}
		out = append(out, info)
	}
	return out, nil
}
