package rc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// codexReadyPane is a codex pane parked at its composer placeholder — classifies ready
// AND matches the codex prompt anchor (so the degraded idle+anchor input policy accepts).
func codexReadyPane() string { return "codex\n> " + codexComposerPlaceholder }

// ---- GET /v1/sessions/{slug}/messages ----

func TestHubHTTPMessagesPagingTruncatedAnd404(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	f.set("rc-msg111", codexReadyPane(), managedEnv("id-m", KindCodex))
	h.reconcile()

	// White-box: seed the tracked session's ring (the HTTP layer's job is paging, not
	// production — the codex-fold→ring path is covered by TestCodexFoldMessageMapping).
	h.trackMu.Lock()
	ring := h.tracked["msg111"].ring
	h.trackMu.Unlock()
	for i := 0; i < 5; i++ {
		ring.append(textMsg("m"), clk.now())
	}

	srv := httptest.NewServer(h.handler())
	defer srv.Close()

	// since=2 (exclusive) + limit=2 → seqs 3,4.
	var body hubMessagesResponse
	getJSON(t, srv.URL+"/v1/sessions/msg111/messages?since=2&limit=2", &body)
	if len(body.Messages) != 2 || body.Messages[0].Seq != 3 || body.Messages[1].Seq != 4 {
		t.Fatalf("page = %v, want seqs 3,4", seqsOf(body.Messages))
	}
	if body.Truncated {
		t.Error("in-ring since must not be truncated")
	}

	// Drop the head, then a fresh since=0 reports truncated.
	for i := 0; i < maxRingMessages+10; i++ {
		ring.append(textMsg("m"), clk.now())
	}
	var body2 hubMessagesResponse
	getJSON(t, srv.URL+"/v1/sessions/msg111/messages", &body2)
	if !body2.Truncated {
		t.Error("since=0 after drop-oldest must report truncated")
	}

	// Unknown slug → 404.
	resp, err := http.Get(srv.URL + "/v1/sessions/nope/messages")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown slug status = %d, want 404", resp.StatusCode)
	}

	// Malformed since → 400.
	resp2, err := http.Get(srv.URL + "/v1/sessions/msg111/messages?since=abc")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed since status = %d, want 400", resp2.StatusCode)
	}
}

