// Package rc implements the guest-side RC Session Convention v2 — the logic the
// shed-ext-rc binary uses to create, classify, list, and tear down remote-control
// tmux sessions (named rc-<slug>) inside a shed. It is the canonical implementation
// of docs/reference/rc-session-convention.md (owned by shed-remote-agent); tools
// invoke shed-ext-rc over SSH and consume the neutral JSON DTO it prints.
//
// This file holds the pure, side-effect-free core (slug/command/env/classification);
// tmux execution lives in tmux.go, trust pre-seeding in trust.go, and the high-level
// subcommand orchestration in ops.go.
package rc

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Kind is the RC session kind. v2 renamed the v1 values (agent/repl); there is no
// aliasing — an unrecognized value reads as legacy/unmanaged.
type Kind string

const (
	// KindClaudeBroker runs `claude remote-control` (the multiplexer/broker).
	KindClaudeBroker Kind = "claude-broker"
	// KindClaudeRC runs an interactive `claude` REPL with `/rc`.
	KindClaudeRC Kind = "claude-rc"
	// KindCodex runs the codex TUI (bare `codex`).
	KindCodex Kind = "codex"
	// KindOpencode runs the opencode TUI (bare `opencode`).
	KindOpencode Kind = "opencode"
	// KindCursor runs the cursor-agent TUI (bare `cursor-agent`).
	KindCursor Kind = "cursor"
	// KindShell runs a plain login bash.
	KindShell Kind = "shell"
)

// DefaultKind is the create-time default (distinct from the legacy/unmanaged
// fallback, which is KindClaudeBroker).
const DefaultKind = KindClaudeRC

// allKinds is every recognized kind. It is the single source of truth shared by
// IsValidKind, KindStrings, and the agent registry (agents_test.go asserts the
// registry covers exactly this set) — add new kinds here and in a spec's Kinds
// together. Order matches the pinned capabilities wire contract.
var allKinds = []Kind{KindClaudeBroker, KindClaudeRC, KindCodex, KindOpencode, KindCursor, KindShell}

// IsValidKind reports whether k is a recognized kind.
func IsValidKind(k Kind) bool {
	return slices.Contains(allKinds, k)
}

// KindStrings returns every valid kind as a string slice (registry-sourced), for CLI
// mirrors that need the accepted --kind values without hand-keeping a second list.
func KindStrings() []string {
	out := make([]string, len(allKinds))
	for i, k := range allKinds {
		out[i] = string(k)
	}
	return out
}

// IsClaudeKind reports whether the kind runs claude (and so gates on workspace
// trust): true iff the kind's spec is the claude tool.
func IsClaudeKind(k Kind) bool {
	spec, ok := specForKind(k)
	return ok && spec.Tool == toolClaude
}

// AcceptsTypedInput reports whether the kind's pane accepts a typed kickoff line
// (claude-rc/codex/opencode/cursor → a prompt, shell → a command). claude-broker's
// input is the remote URL, not the pane — and an UNKNOWN (unregistered) kind is not
// promptable either: under the unknown-kind policy a preserved raw kind renders
// neutrally with no input affordances, so prompt delivery to it is rejected (the
// existing "does not accept a prompt" error path).
func AcceptsTypedInput(k Kind) bool {
	_, ok := specForKind(k)
	return ok && k != KindClaudeBroker
}

// State is the pane-derived liveness of a session. Never stored — always classified
// from a capture-pane on demand.
type State string

const (
	StateStarting     State = "starting"
	StateReady        State = "ready"
	StateReconnecting State = "reconnecting"
	StateNeedsTrust   State = "needs-trust"
	StateNeedsAuth    State = "needs-auth"
	StateDead         State = "dead"
)

const (
	// TmuxPrefix is the reserved tmux session-name prefix for RC sessions.
	TmuxPrefix = "rc-"
	// SchemaVersion is stamped into SHED_RC_V at create.
	SchemaVersion = 2
	// MinManagedVersion is the lowest SHED_RC_V a reader still understands.
	// Deliberately decoupled from SchemaVersion so a future additive bump does
	// not force-drop older managed sessions.
	MinManagedVersion = 2
	// ToolName is this binary's stable provenance token (no '/').
	ToolName = "shed-ext-rc"
)

// SHED_RC_* env keys — the on-session metadata store (RC Session Convention v2).
const (
	envV           = "SHED_RC_V"
	envID          = "SHED_RC_ID"
	envDisplayName = "SHED_RC_DISPLAY_NAME"
	envKind        = "SHED_RC_KIND"
	envWorkdir     = "SHED_RC_WORKDIR"
	envCreatedBy   = "SHED_RC_CREATED_BY"
	envCreatedAt   = "SHED_RC_CREATED_AT"
	envTarget      = "SHED_RC_TARGET"
	envPrefix      = "SHED_RC_"
)

