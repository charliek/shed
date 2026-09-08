package rc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readJSONL loads a fixture's non-blank lines as raw bytes (feed straight to a fold).
func readJSONL(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out [][]byte
	for _, l := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			cp := make([]byte, len(l))
			copy(cp, l)
			out = append(out, cp)
		}
	}
	return out
}

// The codex fold's fixture-arc cell lived here until A6 (charliek/shed#322) removed the
// codex rollout tail and its testdata/jsonl/codex_turn.jsonl fixture.

// ---- opencode fold: the sanitized live /event capture folds to the expected arc ----

// opencodeFeedRow is one expected drained feed row (only the fields the tests assert).
type opencodeFeedRow struct {
	role, typ  string
	textPrefix string // Text must have this prefix ("" = don't check)
	toolName   string // Tool.Name must equal this ("" = don't check / not a tool row)
	detailHas  string // Tool.Detail must contain this substring ("" = don't check)
}

func TestOpencodeFoldFixtureArc(t *testing.T) {
	lines := readJSONL(t, "testdata/jsonl/opencode_turn.jsonl")
	f := newOpencodeFold()

	// Before any confirming (activity-relevant) event the verdict is unknown.
	if got := f.activity(); got != ActivityUnknown {
		t.Fatalf("initial activity = %q, want unknown", got)
	}

	sawWorking := false
	for _, ln := range lines {
		f.applyLine(ln)
		if f.activity() == ActivityWorking {
			sawWorking = true
		}
	}
	if !sawWorking {
		t.Error("expected a working verdict during the turn")
	}
	// The arc ends at session.idle → needs_input, settled, with the final answer.
	if got := f.activity(); got != ActivityNeedsInput {
		t.Fatalf("final activity = %q, want needs_input", got)
	}
	if !f.settled() {
		t.Error("final verdict should be settled")
	}
	if got := f.lastMessage(); got != "3 .txt files." {
		t.Fatalf("last_message = %q, want %q", got, "3 .txt files.")
	}

	// The feed is the normalized turn: user prompt → reasoning → tool_use → tool_result
	// → assistant answer, in that order.
	want := []opencodeFeedRow{
		{role: feedRoleUser, typ: feedTypeText, textPrefix: "Use the bash tool"},
		{role: feedRoleAssistant, typ: feedTypeReasoning, textPrefix: "The user wants"},
		{role: feedRoleTool, typ: feedTypeToolUse, toolName: "bash", detailHas: "ls"},
		{role: feedRoleTool, typ: feedTypeToolResult, toolName: "bash", detailHas: "a.txt"},
		{role: feedRoleAssistant, typ: feedTypeText, textPrefix: "3 .txt files."},
	}
	got := f.drainMessages()
	assertOpencodeRows(t, got, want)

	// Every row carried a source time, so every TS is non-empty and chronological
	// (RFC3339 sorts lexicographically in time order; equal-second rows are allowed).
	prev := ""
	for i, m := range got {
		if m.TS == "" {
			t.Errorf("row %d (%s/%s) has an empty TS, want a source-derived time", i, m.Role, m.Type)
		}
		if m.TS < prev {
			t.Errorf("row %d TS %q is before the previous row's %q (not chronological)", i, m.TS, prev)
		}
		prev = m.TS
	}
}

func assertOpencodeRows(t *testing.T, got []feedMessage, want []opencodeFeedRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("drained %d rows, want %d:\n got=%s", len(got), len(want), formatOpencodeRows(got))
	}
	for i, w := range want {
		m := got[i]
		if m.Role != w.role || m.Type != w.typ {
			t.Errorf("row %d = (%s/%s), want (%s/%s)", i, m.Role, m.Type, w.role, w.typ)
		}
		if w.textPrefix != "" && !strings.HasPrefix(m.Text, w.textPrefix) {
			t.Errorf("row %d text = %q, want prefix %q", i, m.Text, w.textPrefix)
		}
		if w.toolName != "" {
			if m.Tool == nil || m.Tool.Name != w.toolName {
				t.Errorf("row %d tool = %+v, want name %q", i, m.Tool, w.toolName)
			}
		}
		if w.detailHas != "" {
			if m.Tool == nil || !strings.Contains(m.Tool.Detail, w.detailHas) {
				t.Errorf("row %d tool detail = %+v, want substring %q", i, m.Tool, w.detailHas)
			}
		}
	}
}

func formatOpencodeRows(rows []feedMessage) string {
	var b strings.Builder
	for _, m := range rows {
		fmt.Fprintf(&b, "  %s/%s text=%q tool=%+v\n", m.Role, m.Type, m.Text, m.Tool)
	}
	return b.String()
}

// A reconnect re-seeds the same history WITHOUT resetting the fold; the partID/callID
// dedup must make the second fold of the identical arc emit ZERO new rows.
func TestOpencodeFoldReseedIdempotent(t *testing.T) {
	lines := readJSONL(t, "testdata/jsonl/opencode_turn.jsonl")
	f := newOpencodeFold()

	for _, ln := range lines {
		f.applyLine(ln)
	}
	if got := f.drainMessages(); len(got) != 5 {
		t.Fatalf("first drain = %d rows, want 5", len(got))
	}
	// Feed the SAME fixture again (a reconnect reseed — NO reset()).
	for _, ln := range lines {
		f.applyLine(ln)
	}
	if got := f.drainMessages(); len(got) != 0 {
		t.Fatalf("reseed drain = %d rows, want 0 (dedup by partID/callID):\n%s", len(got), formatOpencodeRows(got))
	}
	// Activity is still the settled end-state after the reseed.
	if got := f.activity(); got != ActivityNeedsInput {
		t.Fatalf("post-reseed activity = %q, want needs_input", got)
	}
}

// An assistant text part that gets two non-empty snapshots (partial then full, only the
// full carrying part.time.end) emits exactly ONE row with the COMPLETE text.
func TestOpencodeFoldMultiSnapshot(t *testing.T) {
	f := newOpencodeFold()
	f.applyLine([]byte(`{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"msgX","role":"assistant","time":{"created":1784613627000}}}}`))
	// Partial snapshot: non-empty but no time.end → cached, not emitted.
	f.applyLine([]byte(`{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"prtX","messageID":"msgX","type":"text","text":"3 .txt","time":{"start":1784613627679}}},"time":1784613627679}`))
	if got := f.drainMessages(); len(got) != 0 {
		t.Fatalf("partial (no time.end) drained %d rows, want 0:\n%s", len(got), formatOpencodeRows(got))
	}
	// Full snapshot with time.end → emit the complete text once.
	f.applyLine([]byte(`{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"prtX","messageID":"msgX","type":"text","text":"3 .txt files.","time":{"start":1784613627679,"end":1784613627681}}},"time":1784613627681}`))
	got := f.drainMessages()
	if len(got) != 1 {
		t.Fatalf("full snapshot drained %d rows, want 1:\n%s", len(got), formatOpencodeRows(got))
	}
	if got[0].Role != feedRoleAssistant || got[0].Type != feedTypeText || got[0].Text != "3 .txt files." {
		t.Fatalf("row = (%s/%s,%q), want (assistant/text,\"3 .txt files.\")", got[0].Role, got[0].Type, got[0].Text)
	}
	if got[0].TS == "" {
		t.Error("row TS should be the part's time.end, not empty")
	}
}

