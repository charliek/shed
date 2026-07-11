package rc

import (
	"bytes"
	"io"
	"os"
	"syscall"
	"time"
)

// lineTailer is an offset-tracked, resilient JSONL tailer over a single file. It is
// the shared engine both the codex rollout watcher and the claude transcript watcher
// sit on. It is deliberately pure I/O + buffering (no fsnotify, no parsing): a caller
// drives it with poll() — from the hub's reconcile tick and/or an fsnotify nudge —
// and folds the returned complete lines. This keeps it unit-testable against real
// temp files with manual poll() calls (no filesystem-notification flake).
//
// It handles the ways an append-only agent log misbehaves in practice:
//
//   - Partial lines: a poll can land mid-write; only bytes up to the last '\n' are
//     emitted, the trailing fragment is buffered for the next poll (a half-written
//     JSON object is never handed to the parser).
//   - Oversized lines: a single line longer than tailMaxLine (corrupt / pathological)
//     is skipped rather than buffered without bound — the buffer can't grow forever.
//   - Truncation-in-place: the file shrinking below our offset (a rewrite) resets the
//     read to the new start and signals a reset so the fold clears stale state.
//   - Rotation / inode swap: the path pointing at a NEW inode (rename+recreate) or the
//     file briefly vanishing (rotated away) reopens from the new file's start.
//   - Permission / transient errors: surfaced to the caller, which retains its prior
//     verdict and retries on the next poll (open is re-attempted).
//
// The first successful open honors catchUp: with it set, a bounded window from the
// end of the file is read so a just-correlated session's CURRENT activity is
// established without parsing the whole history; without it, the tailer seeks to EOF
// and only follows new appends (the ambiguous-correlation path, where trusting
// history would risk reading the wrong session's file).
type lineTailer struct {
	path    string
	catchUp bool

	f        *os.File
	inode    uint64
	offset   int64     // bytes consumed from the current file
	buf      []byte    // trailing partial-line bytes (after the last '\n')
	started  bool      // a first open has happened (subsequent opens are rotation reopens)
	skipToNL bool      // drop bytes until the next '\n' (mid-file seek / oversized recovery)
	sawGap   bool      // an oversized line was skipped since the last poll (record lost)
	hdr      []byte    // the file's first bytes at (re)open — the same-size-rewrite detector
	mtime    time.Time // last observed mtime (arms the same-size header check)
}

const (
	// tailCatchUpWindow bounds the initial read on attach: at most this many bytes from
	// the end of the file are read to establish the current activity before following
	// new appends, so a large historical transcript is not fully parsed on correlate.
	tailCatchUpWindow = 64 * 1024
	// tailMaxLine caps a single JSONL line. A longer line (corrupt / pathological
	// record) is skipped rather than buffered without bound.
	tailMaxLine = 1 * 1024 * 1024
	// tailReadChunk bounds one Read of appended bytes.
	tailReadChunk = 256 * 1024
	// tailHeaderLen is how many leading bytes are remembered to detect a same-size
	// rewrite (see poll). Small on purpose — this is a cheap tripwire, not a hash of
	// the file.
	tailHeaderLen = 256
)

// poll advances the tailer and returns the complete lines appended since the last
// poll. didReset is true when the tailer detected a truncation or rotation and reset
// its position — the caller must clear any accumulated fold state. gapped is true when
// an oversized line was SKIPPED since the last poll (a record was lost mid-stream —
// the caller's fold should drop any state that depended on seeing every record, e.g.
// pending tool-call ids). A non-nil err (open failure, stat failure, read failure)
// means the caller should retain its prior verdict; the next poll re-attempts the open.
func (t *lineTailer) poll() (lines [][]byte, didReset, gapped bool, err error) {
	if t.f == nil {
		if t.started {
			// A reopen after the file vanished (rotation): read the new file from start.
			if err := t.openFrom(0); err != nil {
				return nil, false, false, err
			}
			didReset = true
		} else {
			if err := t.openInitial(); err != nil {
				return nil, false, false, err
			}
			t.started = true
		}
	}

	fi, err := os.Stat(t.path)
	if err != nil {
		// The path is gone (rotated away). Drop the handle; the next poll reopens.
		t.closeFile()
		return nil, didReset, t.takeGap(), err
	}

	if ino := inodeOf(fi); t.inode != 0 && ino != 0 && ino != t.inode {
		// The path now points at a different inode (rename+recreate rotation): reopen
		// from the new file's start and treat it as a reset.
		t.closeFile()
		if err := t.openFrom(0); err != nil {
			return nil, true, t.takeGap(), err
		}
		didReset = true
		if fi, err = os.Stat(t.path); err != nil {
			t.closeFile()
			return nil, didReset, t.takeGap(), err
		}
	}

	switch {
	case fi.Size() < t.offset:
		// Truncation in place: the file was rewritten shorter. Reset to its new start.
		if serr := t.resetToStart(); serr != nil {
			return nil, didReset, t.takeGap(), serr
		}
		didReset = true
	case fi.Size() == t.offset && !fi.ModTime().Equal(t.mtime) && t.headerChanged():
		// Same-size rewrite: the size alone can't reveal a rewrite that lands on
		// exactly our offset, so when the mtime moved without the file growing, cheap-
		// verify the remembered leading bytes. A mismatch means new content → reset and
		// reread. Residual (accepted): a same-size rewrite whose first tailHeaderLen
		// bytes are also identical is indistinguishable from no-op and goes undetected.
		if serr := t.resetToStart(); serr != nil {
			return nil, didReset, t.takeGap(), serr
		}
		didReset = true
	}
	t.mtime = fi.ModTime()

	lines, err = t.readAppended()
	return lines, didReset, t.takeGap(), err
}