func TestHubHTTPMessagesEmptyForKnownSlug(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	f.set("rc-empty1", codexReadyPane(), managedEnv("id-e", KindCodex))
	h.reconcile()

	srv := httptest.NewServer(h.handler())
	defer srv.Close()
	// A known slug with no feed messages yet → 200 with an empty (non-null) array.
	resp, err := http.Get(srv.URL + "/v1/sessions/empty1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw := readAll(t, resp)
	if !strings.Contains(raw, `"messages":[]`) {
		t.Errorf("empty page must encode [] not null: %s", raw)
	}
}

// ---- POST /v1/sessions/{slug}/input ----

// newInputHub builds a hub whose stability verdict has SETTLED (two reconciles across
// the quiet period): the input acceptance re-check runs the same watcher+stability
// merge as reconcile, and a first-tick stability is always `working` (a fresh session
// has "just changed"), which would 409 every post.
func newInputHub(t *testing.T, f *hubTmux, clk *hubClock) (*Hub, *httptest.Server) {
	t.Helper()
	h := newTestHub(f, clk)
	h.reconcile()
	clk.advance(5 * time.Second) // past newTestHub's 4s quiet period
	h.reconcile()
	srv := httptest.NewServer(h.handler())
	t.Cleanup(srv.Close)
	return h, srv
}

func postInput(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHubInputHappyPathReachesPane(t *testing.T) {
	prev := sendLineSettle
	sendLineSettle = 0
	t.Cleanup(func() { sendLineSettle = prev })

	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	f.set("rc-inp111", codexReadyPane(), managedEnv("id-i", KindCodex))
	_, srv := newInputHub(t, f, clk)

	resp := postInput(t, srv.URL+"/v1/sessions/inp111/input", `{"text":"hello there"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readAll(t, resp))
	}
	sent := f.recorded()
	if len(sent) != 1 || sent[0] != "hello there" {
		t.Fatalf("delivered payloads = %v, want [\"hello there\"]", sent)
	}
}

func TestHubInputMultilineUsesBracketedPaste(t *testing.T) {
	prev := sendLineSettle
	sendLineSettle = 0
	t.Cleanup(func() { sendLineSettle = prev })

	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	f.set("rc-inp222", codexReadyPane(), managedEnv("id-i2", KindCodex))
	_, srv := newInputHub(t, f, clk)

	resp := postInput(t, srv.URL+"/v1/sessions/inp222/input", `{"text":"line one\nline two"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The multi-line block is delivered as one buffered bracketed paste.
	sent := f.recorded()
	if len(sent) != 1 || sent[0] != "line one\nline two" {
		t.Fatalf("bracketed-paste payload = %v, want the multi-line block", sent)
	}
}

// The degraded idle+anchor policy: with no JSONL watcher (the tail is absent/broken),
// a fresh pane showing the composer anchor is accepted — the documented degraded path.
func TestHubInputDegradedIdleAnchorAccepts(t *testing.T) {
	prev := sendLineSettle
	sendLineSettle = 0
	t.Cleanup(func() { sendLineSettle = prev })

	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	f.set("rc-deg111", codexReadyPane(), managedEnv("id-d", KindCodex))
	_, srv := newInputHub(t, f, clk)

	// The hub's getenv returns "" → no ~/.codex root → no watcher correlated: the only
	// acceptance signal is the composer anchor on the fresh pane.
	resp := postInput(t, srv.URL+"/v1/sessions/deg111/input", `{"text":"go"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("degraded idle+anchor must accept, got %d", resp.StatusCode)
	}
}

func TestHubInputErrorStatuses(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	f.set("rc-err111", codexReadyPane(), managedEnv("id-e", KindCodex))
	_, srv := newInputHub(t, f, clk)
	base := srv.URL + "/v1/sessions/err111/input"

	cases := []struct {
		name, url, body string
		want            int
	}{
		{"invalid json", base, `{not json`, http.StatusBadRequest},
		{"empty text", base, `{"text":"   "}`, http.StatusBadRequest},
		{"unsafe control char", base, `{"text":"a\u001bb"}`, http.StatusBadRequest},
		{"unknown slug", srv.URL + "/v1/sessions/ghost/input", `{"text":"hi"}`, http.StatusNotFound},
		{"too large", base, `{"text":"` + strings.Repeat("x", 17*1024) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := postInput(t, c.url, c.body)
			resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
}

// 409 when the session is not waiting for input (a churning, non-anchor pane).
func TestHubInputNotAcceptingIs409(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	// Ready codex (boot banner classifies ready) but NOT parked at the composer anchor.
	f.set("rc-na111", "boot >_ OpenAI Codex (v1.0)\nworking...", managedEnv("id-na", KindCodex))
	_, srv := newInputHub(t, f, clk)

	resp := postInput(t, srv.URL+"/v1/sessions/na111/input", `{"text":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("non-waiting session status = %d, want 409", resp.StatusCode)
	}
}

// 409 when the pane state FLIPS between the tracked snapshot and the locked re-check:
// tracked while parked at the anchor, but the fresh capture (under the input mutex)
// sees a churning pane → rejected.
func TestHubInputRaceStateFlipIs409(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	f.set("rc-race22", codexReadyPane(), managedEnv("id-r", KindCodex))
	_, srv := newInputHub(t, f, clk) // reconcile tracked it parked at the anchor

	// The pane flips to a churning, non-anchor state before the POST's locked re-check.
	f.setPane("rc-race22", "boot >_ OpenAI Codex (v1.0)\nnow working")

	resp := postInput(t, srv.URL+"/v1/sessions/race22/input", `{"text":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("state-flip status = %d, want 409", resp.StatusCode)
	}
}

// 409 when the slug was recreated (identity changed) since it was tracked.
func TestHubInputIdentityGuardIs409(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	f.set("rc-idg111", codexReadyPane(), managedEnv("id-old", KindCodex))
	_, srv := newInputHub(t, f, clk) // tracked with id-old

	// A new incarnation takes the same slug (different SHED_RC_ID) without a reconcile,
	// so the tracked identity is stale — the locked re-check must reject.
	f.set("rc-idg111", codexReadyPane(), managedEnv("id-new", KindCodex))

	resp := postInput(t, srv.URL+"/v1/sessions/idg111/input", `{"text":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("identity-guard status = %d, want 409", resp.StatusCode)
	}
}

// A non-input-gated kind (opencode) is not accepted even when parked at its anchor —
// feed input is codex-only this phase.
func TestHubInputNonGatedKindIs409(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	f.set("rc-ng111", "opencode ready", managedEnv("id-ng", KindOpencode))
	_, srv := newInputHub(t, f, clk)

	resp := postInput(t, srv.URL+"/v1/sessions/ng111/input", `{"text":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("non-gated kind status = %d, want 409", resp.StatusCode)
	}
}

// inputAccepted's watcher branch: a FRESH JSONL verdict wins over the pane anchor —
// needs_input accepts even on a non-anchor pane; working rejects even on an anchor pane
// (delivering mid-turn would interleave input).
func TestHubInputAcceptedWatcherBranch(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	dir := t.TempDir()

	// A settled turn (task_complete) → the fold reports needs_input, authoritative even
	// on a pane with no composer anchor (stability's verdict is irrelevant).
	settled := dir + "/settled.jsonl"
	writeFile(t, settled, `{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done"}}`+"\n")
	wSettled := newFileWatcher(settled, true, newCodexFold())
	wSettled.refresh(clk.now())
	if !h.inputAccepted(wSettled, ActivityWorking, KindCodex, "no anchor here") {
		t.Error("a fresh needs_input watcher must accept regardless of the pane anchor")
	}

	// An open tool call (no output) → working; must reject even when the pane shows the
	// composer anchor.
	working := dir + "/working.jsonl"
	writeFile(t, working, `{"type":"event_msg","payload":{"type":"task_started"}}`+"\n"+
		`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec"}}`+"\n")
	wWorking := newFileWatcher(working, true, newCodexFold())
	wWorking.refresh(clk.now())
	if h.inputAccepted(wWorking, ActivityWorking, KindCodex, codexReadyPane()) {
		t.Error("a fresh working watcher must reject even with the composer anchor present")
	}
}

// The long-quiet-working case (the handler must not be weaker than the reconcile
// merge): a working verdict whose file has been quiet past the 120s grace is
// EXPIRED-working — with an unsettled stability it still merges to working and must
// reject even with the composer anchor on the pane (a >120s tool call is still a
// live turn). Only a SETTLED quiet stability (the pane genuinely stopped) releases
// it to the anchor path.
func TestHubInputLongQuietWorkingRejected(t *testing.T) {
	f := newHubTmux()
	start := time.Unix(1_700_000_000, 0).UTC()
	clk := &hubClock{t: start}
	h := newTestHub(f, clk)
	dir := t.TempDir()

	working := dir + "/long.jsonl"
	writeFile(t, working, `{"type":"event_msg","payload":{"type":"task_started"}}`+"\n"+
		`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec"}}`+"\n")
	w := newFileWatcher(working, true, newCodexFold())
	w.refresh(clk.now()) // folds the events at t0

	clk.advance(watcherWorkingGrace + time.Second) // file quiet past the working grace

	// Stability unsettled (working) → merged stays working → reject despite the anchor.
	if h.inputAccepted(w, ActivityWorking, KindCodex, codexReadyPane()) {
		t.Error("expired-working with unsettled stability must reject (still mid-turn)")
	}
	// Stability settled (idle: the pane genuinely stopped) → the merge releases to
	// stability and the anchor path may accept.
	if !h.inputAccepted(w, ActivityIdle, KindCodex, codexReadyPane()) {
		t.Error("expired-working with settled-idle stability + anchor should accept")
	}
}

// A transient tmux capture failure at the locked re-check is a 500 (retryable), not a
// 404 — only a genuinely-gone session ("can't find pane") maps to 404.
func TestHubInputTransientCaptureErrorIs500(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	f.set("rc-tra111", codexReadyPane(), managedEnv("id-t", KindCodex))
	_, srv := newInputHub(t, f, clk)

	f.setFlaky("rc-tra111", true)
	resp := postInput(t, srv.URL+"/v1/sessions/tra111/input", `{"text":"hi"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("transient capture error status = %d, want 500", resp.StatusCode)
	}
}

// Input serialization is keyed by SLUG on the hub, not on the tracked entry: a
// tracked-entry replacement (recreate reconciled mid-request) yields the SAME mutex,
// so a post that raced the replacement still serializes against a concurrent post.
// The lock is pruned only when the slug disappears.
func TestHubInputLockSurvivesEntryReplacement(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h, srv := newInputHub(t, f, clk)
	f.set("rc-lok111", codexReadyPane(), managedEnv("id-a", KindCodex))
	h.reconcile()

	mu := h.inputLock("lok111")

	// Replace the tracked entry (recreate: new SHED_RC_ID) — the slug's mutex must be
	// the same object afterward.
	f.set("rc-lok111", codexReadyPane(), managedEnv("id-b", KindCodex))
	h.reconcile()
	if h.inputLock("lok111") != mu {
		t.Fatal("entry replacement must not mint a new input mutex for the slug")
	}

	// Holding the mutex blocks a live POST even across the replacement (the handler
	// resolves the lock by slug, not via the entry it looked up).
	mu.Lock()
	done := make(chan int, 1)
	go func() {
		resp := postInput(t, srv.URL+"/v1/sessions/lok111/input", `{"text":"hi"}`)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case code := <-done:
		t.Fatalf("POST completed (status %d) while the slug's input mutex was held", code)
	case <-time.After(150 * time.Millisecond):
		// still blocked — serialized, as required
	}
	mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("POST did not complete after the mutex was released")
	}

	// Disappear → the lock is pruned; a later recreate gets a fresh mutex.
	f.remove("rc-lok111")
	h.reconcile()
	h.inputLockMu.Lock()
	_, still := h.inputLocks["lok111"]
	h.inputLockMu.Unlock()
	if still {
		t.Fatal("disappeared slug's input lock must be pruned")
	}
}

// ---- small HTTP helpers ----

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
