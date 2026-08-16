package rc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests for the cursor transcript restart-backfill (plan 008 §3.5 "Transcript tail =
// restart backfill only" / C5). testdata/jsonl/cursor_transcript.jsonl is a trimmed,
// hand-built sample matching the spike capture's exact schema
// (~/.claude/plans/shed/008-observatory/spikes/cursor-hooks/transcript-tui-4113a71f.jsonl):
// {"role":"user"|"assistant","message":{"content":[...]}} lines plus a trailing
// {"type":"turn_ended",...} — no ids, no timestamps, tool results absent.

// ---- path derivation ----

func TestCursorSlugForWorkdir(t *testing.T) {
	cases := []struct {
		name, workdir, want string
	}{
		{"simple absolute", "/home/shed", "home-shed"},
		{"nested absolute", "/home/shed/myproj", "home-shed-myproj"},
		{"root", "/", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cursorSlugForWorkdir(tc.workdir); got != tc.want {
				t.Errorf("cursorSlugForWorkdir(%q) = %q, want %q", tc.workdir, got, tc.want)
			}
		})
	}
}

func TestCursorTranscriptPath(t *testing.T) {
	const id = cursorTestSessionID
	cases := []struct {
		name, home, workdir, sessionID string
		want                           string
	}{
		{
			name: "valid triple", home: "/home/shed", workdir: "/home/shed/proj", sessionID: id,
			want: "/home/shed/.cursor/projects/home-shed-proj/agent-transcripts/" + id + "/" + id + ".jsonl",
		},
		{name: "no home", home: "", workdir: "/home/shed", sessionID: id, want: ""},
		{name: "no workdir", home: "/home/shed", workdir: "", sessionID: id, want: ""},
		{name: "malformed session id", home: "/home/shed", workdir: "/home/shed", sessionID: "not-a-uuid", want: ""},
		{name: "empty session id", home: "/home/shed", workdir: "/home/shed", sessionID: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cursorTranscriptPath(tc.home, tc.workdir, tc.sessionID); got != tc.want {
				t.Errorf("cursorTranscriptPath(%q,%q,%q) = %q, want %q",
					tc.home, tc.workdir, tc.sessionID, got, tc.want)
			}
		})
	}
}

// ---- line-level folding ----

