package vmimage

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// startTestRegistry spins up an in-memory OCI registry served over plain
// HTTP on loopback and returns its host:port. Reusable across pull tests
// (cached-layer reporting, byte counting, parallel/dedup) — pulls target it
// with PullOptions{Insecure: true}.
func startTestRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// pushRandomImage builds a random image with the given number of layers and
// pushes it to refStr (e.g. "<host>/test/img:v1") on the test registry,
// returning the full reference string for PullToOCILayout.
func pushRandomImage(t *testing.T, refStr string, layers int) string {
	t.Helper()
	img, err := random.Image(1024, int64(layers))
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	ref, err := name.ParseReference(refStr, name.Insecure)
	if err != nil {
		t.Fatalf("parse ref %q: %v", refStr, err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("push %q: %v", refStr, err)
	}
	return refStr
}

// pullCollecting runs PullToOCILayout against the test registry and returns
// every progress message it emitted.
func pullCollecting(t *testing.T, ref, imagesDir string) []string {
	t.Helper()
	var msgs []string
	_, err := PullToOCILayout(context.Background(), PullOptions{
		Ref:       ref,
		ImagesDir: imagesDir,
		Insecure:  true,
		Progress: func(_, msg string) {
			msgs = append(msgs, msg)
		},
	})
	if err != nil {
		t.Fatalf("PullToOCILayout(%q): %v", ref, err)
	}
	return msgs
}

var layerCounterRe = regexp.MustCompile(`\b(\d+/\d+)\b`)

// layerProgressSeq returns the "i/N" counter tokens, in emission order, from
// the layer-progress messages that contain marker. This asserts the exact
// contiguous 1/N..N/N sequence — the substring being fixed is a *gap* in that
// sequence, so counting occurrences alone would not catch a regression that
// repeated "1/N" instead of advancing.
func layerProgressSeq(msgs []string, marker string) []string {
	var seq []string
	for _, m := range msgs {
		if !strings.Contains(m, marker) {
			continue
		}
		if tok := layerCounterRe.FindString(m); tok != "" {
			seq = append(seq, tok)
		}
	}
	return seq
}

// TestPullToOCILayout_CachedLayerProgress asserts that a re-pull reports every
// already-present layer (no silent skips), so the i+1/N counter never leaves
// gaps that read as "missing layers".
func TestPullToOCILayout_CachedLayerProgress(t *testing.T) {
	host := startTestRegistry(t)
	const layers = 3
	ref := pushRandomImage(t, fmt.Sprintf("%s/test/cached:v1", host), layers)
	dir := t.TempDir()
	want := []string{"1/3", "2/3", "3/3"}

	// First pull populates the blob store: every layer is freshly pulled in
	// a contiguous 1/3..3/3 sequence, and none are reported as cached.
	first := pullCollecting(t, ref, dir)
	if got := layerProgressSeq(first, "Pulling layer"); !slices.Equal(got, want) {
		t.Errorf("first pull: 'Pulling layer' sequence = %v, want %v (msgs: %v)", got, want, first)
	}
	if got := layerProgressSeq(first, "already present"); len(got) != 0 {
		t.Errorf("first pull: got %d 'already present' messages, want 0 (msgs: %v)", len(got), first)
	}

	// Second pull into the same dir: every layer is cached, so each one is
	// reported as already present in the same contiguous 1/3..3/3 sequence
	// (no gaps) and none are re-pulled.
	second := pullCollecting(t, ref, dir)
	if got := layerProgressSeq(second, "already present"); !slices.Equal(got, want) {
		t.Errorf("re-pull: 'already present' sequence = %v, want %v (msgs: %v)", got, want, second)
	}
	if got := layerProgressSeq(second, "Pulling layer"); len(got) != 0 {
		t.Errorf("re-pull: got %d 'Pulling layer' messages, want 0 (msgs: %v)", len(got), second)
	}
}
