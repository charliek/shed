package backend

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPhaseTimerBreakdown(t *testing.T) {
	base := time.Unix(0, 0)
	cur := base
	tm := NewPhaseTimer("create name=x backend=vz", func() time.Time { return cur })

	cur = base.Add(10 * time.Millisecond)
	tm.Track(ProgressEvent{Phase: "image"})

	cur = base.Add(110 * time.Millisecond)
	tm.Track(ProgressEvent{Phase: "rootfs"})

	// A second "rootfs" event must not split the span.
	cur = base.Add(130 * time.Millisecond)
	tm.Track(ProgressEvent{Phase: "rootfs"})

	cur = base.Add(150 * time.Millisecond)
	tm.Track(ProgressEvent{Phase: "vm"})

	cur = base.Add(200 * time.Millisecond)
	got := tm.Finish(nil)

	for _, want := range []string{
		"create name=x backend=vz",
		"total=200ms", "setup=10ms", "image=100ms", "rootfs=40ms", "vm=50ms", "err=<nil>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
	if n := strings.Count(got, "rootfs="); n != 1 {
		t.Errorf("rootfs span was split (%d occurrences): %q", n, got)
	}
}

func TestPhaseTimerError(t *testing.T) {
	base := time.Unix(0, 0)
	cur := base
	tm := NewPhaseTimer("op", func() time.Time { return cur })
	cur = base.Add(5 * time.Millisecond)
	got := tm.Finish(errors.New("boom"))
	if !strings.Contains(got, `err="boom"`) {
		t.Errorf("missing error annotation: %q", got)
	}
}

func TestPhaseTimerGuestSpans(t *testing.T) {
	tm := NewPhaseTimer("op", func() time.Time { return time.Unix(0, 0) })
	tm.AddGuestSpans(PhaseSpan{"mkfs", 1800 * time.Millisecond})
	got := tm.Finish(nil)
	if !strings.Contains(got, "guest:[mkfs=1800ms]") {
		t.Errorf("missing guest spans: %q", got)
	}
}

func TestTeeProgress(t *testing.T) {
	var a, b int
	fn := TeeProgress(func(ProgressEvent) { a++ }, nil, func(ProgressEvent) { b++ })
	fn(ProgressEvent{Phase: "x"})
	if a != 1 || b != 1 {
		t.Fatalf("tee did not fan out to both funcs: a=%d b=%d", a, b)
	}
	if TeeProgress(nil, nil) != nil {
		t.Error("TeeProgress with only nil funcs should return nil")
	}
}
