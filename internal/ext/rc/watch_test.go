package rc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// ---- fixture-driven fold: the sanitized live captures parse to the expected arcs ----

func TestCodexFoldFixtureArc(t *testing.T) {
	lines := readJSONL(t, "testdata/jsonl/codex_turn.jsonl")
	f := newCodexFold()

	// Before any confirming event the verdict is unknown.
	if got := f.activity(); got != ActivityUnknown {
		t.Fatalf("initial activity = %q, want unknown", got)
	}

	sawWorking, sawPendingTool := false, false
	for _, ln := range lines {
		f.applyLine(ln)
		if f.activity() == ActivityWorking {
			sawWorking = true
		}
		if len(f.pending) > 0 {
			sawPendingTool = true // a tool call is open (custom_tool_call) → working
		}
	}
	if !sawWorking {
		t.Error("expected a working verdict during the turn")
	}
	if !sawPendingTool {
		t.Error("expected an open tool call (pending) mid-turn")
	}
	// The arc ends at task_complete → needs_input, settled, with the final answer.
	if got := f.activity(); got != ActivityNeedsInput {
		t.Fatalf("final activity = %q, want needs_input", got)
	}
	if !f.settled() {
		t.Error("final verdict should be settled")
	}
	if got := f.lastMessage(); got != "2+2 equals 4." {
		t.Fatalf("last_message = %q, want %q", got, "2+2 equals 4.")
	}
}

func TestClaudeFoldFixtureArc(t *testing.T) {
	lines := readJSONL(t, "testdata/jsonl/claude_turn.jsonl")
	f := newClaudeFold()

	if got := f.activity(); got != ActivityUnknown {
		t.Fatalf("initial activity = %q, want unknown", got)
	}

	// Step the arc; the mid-turn tool_use must never leave a stale needs_input verdict.
	var seq []Activity
	for _, ln := range lines {
		if f.applyLine(ln) {
			seq = append(seq, f.activity())
		}
	}
	// Working must appear (prompt / tool_use / tool_result) before the final tail.
	working := false
	for _, a := range seq {
		if a == ActivityWorking {
			working = true
		}
	}
	if !working {
		t.Errorf("expected working during the turn, got %v", seq)
	}
	// The tool_use block (before its result) must be a working verdict, never a flapped
	// needs_input — the stop_reason refinement guards the split text/tool_use lines.
	if got := f.activity(); got != ActivityNeedsInput {
		t.Fatalf("final activity = %q, want needs_input", got)
	}
	if !f.settled() {
		t.Error("final verdict should be settled")
	}
	if got := f.lastMessage(); got != "Done — the command printed `hello-from-claude`." {
		t.Fatalf("last_message = %q", got)
	}
}

