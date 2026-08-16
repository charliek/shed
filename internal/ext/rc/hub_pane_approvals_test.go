package rc

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for the PANE-ANCHOR approval path (plan 008 §3.6): codex's approvals never reach
// a protocol, so the hub reads the dialog off the pane. Everything here is driven through
// the real reconcile loop over the committed pane fixtures — the fixtures ARE the
// contract, so a codex TUI redraw that breaks the anchor fails these tests rather than
// silently going quiet in production.

// paneFixture loads a committed pane fixture by basename (without .txt).
func paneFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "panes", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The anchor's positives and negatives, straight off the fixtures. The two negatives are
// the whole reason the anchor is option-line + footer chrome rather than the headline:
// the post-resolution pane STILL shows "Would you like to run the following command?"
// (it scrolled into the transcript) and the quoted pane shows it as agent prose.
func TestCodexApprovalAnchorFixtures(t *testing.T) {
	anchor := approvalAnchorFor(KindCodex)
	if anchor == nil {
		t.Fatal("codex must declare an ApprovalAnchor")
	}
	cases := []struct {
		fixture string
		want    bool
	}{
		{"codex-ready-approval-exec", true},
		{"codex-ready-approval-network", true}, // selection arrowed onto row 4
		{"codex-ready-approval-resolved", false},
		{"codex-ready-approval-quoted", false},
		{"codex-ready-tool-running", false},
		{"codex-ready", false},
		{"codex-needs-trust", false}, // "1. Yes, continue" is a DIFFERENT widget
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			pane := paneFixture(t, c.fixture)
			if got := anchor.MatchString(pane); got != c.want {
				t.Errorf("anchor match = %v, want %v", got, c.want)
			}
			if c.want {
				// The row's text must be the option row itself, not the whole match span.
				line := firstAnchorLine(anchor, pane)
				if strings.Contains(line, "Press enter") || !strings.Contains(line, ". ") {
					t.Errorf("firstAnchorLine = %q, want the single option row", line)
				}
			}
		})
	}
}

// The headline is NOT the anchor — pinned explicitly because it is the obvious-looking
// choice and it is wrong in both directions.
func TestCodexApprovalAnchorIgnoresHeadline(t *testing.T) {
	anchor := approvalAnchorFor(KindCodex)
	const headline = "  Would you like to run the following command?"
	if !strings.Contains(paneFixture(t, "codex-ready-approval-resolved"), headline) {
		t.Fatal("test premise: the post-resolution fixture must still carry the headline")
	}
	if anchor.MatchString(headline) {
		t.Error("the headline alone must never match the anchor")
	}
}

// paneApprovalHub builds a hub with one codex session showing pane, reconciled zero
// times. setPane swaps the session's pane for the next tick; slug names it for the
// tracked-state helpers below.
func paneApprovalHub(t *testing.T, pane string) (h *Hub, f *hubTmux, setPane func(string), slug string) {
	t.Helper()
	f = newHubTmux()
	h = newTestHub(f, &hubClock{t: time.Unix(1_700_000_000, 0).UTC()})
	f.set("rc-pan001", pane, managedEnv("id-pan", KindCodex))
	// setPane is the common case (change what the whole pane shows); f is returned for
	// the tests that need the visible-frame seam or a lifecycle-state change.
	return h, f, func(p string) { f.setPane("rc-pan001", p) }, "pan001"
}

// paneApprovalRows returns the session's approval_request rows, oldest first.
func paneApprovalRows(t *testing.T, h *Hub, slug string) []feedMessage {
	t.Helper()
	h.trackMu.Lock()
	tr, ok := h.tracked[slug]
	h.trackMu.Unlock()
	if !ok {
		t.Fatalf("slug %q not tracked", slug)
	}
	msgs, _ := tr.ring.since(0, maxMessagesLimit)
	var out []feedMessage
	for _, m := range msgs {
		if m.Type == feedTypeApprovalRequest {
			out = append(out, m)
		}
	}
	return out
}

// hubLastMessageOf reads the session's displayed last_message preview.
func hubLastMessageOf(t *testing.T, h *Hub, slug string) string {
	t.Helper()
	h.trackMu.Lock()
	defer h.trackMu.Unlock()
	tr, ok := h.tracked[slug]
	if !ok {
		t.Fatalf("slug %q not tracked", slug)
	}
	return tr.lastMessage
}

