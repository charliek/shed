package rc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// ---- tolerance: malformed / unknown / partial lines never break the fold ----

func TestFoldsToleratePathologicalLines(t *testing.T) {
	for _, tc := range []struct {
		name string
		fold activityFold
	}{
		{"codex", newCodexFold()},
		{"claude", newClaudeFold()},
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