// permission.asked is an OPEN APPROVAL: it emits an approval_request row carrying the
// addressable id + the honored decisions, and it flips the verdict to needs_approval even
// though the tool call that triggered it is still open (the session is blocked on the
// operator, not on the model).
func TestOpencodeFoldPermissionApprovalRow(t *testing.T) {
	f := newOpencodeFold()
	f.applyLine([]byte(`{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}`))
	if got := f.activity(); got != ActivityWorking {
		t.Fatalf("after busy = %q, want working", got)
	}
	f.applyLine([]byte(`{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","messageID":"m1","type":"tool","tool":"bash","callID":"c1","state":{"status":"running","input":{"command":"rm -rf /tmp/x"}}}},"time":1784613621168}`))
	if got := f.activity(); got != ActivityWorking {
		t.Fatalf("with an open tool call = %q, want working", got)
	}

	f.applyLine([]byte(`{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["rm -rf /tmp/x","ls"],"metadata":{"command":"rm -rf /tmp/x"}}}`))
	if got := f.activity(); got != ActivityNeedsApproval {
		t.Fatalf("after permission.asked = %q, want needs_approval (outranks the open tool call)", got)
	}
	if !f.settled() {
		t.Error("needs_approval is an event-bounded end state: settled() must be true")
	}

	got := f.drainMessages()
	// The tool_use row for the open call, then the approval row.
	if len(got) != 2 {
		t.Fatalf("drained %d rows, want 2 (tool_use + approval_request):\n%s", len(got), formatOpencodeRows(got))
	}
	m := got[1]
	if m.Role != feedRoleTool || m.Type != feedTypeApprovalRequest {
		t.Fatalf("row = (%s/%s), want (tool/approval_request)", m.Role, m.Type)
	}
	if !strings.Contains(m.Text, "awaiting approval: bash") || !strings.Contains(m.Text, "rm -rf /tmp/x") {
		t.Fatalf("approval text = %q, want it to name the permission + patterns", m.Text)
	}
	if m.Tool == nil || m.Tool.Name != "bash" || m.Tool.Detail != "rm -rf /tmp/x" {
		t.Fatalf("approval tool block = %+v, want {bash, rm -rf /tmp/x}", m.Tool)
	}
	if m.Approval == nil {
		t.Fatal("approval_request row must carry an approval block")
	}
	if m.Approval.ID != "per_1" || m.Approval.Status != approvalStatusPending || m.Approval.Decision != "" {
		t.Fatalf("approval = %+v, want id per_1 / pending / no decision", m.Approval)
	}
	wantDecisions := []string{approvalDecisionAllow, approvalDecisionAllowAlways, approvalDecisionDeny}
	if !slices.Equal(m.Approval.Decisions, wantDecisions) {
		t.Fatalf("decisions = %v, want %v", m.Approval.Decisions, wantDecisions)
	}

	// The snapshot surface: pending-only, ask-ordered, and state-queryable by id.
	pend := f.pendingApprovals()
	if len(pend) != 1 || pend[0].ID != "per_1" || pend[0].Status != approvalStatusPending {
		t.Fatalf("pendingApprovals = %+v, want the one open ask", pend)
	}
	status, decision, ok := f.approvalState("per_1")
	if !ok || status != approvalStatusPending || decision != "" {
		t.Fatalf("approvalState(per_1) = (%q,%q,%v), want (pending,\"\",true)", status, decision, ok)
	}
	if _, _, ok := f.approvalState("per_nope"); ok {
		t.Error("approvalState must report ok=false for an id this session never saw")
	}
}

// permission.replied closes the ask, appends the resolved row with the decision mapped back
// from the native reply, and releases the verdict to whatever the session was doing.
func TestOpencodeFoldPermissionReplied(t *testing.T) {
	cases := []struct {
		name     string
		reply    string
		decision string
	}{
		{"once maps to allow", "once", approvalDecisionAllow},
		{"always maps to allow_always", "always", approvalDecisionAllowAlways},
		{"reject maps to deny", "reject", approvalDecisionDeny},
		{"an unrecognized reply still closes the ask", "sideways", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newOpencodeFold()
			f.applyLine([]byte(`{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}`))
			f.applyLine([]byte(`{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}`))
			f.drainMessages()

			line := []byte(`{"type":"permission.replied","properties":{"sessionID":"s","requestID":"per_1","reply":"` + c.reply + `"}}`)
			if !f.applyLine(line) {
				t.Fatal("permission.replied on an open ask must advance state")
			}
			if got := f.activity(); got != ActivityWorking {
				t.Fatalf("after the reply = %q, want working (the ask no longer blocks)", got)
			}
			if len(f.pendingApprovals()) != 0 {
				t.Fatalf("pendingApprovals = %+v, want empty after the reply", f.pendingApprovals())
			}
			status, decision, ok := f.approvalState("per_1")
			if !ok || status != approvalStatusResolved || decision != c.decision {
				t.Fatalf("approvalState = (%q,%q,%v), want (resolved,%q,true)", status, decision, ok, c.decision)
			}

			got := f.drainMessages()
			if len(got) != 1 {
				t.Fatalf("drained %d rows, want 1 resolved row:\n%s", len(got), formatOpencodeRows(got))
			}
			m := got[0]
			if m.Role != feedRoleTool || m.Type != feedTypeApprovalRequest || m.Approval == nil {
				t.Fatalf("row = (%s/%s) approval=%+v, want a tool/approval_request row", m.Role, m.Type, m.Approval)
			}
			if m.Approval.ID != "per_1" || m.Approval.Status != approvalStatusResolved || m.Approval.Decision != c.decision {
				t.Fatalf("approval = %+v, want id per_1 / resolved / decision %q", m.Approval, c.decision)
			}

			// Idempotent: a replayed reply (or the event arriving after a local mark) is a no-op.
			if f.applyLine(line) {
				t.Error("a replayed permission.replied must not advance state")
			}
			if got := f.drainMessages(); len(got) != 0 {
				t.Fatalf("replayed reply emitted %d rows, want 0:\n%s", len(got), formatOpencodeRows(got))
			}
		})
	}
}

