package rc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// cursor's preseed (plan 008 §3.5): cursor-agent has no protocol the hub can subscribe
// to, but it DOES run user-configured hooks on every meaningful event of a turn
// (prompt submitted, tool about to run, shell output, file edited, agent replied, turn
// stopped). Those hooks are the ONLY live signal that carries tool output at all — the
// transcript JSONL is user/assistant lines with no tool results and no ids — so the hub
// makes itself a hook consumer: one script, wired to every event we care about, POSTing
// each raw payload to the hub's loopback ingest route (see handleIngestCursor).
//
// Two files, both written best-effort at create:
//
//   - ~/.shed-rc-hub/cursor-hook.sh — HUB-OWNED, overwritten every time. It lives beside
//     the hub's own state (never inside ~/.cursor) so that upgrading shed-ext-rc updates
//     the script for every session, and so the file a user's hooks.json points at is
//     unambiguously ours.
//   - ~/.cursor/hooks.json — USER-OWNED, MERGED never clobbered: existing entries in each
//     event's array are preserved and ours is appended only when absent (matched on the
//     script path, so a re-run is idempotent). Same machinery as the claude preseed:
//     sibling flock, tolerant decode, malformed file left untouched, atomic rename.
//
// The hook script is deliberately mute: cursor interprets a hook's STDOUT as a verdict
// (permission decisions, prompt rewrites), so anything printed here would steer the
// agent. It always exits 0 with empty stdout — a hub that is down, slow, or absent must
// change nothing about how the agent runs. (And it could not help anyway: the spike
// proved a hook `allow` cannot bypass cursor's own allowlist prompt.)

const (
	// cursorHookScriptName is the hub-owned hook script's basename inside ~/.shed-rc-hub.
	cursorHookScriptName = "cursor-hook.sh"
	// cursorHooksConfigVersion is the hooks.json schema version cursor expects. Written
	// only when the file does not already declare one (never clobbered).
	cursorHooksConfigVersion = 1
)

// cursorHookEvents are the hook events wired to the hub's script, in a fixed order (the
// written config is deterministic). Chosen from the live spike's event matrix:
//
//	sessionStart       — the session's first id-carrying event (absent on --resume)
//	beforeSubmitPrompt — the user's prompt: the turn's start boundary + a user feed row
//	preToolUse         — a tool call is starting: a tool_use row (+ working)
//	afterShellExecution— the command's OUTPUT, which exists nowhere else
//	afterFileEdit      — the edited path + edit count
//	postToolUse        — a tool call ENDED: the counter's other half (see below)
//	postToolUseFailure — a tool failed: a status row, and the counter's other half
//	afterAgentResponse — the assistant's message text + last_message
//	stop               — the turn's end boundary (settled needs_input)
//	sessionEnd         — the session closed
//
// postToolUse is wired for the COUNTER, not for its payload: the live capture shows every
// preToolUse is matched by exactly one postToolUse or postToolUseFailure (5 ↔ 3+2 in the
// spike's turn), while afterShellExecution/afterFileEdit are EXTRA events that fire only
// for those two tool families. Without it the fold's open-call count would never come back
// down for a Read/Grep/Glob-class tool and would rely on `stop` to reset it.
//
// Deliberately NOT wired: afterAgentThought (chain-of-thought noise, fires ~10x a turn),
// beforeReadFile (fires only on a successful read — a misleading half-signal),
// workspaceOpen (carries no session id, so it can never be attributed to a session),
// beforeShellExecution (its after* twin already carries the command AND its output).
var cursorHookEvents = []string{
	"sessionStart",
	"beforeSubmitPrompt",
	"preToolUse",
	"afterShellExecution",
	"afterFileEdit",
	"postToolUse",
	"postToolUseFailure",
	"afterAgentResponse",
	"stop",
	"sessionEnd",
}

// ErrCursorHooksForeignDevice reports that ~/.cursor sits on a DIFFERENT filesystem than
// $HOME — in a shed that means it is a host directory bind-mounted in for auth
// (VirtioFS/9P). Writing hooks.json there would push a hook config referencing a script
// path that does not exist on the host into the user's real cursor setup, where it would
// fire on every local cursor run forever. So the hooks.json half is SKIPPED and this is
// returned for the caller to log; the hook script half is still written (it is inert
// until something references it). The paired guidance is that a shed's cursor auth mount
// should be ~/.config/cursor (where the Linux cursor-agent tokens actually live), leaving
// ~/.cursor guest-local so hooks work.
var ErrCursorHooksForeignDevice = errors.New("~/.cursor is on a different device than $HOME (an auth mount); skipping the hooks.json preseed")

