package rc

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// claude transcripts live at ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl, one
// JSON object per line with a top-level type. The types the fold reads:
//
//   - type "user", message.content a STRING: a typed human prompt (a turn begins →
//     working).
//   - type "user", message.content an array of {type:"tool_result", tool_use_id}:
//     a tool result (resolves a pending tool_use).
//   - type "assistant", message.content blocks:
//       {type:"text", text}      → the assistant's message (preview); a text tail with
//                                   no pending tool_use ⇒ needs_input.
//       {type:"tool_use", id}    → a tool invocation ⇒ working (id pending until its
//                                   tool_result).
//   - type "system"|"summary"|meta rows (custom-title, mode, …): ignored.
//
// Parsing is tolerant: unknown types, unparseable lines, or missing fields are
// ignored; the fold retains its prior verdict (the hub falls back to pane stability
// when the watcher goes non-fresh).

// claudeLine is the generic envelope. message is decoded lazily because content is
// polymorphic (string for a typed prompt, array of blocks otherwise).
type claudeLine struct {
	Type    string `json:"type"`
	Message *struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
	} `json:"message"`
}

// claude event-kind tags for the tail verdict.
const (
	claudeKindUserPrompt = "user_prompt"
	claudeKindAsstText   = "assistant_text"
	claudeKindAsstTool   = "assistant_tool"
	claudeKindToolResult = "tool_result"
)

// claudeFold folds the claude transcript stream into an activity verdict.
type claudeFold struct {
	confirmed bool
	pending   map[string]bool // open tool_use ids (awaiting tool_result)
	lastKind  string
	lastStop  string // stop_reason of the last assistant message
	lastMsg   string // last assistant text (sanitized on read)
}

func newClaudeFold() *claudeFold { return &claudeFold{pending: map[string]bool{}} }

func (f *claudeFold) reset() {
	f.confirmed = false
	f.pending = map[string]bool{}
	f.lastKind = ""
	f.lastStop = ""
	f.lastMsg = ""
}

func (f *claudeFold) applyLine(line []byte) bool {
	var env claudeLine
	if json.Unmarshal(line, &env) != nil || env.Message == nil {
		return false
	}
	switch env.Type {
	case "user":
		// content is either a typed-prompt string or a tool_result block array.
		var str string
		if json.Unmarshal(env.Message.Content, &str) == nil {
			f.confirmed = true
			f.lastKind = claudeKindUserPrompt
			return true
		}
		blocks, ok := claudeBlocks(env.Message.Content)
		if !ok {
			return false
		}
		advanced := false
		for _, b := range blocks {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				delete(f.pending, b.ToolUseID)
				f.confirmed = true
				f.lastKind = claudeKindToolResult
				advanced = true
			}
		}
		return advanced
	case "assistant":
		blocks, ok := claudeBlocks(env.Message.Content)
		if !ok {
			return false
		}
		f.lastStop = env.Message.StopReason
		advanced := false
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				if b.ID != "" {
					f.pending[b.ID] = true
				}
				f.confirmed = true
				f.lastKind = claudeKindAsstTool
				advanced = true
			case "text":
				if b.Text != "" {
					f.lastMsg = b.Text
				}
				f.confirmed = true
				// A tool_use in the same message dominates the tail verdict (working);
				// don't let a leading text block downgrade it.
				if f.lastKind != claudeKindAsstTool {
					f.lastKind = claudeKindAsstText
				}
				advanced = true
			}
		}
		return advanced
	default:
		return false // system/summary/meta rows: not activity
	}
}

func (f *claudeFold) activity() Activity {
	if !f.confirmed {
		return ActivityUnknown
	}
	if len(f.pending) > 0 {
		return ActivityWorking
	}
	// A text tail is needs_input UNLESS the message's stop_reason marks it as a
	// mid-turn text block that a tool_use follows (claude splits the two across lines
	// with a shared stop_reason:"tool_use"). Any other/absent stop_reason falls back to
	// the plan's base rule — a text tail with no pending tool_use ⇒ needs_input.
	if f.lastKind == claudeKindAsstText && f.lastStop != "tool_use" {
		return ActivityNeedsInput
	}
	// user_prompt (awaiting the assistant), tool_result (assistant will continue), or a
	// mid-turn text block awaiting its tool_use.
	return ActivityWorking
}