// The verb handler's local mark and opencode's own permission.replied are two announcements
// of ONE resolution: whichever lands first emits the single resolved row, the other is a
// no-op. (The handler marks synchronously so a same-decision replay cannot re-POST.)
func TestOpencodeFoldResolveDedupAgainstLocalMark(t *testing.T) {
	f := newOpencodeFold()
	f.applyLine([]byte(`{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}`))
	f.applyLine([]byte(`{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}`))
	f.drainMessages()

	if !f.resolvePermission("per_1", approvalDecisionAllow) {
		t.Fatal("the local mark must resolve an open ask")
	}
	if f.resolvePermission("per_1", approvalDecisionAllow) {
		t.Error("a second local mark must be a no-op")
	}
	// The stream's own event for the same id lands next: still exactly one resolved row.
	if f.applyLine([]byte(`{"type":"permission.replied","properties":{"sessionID":"s","requestID":"per_1","reply":"once"}}`)) {
		t.Error("permission.replied after a local mark must not advance state")
	}
	got := f.drainMessages()
	if len(got) != 1 {
		t.Fatalf("drained %d rows, want exactly 1 resolved row:\n%s", len(got), formatOpencodeRows(got))
	}
	if got[0].Approval == nil || got[0].Approval.Status != approvalStatusResolved {
		t.Fatalf("row = %+v, want the resolved approval row", got[0].Approval)
	}
	// A reply for an id this fold never saw asked leaves a resolved TOMBSTONE (with its own
	// resolved row) rather than being dropped — so a later ask replay cannot open a pending
	// entry for a permission that is already answered.
	if !f.resolvePermission("per_never_asked", approvalDecisionDeny) {
		t.Fatal("a reply for an unseen id must record a tombstone")
	}
	status, decision, ok := f.approvalState("per_never_asked")
	if !ok || status != approvalStatusResolved || decision != approvalDecisionDeny {
		t.Fatalf("tombstone state = (%q,%q,%v), want (resolved,deny,true)", status, decision, ok)
	}
	if got := f.drainMessages(); len(got) != 1 || got[0].Approval == nil ||
		got[0].Approval.Status != approvalStatusResolved {
		t.Fatalf("tombstone rows = %s, want its one resolved row", formatOpencodeRows(got))
	}
	// The ask finally replays (a reseed racing the reply): it must NOT re-open the entry.
	if f.applyLine([]byte(`{"type":"permission.asked","properties":{"id":"per_never_asked","sessionID":"s","permission":"bash","patterns":["ls"]}}`)) {
		t.Error("an ask replay for a tombstoned id must not advance state")
	}
	if got := f.pendingApprovals(); len(got) != 0 {
		t.Fatalf("pendingApprovals = %+v, want empty (the tombstone stays closed)", got)
	}
	if got := f.openApprovals(); got != 0 {
		t.Fatalf("openApprovals = %d, want 0 — a replied permission must never block the session", got)
	}
}

// A question blocks the session (needs_approval) but is NOT addressable: it keeps the
// display-only status row and never enters pending_approvals — the legal "needs_approval with
// an empty snapshot" state. question.replied / question.rejected clear it.
func TestOpencodeFoldQuestionBlocksWithoutPendingApproval(t *testing.T) {
	for _, clearType := range []string{"question.replied", "question.rejected"} {
		t.Run(clearType, func(t *testing.T) {
			f := newOpencodeFold()
			f.applyLine([]byte(`{"type":"session.idle","properties":{"sessionID":"s"}}`))
			f.applyLine([]byte(`{"type":"question.asked","properties":{"id":"que_1","sessionID":"s","questions":[{"header":"Which file?","text":"pick one"}]}}`))
			if got := f.activity(); got != ActivityNeedsApproval {
				t.Fatalf("after question.asked = %q, want needs_approval", got)
			}
			if got := f.pendingApprovals(); len(got) != 0 {
				t.Fatalf("pendingApprovals = %+v, want empty (questions are not addressable)", got)
			}
			if _, _, ok := f.approvalState("que_1"); ok {
				t.Error("a question must not be resolvable through the approvals verb")
			}
			got := f.drainMessages()
			if len(got) != 1 || got[0].Role != feedRoleSystem || got[0].Type != feedTypeStatus {
				t.Fatalf("question row = %s, want one system/status row", formatOpencodeRows(got))
			}

			f.applyLine([]byte(`{"type":"` + clearType + `","properties":{"sessionID":"s","requestID":"que_1"}}`))
			if got := f.activity(); got != ActivityNeedsInput {
				t.Fatalf("after %s = %q, want needs_input (the block cleared)", clearType, got)
			}
			if got := f.drainMessages(); len(got) != 0 {
				t.Fatalf("clearing a question emitted %d rows, want 0:\n%s", len(got), formatOpencodeRows(got))
			}
		})
	}
}

// The reseed's approval-seed marker is the AUTHORITATIVE open-ask set: an ask the server no
// longer lists was answered while the stream was down, so it is retired (resolved with NO
// decision — the hub cannot know which way the TUI went), while a still-listed ask survives
// the reseed without a duplicate row.
func TestOpencodeFoldApprovalSeedRetiresAnsweredAsks(t *testing.T) {
	f := newOpencodeFold()
	f.applyLine([]byte(`{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}`))
	f.applyLine([]byte(`{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}`))
	f.applyLine([]byte(`{"type":"permission.asked","properties":{"id":"per_2","sessionID":"s","permission":"edit","patterns":["a.go"]}}`))
	f.applyLine([]byte(`{"type":"question.asked","properties":{"id":"que_1","sessionID":"s","questions":[{"header":"Which file?"}]}}`))
	if got := f.drainMessages(); len(got) != 3 {
		t.Fatalf("initial drain = %d rows, want 3:\n%s", len(got), formatOpencodeRows(got))
	}

	// A reconnect replays the still-open asks, then the marker: per_2 + que_1 remain open,
	// per_1 and the question's sibling were answered in the TUI meanwhile.
	f.applyLine([]byte(`{"type":"permission.asked","properties":{"id":"per_2","sessionID":"s","permission":"edit","patterns":["a.go"]}}`))
	f.applyLine([]byte(`{"type":"question.asked","properties":{"id":"que_1","sessionID":"s","questions":[{"header":"Which file?"}]}}`))
	if !f.applyLine([]byte(`{"type":"` + ocApprovalSeedType + `","properties":{"sessionID":"s","permissionIDs":["per_2"],"questionIDs":["que_1"],"permissionsKnown":true,"questionsKnown":true}}`)) {
		t.Fatal("the seed marker retired an ask, so it must advance state")
	}

	pend := f.pendingApprovals()
	if len(pend) != 1 || pend[0].ID != "per_2" {
		t.Fatalf("pendingApprovals = %+v, want only per_2", pend)
	}
	status, decision, ok := f.approvalState("per_1")
	if !ok || status != approvalStatusResolved || decision != "" {
		t.Fatalf("approvalState(per_1) = (%q,%q,%v), want (resolved,\"\",true) — answered outside the hub", status, decision, ok)
	}
	got := f.drainMessages()
	if len(got) != 1 {
		t.Fatalf("reseed drain = %d rows, want 1 (per_1 resolved; no duplicates):\n%s", len(got), formatOpencodeRows(got))
	}
	if got[0].Approval == nil || got[0].Approval.ID != "per_1" || got[0].Approval.Decision != "" {
		t.Fatalf("row approval = %+v, want per_1 resolved with no decision", got[0].Approval)
	}

	// A marker listing nothing (both halves known) retires everything still open, questions
	// included.
	f.applyLine([]byte(`{"type":"` + ocApprovalSeedType + `","properties":{"sessionID":"s","permissionsKnown":true,"questionsKnown":true}}`))
	if got := f.openApprovals(); got != 0 {
		t.Fatalf("openApprovals after an empty marker = %d, want 0", got)
	}
	if got := f.activity(); got != ActivityWorking {
		t.Fatalf("activity = %q, want working (nothing blocks any more)", got)
	}
}