// hubPendingApprovals reads the session's pending_approvals overlay (the union of the
// lane snapshot and the pane episode) the way handleSessions does.
func hubPendingApprovals(t *testing.T, h *Hub, slug string) []FeedApproval {
	t.Helper()
	h.trackMu.Lock()
	defer h.trackMu.Unlock()
	tr, ok := h.tracked[slug]
	if !ok {
		t.Fatalf("slug %q not tracked", slug)
	}
	return copyApprovals(tr.approvalSnapshot())
}

// The core contract: TWO consecutive matching ticks to detect, TWO consecutive
// non-matching ticks to clear — with one informational row per transition and the
// pending_approvals entry present for exactly the open interval.
func TestPaneApprovalDebounceDetectAndClear(t *testing.T) {
	approval := paneFixture(t, "codex-ready-approval-exec")
	resolved := paneFixture(t, "codex-ready-approval-resolved")
	h, _, setPane, slug := paneApprovalHub(t, approval)

	// Tick 1: the anchor matches but the debounce is not satisfied — nothing on the wire.
	h.reconcile()
	if got := hubActivityOf(t, h, slug); got == ActivityNeedsApproval {
		t.Fatal("a single matching tick must not flip activity")
	}
	if rows := paneApprovalRows(t, h, slug); len(rows) != 0 {
		t.Fatalf("a single matching tick must emit no row, got %+v", rows)
	}

	// Tick 2: debounced detection.
	h.reconcile()
	if got := hubActivityOf(t, h, slug); got != ActivityNeedsApproval {
		t.Fatalf("activity = %q, want needs_approval after two matching ticks", got)
	}
	rows := paneApprovalRows(t, h, slug)
	if len(rows) != 1 {
		t.Fatalf("want exactly one row after detection, got %+v", rows)
	}
	row := rows[0]
	if row.Role != feedRoleTool || row.Approval == nil {
		t.Fatalf("pending row shape wrong: %+v", row)
	}
	if row.Approval.ID != "pane-1" || row.Approval.Status != approvalStatusPending {
		t.Errorf("approval = %+v, want {pane-1 pending}", row.Approval)
	}
	// approvals stays "tui" for codex, so the row advertises NO decisions and carries no
	// tool block — a capability-driven client renders zero buttons.
	if len(row.Approval.Decisions) != 0 || row.Approval.Decision != "" || row.Tool != nil {
		t.Errorf("informational row must omit tool/decision/decisions: %+v", row)
	}
	if !strings.Contains(row.Text, "Yes, proceed") {
		t.Errorf("row text = %q, want the first matching option row", row.Text)
	}
	if pend := hubPendingApprovals(t, h, slug); len(pend) != 1 || pend[0].ID != "pane-1" || pend[0].Status != approvalStatusPending {
		t.Errorf("pending_approvals = %+v, want one pending pane-1", pend)
	}

	// Tick 3: the dialog is gone but ONE non-matching tick must not clear it.
	setPane(resolved)
	h.reconcile()
	if got := hubActivityOf(t, h, slug); got != ActivityNeedsApproval {
		t.Fatalf("activity = %q, want needs_approval held through a single clearing tick", got)
	}
	if rows := paneApprovalRows(t, h, slug); len(rows) != 1 {
		t.Fatalf("a single clearing tick must emit no resolved row, got %+v", rows)
	}

	// Tick 4: debounced clear.
	h.reconcile()
	if got := hubActivityOf(t, h, slug); got == ActivityNeedsApproval {
		t.Fatal("two clearing ticks must drop needs_approval")
	}
	rows = paneApprovalRows(t, h, slug)
	if len(rows) != 2 {
		t.Fatalf("want a resolved row after the clear, got %+v", rows)
	}
	res := rows[1]
	if res.Approval.ID != "pane-1" || res.Approval.Status != approvalStatusResolved {
		t.Errorf("resolved approval = %+v, want {pane-1 resolved}", res.Approval)
	}
	// The operator answered in the TUI; the hub cannot know which way, and the contract
	// permits an absent decision on an out-of-hub resolution.
	if res.Approval.Decision != "" {
		t.Errorf("a pane-resolved row must omit decision, got %q", res.Approval.Decision)
	}
	if pend := hubPendingApprovals(t, h, slug); len(pend) != 0 {
		t.Errorf("pending_approvals = %+v, want empty after the clear", pend)
	}
}

