package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// Reading the log back, newest first, a page at a time (the activity feed,
// GET /api/activity).
//
// A page ends at a Cursor: the place in the log it stopped, given as the
// seq of the newest record it is older than and the position in the file
// where the read resumes. The file is named by a hash of its first bytes
// (headBytes), which no rotation changes (a rotated file is renamed, never
// rewritten, and the current file only grows), so a cursor means the same
// place however many rotations come between two pages; a cursor whose file
// has been dropped by rotation has nothing older left. Because the cursor
// holds a position, a page can stop anywhere, in the middle of a damaged
// stretch too, and the next page goes on from there: no page needs a search,
// and none reads more than its Budget.

// MaxLineBytes bounds a line the reader takes. A longer one is passed over
// as damaged (no record the server writes comes near it).
const MaxLineBytes = 1 << 20

// readChunk is how much of a file the reader reads at a time.
const readChunk = 64 << 10

// headBytes is how much of a file's start names it in a cursor: enough to
// hold the first record's time, to the nanosecond, and its seq.
const headBytes = 64

// MinBudgetBytes is the least a page may read: a whole line of the longest
// length the reader takes, with the reads around it and the file heads, so
// every page makes progress.
const MinBudgetBytes = MaxLineBytes + 4*readChunk + 8*headBytes

// Budget bounds what one page reads. Lines counts every line looked at, of
// any kind (records, damaged lines, lines too long, lines passed over);
// Bytes counts every byte read from the files, the file heads included.
// Lines below 1 is taken as 1, and Bytes below MinBudgetBytes as it.
type Budget struct {
	Lines int
	Bytes int64
}

// Cursor is a place in the log: a page read from it holds the lines that
// start before it, and nothing newer than Seq. The zero Cursor is the
// newest end of the log.
type Cursor struct {
	// Seq is the seq of the record the place is just older than; records
	// with this seq or a higher one are never read from it. 0 when the
	// place is newer than every record (or is the zero cursor).
	Seq uint64
	// head names the file (hex of the hash of its first headLen bytes).
	head    string
	headLen int
	// off is where in the file the read resumes: the lines that start
	// before it are read.
	off int64
	// mid says off is inside a line too long to take, which is passed
	// over.
	mid bool
}

// IsZero reports whether c is the zero Cursor (the newest end).
func (c Cursor) IsZero() bool { return c == Cursor{} }

const cursorPrefix = "a2:"

// String is c's opaque form ("" for the zero Cursor).
func (c Cursor) String() string {
	if c.IsZero() {
		return ""
	}
	mid := 0
	if c.mid {
		mid = 1
	}
	raw := fmt.Sprintf("%s%d:%s:%d:%d:%d", cursorPrefix, c.Seq, c.head, c.headLen, c.off, mid)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseCursor reads a Cursor from its opaque form. ok is false for a string
// that is not one String made.
func ParseCursor(s string) (Cursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, false
	}
	rest, found := strings.CutPrefix(string(raw), cursorPrefix)
	if !found {
		return Cursor{}, false
	}
	f := strings.Split(rest, ":")
	if len(f) != 5 {
		return Cursor{}, false
	}
	num := func(s string) (uint64, bool) {
		n, err := strconv.ParseUint(s, 10, 63)
		return n, err == nil && strconv.FormatUint(n, 10) == s
	}
	seq, ok1 := num(f[0])
	headLen, ok2 := num(f[2])
	off, ok3 := num(f[3])
	if !ok1 || !ok2 || !ok3 || headLen < 1 || headLen > headBytes || len(f[1]) != 16 || (f[4] != "0" && f[4] != "1") {
		return Cursor{}, false
	}
	if _, err := hex.DecodeString(f[1]); err != nil || strings.ToLower(f[1]) != f[1] {
		return Cursor{}, false
	}
	c := Cursor{Seq: seq, head: f[1], headLen: int(headLen), off: int64(off), mid: f[4] == "1"}
	if c.IsZero() {
		return Cursor{}, false
	}
	return c, true
}