// noteGap clears the pending-tool set but MUST keep the emitted-part dedup set, so a
// reseed after a gap still emits no duplicate rows.
func TestOpencodeFoldNoteGapKeepsDedup(t *testing.T) {
	lines := readJSONL(t, "testdata/jsonl/opencode_turn.jsonl")
	f := newOpencodeFold()
	for _, ln := range lines {
		f.applyLine(ln)
	}
	if got := f.drainMessages(); len(got) != 5 {
		t.Fatalf("first drain = %d rows, want 5", len(got))
	}
	f.noteGap()
	for _, ln := range lines {
		f.applyLine(ln)
	}
	if got := f.drainMessages(); len(got) != 0 {
		t.Fatalf("post-gap reseed drain = %d rows, want 0 (dedup survives a gap):\n%s", len(got), formatOpencodeRows(got))
	}
}

// ---- opencode fold: correctness edge cases (tolerant parsing / no fabricated rows) ----

// An ask with NO id stays on the display-only status-row path: the id is both the wire
// address the approvals verb resolves and the key a replied event clears, so an id-less ask
// could never be answered NOR retired — tracking it would strand the session at
// needs_approval forever. Its dedup slot is keyed on the row's content, so a reseed replay
// (which also carries no id) emits exactly one row, not one per replay.
func TestOpencodeFoldIDLessAskStaysDisplayOnly(t *testing.T) {
	f := newOpencodeFold()
	line := []byte(`{"type":"permission.asked","properties":{"sessionID":"s","permission":"bash","patterns":["ls"]}}`)
	f.applyLine(line)
	f.applyLine(line) // reseed replay of the identical, id-less ask
	got := f.drainMessages()
	if len(got) != 1 {
		t.Fatalf("id-less permission.asked replayed twice emitted %d rows, want 1 (content-keyed dedup):\n%s", len(got), formatOpencodeRows(got))
	}
	if got[0].Role != feedRoleSystem || got[0].Type != feedTypeStatus {
		t.Fatalf("row = (%s/%s), want (system/status)", got[0].Role, got[0].Type)
	}
	if got[0].Approval != nil {
		t.Fatalf("an id-less ask must not carry an approval block: %+v", got[0].Approval)
	}
	if n := f.openApprovals(); n != 0 {
		t.Fatalf("openApprovals = %d, want 0 (an unclearable ask must not block the session)", n)
	}
	if got := f.activity(); got != ActivityUnknown {
		t.Fatalf("activity = %q, want unknown (a display-only ask confirms nothing)", got)
	}
}

// Fix 2: a tool part whose state.status is unrecognized is tolerantly ignored — it must not
// confirm activity, touch the pending set, or emit.
func TestOpencodeFoldUnknownToolStateIgnored(t *testing.T) {
	f := newOpencodeFold()
	line := []byte(`{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","messageID":"m1","type":"tool","tool":"bash","callID":"c1","state":{"status":"bogus","input":{"command":"ls"}}}},"time":1784613621168}`)
	if f.applyLine(line) {
		t.Fatal("an unrecognized tool state must not advance state (applyLine=false)")
	}
	if got := f.activity(); got != ActivityUnknown {
		t.Fatalf("activity after unknown tool state = %q, want unknown (no confirm)", got)
	}
	if len(f.pending) != 0 {
		t.Fatalf("unknown tool state mutated the pending set: %v", f.pending)
	}
	if got := f.drainMessages(); len(got) != 0 {
		t.Fatalf("unknown tool state emitted %d rows, want 0:\n%s", len(got), formatOpencodeRows(got))
	}
}

// Fix 3: a synthetic/ignored snapshot for a partID that was cached as a normal partial must
// DROP the cached partial so message-completion can never flush the stale text.
func TestOpencodeFoldSyntheticSnapshotDropsCachedPartial(t *testing.T) {
	f := newOpencodeFold()
	f.applyLine([]byte(`{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"assistant","time":{"created":1784613627000}}}}`))
	// A normal partial (non-empty, no time.end) → cached, not emitted.
	f.applyLine([]byte(`{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","messageID":"m1","type":"text","text":"stale partial","time":{"start":1784613627679}}},"time":1784613627679}`))
	if got := f.drainMessages(); len(got) != 0 {
		t.Fatalf("partial (no time.end) drained %d rows, want 0", len(got))
	}
	// A later SYNTHETIC snapshot for the SAME partID must drop the cached partial.
	f.applyLine([]byte(`{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","messageID":"m1","type":"text","text":"stale partial","synthetic":true,"time":{"start":1784613627679}}},"time":1784613627680}`))
	// The message completes — the dropped partial must NOT be flushed.
	f.applyLine([]byte(`{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"assistant","time":{"created":1784613627000,"completed":1784613627684}}}}`))
	if got := f.drainMessages(); len(got) != 0 {
		t.Fatalf("a synthetic snapshot must drop the cached partial; got %d rows:\n%s", len(got), formatOpencodeRows(got))
	}
}

