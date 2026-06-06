package vmimage

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestBlobThrottle(t *testing.T) {
	now := time.Unix(0, 0)
	th := blobThrottle{interval: 100 * time.Millisecond, now: func() time.Time { return now }}

	if !th.ready() {
		t.Fatal("first ready() must be true (last is zero)")
	}
	if th.ready() {
		t.Fatal("second immediate ready() must be false (no time elapsed)")
	}
	now = now.Add(50 * time.Millisecond)
	if th.ready() {
		t.Fatal("ready() before the interval elapses must be false")
	}
	now = now.Add(60 * time.Millisecond) // 110ms since the last tick
	if !th.ready() {
		t.Fatal("ready() after the interval elapses must be true")
	}
}

func TestProgressReaderByteTicks(t *testing.T) {
	var events []ProgressEvent
	now := time.Unix(0, 0)
	pr := &progressReader{
		r:        bytes.NewReader(make([]byte, 1000)),
		id:       "sha256:abc",
		human:    "Pulling",
		total:    1000,
		progress: func(ev ProgressEvent) { events = append(events, ev) },
		throttle: blobThrottle{interval: 100 * time.Millisecond, now: func() time.Time { return now }},
	}
	buf := make([]byte, 100)
	for {
		now = now.Add(200 * time.Millisecond) // advance past the interval every read
		_, err := pr.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if len(events) == 0 {
		t.Fatal("expected byte-tick events")
	}
	for i, ev := range events {
		if !ev.IsBlob() {
			t.Errorf("event %d is not a blob event: %+v", i, ev)
		}
		if ev.Status != BlobStatusDownloading {
			t.Errorf("event %d status = %q, want downloading", i, ev.Status)
		}
		if ev.Total != 1000 {
			t.Errorf("event %d total = %d, want 1000", i, ev.Total)
		}
	}
	if last := events[len(events)-1]; last.Current != 1000 {
		t.Errorf("last tick current = %d, want 1000", last.Current)
	}
}

func TestProgressReaderThrottleSuppresses(t *testing.T) {
	var events []ProgressEvent
	now := time.Unix(0, 0)
	pr := &progressReader{
		r:        bytes.NewReader(make([]byte, 1000)),
		total:    1000,
		progress: func(ev ProgressEvent) { events = append(events, ev) },
		throttle: blobThrottle{interval: 100 * time.Millisecond, now: func() time.Time { return now }},
	}
	// Clock never advances: only the very first read emits a tick.
	buf := make([]byte, 100)
	for {
		_, err := pr.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if len(events) != 1 {
		t.Errorf("with a frozen clock want exactly 1 tick, got %d", len(events))
	}
}

func TestStreamBlobWithProgressStartAndDone(t *testing.T) {
	var events []ProgressEvent
	var written bytes.Buffer
	err := streamBlobWithProgress("sha256:x", "Pulling x", 500,
		func(ev ProgressEvent) { events = append(events, ev) },
		bytes.NewReader(make([]byte, 500)),
		func(r io.Reader) error { _, e := io.Copy(&written, r); return e },
	)
	if err != nil {
		t.Fatalf("streamBlobWithProgress: %v", err)
	}
	if written.Len() != 500 {
		t.Fatalf("wrote %d bytes, want 500", written.Len())
	}
	if len(events) < 2 {
		t.Fatalf("want at least a start and a done event, got %d", len(events))
	}
	if first := events[0]; first.Status != BlobStatusDownloading || first.Current != 0 || first.Total != 500 || first.ID != "sha256:x" {
		t.Errorf("first event = %+v, want downloading 0/500 id sha256:x", first)
	}
	if last := events[len(events)-1]; last.Status != BlobStatusDone || last.Current != 500 || last.Total != 500 {
		t.Errorf("last event = %+v, want done 500/500", last)
	}
}

// TestPullToOCILayout_BlobByteProgress asserts the live pull emits structured
// per-blob events keyed by full digest, each opening with a 0-byte
// downloading start and closing with a done event at Current==Total.
func TestPullToOCILayout_BlobByteProgress(t *testing.T) {
	host := startTestRegistry(t)
	ref := pushRandomImage(t, fmt.Sprintf("%s/test/bytes:v1", host), 2)

	var blobEvents []ProgressEvent
	for _, ev := range pullCollectingEvents(t, ref, t.TempDir()) {
		if ev.IsBlob() {
			blobEvents = append(blobEvents, ev)
		}
	}
	if len(blobEvents) == 0 {
		t.Fatal("no blob events emitted")
	}

	byID := map[string][]ProgressEvent{}
	for _, ev := range blobEvents {
		if !strings.HasPrefix(ev.ID, "sha256:") {
			t.Errorf("blob event ID is not a full digest: %q", ev.ID)
		}
		byID[ev.ID] = append(byID[ev.ID], ev)
	}
	for id, evs := range byID {
		first, last := evs[0], evs[len(evs)-1]
		if first.Status != BlobStatusDownloading || first.Current != 0 {
			t.Errorf("%s: first event = %+v, want downloading at 0 bytes", ShortDigest(id), first)
		}
		if last.Status != BlobStatusDone || last.Total <= 0 || last.Current != last.Total {
			t.Errorf("%s: last event = %+v, want done with Current==Total>0", ShortDigest(id), last)
		}
	}
}