// ReadBack reads the log's records older than from (every record, from the
// newest, when from is the zero Cursor), newest first, across the rotated
// files, and hands each to visit with the cursor just older than it (its
// place: a read from there starts after it) and its seq. visit says whether
// it took the record; line is only valid during the call.
//
// The read stops after limit records were taken, at the end of b, or at the
// end of the log. more says whether there may be more: next is then where
// to go on from. A line that is not a record (not a JSON object, or a seq
// that is not a positive integer) or is longer than MaxLineBytes is passed
// over; a record with no seq member (one an older server wrote) ends the
// read, since nothing older can carry one. A cursor whose file is no longer
// in the log (rotated away) has nothing older: the read ends there.
//
// The files are opened under the log's lock and read after it is released:
// a rotation or a record written during the read does not disturb it (an
// open file survives its rename, and only the bytes the current file held at
// the start are read).
func (l *Log) ReadBack(from Cursor, limit int, b Budget, visit func(at Cursor, seq uint64, line []byte) bool) (next Cursor, more bool, err error) {
	if l == nil {
		return Cursor{}, false, nil
	}
	limit = max(limit, 1)
	b.Lines = max(b.Lines, 1)
	b.Bytes = max(b.Bytes, MinBudgetBytes)
	files, err := l.snapshot()
	if err != nil {
		return Cursor{}, false, err
	}
	defer func() {
		for _, f := range files {
			_ = f.f.Close()
		}
	}()

	r := &pageReader{files: files, budget: b.Bytes}
	start, end, mid := 0, int64(0), false
	if from.IsZero() {
		if len(files) > 0 {
			end = files[0].size
		}
	} else {
		start = -1
		for i := range files {
			if files[i].size < int64(from.headLen) || from.off > files[i].size {
				continue
			}
			h, err := r.head(i, from.headLen)
			if err != nil {
				return Cursor{}, false, err
			}
			if h == from.head {
				start, end, mid = i, from.off, from.mid
				break
			}
		}
		if start < 0 {
			return Cursor{}, false, nil // its file was rotated away
		}
	}

	seqBound := from.Seq
	taken, looked := 0, 0
	for i := start; i < len(files); i++ {
		if i > start {
			end, mid = files[i].size, false
		}
		if files[i].size == 0 {
			continue // an empty file has no lines, and no name
		}
		headLen := int(min(int64(headBytes), files[i].size))
		head, err := r.head(i, headLen)
		if err != nil {
			return Cursor{}, false, err
		}
		place := func(off int64, mid bool) (Cursor, bool) {
			if off == 0 && !mid && !olderLines(files[i+1:]) {
				return Cursor{}, false // the start of the oldest file
			}
			return Cursor{Seq: seqBound, head: head, headLen: headLen, off: off, mid: mid}, true
		}
		sc := &backScanner{r: r, f: files[i].f, pos: end, end: end, skip: mid}
		for {
			ln, err := sc.next()
			if errors.Is(err, errBudget) {
				// The page's bytes are spent: go on from where the
				// reading stopped (inside a line too long to take, or at
				// the start of the last line looked at).
				if sc.skip {
					c, ok := place(sc.pos, true)
					return c, ok, nil
				}
				c, ok := place(sc.end, false)
				return c, ok, nil
			}
			if err != nil {
				return Cursor{}, false, err
			}
			if ln == nil {
				break // the start of the file
			}
			looked++
			kind, seq := classify(ln)
			switch {
			case kind == lineLegacy:
				return Cursor{}, false, nil
			case kind == lineRecord && (seqBound == 0 || seq < seqBound):
				seqBound = seq
				at := Cursor{Seq: seq, head: head, headLen: headLen, off: ln.start}
				if visit(at, seq, ln.data) {
					taken++
				}
			}
			if taken >= limit || looked >= b.Lines {
				c, ok := place(ln.start, false)
				return c, ok, nil
			}
		}
	}
	return Cursor{}, false, nil
}

// olderLines reports whether any of files holds a byte.
func olderLines(files []snapFile) bool {
	for _, f := range files {
		if f.size > 0 {
			return true
		}
	}
	return false
}

// errBudget ends a page whose bytes are spent.
var errBudget = errors.New("audit: the page's read budget is spent")

// pageReader reads the files of one page and counts its bytes.
type pageReader struct {
	files  []snapFile
	budget int64
	read   int64
	heads  map[[2]int]string
}