// Fix 4: permission.asked / question.asked with absent/empty content must not emit a
// fabricated row, and must report applyLine=false. Under the approvals fold this also means
// a hollow ask creates NO pending state: it cannot block the session at needs_approval.
func TestOpencodeFoldEmptyAskNoRow(t *testing.T) {
	cases := []struct {
		name string
		line []byte
	}{
		{"permission.asked with no permission kind", []byte(`{"type":"permission.asked","properties":{"sessionID":"s"}}`)},
		{"question.asked with no questions", []byte(`{"type":"question.asked","properties":{"sessionID":"s"}}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newOpencodeFold()
			if f.applyLine(c.line) {
				t.Fatalf("%s must return false", c.name)
			}
			if got := f.drainMessages(); len(got) != 0 {
				t.Fatalf("%s emitted %d rows, want 0:\n%s", c.name, len(got), formatOpencodeRows(got))
			}
			if n := f.openApprovals(); n != 0 {
				t.Fatalf("%s opened %d approvals, want 0", c.name, n)
			}
		})
	}
}

// Fix 5: a text part owned by a message whose role is neither user nor assistant must not be
// emitted (the feed contract carries only user/assistant/tool/system roles).
func TestOpencodeFoldUnknownMessageRoleNotEmitted(t *testing.T) {
	f := newOpencodeFold()
	f.applyLine([]byte(`{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"bogus","time":{"created":1784613627000}}}}`))
	f.applyLine([]byte(`{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","messageID":"m1","type":"text","text":"should not emit","time":{"start":1784613627679,"end":1784613627681}}},"time":1784613627681}`))
	f.applyLine([]byte(`{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"bogus","time":{"created":1784613627000,"completed":1784613627684}}}}`))
	if got := f.drainMessages(); len(got) != 0 {
		t.Fatalf("a part owned by a non-user/assistant message must not emit; got %d rows:\n%s", len(got), formatOpencodeRows(got))
	}
}

// Fix 6: a text part with an id but NO messageID can never be role-resolved, so it must not
// be cached (and applyLine reports false).
func TestOpencodeFoldOwnerlessPartNotCached(t *testing.T) {
	f := newOpencodeFold()
	line := []byte(`{"type":"message.part.updated","properties":{"sessionID":"s","part":{"id":"p1","type":"text","text":"orphan","time":{"start":1784613627679,"end":1784613627681}}},"time":1784613627681}`)
	if f.applyLine(line) {
		t.Fatal("a part with no messageID must return false (never role-resolvable)")
	}
	if len(f.parts) != 0 || len(f.partOrder) != 0 {
		t.Fatalf("ownerless part was cached: parts=%d partOrder=%d", len(f.parts), len(f.partOrder))
	}
	if got := f.drainMessages(); len(got) != 0 {
		t.Fatalf("ownerless part emitted %d rows, want 0", len(got))
	}
}

// Fix 7: replaying already-emitted snapshots on a reseed must not re-cache them — partOrder
// (and the parts map) must stay bounded rather than growing on every reconnect.
func TestOpencodeFoldReseedDoesNotGrowCache(t *testing.T) {
	lines := readJSONL(t, "testdata/jsonl/opencode_turn.jsonl")
	f := newOpencodeFold()
	for _, ln := range lines {
		f.applyLine(ln)
	}
	f.drainMessages()
	orderAfterFirst := len(f.partOrder)
	partsAfterFirst := len(f.parts)
	if orderAfterFirst == 0 {
		t.Fatal("precondition: the first fold should have appended text/reasoning parts to partOrder")
	}
	// Reseed the identical history (a reconnect replay — no reset()).
	for _, ln := range lines {
		f.applyLine(ln)
	}
	if got := f.drainMessages(); len(got) != 0 {
		t.Fatalf("reseed emitted %d rows, want 0:\n%s", len(got), formatOpencodeRows(got))
	}
	if len(f.partOrder) != orderAfterFirst {
		t.Fatalf("reseed grew partOrder from %d to %d (already-emitted parts were re-cached)", orderAfterFirst, len(f.partOrder))
	}
	if len(f.parts) != partsAfterFirst {
		t.Fatalf("reseed grew the parts cache from %d to %d", partsAfterFirst, len(f.parts))
	}
}

// Fix 8: an epoch-millis value that would expand to a year outside RFC3339's range yields ""
// (the ring stamps it), never a non-RFC3339 expanded-year string.
func TestOpencodeTSOutOfRange(t *testing.T) {
	cases := []struct {
		name      string
		ms        int64
		wantEmpty bool
	}{
		{"year out of RFC3339 range", 1 << 62, true},
		{"zero", 0, true},
		{"negative", -5, true},
		// A normal in-range value still converts.
		{"normal in-range value", 1784613627681, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := opencodeTS(c.ms)
			if c.wantEmpty && got != "" {
				t.Fatalf("opencodeTS(%d) = %q, want \"\"", c.ms, got)
			}
			if !c.wantEmpty && got == "" {
				t.Fatalf("opencodeTS(%d) must convert, got empty", c.ms)
			}
		})
	}
}

// Fix 9: a message.updated that changes nothing meaningful (a repeat, or an id-only frame)
// must return false — feed-tracking that did not advance is not an event.
func TestOpencodeFoldNoOpMessageUpdatedReturnsFalse(t *testing.T) {
	f := newOpencodeFold()
	// First sighting with a role advances state.
	if !f.applyLine([]byte(`{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"assistant","time":{"created":1784613627000}}}}`)) {
		t.Fatal("first message.updated (new role) should advance state")
	}
	// Re-emitting the same info (nothing new) must NOT advance state.
	if f.applyLine([]byte(`{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m1","role":"assistant","time":{"created":1784613627000}}}}`)) {
		t.Fatal("a repeated message.updated (nothing new) must return false")
	}
	// An id-only frame (no role, no times) is likewise not activity-relevant.
	if f.applyLine([]byte(`{"type":"message.updated","properties":{"sessionID":"s","info":{"id":"m2"}}}`)) {
		t.Fatal("an id-only message.updated must return false")
	}
}

// ---- tolerance: malformed / unknown / partial lines never break the fold ----

func TestFoldsToleratePathologicalLines(t *testing.T) {
	for _, tc := range []struct {
		name string
		fold activityFold
	}{
		{"opencode", newOpencodeFold()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := [][]byte{
				[]byte(`not json at all`),
				[]byte(`{"type":`),                        // truncated
				[]byte(`{"type":"totally_unknown_type"}`), // unknown type
				[]byte(`{}`),                              // empty object
				[]byte(``),                                // empty
				[]byte(`{"type":"event_msg","payload":{"type":"token_count"}}`), // noise
			}
			for _, b := range bad {
				if tc.fold.applyLine(b) {
					t.Errorf("line %q should not advance state", b)
				}
			}
			if got := tc.fold.activity(); got != ActivityUnknown {
				t.Fatalf("activity after only-noise = %q, want unknown", got)
			}
		})
	}
}