// The mid-turn split (assistant text with stop_reason:"tool_use", then a tool_use
// line) must read working the whole way — no transient needs_input flap.
func TestClaudeFoldNoMidTurnFlap(t *testing.T) {
	f := newClaudeFold()
	f.applyLine([]byte(`{"type":"user","message":{"role":"user","content":"hi"}}`))
	if got := f.activity(); got != ActivityWorking {
		t.Fatalf("after prompt = %q, want working", got)
	}
	// Text block carrying stop_reason tool_use (more of the turn follows).
	f.applyLine([]byte(`{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"text","text":"working on it"}]}}`))
	if got := f.activity(); got != ActivityWorking {
		t.Fatalf("mid-turn text = %q, want working (no flap)", got)
	}
	f.applyLine([]byte(`{"type":"assistant","message":{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`))
	if got := f.activity(); got != ActivityWorking {
		t.Fatalf("tool_use = %q, want working", got)
	}
	f.applyLine([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}}`))
	if got := f.activity(); got != ActivityWorking {
		t.Fatalf("tool_result = %q, want working (turn continues)", got)
	}
	f.applyLine([]byte(`{"type":"assistant","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"all done"}]}}`))
	if got := f.activity(); got != ActivityNeedsInput {
		t.Fatalf("end_turn text = %q, want needs_input", got)
	}
	if got := f.lastMessage(); got != "all done" {
		t.Fatalf("last_message = %q, want %q", got, "all done")
	}
}

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

// permission.asked is a display-only status feed row: it emits one system/status row and
// does NOT change the activity verdict (which stays whatever session.status last said).
func TestOpencodeFoldPermissionStatusRow(t *testing.T) {
	f := newOpencodeFold()
	f.applyLine([]byte(`{"type":"session.status","properties":{"sessionID":"s","status":{"type":"busy"}}}`))
	if got := f.activity(); got != ActivityWorking {
		t.Fatalf("after busy = %q, want working", got)
	}
	f.applyLine([]byte(`{"type":"permission.asked","properties":{"id":"per_1","sessionID":"s","permission":"bash","patterns":["rm -rf /tmp/x","ls"]}}`))
	// Activity is unaffected by the permission ask.
	if got := f.activity(); got != ActivityWorking {
		t.Fatalf("after permission.asked = %q, want working (unaffected)", got)
	}
	got := f.drainMessages()
	if len(got) != 1 {
		t.Fatalf("drained %d rows, want 1:\n%s", len(got), formatOpencodeRows(got))
	}
	m := got[0]
	if m.Role != feedRoleSystem || m.Type != feedTypeStatus {
		t.Fatalf("row = (%s/%s), want (system/status)", m.Role, m.Type)
	}
	if !strings.Contains(m.Text, "awaiting approval: bash") || !strings.Contains(m.Text, "rm -rf /tmp/x") {
		t.Fatalf("status text = %q, want it to name the permission + patterns", m.Text)
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

// Fix 1: a permission.asked with NO id keys its dedup slot on the row's content, so a
// reseed replay (which also carries no id) emits exactly one status row, not one per replay.
func TestOpencodeFoldStatusRowDedupNoID(t *testing.T) {
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
// fabricated row, and must report applyLine=false.
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
		{"codex", newCodexFold()},
		{"claude", newClaudeFold()},
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

// ---- lineTailer: partial buffering, oversized cap, truncation, inode swap, catch-up ----

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func linesToStrings(lines [][]byte) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(l)
	}
	return out
}

func TestLineTailerPartialLineBuffering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	writeFile(t, path, "")
	tl := &lineTailer{path: path} // follow-only (no catch-up)
	if lines, _, _, err := tl.poll(); err != nil || len(lines) != 0 {
		t.Fatalf("initial poll: lines=%v err=%v", linesToStrings(lines), err)
	}
	// A half-written line must not be emitted.
	appendFile(t, path, `{"a":1`)
	if lines, _, _, _ := tl.poll(); len(lines) != 0 {
		t.Fatalf("partial line emitted: %v", linesToStrings(lines))
	}
	// Completing it emits exactly the whole line.
	appendFile(t, path, "}\n")
	lines, _, _, _ := tl.poll()
	if got := linesToStrings(lines); len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("completed line = %v, want [{\"a\":1}]", got)
	}
}

func TestLineTailerOversizedLineSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	writeFile(t, path, "")
	tl := &lineTailer{path: path}
	tl.poll() // open at EOF

	big := strings.Repeat("x", tailMaxLine+10)
	appendFile(t, path, big+"\n"+"small\n")
	lines, _, gapped, err := tl.poll()
	if err != nil {
		t.Fatal(err)
	}
	got := linesToStrings(lines)
	if len(got) != 1 || got[0] != "small" {
		t.Fatalf("lines = %v, want [small] (oversized skipped)", got)
	}
	if !gapped {
		t.Fatal("an oversized skip must be reported as a gap")
	}
	// The gap flag is one-shot: the next (quiet) poll reports no gap.
	if _, _, gapped, _ := tl.poll(); gapped {
		t.Fatal("gap must clear after being reported")
	}
}

func TestLineTailerTruncationResets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	writeFile(t, path, "a\nb\n")
	tl := &lineTailer{path: path, catchUp: true}
	if lines, _, _, _ := tl.poll(); len(lines) != 2 {
		t.Fatalf("catch-up read = %v, want [a b]", linesToStrings(lines))
	}
	// Rewrite shorter (truncation in place).
	writeFile(t, path, "c\n")
	lines, didReset, _, _ := tl.poll()
	if !didReset {
		t.Error("truncation should report didReset")
	}
	if got := linesToStrings(lines); len(got) != 1 || got[0] != "c" {
		t.Fatalf("post-truncation = %v, want [c]", got)
	}
}

func TestLineTailerInodeSwapResets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	writeFile(t, path, "a\n")
	tl := &lineTailer{path: path, catchUp: true}
	if lines, _, _, _ := tl.poll(); len(lines) != 1 {
		t.Fatalf("first read = %v, want [a]", linesToStrings(lines))
	}
	// Replace the file with a new inode (remove + recreate).
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "x\n")
	lines, didReset, _, err := tl.poll()
	if err != nil {
		t.Fatal(err)
	}
	if !didReset {
		t.Error("inode swap should report didReset")
	}
	if got := linesToStrings(lines); len(got) != 1 || got[0] != "x" {
		t.Fatalf("post-swap = %v, want [x]", got)
	}
}

func TestLineTailerCatchUpBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	var b strings.Builder
	pad := strings.Repeat("p", 1024)
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "L%03d-%s\n", i, pad) // ~1KB/line → ~200KB > 64KB window
	}
	writeFile(t, path, b.String())

	tl := &lineTailer{path: path, catchUp: true}
	lines, _, _, err := tl.poll()
	if err != nil {
		t.Fatal(err)
	}
	got := linesToStrings(lines)
	if len(got) == 0 || len(got) >= 200 {
		t.Fatalf("catch-up read %d lines, want a bounded tail (<200)", len(got))
	}
	// The most recent line is present; the very first is not (bounded window).
	if !strings.HasPrefix(got[len(got)-1], "L199-") {
		t.Fatalf("last line = %q, want L199", got[len(got)-1])
	}
	if strings.HasPrefix(got[0], "L000-") {
		t.Fatal("catch-up should not include the oldest line")
	}
}

func TestLineTailerPermissionErrorTolerant(t *testing.T) {
	// A non-existent path is a poll error, not a panic; a later create is picked up.
	dir := t.TempDir()
	path := filepath.Join(dir, "later.jsonl")
	tl := &lineTailer{path: path}
	if _, _, _, err := tl.poll(); err == nil {
		t.Fatal("poll of a missing file should error")
	}
	writeFile(t, path, "a\n")
	// follow-only: opening now seeks to EOF, so the pre-existing line is not re-read,
	// but the poll must succeed (no lingering error).
	if _, _, _, err := tl.poll(); err != nil {
		t.Fatalf("poll after create should succeed, got %v", err)
	}
	appendFile(t, path, "b\n")
	lines, _, _, err := tl.poll()
	if err != nil {
		t.Fatal(err)
	}
	if got := linesToStrings(lines); len(got) != 1 || got[0] != "b" {
		t.Fatalf("lines = %v, want [b]", got)
	}
}

// ---- fileWatcher freshness / merge precedence ----

func TestFileWatcherFreshnessSettledVsWorkingGrace(t *testing.T) {
	dir := t.TempDir()

	// Settled (needs_input) stays authoritative even long after the last event.
	settledPath := filepath.Join(dir, "settled.jsonl")
	writeFile(t, settledPath, `{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done"}}`+"\n")
	sw := newFileWatcher(settledPath, true, newCodexFold())
	t0 := time.Unix(1_700_000_000, 0).UTC()
	sw.refresh(t0)
	if a, msg, fresh, _ := sw.snapshot(t0); !fresh || a != ActivityNeedsInput || msg != "done" {
		t.Fatalf("settled snapshot: a=%q msg=%q fresh=%v", a, msg, fresh)
	}
	if _, _, fresh, _ := sw.snapshot(t0.Add(10 * time.Minute)); !fresh {
		t.Fatal("a settled verdict must stay fresh while the file is quiet")
	}

	// Working keeps its authority through the LONG grace (a silent turn is normal),
	// and only past watcherWorkingGrace demotes to expiredWorking (still not dropped —
	// the merge decides against stability's verdict).
	workPath := filepath.Join(dir, "work.jsonl")
	writeFile(t, workPath, `{"type":"event_msg","payload":{"type":"task_started"}}`+"\n")
	ww := newFileWatcher(workPath, true, newCodexFold())
	ww.refresh(t0)
	if a, _, fresh, expired := ww.snapshot(t0); !fresh || expired || a != ActivityWorking {
		t.Fatalf("working snapshot at t0: a=%q fresh=%v expired=%v", a, fresh, expired)
	}
	// Past the 30s window but inside the working grace: still fresh (no flap).
	if _, _, fresh, expired := ww.snapshot(t0.Add(watcherFreshWindow + time.Second)); !fresh || expired {
		t.Fatalf("working inside grace must stay fresh (fresh=%v expired=%v)", fresh, expired)
	}
	// Past the working grace: no longer fresh, flagged expiredWorking.
	if _, _, fresh, expired := ww.snapshot(t0.Add(watcherWorkingGrace + time.Second)); fresh || !expired {
		t.Fatalf("working past grace: fresh=%v expired=%v, want (false,true)", fresh, expired)
	}
}

