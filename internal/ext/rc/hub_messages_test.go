package rc

import (
	"strings"
	"sync"
	"testing"
	"time"
)

var ringClock = time.Unix(1_700_000_000, 0).UTC()

func textMsg(text string) feedMessage {
	return feedMessage{Role: feedRoleAssistant, Type: feedTypeText, Text: text}
}

// ---- ring: seq assignment + since paging (exclusive) ----

func TestMessageRingSeqMonotonicAndSincePaging(t *testing.T) {
	r := newMessageRing()
	for i := 0; i < 5; i++ {
		seq := r.append(textMsg("m"), ringClock)
		if want := uint64(i + 1); seq != want {
			t.Fatalf("append %d assigned seq %d, want %d (monotonic from 1)", i, seq, want)
		}
	}

	// since is EXCLUSIVE: since=2 returns seq 3,4,5.
	got, truncated := r.since(2, 100)
	if truncated {
		t.Error("since within the ring must not report truncated")
	}
	if len(got) != 3 || got[0].Seq != 3 || got[2].Seq != 5 {
		t.Fatalf("since(2) = %+v, want seqs 3..5", seqsOf(got))
	}

	// limit caps the page; the client pages again from the last seq it saw.
	page, _ := r.since(0, 2)
	if len(page) != 2 || page[0].Seq != 1 || page[1].Seq != 2 {
		t.Fatalf("since(0, limit 2) = %v, want seqs 1,2", seqsOf(page))
	}

	// ts is stamped from the clock when the message carries none.
	if page[0].TS != ringClock.Format(time.RFC3339) {
		t.Errorf("ts = %q, want clock-stamped %q", page[0].TS, ringClock.Format(time.RFC3339))
	}
}

func TestMessageRingLimitClampAndDefault(t *testing.T) {
	r := newMessageRing()
	for i := 0; i < 250; i++ {
		r.append(textMsg("m"), ringClock)
	}
	// default (limit<=0) is 100.
	if got, _ := r.since(0, 0); len(got) != defaultMessagesLimit {
		t.Errorf("default page = %d, want %d", len(got), defaultMessagesLimit)
	}
	// clamp to the hard cap.
	if got, _ := r.since(0, 10_000); len(got) != maxMessagesLimit {
		t.Errorf("clamped page = %d, want %d", len(got), maxMessagesLimit)
	}
}

// ---- ring: drop-oldest by COUNT + truncated flag ----

func TestMessageRingDropOldestByCount(t *testing.T) {
	r := newMessageRing()
	total := maxRingMessages + 50
	for i := 0; i < total; i++ {
		r.append(textMsg("m"), ringClock)
	}
	got, _ := r.since(0, maxMessagesLimit)
	// The earliest retained seq is total-maxRingMessages+1 (the first 50 dropped).
	wantEarliest := uint64(total - maxRingMessages + 1)
	if got[0].Seq != wantEarliest {
		t.Fatalf("earliest retained seq = %d, want %d (drop-oldest by count)", got[0].Seq, wantEarliest)
	}

	// A fresh client (since=0) whose expected next seq predates the ring gets truncated.
	_, truncated := r.since(0, maxMessagesLimit)
	if !truncated {
		t.Error("since=0 against a ring that dropped its head must report truncated")
	}
	// A since AT the earliest-1 boundary is contiguous → not truncated.
	if _, tr := r.since(wantEarliest-1, 10); tr {
		t.Error("since exactly at earliest-1 is contiguous, must not be truncated")
	}
	// A since one below the boundary IS truncated (a gap was dropped).
	if _, tr := r.since(wantEarliest-2, 10); !tr {
		t.Error("since below earliest-1 must report truncated")
	}
}

// ---- ring: drop-oldest by BYTES ----

func TestMessageRingDropOldestByBytes(t *testing.T) {
	r := newMessageRing()
	big := strings.Repeat("x", 100*1024) // 100 KiB each (well over 8 KiB → truncated to ~8 KiB)
	// After the 8 KiB per-message cap, ~130 messages fit under the 1 MiB byte budget;
	// appending far more must drop the oldest by bytes, not by the 500 count cap.
	const n = 400
	for i := 0; i < n; i++ {
		r.append(textMsg(big), ringClock)
	}
	got, _ := r.since(0, maxMessagesLimit)
	// Sum retained bytes ≤ the byte cap.
	sum := 0
	for _, m := range got {
		sum += m.size()
	}
	if sum > maxRingBytes {
		t.Fatalf("retained bytes = %d, want ≤ %d", sum, maxRingBytes)
	}
	if len(got) >= n {
		t.Fatalf("byte cap did not drop any of %d oversized messages (retained %d)", n, len(got))
	}
	// Each retained message was truncated to the 8 KiB cap with the marker.
	if !strings.HasSuffix(got[0].Text, feedTruncMarker) {
		t.Error("an oversized message must be truncated with the marker")
	}
	if len([]byte(strings.TrimSuffix(got[0].Text, feedTruncMarker))) != maxFeedMessageBytes {
		t.Errorf("truncated text length = %d, want %d", len(got[0].Text), maxFeedMessageBytes)
	}
}

