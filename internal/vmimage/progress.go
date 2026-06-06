package vmimage

import (
	"io"
	"time"
)

// ProgressEvent is a progress update from a long-running image operation
// (pull / push / ensure). Stage + Message are the plain status fields used
// since the beginning. The Kind=="blob" fields carry structured per-blob
// byte progress for the `shed` CLI's live renderer; line-mode and older
// clients ignore them and render Message.
//
// Backends translate this to backend.ProgressEvent at the API boundary
// (vmimage must not import backend), so the wire shape is owned by the
// backend package — this struct is the in-process carrier.
type ProgressEvent struct {
	Stage   string // phase/stage label (e.g. "image"); advances the PhaseTimer
	Message string // human-readable status line; always set for emitted events

	// Structured per-blob byte progress. Zero on plain status events.
	Kind    string // "" plain status; "blob" per-blob byte progress
	ID      string // full digest (blob events); renderer keys on this
	Status  string // BlobStatus* (blob events)
	Current int64  // bytes fetched so far (blob events)
	Total   int64  // total bytes — compressed descriptor size (blob events)
}

// ProgressFunc receives progress events during long-running operations.
type ProgressFunc func(ProgressEvent)

// IsBlob reports whether e carries structured per-blob byte progress. Plain
// line-oriented consumers (logs, the bulk pull-images command) use this to
// skip byte-tick events they would otherwise spam.
func (e ProgressEvent) IsBlob() bool { return e.Kind == progressKind }

// Blob status values for ProgressEvent.Status when Kind=="blob". These
// string values cross the wire (the backend forwards Status verbatim), so
// they are the single source of truth for both sides.
const (
	BlobStatusDownloading = "downloading" // a byte tick or the initial 0-byte start
	BlobStatusExists      = "exists"      // blob already present in the store
	BlobStatusDone        = "done"        // fully fetched + verified (Current == Total)
)

// progressKind marks a blob event.
const progressKind = "blob"

// safeEmit delivers ev to p when p is non-nil. The single nil-check point
// for all progress emission in this package.
func safeEmit(p ProgressFunc, ev ProgressEvent) {
	if p != nil {
		p(ev)
	}
}

// emitStatus delivers a plain status line to p (nil-safe). Use this for the
// human-readable progress lines that line-mode clients render.
func emitStatus(p ProgressFunc, stage, msg string) {
	safeEmit(p, ProgressEvent{Stage: stage, Message: msg})
}

// emitBlob delivers a one-shot structured per-blob event to p (nil-safe),
// e.g. the "exists" event for a cached blob. human is carried as Message
// for line-mode fallback.
func emitBlob(p ProgressFunc, digest, human, status string, current, total int64) {
	safeEmit(p, ProgressEvent{
		Stage:   "image",
		Message: human,
		Kind:    progressKind,
		ID:      digest,
		Status:  status,
		Current: current,
		Total:   total,
	})
}

// blobThrottle rate-limits byte-tick emission to at most once per interval,
// so a fast download doesn't flood the SSE channel. now is injectable for
// tests; a zero now defaults to time.Now.
type blobThrottle struct {
	interval time.Duration
	last     time.Time
	now      func() time.Time
}

// ready reports whether enough time has elapsed since the last tick. The
// first call always returns true (last is zero).
func (t *blobThrottle) ready() bool {
	nowFn := t.now
	if nowFn == nil {
		nowFn = time.Now
	}
	n := nowFn()
	if t.last.IsZero() || n.Sub(t.last) >= t.interval {
		t.last = n
		return true
	}
	return false
}

// defaultBlobTickInterval bounds byte-tick events to ~10/sec per blob.
const defaultBlobTickInterval = 100 * time.Millisecond

// progressReader wraps a blob's compressed reader and emits throttled
// "downloading" byte-tick events as bytes flow. The initial 0-byte start
// and the terminal "done" event are emitted explicitly by the caller (see
// streamBlobWithProgress) so they are never coalesced away.
type progressReader struct {
	r        io.Reader
	id       string // full digest
	human    string // human message carried on every blob event
	total    int64
	current  int64
	progress ProgressFunc
	throttle blobThrottle
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.current += int64(n)
		if p.throttle.ready() {
			p.emit(BlobStatusDownloading)
		}
	}
	return n, err
}

// emit sends a blob event with the reader's current byte count.
func (p *progressReader) emit(status string) {
	emitBlob(p.progress, p.id, p.human, status, p.current, p.total)
}

// streamBlobWithProgress streams rc into the blob store under digest while
// emitting per-blob byte progress: a 0-byte "downloading" start, throttled
// byte ticks, and a terminal "done" (Current==Total). human is the message
// carried on each event for line-mode fallback. write performs the actual
// hash-verified write (writeBlobFromReader-shaped).
// The caller emits the 0-byte "downloading" start (so the renderer learns
// Total up front and, for layers, so the start precedes the verbose line);
// this function emits the throttled byte ticks and the terminal "done".
func streamBlobWithProgress(digest, human string, total int64, progress ProgressFunc, rc io.Reader, write func(io.Reader) error) error {
	pr := &progressReader{
		r:        rc,
		id:       digest,
		human:    human,
		total:    total,
		progress: progress,
		throttle: blobThrottle{interval: defaultBlobTickInterval},
	}
	if err := write(pr); err != nil {
		return err
	}
	pr.current = total
	pr.emit(BlobStatusDone)
	return nil
}
