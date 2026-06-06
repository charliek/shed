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

func TestEmitProgress(t *testing.T) {
	cases := []struct {
		name  string
		event ProgressEvent
		check func(t *testing.T, got []ProgressEvent)
	}{
		{
			name:  "plain event advances the phase then emits a status line",
			event: ProgressEvent{Phase: "image", Message: "Pulling layer 1/2"},
			check: func(t *testing.T, got []ProgressEvent) {
				if len(got) != 2 {
					t.Fatalf("got %d events, want 2 (phase + status): %+v", len(got), got)
				}
				if got[0].Phase != "image" || got[0].Message != "" {
					t.Errorf("event 0 = %+v, want phase-only {image}", got[0])
				}
				if got[1].Message != "Pulling layer 1/2" || got[1].Phase != "" || got[1].Warning {
					t.Errorf("event 1 = %+v, want a plain status line", got[1])
				}
			},
		},
		{
			// EmitProgress must not strip Warning when routing a plain event.
			name:  "warning is preserved on plain events",
			event: ProgressEvent{Message: "heads up", Warning: true},
			check: func(t *testing.T, got []ProgressEvent) {
				if len(got) != 1 || !got[0].Warning || got[0].Message != "heads up" {
					t.Fatalf("got %+v, want a single warning status line", got)
				}
			},
		},
		{
			// PhaseTimer-safety guard: a blob event must emit a single
			// structured event with Phase forced empty, so parallel download
			// goroutines (PR 4) can't corrupt phase accounting.
			name: "blob event never advances the phase",
			event: ProgressEvent{
				Phase: "image", // must be discarded
				Kind:  KindBlob, Message: "blob", ID: "sha256:x",
				Status: "downloading", Current: 5, Total: 10,
			},
			check: func(t *testing.T, got []ProgressEvent) {
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
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collectProgress(t, func(ctx context.Context) { EmitProgress(ctx, tc.event) })
			tc.check(t, got)
		})
	}
}
