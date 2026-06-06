package api

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
)

// drain collects everything buffered in ch after it is closed.
func drain(ch chan backend.ProgressEvent) []backend.ProgressEvent {
	close(ch)
	var got []backend.ProgressEvent
	for ev := range ch {
		got = append(got, ev)
	}
	return got
}

// TestProgressSinkGatesBlobWithoutOptIn: a client that did not request
// ?progress=blob gets the plain line and never the byte-tick events, and
// phase-only (Message-less) events are dropped. This is the compat guard —
// older/line-mode clients see exactly today's output.
func TestProgressSinkGatesBlobWithoutOptIn(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/images/pull", nil)
	ch := make(chan backend.ProgressEvent, 8)
	sink := newProgressSink(r, ch)

	sink(backend.ProgressEvent{Message: "Pulling layer 1/2"})
	sink(backend.ProgressEvent{Kind: backend.KindBlob, Message: "blob", Status: "downloading", Current: 1, Total: 2})
	sink(backend.ProgressEvent{Phase: "image"}) // Message == "" → dropped

	got := drain(ch)
	if len(got) != 1 || got[0].Message != "Pulling layer 1/2" {
		t.Fatalf("without opt-in got %+v, want only the plain line", got)
	}
}

// TestProgressSinkForwardsBlobWithOptIn: a client that requested
// ?progress=blob receives the structured byte events verbatim.
func TestProgressSinkForwardsBlobWithOptIn(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/images/pull?progress=blob", nil)
	ch := make(chan backend.ProgressEvent, 8)
	sink := newProgressSink(r, ch)

	sink(backend.ProgressEvent{Kind: backend.KindBlob, Message: "blob", Status: "downloading", Current: 1, Total: 2})

	got := drain(ch)
	if len(got) != 1 || got[0].Kind != backend.KindBlob || got[0].Current != 1 || got[0].Total != 2 {
		t.Fatalf("with opt-in got %+v, want the blob event", got)
	}
}

// TestProgressSinkSendUnblocksOnCancel: the send is non-dropping (blocks
// rather than silently discarding a status transition), but a cancelled
// request must release it instead of deadlocking the pull goroutine.
func TestProgressSinkSendUnblocksOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/api/images/pull", nil).WithContext(ctx)
	ch := make(chan backend.ProgressEvent) // unbuffered, no reader
	sink := newProgressSink(r, ch)

	cancel() // client disconnected
	done := make(chan struct{})
	go func() {
		sink(backend.ProgressEvent{Message: "x"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sink blocked forever on a cancelled request (should unblock via ctx.Done)")
	}
}
