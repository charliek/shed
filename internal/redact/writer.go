package redact

import (
	"bytes"
	"io"
	"sync"
)

// maxPending bounds how much of an unterminated line a Writer holds back.
// A longer line is passed on in pieces cut at white space, which no URL
// token spans, so each piece is redacted whole. A single token longer than
// the bound cannot be cut safely (a URL's scheme could fall on one side and
// its '@' on the other), so it is not passed on at all: Dropped is written in
// its place.
const maxPending = 64 << 10

// Dropped is written in place of a token that is longer than the Writer can
// hold and so cannot be redacted.
const Dropped = "[more than 64 KiB without a space or line break: not logged]"

// Writer redacts a stream line by line before passing it on. Lines end at
// '\n' or '\r' (git's progress output uses '\r'). Call Flush when the stream
// ends to pass on a final unterminated line. Safe for concurrent use.
type Writer struct {
	mu      sync.Mutex
	w       io.Writer
	pending []byte
	// skipping is set while the rest of an over-long token is discarded.
	skipping bool
}

// NewWriter returns a Writer that passes redacted lines to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// isBreak reports whether c is white space or a control byte: a byte no URL
// token contains, so the stream may be cut after it.
func isBreak(c byte) bool { return c <= ' ' }

// Write buffers p and passes on every complete line, redacted. It reports
// len(p) unless the underlying writer fails.
//
// p is taken in pieces of at most maxPending bytes, so the Writer never holds
// more than twice that, whatever the size of one Write: a guest's data frame
// can be 16 MiB, and one with no break in it would otherwise be copied whole
// before it is dropped. Cutting p anywhere is safe, because a Write boundary
// may already fall anywhere.
func (rw *Writer) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		piece := p
		if len(piece) > maxPending {
			piece = piece[:maxPending]
		}
		p = p[len(piece):]
		if err := rw.write(piece); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// write takes one piece of at most maxPending bytes. The caller holds rw.mu.
func (rw *Writer) write(p []byte) error {
	if rw.skipping {
		i := 0
		for i < len(p) && !isBreak(p[i]) {
			i++
		}
		if i == len(p) {
			return nil
		}
		p = p[i:]
		rw.skipping = false
	}
	rw.pending = append(rw.pending, p...)
	cut := bytes.LastIndexAny(rw.pending, "\n\r") + 1
	if len(rw.pending)-cut > maxPending {
		// Too much after the last line end (or no line end at all): cut
		// after the last break instead.
		for cut = len(rw.pending); cut > 0 && !isBreak(rw.pending[cut-1]); cut-- {
		}
	}
	if cut > 0 {
		err := rw.emit(rw.pending[:cut])
		rw.pending = append(rw.pending[:0], rw.pending[cut:]...)
		if err != nil {
			return err
		}
	}
	if len(rw.pending) > maxPending {
		// What is left is one token (it holds no break) longer than the
		// bound. Nothing of it is passed on, and neither is the rest of it
		// when it comes.
		rw.pending = rw.pending[:0]
		rw.skipping = true
		if _, err := io.WriteString(rw.w, Dropped); err != nil {
			return err
		}
	}
	return nil
}

// Flush passes on whatever is buffered, redacted.
func (rw *Writer) Flush() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.skipping = false
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