// ---- shared file helper ----

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The JSONL line-tailer suite (partial buffering, the oversized-line cap, truncation and
// inode swaps, bounded catch-up, permission tolerance, the same-size/growing rewrite
// tripwires and the exact-boundary catch-up case) lived here. The tailer was the codex
// rollout lane's transport and had no other consumer, so it went with that lane in A6
// (charliek/shed#322).

// ---- merge precedence ----

// TestFileWatcherFreshnessSettledVsWorkingGrace, which drove the settled/working-grace
// rule through a tailing fileWatcher, went with the tailer in A6 (charliek/shed#322).
// watcherFreshness itself survives — it is the shared rule the opencode watcher applies
// (watch_opencode_transport_test.go covers it on that transport).

// S2 (charliek/shed#324) reduced mergedActivity to two arms: a FRESH watcher verdict,
// and "" for everything else. The stability argument and the expired-working arm went
// with the pane-stability engine (the arm's expiry clock WAS that engine's quiet
// period), so an opencode row whose SSE feed dies mid-turn goes to *unknown* rather
// than sitting at `working` forever.
func TestMergedActivityPrecedence(t *testing.T) {
	// Arm 1 — a fresh watcher verdict wins, message and all.
	if a, m := mergedActivity(ActivityWorking, "hello", true); a != ActivityWorking || m != "hello" {
		t.Fatalf("fresh watcher merge = (%q,%q), want (working,hello)", a, m)
	}
	if a, m := mergedActivity(ActivityNeedsApproval, "tool", true); a != ActivityNeedsApproval || m != "tool" {
		t.Fatalf("fresh needs_approval merge = (%q,%q), want (needs_approval,tool)", a, m)
	}
	// Arm 2 — everything else is no activity at all, and the message goes with it:
	// a stale verdict, an expired-working one, and no watcher (unknown) alike.
	for _, c := range []struct {
		name     string
		activity Activity
	}{
		{"stale non-working verdict", ActivityUnknown},
		{"expired-working verdict", ActivityWorking},
		{"stale needs_input verdict", ActivityNeedsInput},
		{"no watcher at all", ""},
	} {
		if a, m := mergedActivity(c.activity, "hello", false); a != "" || m != "" {
			t.Fatalf("%s merge = (%q,%q), want (\"\",\"\")", c.name, a, m)
		}
	}
}

// The codex file-correlation suite (two sessions in one workdir, the back-written-id
// exact path) went with the codex rollout tail in A6 (charliek/shed#322).

// ---- env round trip: back-write + read ----

// envRecRunner records set-environment and answers show-environment from a map.
type envRecRunner struct{ env map[string]string }

func (r *envRecRunner) Run(args ...string) Result {
	switch args[0] {
	case "set-environment":
		// set-environment -t <name> <KEY> <VAL>
		if len(args) >= 5 {
			r.env[args[3]] = args[4]
		}
		return Result{}
	case "show-environment":
		var b strings.Builder
		for k, v := range r.env {
			if strings.HasPrefix(k, envPrefix) {
				fmt.Fprintf(&b, "%s=%s\n", k, v)
			}
		}
		return Result{Stdout: b.String()}
	}
	return Result{}
}

func TestBackWriteAgentSessionRoundTrip(t *testing.T) {
	r := &envRecRunner{env: map[string]string{}}
	if got := agentSessionEnv(r, "rc-x"); got != "" {
		t.Fatalf("initial agent session = %q, want empty", got)
	}
	backWriteAgentSession(r, "rc-x", "sess-123")
	if got := agentSessionEnv(r, "rc-x"); got != "sess-123" {
		t.Fatalf("after back-write = %q, want sess-123", got)
	}
	// Control chars are rejected (never stamped).
	backWriteAgentSession(r, "rc-x", "bad\nvalue")
	if got := agentSessionEnv(r, "rc-x"); got != "sess-123" {
		t.Fatalf("control-char id should be rejected, env = %q", got)
	}
}

func TestOpencodePortEnv(t *testing.T) {
	cases := []struct {
		name    string
		raw     string // "" means the key is never set
		wantOK  bool
		wantVal int
	}{
		{"round-trip", "4096", true, 4096},
		{"missing", "", false, 0},
		{"non-numeric", "abc", false, 0},
		{"zero-out-of-range", "0", false, 0},
		{"above-max-out-of-range", "70000", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &envRecRunner{env: map[string]string{}}
			if c.raw != "" {
				r.env[envOpencodePort] = c.raw
			}
			gotVal, gotOK := opencodePortEnv(r, "rc-x")
			if gotOK != c.wantOK || gotVal != c.wantVal {
				t.Errorf("opencodePortEnv() = (%d, %v), want (%d, %v)", gotVal, gotOK, c.wantVal, c.wantOK)
			}
		})
	}
}

// The tailer rewrite tripwires, the exact-boundary catch-up case, the codex gap fold
// and the ambiguous-correlation back-write deferral all went with the codex rollout
// tail in A6 (charliek/shed#322).

// ---- fsNudger forgets removed dirs so a recreation is re-watchable ----

func TestFSNudgerForgetDirAllowsReAdd(t *testing.T) {
	n, err := newFSNudger(nil, func(string, ...any) {})
	if err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	}
	defer n.w.Close()

	dir := t.TempDir()
	sub := filepath.Join(dir, "child")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	n.addDir(dir)
	n.addDir(sub)
	n.mu.Lock()
	both := n.added[dir] && n.added[sub]
	n.mu.Unlock()
	if !both {
		t.Fatal("precondition: both dirs recorded as added")
	}

	// Forgetting the parent must drop it AND its children (a removed tree takes its
	// subdirs with it).
	n.forgetDir(dir)
	n.mu.Lock()
	stillDir, stillSub := n.added[dir], n.added[sub]
	n.mu.Unlock()
	if stillDir || stillSub {
		t.Fatalf("forgetDir left entries behind: dir=%v sub=%v", stillDir, stillSub)
	}

	// A recreation at the same path can now be re-added (the dedupe no longer blocks).
	n.addDir(dir)
	n.mu.Lock()
	readded := n.added[dir]
	n.mu.Unlock()
	if !readded {
		t.Fatal("re-add after forget must succeed")
	}
}

// ---- fsnotify nudge layer (best-effort latency) ----

func TestFSNudgerNudgesOnChange(t *testing.T) {
	root := t.TempDir()
	// A dated subdir created AFTER the nudger starts must still be watched (fsnotify is
	// non-recursive; the Create handler adds it).
	n, err := newFSNudger([]string{root}, func(string, ...any) {})
	if err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.run(ctx)

	// Give run() a moment to add the root watch.
	time.Sleep(50 * time.Millisecond)
	sub := filepath.Join(root, "2026", "07", "11")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sub, "rollout-x.jsonl"), "hi\n")

	select {
	case <-n.nudge:
	case <-time.After(3 * time.Second):
		t.Fatal("expected a nudge on a file change under a watched tree")
	}
}

// The end-to-end "a correlated codex watcher overrides pane stability" cell went with
// the codex rollout tail in A6 (charliek/shed#322); its opencode twin
// (TestReconcileOpencodeWatcherOverridesStability, below) is the surviving proof.

// ---- opencode watcher wire-in (C5) ----

// watchableKind must admit opencode (so ensureWatcher's first guard lets it through to
// the SSE/REST arm) and NOTHING else: A6 (charliek/shed#322) retired the codex rollout
// tail and the cursor hook-ingest lane, so both of those kinds are stability-only now,
// exactly like shell.
func TestWatchableKindOpencodeOnly(t *testing.T) {
	if !watchableKind(KindOpencode) {
		t.Fatal("watchableKind(KindOpencode) = false, want true")
	}
	for _, k := range []Kind{KindCodex, KindCursor, KindClaudeRC, KindClaudeBroker, KindShell} {
		if watchableKind(k) {
			t.Errorf("watchableKind(%q) = true, want false (no activity source)", k)
		}
	}
}

