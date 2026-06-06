package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/vmimage"
	"golang.org/x/term"
)

// layerProgressLineRe matches the per-layer pull lines the server emits
// (internal/vmimage/registry.go): "Pulling layer N/M …" and
// "Layer N/M … already present". A renderer client suppresses these once bars
// are live because the per-blob bar conveys the same thing; every other plain
// line still shows — the manifest header for `image pull`, and the boot-phase
// lines for `create`, which arrive after the pull bars.
var layerProgressLineRe = regexp.MustCompile(`^(Pulling layer|Layer) \d+/\d+\b`)

func isLayerProgressLine(msg string) bool { return layerProgressLineRe.MatchString(msg) }

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
//
// Not safe for concurrent use: it is driven serially by the single SSE
// reader goroutine (readSSEStream), which delivers one event at a time.
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
	// Plain (non-blob) event. Suppress only the redundant per-layer pull
	// lines once bars are live (their bar conveys the same thing). Everything
	// else shows: the "Fetching manifest..." header for `image pull`, and the
	// boot-phase lines for `create` (which arrive after the pull bars — so a
	// blanket "suppress after first blob" would wrongly hide them). If NO blob
	// event ever arrives (an older server that ignores ?progress=blob and
	// streams only plain lines), nothing is suppressed.
	if ev.Message == "" || (r.sawBlob && isLayerProgressLine(ev.Message)) {
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

// progressSink wires a backend.ProgressEvent stream (from `image pull` or
// `create`) to either a live TTY renderer or the plain line printer. It
// returns the onProgress callback, a finish func to call when the stream
// ends, and wantBlob — whether the caller should request ?progress=blob
// (true only on an interactive, non-JSON terminal). Warnings always go to
// stderr, erased/redrawn around so they don't corrupt the live block.
func progressSink(jsonOutput bool) (onProgress func(backend.ProgressEvent), finish func(), wantBlob bool) {
	useLive := !jsonOutput && isProgressTTY(os.Stdout)
	var renderer *liveRenderer
	if useLive {
		renderer = newLiveRenderer(os.Stdout, func() (int, int) { return terminalSize(os.Stdout) })
	}
	onProgress = func(event backend.ProgressEvent) {
		switch {
		case jsonOutput:
			// --json suppresses progress; only the final response prints.
		case event.Warning:
			warn := func() { fmt.Fprintf(os.Stderr, "  Warning: %s\n", event.Message) }
			if renderer != nil {
				renderer.printAbove(warn)
			} else {
				warn()
			}
		case renderer != nil:
			renderer.handle(event)
		default:
			fmt.Printf("  %s\n", event.Message)
		}
	}
	finish = func() {
		if renderer != nil {
			renderer.finish()
		}
	}
	return onProgress, finish, useLive
}