func (f *claudeFold) settled() bool { return f.activity() == ActivityNeedsInput }

// noteGap drops the pending tool_use set: a lost (oversized, skipped) record may have
// been the tool_result resolving one of them — same rationale as codexFold.noteGap.
// The verdict then rides the message tail (lastKind/lastStop) until the next exchange.
func (f *claudeFold) noteGap() {
	f.pending = map[string]bool{}
}

func (f *claudeFold) lastMessage() string { return SanitizeLastMessage(f.lastMsg) }

// claudeBlock is one message-content block (only the read fields).
type claudeBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`        // text block
	ID        string `json:"id"`          // tool_use block
	ToolUseID string `json:"tool_use_id"` // tool_result block
}

// claudeBlocks decodes a content array; ok=false when content is not an array (e.g. a
// bare string, handled by the caller).
func claudeBlocks(raw json.RawMessage) ([]claudeBlock, bool) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, false
	}
	return blocks, true
}

// ---- correlation ----

// claudeProjectsRoot is ~/.claude/projects ("" when HOME is unset).
func claudeProjectsRoot(getenv Getenv) string {
	home := getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// encodeClaudeProject maps a cwd to claude's project-dir name: every rune that is not
// an ASCII letter or digit becomes '-'. e.g. "/home/shed" → "-home-shed".
func encodeClaudeProject(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// peekClaudeTranscript reads a transcript's first lines for correlation: the session
// id (on every row), the cwd (on system/message rows), and the earliest timestamp.
func peekClaudeTranscript(path string) (jsonlPeek, bool) {
	f, err := os.Open(path)
	if err != nil {
		return jsonlPeek{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	var pk jsonlPeek
	for i := 0; i < 40 && sc.Scan(); i++ {
		var row struct {
			SessionID string `json:"sessionId"`
			CWD       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(sc.Bytes(), &row) != nil {
			continue
		}
		if pk.sessionID == "" {
			pk.sessionID = row.SessionID
		}
		if pk.cwd == "" {
			pk.cwd = row.CWD
		}
		if !pk.hasTime {
			if t, ok := parseJSONLTime(row.Timestamp); ok {
				pk.createdAt, pk.hasTime = t, true
			}
		}
	}
	if pk.sessionID == "" {
		return jsonlPeek{}, false
	}
	return pk, true
}

// correlateClaude maps a claude session to its transcript. The project dir is derived
// from the cwd encoding; with a back-written agent session id the file is named
// directly, otherwise candidates in the dir are filtered by the created-at window and
// the newest is pinned (ambiguous when >1 survive).
func correlateClaude(getenv Getenv, cwd, agentSessionID string, createdAt time.Time, hasCreatedAt bool) (correlation, bool) {
	root := claudeProjectsRoot(getenv)
	if root == "" || cwd == "" {
		return correlation{}, false
	}
	dir := filepath.Join(root, encodeClaudeProject(cwd))

	if agentSessionID != "" {
		p := filepath.Join(dir, agentSessionID+".jsonl")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return correlation{path: p, sessionID: agentSessionID}, true
		}
		// The pinned file is gone — fall through to a window match.
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return correlation{}, false
	}
	var matches []peekCandidate
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		p := filepath.Join(dir, e.Name())
		pk, ok := peekClaudeTranscript(p)
		// No peeked timestamp → the window can't be applied; such a file is eligible
		// only for the exact-id path above (a UUID filename is no chronology signal).
		if !ok || !pk.hasTime {
			continue
		}
		// The encoded project-dir name is LOSSY ("a-b" and "a_b" share a dir), so the
		// dir alone does not prove the cwd: when the transcript records its cwd it must
		// equal the session's workdir exactly.
		if pk.cwd != "" && pk.cwd != cwd {
			continue
		}
		if hasCreatedAt && !withinWindow(pk.createdAt, createdAt, correlateWindow) {
			continue
		}
		matches = append(matches, peekCandidate{p, pk})
	}
	if len(matches) == 0 {
		return correlation{}, false
	}
	// nameTiebreak=false: claude transcript names are bare session UUIDs — lexical
	// order carries no chronology, so an exact created-at tie is left to slice order.
	return pickCorrelation(matches, false), true
}
