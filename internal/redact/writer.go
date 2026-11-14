package redact

import (
	"bytes"
	"io"
	"sync"
)

// maxPending bounds how much of an unterminated line a Writer holds back. A
// longer line is passed on in pieces, each redacted on its own, so a URL split
// across that boundary may escape: output this long without a line break is
// not a log line anyone reads.
const maxPending = 64 << 10

// Writer redacts a stream line by line before passing it on. Lines end at
// '\n' or '\r' (git's progress output uses '\r'). Call Flush when the stream
// ends to pass on a final unterminated line. Safe for concurrent use.
type Writer struct {
	mu      sync.Mutex
	w       io.Writer
	pending []byte
}

// NewWriter returns a Writer that passes redacted lines to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write buffers p and passes on every complete line, redacted. It reports
// len(p) unless the underlying writer fails.
func (rw *Writer) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.pending = append(rw.pending, p...)
	cut := bytes.LastIndexAny(rw.pending, "\n\r") + 1
	if cut == 0 && len(rw.pending) > maxPending {
		cut = len(rw.pending)
	}
	if cut == 0 {
		return len(p), nil
	}
	err := rw.emit(rw.pending[:cut])
	rw.pending = append(rw.pending[:0], rw.pending[cut:]...)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush passes on whatever is buffered, redacted.
func (rw *Writer) Flush() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.pending) == 0 {
		return nil
	}
	err := rw.emit(rw.pending)
	rw.pending = rw.pending[:0]
	return err
}

func (rw *Writer) emit(b []byte) error {
	_, err := io.WriteString(rw.w, String(string(b)))
	return err
}