func TestMergedActivityPrecedence(t *testing.T) {
	// Fresh watcher wins (activity + message).
	if a, m := mergedActivity(ActivityWorking, "hello", true, false, ActivityIdle); a != ActivityWorking || m != "hello" {
		t.Fatalf("fresh watcher merge = (%q,%q), want (working,hello)", a, m)
	}
	// Stale (non-working) watcher → stability drives and the message is dropped.
	if a, m := mergedActivity(ActivityUnknown, "hello", false, false, ActivityIdle); a != ActivityIdle || m != "" {
		t.Fatalf("stale merge = (%q,%q), want (idle,\"\")", a, m)
	}
	// Expired working + stability SETTLED quiet (idle/needs_input) → stability wins.
	if a, m := mergedActivity(ActivityWorking, "hello", false, true, ActivityNeedsInput); a != ActivityNeedsInput || m != "" {
		t.Fatalf("expired-working vs settled stability = (%q,%q), want (needs_input,\"\")", a, m)
	}
	if a, _ := mergedActivity(ActivityWorking, "hello", false, true, ActivityIdle); a != ActivityIdle {
		t.Fatalf("expired-working vs idle stability = %q, want idle", a)
	}
	// Expired working + stability still churning (working) → keep working (no flap).
	if a, m := mergedActivity(ActivityWorking, "hello", false, true, ActivityWorking); a != ActivityWorking || m != "hello" {
		t.Fatalf("expired-working vs churning stability = (%q,%q), want (working,hello)", a, m)
	}
	// Expired working + stability has no verdict → keep working too.
	if a, _ := mergedActivity(ActivityWorking, "hello", false, true, ActivityUnknown); a != ActivityWorking {
		t.Fatalf("expired-working vs unknown stability = %q, want working", a)
	}
}

// ---- cwd encoding ----

