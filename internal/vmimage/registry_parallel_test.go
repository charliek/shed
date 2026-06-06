package vmimage

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
)

// TestPullToOCILayout_Parallel pulls a multi-layer image with concurrency > 1
// and asserts every blob lands. Run under `-race` it also guards the parallel
// progress emission and layerDigests indexing against data races.
func TestPullToOCILayout_Parallel(t *testing.T) {
	host := startTestRegistry(t)
	const layers = 6
	ref := pushRandomImage(t, fmt.Sprintf("%s/test/parallel:v1", host), layers)
	dir := t.TempDir()

	// A deliberately non-thread-safe sink (unguarded slice append): under
	// -race this proves PullToOCILayout serializes the callback across its
	// parallel workers.
	var events []ProgressEvent
	res, err := PullToOCILayout(context.Background(), PullOptions{
		Ref:         ref,
		ImagesDir:   dir,
		Insecure:    true,
		Concurrency: 4,
		Progress:    func(ev ProgressEvent) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatalf("parallel pull: %v", err)
	}
	if len(events) == 0 {
		t.Error("expected progress events from the parallel pull")
	}
	if len(res.LayerDigests) != layers {
		t.Fatalf("got %d layer digests, want %d", len(res.LayerDigests), layers)
	}
	for i, d := range res.LayerDigests {
		if !BlobExists(dir, d) {
			t.Errorf("layer %d blob %s missing after parallel pull", i, d)
		}
	}
	if !BlobExists(dir, res.ConfigDigest) {
		t.Error("config blob missing after parallel pull")
	}
}

// countingRegistry wraps the in-memory registry and tracks the peak number of
// concurrent blob GETs so a test can assert the concurrency cap is honored.
func countingRegistry(t testing.TB, hold time.Duration) (handler http.Handler, peak *int32) {
	base := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var inFlight, max int32
	peak = &max
	handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				m := atomic.LoadInt32(&max)
				if cur <= m || atomic.CompareAndSwapInt32(&max, m, cur) {
					break
				}
			}
			time.Sleep(hold) // widen the window so concurrency is observable
			atomic.AddInt32(&inFlight, -1)
		}
		base.ServeHTTP(w, r)
	})
	return handler, peak
}

func TestPullConcurrencyLimit(t *testing.T) {
	const limit = 2
	handler, peak := countingRegistry(t, 40*time.Millisecond)
	host := startTestRegistryHandler(t, handler)
	ref := pushRandomImage(t, fmt.Sprintf("%s/test/limit:v1", host), 6)

	_, err := PullToOCILayout(context.Background(), PullOptions{
		Ref:         ref,
		ImagesDir:   t.TempDir(),
		Insecure:    true,
		Concurrency: limit,
		Progress:    func(ProgressEvent) {},
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got := atomic.LoadInt32(peak); got > limit {
		t.Errorf("peak concurrent blob GETs = %d, want <= %d", got, limit)
	}
	if got := atomic.LoadInt32(peak); got < 2 {
		t.Errorf("peak concurrent blob GETs = %d, expected the pull to actually parallelize", got)
	}
}

// TestPullConcurrencyOne reproduces serial behavior with Concurrency == 1.
func TestPullConcurrencyOne(t *testing.T) {
	handler, peak := countingRegistry(t, 15*time.Millisecond)
	host := startTestRegistryHandler(t, handler)
	ref := pushRandomImage(t, fmt.Sprintf("%s/test/serial:v1", host), 4)

	if _, err := PullToOCILayout(context.Background(), PullOptions{
		Ref: ref, ImagesDir: t.TempDir(), Insecure: true, Concurrency: 1,
		Progress: func(ProgressEvent) {},
	}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got := atomic.LoadInt32(peak); got != 1 {
		t.Errorf("Concurrency=1 peak concurrent blob GETs = %d, want exactly 1", got)
	}
}

func BenchmarkPullToOCILayout(b *testing.B) {
	host := startTestRegistry(b)
	ref := pushRandomImage(b, fmt.Sprintf("%s/test/bench:v1", host), 6)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		dir := b.TempDir()
		b.StartTimer()
		if _, err := PullToOCILayout(context.Background(), PullOptions{
			Ref: ref, ImagesDir: dir, Insecure: true, Concurrency: 4,
			Progress: func(ProgressEvent) {},
		}); err != nil {
			b.Fatalf("pull: %v", err)
		}
	}
}
