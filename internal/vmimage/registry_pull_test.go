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
// the plain (line-mode) progress messages it emitted. Structured blob events
// are excluded so these assertions track exactly what a line-mode client
// renders; the byte-progress events are exercised by pullCollectingEvents.
func pullCollecting(t *testing.T, ref, imagesDir string) []string {
	t.Helper()
	var msgs []string
	for _, ev := range pullCollectingEvents(t, ref, imagesDir) {
		if ev.IsBlob() {
			continue
		}
		msgs = append(msgs, ev.Message)
	}
	return msgs
}

// pullCollectingEvents runs PullToOCILayout and returns every progress event.
func pullCollectingEvents(t *testing.T, ref, imagesDir string) []ProgressEvent {
	t.Helper()
	var events []ProgressEvent
	_, err := PullToOCILayout(context.Background(), PullOptions{
		Ref:       ref,
		ImagesDir: imagesDir,
		Insecure:  true,
		Progress: func(ev ProgressEvent) {
			events = append(events, ev)
		},
	})
	if err != nil {
		t.Fatalf("PullToOCILayout(%q): %v", ref, err)
	}
	return events
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
//
// The cases run in order against the SAME store: the fresh pull populates the
// cache that the re-pull then reports. Each case asserts the layer counter is
// the exact contiguous 1/N..N/N sequence (the bug being fixed is a *gap* in
// that sequence) and that the opposite marker never appears.
func TestPullToOCILayout_CachedLayerProgress(t *testing.T) {
	host := startTestRegistry(t)
	const layers = 3
	ref := pushRandomImage(t, fmt.Sprintf("%s/test/cached:v1", host), layers)
	dir := t.TempDir()
	want := []string{"1/3", "2/3", "3/3"}

	cases := []struct {
		name         string
		wantMarker   string // marker whose lines must form the 1/N..N/N sequence
		absentMarker string // marker that must not appear
	}{
		{"fresh pull pulls every layer", "Pulling layer", "already present"},
		{"re-pull reports every cached layer", "already present", "Pulling layer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := pullCollecting(t, ref, dir)
			if got := layerProgressSeq(msgs, tc.wantMarker); !slices.Equal(got, want) {
				t.Errorf("%q sequence = %v, want %v (msgs: %v)", tc.wantMarker, got, want, msgs)
			}
			if got := layerProgressSeq(msgs, tc.absentMarker); len(got) != 0 {
				t.Errorf("got %d %q messages, want 0 (msgs: %v)", len(got), tc.absentMarker, msgs)
			}
		})
	}
}