// sameDevice reports whether a and b live on the same filesystem (st_dev). It is a
// package var so tests can substitute the device check without needing two real
// filesystems — the ONE seam in this file that cannot be exercised with a TempDir.
var sameDevice = statSameDevice

// statSameDevice is the real st_dev comparison. The two Stat_t values are compared
// directly (no widening conversion) because syscall.Stat_t.Dev is a different integer
// type per platform.
func statSameDevice(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}

// PreseedCursorHooks writes the hub's cursor hook script and merges it into the user's
// ~/.cursor/hooks.json, so a cursor-agent session reports its activity, turn boundaries
// and message feed to the hub. It is the cursor spec's AgentSpec.Preseed, invoked
// best-effort by Create: the returned error is DIAGNOSTIC only (a create never fails on
// it), and the session runs exactly as before when it fires — the hub simply falls back
// to pane stability for that session. workdir is unused (unlike claude's trust preseed,
// nothing here is per-project — cursor hooks are global) and is accepted only to satisfy
// the shared Preseed signature.
//
// Invariants: the script is overwritten every time (hub-owned, so an upgraded binary's
// script always wins); hooks.json is merged, never clobbered; a malformed hooks.json is
// left untouched; the write is atomic under a sibling flock; ~/.cursor on a foreign
// device skips the config half (ErrCursorHooksForeignDevice).
func PreseedCursorHooks(_ string, getenv func(string) string) error {
	home := getenv("HOME")
	if home == "" {
		return errors.New("no HOME; skipping cursor hook preseed")
	}

	scriptPath, err := writeCursorHookScript(home)
	if err != nil {
		return err
	}

	cursorDir := filepath.Join(home, ".cursor")
	// MkdirAll first so the device check below has something to stat: a fresh shed has no
	// ~/.cursor until cursor-agent first runs, and a directory we just created under $HOME
	// is on $HOME's device by construction.
	if err := os.MkdirAll(cursorDir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", cursorDir, err)
	}
	same, err := sameDevice(cursorDir, home)
	if err != nil {
		return fmt.Errorf("checking whether %s is a mount: %w", cursorDir, err)
	}
	if !same {
		return ErrCursorHooksForeignDevice
	}
	return mergeCursorHooksConfig(filepath.Join(cursorDir, "hooks.json"), scriptPath)
}

// writeCursorHookScript writes ~/.shed-rc-hub/cursor-hook.sh (0755) and returns its path.
// Overwrite-always: the script is hub-owned, so the binary that runs now defines what it
// contains — a stale script from an older shed-ext-rc must never survive an upgrade.
func writeCursorHookScript(home string) (string, error) {
	dir := filepath.Join(home, hubDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, cursorHookScriptName)
	if err := atomicWrite(path, ".cursor-hook.sh.*.tmp", []byte(cursorHookScript()), 0o755); err != nil {
		return "", err
	}
	return path, nil
}

// cursorHookScript renders the hook script. Contract, line by line:
//
//   - `$SHED_RC_SLUG` unset ⇒ exit immediately, WITHOUT reading stdin: outside a managed
//     rc session (a plain cursor-agent run on the same machine) there is nothing to
//     address, and the cheapest possible no-op is the point — this runs on every hook of
//     every turn.
//   - the payload is streamed from stdin to the hub verbatim (`--data-binary @-`): the
//     watcher folds cursor's OWN payload shapes, so nothing here reshapes or truncates.
//     Oversized payloads are the hub's decision (413, event dropped), not the script's.
//   - `--max-time 2 --connect-timeout 1` bounds the agent's turn: cursor waits for its
//     hooks, so an unreachable or wedged hub may cost at most ~2s per event.
//   - `--noproxy '*'` is a CONFINEMENT control, not a nicety: curl honors http_proxy from
//     the environment, and a shed image (or a user rc file, or an egress policy) that sets
//     one without a matching no_proxy would send every prompt and every command's output to
//     that proxy instead of to loopback — exfiltrating the session's whole content off-box
//     and killing the feed at the same time. The hub is only ever at 127.0.0.1, so no proxy
//     is ever correct here.
//   - `--globoff` stops curl from interpreting `[]{}` in the URL as its globbing syntax.
//     The slug is the only interpolated value and ValidCallerSlug (rc.go) confines it to
//     `[a-z0-9-]` — no `&`, `#`, space or brace can appear, so the raw interpolation is
//     safe by GRAMMAR, and --globoff means a future loosening of that grammar degrades to a
//     failed POST rather than a mangled request. The event name is a fixed literal chosen
//     from cursorHookEvents at write time.
//   - `|| true` + `exit 0` with NO stdout: fail-open. Cursor reads a hook's stdout as a
//     VERDICT, so silence is mandatory — see the file doc.
//
// The URL is built from HubAddr so the hub's port lives in exactly one place.
func cursorHookScript() string {
	return `#!/bin/sh
# shed rc hub — cursor hook relay. GENERATED by shed-ext-rc (PreseedCursorHooks);
# rewritten on every rc session create, so local edits do not survive.
#
# Reads one hook payload on stdin and POSTs it to the shed rc hub's ingest route.
# ALWAYS exits 0 with EMPTY stdout: cursor treats hook stdout as a verdict, so this
# script must never influence the agent. A hub that is down/slow costs at most the
# curl timeouts below and changes nothing about the run.
#
# --noproxy '*' is load-bearing: the hub is on loopback, and an http_proxy in the
# environment would otherwise send this session's prompts and command output to it.
[ -n "${SHED_RC_SLUG}" ] || exit 0
curl --silent --output /dev/null --noproxy '*' --globoff \
	--connect-timeout 1 --max-time 2 \
	-X POST -H 'Content-Type: application/json' --data-binary @- \
	"http://` + HubAddr + `/v1/ingest/cursor?slug=${SHED_RC_SLUG}&event=$1" || true
exit 0
`
}