// Single-tick blips are ignored in BOTH directions: a lone matching frame never opens an
// episode, and a lone non-matching frame never closes one. This is the anti-flap pin —
// a mid-redraw capture must not put an approval on a phone, nor take one off it.
func TestPaneApprovalSingleTickBlipsIgnored(t *testing.T) {
	approval := paneFixture(t, "codex-ready-approval-exec")
	resolved := paneFixture(t, "codex-ready-approval-resolved")
	h, _, setPane, slug := paneApprovalHub(t, resolved)

	// Blip ON: quiet, one matching frame, quiet again.
	h.reconcile()
	setPane(approval)
	h.reconcile()
	setPane(resolved)
	h.reconcile()
	if got := hubActivityOf(t, h, slug); got == ActivityNeedsApproval {
		t.Error("a one-tick anchor blip must not open an episode")
	}
	if rows := paneApprovalRows(t, h, slug); len(rows) != 0 {
		t.Errorf("a one-tick blip must emit no rows, got %+v", rows)
	}

	// Open a real episode, then blip OFF for one tick.
	setPane(approval)
	h.reconcile()
	h.reconcile()
	if got := hubActivityOf(t, h, slug); got != ActivityNeedsApproval {
		t.Fatalf("test premise: two matching ticks must open an episode, got %q", got)
	}
	setPane(resolved)
	h.reconcile()
	setPane(approval)
	h.reconcile()
	if got := hubActivityOf(t, h, slug); got != ActivityNeedsApproval {
		t.Error("a one-tick clearing blip must not close an open episode")
	}
	if rows := paneApprovalRows(t, h, slug); len(rows) != 1 {
		t.Errorf("a one-tick clearing blip must emit no resolved row, got %+v", rows)
	}
}

// Episode ids are per-session monotonic: a second dialog is pane-2, never a reuse of
// pane-1 (a client folds approval rows by id — reuse would rewrite history).
func TestPaneApprovalIDsMonotonicAcrossEpisodes(t *testing.T) {
	approval := paneFixture(t, "codex-ready-approval-exec")
	resolved := paneFixture(t, "codex-ready-approval-resolved")
	h, _, setPane, slug := paneApprovalHub(t, approval)

	for _, pane := range []string{approval, approval, resolved, resolved, approval, approval} {
		setPane(pane)
		h.reconcile()
	}
	rows := paneApprovalRows(t, h, slug)
	if len(rows) != 3 {
		t.Fatalf("want pending/resolved/pending across two episodes, got %+v", rows)
	}
	want := []struct{ id, status string }{
		{"pane-1", approvalStatusPending},
		{"pane-1", approvalStatusResolved},
		{"pane-2", approvalStatusPending},
	}
	for i, w := range want {
		if rows[i].Approval.ID != w.id || rows[i].Approval.Status != w.status {
			t.Errorf("row %d = %+v, want {%s %s}", i, rows[i].Approval, w.id, w.status)
		}
	}
	if pend := hubPendingApprovals(t, h, slug); len(pend) != 1 || pend[0].ID != "pane-2" {
		t.Errorf("pending_approvals = %+v, want the second episode only", pend)
	}
}

// The negative that matters most (plan 008 §3.6): codex writes the tool-call record to
// its rollout BEFORE the approval gate, so a session blocked on approval and one running
// a slow tool are byte-identical in the log. Only the pane distinguishes them — and a
// working session with no overlay must NEVER read needs_approval, however long it churns.
func TestPaneApprovalLongToolCallIsNotApproval(t *testing.T) {
	base := paneFixture(t, "codex-ready-tool-running")
	h, _, setPane, slug := paneApprovalHub(t, base)

	for i := 0; i < 6; i++ {
		// The elapsed readout ticks every frame — a genuinely working pane.
		setPane(strings.Replace(base, "2m14s", "2m"+string(rune('1'+i))+"4s", 1))
		h.reconcile()
		if got := hubActivityOf(t, h, slug); got == ActivityNeedsApproval {
			t.Fatalf("tick %d: a long tool call must not read needs_approval", i)
		}
	}
	if rows := paneApprovalRows(t, h, slug); len(rows) != 0 {
		t.Errorf("a long tool call must emit no approval rows, got %+v", rows)
	}
	if pend := hubPendingApprovals(t, h, slug); len(pend) != 0 {
		t.Errorf("pending_approvals = %+v, want empty", pend)
	}
}

// Agent prose that DESCRIBES the approval dialog — headline and option labels quoted
// inside an assistant message — must never open an episode, no matter how long it sits
// on screen (the debounce cannot save us here; only the anchor's shape can).
func TestPaneApprovalQuotedProseIsNotApproval(t *testing.T) {
	h, _, _, slug := paneApprovalHub(t, paneFixture(t, "codex-ready-approval-quoted"))
	for i := 0; i < 4; i++ {
		h.reconcile()
		if got := hubActivityOf(t, h, slug); got == ActivityNeedsApproval {
			t.Fatalf("tick %d: quoted prose must not read needs_approval", i)
		}
	}
	if rows := paneApprovalRows(t, h, slug); len(rows) != 0 {
		t.Errorf("quoted prose must emit no approval rows, got %+v", rows)
	}
}