func TestParseCursorTranscriptLine(t *testing.T) {
	t.Run("user text", func(t *testing.T) {
		rows := parseCursorTranscriptLine([]byte(`{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}`))
		if len(rows) != 1 || rows[0].Role != feedRoleUser || rows[0].Type != feedTypeText || rows[0].Text != "hello" {
			t.Fatalf("rows = %+v, want one user text row", rows)
		}
	})

	t.Run("assistant text and tool_use blocks", func(t *testing.T) {
		line := `{"role":"assistant","message":{"content":[` +
			`{"type":"text","text":"doing it"},` +
			`{"type":"tool_use","name":"Shell","input":{"command":"echo hi"}}` +
			`]}}`
		rows := parseCursorTranscriptLine([]byte(line))
		if len(rows) != 2 {
			t.Fatalf("rows = %+v, want 2", rows)
		}
		if rows[0].Role != feedRoleAssistant || rows[0].Type != feedTypeText || rows[0].Text != "doing it" {
			t.Errorf("row0 = %+v, want assistant text 'doing it'", rows[0])
		}
		if rows[1].Role != feedRoleTool || rows[1].Type != feedTypeToolUse {
			t.Errorf("row1 = %+v, want a tool_use row", rows[1])
		}
		if rows[1].Tool == nil || rows[1].Tool.Name != "Shell" || rows[1].Tool.Detail != "echo hi" {
			t.Errorf("row1.Tool = %+v, want {Shell, echo hi}", rows[1].Tool)
		}
	})

	t.Run("tool_use with path input falls back to path", func(t *testing.T) {
		line := `{"role":"assistant","message":{"content":[` +
			`{"type":"tool_use","name":"Write","input":{"path":"/tmp/notes.txt","contents":"hi\n"}}` +
			`]}}`
		rows := parseCursorTranscriptLine([]byte(line))
		if len(rows) != 1 || rows[0].Tool == nil || rows[0].Tool.Detail != "/tmp/notes.txt" {
			t.Fatalf("rows = %+v, want one tool_use row with path detail", rows)
		}
	})

	t.Run("turn_ended ignored", func(t *testing.T) {
		rows := parseCursorTranscriptLine([]byte(`{"type":"turn_ended","status":"completed"}`))
		if rows != nil {
			t.Fatalf("rows = %+v, want nil for turn_ended", rows)
		}
	})

	t.Run("malformed line ignored", func(t *testing.T) {
		rows := parseCursorTranscriptLine([]byte(`{"role":"user","message":`)) // truncated JSON
		if rows != nil {
			t.Fatalf("rows = %+v, want nil for malformed JSON", rows)
		}
	})

	t.Run("empty text blocks dropped", func(t *testing.T) {
		rows := parseCursorTranscriptLine([]byte(`{"role":"user","message":{"content":[{"type":"text","text":"   "}]}}`))
		if rows != nil {
			t.Fatalf("rows = %+v, want nil for a whitespace-only text block", rows)
		}
	})

	t.Run("no message field ignored", func(t *testing.T) {
		rows := parseCursorTranscriptLine([]byte(`{"role":"user"}`))
		if rows != nil {
			t.Fatalf("rows = %+v, want nil with no message field", rows)
		}
	})

	t.Run("unknown block type ignored (no tool_result in cursor transcripts)", func(t *testing.T) {
		line := `{"role":"assistant","message":{"content":[{"type":"tool_result","text":"output"}]}}`
		rows := parseCursorTranscriptLine([]byte(line))
		if rows != nil {
			t.Fatalf("rows = %+v, want nil for an unrecognized block type", rows)
		}
	})
}

// ---- file-level reading ----

func TestReadCursorTranscriptFixture(t *testing.T) {
	rows := readCursorTranscript("testdata/jsonl/cursor_transcript.jsonl")
	// The fixture is: user, assistant(text+tool_use), assistant(text), user, turn_ended
	// (ignored) — five rows in stream order.
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5: %+v", len(rows), rows)
	}
	want := []struct {
		role, typ string
	}{
		{feedRoleUser, feedTypeText},
		{feedRoleAssistant, feedTypeText},
		{feedRoleTool, feedTypeToolUse},
		{feedRoleAssistant, feedTypeText},
		{feedRoleUser, feedTypeText},
	}
	for i, w := range want {
		if rows[i].Role != w.role || rows[i].Type != w.typ {
			t.Errorf("row[%d] = {role:%q type:%q}, want {role:%q type:%q}",
				i, rows[i].Role, rows[i].Type, w.role, w.typ)
		}
	}
	if rows[2].Tool == nil || rows[2].Tool.Name != "Shell" || rows[2].Tool.Detail != "make build" {
		t.Errorf("row[2].Tool = %+v, want {Shell, make build}", rows[2].Tool)
	}
	if rows[4].Text != "Thanks" {
		t.Errorf("row[4].Text = %q, want %q", rows[4].Text, "Thanks")
	}
	// No row derived from the trailing turn_ended line.
	for _, r := range rows {
		if r.Type == "turn_ended" {
			t.Fatalf("turn_ended leaked into a feed row: %+v", r)
		}
	}
}

func TestReadCursorTranscriptMissingFile(t *testing.T) {
	rows := readCursorTranscript(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if rows != nil {
		t.Fatalf("rows = %+v, want nil for a missing file", rows)
	}
}

// A transcript is written incrementally; a hub restart can land mid-write, leaving the
// last line truncated. That must not lose the earlier, complete lines.
func TestReadCursorTranscriptPartialLastLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	content := `{"role":"user","message":{"content":[{"type":"text","text":"complete line"}]}}` + "\n" +
		`{"role":"assistant","message":{"content":[{"type":"text","text":"cut off mid-w` // no closing, no newline
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := readCursorTranscript(path)
	if len(rows) != 1 || rows[0].Text != "complete line" {
		t.Fatalf("rows = %+v, want exactly the complete line's row", rows)
	}
}