// ---- ring: approval rows (byte accounting + append/since round-trip) ----

// approvalMsg builds a pending approval_request row: role `tool`, a human-readable
// summary in text, and the machine-readable approval block.
func approvalMsg(id, text string) feedMessage {
	return feedMessage{
		Role: feedRoleTool,
		Type: feedTypeApprovalRequest,
		Text: text,
		Approval: &FeedApproval{
			ID:        id,
			Status:    approvalStatusPending,
			Decisions: []string{approvalDecisionAllow, approvalDecisionAllowAlways, approvalDecisionDeny},
		},
	}
}

// An approval row's payload counts toward the ring's 1 MiB byte budget: size() sums
// id + status + decision + every advertised decision alongside text/tool, so an
// approval-heavy feed can never exceed the budget it is accounted against.
func TestMessageRingApprovalBytesCounted(t *testing.T) {
	m := approvalMsg("call_01HQ8Z3K.tool:2", "Allow running `rm -rf build/`?")
	wantApproval := len(m.Approval.ID) + len(approvalStatusPending) +
		len(approvalDecisionAllow) + len(approvalDecisionAllowAlways) + len(approvalDecisionDeny)
	if got := m.Approval.size(); got != wantApproval {
		t.Errorf("approval size = %d, want %d", got, wantApproval)
	}
	// The row's size is its text PLUS the whole approval payload — proof the approval
	// fields are accounted rather than ignored.
	if got, want := m.size(), len(m.Text)+wantApproval; got != want {
		t.Errorf("message size = %d, want %d (text + approval payload)", got, want)
	}

	// The ring's running total agrees with the per-message accounting.
	r := newMessageRing()
	r.append(m, ringClock)
	if r.bytes != m.size() {
		t.Errorf("ring bytes = %d, want %d", r.bytes, m.size())
	}
}

// An approval_request row survives append → since unchanged (ids, status, decisions),
// and its resolution is a SECOND row carrying the same id — the id-keyed,
// last-write-wins stream clients fold.
func TestMessageRingApprovalRoundTrip(t *testing.T) {
	r := newMessageRing()
	const id = "call_01HQ8Z3K.tool:2"
	r.append(approvalMsg(id, "Allow running `rm -rf build/`?"), ringClock)
	resolved := approvalMsg(id, "Allow running `rm -rf build/`?")
	resolved.Approval.Status = approvalStatusResolved
	resolved.Approval.Decision = approvalDecisionAllow
	resolved.Approval.Decisions = nil
	r.append(resolved, ringClock)

	got, truncated := r.since(0, 10)
	if truncated {
		t.Error("a contiguous page must not report truncated")
	}
	if len(got) != 2 {
		t.Fatalf("want both approval rows, got %d", len(got))
	}
	pending, done := got[0], got[1]
	if pending.Type != feedTypeApprovalRequest || pending.Role != feedRoleTool {
		t.Errorf("pending row = %s/%s, want %s/%s", pending.Role, pending.Type, feedRoleTool, feedTypeApprovalRequest)
	}
	if pending.Approval == nil || pending.Approval.ID != id ||
		pending.Approval.Status != approvalStatusPending || len(pending.Approval.Decisions) != 3 {
		t.Fatalf("pending approval mangled by the ring: %+v", pending.Approval)
	}
	if done.Approval == nil || done.Approval.ID != id ||
		done.Approval.Status != approvalStatusResolved || done.Approval.Decision != approvalDecisionAllow {
		t.Fatalf("resolved approval mangled by the ring: %+v", done.Approval)
	}
	if done.Seq <= pending.Seq {
		t.Errorf("the resolution must be appended after the request (seqs %d, %d)", pending.Seq, done.Seq)
	}
}

// Approval fields are sanitized and BOUNDED on the way into the ring (ANSI/control
// stripping, the grammar's 128-byte id ceiling, a capped decision list) so a
// misbehaving producer cannot inflate the ring past its accounted byte budget. The
// stored row is a copy — mutating the producer's struct afterwards cannot rewrite it.
func TestMessageRingApprovalSanitizedAndBounded(t *testing.T) {
	r := newMessageRing()
	m := feedMessage{
		Role: feedRoleTool,
		Type: feedTypeApprovalRequest,
		Approval: &FeedApproval{
			// The id smuggles whitespace/control separators a feed TEXT sanitizer
			// would keep (tab, newline, DEL, a C1 CSI) — the token sanitizer must
			// drop them all, then the length cap applies.
			ID:        "a\tb\nc\x7fd\u009be" + strings.Repeat("x", maxApprovalTokenBytes+50),
			Status:    "\x1b[31mpen ding\x1b[0m",
			Decisions: make([]string, maxApprovalDecisions+5),
		},
	}
	for i := range m.Approval.Decisions {
		m.Approval.Decisions[i] = approvalDecisionAllow
	}
	r.append(m, ringClock)

	got, _ := r.since(0, 10)
	a := got[0].Approval
	if len(a.ID) != maxApprovalTokenBytes {
		t.Errorf("id = %d bytes, want capped at %d", len(a.ID), maxApprovalTokenBytes)
	}
	if a.Status != approvalStatusPending {
		t.Errorf("status = %q, want the ANSI stripped to %q", a.Status, approvalStatusPending)
	}
	if len(a.Decisions) != maxApprovalDecisions {
		t.Errorf("decisions = %d, want capped at %d", len(a.Decisions), maxApprovalDecisions)
	}
	if r.bytes != got[0].size() {
		t.Errorf("ring bytes = %d, want the sanitized size %d", r.bytes, got[0].size())
	}

	// The producer mutating its own struct after the append must not touch the ring.
	m.Approval.ID = "rewritten"
	if again, _ := r.since(0, 10); again[0].Approval.ID == "rewritten" {
		t.Error("the ring stored the producer's pointer instead of a copy")
	}
}