// The wire path end-to-end: needs_approval must reach the SSE activity.changed stream and
// the /v1/sessions rows through the normal machinery (no special-casing) — the phone sees
// an approval the same way it sees any other activity change.
func TestPaneApprovalReachesSSEAndSessions(t *testing.T) {
	h, _, _, _ := paneApprovalHub(t, paneFixture(t, "codex-ready-approval-exec"))
	sub := h.subscribe()
	h.reconcile()
	h.reconcile()

	evs := drainEvents(sub)
	found := false
	for _, e := range evs {
		if e.name == "activity.changed" && strings.Contains(e.raw, `"activity":"needs_approval"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no activity.changed carrying needs_approval: %+v", evs)
	}
	if countEvents(evs, "message.appended") == 0 {
		t.Error("the informational approval row must announce itself as message.appended")
	}

	srv := httptest.NewServer(h.handler())
	t.Cleanup(srv.Close)
	var body hubSessionsResponse
	getJSON(t, srv.URL+"/v1/sessions", &body)
	if len(body.Sessions) != 1 {
		t.Fatalf("want one session, got %+v", body.Sessions)
	}
	s := body.Sessions[0]
	if s.Activity != ActivityNeedsApproval {
		t.Errorf("session activity = %q, want needs_approval", s.Activity)
	}
	if len(s.PendingApprovals) != 1 || s.PendingApprovals[0].ID != "pane-1" {
		t.Errorf("pending_approvals = %+v, want pane-1", s.PendingApprovals)
	}
	if len(s.PendingApprovals[0].Decisions) != 0 {
		t.Errorf("codex approvals are answered in the TUI — the entry must advertise no decisions: %+v", s.PendingApprovals[0])
	}
}

// The anchor evaluates the VISIBLE FRAME, never the scrollback. A dialog that was
// answered — or that was on screen when the agent crashed out to a shell — stays in the
// 200-line history verbatim, so an episode opened off scrollback would have no clearing
// arm and would pin the session at needs_approval forever.
func TestPaneApprovalIgnoresScrollback(t *testing.T) {
	approval := paneFixture(t, "codex-ready-approval-exec")
	h, f, _, slug := paneApprovalHub(t, approval)
	name := "rc-" + slug

	// Scrollback still carries the whole dialog; the visible frame has moved on.
	f.setVisible(name, paneFixture(t, "codex-ready-approval-resolved"))
	for i := 0; i < 4; i++ {
		h.reconcile()
		if got := hubActivityOf(t, h, slug); got == ActivityNeedsApproval {
			t.Fatalf("tick %d: chrome in the scrollback must not open an episode", i)
		}
	}
	if rows := paneApprovalRows(t, h, slug); len(rows) != 0 {
		t.Errorf("scrollback-only chrome must emit no rows, got %+v", rows)
	}

	// And an OPEN episode clears when the dialog scrolls out of the visible frame, even
	// though the scrollback capture is unchanged the whole time.
	f.setVisible(name, "")
	h.reconcile()
	h.reconcile()
	if got := hubActivityOf(t, h, slug); got != ActivityNeedsApproval {
		t.Fatalf("test premise: a visible dialog must open an episode, got %q", got)
	}
	f.setVisible(name, paneFixture(t, "codex-ready-approval-resolved"))
	h.reconcile()
	h.reconcile()
	if got := hubActivityOf(t, h, slug); got == ActivityNeedsApproval {
		t.Error("an episode must clear once the dialog leaves the visible frame")
	}
}

// A blocking lifecycle state suppresses the whole activity dimension, so the anchor is
// not evaluated at all there — no episode opens on a dead/gated session.
func TestPaneApprovalNotEvaluatedWhileLifecycleBlocks(t *testing.T) {
	// A codex pane showing the dialog AND the needs-auth signal: classification wins.
	pane := paneFixture(t, "codex-ready-approval-exec") + "\nSign in with ChatGPT\n"
	f := newHubTmux()
	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(f, clk)
	f.set("rc-pan002", pane, managedEnv("id-pan2", KindCodex))
	// codexReadyRe wins over codexAuthRe on a pane carrying both, so strip the banner to
	// get a genuinely needs-auth pane that still shows the dialog chrome.
	f.setPane("rc-pan002", strings.SplitN(pane, "╰", 2)[1])

	for i := 0; i < 4; i++ {
		h.reconcile()
	}
	if got := hubActivityOf(t, h, "pan002"); got != "" {
		t.Fatalf("activity = %q, want suppressed on a blocking lifecycle state", got)
	}
	if rows := paneApprovalRows(t, h, "pan002"); len(rows) != 0 {
		t.Errorf("a gated session must emit no approval rows, got %+v", rows)
	}
	if pend := hubPendingApprovals(t, h, "pan002"); len(pend) != 0 {
		t.Errorf("pending_approvals = %+v, want empty", pend)
	}
}

// A session that DIES mid-dialog resolved nothing. The episode is dropped silently — no
// resolved row — because the session.updated carrying the new state is the honest signal,
// and a resolved row would assert an answer nobody gave. Ids stay monotonic across it.
func TestPaneApprovalDroppedSilentlyOnDeath(t *testing.T) {
	approval := paneFixture(t, "codex-ready-approval-exec")
	h, f, _, slug := paneApprovalHub(t, approval)
	name := "rc-" + slug

	h.reconcile()
	h.reconcile()
	if got := hubActivityOf(t, h, slug); got != ActivityNeedsApproval {
		t.Fatalf("test premise: want an open episode, got %q", got)
	}

	// codex exits with the dialog still on screen: the pane ends at a shed shell prompt.
	f.set(name, approval+"\n[shed:agent-fixtures] ~ $ ", managedEnv("id-pan", KindCodex))
	h.reconcile()

	if got := hubActivityOf(t, h, slug); got != "" {
		t.Errorf("activity = %q, want suppressed on a dead session", got)
	}
	rows := paneApprovalRows(t, h, slug)
	if len(rows) != 1 || rows[0].Approval.Status != approvalStatusPending {
		t.Fatalf("a death must not append a resolved row, got %+v", rows)
	}
	if pend := hubPendingApprovals(t, h, slug); len(pend) != 0 {
		t.Errorf("pending_approvals = %+v, want the abandoned episode gone", pend)
	}

	// Recovery: the ids do not rewind — a new dialog is pane-2, never a reused pane-1.
	f.set(name, approval, managedEnv("id-pan", KindCodex))
	h.reconcile()
	h.reconcile()
	rows = paneApprovalRows(t, h, slug)
	if len(rows) != 2 || rows[1].Approval.ID != "pane-2" {
		t.Errorf("want a fresh pane-2 episode after recovery, got %+v", rows)
	}
}

// While an episode is open, last_message must be the approval's own summary — not the
// watcher's preview of the tool call the dialog SUSPENDED, which would read on a phone as
// "busy running this" at the exact moment the truth is "waiting on you". The normal merge
// is restored on clear.
func TestPaneApprovalOverridesLastMessage(t *testing.T) {
	approval := paneFixture(t, "codex-ready-approval-exec")
	resolved := paneFixture(t, "codex-ready-approval-resolved")
	h, f, _, slug := paneApprovalHub(t, approval)
	name := "rc-" + slug

	h.reconcile()
	h.reconcile()
	msg := hubLastMessageOf(t, h, slug)
	if !strings.Contains(msg, "Yes, proceed") {
		t.Errorf("last_message = %q, want the approval's option-row summary", msg)
	}
	rows := paneApprovalRows(t, h, slug)
	if len(rows) != 1 || rows[0].Text != msg {
		t.Errorf("last_message must be the pending row's text (%q vs %q)", msg, rows[0].Text)
	}

	f.setPane(name, resolved)
	h.reconcile()
	h.reconcile()
	if msg := hubLastMessageOf(t, h, slug); strings.Contains(msg, "Yes, proceed") {
		t.Errorf("last_message = %q, want the normal merge restored after the clear", msg)
	}
}

// A pane-* id is NOT remotely resolvable: codex's kind_features row says approvals "tui",
// and the R0 handler rejects on that capability BEFORE any id lookup — so the answer is
// 409 not_supported, never 404 unknown_approval. Pinned because the id now exists and
// looks addressable on the wire.
func TestPaneApprovalIDStillNotSupported(t *testing.T) {
	srv := newVerbHub(t)
	resp := doRequest(t, http.MethodPost, srv.URL+"/v1/sessions/"+verbSlugCodex+"/approvals/pane-1", `{"decision":"allow"}`)
	wantEnvelope(t, resp, http.StatusConflict, errNotSupported)
}
