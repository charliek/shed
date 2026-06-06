package backend

import (
	"context"
	"testing"
)

func collectProgress(t *testing.T, fn func(ctx context.Context)) []ProgressEvent {
	t.Helper()
	var got []ProgressEvent
	ctx := ContextWithProgress(context.Background(), func(ev ProgressEvent) { got = append(got, ev) })
	fn(ctx)
	return got
}

// TestEmitProgressPlain keeps the historical two-event behavior for plain
// events: a phase advance followed by a status line.
func TestEmitProgressPlain(t *testing.T) {
	got := collectProgress(t, func(ctx context.Context) {
		EmitProgress(ctx, ProgressEvent{Phase: "image", Message: "Pulling layer 1/2"})
	})
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (phase + status): %+v", len(got), got)
	}
	if got[0].Phase != "image" || got[0].Message != "" {
		t.Errorf("event 0 = %+v, want phase-only {image}", got[0])
	}
	if got[1].Message != "Pulling layer 1/2" || got[1].Phase != "" {
		t.Errorf("event 1 = %+v, want status-only line", got[1])
	}
}

// TestEmitProgressBlobNeverAdvancesPhase is the PhaseTimer-safety guard: a
// blob event must emit a single structured event with Phase forced empty, so
// parallel download goroutines (PR 4) can't corrupt phase accounting.
func TestEmitProgressBlobNeverAdvancesPhase(t *testing.T) {
	got := collectProgress(t, func(ctx context.Context) {
		EmitProgress(ctx, ProgressEvent{
			Phase: "image", // must be discarded
			Kind:  KindBlob, Message: "blob", ID: "sha256:x",
			Status: "downloading", Current: 5, Total: 10,
		})
	})
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 (no phase advance for blob): %+v", len(got), got)
	}
	ev := got[0]
	if ev.Phase != "" {
		t.Errorf("blob event advanced the phase: %+v", ev)
	}
	if ev.Kind != KindBlob || ev.ID != "sha256:x" || ev.Current != 5 || ev.Total != 10 {
		t.Errorf("blob event fields not preserved: %+v", ev)
	}
}
