package api

import (
	"context"
	"net/http/httptest"
	"reflect"
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

// TestProgressSinkGating covers the ?progress=blob opt-in: a client that did
// not opt in gets the plain line and never byte-tick events (the compat
// guard), and phase-only (Message-less) events are always dropped; an
// opted-in client receives the structured events verbatim.
func TestProgressSinkGating(t *testing.T) {
	blobEvent := backend.ProgressEvent{Kind: backend.KindBlob, Message: "blob", Status: "downloading", Current: 1, Total: 2}
	cases := []struct {
		name string
		url  string
		send []backend.ProgressEvent
		want []backend.ProgressEvent
	}{
		{
			name: "without opt-in: blob dropped, plain kept, phase-only dropped",
			url:  "/api/images/pull",
			send: []backend.ProgressEvent{
				{Message: "Pulling layer 1/2"},
				blobEvent,
				{Phase: "image"}, // Message == "" → dropped
			},
			want: []backend.ProgressEvent{{Message: "Pulling layer 1/2"}},
		},
		{
			name: "with opt-in: blob forwarded verbatim",
			url:  "/api/images/pull?progress=blob",
			send: []backend.ProgressEvent{blobEvent},
			want: []backend.ProgressEvent{blobEvent},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", tc.url, nil)
			ch := make(chan backend.ProgressEvent, 8)
			sink := newProgressSink(r, ch)
			for _, ev := range tc.send {
				sink(ev)
			}
			if got := drain(ch); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestProgressSinkSendUnblocksOnCancel exercises a distinct concurrency
// invariant (not a data case): the send is non-dropping — it blocks rather
// than silently discarding a status transition — but a cancelled request must
// release it instead of deadlocking the pull goroutine.
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
