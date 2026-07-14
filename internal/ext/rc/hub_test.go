package rc

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// hubClock is a manually-advanced clock for hub tests (mirrors stability_test's
// fakeClock but exposed to the hub via now()).
type hubClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *hubClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *hubClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// hubTmux is a programmable tmux runner for the hub: `ls` returns the configured
// session names, and capture-pane/show-environment answer per-session from maps
// keyed by tmux session name. Safe for concurrent use (the reconcile goroutine and
// a test goroutine can both call it).
type hubTmux struct {
	mu     sync.Mutex
	names  []string          // rc-* (and other) session names for `ls`
	panes  map[string]string // tmux name → capture-pane stdout
	envs   map[string]string // tmux name → show-environment stdout
	gone   map[string]bool   // tmux name → capture-pane reports "can't find pane"
	flaky  map[string]bool   // tmux name → capture-pane fails TRANSIENTLY (not gone)
	lsFail string            // non-empty → `ls` fails transiently with this stderr
	sent   []string          // recorded delivery payloads (send-keys -l / set-buffer text)
}

func newHubTmux() *hubTmux {
	return &hubTmux{
		panes: map[string]string{}, envs: map[string]string{},
		gone: map[string]bool{}, flaky: map[string]bool{},
	}
}

// setLsFail makes `ls` fail transiently (stderr must not read as "no server").
func (f *hubTmux) setLsFail(stderr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lsFail = stderr
}

// setFlaky makes a session's capture-pane fail transiently (a tmux hiccup, NOT gone).
func (f *hubTmux) setFlaky(name string, flaky bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flaky[name] = flaky
}

func (f *hubTmux) set(name, pane, env string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Contains(f.names, name) {
		f.names = append(f.names, name)
	}
	f.panes[name] = pane
	f.envs[name] = env
	delete(f.gone, name)
}

func (f *hubTmux) remove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.names[:0]
	for _, n := range f.names {
		if n != name {
			out = append(out, n)
		}
	}
	f.names = out
	f.gone[name] = true
}

func (f *hubTmux) setPane(name, pane string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panes[name] = pane
}

func (f *hubTmux) Run(args ...string) Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "ls":
		if f.lsFail != "" {
			return Result{Code: 1, Stderr: f.lsFail}
		}
		return Result{Stdout: strings.Join(f.names, "\n")}
	case "capture-pane":
		name := targetOf(args)
		if f.gone[name] {
			return Result{Code: 1, Stderr: "can't find pane: " + name}
		}
		if f.flaky[name] {
			return Result{Code: 1, Stderr: "lost server connection (transient)"}
		}
		return Result{Stdout: f.panes[name]}
	case "show-environment":
		name := targetOf(args)
		return Result{Stdout: f.envs[name]}
	case "send-keys":
		// Record the literal text of a `send-keys -t <name> -l -- <text>` delivery
		// (the single-line paste path); the bare Enter submit is ignored.
		if payload, ok := literalSendKeys(args); ok {
			f.sent = append(f.sent, payload)
		}
		return Result{}
	case "set-buffer":
		// The multi-line bracketed-paste path loads the block into a buffer first.
		if i := indexOf(args, "--"); i >= 0 && i+1 < len(args) {
			f.sent = append(f.sent, args[i+1])
		}
		return Result{}
	case "paste-buffer":
		return Result{}
	}
	return Result{}
}

