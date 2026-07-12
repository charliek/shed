package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// upstreamHub is a scripted stand-in for the per-shed upstream hub SSE the
// aggregator reads. Each open() hands the reader a pipe whose write end the test
// drives (push) or drops (drop); the read end closes when the reader's context is
// canceled, mirroring how a real http response body unblocks on ctx cancel.
type upstreamHub struct {
	mu        sync.Mutex
	writers   map[string][]*io.PipeWriter
	openCount atomic.Int32
	failing   atomic.Bool // when set, open() returns an error (hub down)
}

func newUpstreamHub() *upstreamHub {
	return &upstreamHub{writers: map[string][]*io.PipeWriter{}}
}

func (u *upstreamHub) open(ctx context.Context, shed string) (io.ReadCloser, error) {
	u.openCount.Add(1)
	if u.failing.Load() {
		return nil, errors.New("hub down")
	}
	pr, pw := io.Pipe()
	go func() {
		<-ctx.Done()
		_ = pw.CloseWithError(ctx.Err())
	}()
	u.mu.Lock()
	u.writers[shed] = append(u.writers[shed], pw)
	u.mu.Unlock()
	return pr, nil
}

// push writes a raw SSE frame to the most recent upstream connection for shed.
func (u *upstreamHub) push(shed, frame string) {
	u.mu.Lock()
	ws := u.writers[shed]
	var pw *io.PipeWriter
	if len(ws) > 0 {
		pw = ws[len(ws)-1]
	}
	u.mu.Unlock()
	if pw != nil {
		_, _ = pw.Write([]byte(frame))
	}
}

// drop closes every upstream connection for shed (simulating a hub disconnect).
func (u *upstreamHub) drop(shed string) {
	u.mu.Lock()
	ws := u.writers[shed]
	u.writers[shed] = nil
	u.mu.Unlock()
	for _, pw := range ws {
		_ = pw.Close()
	}
}

