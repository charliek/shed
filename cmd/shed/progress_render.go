package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/vmimage"
	"golang.org/x/term"
)

// isProgressTTY reports whether f is an interactive terminal we can draw a
// live progress block on.
func isProgressTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// terminalSize returns the (cols, rows) of f, falling back to 80x24 for any
// dimension it can't read. Re-read per frame so the renderer follows a resize.
func terminalSize(f *os.File) (cols, rows int) {
	cols, rows = 80, 24
	if w, h, err := term.GetSize(int(f.Fd())); err == nil {
		if w > 0 {
			cols = w
		}
		if h > 0 {
			rows = h
		}
	}
	return cols, rows
}

// renderBar renders a fixed-width progress bar for current/total. width is the
// number of cells between the brackets. A non-positive total renders an
// indeterminate (all-dash) bar; current is clamped to [0, total].
func renderBar(current, total int64, width int) string {
	if width < 1 {
		width = 1
	}
	if total <= 0 {
		return "[" + strings.Repeat("-", width) + "]"
	}
	if current < 0 {
		current = 0
	}
	if current > total {
		current = total
	}
	filled := int(float64(width) * float64(current) / float64(total))
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

// truncate clamps s to at most width runes, appending an ellipsis when cut.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// blobState is the latest known state of one downloading/cached blob.
type blobState struct {
	human   string
	status  string
	current int64
	total   int64
}

// liveRenderer draws a Docker-style block of per-blob progress bars that
// redraw in place. It is fed backend.ProgressEvent values and owns a
// contiguous block of lines at the bottom of the terminal.
type liveRenderer struct {
	w       io.Writer
	size    func() (cols, rows int) // re-read per frame so a resize is honored
	order   []string                // blob IDs (full digests) in first-seen order
	blobs   map[string]*blobState
	drawn   int  // lines drawn in the last block (for in-place redraw)
	sawBlob bool // a blob event has arrived → per-layer plain lines are now redundant
}

// newLiveRenderer builds a renderer writing to w. size reports the current
// terminal (cols, rows); it is queried each frame so the block follows a
// resize and never grows past the visible viewport. A nil size defaults to
// a fixed 80x24.
func newLiveRenderer(w io.Writer, size func() (int, int)) *liveRenderer {
	if size == nil {
		size = func() (int, int) { return 80, 24 }
	}
	return &liveRenderer{w: w, size: size, blobs: map[string]*blobState{}}
}

// handle consumes one progress event and updates the live display.
func (r *liveRenderer) handle(ev backend.ProgressEvent) {
	if ev.Kind == backend.KindBlob {
		r.sawBlob = true
		b, ok := r.blobs[ev.ID]
		if !ok {
			b = &blobState{}
			r.blobs[ev.ID] = b
			r.order = append(r.order, ev.ID)
		}
		b.human, b.status, b.current, b.total = ev.Message, ev.Status, ev.Current, ev.Total
		r.redraw()
		return
	}
	// Plain (non-blob) event. Show it until bars take over: the pre-blob
	// "Fetching manifest..." header shows, and once blob events start the
	// per-layer plain lines ("Pulling layer 2/6") are redundant with the
	// bars, so suppress them. Crucially, if NO blob events ever arrive (an
	// older server that ignores ?progress=blob and streams only plain
	// lines), nothing is suppressed and the user still sees full progress.
	if ev.Message == "" || r.sawBlob {
		return
	}
	r.printAbove(func() { fmt.Fprintf(r.w, "  %s\n", ev.Message) })
}

// printAbove erases the live block, runs emit (which scrolls text into
// history above the block), then redraws the block beneath it.
func (r *liveRenderer) printAbove(emit func()) {
	r.erase()
	emit()
	r.draw()
}

func (r *liveRenderer) redraw() {
	r.erase()
	r.draw()
}

func (r *liveRenderer) erase() {
	if r.drawn > 0 {
		// Move up r.drawn lines and clear from the cursor to end of screen.
		fmt.Fprintf(r.w, "\x1b[%dA\x1b[J", r.drawn)
		r.drawn = 0
	}
}

func (r *liveRenderer) draw() {
	cols, rows := r.size()
	// Never draw a block taller than the viewport: erasing more rows than
	// are visible would clamp at the top and clear unrelated scrollback. When
	// there are more blobs than fit, show the most recent ones plus a
	// "… N more" line so the block stays bounded and the cursor math exact.
	ids := r.order
	maxRows := rows - 1
	if maxRows < 1 {
		maxRows = 1
	}
	if len(ids) > maxRows {
		hidden := len(ids) - (maxRows - 1)
		fmt.Fprintln(r.w, truncate(fmt.Sprintf("  … %d more", hidden), cols-1))
		ids = ids[len(ids)-(maxRows-1):]
		r.drawn = 1
	} else {
		r.drawn = 0
	}
	for _, id := range ids {
		fmt.Fprintln(r.w, r.line(id, r.blobs[id], cols))
	}
	r.drawn += len(ids)
}

// line formats one blob's display line, clamped to width-1 columns (leaving a
// cell of slack so the terminal never autowraps the line into a second row,
// which would break the redraw row count).
func (r *liveRenderer) line(id string, b *blobState, width int) string {
	const barWidth = 24
	label := b.human
	if label == "" {
		label = vmimage.ShortDigest(id)
	}
	var s string
	switch b.status {
	case vmimage.BlobStatusExists:
		s = fmt.Sprintf("  ✓ %s  (cached)", label)
	case vmimage.BlobStatusDone:
		s = fmt.Sprintf("  %s  %s  ✓ %s", label, renderBar(b.current, b.total, barWidth), formatSize(b.total))
	default: // downloading
		pct := int64(0)
		if b.total > 0 {
			pct = 100 * b.current / b.total
		}
		s = fmt.Sprintf("  %s  %s %3d%%  %s/%s", label, renderBar(b.current, b.total, barWidth), pct, formatSize(b.current), formatSize(b.total))
	}
	return truncate(s, width-1)
}

// finish redraws the final state and leaves the block in the scrollback.
func (r *liveRenderer) finish() {
	r.redraw()
}