// A pre-upgrade opencode session (created before the port plumbing shipped, so no
// SHED_RC_OPENCODE_PORT is stamped) is unwatchable over the SSE transport: ensureWatcher
// returns no watcher, and since S2 (charliek/shed#324) there is no fallback behind it —
// the row is tracked (so /messages answers 200-empty) but carries no activity.
func TestReconcileOpencodeNoPortIsTrackedWithNoActivity(t *testing.T) {
	tm := newHubTmux()
	env := strings.Join([]string{
		envV + "=2",
		envID + "=id-oc-legacy",
		envKind + "=" + string(KindOpencode),
		envWorkdir + "=/home/shed",
		// deliberately NO SHED_RC_OPENCODE_PORT
	}, "\n") + "\n"
	tm.set("rc-ocleg1", "opencode\nAsk anything...", env)

	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newTestHub(tm, clk)

	h.reconcile()

	h.trackMu.Lock()
	tr := h.tracked["ocleg1"]
	h.trackMu.Unlock()
	if tr == nil {
		t.Fatal("opencode session not tracked")
	}
	if tr.watcher != nil {
		t.Fatal("a session with no recorded port must get NO watcher")
	}
	if tr.activity != "" {
		t.Fatalf("activity = %q, want none (no watcher, no fallback)", tr.activity)
	}
}