func newTestAgg(discover func(context.Context) []string, open func(context.Context, string) (io.ReadCloser, error)) *rcAggregator {
	return &rcAggregator{
		clients:        map[*aggClient]struct{}{},
		readers:        map[string]*shedReader{},
		rescanInterval: 20 * time.Millisecond,
		backoffInitial: 5 * time.Millisecond,
		backoffMax:     20 * time.Millisecond,
		discover:       discover,
		openUpstream:   open,
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// readFrame reads one broadcast frame from c within a bound, failing otherwise.
func readFrame(t *testing.T, c *aggClient) string {
	t.Helper()
	select {
	case f := <-c.ch:
		return string(f)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a broadcast frame")
		return ""
	}
}

func newTestClient(buf int) *aggClient {
	return &aggClient{ch: make(chan []byte, buf), closed: make(chan struct{})}
}

// TestAgg_BroadcastDropOnOverflow: a full client queue drops frames instead of
// blocking the aggregator. Run with -race.
func TestAgg_BroadcastDropOnOverflow(t *testing.T) {
	agg := newTestAgg(nil, nil)
	c := newTestClient(1)
	agg.clients[c] = struct{}{}
	for i := 0; i < 1000; i++ {
		agg.broadcast([]byte("frame")) // must never block
	}
	if got := len(c.ch); got != 1 {
		t.Fatalf("overflow client buffered %d, want exactly 1 (rest dropped)", got)
	}
}

// TestAgg_FanoutInjectsShed: the first client spins up an upstream reader; an
// upstream envelope event is re-broadcast with `shed` filled in.
func TestAgg_FanoutInjectsShed(t *testing.T) {
	u := newUpstreamHub()
	agg := newTestAgg(func(context.Context) []string { return []string{"s1"} }, u.open)
	c := newTestClient(16)
	agg.addClient(c)
	defer agg.removeClient(c)

	waitFor(t, func() bool { return u.openCount.Load() >= 1 }, "upstream reader to open")
	u.push("s1", "event: activity.changed\ndata: {\"slug\":\"abc\",\"activity\":\"working\",\"shed\":\"\"}\n\n")

	frame := readFrame(t, c)
	if !strings.Contains(frame, "event: activity.changed") || !strings.Contains(frame, `"shed":"s1"`) {
		t.Fatalf("re-broadcast frame missing event/shed: %q", frame)
	}
}

// TestAgg_SlowClientDoesNotStarveFast: a stalled slow client drops frames while a
// fast client still receives every event. Run with -race.
func TestAgg_SlowClientDoesNotStarveFast(t *testing.T) {
	u := newUpstreamHub()
	agg := newTestAgg(func(context.Context) []string { return []string{"s1"} }, u.open)
	fast := newTestClient(64)
	slow := newTestClient(1) // never drained
	agg.addClient(fast)
	agg.addClient(slow)
	defer agg.removeClient(fast)
	defer agg.removeClient(slow)

	waitFor(t, func() bool { return u.openCount.Load() >= 1 }, "upstream reader to open")
	const n = 10
	for i := 0; i < n; i++ {
		u.push("s1", "event: message.appended\ndata: {\"slug\":\"abc\",\"seq\":1}\n\n")
	}
	for i := 0; i < n; i++ {
		f := readFrame(t, fast)
		if !strings.Contains(f, `"shed":"s1"`) {
			t.Fatalf("fast client frame %d wrong: %q", i, f)
		}
	}
}

// TestAgg_UpstreamDropReconnect: an upstream disconnect emits a synthetic
// hub.unavailable and the reader reconnects (a second open).
func TestAgg_UpstreamDropReconnect(t *testing.T) {
	u := newUpstreamHub()
	agg := newTestAgg(func(context.Context) []string { return []string{"s1"} }, u.open)
	c := newTestClient(16)
	agg.addClient(c)
	defer agg.removeClient(c)

	waitFor(t, func() bool { return u.openCount.Load() >= 1 }, "first upstream open")
	u.drop("s1") // disconnect

	frame := readFrame(t, c)
	if !strings.Contains(frame, "event: hub.unavailable") || !strings.Contains(frame, `"shed":"s1"`) {
		t.Fatalf("expected hub.unavailable{shed:s1}, got %q", frame)
	}
	waitFor(t, func() bool { return u.openCount.Load() >= 2 }, "upstream reconnect")
}

// TestAgg_ShedStopped: a shed leaving the candidate set on rescan emits
// shed.stopped and tears down its reader.
func TestAgg_ShedStopped(t *testing.T) {
	u := newUpstreamHub()
	var present atomic.Bool
	present.Store(true)
	agg := newTestAgg(func(context.Context) []string {
		if present.Load() {
			return []string{"s1"}
		}
		return nil
	}, u.open)
	c := newTestClient(16)
	agg.addClient(c)
	defer agg.removeClient(c)

	waitFor(t, func() bool { return u.openCount.Load() >= 1 }, "upstream open")
	present.Store(false) // s1 leaves candidacy; the next rescan drops it

	frame := readFrame(t, c)
	if !strings.Contains(frame, "event: shed.stopped") || !strings.Contains(frame, `"shed":"s1"`) {
		t.Fatalf("expected shed.stopped{shed:s1}, got %q", frame)
	}
}

// TestAgg_ZeroClientTeardown: the last client leaving tears down all upstream
// readers (demand-driven).
func TestAgg_ZeroClientTeardown(t *testing.T) {
	u := newUpstreamHub()
	agg := newTestAgg(func(context.Context) []string { return []string{"s1"} }, u.open)
	c := newTestClient(16)
	agg.addClient(c)
	waitFor(t, func() bool { return u.openCount.Load() >= 1 }, "upstream open")

	agg.removeClient(c)

	agg.mu.Lock()
	nReaders := len(agg.readers)
	mgr := agg.managerCancel
	agg.mu.Unlock()
	if nReaders != 0 || mgr != nil {
		t.Fatalf("after zero clients: readers=%d managerCancel!=nil=%v, want 0/nil", nReaders, mgr != nil)
	}
}

// TestAgg_StaleManagerRescanDiscarded pins the manager-generation guard: a
// rescan carrying a stale generation (an old, just-canceled manager whose
// discover call was in flight when a new manager was installed) must NOT mutate
// the readers map — otherwise it would insert readers bound to its dead context
// that silently deliver no events. Only the CURRENT generation's rescan may.
func TestAgg_StaleManagerRescanDiscarded(t *testing.T) {
	u := newUpstreamHub()
	agg := newTestAgg(func(context.Context) []string { return []string{"s1"} }, u.open)

	// Simulate the interleaving directly: a current manager at generation 2 is
	// installed (managerCancel non-nil), and the OLD manager (generation 1) wins
	// the lock with its late rescan.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agg.mu.Lock()
	agg.managerCancel = func() {}
	agg.managerGen = 2
	agg.mu.Unlock()

	agg.rescan(ctx, 1) // stale generation — must be discarded
	agg.mu.Lock()
	nReaders := len(agg.readers)
	agg.mu.Unlock()
	if nReaders != 0 {
		t.Fatalf("stale-generation rescan mutated readers (%d entries), want 0", nReaders)
	}

	// A canceled-context rescan at the CURRENT generation is discarded too.
	deadCtx, deadCancel := context.WithCancel(context.Background())
	deadCancel()
	agg.rescan(deadCtx, 2)
	agg.mu.Lock()
	nReaders = len(agg.readers)
	agg.mu.Unlock()
	if nReaders != 0 {
		t.Fatalf("canceled-ctx rescan mutated readers (%d entries), want 0", nReaders)
	}

	// The current generation with a live context reconciles normally.
	agg.rescan(ctx, 2)
	agg.mu.Lock()
	_, ok := agg.readers["s1"]
	agg.mu.Unlock()
	if !ok {
		t.Fatal("current-generation rescan must install the reader")
	}
}

// TestRCEvents_MethodGuard: /api/rc/events is GET-only (405 on POST).
func TestRCEvents_MethodGuard(t *testing.T) {
	be := &rcFakeBackend{}
	srv := newRCServer(be)
	r := httptest.NewRequest(http.MethodPost, "/api/rc/events", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/rc/events = %d, want 405", w.Code)
	}
}

// TestRCEvents_Scope: control scope required — no token 401, credentials 403,
// control 200. The 200 stream is bounded by a short request context so the test
// doesn't block on the long-lived SSE loop.
func TestRCEvents_Scope(t *testing.T) {
	be := &rcFakeBackend{} // empty: discover finds no sheds, no upstreams
	srv, control, credentials := newSecureRCServer(t, be)

	call := func(token string, bound bool) int {
		ctx := context.Background()
		if bound {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
		}
		r := httptest.NewRequest(http.MethodGet, "/api/rc/events", nil).WithContext(ctx)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w.Code
	}
	if got := call("", false); got != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", got)
	}
	if got := call(credentials, false); got != http.StatusForbidden {
		t.Errorf("credentials token: got %d, want 403", got)
	}
	// Control token: the handler streams until the (bounded) context expires, then
	// returns with the already-committed 200.
	if got := call(control, true); got != http.StatusOK {
		t.Errorf("control token: got %d, want 200", got)
	}
}

// TestRewriteFrame_UntrustedData exercises rewriteFrame's untrusted-payload
// handling: only allowlisted events with a JSON OBJECT payload are forwarded (with
// `shed` injected). The critical case is `data: null` — json.Unmarshal leaves the
// map nil WITHOUT an error (golang/go#10411), so a guest hub could otherwise crash
// the server on obj["shed"] = ... ("assignment to entry in nil map").
func TestRewriteFrame_UntrustedData(t *testing.T) {
	agg := newTestAgg(nil, nil)
	cases := []struct {
		name  string
		event string
		data  string
		want  bool // whether a frame is produced
	}{
		{"valid object", "activity.changed", `{"slug":"abc","activity":"working"}`, true},
		{"null payload (nil-map panic vector)", "activity.changed", `null`, false},
		{"json number", "session.updated", `123`, false},
		{"json string", "message.appended", `"hi"`, false},
		{"json array", "activity.changed", `[1,2,3]`, false},
		{"invalid json", "activity.changed", `{not json`, false},
		{"non-allowlisted event", "hub.unavailable", `{"shed":"x"}`, false},
		{"empty data", "activity.changed", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Must never panic, even on the null/nil-map vector.
			frame, ok := agg.rewriteFrame("s1", tc.event, tc.data)
			if ok != tc.want {
				t.Fatalf("rewriteFrame ok = %v, want %v (frame=%q)", ok, tc.want, frame)
			}
			if ok && !strings.Contains(string(frame), `"shed":"s1"`) {
				t.Fatalf("forwarded frame missing injected shed: %q", frame)
			}
		})
	}
}