// readAt reads len(p) bytes of f at off, counted against the budget.
func (r *pageReader) readAt(f *os.File, p []byte, off int64) error {
	if r.read+int64(len(p)) > r.budget {
		return errBudget
	}
	r.read += int64(len(p))
	if _, err := f.ReadAt(p, off); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// head is file i's name in a cursor: the hash of its first n bytes (n is
// at most headBytes and the file's size). Each is read once a page.
func (r *pageReader) head(i, n int) (string, error) {
	if h, ok := r.heads[[2]int{i, n}]; ok {
		return h, nil
	}
	buf := make([]byte, n)
	r.read += int64(n) // within MinBudgetBytes
	if _, err := r.files[i].f.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	sum := sha256.Sum256(buf)
	h := hex.EncodeToString(sum[:8])
	if r.heads == nil {
		r.heads = map[[2]int]string{}
	}
	r.heads[[2]int{i, n}] = h
	return h, nil
}

// line is one line a backScanner found: its bytes (nil when it is longer
// than MaxLineBytes) and where it starts.
type line struct {
	data     []byte
	start    int64
	oversize bool
}

// backScanner gives the lines of f that start before end, last first.
type backScanner struct {
	r *pageReader
	f *os.File
	// pos is where the bytes read so far begin; data holds [pos, end), the
	// part of the file before end not given out yet.
	pos, end int64
	data     []byte
	// skip is set while the bytes before end belong to a line found too
	// long: they are read and dropped until its start.
	skip bool
}

// next returns the last line before s.end, or nil at the start of the file.
// It stops with errBudget when the page's bytes are spent; s.pos, s.end and
// s.skip then say where it stopped.
func (s *backScanner) next() (*line, error) {
	for {
		if s.skip {
			// Find the start of the line too long to take.
			if i := bytes.LastIndexByte(s.data, '\n'); i >= 0 {
				start := s.pos + int64(i) + 1
				s.data, s.end, s.skip = s.data[:i+1], start, false
				return &line{start: start, oversize: true}, nil
			}
			s.data = nil
			if s.pos == 0 {
				s.end, s.skip = 0, false
				return &line{start: 0, oversize: true}, nil
			}
			if err := s.readMore(); err != nil {
				return nil, err
			}
			continue
		}
		body := s.data
		if n := len(body); n > 0 && body[n-1] == '\n' {
			body = body[:n-1]
		}
		i := bytes.LastIndexByte(body, '\n')
		// Every completed line is held to MaxLineBytes before it is given
		// out; one still being read is dropped as soon as it passes it.
		if n := len(body) - (i + 1); n > MaxLineBytes {
			switch {
			case i >= 0 || s.pos == 0:
				start := s.pos + int64(i) + 1
				s.data, s.end = s.data[:i+1], start
				return &line{start: start, oversize: true}, nil
			default:
				s.data, s.skip = nil, true
				continue
			}
		}
		if i >= 0 {
			start := s.pos + int64(i) + 1
			out := body[i+1:]
			s.data, s.end = s.data[:i+1], start
			if len(out) == 0 {
				continue
			}
			return &line{data: out, start: start}, nil
		}
		if s.pos == 0 {
			s.data, s.end = nil, 0
			if len(body) == 0 {
				return nil, nil
			}
			return &line{data: body, start: 0}, nil
		}
		if err := s.readMore(); err != nil {
			return nil, err
		}
	}
}

// readMore reads the chunk before s.pos in front of s.data.
func (s *backScanner) readMore() error {
	n := min(int64(readChunk), s.pos)
	buf := make([]byte, n, n+int64(len(s.data)))
	if err := s.r.readAt(s.f, buf, s.pos-n); err != nil {
		return err
	}
	s.data = append(buf, s.data...)
	s.pos -= n
	return nil
}

// The kinds of line the reader tells apart.
const (
	lineDamaged = iota // not a JSON object, a seq that is not a positive integer, or too long
	lineLegacy         // a record with no seq member: an older server's
	lineRecord
)

// classify says what ln is, and its seq when it is a record.
func classify(ln *line) (kind int, seq uint64) {
	if ln.oversize {
		return lineDamaged, 0
	}
	return lineSeq(ln.data)
}

// lineSeq says what a line is, and its seq when it is a record.
func lineSeq(data []byte) (kind int, seq uint64) {
	var v struct {
		Seq json.RawMessage `json:"seq"`
	}
	if len(data) == 0 || data[0] != '{' || json.Unmarshal(data, &v) != nil {
		return lineDamaged, 0
	}
	if v.Seq == nil {
		return lineLegacy, 0
	}
	if err := json.Unmarshal(v.Seq, &seq); err != nil || seq == 0 {
		return lineDamaged, 0
	}
	return lineRecord, seq
}

// snapFile is one of the log's files, open for reading, and how many of its
// bytes the read takes.
type snapFile struct {
	f    *os.File
	size int64
}

// snapshot opens the log's files, newest first, under its lock (a missing
// one is passed over).
func (l *Log) snapshot() ([]snapFile, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []snapFile
	for _, p := range l.files() {
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			var st os.FileInfo
			if st, err = f.Stat(); err == nil {
				out = append(out, snapFile{f: f, size: st.Size()})
				continue
			}
			_ = f.Close()
		}
		for _, o := range out {
			_ = o.f.Close()
		}
		return nil, err
	}
	return out, nil
}

// lastSeq is the highest seq in the files at paths (newest first): the first
// record's seq found reading back from the newest line. 0 when there is
// none. It reads as far as it must (it runs once, at Open).
func lastSeq(paths []string) (uint64, error) {
	r := &pageReader{budget: 1<<63 - 1}
	for _, p := range paths {
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return 0, err
		}
		sc := &backScanner{r: r, f: f, pos: st.Size(), end: st.Size()}
		var found uint64
		for {
			ln, err := sc.next()
			if err != nil {
				_ = f.Close()
				return 0, err
			}
			if ln == nil {
				break
			}
			if kind, seq := classify(ln); kind == lineRecord {
				found = seq
				break
			}
		}
		_ = f.Close()
		if found > 0 {
			return found, nil
		}
	}
	return 0, nil
}