// End-to-end: a correlated opencode SSE watcher supplies the activity, populates the
// message ring, and its discovered session id is back-written into the tmux env. Mirrors
// TestReconcileCodexWatcherOverridesStability but drives the fake opencode HTTP+SSE server
// (from watch_opencode_transport_test.go) over the hub's real reconcile loop.
func TestReconcileOpencodeWatcherOverridesStability(t *testing.T) {
	f := newFakeOpencode(t)
	// A fresh session: no candidate list; status reports busy during the turn; the SSE
	// fixture arc drives the pin (session.created dir-match), the feed, and the activity arc.
	f.statusBody = fmt.Sprintf(`{%q:{"type":"busy"}}`, ocFixtureSID)
	frames := fixtureFrames(t)
	f.onEvent = func(conn int64, w io.Writer, flush func(), ctx context.Context) {
		writeSSE(w, flush, sseServerConnected)
		for _, fr := range frames {
			writeSSE(w, flush, fr)
		}
		<-ctx.Done() // hold the connection open (no reconnect churn)
	}

	tm := newHubTmux()
	// A live opencode session whose workdir matches the fixture directory (so the SSE
	// transport pins on the fixture's session.created) and whose recorded port targets
	// the fake server. Since S2 (charliek/shed#324) there is no other activity source
	// at all, so a needs_input verdict can only have come from the SSE watcher.
	env := strings.Join([]string{
		envV + "=2",
		envID + "=id-oc",
		envKind + "=" + string(KindOpencode),
		envWorkdir + "=" + ocFixtureDir,
		envOpencodePort + "=" + strconv.Itoa(f.port(t)),
	}, "\n") + "\n"
	tm.set("rc-oc0001", "opencode\nAsk anything...", env)

	clk := opencodeClock() // fixed instant; never advanced
	h := newTestHub(tm, clk)
	// The SSE watcher runs a background goroutine — close every tracked watcher on teardown
	// (before the fake server's own cleanup, LIFO) so the goroutine exits, no leak.
	t.Cleanup(func() {
		h.trackMu.Lock()
		defer h.trackMu.Unlock()
		for _, tr := range h.tracked {
			if tr.watcher != nil {
				tr.watcher.close()
			}
		}
	})

	// Poll the reconcile loop (real sleeps, frozen clock) until the async SSE watcher has
	// folded the whole arc: activity settles to needs_input AND the ring holds the 5 rows.
	var tr *trackedSession
	var msgs []feedMessage
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.reconcile()
		h.trackMu.Lock()
		tr = h.tracked["oc0001"]
		h.trackMu.Unlock()
		if tr != nil {
			msgs, _ = tr.ring.since(0, 10)
			if tr.activity == ActivityNeedsInput && len(msgs) >= 5 {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tr == nil {
		t.Fatal("opencode session was never tracked")
	}

	h.trackMu.Lock()
	activity := tr.activity
	lastMessage := tr.lastMessage
	watcher := tr.watcher
	h.trackMu.Unlock()

	if activity != ActivityNeedsInput {
		t.Fatalf("activity = %q, want needs_input (SSE watcher override)", activity)
	}
	if _, ok := watcher.(*opencodeWatcher); !ok {
		t.Fatalf("watcher = %T, want *opencodeWatcher", watcher)
	}
	if lastMessage != "3 .txt files." {
		t.Fatalf("last_message = %q, want %q", lastMessage, "3 .txt files.")
	}

	// The ring holds the normalized turn in order: user → reasoning → tool_use →
	// tool_result → assistant.
	want := []opencodeFeedRow{
		{role: feedRoleUser, typ: feedTypeText, textPrefix: "Use the bash tool"},
		{role: feedRoleAssistant, typ: feedTypeReasoning, textPrefix: "The user wants"},
		{role: feedRoleTool, typ: feedTypeToolUse, toolName: "bash", detailHas: "ls"},
		{role: feedRoleTool, typ: feedTypeToolResult, toolName: "bash", detailHas: "a.txt"},
		{role: feedRoleAssistant, typ: feedTypeText, textPrefix: "3 .txt files."},
	}
	assertOpencodeRows(t, msgs, want)

	// The SSE-discovered session id was back-written into the tmux env for exact
	// re-correlation on a hub restart (drainConfirmedAgentID → backWriteAgentSession).
	wantEnv := envAgentSession + "=" + ocFixtureSID
	if !slices.Contains(tm.setEnvCalls(), wantEnv) {
		t.Fatalf("set-environment calls = %v, want one == %q", tm.setEnvCalls(), wantEnv)
	}
}

// The producer→wire seam: the rows the opencode fold emits must be legal for the surfaces
// that consume them — the id addressable by the approvals verb (ApprovalIDRe, which the server
// proxy mirrors), the advertised decisions inside the contract's enum, and the omitempty rules
// intact after the ring's sanitize/copy (no `decision` on a pending row, no `decisions` on a
// resolved one).
func TestOpencodeApprovalRowsAreWireLegal(t *testing.T) {
	f := newOpencodeFold()
	f.applyLine([]byte(`{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}`))
	f.applyLine([]byte(`{"type":"permission.asked","properties":{"id":"per_f8342bca6001","sessionID":"s","permission":"bash","patterns":["ls"],"metadata":{"command":"ls"}}}`))
	f.applyLine([]byte(`{"type":"permission.replied","properties":{"sessionID":"s","requestID":"per_f8342bca6001","reply":"always"}}`))

	ring := newMessageRing()
	for _, m := range f.drainMessages() {
		ring.append(m, time.Unix(1_700_000_000, 0).UTC())
	}
	rows, _ := ring.since(0, 10)
	if len(rows) != 2 {
		t.Fatalf("ring holds %d rows, want the pending + resolved pair:\n%s", len(rows), formatOpencodeRows(rows))
	}
	pending, resolved := rows[0], rows[1]
	if !ApprovalIDRe.MatchString(pending.Approval.ID) {
		t.Errorf("approval id %q does not match the contract grammar — the verb could never address it", pending.Approval.ID)
	}
	for _, d := range pending.Approval.Decisions {
		if !validApprovalDecision(d) {
			t.Errorf("advertised decision %q is outside the contract enum", d)
		}
	}
	raw, err := json.Marshal(pending)
	if err != nil {
		t.Fatalf("marshal pending row: %v", err)
	}
	if strings.Contains(string(raw), `"decision"`) {
		t.Errorf("pending row must omit `decision`: %s", raw)
	}
	raw, err = json.Marshal(resolved)
	if err != nil {
		t.Fatalf("marshal resolved row: %v", err)
	}
	if strings.Contains(string(raw), `"decisions"`) {
		t.Errorf("resolved row must omit `decisions` (nothing left to choose): %s", raw)
	}
	if !strings.Contains(string(raw), `"decision":"`+approvalDecisionAllowAlways+`"`) {
		t.Errorf("resolved row must carry the mapped decision: %s", raw)
	}
}

// The REOPEN rule: a seed-retired ask (resolved with NO decision — retired only because a REST
// snapshot did not list it) is REOPENED by a later ask replay, which is newer and stronger
// evidence that it is genuinely open. Without this, one stale/racing GET /permission would
// silently un-block a session that is still waiting on the operator. An ask genuinely answered
// (resolved with a KNOWN decision) is NOT reopened.
func TestOpencodeFoldSeedRetiredAskReopens(t *testing.T) {
	ask := []byte(`{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}`)
	emptySeed := []byte(`{"type":"` + ocApprovalSeedType + `","properties":{"sessionID":"s","permissionsKnown":true,"questionsKnown":true}}`)

	f := newOpencodeFold()
	f.applyLine([]byte(`{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}`))
	f.applyLine(ask)
	f.applyLine(emptySeed) // a stale snapshot retires it
	if got := f.activity(); got != ActivityWorking {
		t.Fatalf("after the retiring seed = %q, want working", got)
	}
	f.drainMessages()

	// The next reseed replays the ask: it is still open after all.
	if !f.applyLine(ask) {
		t.Fatal("an ask replay for a seed-retired entry must reopen it (state advanced)")
	}
	if got := f.activity(); got != ActivityNeedsApproval {
		t.Fatalf("after the reopening ask = %q, want needs_approval", got)
	}
	if pend := f.pendingApprovals(); len(pend) != 1 || pend[0].ID != "per_1" {
		t.Fatalf("pendingApprovals = %+v, want per_1 open again", pend)
	}
	status, _, ok := f.approvalState("per_1")
	if !ok || status != approvalStatusPending {
		t.Fatalf("approvalState = (%q,%v), want (pending,true)", status, ok)
	}
	// The client needs the pending row again to render the buttons, so it is re-announced.
	got := f.drainMessages()
	if len(got) != 1 || got[0].Approval == nil || got[0].Approval.Status != approvalStatusPending ||
		len(got[0].Approval.Decisions) != 3 {
		t.Fatalf("reopen rows = %s, want one re-announced pending approval row", formatOpencodeRows(got))
	}
	// A real reply closes it, and now an ask replay must NOT reopen it.
	f.applyLine([]byte(`{"type":"permission.replied","properties":{"sessionID":"s","requestID":"per_1","reply":"once"}}`))
	f.drainMessages()
	if f.applyLine(ask) {
		t.Error("an ask replay must not reopen an entry resolved with a known decision")
	}
	if got := f.openApprovals(); got != 0 {
		t.Fatalf("openApprovals = %d, want 0 (a genuinely answered ask stays closed)", got)
	}
}

// The seed marker's two halves carry INDEPENDENT authority: a failed GET /question must not
// block permission healing (and vice versa), or one persistently failing read would strand
// every answered approval as pending forever.
func TestOpencodeFoldApprovalSeedHalvesAreIndependent(t *testing.T) {
	newFold := func(t *testing.T) *opencodeFold {
		t.Helper()
		f := newOpencodeFold()
		f.applyLine([]byte(`{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}`))
		f.applyLine([]byte(`{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["ls"]}}`))
		f.applyLine([]byte(`{"type":"question.asked","properties":{"id":"que_1","sessionID":"s","questions":[{"header":"Which file?"}]}}`))
		f.drainMessages()
		return f
	}

	t.Run("permissions heal while the question read failed", func(t *testing.T) {
		f := newFold(t)
		f.applyLine([]byte(`{"type":"` + ocApprovalSeedType + `","properties":{"sessionID":"s","permissionsKnown":true}}`))
		if status, _, _ := f.approvalState("per_1"); status != approvalStatusResolved {
			t.Errorf("per_1 = %q, want resolved (its half's read succeeded)", status)
		}
		if len(f.pendingQuestions) != 1 {
			t.Errorf("pendingQuestions = %v, want the question retained (its read failed)", f.pendingQuestions)
		}
		if got := f.activity(); got != ActivityNeedsApproval {
			t.Errorf("activity = %q, want needs_approval (the question still blocks)", got)
		}
	})

	t.Run("questions heal while the permission read failed", func(t *testing.T) {
		f := newFold(t)
		f.applyLine([]byte(`{"type":"` + ocApprovalSeedType + `","properties":{"sessionID":"s","questionsKnown":true}}`))
		if len(f.pendingQuestions) != 0 {
			t.Errorf("pendingQuestions = %v, want empty (its half's read succeeded)", f.pendingQuestions)
		}
		if status, _, _ := f.approvalState("per_1"); status != approvalStatusPending {
			t.Errorf("per_1 = %q, want pending (a failed read is never 'nothing is open')", status)
		}
	})

	t.Run("a marker with neither half known retires nothing", func(t *testing.T) {
		f := newFold(t)
		if f.applyLine([]byte(`{"type":"` + ocApprovalSeedType + `","properties":{"sessionID":"s"}}`)) {
			t.Error("a marker with no authority must not advance state")
		}
		if got := f.openApprovals(); got != 2 {
			t.Errorf("openApprovals = %d, want 2 (both asks retained)", got)
		}
	})
}
