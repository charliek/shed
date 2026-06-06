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
	cases := []struct {
		name           string
		advancePerRead time.Duration
		wantTicks      func(t *testing.T, events []ProgressEvent)
	}{
		{
			name:           "advancing clock emits a tick per read up to the full byte count",
			advancePerRead: 200 * time.Millisecond, // past the 100ms interval every read
			wantTicks: func(t *testing.T, events []ProgressEvent) {
				if len(events) == 0 {
					t.Fatal("expected byte-tick events")
				}
				if last := events[len(events)-1]; last.Current != 1000 {
					t.Errorf("last tick current = %d, want 1000", last.Current)
				}
			},
		},
		{
			name:           "frozen clock emits only the first tick",
			advancePerRead: 0,
			wantTicks: func(t *testing.T, events []ProgressEvent) {
				if len(events) != 1 {
					t.Errorf("with a frozen clock want exactly 1 tick, got %d", len(events))
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
				now = now.Add(tc.advancePerRead)
				_, err := pr.Read(buf)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("read: %v", err)
				}
			}
			for i, ev := range events {
				if !ev.IsBlob() || ev.Status != BlobStatusDownloading || ev.Total != 1000 {
					t.Errorf("event %d = %+v, want a downloading blob tick with total 1000", i, ev)
				}
			}
			tc.wantTicks(t, events)
		})
	}
}

func TestStreamBlobWithProgressEmitsDone(t *testing.T) {
	var events []ProgressEvent
	var written bytes.Buffer
	err := streamBlobWithProgress("sha256:x", "x", 500,
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
	if len(events) == 0 {
		t.Fatal("want at least a done event")
	}
	// The caller emits the 0-byte start; this function emits ticks + done.
	for i, ev := range events {
		if !ev.IsBlob() || ev.ID != "sha256:x" || ev.Total != 500 {
			t.Errorf("event %d = %+v, want a blob event for sha256:x total 500", i, ev)
		}
	}
	if last := events[len(events)-1]; last.Status != BlobStatusDone || last.Current != 500 {
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