func TestEncodeClaudeProject(t *testing.T) {
	cases := map[string]string{
		"/home/shed":            "-home-shed",
		"/home/shed/my.project": "-home-shed-my-project",
		"/Users/dev/code_2":     "-Users-dev-code-2",
	}
	for in, want := range cases {
		if got := encodeClaudeProject(in); got != want {
			t.Errorf("encodeClaudeProject(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- correlation: two sessions, one workdir (codex + claude) ----

func writeCodexRollout(t *testing.T, root, sessionID, cwd string, createdAt time.Time) string {
	t.Helper()
	day := createdAt.Format("2006/01/02")
	dir := filepath.Join(root, day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("rollout-%s-%s.jsonl", createdAt.Format("2006-01-02T15-04-05"), sessionID)
	path := filepath.Join(dir, name)
	line := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"session_id":%q,"cwd":%q,"timestamp":%q}}`,
		createdAt.Format(time.RFC3339), sessionID, cwd, createdAt.Format(time.RFC3339Nano))
	writeFile(t, path, line+"\n")
	return path
}

func TestCorrelateCodexTwoSessionsOneWorkdir(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	root := filepath.Join(home, ".codex", "sessions")
	base := time.Date(2026, 7, 11, 17, 0, 0, 0, time.UTC)

	// Session A (target): created at base. Session B: created 10 min earlier (outside
	// the ±60s window). Same cwd.
	pathA := writeCodexRollout(t, root, "aaaa-a", "/home/shed", base)
	writeCodexRollout(t, root, "bbbb-b", "/home/shed", base.Add(-10*time.Minute))

	corr, ok := correlateCodex(getenv, "/home/shed", "", base.Add(5*time.Second), true)
	if !ok {
		t.Fatal("expected a correlation")
	}
	if corr.path != pathA {
		t.Fatalf("chose %q, want %q (the in-window session)", corr.path, pathA)
	}
	if corr.ambiguous {
		t.Error("single in-window match must not be ambiguous")
	}
	if corr.sessionID != "aaaa-a" {
		t.Fatalf("sessionID = %q, want aaaa-a", corr.sessionID)
	}

	// A second session inside the window makes it ambiguous → newest chosen.
	newer := writeCodexRollout(t, root, "cccc-c", "/home/shed", base.Add(20*time.Second))
	corr, ok = correlateCodex(getenv, "/home/shed", "", base.Add(5*time.Second), true)
	if !ok || !corr.ambiguous {
		t.Fatalf("expected ambiguous match, got ok=%v ambiguous=%v", ok, corr.ambiguous)
	}
	if corr.path != newer {
		t.Fatalf("ambiguous pick = %q, want the newest %q", corr.path, newer)
	}
}

func TestCorrelateCodexByBackWrittenID(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	root := filepath.Join(home, ".codex", "sessions")
	base := time.Date(2026, 7, 11, 17, 0, 0, 0, time.UTC)
	// Two files far apart in time; the back-written id pins the OLD one exactly, even
	// though it is outside any created-at window.
	old := writeCodexRollout(t, root, "pinned-id", "/home/shed", base.Add(-time.Hour))
	writeCodexRollout(t, root, "other-id", "/home/shed", base)

	corr, ok := correlateCodex(getenv, "/home/shed", "pinned-id", base, true)
	if !ok || corr.path != old {
		t.Fatalf("id match = (%v,%q), want %q", ok, corr.path, old)
	}
}

func writeClaudeTranscript(t *testing.T, projectDir, sessionID, cwd string, createdAt time.Time) string {
	t.Helper()
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projectDir, sessionID+".jsonl")
	first := fmt.Sprintf(`{"type":"system","cwd":%q,"timestamp":%q,"sessionId":%q}`,
		cwd, createdAt.Format(time.RFC3339Nano), sessionID)
	writeFile(t, path, first+"\n")
	return path
}

func TestCorrelateClaudeWindowAndID(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	cwd := "/home/shed"
	projectDir := filepath.Join(home, ".claude", "projects", encodeClaudeProject(cwd))
	base := time.Date(2026, 7, 11, 17, 0, 0, 0, time.UTC)

	inWindow := writeClaudeTranscript(t, projectDir, "aaaa-a", cwd, base)
	writeClaudeTranscript(t, projectDir, "bbbb-b", cwd, base.Add(-10*time.Minute))

	corr, ok := correlateClaude(getenv, cwd, "", base.Add(3*time.Second), true)
	if !ok || corr.path != inWindow {
		t.Fatalf("window match = (%v,%q), want %q", ok, corr.path, inWindow)
	}
	if corr.ambiguous {
		t.Error("single window match must not be ambiguous")
	}
	// Exact id match ignores the window.
	corr, ok = correlateClaude(getenv, cwd, "bbbb-b", base, true)
	want := filepath.Join(projectDir, "bbbb-b.jsonl")
	if !ok || corr.path != want {
		t.Fatalf("id match = (%v,%q), want %q", ok, corr.path, want)
	}
}

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

// ---- same-size rewrite detection (header tripwire) ----

func TestLineTailerSameSizeRewriteResets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	writeFile(t, path, "aaaa\nbbbb\n")
	tl := &lineTailer{path: path, catchUp: true}
	if lines, _, _, _ := tl.poll(); len(lines) != 2 {
		t.Fatalf("catch-up read = %v, want 2 lines", linesToStrings(lines))
	}
	// Rewrite with DIFFERENT content of the SAME size (os.WriteFile truncates+rewrites,
	// so the size never dips below the offset between polls). Force a distinct mtime in
	// case the filesystem's timestamp granularity would hide the rewrite.
	writeFile(t, path, "cccc\ndddd\n")
	if err := os.Chtimes(path, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	lines, didReset, _, err := tl.poll()
	if err != nil {
		t.Fatal(err)
	}
	if !didReset {
		t.Fatal("a same-size rewrite with a changed header must reset")
	}
	if got := linesToStrings(lines); len(got) != 2 || got[0] != "cccc" || got[1] != "dddd" {
		t.Fatalf("post-rewrite = %v, want [cccc dddd]", got)
	}
}

// TestLineTailerGrowingRewriteResets covers an in-place rewrite that ends LONGER than
// the previous offset. Without the size >= offset check this would be mistaken for a
// plain append and read from the stale offset, mixing old and rewritten records.
func TestLineTailerGrowingRewriteResets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	writeFile(t, path, "aaaa\nbbbb\n")
	tl := &lineTailer{path: path, catchUp: true}
	if lines, _, _, _ := tl.poll(); len(lines) != 2 {
		t.Fatalf("catch-up read = %v, want 2 lines", linesToStrings(lines))
	}
	// Rewrite the file to a LARGER size with different leading content (a different
	// header), simulating a rollout/transcript rewrite that grew past our offset.
	writeFile(t, path, "cccc\ndddd\neeee\n")
	if err := os.Chtimes(path, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	lines, didReset, _, err := tl.poll()
	if err != nil {
		t.Fatal(err)
	}
	if !didReset {
		t.Fatal("a grow-past-offset rewrite with a changed header must reset, not append")
	}
	if got := linesToStrings(lines); len(got) != 3 || got[0] != "cccc" || got[2] != "eeee" {
		t.Fatalf("post-rewrite = %v, want [cccc dddd eeee]", got)
	}
}

// ---- catch-up window landing exactly on a line boundary ----

func TestLineTailerCatchUpExactBoundaryKeepsFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	var b strings.Builder
	// Prefix: 3 lines of 100 bytes each, ending in '\n' → the byte just before the
	// catch-up window is a newline when the window covers exactly the rest.
	prefixLine := "P" + strings.Repeat("x", 98) // 99 chars + '\n' = 100 bytes
	for i := 0; i < 3; i++ {
		b.WriteString(prefixLine + "\n")
	}
	// Window: exactly tailCatchUpWindow bytes of 1024-byte lines ("W%03d-" is 5 chars,
	// so body = 1024-5-1 and each line incl. '\n' is exactly 1024 bytes).
	lineBody := strings.Repeat("w", 1024-5-1)
	nWindow := tailCatchUpWindow / 1024
	for i := 0; i < nWindow; i++ {
		fmt.Fprintf(&b, "W%03d-%s\n", i, lineBody)
	}
	writeFile(t, path, b.String())
	// Sanity: the catch-up start must land exactly at the prefix/window boundary, i.e.
	// right after a '\n' — otherwise this test would silently exercise the skip path.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if fi.Size()-tailCatchUpWindow != int64(3*100) {
		t.Fatalf("boundary math broken: size=%v (want window start at byte 300)", fi.Size())
	}

	tl := &lineTailer{path: path, catchUp: true}
	lines, _, _, err := tl.poll()
	if err != nil {
		t.Fatal(err)
	}
	got := linesToStrings(lines)
	if len(got) != nWindow {
		t.Fatalf("read %d lines, want %d (window starts on a boundary — nothing dropped)", len(got), nWindow)
	}
	if !strings.HasPrefix(got[0], "W000-") {
		t.Fatalf("first line = %.20q, want W000- (the boundary line must be kept)", got[0])
	}
}

// ---- gap (oversized skip) clears pending tool calls ----

func TestCodexFoldGapClearsPendingThenTaskCompleteSettles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	writeFile(t, path,
		`{"type":"event_msg","payload":{"type":"task_started"}}`+"\n"+
			`{"type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec"}}`+"\n")
	w := newFileWatcher(path, true, newCodexFold())
	t0 := time.Unix(1_700_000_000, 0).UTC()
	w.refresh(t0)
	if a, _, _, _ := w.snapshot(t0); a != ActivityWorking {
		t.Fatalf("activity = %q, want working (open tool call)", a)
	}

	// The tool's OUTPUT line is pathological (> tailMaxLine) and gets skipped — without
	// the gap signal, call_id c1 would stay pending forever and pin the verdict at
	// working even after task_complete.
	oversized := `{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":"` +
		strings.Repeat("x", tailMaxLine+16) + `"}}`
	appendFile(t, path, oversized+"\n"+
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done"}}`+"\n")
	w.refresh(t0.Add(time.Second))

	if a, _, _, _ := w.snapshot(t0.Add(time.Second)); a != ActivityNeedsInput {
		t.Fatalf("activity = %q, want needs_input (gap cleared the pending call)", a)
	}
}

// ---- claude cwd equality (the encoded dir name is lossy) ----

func TestCorrelateClaudeCwdCollisionRejected(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	// "/home/shed/a-b" and "/home/shed/a_b" encode to the SAME project dir.
	cwdA, cwdB := "/home/shed/a-b", "/home/shed/a_b"
	if encodeClaudeProject(cwdA) != encodeClaudeProject(cwdB) {
		t.Fatal("precondition: the two cwds must collide in the encoding")
	}
	projectDir := filepath.Join(home, ".claude", "projects", encodeClaudeProject(cwdA))
	base := time.Date(2026, 7, 11, 17, 0, 0, 0, time.UTC)

	// Only a transcript whose PEEKED cwd is the other path exists: no match for cwdA.
	writeClaudeTranscript(t, projectDir, "bbbb-b", cwdB, base)
	if _, ok := correlateClaude(getenv, cwdA, "", base, true); ok {
		t.Fatal("a transcript with a colliding-but-different cwd must not correlate")
	}
	// The exact-cwd transcript does match.
	want := writeClaudeTranscript(t, projectDir, "aaaa-a", cwdA, base)
	corr, ok := correlateClaude(getenv, cwdA, "", base, true)
	if !ok || corr.path != want {
		t.Fatalf("exact-cwd match = (%v,%q), want %q", ok, corr.path, want)
	}
}

// ---- candidates without a peeked timestamp are excluded from window matching ----

func TestCorrelateExcludesNoTimestampCandidates(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	base := time.Date(2026, 7, 11, 17, 0, 0, 0, time.UTC)

	// codex: a rollout whose session_meta has NO usable timestamp.
	codexDir := filepath.Join(home, ".codex", "sessions", "2026", "07", "11")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(codexDir, "rollout-x-notime.jsonl"),
		`{"type":"session_meta","payload":{"session_id":"nt-1","cwd":"/home/shed"}}`+"\n")
	if _, ok := correlateCodex(getenv, "/home/shed", "", base, true); ok {
		t.Fatal("codex: a no-timestamp candidate must not window-match")
	}
	// It remains reachable via the exact-id path (the filename embeds the id).
	corr, ok := correlateCodex(getenv, "/home/shed", "notime", base, true)
	if !ok || !strings.Contains(corr.path, "notime") {
		t.Fatalf("codex: exact-id match should still work, got (%v,%q)", ok, corr.path)
	}

	// claude: a transcript with rows carrying no timestamp.
	projectDir := filepath.Join(home, ".claude", "projects", encodeClaudeProject("/home/shed"))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(projectDir, "nt-2.jsonl"),
		`{"type":"system","cwd":"/home/shed","sessionId":"nt-2"}`+"\n")
	if _, ok := correlateClaude(getenv, "/home/shed", "", base, true); ok {
		t.Fatal("claude: a no-timestamp candidate must not window-match")
	}
	// Exact-id still resolves it by filename.
	corr, ok = correlateClaude(getenv, "/home/shed", "nt-2", base, true)
	if !ok || filepath.Base(corr.path) != "nt-2.jsonl" {
		t.Fatalf("claude: exact-id match should still work, got (%v,%q)", ok, corr.path)
	}
}

// ---- ambiguous correlation: back-write deferred until an in-file event confirms ----

// backWriteRecorder wraps a Runner and records set-environment calls (the back-write
// channel), forwarding everything else.
type backWriteRecorder struct {
	Runner
	mu     sync.Mutex
	writes map[string]string // tmux name → written SHED_RC_AGENT_SESSION value
}

func (r *backWriteRecorder) Run(args ...string) Result {
	if args[0] == "set-environment" && len(args) >= 5 && args[3] == envAgentSession {
		r.mu.Lock()
		r.writes[args[2]] = args[4]
		r.mu.Unlock()
		return Result{}
	}
	return r.Runner.Run(args...)
}

func (r *backWriteRecorder) written(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writes[name]
}

func TestReconcileAmbiguousNoBackWriteUntilConfirmed(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	root := filepath.Join(home, ".codex", "sessions")
	base := time.Date(2026, 7, 11, 17, 17, 35, 0, time.UTC)

	// TWO rollouts in the same workdir within the created-at window → ambiguous.
	writeCodexRollout(t, root, "older-id", "/home/shed", base.Add(-20*time.Second))
	newest := writeCodexRollout(t, root, "newest-id", "/home/shed", base.Add(10*time.Second))

	f := newHubTmux()
	rec := &backWriteRecorder{Runner: f, writes: map[string]string{}}
	env := strings.Join([]string{
		envV + "=2",
		envID + "=id-amb",
		envKind + "=" + string(KindCodex),
		envWorkdir + "=/home/shed",
		envCreatedAt + "=" + base.Format(time.RFC3339),
	}, "\n") + "\n"
	f.set("rc-amb001", "boot >_ OpenAI Codex (v1.0)", env)

	clk := &hubClock{t: base.Add(15 * time.Second)}
	h := newHub(HubConfig{
		Runner: rec, Getenv: getenv, Now: clk.now,
		Logf: func(string, ...any) {}, QuietPeriod: 4 * time.Second,
	})

	h.reconcile()
	if got := rec.written("rc-amb001"); got != "" {
		t.Fatalf("ambiguous correlation back-wrote %q before confirmation", got)
	}
	h.trackMu.Lock()
	tr := h.tracked["amb001"]
	pending := tr.pendingAgentID
	h.trackMu.Unlock()
	if tr.watcher == nil || pending != "newest-id" {
		t.Fatalf("expected a watcher with pendingAgentID=newest-id, got pending=%q", pending)
	}

	// A quiet second pass still must not write (no in-file event yet).
	clk.advance(2 * time.Second)
	h.reconcile()
	if got := rec.written("rc-amb001"); got != "" {
		t.Fatalf("back-write happened with no confirming event: %q", got)
	}

	// The first in-file event (appended AFTER attach — the watcher is follow-only)
	// confirms the pick; the deferred back-write fires.
	appendFile(t, newest, `{"type":"event_msg","payload":{"type":"task_started"}}`+"\n")
	clk.advance(2 * time.Second)
	h.reconcile()
	if got := rec.written("rc-amb001"); got != "newest-id" {
		t.Fatalf("confirmed back-write = %q, want newest-id", got)
	}
}

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

// ---- end-to-end reconcile: a correlated codex watcher overrides pane stability ----

func TestReconcileCodexWatcherOverridesStability(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	// Place the sanitized codex fixture at its real rollout path. Its session_meta cwd
	// is /home/shed and created-at is 2026-07-11T17:17:35Z.
	fixture, err := os.ReadFile("testdata/jsonl/codex_turn.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	rolloutDir := filepath.Join(home, ".codex", "sessions", "2026", "07", "11")
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(rolloutDir, "rollout-2026-07-11T17-17-35-019f0000-0000-7000-8000-00000000c0de.jsonl")
	writeFile(t, rollout, string(fixture))

	f := newHubTmux()
	// A ready codex session whose workdir + created-at correlate to the fixture. The
	// pane classifies ready and, on its own, pane-stability would report working on the
	// first tick — the watcher must override that with needs_input (task_complete).
	env := strings.Join([]string{
		envV + "=2",
		envID + "=id-codex",
		envKind + "=" + string(KindCodex),
		envWorkdir + "=/home/shed",
		envCreatedAt + "=2026-07-11T17:17:35Z",
	}, "\n") + "\n"
	f.set("rc-cdx001", "boot >_ OpenAI Codex (v1.0)", env)

	clk := &hubClock{t: time.Date(2026, 7, 11, 17, 17, 40, 0, time.UTC)}
	h := newHub(HubConfig{
		Runner:      f,
		Getenv:      getenv,
		Now:         clk.now,
		Logf:        func(string, ...any) {},
		QuietPeriod: 4 * time.Second,
	})

	h.reconcile()

	if got := hubActivityOf(t, h, "cdx001"); got != ActivityNeedsInput {
		t.Fatalf("activity = %q, want needs_input (watcher override)", got)
	}
	h.trackMu.Lock()
	tr := h.tracked["cdx001"]
	msg := tr.lastMessage
	hasWatcher := tr.watcher != nil
	h.trackMu.Unlock()
	if !hasWatcher {
		t.Fatal("expected a correlated watcher on the codex session")
	}
	if msg != "2+2 equals 4." {
		t.Fatalf("last_message = %q, want %q", msg, "2+2 equals 4.")
	}

	// The overlay carries the watcher's activity + last_message.
	sessions := List(f, nil).RCSessions
	h.trackMu.Lock()
	for i := range sessions {
		if tr, ok := h.tracked[sessions[i].Slug]; ok && tr.sameIdentity(sessions[i]) && tr.activity != "" {
			sessions[i].Activity = tr.activity
			sessions[i].LastMessage = tr.lastMessage
		}
	}
	h.trackMu.Unlock()
	if sessions[0].Activity != ActivityNeedsInput || sessions[0].LastMessage != "2+2 equals 4." {
		t.Fatalf("overlay = %+v", sessions[0])
	}
}

// ---- opencode watcher wire-in (C5) ----

// watchableKind must admit opencode (so ensureWatcher's first guard lets it through to
// the SSE/REST arm) while non-agentic kinds stay stability-only.
func TestWatchableKindOpencode(t *testing.T) {
	if !watchableKind(KindOpencode) {
		t.Fatal("watchableKind(KindOpencode) = false, want true")
	}
	if watchableKind(KindShell) {
		t.Fatal("watchableKind(KindShell) = true, want false (stability only)")
	}
}

// A pre-upgrade opencode session (created before the port plumbing shipped, so no
// SHED_RC_OPENCODE_PORT is stamped) is unwatchable over the SSE transport: ensureWatcher
// returns no watcher and pane-stability drives its activity.
func TestReconcileOpencodeNoPortStabilityOnly(t *testing.T) {
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
		t.Fatal("a session with no recorded port must get NO watcher (stability only)")
	}
	// Pane-stability drives: a ready, just-appeared session reports working on tick 1.
	if tr.activity != ActivityWorking {
		t.Fatalf("activity = %q, want working (pane-stability)", tr.activity)
	}
}

// End-to-end: a correlated opencode SSE watcher overrides pane stability, populates the
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
	// A ready opencode session whose workdir matches the fixture directory (so the SSE
	// transport pins on the fixture's session.created) and whose recorded port targets the
	// fake server. The pane classifies ready ("Ask anything...") and — because the hub
	// clock never advances below — pane-stability can only ever report working (it never
	// reaches the quiet period), so a needs_input verdict MUST come from the SSE watcher.
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
	lastStability := tr.lastStability
	lastMessage := tr.lastMessage
	watcher := tr.watcher
	h.trackMu.Unlock()

	if activity != ActivityNeedsInput {
		t.Fatalf("activity = %q, want needs_input (SSE watcher override)", activity)
	}
	if _, ok := watcher.(*opencodeWatcher); !ok {
		t.Fatalf("watcher = %T, want *opencodeWatcher", watcher)
	}
	// The override is real: pane-stability, with the clock frozen, held working the whole
	// time — the needs_input verdict came from the watcher, not the anchor/quiet fallback.
	if lastStability != ActivityWorking {
		t.Fatalf("lastStability = %q, want working (frozen clock never reaches quiet)", lastStability)
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