// Session is the neutral, target-agnostic DTO the binary prints. Optional fields are
// omitted (absent, not null) when unknown — `managed` is always present. Mirrors
// shed-remote-agent's rcSessionDtoSchema; a golden fixture cross-checks both.
type Session struct {
	Slug        string `json:"slug"`
	TmuxSession string `json:"tmux_session"`
	Kind        Kind   `json:"kind"`
	State       State  `json:"state"`
	Managed     bool   `json:"managed"`
	DisplayName string `json:"display_name,omitempty"`
	Workdir     string `json:"workdir,omitempty"`
	URL         string `json:"url,omitempty"`
	ID          string `json:"id,omitempty"`
	CreatedBy   string `json:"created_by,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	TargetLabel string `json:"target_label,omitempty"`
}

// ListResponse is the `list` subcommand's stdout shape. Capabilities is embedded so a
// single guest exec feeds both the session list and capability discovery; it is a
// pointer with omitempty so an old binary's bare `{"rc_sessions":[…]}` envelope still
// decodes (the server tolerates its absence).
type ListResponse struct {
	RCSessions   []Session     `json:"rc_sessions"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// TmuxName returns the tmux session name for a slug.
func TmuxName(slug string) string { return TmuxPrefix + slug }

// IndexByTmux keys sessions by their tmux session name (e.g. "rc-abc234") for an
// O(1) merge onto a shed's enumerated tmux sessions. Entries with an empty
// TmuxSession are skipped. This is the canonical merge-by-tmux-name helper shared
// by every consumer of a `list` envelope (the server enrichment path, the CLI)
// so the keying convention lives in exactly one place.
func IndexByTmux(sessions []Session) map[string]Session {
	out := make(map[string]Session, len(sessions))
	for _, s := range sessions {
		if s.TmuxSession != "" {
			out[s.TmuxSession] = s
		}
	}
	return out
}

const slugAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// GenSlug returns a 6-char slug from the confusable-free alphabet (no 0/o, 1/l/i)
// so it survives being read from a QR or typed URL.
func GenSlug() (string, error) {
	var b strings.Builder
	for range 6 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(slugAlphabet))))
		if err != nil {
			return "", fmt.Errorf("generating slug: %w", err)
		}
		b.WriteByte(slugAlphabet[n.Int64()])
	}
	return b.String(), nil
}

var slugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidCallerSlug reports whether a caller-supplied slug matches the grammar.
func ValidCallerSlug(slug string) bool { return slugRe.MatchString(slug) }

// HasControlChars reports whether s contains a control character (incl. newline,
// CR, tab). SHED_RC_* values must be single-line.
func HasControlChars(s string) bool {
	for _, r := range s {
		if r <= 0x1f || r == 0x7f {
			return true
		}
	}
	return false
}

// NormalizeNewlines collapses CRLF and lone CR to LF, so a multi-line prompt pasted
// from any platform is uniform before delivery.
func NormalizeNewlines(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// HasUnsafePromptChars reports a control char that must not appear in a kickoff
// prompt. Newlines and tabs are allowed — a multi-line prompt is delivered via a
// bracketed paste (see sendBlock) — but every other control char is rejected: C0
// (`<= 0x1f`, notably ESC, so a paste can't smuggle the bracketed-paste end sequence
// and break out into raw keystrokes), DEL (`0x7f`), and C1 (`0x80`–`0x9f`, e.g. the
// 8-bit CSI `0x9b`, which terminals that honor C1 would treat as a control sequence).
// Normalize with NormalizeNewlines first.
func HasUnsafePromptChars(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r <= 0x1f || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
	}
	return false
}

// shellQuote wraps s in single quotes, escaping embedded single quotes with the
// POSIX `'\”` trick, so it is a single safe shell token.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Generic permission modes: a tri-state accepted by ALL kinds and mapped per agent
// to real flags in the registry (the VM is already the sandbox). claude additionally
// accepts its full historical --permission-mode set via the claude spec's ExtraModes.
const (
	PermModeDefault = "default"
	PermModeAuto    = "auto"
	PermModeSkip    = "skip"
)

// PermissionModeBypass is claude's full-bypass --permission-mode value (what the
// generic "skip" maps to for claude). Its session shows a one-time acceptance dialog
// that the create poller auto-confirms.
const PermissionModeBypass = "bypassPermissions"

// genericPermModes returns the generic tri-state permission modes valid for every
// kind.
func genericPermModes() []string {
	return []string{PermModeDefault, PermModeAuto, PermModeSkip}
}

// ValidPermissionMode reports whether m is a permission mode the claude kinds accept
// (the generic tri-state plus claude's historical set). Retained for callers that
// only care about the claude posture; PermModeAcceptedBy is the kind-aware check.
func ValidPermissionMode(m string) bool {
	spec, ok := specForKind(KindClaudeRC)
	return ok && spec.validMode(m)
}

// PermModeAcceptedBy reports whether kind accepts permission mode m: the generic
// tri-state for every kind, plus claude's historical set for the claude kinds. Used
// by the CLI to fail fast locally before any network round-trip.
func PermModeAcceptedBy(kind Kind, m string) bool {
	_, ok := permFlagsFor(kind, m)
	return ok
}

