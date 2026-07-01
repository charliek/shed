package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charliek/shed/internal/backend"
)

// TestDeleteShedWithProgress covers the three response shapes the delete client
// must handle (#232): a streamed SSE delete, the plain-204 fallback for a server
// that predates delete-SSE, and a terminal SSE error event.
func TestDeleteShedWithProgress(t *testing.T) {
	t.Run("streams SSE progress then completes", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Accept"); got != "text/event-stream" {
				t.Errorf("Accept = %q, want text/event-stream", got)
			}
			if r.Method != http.MethodDelete {
				t.Errorf("method = %q, want DELETE", r.Method)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "event: progress\ndata: {\"message\":\"Terminating virtual machine...\"}\n\n")
			fmt.Fprint(w, "event: complete\ndata: {\"name\":\"myshed\"}\n\n")
		}))
		defer srv.Close()

		c := newAPIClient(srv.URL, "", "", DefaultTimeout)
		var msgs []string
		if err := c.DeleteShedWithProgress("myshed", func(e backend.ProgressEvent) {
			msgs = append(msgs, e.Message)
		}); err != nil {
			t.Fatalf("DeleteShedWithProgress: %v", err)
		}
		if len(msgs) != 1 || msgs[0] != "Terminating virtual machine..." {
			t.Errorf("progress messages = %v, want one 'Terminating virtual machine...'", msgs)
		}
	})

	t.Run("falls back to 204 without reading a stream (old server)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// Old server ignores Accept and returns a plain 204 with no body.
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		c := newAPIClient(srv.URL, "", "", DefaultTimeout)
		called := false
		if err := c.DeleteShedWithProgress("myshed", func(backend.ProgressEvent) { called = true }); err != nil {
			t.Fatalf("expected 204 to be treated as success, got: %v", err)
		}
		if called {
			t.Error("onProgress must not be called on a 204 fallback")
		}
	})

	t.Run("surfaces a terminal SSE error event", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "event: error\ndata: {\"error\":{\"code\":\"SHED_NOT_FOUND\",\"message\":\"shed not found\"}}\n\n")
		}))
		defer srv.Close()

		c := newAPIClient(srv.URL, "", "", DefaultTimeout)
		if err := c.DeleteShedWithProgress("ghost", func(backend.ProgressEvent) {}); err == nil {
			t.Fatal("expected an error from a terminal SSE error event")
		}
	})

	t.Run("treats a non-SSE 200 as success (old/proxied plain delete)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// 200 without a text/event-stream content type — must not be fed to
			// the SSE reader (which would error on EOF); the delete succeeded.
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := newAPIClient(srv.URL, "", "", DefaultTimeout)
		if err := c.DeleteShedWithProgress("myshed", func(backend.ProgressEvent) {}); err != nil {
			t.Fatalf("expected non-SSE 200 to be treated as success, got: %v", err)
		}
	})
}