// ---- sanitizeFeedText: strip + preserve newlines + rune-safe truncation ----

func TestSanitizeFeedText(t *testing.T) {
	// ANSI + control stripped, newlines PRESERVED (unlike SanitizeLastMessage).
	in := "line1\n\x1b[31mred\x1b[0m\x07line2"
	got := sanitizeFeedText(in)
	if got != "line1\nredline2" {
		t.Fatalf("sanitize = %q, want %q", got, "line1\nredline2")
	}

	// Multi-byte truncation never splits a rune.
	long := strings.Repeat("é", maxFeedMessageBytes) // 2 bytes each → 16 KiB
	out := sanitizeFeedText(long)
	if !strings.HasSuffix(out, feedTruncMarker) {
		t.Fatal("over-cap text must carry the truncation marker")
	}
	body := strings.TrimSuffix(out, feedTruncMarker)
	if len(body) > maxFeedMessageBytes {
		t.Errorf("truncated body = %d bytes, want ≤ %d", len(body), maxFeedMessageBytes)
	}
	if !isValidUTF8(body) {
		t.Error("truncation split a multi-byte rune")
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// ---- -race: concurrent append + read ----

func TestMessageRingConcurrentAppendRead(t *testing.T) {
	r := newMessageRing()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			r.append(textMsg("m"), ringClock)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_, _ = r.since(uint64(i), 50)
		}
	}()
	wg.Wait()
}

// The codex fold section that lived here (its message-mapping and assistant-text-dedup
// cells, driven off testdata/jsonl/codex_turn.jsonl) went with the codex rollout tail
// in A6 (charliek/shed#322).

// A since cursor beyond the ring's latest seq (a previous incarnation's cursor — the
// hub restarted or the session was recreated, restarting seq at 1) must report
// truncated so a poll-only client refetches instead of idling on empty pages forever.
// The uint64 edge (MaxUint64, where since+1 would wrap) is the same beyond-tail case.
func TestMessageRingSinceBeyondTailTruncated(t *testing.T) {
	r := newMessageRing()
	for i := 0; i < 3; i++ {
		r.append(textMsg("m"), ringClock)
	}

	msgs, truncated := r.since(10, 10) // cursor from a bigger, previous ring
	if !truncated {
		t.Error("since beyond the tail must report truncated")
	}
	if len(msgs) != 0 {
		t.Errorf("beyond-tail page must be empty, got %v", seqsOf(msgs))
	}
	// Exactly at the tail: an up-to-date cursor — empty page, NOT truncated.
	if _, tr := r.since(3, 10); tr {
		t.Error("since == latest seq is an up-to-date cursor, not truncated")
	}
	// The wrap edge.
	if _, tr := r.since(^uint64(0), 10); !tr {
		t.Error("since=MaxUint64 must report truncated (stale cursor), not wrap")
	}
	// An empty ring (seq 0) with any positive cursor is also beyond-tail.
	empty := newMessageRing()
	if _, tr := empty.since(1, 10); !tr {
		t.Error("a positive cursor against an empty ring must report truncated")
	}
}

// A6 (charliek/shed#322) removed the `gated` input mode entirely: no kind advertises
// it, so the derivation this used to pin has one live value left. opencode carries the
// whole-turn lane (input == "turn"); every other kind advertises no feed input at all,
// and POST /input answers 409 not_accepting for all of them (hub_input_test.go).
func TestNoKindAdvertisesGatedInput(t *testing.T) {
	kf := kindFeatures()
	for k, row := range kf {
		if row.Input == "gated" {
			t.Errorf("kind_features[%q].input = \"gated\"; the gated lane is gone", k)
		}
	}
	if kf[KindOpencode].Input != inputModeTurn {
		t.Errorf("opencode input = %q, want %q (the turn lane is the only input surface left)", kf[KindOpencode].Input, inputModeTurn)
	}
	for _, k := range []Kind{KindCodex, KindCursor, KindClaudeRC} {
		if got := kf[k].Input; got != "" {
			t.Errorf("kind_features[%q].input = %q, want \"\"", k, got)
		}
	}
}

func seqsOf(msgs []feedMessage) []uint64 {
	out := make([]uint64, len(msgs))
	for i, m := range msgs {
		out[i] = m.Seq
	}
	return out
}