// literalSendKeys extracts the text of a literal `send-keys -l -- <text>` call.
func literalSendKeys(args []string) (string, bool) {
	hasL := false
	for _, a := range args {
		if a == "-l" {
			hasL = true
		}
	}
	if !hasL {
		return "", false
	}
	if i := indexOf(args, "--"); i >= 0 && i+1 < len(args) {
		return args[i+1], true
	}
	return "", false
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func (f *hubTmux) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func targetOf(args []string) string {
	for i, a := range args {
		if a == "-t" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// managedEnv builds a show-environment dump for a managed session of the given kind.
func managedEnv(id string, kind Kind) string {
	return strings.Join([]string{
		envV + "=2",
		envID + "=" + id,
		envKind + "=" + string(kind),
		envDisplayName + "=disp",
		envWorkdir + "=/home/shed",
	}, "\n") + "\n"
}

// newTestHub builds a hub wired to a fake tmux + clock and small intervals.
func newTestHub(f Runner, clk *hubClock) *Hub {
	return newHub(HubConfig{
		Runner:       f,
		Getenv:       func(string) string { return "" },
		Now:          clk.now,
		Logf:         func(string, ...any) {},
		QuietPeriod:  4 * time.Second,
		Heartbeat:    20 * time.Millisecond,
		WriteTimeout: time.Second,
	})
}

// drainedEvent is a decoded SSE frame (event name + raw JSON data line).
type drainedEvent struct {
	name string
	raw  string
}

// drainEvents decodes every buffered frame on a subscriber into (name, raw-json).
func drainEvents(sub *subscriber) []drainedEvent {
	var out []drainedEvent
	for {
		select {
		case frame := <-sub.ch:
			s := string(frame)
			name := ""
			data := ""
			for _, line := range strings.Split(s, "\n") {
				if strings.HasPrefix(line, "event: ") {
					name = strings.TrimPrefix(line, "event: ")
				}
				if strings.HasPrefix(line, "data: ") {
					data = strings.TrimPrefix(line, "data: ")
				}
			}
			out = append(out, drainedEvent{name, data})
		default:
			return out
		}
	}
}

func countEvents(evs []drainedEvent, name string) int {
	n := 0
	for _, e := range evs {
		if e.name == name {
			n++
		}
	}
	return n
}

// ---- reconcile-loop transitions ----

func TestHubReconcileSessionAppearAndActivity(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	// A codex session appears, its composer churning (working).
	f.set("rc-aaa111", "boot >_ OpenAI Codex (v1.0)\nline", managedEnv("id-1", KindCodex))
	sub := h.subscribe() // attach so we can observe broadcast frames

	h.reconcile()
	evs := drainEvents(sub)
	if countEvents(evs, "session.updated") == 0 {
		t.Fatalf("expected session.updated on appear, got %+v", evs)
	}
	if countEvents(evs, "activity.changed") == 0 {
		t.Fatalf("expected activity.changed (working) on first tick, got %+v", evs)
	}
	// First activity is working.
	if got := hubActivityOf(t, h, "aaa111"); got != ActivityWorking {
		t.Fatalf("activity = %q, want working", got)
	}
}

func TestHubReconcileQuietAnchorNeedsInput(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	// codex parked at its composer placeholder (the prompt anchor) — a stable pane.
	pane := "> " + codexComposerPlaceholder + "\nother"
	f.set("rc-bbb222", pane, managedEnv("id-2", KindCodex))

	h.reconcile() // first tick: working (fresh session "just changed")
	if got := hubActivityOf(t, h, "bbb222"); got != ActivityWorking {
		t.Fatalf("tick1 activity = %q, want working", got)
	}

	// Pane unchanged; advance past the quiet period → needs_input (anchor matches).
	clk.advance(5 * time.Second)
	sub := h.subscribe()
	h.reconcile()
	if got := hubActivityOf(t, h, "bbb222"); got != ActivityNeedsInput {
		t.Fatalf("tick2 activity = %q, want needs_input", got)
	}
	evs := drainEvents(sub)
	if countEvents(evs, "activity.changed") == 0 {
		t.Fatalf("expected activity.changed on working→needs_input, got %+v", evs)
	}
}

func TestHubReconcileQuietNoAnchorIdle(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	// A shell session with a non-prompt line (no anchor match) that holds still.
	f.set("rc-ccc333", "some output line", managedEnv("id-3", KindShell))
	h.reconcile() // working
	clk.advance(5 * time.Second)
	h.reconcile() // quiet, no anchor → idle
	if got := hubActivityOf(t, h, "ccc333"); got != ActivityIdle {
		t.Fatalf("activity = %q, want idle", got)
	}
}

func TestHubReconcileDisappearEmitsGone(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	f.set("rc-ddd444", "boot >_ OpenAI Codex (v1.0)", managedEnv("id-4", KindCodex))
	h.reconcile()
	sub := h.subscribe()

	f.remove("rc-ddd444")
	h.reconcile()
	evs := drainEvents(sub)
	found := false
	for _, e := range evs {
		if e.name == "session.updated" && strings.Contains(e.raw, `"session":null`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected session.updated with session:null on disappear, got %+v", evs)
	}
	h.trackMu.Lock()
	_, stillTracked := h.tracked["ddd444"]
	h.trackMu.Unlock()
	if stillTracked {
		t.Fatal("disappeared session should be dropped from tracked")
	}
}

// A transient tmux listing failure must not read as "every session is gone": the
// reconcile pass is skipped wholesale — tracked state, rings, and watchers survive,
// no session-gone events fire, and the idle-exit clock does not start. A genuine
// "no server running" answer (killing the last session stops the tmux server) still
// reads as everything-gone.
func TestHubReconcileSkipsOnTransientListFailure(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	f.set("rc-lsf111", "boot >_ OpenAI Codex (v1.0)", managedEnv("id-lsf", KindCodex))
	h.reconcile()
	h.trackMu.Lock()
	tr := h.tracked["lsf111"]
	h.trackMu.Unlock()
	if tr == nil {
		t.Fatal("precondition: session tracked")
	}
	tr.ring.append(textMsg("kept"), clk.now())

	sub := h.subscribe()
	f.setLsFail("error connecting to /tmp/tmux-1000/default (transient)")
	h.reconcile() // must be a no-op pass

	if evs := drainEvents(sub); len(evs) != 0 {
		t.Fatalf("a skipped pass must emit no events, got %+v", evs)
	}
	h.trackMu.Lock()
	tr2, still := h.tracked["lsf111"]
	idleStarted := !h.idleSince.IsZero()
	h.trackMu.Unlock()
	if !still || tr2 != tr {
		t.Fatal("tracked state must survive a transient listing failure")
	}
	if msgs, _ := tr2.ring.since(0, 10); len(msgs) != 1 {
		t.Fatal("the session ring must survive a transient listing failure")
	}
	if idleStarted {
		t.Fatal("a skipped pass must not start the idle-exit clock")
	}

	// The failure clears → normal reconcile resumes with the same tracked entry.
	f.setLsFail("")
	h.reconcile()
	h.trackMu.Lock()
	tr3 := h.tracked["lsf111"]
	h.trackMu.Unlock()
	if tr3 != tr {
		t.Fatal("recovery must keep the same tracked entry (no reset)")
	}

	// Contrast: a genuine "no server running" answer IS everything-gone.
	f.setLsFail("no server running on /tmp/tmux-1000/default")
	h.reconcile()
	h.trackMu.Lock()
	_, stillTracked := h.tracked["lsf111"]
	h.trackMu.Unlock()
	if stillTracked {
		t.Fatal("a no-server answer must drop the tracked session (genuinely gone)")
	}
}

func TestHubReconcileStateChangeEmitsSessionUpdated(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	// Starts "starting" (blank-ish codex boot), then reaches ready.
	f.set("rc-eee555", "booting", managedEnv("id-5", KindCodex))
	h.reconcile()
	sub := h.subscribe()

	f.setPane("rc-eee555", "boot >_ OpenAI Codex (v1.0)")
	h.reconcile()
	evs := drainEvents(sub)
	if countEvents(evs, "session.updated") == 0 {
		t.Fatalf("expected session.updated on lifecycle state change, got %+v", evs)
	}
}

// hubActivityOf reads the tracked (displayed) activity for a slug.
func hubActivityOf(t *testing.T, h *Hub, slug string) Activity {
	t.Helper()
	h.trackMu.Lock()
	defer h.trackMu.Unlock()
	tr, ok := h.tracked[slug]
	if !ok {
		t.Fatalf("slug %q not tracked", slug)
	}
	return tr.activity
}

// Contract: activity.changed carries only valid non-empty activity values. A
// transition INTO suppression (ready → needs-auth) must emit session.updated (the
// state change) and NO activity.changed — never one with activity:"" (a strict
// decoder would reject it).
func TestHubNoActivityChangedOnSuppression(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	// Ready codex session → first tick reports working.
	f.set("rc-sup111", "boot >_ OpenAI Codex (v1.0)", managedEnv("id-sup", KindCodex))
	h.reconcile()
	if got := hubActivityOf(t, h, "sup111"); got != ActivityWorking {
		t.Fatalf("precondition: activity = %q, want working", got)
	}

	// The session drops to needs-auth (blocking lifecycle) → activity suppressed.
	sub := h.subscribe()
	f.setPane("rc-sup111", "Sign in with ChatGPT")
	h.reconcile()

	if got := hubActivityOf(t, h, "sup111"); got != "" {
		t.Fatalf("activity = %q, want suppressed", got)
	}
	evs := drainEvents(sub)
	if countEvents(evs, "session.updated") == 0 {
		t.Fatalf("expected session.updated for the ready→needs-auth transition, got %+v", evs)
	}
	if n := countEvents(evs, "activity.changed"); n != 0 {
		t.Fatalf("suppression must not emit activity.changed (got %d): %+v", n, evs)
	}
	for _, e := range evs {
		if strings.Contains(e.raw, `"activity":""`) {
			t.Fatalf("an event carried the empty activity value: %+v", e)
		}
	}
}

// Recreate detection must not depend on SHED_RC_ID alone: a legacy/partial session
// with no id but a created_at is re-pinned by created_at — a change means the slug
// was killed and recreated, so the tracker resets and session.updated fires.
func TestHubReconcileLegacyRecreateByCreatedAt(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	legacyEnv := func(createdAt string) string {
		return strings.Join([]string{
			envV + "=2", // managed, but no SHED_RC_ID stamped (older/partial creator)
			envKind + "=" + string(KindShell),
			envCreatedAt + "=" + createdAt,
		}, "\n") + "\n"
	}

	f.set("rc-leg111", "output A", legacyEnv("2026-01-01T00:00:00Z"))
	h.reconcile()
	sub := h.subscribe()

	// Same slug, still no id, NEW created_at → a recreate.
	f.set("rc-leg111", "output A", legacyEnv("2026-01-02T00:00:00Z"))
	h.reconcile()

	evs := drainEvents(sub)
	if countEvents(evs, "session.updated") == 0 {
		t.Fatalf("expected session.updated on a created_at-detected recreate, got %+v", evs)
	}
	h.trackMu.Lock()
	tr := h.tracked["leg111"]
	h.trackMu.Unlock()
	if tr == nil || tr.createdAt != "2026-01-02T00:00:00Z" {
		t.Fatalf("tracker not reset to the recreated identity: %+v", tr)
	}
}

// ---- lifecycle-trumps-activity precedence ----

func TestHubReconcileLifecycleTrumpsActivity(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)

	// A codex session that is not-signed-in → needs-auth lifecycle; activity must be
	// suppressed ("") even though the pane churns.
	f.set("rc-fff666", "Sign in with ChatGPT", managedEnv("id-6", KindCodex))
	h.reconcile()
	if got := hubActivityOf(t, h, "fff666"); got != "" {
		t.Fatalf("needs-auth session activity = %q, want suppressed", got)
	}
}

// ---- idle-exit with injected clock ----

func TestHubIdleExit(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newHub(HubConfig{
		Runner:      f,
		Getenv:      func(string) string { return "" },
		Now:         clk.now,
		Logf:        func(string, ...any) {},
		IdleTimeout: 15 * time.Minute,
	})

	// No sessions: idle clock starts on the first reconcile.
	h.reconcile()
	if h.shouldIdleExit(clk.now()) {
		t.Fatal("should not idle-exit immediately")
	}
	clk.advance(14 * time.Minute)
	if h.shouldIdleExit(clk.now()) {
		t.Fatal("should not idle-exit before the timeout")
	}
	clk.advance(2 * time.Minute)
	if !h.shouldIdleExit(clk.now()) {
		t.Fatal("should idle-exit after the timeout with zero sessions")
	}
}

func TestHubIdleClockResetsWhenSessionAppears(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newHub(HubConfig{
		Runner: f, Getenv: func(string) string { return "" }, Now: clk.now,
		Logf: func(string, ...any) {}, IdleTimeout: 15 * time.Minute,
	})

	h.reconcile() // zero sessions → idle clock starts
	clk.advance(20 * time.Minute)
	// A session appears before the exit decision is taken.
	f.set("rc-ggg777", "boot >_ OpenAI Codex (v1.0)", managedEnv("id-7", KindCodex))
	h.reconcile() // resets idle clock
	if h.shouldIdleExit(clk.now()) {
		t.Fatal("idle clock must reset once a session exists")
	}
}

// Subscribers must NOT block idle-exit: with zero sessions the hub exits even with
// a subscriber attached, and closing subscribers ends their streams.
func TestHubSubscribersDoNotBlockIdleExit(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newHub(HubConfig{
		Runner: f, Getenv: func(string) string { return "" }, Now: clk.now,
		Logf: func(string, ...any) {}, IdleTimeout: 15 * time.Minute,
	})
	sub := h.subscribe()
	h.reconcile()
	clk.advance(16 * time.Minute)
	if !h.shouldIdleExit(clk.now()) {
		t.Fatal("zero sessions + subscriber attached must still idle-exit")
	}
	h.closeAllSubscribers()
	select {
	case <-sub.closed:
	default:
		t.Fatal("closeAllSubscribers must close the subscriber's stream")
	}
}

// ---- SSE subscriber overflow drop ----

func TestHubBroadcastDropsOnOverflow(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newHub(HubConfig{
		Runner: f, Getenv: func(string) string { return "" }, Now: clk.now,
		Logf: func(string, ...any) {}, SubscriberBuffer: 4,
	})
	sub := h.subscribe()

	// Broadcast far more than the buffer without the subscriber reading — must never
	// block, and the excess must be dropped (counted).
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			h.broadcast(activityChangedEvent("s", ActivityWorking, "t", StateReady, "preview"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked on a full subscriber queue")
	}
	if sub.dropped.Load() == 0 {
		t.Fatal("expected dropped frames on overflow")
	}
	if len(sub.ch) != 4 {
		t.Fatalf("queue depth = %d, want 4 (buffer full, rest dropped)", len(sub.ch))
	}
}

// ---- loopback HTTP endpoints ----

func TestHubHTTPSessions(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	f.set("rc-hhh888", "> "+codexComposerPlaceholder, managedEnv("id-8", KindCodex))
	h.reconcile()

	srv := httptest.NewServer(h.handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body hubSessionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Sessions) != 1 || body.Sessions[0].Slug != "hhh888" {
		t.Fatalf("unexpected sessions: %+v", body.Sessions)
	}
	if body.Sessions[0].Activity != ActivityWorking {
		t.Fatalf("session activity = %q, want working overlaid", body.Sessions[0].Activity)
	}
}

func TestHubHTTPRoutesAndMethods(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	srv := httptest.NewServer(h.handler())
	defer srv.Close()

	cases := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/v1/sessions/x/messages", "", http.StatusNotFound},            // unknown slug
		{"POST", "/v1/sessions/x/input", `{"text":"hi"}`, http.StatusNotFound}, // unknown slug (valid body)
		{"POST", "/v1/sessions", "", http.StatusMethodNotAllowed},              // sessions is GET-only
		{"GET", "/v1/nope", "", http.StatusNotFound},                           // unknown path
	}
	for _, c := range cases {
		var body io.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		}
		req, _ := http.NewRequest(c.method, srv.URL+c.path, body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s %s: status = %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
}

// SSE stream: events are delivered and a heartbeat comment arrives on the interval.
func TestHubHTTPEventsStreamAndHeartbeat(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk) // Heartbeat = 20ms
	srv := httptest.NewServer(h.handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	// Wait until the server has registered the subscriber, then broadcast.
	waitFor(t, func() bool { return h.subscriberCount() == 1 })
	h.broadcast(activityChangedEvent("s1", ActivityWorking, "2026-01-01T00:00:00Z", StateReady, ""))

	reader := bufio.NewReader(resp.Body)
	sawEvent, sawHeartbeat := false, false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (!sawEvent || !sawHeartbeat) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "event: activity.changed") {
			sawEvent = true
		}
		if strings.HasPrefix(line, ": heartbeat") {
			sawHeartbeat = true
		}
	}
	if !sawEvent {
		t.Error("did not receive the broadcast activity.changed event")
	}
	if !sawHeartbeat {
		t.Error("did not receive a heartbeat comment")
	}
}

// A wedged client (connected but never reading) must not park the events handler
// forever: once the TCP buffers fill, the write deadline — which covers the Flush
// as well as the Write — fires, the handler returns, and the subscriber is removed
// (so the reconcile cadence can drop back to idle). This drives a REAL connection:
// only a real conn honors SetWriteDeadline.
func TestHubEventsWedgedClientUnsubscribes(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newHub(HubConfig{
		Runner: f, Getenv: func(string) string { return "" }, Now: clk.now,
		Logf:         func(string, ...any) {},
		Heartbeat:    time.Hour,              // heartbeats out of the picture
		WriteTimeout: 100 * time.Millisecond, // fast deadline for the test
	})
	srv := httptest.NewServer(h.handler())
	defer srv.Close()

	// Raw TCP client: send the request, then never read the response.
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /v1/events HTTP/1.1\r\nHost: hub\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.subscriberCount() == 1 })

	// Push large frames until the kernel buffers fill and the handler's write/flush
	// blocks, trips the deadline, and errors out. Keep broadcasting in the background
	// (drops once the queue is full — never blocks us).
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		big := strings.Repeat("x", 64*1024)
		for {
			select {
			case <-stop:
				return
			default:
				h.broadcast(hubEvent{name: "bulk", data: big})
			}
		}
	}()

	waitFor(t, func() bool { return h.subscriberCount() == 0 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// ---- bind-as-lock / EADDRINUSE / identity handshake ----

func TestBindHubListenerReportsAlreadyInUse(t *testing.T) {
	// Hold the port with a real listener, then a second bind must report already=true
	// (not an error) — the bind-as-lock signal RunHub then identity-verifies.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ln2, already, err := bindHubListener(ln.Addr().String())
	if err != nil {
		t.Fatalf("second bind returned error, want already=true: %v", err)
	}
	if ln2 != nil {
		ln2.Close()
		t.Fatal("second bind unexpectedly succeeded")
	}
	if !already {
		t.Fatal("second bind should report already-in-use")
	}
}

// startFakeHub serves a real hub handler on an ephemeral loopback port and returns
// its address (server torn down with the test).
func startFakeHub(t *testing.T, h *Hub) string {
	t.Helper()
	srv := httptest.NewServer(h.handler())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestHubRunHubSecondExitsZeroAgainstRealHub(t *testing.T) {
	// A real hub holds the port and answers /v1/health → a second RunHub verifies
	// the identity and returns nil (exit 0).
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	addr := startFakeHub(t, newTestHub(f, clk))

	err := RunHub(HubConfig{
		Runner: f, Getenv: func(string) string { return "" },
		Logf: func(string, ...any) {}, Addr: addr,
	})
	if err != nil {
		t.Fatalf("RunHub against a verified hub should return nil, got %v", err)
	}
}

func TestHubRunHubForeignListenerErrors(t *testing.T) {
	// The port is held by an HTTP server that is NOT a hub (404s /v1/health) →
	// RunHub must exit NON-zero with a clear error, not a silent success.
	foreign := httptest.NewServer(http.NotFoundHandler())
	defer foreign.Close()
	addr := strings.TrimPrefix(foreign.URL, "http://")

	err := RunHub(HubConfig{
		Runner: newHubTmux(), Getenv: func(string) string { return "" },
		Logf: func(string, ...any) {}, Addr: addr,
	})
	if err == nil {
		t.Fatal("RunHub against a foreign listener must error")
	}
	if !strings.Contains(err.Error(), "another process") {
		t.Fatalf("error should name the foreign-process cause, got: %v", err)
	}
}

func TestProbeHubIdentityForeignListenerFailsFast(t *testing.T) {
	// The detach parent's probe must not poll a foreign listener for the full
	// budget: it fails promptly with the clear held-by-another-process error.
	foreign := httptest.NewServer(http.NotFoundHandler())
	defer foreign.Close()
	addr := strings.TrimPrefix(foreign.URL, "http://")

	start := time.Now()
	err := probeHubIdentity(addr, 5*time.Second)
	if err == nil {
		t.Fatal("probeHubIdentity against a foreign listener must error")
	}
	if !strings.Contains(err.Error(), "another process") {
		t.Fatalf("error should name the foreign-process cause, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("foreign listener should fail fast, took %v", elapsed)
	}
}

func TestProbeHubIdentityVerifiedHubSucceeds(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	addr := startFakeHub(t, newTestHub(f, clk))
	if err := probeHubIdentity(addr, 3*time.Second); err != nil {
		t.Fatalf("probeHubIdentity against a real hub failed: %v", err)
	}
}

func TestQueryHubHealthNothingListening(t *testing.T) {
	// Grab a port and close it so nothing listens there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	isHub, err := queryHubHealth(addr, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected a dial error when nothing is listening")
	}
	if isHub {
		t.Fatal("nothing listening must not read as a hub")
	}
}

// ---- idle-exit handoff (create races the exit) ----

func TestHubIdleExitHandoffRespawnsWhenSessionsAppear(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	respawned := 0
	h := newHub(HubConfig{
		Runner: f, Getenv: func(string) string { return "" }, Now: clk.now,
		Logf: func(string, ...any) {}, IdleTimeout: 15 * time.Minute,
		Respawn: func() error { respawned++; return nil },
	})

	// Zero sessions for the whole idle window → the exit decision fires.
	h.reconcile()
	clk.advance(16 * time.Minute)
	if !h.shouldIdleExit(clk.now()) {
		t.Fatal("precondition: hub should have decided to idle-exit")
	}

	// The race: a create adds its tmux session AFTER the zero-check (its ensure-hub
	// probe was answered by this dying hub, so it skipped its own spawn).
	f.set("rc-race11", "some output", managedEnv("id-race", KindShell))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	h.idleExitHandoff(ln)

	if respawned != 1 {
		t.Fatalf("respawn invoked %d times, want 1 (session appeared during exit)", respawned)
	}
	// The bind was released BEFORE the re-check, so a fresh hub can take the port.
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port must be free after the handoff released the bind: %v", err)
	}
	ln2.Close()
}

func TestHubIdleExitHandoffNoRespawnWithoutSessions(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	respawned := 0
	h := newHub(HubConfig{
		Runner: f, Getenv: func(string) string { return "" }, Now: clk.now,
		Logf: func(string, ...any) {}, IdleTimeout: 15 * time.Minute,
		Respawn: func() error { respawned++; return nil },
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.idleExitHandoff(ln)
	if respawned != 0 {
		t.Fatalf("respawn invoked %d times, want 0 (still zero sessions)", respawned)
	}
}

func TestBindHubListenerFreePortSucceeds(t *testing.T) {
	ln, already, err := bindHubListener("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if already {
		t.Fatal("a free port must not report already-in-use")
	}
	if ln == nil {
		t.Fatal("expected a listener for a free port")
	}
	ln.Close()
}

// ---- stale pidfile is harmless ----

func TestHubStalePidfileHarmless(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	dir := hubDir(getenv)
	if dir != filepath.Join(home, hubDirName) {
		t.Fatalf("hubDir = %q, want %q", dir, filepath.Join(home, hubDirName))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-seed a stale pidfile pointing at an implausible pid.
	if err := os.WriteFile(filepath.Join(dir, hubPidName), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Writing our own pidfile over the stale one must succeed (advisory; overwritten)
	// and leave the current pid — the port bind, not the pidfile, is the lock.
	if err := writePidfile(dir); err != nil {
		t.Fatalf("writePidfile over a stale file failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, hubPidName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pidfile = %q, want current pid %d", strings.TrimSpace(string(got)), os.Getpid())
	}
}
