package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q is not JSON: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecordShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 11, 11, 3, 4, 5, 6, time.UTC)
	l.now = func() time.Time { return fixed }

	if err := l.Record(APIEvent{
		Event: EventAPI, RequestID: "r1",
		Identity: Identity{Kind: KindClient, Name: "nocturn", Serial: "3fa2"},
		Remote:   "100.1.2.3:5123", Method: "POST", Route: "/api/sheds/{name}/exec",
		Path: "/api/sheds/nocturn-t1/exec", Shed: "nocturn-t1", Status: 200, DurationMS: 12,
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(ExecEndEvent{
		Event: EventExecEnd, Identity: Identity{Kind: KindAdmin}, Shed: "s",
		Argv0: "craze", Argc: 6, ExitCode: -1, Disconnected: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(struct{}{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Record([]int{1}); err == nil {
		t.Error("a non-object record must be refused")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `{"ts":"2026-11-11T03:04:05.000000006Z","event":"api",`) {
		t.Errorf("ts must come first: %s", raw)
	}
	lines := readLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	api := lines[0]
	id := api["identity"].(map[string]any)
	if id["kind"] != "client" || id["name"] != "nocturn" || id["serial"] != "3fa2" {
		t.Errorf("identity = %v", id)
	}
	if _, ok := id["subject"]; ok {
		t.Error("a client identity has no subject")
	}
	for _, k := range []string{"request_id", "remote", "method", "route", "path", "shed", "status", "duration_ms"} {
		if _, ok := api[k]; !ok {
			t.Errorf("api line lacks %q: %v", k, api)
		}
	}
	exec := lines[1]
	if exec["exit_code"] != float64(-1) || exec["disconnected"] != true || exec["argv0"] != "craze" || exec["argc"] != float64(6) {
		t.Errorf("exec.end = %v", exec)
	}
	if _, ok := exec["argv"]; ok {
		t.Error("argv must be absent unless set")
	}
	if len(lines[2]) != 1 || lines[2]["ts"] == nil {
		t.Errorf("an empty object records only ts: %v", lines[2])
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	if err := l.Record(struct{}{}); err == nil {
		t.Error("Record after Close must fail")
	}
}

func TestNilLogRecordsNothing(t *testing.T) {
	var l *Log
	if err := l.Record(APIEvent{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if l.Path() != "" {
		t.Error("nil log has no path")
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	// Each line is about 60 bytes; 200 bytes holds three.
	l, err := Open(path, 200, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		if err := l.Record(ReapEvent{Event: EventReap, Shed: fmt.Sprintf("s%02d", i), Owner: "o"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{path, path + ".1", path + ".2"} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if st.Size() > 200 {
			t.Errorf("%s is %d bytes, over the 200-byte limit", p, st.Size())
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Errorf("only keep=2 rotated files may exist: %v", err)
	}
	// The newest records are in the current file, the older ones in .1, then .2.
	cur, one, two := readLines(t, path), readLines(t, path+".1"), readLines(t, path+".2")
	if last := cur[len(cur)-1]["shed"]; last != "s19" {
		t.Errorf("last record = %v, want s19", last)
	}
	shed := func(m map[string]any) string { return m["shed"].(string) }
	if shed(one[len(one)-1]) >= shed(cur[0]) || shed(two[len(two)-1]) >= shed(one[0]) {
		t.Errorf("rotated files out of order: .2=%v .1=%v cur=%v", two, one, cur)
	}

	// Reopening appends to the current file and keeps its size.
	l2, err := Open(path, 200, 2)
	if err != nil {
		t.Fatal(err)
	}
	before := len(readLines(t, path))
	if err := l2.Record(ReapEvent{Event: EventReap, Shed: "again"}); err != nil {
		t.Fatal(err)
	}
	_ = l2.Close()
	after := readLines(t, path)
	if len(after) != before+1 && len(after) != 1 {
		t.Errorf("reopen: %d lines before, %d after", before, len(after))
	}
}

func TestOversizedLineIsWrittenWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, 50, 1)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 200)
	for range 3 {
		if err := l.Record(ReapEvent{Event: EventReap, Shed: long}); err != nil {
			t.Fatal(err)
		}
	}
	_ = l.Close()
	if n := len(readLines(t, path)); n != 1 {
		t.Errorf("a line over the limit still goes in whole, alone: %d lines", n)
	}
	if n := len(readLines(t, path+".1")); n != 1 {
		t.Errorf(".1 holds the previous line: %d lines", n)
	}
}

func TestConcurrentRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, 4096, 50)
	if err != nil {
		t.Fatal(err)
	}
	const workers, each = 8, 100
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range each {
				if err := l.Record(APIEvent{Event: EventAPI, Shed: fmt.Sprintf("w%d-%d", w, i), Status: 200}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	_ = l.Close()

	seen := map[string]bool{}
	files, _ := filepath.Glob(path + "*")
	for _, f := range files {
		for _, m := range readLines(t, f) { // every line parses: no interleaving
			seen[m["shed"].(string)] = true
		}
	}
	if len(seen) != workers*each {
		t.Errorf("want %d distinct records across the files, got %d", workers*each, len(seen))
	}
}