// resetToStart rewinds the open file to byte 0, clears the partial buffer, and
// re-captures the header (the file's content is new).
func (t *lineTailer) resetToStart() error {
	if _, err := t.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	t.offset = 0
	t.buf = t.buf[:0]
	t.skipToNL = false
	t.captureHeader()
	return nil
}

// takeGap returns-and-clears the oversized-skip flag (one report per poll).
func (t *lineTailer) takeGap() bool {
	g := t.sawGap
	t.sawGap = false
	return g
}

// captureHeader remembers the file's first tailHeaderLen bytes (ReadAt — the tail's
// read offset is unaffected). Used by the same-size-rewrite check.
func (t *lineTailer) captureHeader() {
	buf := make([]byte, tailHeaderLen)
	n, _ := t.f.ReadAt(buf, 0)
	t.hdr = append(t.hdr[:0], buf[:n]...)
}

// headerChanged reports whether the file's current leading bytes differ from the
// remembered header.
func (t *lineTailer) headerChanged() bool {
	buf := make([]byte, len(t.hdr))
	n, _ := t.f.ReadAt(buf, 0)
	return !bytes.Equal(buf[:n], t.hdr)
}

// openInitial opens the file for the first time, honoring catchUp (bounded tail read)
// vs. seek-to-EOF (follow-only).
func (t *lineTailer) openInitial() error {
	f, err := os.Open(t.path)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	t.f = f
	t.inode = inodeOf(fi)
	t.buf = t.buf[:0]
	t.skipToNL = false
	t.mtime = fi.ModTime()
	t.captureHeader()
	size := fi.Size()

	start := size // follow-only default: seek to EOF
	if t.catchUp {
		start = 0
		if size > tailCatchUpWindow {
			start = size - tailCatchUpWindow
			// Seeking into the middle of the file usually lands mid-line, so the first
			// partial must be dropped — UNLESS the byte just before the window is '\n',
			// in which case the window starts exactly on a line boundary and that first
			// complete record must be kept.
			t.skipToNL = true
			var b [1]byte
			if n, _ := f.ReadAt(b[:], start-1); n == 1 && b[0] == '\n' {
				t.skipToNL = false
			}
		}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		t.closeFile()
		return err
	}
	t.offset = start
	return nil
}

// openFrom (re)opens the file and seeks to pos (used for rotation reopens, always 0).
func (t *lineTailer) openFrom(pos int64) error {
	f, err := os.Open(t.path)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Seek(pos, io.SeekStart); err != nil {
		_ = f.Close()
		return err
	}
	t.f = f
	t.inode = inodeOf(fi)
	t.offset = pos
	t.buf = t.buf[:0]
	t.skipToNL = false
	t.mtime = fi.ModTime()
	t.captureHeader()
	return nil
}

// readAppended reads from the current position to EOF, returning every complete line
// and buffering the trailing partial.
func (t *lineTailer) readAppended() ([][]byte, error) {
	var out [][]byte
	chunk := make([]byte, tailReadChunk)
	for {
		n, err := t.f.Read(chunk)
		if n > 0 {
			t.offset += int64(n)
			t.buf = append(t.buf, chunk[:n]...)
			out = t.drainLines(out)
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// drainLines splits complete lines out of the partial buffer, honoring the
// skip-to-newline (mid-file/oversized recovery) and oversized-line caps. The trailing
// partial is compacted back into a small buffer so a long-lived tailer's buffer does
// not retain a large backing array.
func (t *lineTailer) drainLines(out [][]byte) [][]byte {
	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			if len(t.buf) > tailMaxLine {
				// The pending partial has blown the line cap without a newline: drop it
				// and skip until the next newline (that oversized record is abandoned —
				// a real record was lost, so flag the gap for the fold).
				t.buf = t.buf[:0]
				t.skipToNL = true
				t.sawGap = true
			}
			return out
		}
		line := t.buf[:i]
		rest := t.buf[i+1:]

		switch {
		case t.skipToNL:
			// This newline closes a skipped region (mid-file seek or oversized drop).
			t.skipToNL = false
		case len(line) > tailMaxLine:
			// Oversized complete line — skip it (a record was lost: flag the gap).
			t.sawGap = true
		case len(bytes.TrimSpace(line)) > 0:
			cp := make([]byte, len(line))
			copy(cp, line)
			out = append(out, cp)
		}

		// Compact the remaining bytes to the front of a fresh small slice so repeated
		// reslicing can't pin a large backing array across polls.
		t.buf = append(t.buf[:0], rest...)
	}
}

func (t *lineTailer) closeFile() {
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
}

// close releases the underlying file (watcher teardown).
func (t *lineTailer) close() { t.closeFile() }

// inodeOf returns a file's inode number, or 0 when the platform stat is unavailable.
// Both darwin (dev machine) and linux (the guest the hub runs in) expose Ino on
// syscall.Stat_t, so no build tag is needed.
func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
