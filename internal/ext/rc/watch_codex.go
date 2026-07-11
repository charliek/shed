package rc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codex rollout files live at ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl,
// one JSON object per line: {timestamp, type, payload}. The types the fold reads:
//
//   - session_meta      (first line): session_id + cwd + timestamp — correlation only.
//   - event_msg/task_started, task_complete: bracket a turn (working ↔ needs_input);
//     task_complete also carries last_agent_message (the message preview).
//   - event_msg/agent_message: the assistant's text (message preview).
//   - event_msg/user_message: a new turn's input (working).
//   - response_item/(function_call|custom_tool_call): a tool invocation (working; its
//     call_id is tracked as pending until the matching *_output line).
//   - response_item/(function_call_output|custom_tool_call_output): resolves a call_id.
//   - response_item/message role=assistant: assistant text (message preview fallback).
//   - token_count / reasoning / world_state / turn_context: ignored (interspersed noise).
//
// Everything is parsed tolerantly: an unknown type, an unparseable line, or a missing
// field is ignored, and the fold falls back to whatever verdict it last held (the hub
// then falls back to pane stability when the watcher goes non-fresh).

// codexLine is the generic envelope every rollout line shares.
type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexPayload is the union of the payload fields the fold reads (all optional).
type codexPayload struct {
	Type             string          `json:"type"`
	Message          string          `json:"message"`            // event_msg/agent_message|user_message
	Phase            string          `json:"phase"`              // agent_message: commentary|final_answer
	LastAgentMessage string          `json:"last_agent_message"` // task_complete
	CallID           string          `json:"call_id"`            // (custom_)function_call(_output)
	Role             string          `json:"role"`               // response_item/message
	Content          json.RawMessage `json:"content"`            // response_item/message
}

// codexFold folds the codex rollout stream into an activity verdict.
type codexFold struct {
	confirmed    bool            // seen ≥1 activity-relevant event
	pending      map[string]bool // open tool-call ids
	lastBoundary string          // "start" | "complete" | ""
	lastMsg      string          // raw last message text (sanitized on read)
}

func newCodexFold() *codexFold { return &codexFold{pending: map[string]bool{}} }

func (f *codexFold) reset() {
	f.confirmed = false
	f.pending = map[string]bool{}
	f.lastBoundary = ""
	f.lastMsg = ""
}

func (f *codexFold) applyLine(line []byte) bool {
	var env codexLine
	if json.Unmarshal(line, &env) != nil {
		return false
	}
	var p codexPayload
	if len(env.Payload) > 0 {
		_ = json.Unmarshal(env.Payload, &p)
	}

	switch env.Type {
	case "event_msg":
		switch p.Type {
		case "task_started":
			f.confirmed = true
			f.lastBoundary = "start"
			return true
		case "task_complete":
			f.confirmed = true
			f.lastBoundary = "complete"
			if p.LastAgentMessage != "" {
				f.lastMsg = p.LastAgentMessage
			}
			return true
		case "agent_message":
			f.confirmed = true
			if p.Message != "" {
				f.lastMsg = p.Message
			}
			return true
		case "user_message":
			f.confirmed = true
			f.lastBoundary = "start"
			return true
		default:
			return false // token_count and other event_msg subtypes: noise
		}
	case "response_item":
		switch p.Type {
		case "function_call", "custom_tool_call":
			f.confirmed = true
			if p.CallID != "" {
				f.pending[p.CallID] = true
			}
			return true
		case "function_call_output", "custom_tool_call_output":
			f.confirmed = true
			if p.CallID != "" {
				delete(f.pending, p.CallID)
			}
			return true
		case "message":
			if p.Role == "assistant" {
				if txt := codexMessageText(p.Content); txt != "" {
					f.confirmed = true
					f.lastMsg = txt
					return true
				}
			}
			return false // developer/user instruction messages: not activity
		default:
			return false // reasoning and other response_items: noise
		}
	default:
		return false // session_meta, turn_context, world_state, unknown: noise
	}
}

