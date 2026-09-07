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

// codexReadyPane is a codex pane parked at its composer. Since S2 (charliek/shed#324)
// the hub reads nothing out of it — a session that ENUMERATES is live — so this is
// just plausible pane text for a live codex row.
func codexReadyPane() string { return "codex\n> Ask Codex to do anything" }

// opencodeReadyPane is the same for an opencode row.
func opencodeReadyPane() string { return "opencode\n> Ask anything..." }

// ---- GET /v1/sessions/{slug}/messages ----

func TestHubHTTPMessagesPagingTruncatedAnd404(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	f.set("rc-msg111", codexReadyPane(), managedEnv("id-m", KindCodex))
	h.reconcile()

	// White-box: seed the tracked session's ring (the HTTP layer's job is paging, not
	// production — the fold→ring path is covered by the opencode fold's own tests).
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

// newInputHub reconciles once (which is what puts a session in the tracked map) and
// serves the hub over HTTP. There is nothing left to settle: A6 removed the gated lane
// and S2 (charliek/shed#324) the pane-stability engine whose quiet period the second
// reconcile used to cross.
func newInputHub(t *testing.T, f *hubTmux, clk *hubClock) (*Hub, *httptest.Server) {
	t.Helper()
	h := newTestHub(f, clk)
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

// A6 (charliek/shed#322) removed the gated-input lane: `kind_features.input` is "" for
// every TUI kind and "turn" for opencode, so NO kind is `gated` and a well-formed POST
// for a live session is 409 not_accepting whatever the pane shows. The gated-lane cells
// this replaces — happy path, bracketed paste, degraded-anchor accept, the under-a-dialog
// / state-flip / identity 409s, the acceptance-merge branches and the per-slug delivery
// mutex — went with the lane.
func TestHubInputNotAcceptingForEveryKind(t *testing.T) {
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	f.set("rc-nac001", codexReadyPane(), managedEnv("id-c", KindCodex))
	f.set("rc-nac002", opencodeReadyPane(), managedEnv("id-o", KindOpencode))
	f.set("rc-nac003", "cursor\n> ", managedEnv("id-u", KindCursor))
	f.set("rc-nac004", "claude\n> ", managedEnv("id-r", KindClaudeRC))
	_, srv := newInputHub(t, f, clk)

	for _, slug := range []string{"nac001", "nac002", "nac003", "nac004"} {
		t.Run(slug, func(t *testing.T) {
			resp := postInput(t, srv.URL+"/v1/sessions/"+slug+"/input", `{"text":"hi"}`)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409 (no kind accepts feed input)", resp.StatusCode)
			}
			body := readAll(t, resp)
			if !strings.Contains(body, "not_accepting") || !strings.Contains(body, "does not accept feed input") {
				t.Errorf("rejection = %s, want the kind-gate not_accepting envelope", body)
			}
		})
	}
}

// stubWatcher is a scripted sessionWatcher: it reports a fixed verdict with fixed
// authority, so a merge case (needs_approval, expired-working, …) can be exercised
// without standing up a real SSE transport.
type stubWatcher struct {
	activity       Activity
	message        string
	fresh          bool
	expiredWorking bool
	approvals      []FeedApproval
}

func (s *stubWatcher) refresh(time.Time) {}
func (s *stubWatcher) snapshot(time.Time) (Activity, string, bool, bool) {
	return s.activity, s.message, s.fresh, s.expiredWorking
}
func (s *stubWatcher) drainPending() []feedMessage { return nil }
func (s *stubWatcher) hadEvent() bool              { return true }
func (s *stubWatcher) close()                      {}

// stubApprovalWatcher adds the approval snapshot reconcile publishes
// (approvalPublisher). blocked models an open ask that is NOT in the snapshot — an
// opencode question — so the two can be driven apart.
type stubApprovalWatcher struct {
	stubWatcher
	blocked bool
}

func (s *stubApprovalWatcher) pendingApprovals() []FeedApproval { return s.approvals }
func (s *stubApprovalWatcher) hasOpenApprovals() bool           { return s.blocked || len(s.approvals) > 0 }

var (
	_ sessionWatcher    = (*stubWatcher)(nil)
	_ approvalPublisher = (*stubApprovalWatcher)(nil)
)

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