// mergeCursorHooksConfig merges the hub's hook entry into hooks.json for every event in
// cursorHookEvents, preserving everything else in the file. Merge rules (the claude
// preseed's rules, applied to a different shape):
//
//   - the whole document is round-tripped through map[string]any (never a typed struct),
//     so unknown top-level keys and unknown per-entry fields survive verbatim;
//   - `version` is written only when absent — a user (or a newer cursor) declaring their
//     own is never overwritten;
//   - each event's array keeps its existing entries in order, with ours APPENDED; a
//     re-run finds our entry by command string and changes nothing (idempotent);
//   - a malformed/unreadable file is left EXACTLY as it is and reported.
func mergeCursorHooksConfig(path, scriptPath string) error {
	unlock, err := lockSibling(path)
	if err != nil {
		return fmt.Errorf("locking cursor hooks: %w", err)
	}
	defer unlock()

	// Shared with the claude preseed (trust.go): tolerant decode, mode preserved, a
	// malformed file reported and left exactly as it is.
	config, mode, err := readJSONObject(path)
	if err != nil {
		return err
	}

	if _, ok := config["version"]; !ok {
		config["version"] = cursorHooksConfigVersion
	}
	// An UNEXPECTED SHAPE is treated exactly like a malformed file: bail without writing.
	// The tempting `hooks, _ := config["hooks"].(map[string]any)` discards on a failed
	// assertion and then overwrites, so a config that is perfectly valid JSON but shaped
	// differently from what this code expects — `"hooks": [...]`, or an event mapped to an
	// object instead of an array — would be silently DELETED by a preseed the user never
	// asked for. Never clobber means never clobber, including shapes we do not understand.
	hooks := map[string]any{}
	if raw, present := config["hooks"]; present {
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s has a %s `hooks` value, not an object; leaving untouched", path, jsonShapeOf(raw))
		}
		hooks = m
	}
	for _, event := range cursorHookEvents {
		var entries []any
		if raw, present := hooks[event]; present {
			arr, ok := raw.([]any)
			if !ok {
				return fmt.Errorf("%s has a %s `hooks.%s` value, not an array; leaving untouched",
					path, jsonShapeOf(raw), event)
			}
			entries = arr
		}
		if !cursorHookEntryPresent(entries, scriptPath) {
			// The command is the script path plus the event name as argv[1] — one script
			// serves every event, which is exactly why the hub can wire nine events with
			// one file. cursor runs `command` through a shell, so the two tokens suffice.
			entries = append(entries, map[string]any{"command": shellQuote(scriptPath) + " " + event})
		}
		hooks[event] = entries
	}
	config["hooks"] = hooks

	out, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding cursor hooks: %w", err)
	}
	return atomicWrite(path, ".hooks.json.*.tmp", out, mode)
}

// jsonShapeOf names a decoded JSON value's shape for an error message ("array", "string",
// …), so a refusal tells the operator WHAT it found rather than just that it declined.
func jsonShapeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// cursorHookEntryPresent reports whether one of an event's existing entries already
// invokes the hub's script. Matched on the SCRIPT PATH appearing anywhere in the entry's
// command (not on the exact command string) so a hand-edited invocation — extra flags, a
// different quoting style, a wrapper — counts as ours and is not duplicated on every
// create. A non-object entry, or one with no string command, is simply not a match.
func cursorHookEntryPresent(entries []any, scriptPath string) bool {
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := entry["command"].(string); ok && strings.Contains(cmd, scriptPath) {
			return true
		}
	}
	return false
}
