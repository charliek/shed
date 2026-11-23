package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunRefusesBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no server", nil},
		{"cert without key", []string{"--server", "http://127.0.0.1:1", "--cert", "x.pem"}},
		{"answer cert without key", []string{"--server", "http://127.0.0.1:1", "--answer-cert", "x.pem"}},
		{"bad machine id", []string{"--server", "http://127.0.0.1:1", "--machine-id", "nope"}},
		{"stray argument", []string{"--server", "http://127.0.0.1:1", "extra"}},
		{"missing items file", []string{"--server", "http://127.0.0.1:1", "--items", "/nonexistent/items.json"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if err := run(context.Background(), c.args, &out, &errb); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// TestRunOnce answers one request from a canned file over a stream that
// then ends, and prints one JSON line per event.
func TestRunOnce(t *testing.T) {
	const reqID = "0123456789abcdef0123456789abcdef"
	const stream = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
	answered := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			answered <- b
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Shed-Delivery-Stream", stream)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `event: request`+"\n"+`data: {"version":1,"id":"`+reqID+`","reason":"create","deadline":"2026-11-23T10:00:30Z","user":"ada","shed":{"name":"w","id":"abc","backend":"vz"}}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-answered:
			// the answer arrived: end the stream
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	items := filepath.Join(t.TempDir(), "items.json")
	if err := os.WriteFile(items, []byte(`{"items":[],"problems":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx, []string{"--server", srv.URL, "--items", items, "--once"}, &out, &errb); err != nil {
		t.Fatalf("%v: %s", err, errb.String())
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var ev struct {
			Event  string `json:"event"`
			Status int    `json:"status"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		kinds = append(kinds, ev.Event)
		if ev.Event == "answer" && ev.Status != http.StatusNoContent {
			t.Errorf("answer status %d", ev.Status)
		}
	}
	if got := strings.Join(kinds, ","); got != "stream,request,answer,end" {
		t.Errorf("events %s", got)
	}
}