func TestReadCursorTranscriptUnreadableDirNoPanic(t *testing.T) {
	// A path that names a directory (not a file) — os.Open succeeds but Scan fails
	// immediately; must not panic and must return nil.
	rows := readCursorTranscript(t.TempDir())
	if rows != nil {
		t.Fatalf("rows = %+v, want nil when the path is a directory", rows)
	}
}

// The backfill is bounded to the transcript's LAST N lines: write more than the cap and
// verify only the tail survives.
func TestReadCursorTranscriptLineCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	var b strings.Builder
	total := maxCursorTranscriptLines + 50
	for i := 0; i < total; i++ {
		line := map[string]any{
			"role": "user",
			"message": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "line-" + strconv.Itoa(i)}},
			},
		}
		enc, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(enc)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := readCursorTranscript(path)
	if len(rows) != maxCursorTranscriptLines {
		t.Fatalf("got %d rows, want the cap %d", len(rows), maxCursorTranscriptLines)
	}
	// The retained window is the LAST maxCursorTranscriptLines lines: the earliest
	// surviving row is line total-maxCursorTranscriptLines, the last is total-1.
	wantFirst := "line-" + strconv.Itoa(total-maxCursorTranscriptLines)
	wantLast := "line-" + strconv.Itoa(total-1)
	if rows[0].Text != wantFirst {
		t.Errorf("rows[0].Text = %q, want %q (oldest lines should have been dropped)", rows[0].Text, wantFirst)
	}
	if rows[len(rows)-1].Text != wantLast {
		t.Errorf("last row.Text = %q, want %q", rows[len(rows)-1].Text, wantLast)
	}
}

// ---- watcher-level seeding ----

