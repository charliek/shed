package audit

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// readSeqs is every seq in the files of the log at path, in file order
// (oldest file first), with the lines that carry none left out.
func readSeqs(t *testing.T, path string, keep int) []uint64 {
	t.Helper()
	var out []uint64
	for i := keep; i >= 0; i-- {
		p := path
		if i > 0 {
			p = fmt.Sprintf("%s.%d", path, i)
		}
		f, err := os.Open(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<16), 4<<20)
		for sc.Scan() {
			if kind, seq := lineSeq(sc.Bytes()); kind == lineRecord {
				out = append(out, seq)
			}
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// readAll pages the whole log back with limit per page and b per page,
// taking what take takes, and returns the seqs taken in order and how many
// pages it took. It fails on a page over its limit, a seq read twice, or a
// cursor that does not move.
func readAll(t *testing.T, l *Log, limit int, b Budget, take func(seq uint64, line []byte) bool) ([]uint64, int) {
	t.Helper()
	return readAllFrom(t, l, Cursor{}, limit, b, take)
}

func readAllFrom(t *testing.T, l *Log, from Cursor, limit int, b Budget, take func(seq uint64, line []byte) bool) ([]uint64, int) {
	t.Helper()
	var got []uint64
	seen := map[uint64]bool{}
	pages := 0
	for {
		pages++
		if pages > 100000 {
			t.Fatal("the read does not end")
		}
		n := 0
		next, more, err := l.ReadBack(from, limit, b, func(_ Cursor, seq uint64, line []byte) bool {
			if !take(seq, line) {
				return false
			}
			if seen[seq] {
				t.Fatalf("seq %d read twice", seq)
			}
			seen[seq] = true
			n++
			got = append(got, seq)
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		if n > limit {
			t.Fatalf("a page took %d records, over its limit %d", n, limit)
		}
		if !more {
			return got, pages
		}
		if next == from {
			t.Fatalf("the cursor did not move: %+v", next)
		}
		from = next
	}
}

// big is a budget no test reaches.
var big = Budget{Lines: 1 << 30, Bytes: 1 << 40}

func all(uint64, []byte) bool { return true }

// descending is n, n-1, ..., 1.
func descending(n int) []uint64 {
	out := make([]uint64, 0, n)
	for i := n; i >= 1; i-- {
		out = append(out, uint64(i))
	}
	return out
}

func recordN(t *testing.T, l *Log, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := l.Record(ShedEvent{Event: EventShedStart, Identity: SystemIdentity(), ShedRef: ShedRef{Shed: fmt.Sprintf("s%d", i)}, Result: ResultOK}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSeqConcurrentWriters: records written at once from many goroutines get
// every seq from 1 once, in the files' order.
func TestSeqConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	// Small files: rotations happen while the writers run.
	l, err := Open(path, 4096, 50)
	if err != nil {
		t.Fatal(err)
	}
	const writers, each = 16, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := l.Record(APIEvent{Event: EventAPI, Identity: ClientIdentity("c", "1"), Method: "GET", Route: "/api/sheds", Path: "/api/sheds"}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	// A reader at the same time sees a consistent, newest-first log.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			var last uint64
			if _, _, err := l.ReadBack(Cursor{}, 500, big, func(_ Cursor, seq uint64, _ []byte) bool {
				if last != 0 && seq >= last {
					t.Errorf("read %d after %d", seq, last)
				}
				last = seq
				return true
			}); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	got := readSeqs(t, path, 50)
	want := descending(writers * each)
	slices.Reverse(want)
	if !slices.Equal(got, want) {
		t.Fatalf("seqs in file order are not 1..%d once each: %v", writers*each, got)
	}
}

// TestSeqSurvivesRestart: a log opened again goes on from the highest seq in
// its files, wherever that is.
func TestSeqSurvivesRestart(t *testing.T) {
	t.Run("current file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		l, err := Open(path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		recordN(t, l, 3)
		_ = l.Close()
		l, err = Open(path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		recordN(t, l, 2)
		_ = l.Close()
		if got := readSeqs(t, path, DefaultKeep); !slices.Equal(got, []uint64{1, 2, 3, 4, 5}) {
			t.Fatalf("seqs = %v", got)
		}
	})
	t.Run("only a rotated file has one", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		writeFile(t, path, "")
		writeFile(t, path+".1", `{"ts":"t","seq":6,"event":"api"}`+"\n"+`{"ts":"t","seq":7,"event":"api"}`+"\n")
		l, err := Open(path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		recordN(t, l, 1)
		_ = l.Close()
		if got := readSeqs(t, path, DefaultKeep); !slices.Equal(got, []uint64{6, 7, 8}) {
			t.Fatalf("seqs = %v", got)
		}
	})
	t.Run("an older server's lines after the last seq", func(t *testing.T) {
		// A downgrade wrote lines with no seq after ones with: the next seq
		// still follows the highest.
		path := filepath.Join(t.TempDir(), FileName)
		writeFile(t, path, `{"ts":"t","seq":4,"event":"api"}`+"\n"+`{"ts":"t","event":"api"}`+"\n")
		l, err := Open(path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		recordN(t, l, 1)
		_ = l.Close()
		if got := readSeqs(t, path, DefaultKeep); !slices.Equal(got, []uint64{4, 5}) {
			t.Fatalf("seqs = %v", got)
		}
	})
	t.Run("a log from before seq", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		writeFile(t, path, `{"ts":"t","event":"api"}`+"\n")
		l, err := Open(path, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		recordN(t, l, 2)
		got, _ := readAll(t, l, 10, big, all)
		_ = l.Close()
		// The old line ends the read: nothing older can carry a seq.
		if !slices.Equal(got, []uint64{2, 1}) {
			t.Fatalf("read = %v", got)
		}
	})
}

// TestOpenEndsACutShortLine: a last line cut short by a crash does not
// swallow the next record.
func TestOpenEndsACutShortLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writeFile(t, path, `{"ts":"t","seq":1,"event":"api"}`+"\n"+`{"ts":"t","seq":2,"ev`)
	l, err := Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	recordN(t, l, 1)
	got, _ := readAll(t, l, 10, big, all)
	_ = l.Close()
	if !slices.Equal(got, []uint64{2, 1}) {
		t.Fatalf("read = %v (the record after the cut line must be its own line, with seq 2)", got)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReadBackPaging: pages newest first, each continuing where the last
// ended, with no record twice and none missed, across rotated files.
func TestReadBackPaging(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, 1024, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	recordN(t, l, 97)
	if _, err := os.Stat(path + ".3"); err != nil {
		t.Fatalf("the test needs several rotated files: %v", err)
	}
	for _, limit := range []int{1, 2, 7, 13, 96, 97, 500} {
		got, pages := readAll(t, l, limit, big, all)
		if !slices.Equal(got, descending(97)) {
			t.Fatalf("limit %d: read %v", limit, got)
		}
		if want := (97 + limit - 1) / limit; pages < want || pages > want+1 {
			t.Errorf("limit %d: %d pages, want %d or %d", limit, pages, want, want+1)
		}
	}
}

// TestReadBackEventCursor: the cursor handed with a record is its place: a
// read from it goes on with the record just older, in its file or the next.
func TestReadBackEventCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, 1024, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	recordN(t, l, 40)
	at := map[uint64]Cursor{}
	if _, _, err := l.ReadBack(Cursor{}, 100, big, func(c Cursor, seq uint64, _ []byte) bool { at[seq] = c; return true }); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 40; seq++ {
		c, ok := ParseCursor(at[seq].String())
		if !ok || c != at[seq] {
			t.Fatalf("seq %d: cursor does not round-trip: %+v", seq, at[seq])
		}
		var got []uint64
		_, _, err := l.ReadBack(c, 1, big, func(_ Cursor, s uint64, _ []byte) bool { got = append(got, s); return true })
		if err != nil {
			t.Fatal(err)
		}
		want := []uint64{seq - 1}
		if seq == 1 {
			want = nil
		}
		if !slices.Equal(got, want) {
			t.Errorf("after seq %d: read %v, want %v", seq, got, want)
		}
	}
}

// TestReadBackFilterBeforePaging: the records a page skips do not count
// toward its limit, so every page but the last is full.
func TestReadBackFilterBeforePaging(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, 2048, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	recordN(t, l, 60)
	var from Cursor
	var got []uint64
	for page := 0; ; page++ {
		n := 0
		next, more, err := l.ReadBack(from, 4, big, func(_ Cursor, seq uint64, _ []byte) bool {
			if seq%2 != 0 {
				return false
			}
			n++
			got = append(got, seq)
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		if more && n != 4 {
			t.Fatalf("page %d took %d records, want a full page of 4 (the filter must run before the limit)", page, n)
		}
		if !more {
			break
		}
		from = next
	}
	var want []uint64
	for i := 60; i >= 2; i -= 2 {
		want = append(want, uint64(i))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("read %v, want %v", got, want)
	}
}

// TestReadBackAcrossRotationBetweenPages: records written (and files
// rotated, and the oldest dropped) between two pages do not shift what the
// cursor means; a cursor whose file was dropped reads nothing more.
func TestReadBackAcrossRotationBetweenPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, 1024, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	recordN(t, l, 40)
	var got []uint64
	next, more, err := l.ReadBack(Cursor{}, 15, big, func(_ Cursor, seq uint64, _ []byte) bool { got = append(got, seq); return true })
	if err != nil || !more || next.Seq != 26 {
		t.Fatalf("first page: next %+v more %v err %v", next, more, err)
	}
	// Enough new records to rotate every file at least once.
	recordN(t, l, 60)
	rest, _ := readAllFrom(t, l, next, 15, big, all)
	got = append(got, rest...)
	if !slices.Equal(got, descending(40)) {
		t.Fatalf("read %v", got)
	}

	// Keep two old files: the cursor's file goes, and with it everything
	// older.
	small := filepath.Join(t.TempDir(), FileName)
	l2, err := Open(small, 512, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	recordN(t, l2, 10)
	next, more, _ = l2.ReadBack(Cursor{}, 2, big, func(Cursor, uint64, []byte) bool { return true })
	if !more {
		t.Fatal("no cursor")
	}
	recordN(t, l2, 40)
	n := 0
	_, more, err = l2.ReadBack(next, 100, big, func(Cursor, uint64, []byte) bool { n++; return true })
	if err != nil || more || n != 0 {
		t.Errorf("a cursor into a dropped file: read %d, more %v, err %v", n, more, err)
	}
}

// TestReadBackBudgetLines: a page looks at no more than Budget.Lines lines,
// however few it takes, and goes on from the last one it looked at.
func TestReadBackBudgetLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, 2048, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	recordN(t, l, 50)
	looked := 0
	next, more, err := l.ReadBack(Cursor{}, 5, Budget{Lines: 10}, func(Cursor, uint64, []byte) bool { looked++; return false })
	if err != nil {
		t.Fatal(err)
	}
	if looked != 10 || !more || next.Seq != 41 {
		t.Fatalf("looked at %d records, next %+v more %v; want 10 and seq 41", looked, next, more)
	}
	// Taking only the oldest record, ten at a time: five pages, the last
	// one holding it.
	got, pages := readAll(t, l, 5, Budget{Lines: 10}, func(seq uint64, _ []byte) bool { return seq == 1 })
	if !slices.Equal(got, []uint64{1}) || pages != 5 {
		t.Fatalf("got %v in %d pages, want [1] in 5", got, pages)
	}
}

// TestReadBackMalformedSeq: a line whose seq is not a positive integer is
// passed over, and the records older than it are still read; a record with
// no seq member at all (an older server's) ends the read.
func TestReadBackMalformedSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	writeFile(t, path, `{"ts":"t","event":"api"}`+"\n"+
		`{"ts":"t","seq":1,"event":"api"}`+"\n"+
		`{"ts":"t","seq":2,"event":"api"}`+"\n"+
		`{"ts":"t","seq":"bad","event":"api"}`+"\n"+
		`{"ts":"t","seq":0,"event":"api"}`+"\n"+
		`{"ts":"t","seq":-3,"event":"api"}`+"\n"+
		`{"ts":"t","seq":4,"event":"api"}`+"\n")
	l, err := Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, limit := range []int{1, 2, 10} {
		if got, _ := readAll(t, l, limit, big, all); !slices.Equal(got, []uint64{4, 2, 1}) {
			t.Errorf("limit %d: read %v, want [4 2 1]", limit, got)
		}
	}
}

// TestReadBackDamagedRegionIsBounded: every line looked at counts toward the
// page's lines, damaged ones included, so a long damaged stretch is read a
// page at a time, each page going on where the last stopped.
func TestReadBackDamagedRegionIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"ts":"t","seq":1,"event":"api"}` + "\n")
	for i := 0; i < 1000; i++ {
		switch i % 3 {
		case 0:
			b.WriteString("garbage\n")
		case 1:
			b.WriteString(`{"ts":"t","seq":` + "\n")
		default:
			b.WriteString(`{"ts":"t","seq":"x"}` + "\n")
		}
	}
	b.WriteString(`{"ts":"t","seq":2,"event":"api"}` + "\n")
	path := filepath.Join(t.TempDir(), FileName)
	writeFile(t, path, b.String())
	l, err := Open(path, 1<<30, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var got []uint64
	from, pages := Cursor{}, 0
	for {
		pages++
		looked := 0
		next, more, err := l.ReadBack(from, 10, Budget{Lines: 10}, func(_ Cursor, seq uint64, _ []byte) bool {
			looked++
			got = append(got, seq)
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		if looked > 10 {
			t.Fatalf("page %d took %d", pages, looked)
		}
		if !more {
			break
		}
		if next == from {
			t.Fatal("no progress")
		}
		from = next
	}
	// 1002 lines, ten a page.
	if !slices.Equal(got, []uint64{2, 1}) || pages != 101 {
		t.Errorf("read %v in %d pages, want [2 1] in 101", got, pages)
	}
}

// TestReadBackOversizeIsBounded: a line far longer than MaxLineBytes is
// read under the page's byte budget: the page stops inside it, and the next
// goes on there, until the records older than it are reached.
func TestReadBackOversizeIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"ts":"t","seq":1,"event":"api"}` + "\n")
	b.WriteString(`{"ts":"t","seq":99999,"x":"` + strings.Repeat("a", 5*MaxLineBytes) + "\"}\n")
	b.WriteString(strings.Repeat("z", 3*MaxLineBytes) + "\n") // no JSON at all
	b.WriteString(`{"ts":"t","seq":3,"event":"api"}` + "\n")
	path := filepath.Join(t.TempDir(), FileName)
	writeFile(t, path, b.String())
	l, err := Open(path, 1<<30, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got, pages := readAll(t, l, 10, Budget{Lines: 100, Bytes: MinBudgetBytes}, all)
	if !slices.Equal(got, []uint64{3, 1}) {
		t.Errorf("read %v, want [3 1]", got)
	}
	if pages < 6 {
		t.Errorf("%d pages: the byte budget did not bound the pages", pages)
	}
}

// TestReadBackOversizeRecordIsNeverTaken: a completed line longer than
// MaxLineBytes is never handed to the visitor, whatever seq it names, so
// paging over it neither repeats nor misses a record (the case the review
// found: [3, 99999] and then 3 again).
func TestReadBackOversizeRecordIsNeverTaken(t *testing.T) {
	type tc struct {
		size int
		seq  string
	}
	var cases []tc
	for _, size := range []int{MaxLineBytes + 1, MaxLineBytes + readChunk/2, 2 * MaxLineBytes} {
		// 2 sits in order between 1 and 3: only the length check keeps it
		// out. 99999 is the review's case.
		cases = append(cases, tc{size, "2"}, tc{size, "99999"})
	}
	cases = append(cases, tc{MaxLineBytes - 100, "99999"})
	for _, c := range cases {
		size := c.size
		var b strings.Builder
		b.WriteString(`{"ts":"t","seq":1,"event":"api"}` + "\n")
		pad := size - len(`{"ts":"t","seq":,"x":""}`) - len(c.seq)
		b.WriteString(`{"ts":"t","seq":` + c.seq + `,"x":"` + strings.Repeat("a", pad) + "\"}\n")
		b.WriteString(`{"ts":"t","seq":3,"event":"api"}` + "\n")
		path := filepath.Join(t.TempDir(), FileName)
		writeFile(t, path, b.String())
		l, err := Open(path, 1<<30, 1)
		if err != nil {
			t.Fatal(err)
		}
		// A line the reader does take (the first size) names a seq newer
		// than the record read before it, so it is passed over too.
		for _, limit := range []int{1, 2, 10} {
			if got, _ := readAll(t, l, limit, big, all); !slices.Equal(got, []uint64{3, 1}) {
				t.Errorf("size %d seq %s, limit %d: read %v, want [3 1]", size, c.seq, limit, got)
			}
		}
		_ = l.Close()
	}
}

// TestReadBackDamagedLines: lines that are not records (a cut-short line, a
// line too long, garbage) are passed over, from any record's place.
func TestReadBackDamagedLines(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var b strings.Builder
	for seq := uint64(1); seq <= 400; seq++ {
		switch {
		case seq == 150 || seq == 300:
			b.WriteString(`{"ts":"t","seq":99999,"x":"` + strings.Repeat("a", MaxLineBytes) + "\"}\n") // too long
		case rng.Intn(10) == 0:
			b.WriteString(`{"ts":"t","seq":` + "\n") // cut short
		case rng.Intn(10) == 0:
			b.WriteString("garbage\n")
		}
		fmt.Fprintf(&b, `{"ts":"t","seq":%d,"event":"api"}`+"\n", seq)
	}
	path := filepath.Join(t.TempDir(), FileName)
	writeFile(t, path, b.String())
	l, err := Open(path, 1<<30, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.seq != 400 {
		t.Fatalf("Open read the last seq as %d, want 400", l.seq)
	}
	at := map[uint64]Cursor{}
	got, _ := readAll(t, l, 1000, big, all)
	if !slices.Equal(got, descending(400)) {
		t.Fatalf("read %v", got)
	}
	if _, _, err := l.ReadBack(Cursor{}, 1000, big, func(c Cursor, seq uint64, _ []byte) bool { at[seq] = c; return true }); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []uint64{1, 2, 149, 150, 151, 299, 300, 301, 400} {
		got, _ := readAllFrom(t, l, at[seq], 7, Budget{Lines: 5}, all)
		if !slices.Equal(got, descending(int(seq-1))) {
			t.Fatalf("from seq %d: read %v", seq, got)
		}
	}
}

// TestParseCursor: only a cursor String made is read back.
func TestParseCursor(t *testing.T) {
	c := Cursor{Seq: 7, head: "0123456789abcdef", headLen: 64, off: 1234, mid: true}
	if got, ok := ParseCursor(c.String()); !ok || got != c {
		t.Fatalf("round trip: %+v %v", got, ok)
	}
	if (Cursor{}).String() != "" {
		t.Error("the zero cursor has a form")
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, s := range []string{
		"", "x", enc("a1:5"), enc("a2:7:0123456789abcdef:64:1234"), enc("a2:7:0123456789abcdef:65:1234:0"),
		enc("a2:7:0123456789abcdef:0:1234:0"), enc("a2:7:0123456789ABCDEF:64:1234:0"), enc("a2:7:0123456789abcde:64:1234:0"),
		enc("a2:07:0123456789abcdef:64:1234:0"), enc("a2:7:0123456789abcdef:64:-1:0"), enc("a2:7:0123456789abcdef:64:1:2"),
	} {
		if _, ok := ParseCursor(s); ok {
			t.Errorf("%q was read as a cursor", s)
		}
	}
}

// TestReadBackNilLog: a log that is off reads as empty.
func TestReadBackNilLog(t *testing.T) {
	var l *Log
	next, more, err := l.ReadBack(Cursor{}, 10, big, func(Cursor, uint64, []byte) bool { t.Fatal("visited"); return false })
	if !next.IsZero() || more || err != nil {
		t.Fatalf("next %+v more %v err %v", next, more, err)
	}
}

// TestShedEventShape pins the lifecycle and delivery lines: the shed by
// name, id and whose it was, flat; the delivery's outcome under "delivery".
func TestShedEventShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ref := ShedRef{Shed: "web", ShedID: "abcdefghijklmnopqrstuvwxyz", ShedUser: "alice"}
	if err := l.Record(ShedEvent{Event: EventShedStop, Identity: UserIdentity("root", false, true, false, "SHA256:x", ""), ShedRef: ref, Result: ResultOK}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(DeliveryEvent{Event: EventDelivery, Identity: UserIdentity("alice", false, false, false, "", ""), ShedRef: ref,
		Delivery: DeliveryOutcome{Reason: "create", State: "delivered", Outcome: "accepted", Items: []DeliveryItem{{Name: "k", Provider: "ssh-key", Stage: 1, Kind: "file", Result: "ok", Fingerprint: "fp"}}}}); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var stop, del map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &stop)
	_ = json.Unmarshal([]byte(lines[1]), &del)
	delete(stop, "ts")
	delete(del, "ts")
	wantStop := `{"event":"shed.stop","identity":{"admin":true,"kind":"user","script":false,"subject":"SHA256:x","user":"root"},"result":"ok","seq":1,"shed":"web","shed_id":"abcdefghijklmnopqrstuvwxyz","shed_user":"alice"}`
	if got, _ := json.Marshal(stop); string(got) != wantStop {
		t.Errorf("stop line = %s\nwant %s", got, wantStop)
	}
	wantDel := `{"delivery":{"items":[{"fingerprint":"fp","kind":"file","name":"k","provider":"ssh-key","result":"ok","stage":1}],"outcome":"accepted","reason":"create","state":"delivered"},"event":"delivery","identity":{"admin":false,"kind":"user","script":false,"user":"alice"},"seq":2,"shed":"web","shed_id":"abcdefghijklmnopqrstuvwxyz","shed_user":"alice"}`
	if got, _ := json.Marshal(del); string(got) != wantDel {
		t.Errorf("delivery line = %s\nwant %s", got, wantDel)
	}
	for _, k := range ShedEventKinds {
		if !IsShedEventKind(k) {
			t.Errorf("%s is not a shed event kind", k)
		}
	}
	for _, k := range []string{EventAPI, EventExecEnd, EventReap, EventSSH, EventConnect, ""} {
		if IsShedEventKind(k) {
			t.Errorf("%q is a shed event kind; the older kinds carry no shed user", k)
		}
	}
}
