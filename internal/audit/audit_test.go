package audit

import (
	"bufio"
	"context"
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
		Event: EventExecEnd, Identity: UserIdentity("", true, true, false, "", ""), Shed: "s",
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

// TestIdentityShapes pins each identity kind's JSON exactly, as a reader of
// audit.jsonl sees it: a client keeps its shape (name and serial, nothing
// else), a person always carries both flags, the implicit user carries no
// name, and the server's own actions carry the kind alone. No constructor
// makes the retired kind "admin".
func TestIdentityShapes(t *testing.T) {
	tests := []struct {
		name string
		id   Identity
		want string
	}{
		{"client", ClientIdentity("build-1", "3fa2"), `{"kind":"client","name":"build-1","serial":"3fa2"}`},
		{"client without serial", ClientIdentity("build-1", ""), `{"kind":"client","name":"build-1"}`},
		{
			"user", UserIdentity("pat", false, false, false, "SHA256:abc", "1f"),
			`{"kind":"user","user":"pat","admin":false,"script":false,"subject":"SHA256:abc","serial":"1f"}`,
		},
		{
			"admin user", UserIdentity("admin", false, true, false, "SHA256:abc", "1f"),
			`{"kind":"user","user":"admin","admin":true,"script":false,"subject":"SHA256:abc","serial":"1f"}`,
		},
		{
			"script key", UserIdentity("ci", false, false, true, "SHA256:abc", ""),
			`{"kind":"user","user":"ci","admin":false,"script":true,"subject":"SHA256:abc"}`,
		},
		{
			// An SSH login has a fingerprint and no certificate serial.
			"user over ssh", UserIdentity("pat", false, true, false, "SHA256:abc", ""),
			`{"kind":"user","user":"pat","admin":true,"script":false,"subject":"SHA256:abc"}`,
		},
		{"implicit, open mode", UserIdentity("", true, true, false, "", ""), `{"kind":"user","implicit":true,"admin":true,"script":false}`},
		{
			"implicit with a certificate", UserIdentity("", true, true, false, "SHA256:abc", "1f"),
			`{"kind":"user","implicit":true,"admin":true,"script":false,"subject":"SHA256:abc","serial":"1f"}`,
		},
		{
			// The implicit user has no name; one passed by mistake is dropped.
			"implicit drops a name", UserIdentity("pat", true, true, false, "", ""),
			`{"kind":"user","implicit":true,"admin":true,"script":false}`,
		},
		{"system", SystemIdentity(), `{"kind":"system"}`},
		{"unknown", UnknownIdentity(), `{"kind":"unknown"}`},
	}
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.id)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("identity = %s, want %s", got, tt.want)
			}
			if err := l.Record(ExecEndEvent{Event: EventExecEnd, Identity: tt.id, Shed: tt.name}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// Round trip: each line read back holds the identity as written.
	lines := readLines(t, path)
	if len(lines) != len(tests) {
		t.Fatalf("want %d lines, got %d", len(tests), len(lines))
	}
	for i, tt := range tests {
		var want map[string]any
		if err := json.Unmarshal([]byte(tt.want), &want); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(lines[i]["identity"])
		wantJSON, _ := json.Marshal(want)
		if string(got) != string(wantJSON) {
			t.Errorf("%s: line identity = %s, want %s", tt.name, got, wantJSON)
		}
		if lines[i]["identity"].(map[string]any)["kind"] == "admin" {
			t.Errorf("%s: a line carries the retired kind admin", tt.name)
		}
	}
}