// writeCursorTranscriptFixture copies testdata/jsonl/cursor_transcript.jsonl to the exact
// path cursorTranscriptPath derives for (home, workdir, sessionID), creating parents.
func writeCursorTranscriptFixture(t *testing.T, home, workdir, sessionID string) {
	t.Helper()
	src, err := os.ReadFile("testdata/jsonl/cursor_transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path := cursorTranscriptPath(home, workdir, sessionID)
	if path == "" {
		t.Fatalf("cursorTranscriptPath(%q,%q,%q) = \"\"", home, workdir, sessionID)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCursorWatcherSeedFromTranscript(t *testing.T) {
	home := t.TempDir()
	const workdir = "/home/shed/proj"
	writeCursorTranscriptFixture(t, home, workdir, cursorTestSessionID)

	w := newCursorWatcher(cursorTestSessionID, func(string, ...any) {})
	if w.priorID != cursorTestSessionID {
		t.Fatalf("priorID = %q, want the pin (construction must validate but keep it)", w.priorID)
	}
	w.seedFromTranscript(home, workdir, w.priorID)

	rows := w.drainPending()
	if len(rows) != 5 {
		t.Fatalf("drainPending() = %d rows, want 5: %+v", len(rows), rows)
	}
	// A second drain is empty — seeding is one-shot, not re-appended.
	if rows2 := w.drainPending(); rows2 != nil {
		t.Fatalf("second drainPending() = %+v, want nil", rows2)
	}
}

func TestCursorWatcherSeedFromTranscriptMissingFile(t *testing.T) {
	home := t.TempDir() // no transcript written anywhere under here
	w := newCursorWatcher(cursorTestSessionID, func(string, ...any) {})
	w.seedFromTranscript(home, "/home/shed/proj", w.priorID)
	if rows := w.drainPending(); rows != nil {
		t.Fatalf("drainPending() = %+v, want nil when the transcript file does not exist", rows)
	}
}

// A fresh session (no prior SHED_RC_AGENT_SESSION pin) must not attempt a transcript
// read at all — newCursorWatcher leaves priorID empty, and the caller (ensureWatcher)
// gates the call on it being non-empty. This pins that a watcher built with no pin has
// nothing to seed from, structurally: there is no session id to derive a path from.
func TestCursorWatcherFreshSessionNoBackfillAttempt(t *testing.T) {
	w := newCursorWatcher("", func(string, ...any) {})
	if w.priorID != "" {
		t.Fatalf("priorID = %q, want empty for a fresh session", w.priorID)
	}
	// Even called explicitly with an empty sessionID (mirroring what ensureWatcher's
	// guard would otherwise skip), it must no-op rather than guess a path.
	w.seedFromTranscript("/home/shed", "/home/shed/proj", w.priorID)
	if rows := w.drainPending(); rows != nil {
		t.Fatalf("drainPending() = %+v, want nil with no prior pin", rows)
	}
}

func TestCursorWatcherSeedFromTranscriptClosedWatcherNoop(t *testing.T) {
	home := t.TempDir()
	const workdir = "/home/shed/proj"
	writeCursorTranscriptFixture(t, home, workdir, cursorTestSessionID)

	w := newCursorWatcher(cursorTestSessionID, func(string, ...any) {})
	w.close()
	w.seedFromTranscript(home, workdir, w.priorID)
	if rows := w.drainPending(); rows != nil {
		t.Fatalf("drainPending() = %+v, want nil on a closed watcher", rows)
	}
}

// ---- ensureWatcher wiring (hub-level) ----

// TestEnsureWatcherCursorSeedsTranscriptOnRestartPin exercises the real construction
// path in hub_reconcile.go: a tracked cursor session with an existing SHED_RC_AGENT_SESSION
// pin (simulating a hub restart mid-session) must have its feed backfilled from the
// transcript on the very first reconcile tick that builds the watcher.
func TestEnsureWatcherCursorSeedsTranscriptOnRestartPin(t *testing.T) {
	home := t.TempDir()
	const workdir = "/home/shed"
	writeCursorTranscriptFixture(t, home, workdir, cursorTestSessionID)

	f := newHubTmux()
	env := managedEnv("id-restart", KindCursor) + envAgentSession + "=" + cursorTestSessionID + "\n"
	f.set("rc-restart001", paneFixture(t, "cursor-ready"), env)

	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newHub(HubConfig{
		Runner: f,
		Getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		Now: clk.now, Logf: func(string, ...any) {}, QuietPeriod: 4 * time.Second,
	})

	h.reconcile()

	rows := feedRowsOf(t, h, "restart001")
	if len(rows) != 5 {
		t.Fatalf("got %d seeded feed rows, want 5: %+v", len(rows), rows)
	}
	if rows[0].Role != feedRoleUser || rows[0].Text != "Run the build and report results." {
		t.Fatalf("rows[0] = %+v, want the transcript's first user row", rows[0])
	}
}

// A fresh session — no prior SHED_RC_AGENT_SESSION pin — must NOT seed anything from a
// transcript, even when one happens to exist on disk (there is nothing to key it by yet;
// the pin arrives later via a hook payload).
func TestEnsureWatcherCursorFreshSessionSkipsBackfill(t *testing.T) {
	home := t.TempDir()
	// Note: no session id is known for a fresh session, so there is nothing to write a
	// fixture "at" — this proves the guard is structural, not merely a missing file.

	f := newHubTmux()
	f.set("rc-fresh001", paneFixture(t, "cursor-ready"), managedEnv("id-fresh", KindCursor))

	clk := &hubClock{t: time.Unix(1_700_000_000, 0).UTC()}
	h := newHub(HubConfig{
		Runner: f,
		Getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		Now: clk.now, Logf: func(string, ...any) {}, QuietPeriod: 4 * time.Second,
	})

	h.reconcile()

	rows := feedRowsOf(t, h, "fresh001")
	if len(rows) != 0 {
		t.Fatalf("got %d feed rows for a fresh session, want 0: %+v", len(rows), rows)
	}
	w := cursorWatcherOf(t, h, "fresh001")
	if w.priorID != "" {
		t.Fatalf("watcher priorID = %q, want empty for a fresh session", w.priorID)
	}
}