func (f *codexFold) activity() Activity {
	if !f.confirmed {
		return ActivityUnknown
	}
	if len(f.pending) > 0 {
		return ActivityWorking
	}
	if f.lastBoundary == "complete" {
		return ActivityNeedsInput
	}
	return ActivityWorking
}

func (f *codexFold) settled() bool { return f.activity() == ActivityNeedsInput }

// noteGap drops the pending tool-call set: a lost (oversized, skipped) record may have
// been the *_output resolving one of them, and a forever-pending call_id would pin the
// verdict at working until the freshness grace expired. After a gap the verdict rides
// the turn-boundary events alone (task_started/task_complete) until the next turn.
func (f *codexFold) noteGap() {
	f.pending = map[string]bool{}
}

func (f *codexFold) lastMessage() string { return SanitizeLastMessage(f.lastMsg) }

// codexMessageText extracts the concatenated text of a response_item/message content
// array ([{type:"output_text"|"input_text"|"text", text:"..."}]).
func codexMessageText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Text != "" {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// ---- correlation ----

// codexSessionsRoot is ~/.codex/sessions ("" when HOME is unset).
func codexSessionsRoot(getenv Getenv) string {
	home := getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

// peekCodexRollout reads a rollout file's first line (session_meta) for correlation.
func peekCodexRollout(path string) (jsonlPeek, bool) {
	f, err := os.Open(path)
	if err != nil {
		return jsonlPeek{}, false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 128*1024)
	line, err := r.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return jsonlPeek{}, false
	}
	var env codexLine
	if json.Unmarshal(bytes.TrimSpace(line), &env) != nil || env.Type != "session_meta" {
		return jsonlPeek{}, false
	}
	var meta struct {
		SessionID string `json:"session_id"`
		ID        string `json:"id"`
		CWD       string `json:"cwd"`
		Timestamp string `json:"timestamp"`
	}
	if json.Unmarshal(env.Payload, &meta) != nil {
		return jsonlPeek{}, false
	}
	pk := jsonlPeek{sessionID: firstNonEmpty(meta.SessionID, meta.ID), cwd: meta.CWD}
	if t, ok := parseJSONLTime(firstNonEmpty(meta.Timestamp, env.Timestamp)); ok {
		pk.createdAt, pk.hasTime = t, true
	}
	return pk, true
}

// correlateCodex maps a codex session to its rollout file. With a back-written agent
// session id it matches the file whose name embeds that uuid (exact). Otherwise it
// filters candidates by cwd + the created-at window and pins the newest, flagging
// ambiguity when more than one survives the window.
func correlateCodex(getenv Getenv, cwd, agentSessionID string, createdAt time.Time, hasCreatedAt bool) (correlation, bool) {
	root := codexSessionsRoot(getenv)
	if root == "" {
		return correlation{}, false
	}
	files := listJSONLUnder(root, func(base string) bool {
		return strings.HasPrefix(base, "rollout-")
	})
	if len(files) == 0 {
		return correlation{}, false
	}

	if agentSessionID != "" {
		for _, p := range files {
			if strings.Contains(filepath.Base(p), agentSessionID) {
				return correlation{path: p, sessionID: agentSessionID}, true
			}
		}
		// The pinned file is gone — fall through to a fresh window match.
	}

	var matches []peekCandidate
	for _, p := range files {
		pk, ok := peekCodexRollout(p)
		// A candidate with no peeked timestamp can't be window-matched at all — it is
		// eligible only for the exact-id path above. Including it here would bypass the
		// window and could get a wrong file pinned (and back-written).
		if !ok || !pk.hasTime {
			continue
		}
		if cwd != "" && pk.cwd != cwd {
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
	// nameTiebreak=true: rollout filenames are timestamp-prefixed, so a lexical
	// comparison is a valid chronological tiebreak for an exact created-at tie.
	return pickCorrelation(matches, true), true
}