// TestAgg_OversizedEventDropped: a hostile upstream that streams many data: lines
// without a terminating blank line must not accumulate unboundedly — the cumulative
// per-event cap drops the partial event. The subsequent well-formed event still
// makes it through, proving the reader resynced rather than wedging.
func TestAgg_OversizedEventDropped(t *testing.T) {
	u := newUpstreamHub()
	agg := newTestAgg(func(context.Context) []string { return []string{"s1"} }, u.open)
	c := newTestClient(16)
	agg.addClient(c)
	defer agg.removeClient(c)

	waitFor(t, func() bool { return u.openCount.Load() >= 1 }, "upstream reader to open")

	// Stream an event whose data lines together exceed the per-event cap (each line
	// under the single-line limit), terminated by the blank line per SSE framing.
	var b strings.Builder
	b.WriteString("event: activity.changed\n")
	line := strings.Repeat("x", 60<<10) // 60 KiB per line, several lines > 256 KiB total
	for i := 0; i < 6; i++ {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n") // blank line terminates the (oversized, dropped) event
	u.push("s1", b.String())
	// Now a clean, small event on the same stream: it must be delivered (proving the
	// reader resynced past the dropped event rather than wedging).
	u.push("s1", "event: activity.changed\ndata: {\"slug\":\"ok\",\"activity\":\"idle\"}\n\n")

	frame := readFrame(t, c)
	if !strings.Contains(frame, `"slug":"ok"`) || !strings.Contains(frame, `"shed":"s1"`) {
		t.Fatalf("post-overflow resync frame wrong: %q", frame)
	}
}