// validatePermissionMode returns a domain error when mode is not accepted by kind:
// a claude-only mode used with a non-claude kind names the generic set; anything else
// is a plain invalid-mode error. "" (no posture) always validates.
func validatePermissionMode(kind Kind, mode string) error {
	if mode == "" {
		return nil
	}
	if PermModeAcceptedBy(kind, mode) {
		return nil
	}
	if ValidPermissionMode(mode) { // claude accepts it → it's a claude-only mode here
		return fmt.Errorf("%w: permission mode %q is claude-only; %s kinds accept default|auto|skip",
			ErrBadArgs, mode, kind)
	}
	return fmt.Errorf("%w: invalid permission mode %q (want default|auto|skip)", ErrBadArgs, mode)
}

// InnerCommand builds the command the tmux session runs for a kind. interactiveShell
// wraps the claude kinds in `bash -ic` so a login rc-file loads PATH (nvm/asdf)
// before claude is exec'd (native machines); sheds bake claude into the system path.
//
// permissionMode (claude kinds only; "" = omit) sets claude's `--permission-mode`
// so a remote-control session can run unattended (e.g. "auto" or
// "bypassPermissions"). For claude-rc the mode is delivered via the
// `--remote-control` flag form rather than the bare `/rc` slash command — the slash
// form takes no flags, while `--remote-control --permission-mode <m>` is the
// verified form that carries the posture into the live session and still yields a
// `session_*` URL (so the pane classifier treats it identically). With no mode,
// claude-rc keeps the original `/rc` form for backward compatibility.
//
// An unregistered kind falls back to `bash -l`.
func InnerCommand(kind Kind, displayName, permissionMode string, interactiveShell bool) string {
	spec, ok := specForKind(kind)
	if !ok {
		return "bash -l"
	}
	return spec.InnerCommand(kind, displayName, permissionMode, interactiveShell)
}

var (
	notTrustedRe  = regexp.MustCompile(`(?i)Workspace not trusted`)
	safetyCheckRe = regexp.MustCompile(`(?i)Quick safety check`)
	trustFolderRe = regexp.MustCompile(`(?i)Yes,\s*I trust this folder`)
)

// IsTrustPrompt reports whether the pane is showing claude's first-run
// workspace-trust prompt (used by accept-trust to verify before sending Enter).
func IsTrustPrompt(pane string) bool {
	return notTrustedRe.MatchString(pane) || safetyCheckRe.MatchString(pane) ||
		trustFolderRe.MatchString(pane)
}

var (
	bypassWarnRe   = regexp.MustCompile(`(?i)Bypass Permissions mode`)
	bypassAcceptRe = regexp.MustCompile(`(?i)Yes,\s*I accept`)
)

// IsBypassAcceptPrompt reports whether the pane is showing claude's one-time
// "Bypass Permissions mode" acceptance dialog (shown when a session starts under
// --permission-mode bypassPermissions). Option "1. No, exit" is pre-selected, so a
// creator must move to "2. Yes, I accept" before Enter (see acceptBypassPrompt).
func IsBypassAcceptPrompt(pane string) bool {
	return bypassWarnRe.MatchString(pane) && bypassAcceptRe.MatchString(pane)
}

// ClassifyPane derives (state, url) from a captured pane for a kind. Mirrors
// shed-remote-agent's classifyPane. An unregistered (unknown) kind renders neutrally
// as a plain shell pane — no kind-specific affordances or claude URL (the unknown-kind
// policy). For agent kinds a shared shed-guest dead check runs first: if the agent has
// exited back to the login shell, the session is dead regardless of any auth/trust
// text still in scrollback. Shell is exempt (its prompt is the ready state).
func ClassifyPane(kind Kind, pane string) (State, string) {
	spec, ok := specForKind(kind)
	if !ok {
		r := classifyShell(kind, pane)
		return r.State, r.URL
	}
	if spec.Tool != toolShell && exitedToShell(pane) {
		return StateDead, ""
	}
	r := spec.Classify(kind, pane)
	return r.State, r.URL
}

var canonicalIntRe = regexp.MustCompile(`^\d+$`)

// isManagedVersion reports whether a raw SHED_RC_V value denotes a managed session:
// a canonical positive integer >= MinManagedVersion. A v1 (or malformed) value is
// legacy/unmanaged.
func isManagedVersion(raw string) bool {
	if !canonicalIntRe.MatchString(raw) {
		return false
	}
	n, err := strconv.Atoi(raw)
	return err == nil && n >= MinManagedVersion
}

// parseKind maps a managed session's SHED_RC_KIND to a Kind. An unrecognized value is
// PRESERVED verbatim (the unknown-kind policy): a reader that doesn't know the kind
// keeps the raw string and renders it neutrally (name + state only, no claude URL
// affordances) rather than inheriting claude-broker behavior. ClassifyPane falls back
// to a neutral shell-style classification for such kinds.
func parseKind(raw string) Kind {
	return Kind(raw)
}
