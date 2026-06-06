package vmimage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// shedImage is the result of pushing a v0.5.2-shaped image: a layered image
// plus loose kernel/initrd/erofs blobs referenced by manifest annotations.
type shedImage struct {
	ref          string
	erofsDigest  string
	kernelDigest string
	initrdDigest string
}

// pushShedImage pushes a layered image AND the loose kernel/initrd/erofs
// sibling blobs, wired up via the io.shed.* manifest annotations — i.e. a
// boot-only-pullable image. Each loose blob is small random content.
func pushShedImage(t testing.TB, refStr string, layers int) shedImage {
	t.Helper()
	ref, err := name.ParseReference(refStr, name.Insecure)
	if err != nil {
		t.Fatalf("parse ref %q: %v", refStr, err)
	}
	img, err := random.Image(2048, int64(layers))
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	// Push three loose sibling blobs (distinct content → distinct digests).
	loose := func(seed string, n int) string {
		t.Helper()
		layer := static.NewLayer([]byte(strings.Repeat(seed, n)), types.MediaType("application/octet-stream"))
		if err := remote.WriteLayer(ref.Context(), layer); err != nil {
			t.Fatalf("push loose blob %q: %v", seed, err)
		}
		d, err := layer.Digest()
		if err != nil {
			t.Fatalf("loose digest: %v", err)
		}
		return d.String()
	}
	si := shedImage{
		ref:          refStr,
		erofsDigest:  loose("erofs", 4096),
		kernelDigest: loose("kernel", 512),
		initrdDigest: loose("initrd", 256),
	}
	annotated, ok := mutate.Annotations(img, map[string]string{
		AnnotationRootfsErofsDigest: si.erofsDigest,
		AnnotationKernelDigest:      si.kernelDigest,
		AnnotationInitrdDigest:      si.initrdDigest,
	}).(v1.Image)
	if !ok {
		t.Fatal("mutate.Annotations did not return a v1.Image")
	}
	if err := remote.Write(ref, annotated); err != nil {
		t.Fatalf("push annotated image: %v", err)
	}
	return si
}

func bootOnlyOpts(ref, dir string, skipLayers bool) PullOptions {
	return PullOptions{
		Ref:           ref,
		ImagesDir:     dir,
		Insecure:      true,
		ExtractKernel: true,
		NeedsInitrd:   true,
		SkipLayers:    skipLayers,
		Progress:      func(ProgressEvent) {},
	}
}

// TestPullBootOnlySkipsLayers: a boot-only pull writes the erofs/kernel/initrd
// (and manifest/config) but NONE of the layer tarballs.
func TestPullBootOnlySkipsLayers(t *testing.T) {
	host := startTestRegistry(t)
	si := pushShedImage(t, fmt.Sprintf("%s/test/bootonly:v1", host), 4)
	dir := t.TempDir()

	res, err := PullToOCILayout(context.Background(), bootOnlyOpts(si.ref, dir, true))
	if err != nil {
		t.Fatalf("boot-only pull: %v", err)
	}
	for i, d := range res.LayerDigests {
		if BlobExists(dir, d) {
			t.Errorf("layer %d (%s) present after a boot-only pull — should be skipped", i, ShortDigest(d))
		}
	}
	for name, d := range map[string]string{"erofs": si.erofsDigest, "kernel": si.kernelDigest, "initrd": si.initrdDigest} {
		if !BlobExists(dir, d) {
			t.Errorf("%s blob %s missing after boot-only pull", name, ShortDigest(d))
		}
	}
	if !BlobExists(dir, res.ConfigDigest) {
		t.Error("config blob missing after boot-only pull")
	}
}

// TestPullWithLayersHydrates: after a boot-only pull, a --with-layers pull
// (SkipLayers=false) fetches the previously-skipped layer blobs.
func TestPullWithLayersHydrates(t *testing.T) {
	host := startTestRegistry(t)
	si := pushShedImage(t, fmt.Sprintf("%s/test/hydrate:v1", host), 4)
	dir := t.TempDir()

	res, err := PullToOCILayout(context.Background(), bootOnlyOpts(si.ref, dir, true))
	if err != nil {
		t.Fatalf("boot-only pull: %v", err)
	}
	for _, d := range res.LayerDigests {
		if BlobExists(dir, d) {
			t.Fatalf("layer present before hydration")
		}
	}
	// Hydrate.
	if _, err := PullToOCILayout(context.Background(), bootOnlyOpts(si.ref, dir, false)); err != nil {
		t.Fatalf("--with-layers hydration pull: %v", err)
	}
	for i, d := range res.LayerDigests {
		if !BlobExists(dir, d) {
			t.Errorf("layer %d (%s) still missing after --with-layers hydration", i, ShortDigest(d))
		}
	}
}

// TestPullBootOnlyRejectsUnannotated: an image without the erofs annotation
// (pre-v0.5.2 shape) can't be pulled boot-only — it would need layer
// extraction. random.Image has no shed annotations.
func TestPullBootOnlyRejectsUnannotated(t *testing.T) {
	host := startTestRegistry(t)
	ref := pushRandomImage(t, fmt.Sprintf("%s/test/preerofs:v1", host), 2)

	_, err := PullToOCILayout(context.Background(), bootOnlyOpts(ref, t.TempDir(), true))
	if err == nil {
		t.Fatal("expected boot-only pull of an un-annotated image to be rejected")
	}
	if !strings.Contains(err.Error(), "boot-only") || !strings.Contains(err.Error(), "--with-layers") {
		t.Errorf("error %q should mention boot-only and --with-layers", err)
	}
}

// TestPushPreflightMissingLayers: pushing a boot-only image fails the layer
// preflight with the actionable ErrLayersMissing.
func TestPushPreflightMissingLayers(t *testing.T) {
	host := startTestRegistry(t)
	si := pushShedImage(t, fmt.Sprintf("%s/test/push:v1", host), 3)
	dir := t.TempDir()

	res, err := PullToOCILayout(context.Background(), bootOnlyOpts(si.ref, dir, true))
	if err != nil {
		t.Fatalf("boot-only pull: %v", err)
	}
	err = PushFromOCILayout(context.Background(), PushOptions{
		Ref:            fmt.Sprintf("%s/test/push-dest:v1", host),
		ImagesDir:      dir,
		ManifestDigest: res.ManifestDigest,
		Insecure:       true,
		Progress:       func(ProgressEvent) {},
	})
	if !errors.Is(err, ErrLayersMissing) {
		t.Fatalf("push of a boot-only image: got %v, want ErrLayersMissing", err)
	}
	if !strings.Contains(err.Error(), "--with-layers") {
		t.Errorf("push error %q should point at --with-layers", err)
	}
}
