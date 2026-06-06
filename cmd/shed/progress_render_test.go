package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/vmimage"
)

func TestRenderBar(t *testing.T) {
	cases := []struct {
		name           string
		current, total int64
		width          int
		want           string
	}{
		{"empty", 0, 100, 10, "[----------]"},
		{"half", 50, 100, 10, "[#####-----]"},
		{"full", 100, 100, 10, "[##########]"},
		{"over clamps to full", 150, 100, 10, "[##########]"},
		{"negative clamps to empty", -5, 100, 10, "[----------]"},
		{"unknown total is indeterminate", 5, 0, 10, "[----------]"},
		{"zero width floors to one cell", 5, 100, 0, "[-]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderBar(tc.current, tc.total, tc.width); got != tc.want {
				t.Errorf("renderBar(%d,%d,%d) = %q, want %q", tc.current, tc.total, tc.width, got, tc.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		s     string
		width int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 3, "he…"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.s+"/"+tc.want, func(t *testing.T) {
			if got := truncate(tc.s, tc.width); got != tc.want {
				t.Errorf("truncate(%q,%d) = %q, want %q", tc.s, tc.width, got, tc.want)
			}
		})
	}
}

// blob builds a per-blob progress event for the renderer scripts.
func blob(id, msg, status string, cur, total int64) backend.ProgressEvent {
	return backend.ProgressEvent{Kind: backend.KindBlob, ID: id, Message: msg, Status: status, Current: cur, Total: total}
}

func TestLiveRenderer(t *testing.T) {
	cases := []struct {
		name       string
		cols, rows int
		events     []backend.ProgressEvent
		wantSubstr []string
		notSubstr  []string
	}{
		{
			name: "header shows, then per-layer plain lines suppressed once bars start",
			cols: 200, rows: 50,
			events: []backend.ProgressEvent{
				{Message: "Fetching manifest ghcr.io/x:v1..."}, // header (pre-blob)
				blob("sha256:aaa", "layer 1/1 sha256:aaa", vmimage.BlobStatusDownloading, 0, 100),
				blob("sha256:aaa", "layer 1/1 sha256:aaa", vmimage.BlobStatusDownloading, 50, 100),
				blob("sha256:aaa", "layer 1/1 sha256:aaa", vmimage.BlobStatusDone, 100, 100),
				{Message: "SHOULD_NOT_APPEAR_after_bars"}, // plain, post-blob → suppressed
			},
			wantSubstr: []string{"Fetching manifest", "#", "✓"},
			notSubstr:  []string{"SHOULD_NOT_APPEAR_after_bars"},
		},
		{
			// Old server that ignores ?progress=blob: only plain lines arrive,
			// so the renderer must NOT suppress them (no blob ever starts).
			name: "no blob events: all plain lines shown (old-server fallback)",
			cols: 200, rows: 50,
			events: []backend.ProgressEvent{
				{Message: "Fetching manifest ghcr.io/x:v1..."},
				{Message: "Pulling layer 1/2 sha256:aaa"},
				{Message: "Pulling layer 2/2 sha256:bbb"},
			},
			wantSubstr: []string{"Fetching manifest", "Pulling layer 1/2", "Pulling layer 2/2"},
		},
		{
			name: "all-cached pull settles with cached markers",
			cols: 200, rows: 50,
			events: []backend.ProgressEvent{
				blob("sha256:aaa", "layer 1/2 sha256:aaa", vmimage.BlobStatusExists, 10, 10),
				blob("sha256:bbb", "layer 2/2 sha256:bbb", vmimage.BlobStatusExists, 20, 20),
			},
			wantSubstr: []string{"✓", "(cached)", "layer 1/2", "layer 2/2"},
		},
		{
			name: "narrow terminal does not panic and clamps content",
			cols: 12, rows: 50,
			events: []backend.ProgressEvent{
				blob("sha256:ccc", "layer 1/1 sha256:cccccccccccc", vmimage.BlobStatusDownloading, 5, 100),
			},
		},
		{
			// More blobs than rows: block must stay bounded with a "… N more"
			// line so erase/redraw never exceeds the viewport.
			name: "short terminal caps the block height",
			cols: 200, rows: 4,
			events: []backend.ProgressEvent{
				blob("sha256:a", "layer 1/6 sha256:a", vmimage.BlobStatusExists, 1, 1),
				blob("sha256:b", "layer 2/6 sha256:b", vmimage.BlobStatusExists, 1, 1),
				blob("sha256:c", "layer 3/6 sha256:c", vmimage.BlobStatusExists, 1, 1),
				blob("sha256:d", "layer 4/6 sha256:d", vmimage.BlobStatusExists, 1, 1),
				blob("sha256:e", "layer 5/6 sha256:e", vmimage.BlobStatusExists, 1, 1),
				blob("sha256:f", "layer 6/6 sha256:f", vmimage.BlobStatusExists, 1, 1),
			},
			wantSubstr: []string{"more"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := newLiveRenderer(&buf, func() (int, int) { return tc.cols, tc.rows })
			for _, ev := range tc.events {
				r.handle(ev)
			}
			r.finish()
			out := buf.String()
			for _, want := range tc.wantSubstr {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q\n---\n%s", want, out)
				}
			}
			for _, no := range tc.notSubstr {
				if strings.Contains(out, no) {
					t.Errorf("output unexpectedly contains %q\n---\n%s", no, out)
				}
			}
			// The final block must never claim more rows than the viewport.
			if r.drawn > tc.rows {
				t.Errorf("drawn=%d exceeds rows=%d (would corrupt scrollback)", r.drawn, tc.rows)
			}
		})
	}
}