func TestReapEventIdentity(t *testing.T) {
	got, err := json.Marshal(ReapEvent{Event: EventReap, Identity: SystemIdentity(), Shed: "s", Owner: "build-1", AgeSeconds: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"event":"reap","identity":{"kind":"system"},"shed":"s","owner":"build-1","age_seconds":3}`
	if string(got) != want {
		t.Errorf("reap = %s, want %s", got, want)
	}
}

// TestSSHEventShape pins the ssh line exactly: a person's identity with the
// key's fingerprint and no serial, the peer, the shed and its owner, the
// channel, and port only for a tunnel. It has no member for a command.
func TestSSHEventShape(t *testing.T) {
	id := UserIdentity("admin", false, true, false, "SHA256:abc", "")
	tests := []struct {
		name string
		ev   SSHEvent
		want string
	}{
		{
			"exec",
			SSHEvent{Event: EventSSH, Identity: id, Remote: "127.0.0.1:5123", Shed: "ci-1", Owner: "build-1", Channel: SSHChannelExec},
			`{"event":"ssh","identity":{"kind":"user","user":"admin","admin":true,"script":false,"subject":"SHA256:abc"},"remote":"127.0.0.1:5123","shed":"ci-1","owner":"build-1","channel":"exec"}`,
		},
		{
			"tunnel",
			SSHEvent{Event: EventSSH, Identity: id, Remote: "127.0.0.1:5123", Shed: "ci-1", Owner: "build-1", Channel: SSHChannelTunnel, Port: 8080},
			`{"event":"ssh","identity":{"kind":"user","user":"admin","admin":true,"script":false,"subject":"SHA256:abc"},"remote":"127.0.0.1:5123","shed":"ci-1","owner":"build-1","channel":"tunnel","port":8080}`,
		},
		{
			"implicit shell",
			SSHEvent{Event: EventSSH, Identity: UserIdentity("", true, true, false, "SHA256:abc", ""), Remote: "127.0.0.1:5123", Shed: "ci-1", Owner: "build-1", Channel: SSHChannelShell},
			`{"event":"ssh","identity":{"kind":"user","implicit":true,"admin":true,"script":false,"subject":"SHA256:abc"},"remote":"127.0.0.1:5123","shed":"ci-1","owner":"build-1","channel":"shell"}`,
		},
	}
	path := filepath.Join(t.TempDir(), FileName)
	l, err := Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		got, err := json.Marshal(tt.ev)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tt.want {
			t.Errorf("%s: ssh = %s, want %s", tt.name, got, tt.want)
		}
		if err := l.Record(tt.ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, path)
	if len(lines) != len(tests) {
		t.Fatalf("want %d lines, got %d", len(tests), len(lines))
	}
	for i, tt := range tests {
		var want map[string]any
		if err := json.Unmarshal([]byte(tt.want), &want); err != nil {
			t.Fatal(err)
		}
		want["ts"] = lines[i]["ts"]
		got, _ := json.Marshal(lines[i])
		wantJSON, _ := json.Marshal(want)
		if string(got) != string(wantJSON) {
			t.Errorf("%s: line = %s, want %s", tt.name, got, wantJSON)
		}
	}
}

// TestConnectEventShape pins the connect line exactly: a person as an HTTPS
// caller (with the certificate's subject and serial, or the implicit user
// with neither in open mode), the peer, the shed and its owner, and the port.
// It has no request id member: that value can come from a caller's
// X-Request-Id header.
func TestConnectEventShape(t *testing.T) {
	tests := []struct {
		name string
		ev   ConnectEvent
		want string
	}{
		{
			"a named admin",
			ConnectEvent{Event: EventConnect, Identity: UserIdentity("pat", false, true, false, "SHA256:abc", "1f"), Remote: "192.0.2.10:51234", Shed: "ci-1", Owner: "build-1", Port: 8080},
			`{"event":"connect","identity":{"kind":"user","user":"pat","admin":true,"script":false,"subject":"SHA256:abc","serial":"1f"},"remote":"192.0.2.10:51234","shed":"ci-1","owner":"build-1","port":8080}`,
		},
		{
			"the implicit user in open mode",
			ConnectEvent{Event: EventConnect, Identity: UserIdentity("", true, true, false, "", ""), Remote: "127.0.0.1:5123", Shed: "ci-1", Owner: "build-1", Port: 22},
			`{"event":"connect","identity":{"kind":"user","implicit":true,"admin":true,"script":false},"remote":"127.0.0.1:5123","shed":"ci-1","owner":"build-1","port":22}`,
		},
	}
	for _, tt := range tests {
		got, err := json.Marshal(tt.ev)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tt.want {
			t.Errorf("%s: connect = %s, want %s", tt.name, got, tt.want)
		}
	}
}

// TestPendingDrain: Drain waits for every entry begun before it, refuses any
// begun after it, and gives up (with an error naming how many) when its
// context ends first.
func TestPendingDrain(t *testing.T) {
	var p Pending
	end1, ok := p.Begin()
	if !ok {
		t.Fatal("Begin refused before Drain")
	}
	end2, _ := p.Begin()
	end2()
	end2() // a second end is a no-op

	drained := make(chan error, 1)
	go func() { drained <- p.Drain(context.Background()) }()
	select {
	case err := <-drained:
		t.Fatalf("Drain returned (%v) with an entry still begun", err)
	case <-time.After(100 * time.Millisecond):
	}
	// Once Drain has begun, a new entry is refused (one let through before
	// that is ended at once, so the wait is not held up by the probe).
	waitUntil(t, func() bool {
		end, ok := p.Begin()
		end()
		return !ok
	})
	end1()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return once every entry ended")
	}
	if _, ok := p.Begin(); ok {
		t.Error("Begin after Drain was let through")
	}

	var q Pending
	if _, ok := q.Begin(); !ok {
		t.Fatal("Begin refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := q.Drain(ctx); err == nil || !strings.Contains(err.Error(), "1 entries") {
		t.Errorf("Drain past its context = %v, want the count", err)
	}

	var empty Pending
	if err := empty.Drain(context.Background()); err != nil {
		t.Errorf("Drain with nothing begun = %v", err)
	}
}

// TestPendingCloseThenDrain: Close shuts the gate at once without waiting
// (an entry begun before it is still counted, and one after it refused);
// Waiting is closed only once a Drain waits on that entry; and a Drain whose
// context has already ended returns nil when no entry is left.
func TestPendingCloseThenDrain(t *testing.T) {
	var p Pending
	end, ok := p.Begin()
	if !ok {
		t.Fatal("Begin refused before Close")
	}
	p.Close()
	if _, ok := p.Begin(); ok {
		t.Fatal("Begin after Close was let through")
	}
	p.Close() // a second Close is a no-op
	select {
	case <-p.Waiting():
		t.Fatal("Waiting closed before any Drain waited")
	default:
	}

	drained := make(chan error, 1)
	go func() { drained <- p.Drain(context.Background()) }()
	select {
	case <-p.Waiting():
	case <-time.After(5 * time.Second):
		t.Fatal("Waiting was not closed while Drain waited on a begun entry")
	}
	select {
	case err := <-drained:
		t.Fatalf("Drain returned (%v) with an entry still begun", err)
	default:
	}
	end()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Drain = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Drain did not return once the entry ended")
	}

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Drain(ended); err != nil {
		t.Errorf("Drain past its context with nothing left = %v", err)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
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
