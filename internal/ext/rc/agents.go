package rc

import (
	"regexp"
	"strconv"
	"strings"
)

// AgentSpec is the single per-tool table entry that gathers everything
// agent-specific behind one lookup: how a kind's inner tmux command is built, how
// its pane is classified into a lifecycle state, its optional trust/onboarding
// pre-seed, and the permission modes it accepts. One spec backs one or more Kinds
// (claude backs both claude-broker and claude-rc). This is the seam every agent
// (claude/codex/opencode/cursor/shell) slots into without touching the ops/classify
// core.
type AgentSpec struct {
	// Tool is the agent's stable identity token ("claude", "codex", "opencode",
	// "cursor", "shell") — the key under capabilities.agents.
	Tool string
	// Bin is the executable probed for capabilities (`command -v <Bin>` +
	// `<Bin> --version`). Usually equal to Tool, but cursor's binary is
	// "cursor-agent". Empty for tools with nothing to probe (shell).
	Bin string
	// Kinds are the session kinds this tool provides. Disjoint across specs.
	Kinds []Kind
	// Lane is the session lane every kind of this tool runs in (contract v2):
	// LaneTUI (an rc-tmux pane driven through capture/send-keys) or LaneStructured
	// (a native protocol lane — codex app-server, cursor ACP, opencode server API).
	// A KIND IS LANE-HOMOGENEOUS: all sessions of one kind share one lane, which is
	// what keeps the kind-keyed kind_features map a complete description of what a
	// client can do. A structured lane therefore arrives as a DISTINCT kind beside
	// the TUI kind (with its own spec + kind_features row), never as a second lane
	// on an existing kind. Every spec in this phase is LaneTUI.
	Lane string
	// InnerCommand builds the command the tmux session runs for one of the tool's
	// kinds. Signature mirrors the exported InnerCommand: display name (already
	// resolved by the caller), the generic/claude permission mode ("" = omit),
	// whether to wrap in `bash -ic` (native machines / non-shed PATH), and a trailing
	// port — opencode's allocated loopback SSE/HTTP server port (0 = none / not
	// opencode). Every builder accepts port so the func-value signature stays uniform
	// across the registry; only innerCommandTUI's opencode branch actually consumes it
	// (claude/shell ignore it).
	InnerCommand func(kind Kind, displayName, permissionMode string, interactiveShell bool, port int) string
	// Classify derives a pane's lifecycle state (+ optional url/id) for one of the
	// tool's kinds. Takes the kind because a tool can back several kinds with
	// different ready/url regex sets (claude-broker vs claude-rc). The shared
	// shed-guest "exited to shell" dead signal is applied by ClassifyPane before this
	// runs, so a spec's Classify only handles its live states.
	Classify func(kind Kind, pane string) PaneResult
	// Preseed prepares on-disk tool config before the session launches: claude's trust +
	// onboarding gates (so a fresh session reaches ready unattended), cursor's hook relay
	// (so the hub gets a signal at all — see PreseedCursorHooks). nil when the tool needs
	// none, or when its trust gate is auto-accepted from the pane instead (codex).
	// Best-effort by contract: Create reports a failure through CreateOptions.Warnf and
	// carries on.
	Preseed func(workdir string, getenv func(string) string) error
	// PermMap maps a generic permission mode (default/auto/skip) to this tool's argv
	// flags. Every spec defines all three keys; a value of nil means "no posture flag"
	// (the VM is already the sandbox). Kept in one table because the underlying CLI
	// flags churn (see the generic-mode table in the design doc).
	PermMap map[string][]string
	// ExtraModes are tool-specific permission-mode values accepted beyond the generic
	// tri-state (claude's historical --permission-mode set). Passed through verbatim as
	// `--permission-mode <value>`. Empty for tools with only the generic modes.
	ExtraModes []string
	// AuthHint is the human remediation for this tool's needs-auth state — what to run
	// in a terminal to log in (surfaced by clients via AuthHintFor). Empty for tools
	// with no auth (shell).
	AuthHint string
	// PromptAnchor matches a stable pane that is sitting at this tool's empty input
	// composer — the "waiting for the operator to type" chrome. The pane-stability
	// engine (stability.go) uses it to distinguish needs_input (quiet AND at a prompt)
	// from plain idle (quiet, no prompt visible). nil for tools whose activity is not
	// pane-derived and that expose no anchor. Matched against the RAW captured pane
	// (not the normalized snapshot) so composer chrome survives.
	PromptAnchor *regexp.Regexp
	// ApprovalAnchor matches a pane showing this tool's APPROVAL dialog — the chrome a
	// TUI puts up when it is blocked on the operator's yes/no. It is the pane-side
	// counterpart to a lane's native approval events: kinds whose approvals never reach
	// a protocol (codex's rollout carries no approval record; cursor has no approval
	// hook) can only be known to be blocked by looking at the pane. Two consumers: the
	// input gate rejects a post while it matches (typed input would answer the dialog by
	// accident), and reconcile derives needs_approval from it (debounced two ticks each
	// way, emitting informational approval rows — see hub_reconcile.go).
	//
	// Declared by codex and cursor — the two kinds whose approvals reach no protocol at
	// all. Every anchor is built on the widget's OPTION-ROW chrome (the gutter/selection
	// marker plus the whole-line label envelope), never on a headline alone: headlines get
	// quoted in agent prose and survive in the transcript after the dialog is answered, so
	// they can neither be trusted nor observed to clear, whereas option rows exist exactly
	// while the widget is mounted. (cursor's anchor conjoins a headline WITH an option row,
	// because its TUI draws no footer to conjoin against — see cursorApprovalAnchorRe.)
	// Matched, like PromptAnchor, against the RAW captured pane so dialog chrome survives
	// normalization.
	ApprovalAnchor *regexp.Regexp
	// ComposerUnderModal records that this tool KEEPS ITS INPUT COMPOSER DRAWN while a
	// modal owns the keyboard — so its PromptAnchor is NOT evidence the session is
	// accepting input. It is a per-tool rendering fact, verified against the committed
	// fixtures (TestComposerUnderModalMatchesTheFixtures):
	//
	//   codex  — false: the approval overlay REPLACES the composer area, so a pane showing
	//            the dialog does not match codexPromptAnchorRe at all. The prompt anchor is
	//            self-sufficient evidence there.
	//   cursor — TRUE: the prompt bar renders its text input in a disabled, dimmed variant
	//            with the placeholder still drawn beneath the decision surface, so
	//            cursorReadyRe matches a pane that is blocked on an approval.
	//
	// The consequence is in the input gate (hub.go inputAccepted): for such a kind, an
	// EXPIRED-WORKING watcher verdict may not fall through to the degraded
	// "composer-anchor accepts" path. That path exists for kinds whose composer means
	// "ready"; where the composer is drawn under a modal it means nothing, and accepting
	// there would type into whatever widget owns the keyboard — for cursor, a line
	// containing "y" would hit its y=approve keybind and answer the dialog.
	ComposerUnderModal bool
}

// PaneResult is a Classify outcome: the derived lifecycle state plus the optional
// remote-control url and session id. url/id are claude-remote-control-specific and
// stay empty for other tools (the DTO omits empties).
type PaneResult struct {
	State State
	URL   string
	ID    string
}

// permFlags returns the argv flags for a permission mode and whether the mode is
// valid for this tool. "" (no posture) is always valid and yields no flags. Generic
// modes resolve through PermMap; a tool's ExtraModes resolve to `--permission-mode
// <mode>` (claude only).
func (s *AgentSpec) permFlags(mode string) ([]string, bool) {
	if mode == "" {
		return nil, true
	}
	if flags, ok := s.PermMap[mode]; ok {
		return flags, true
	}
	for _, m := range s.ExtraModes {
		if m == mode {
			return []string{"--permission-mode", mode}, true
		}
	}
	return nil, false
}

// validMode reports whether m is a (non-empty) permission mode this tool accepts. The
// empty string is the absence of a mode, not a mode, so it is rejected here even
// though permFlags("") is a valid no-posture resolution for the inner-command path.
func (s *AgentSpec) validMode(m string) bool {
	if m == "" {
		return false
	}
	_, ok := s.permFlags(m)
	return ok
}

// Session lanes (AgentSpec.Lane / Session.Lane — the contract-v2 wire values).
const (
	// LaneTUI is the universal substrate: an rc-tmux session whose pane is captured
	// and driven with send-keys. Every kind in this phase is a TUI lane, and an
	// UNKNOWN (unregistered) kind renders as one too — the neutral rendering a client
	// already applies to it is exactly the TUI affordance set.
	LaneTUI = "tui"
	// LaneStructured is an agent driven over its native protocol (codex app-server,
	// cursor ACP, opencode server API) rather than through a pane. Declared now so
	// the wire values are fixed up front; no kind derives it in this phase.
	LaneStructured = "structured"
)

// laneForKind returns the lane a kind's sessions run in, defaulting to LaneTUI for an
// unregistered kind or a spec that declares none (the unknown-kind policy: a preserved
// raw kind renders neutrally, which is the TUI affordance set). The DTO's `lane` is
// ALWAYS present, so this never returns "".
func laneForKind(k Kind) string {
	if spec, ok := specForKind(k); ok && spec.Lane != "" {
		return spec.Lane
	}
	return LaneTUI
}

// Tool-name tokens (AgentSpec.Tool).
const (
	toolClaude   = "claude"
	toolCodex    = "codex"
	toolOpencode = "opencode"
	toolCursor   = "cursor"
	toolShell    = "shell"
)

// Prompt anchors — the empty-composer / waiting-for-input chrome per tool, used by
// the pane-stability engine to split needs_input from idle on a quiet pane. Each is
// pinned to a live-captured fixture line under testdata/panes (or the SUMMARY.txt
// capture notes where no ready fixture is committed) — see the doc comment on each.
var (
	// codexPromptAnchorRe matches codex's empty-composer placeholder — the prompt
	// codex draws when idle and waiting for a line. Composed from the shared
	// codexComposerPlaceholder literal (one source of truth with codexReadyRe, the
	// opencode composition pattern). Deliberately narrower than codexReadyRe (which
	// also matches the boot banner) so ONLY a pane genuinely parked at the composer
	// reads needs_input.
	codexPromptAnchorRe = regexp.MustCompile(codexComposerPlaceholder)
	// opencodePromptAnchorRe matches opencode's fresh-composer placeholder
	// ("Ask anything...", testdata/panes/opencode-ready.txt) or its persistent
	// "ctrl+p commands" footer (opencode-ready-active.txt) — the placeholder is the
	// clean idle-at-prompt signal, the footer catches a mid-conversation quiescent
	// pane where the placeholder is gone. Composed from the two classifier ready-chrome
	// regexes (opencodePlaceholderRe | opencodeFooterRe) rather than re-spelling their
	// literals, so the anchor shares one source of truth with them and can't silently
	// drift out of sync if opencode rebrands either string.
	opencodePromptAnchorRe = regexp.MustCompile(opencodePlaceholderRe.String() + "|" + opencodeFooterRe.String())
	// claudePromptAnchorRe matches claude's composer box ("> Try \"…\"" placeholder)
	// or its "? for shortcuts" footer hint. No claude-ready pane fixture is committed
	// (only claude-dead.txt); the anchor text is taken from the live capture recorded
	// in testdata/panes/SUMMARY.txt (CLAUDE section: composer `> Try "refactor
	// <filepath>"`, footer `? for shortcuts · <- for agents`). claude's activity is
	// primarily transcript-derived; this anchor is the stability fallback.
	claudePromptAnchorRe = regexp.MustCompile(`(?m)^\s*>\s+Try "|\? for shortcuts`)
	// shellPromptAnchorRe matches a bare shed guest login-shell prompt line
	// ("[shed:<name>] <cwd> $") anywhere in the pane — a shell session idle at its
	// PS1 is waiting for a command. Mirrors shedShellPromptRe but multiline (matches a
	// prompt line within the whole capture, not just the trimmed last line).
	shellPromptAnchorRe = regexp.MustCompile(`(?m)^\s*\[shed:[^\]]+\][^$]*\$\s*$`)
)

// Approval anchors — the blocked-on-a-yes/no dialog chrome per tool (AgentSpec.
// ApprovalAnchor). Structural, whole-line expressions in the cursorReadyRe tradition:
// what makes a match trustworthy is the WIDGET SHAPE around the words, not the words.
var (
	// codexApprovalAnchorRe matches one rendered OPTION ROW of codex's approval overlay
	// (tui/src/bottom_pane/approval_overlay.rs, rendered through list_selection_view.rs).
	// Fixtures: testdata/panes/codex-ready-approval-exec.txt (exec prompt) and
	// codex-ready-approval-network.txt (network prompt), adapted from the codex repo's
	// committed TUI snapshots; codex-ready-approval-resolved.txt (overlay gone) and
	// codex-ready-approval-quoted.txt (the same words as assistant prose) are the
	// negatives.
	//
	// Why option rows and not the HEADLINE ("Would you like to run the following
	// command?"): a headline is ordinary English that an agent quotes back in its own
	// prose all the time, and — decisively — the headline scrolls into the transcript
	// while the overlay is torn down, so it does NOT disappear when the dialog is
	// answered. The option rows do: they exist only while the widget is mounted, which
	// is exactly the interval the anchor is supposed to describe.
	//
	// Shape, from list_selection_view.rs's row builder (`format!("{prefix} {n}. ")`,
	// prefix '›' when selected and ' ' otherwise, an optional " (key)" shortcut hint
	// appended by the row renderer):
	//
	//	› 1. Yes, proceed (y)
	//	  2. No, and tell Codex what to do differently (esc)
	//
	// The regex therefore requires TWO pieces of widget chrome, in order:
	//
	//  1. a whole option row at COLUMN 0 with no leading indent — the selection gutter
	//     (`›`, or the single space that replaces it on an unselected row), a space, the
	//     enabled-row number, `. `, one of the pinned labels, an optional parenthesized
	//     shortcut hint, and end of line;
	//  2. within the next few lines, the overlay's footer `Press enter to confirm or esc
	//     to cancel` (codex's OTHER list views end "esc to go back" — this exact wording
	//     belongs to the approval overlay, and the cross-thread variant only APPENDS
	//     " or o to open thread", so a prefix match holds).
	//
	// The conjunction is what makes the quoted-prose fixture
	// (codex-ready-approval-quoted.txt) a genuine negative. Requiring only the option row
	// would not be enough on its own: codex renders assistant text with a `• ` bullet and
	// a two-space body indent, so a markdown list inside a message can land at exactly
	// the same column as an UNSELECTED option row. Requiring only the `›` marker would
	// fail the opposite way — the operator can arrow the marker onto a row whose label is
	// outside the pinned set, and the anchor would report the dialog gone while it is
	// still up. Demanding the row AND the footer costs an agent-prose false positive both
	// pieces of chrome in the right order, and costs a live dialog nothing.
	//
	// KNOWN LIMITATION (accepted, inherent to a pane-derived signal): a VERBATIM
	// reproduction of the whole dialog on screen — `cat`ing this package's own fixture
	// files, pasting a transcript that preserves the gutter and the footer — reproduces
	// both pieces of chrome and does read as an approval. No regex can separate "the
	// widget is mounted" from "a perfect picture of the widget is on screen" when the
	// pane is all we get, and the debounce cannot help (the text is static, so it agrees
	// with itself indefinitely). Two things bound the damage: the anchor is evaluated
	// against the VISIBLE FRAME only (captureVisiblePane), so a false episode lives
	// exactly as long as the text is on screen and clears when it scrolls away, and the
	// consequence is an informational row plus a decision-less pending entry — nothing is
	// auto-approved and nothing is auto-denied. The real fix is a structured lane (codex
	// app-server), not a better regex.
	//
	// Label set (deliberately small, per plan 008 §3.6): every exec / patch / network
	// approval renders at least one of these three rows — "Yes, proceed" (exec+patch
	// option 1), "Yes, just this once" (network option 1), and "No, and tell Codex what
	// to do differently" (the Cancel option, present on all three) — so the anchor is
	// stable no matter which row the operator has arrowed onto. The shortcut hint is
	// optional because the approval keymap is configurable; the labels are compiled
	// literals. KNOWN GAP, and the extension point when it matters: the newer
	// additional-permissions overlay ("Would you like to grant these permissions?") uses
	// a disjoint label set ("Yes, grant these permissions for this turn" … "No, continue
	// without permissions") and is not matched.
	codexApprovalAnchorRe = regexp.MustCompile(
		`(?m)^[› ] \d+\. (?:Yes, proceed|Yes, just this once|No, and tell Codex what to do differently)(?: \([^()\n]{1,16}\))? *\n` +
			`(?:[^\n]*\n){0,12} *Press enter to confirm or esc to cancel`)

	// cursorApprovalAnchorRe matches cursor-agent's approval prompt (its "decision
	// surface"): the widget the TUI mounts when a command is not in the allowlist, or a
	// hook asked for approval, and the turn is blocked on the operator.
	//
	// SOURCE, not guesswork — the strings and the row chrome are read out of the installed
	// cursor-agent bundle (2026.08.11-e8db854), which ships its TUI as readable (minified)
	// JS:
	//   src/components/prompt/decision-logic.ts     — the headline per operation type
	//                                                 (`Run this command?` …) and the option
	//                                                 label+hint list per type
	//   src/components/prompt/decision-dropdown.tsx — the row: paddingLeft, then the
	//                                                 selection marker ("→ " selected,
	//                                                 "  " not — pager/tokens.ts), the
	//                                                 label, then " (hint)"
	//   the policy engine's reason list            — `Not in allowlist: <cmds>` and
	//                                                 `Hook requested approval: <msg>`
	//
	// THE CONJUNCTION (the structural equivalent of codex's option-row + footer): a
	// whole-line HEADLINE, then within a few lines a whole-line OPTION ROW. Neither half is
	// trusted alone. A headline alone is ordinary English an agent quotes back — and, like
	// codex's, it scrolls into the transcript and survives the answer, so it can never be
	// observed to clear (testdata/panes/cursor-ready-approval-resolved.txt keeps it
	// deliberately). An option label alone is a phrase an agent explaining the prompt puts
	// at the end of a line. Both, in order, is the widget.
	//
	// The row's accepted gutter is exactly what the dropdown renders: leading indent and an
	// optional "→ " marker. Markdown bullets (`- `, `* `) are NOT accepted, because that is
	// precisely how an assistant renders a list of the same labels — see
	// cursor-ready-approval-quoted.txt, which quotes the labels inline AND as a bulleted
	// list and must not match.
	//
	// THE LABEL SET IS EXHAUSTIVE OVER ALL EIGHT DECISION SURFACES, deliberately — and this
	// is where it differs from codex's small pinned set. A surface whose rows do not match
	// opens no episode, which does not merely lose a badge: the pane freezes, the watcher's
	// working verdict expires after the grace, and the input gate's degraded path would see
	// cursor's still-drawn composer and accept a posted line straight into the widget, where
	// a "y" anywhere in the text hits cursor's y=approve keybind. Missing a surface here is
	// therefore an AUTO-APPROVAL bug, not a cosmetic one, so every label decision-logic.ts
	// can render is listed: shell (Run (once) / Run outside sandbox (once) / Add Shell(…) to
	// allowlist? / Skip & tell the agent what to do instead), MCP (Run (once) / Allowlist MCP
	// Tool / Reject & propose changes / Skip), delete (Delete / Keep), write (Proceed /
	// Reject & propose changes / Add Write(…) to allowlist? / Add to allowlist), web search
	// (Allow search / Skip), web fetch (Fetch / Always allow <domain> / Skip), the edit
	// surface (headline-only), and the shared auto-run row (Run Everything / Run in Sandbox /
	// Checking Run Everything availability). The generic one-word labels (Skip, Keep, Delete,
	// Proceed, Fetch) are only safe because the HEADLINE half carries the trust — neither
	// half is ever matched alone. The three dynamic labels are anchored on their fixed
	// prefix, since they interpolate a path/command/domain.
	//
	// SAFETY-CRITICAL: for a ComposerUnderModal kind (cursor), THIS regex matched against the
	// FRESH visible pane is the SOLE guard against typing into a modal. inputAccepted's
	// expired-working arm recovers gated input whenever no ApprovalAnchor matches the visible
	// frame AND pane-stability has settled on needs_input (plan 008 C9) — there is no longer a
	// blanket reject beneath it. Because cursor draws its composer, disabled, UNDER the dialog,
	// a decision surface this regex does NOT know about still settles the composer to
	// needs_input, and the gate would then ACCEPT a keystroke straight into that widget (where
	// a "y" answers it). Keeping cursorApprovalAnchorRe EXHAUSTIVE across every decision surface
	// decision-logic.ts can render is therefore load-bearing, not cosmetic — a missed or newly
	// added surface is an auto-approval hole. TestCursorApprovalAnchorCoversEveryDecisionSurface
	// pins the exhaustiveness; a future cursor-upgrade audit that adds a surface MUST teach this
	// regex in the same change.
	//
	// KNOWN LIMITATION, shared with codex and inherent to a pane-derived signal: a verbatim
	// reproduction of the widget on screen reads as the widget. The consequence is an
	// informational row plus a refused keystroke — nothing is auto-approved — and the anchor
	// only ever sees the VISIBLE frame (captureVisiblePane), so a false episode clears the
	// moment the text scrolls away.
	//
	// FIXTURE PROVENANCE: reconstructed from the bundle above, NOT captured from a live
	// pane (the hook spike recorded payloads, not panes, and cursor's account was not
	// re-run for a capture). The labels, hints, headline, marker and indentation are
	// source-faithful; the surrounding transcript/composer frame is the live
	// cursor-ready-active.txt shape. Re-capture live when convenient — see
	// testdata/panes/SUMMARY.txt.
	cursorApprovalAnchorRe = regexp.MustCompile(
		`(?m)^[ \t]*` + cursorApprovalHeadline + `[ \t]*\n` +
			`(?:[^\n]*\n){0,8}` +
			`[ \t]*(?:→ )?` + cursorApprovalOption + `[ \t]*$`)
)

// cursorApprovalHeadline is the decision surface's bold headline, one alternative per
// operation type (decision-logic.ts), plus the hook-approval reason line — which owns its
// line, carries the hook's message after the colon, and appears in no other widget.
const cursorApprovalHeadline = `(?:Run this command\?|Run this command outside the sandbox\?` +
	`|Run this MCP tool\?|Delete this file\?|Write to this file\?` +
	`|Allow this web search\?|Allow this web fetch\?|Proceed with this edit\?` +
	`|Hook requested approval:[^\n]*)`

// cursorApprovalOption is one option row's LABEL — every label decision-logic.ts can push,
// across all eight decision surfaces (see the anchor's doc for why exhaustiveness is a
// safety property here, not a nicety). Two families:
//
//   - FIXED labels, which own their whole line up to an optional " (hint)";
//   - PREFIX labels, which interpolate a path/command/domain ("Add Write(<path>) to
//     allowlist?", "Add Shell(<cmds>) to allowlist?", "Always allow <domain>") and so
//     consume the rest of the line.
//
// The hint suffix is attached HERE rather than at the call site, because a prefix label
// already swallows its own trailing hint.
const cursorApprovalOption = `(?:(?:Run \(once\)|Run outside sandbox \(once\)|Run Everything` +
	`|Run in Sandbox|Checking Run Everything availability` +
	`|Skip & tell the agent what to do instead|Reject & propose changes` +
	`|Allowlist MCP Tool|Add to allowlist|Allow search|Delete|Keep|Proceed|Fetch|Skip)` +
	`(?: \([^()\n]{1,16}\))?` +
	`|(?:Always allow |Add Write\(|Add Shell\()[^\n]*)`

// noPostureMap is the generic tri-state mapping for tools whose modes need no flags
// at all (shell): every generic mode is accepted but produces nothing. Agent specs
// define their own maps with the real flags.
var noPostureMap = map[string][]string{
	PermModeDefault: nil,
	PermModeAuto:    nil,
	PermModeSkip:    nil,
}

// agentRegistry is the canonical per-tool table. Each spec's Kinds are disjoint,
// and together they cover every IsValidKind kind (asserted by agents_test.go).
var agentRegistry = []*AgentSpec{
	{
		Tool:         toolClaude,
		Bin:          "claude",
		Kinds:        []Kind{KindClaudeBroker, KindClaudeRC},
		Lane:         LaneTUI,
		InnerCommand: innerCommandClaude,
		Classify:     classifyClaude,
		Preseed:      PreseedClaudeConfig,
		// Generic tri-state → claude's --permission-mode flags. "default" passes no
		// posture (claude's own default); "skip" is full bypass.
		PermMap: map[string][]string{
			PermModeDefault: nil,
			PermModeAuto:    {"--permission-mode", "auto"},
			PermModeSkip:    {"--permission-mode", PermissionModeBypass},
		},
		// claude additionally accepts its full historical --permission-mode set.
		ExtraModes:   []string{"acceptEdits", "plan", "dontAsk", PermissionModeBypass},
		AuthHint:     "run `claude` \u2192 /login",
		PromptAnchor: claudePromptAnchorRe,
	},
	{
		Tool:         toolCodex,
		Bin:          "codex",
		Kinds:        []Kind{KindCodex},
		Lane:         LaneTUI,
		InnerCommand: innerCommandTUI("codex"),
		Classify:     classifyCodex,
		// codex's directory-trust gate is a pre-selected "Yes, continue" prompt, so it
		// is auto-accepted from the pane in waitUntilReady rather than config-preseeded.
		Preseed: nil,
		PermMap: map[string][]string{
			PermModeDefault: nil,
			// codex 0.144.1 removed the top-level `--full-auto` convenience flag; the
			// autonomous-with-approvals posture is now spelled explicitly as
			// `--ask-for-approval on-request` (model decides when to escalate) +
			// `--sandbox workspace-write` (write inside the workspace, the VM is the
			// outer sandbox). Passing the old `--full-auto` makes codex exit immediately
			// with `error: unexpected argument '--full-auto' found`.
			PermModeAuto: {"--ask-for-approval", "on-request", "--sandbox", "workspace-write"},
			PermModeSkip: {"--dangerously-bypass-approvals-and-sandbox"},
		},
		AuthHint:     "run `codex` and complete login (`codex login`)",
		PromptAnchor: codexPromptAnchorRe,
		// codex's approvals never reach a protocol — its rollout JSONL filters every
		// approval record out (see plan 008 §2), so a session blocked on the overlay is
		// byte-identical in the log to a long-running tool call. The pane is the only
		// evidence there is.
		ApprovalAnchor: codexApprovalAnchorRe,
	},
	{
		Tool:         toolOpencode,
		Bin:          "opencode",
		Kinds:        []Kind{KindOpencode},
		Lane:         LaneTUI,
		InnerCommand: innerCommandTUI("opencode"),
		Classify:     classifyOpencode,
		Preseed:      nil,
		PermMap: map[string][]string{
			PermModeDefault: nil,
			// opencode's --auto approves everything not denied — the closest mapping for
			// both auto and skip until a finer split exists.
			PermModeAuto: {"--auto"},
			PermModeSkip: {"--auto"},
		},
		AuthHint:     "run `opencode auth login`",
		PromptAnchor: opencodePromptAnchorRe,
	},
	{
		Tool:  toolCursor,
		Bin:   "cursor-agent",
		Kinds: []Kind{KindCursor},
		Lane:  LaneTUI,
		// --trust skips cursor's workspace-trust dialog, which is otherwise a
		// hard stop for an unattended kickoff: neither classifier models that
		// dialog (it postdates the pane fixtures), so a fresh workspace read
		// `starting` until the wait timed out. Same posture as claude's trust
		// PRESEED (PreseedClaudeConfig marks the workdir trusted) — the rc
		// environment is a sandbox VM or a deliberately-targeted machine.
		// Verified live 2026-08-17: without the flag the dialog shows; with it
		// the composer is immediately ready.
		InnerCommand: innerCommandTUI("cursor-agent", "--trust"),
		Classify:     classifyCursor,
		// cursor's preseed is not a trust/onboarding gate (it has none) — it installs the
		// hub's hook relay into ~/.cursor/hooks.json, which is the ONLY live signal a cursor
		// session produces (see preseed_cursor.go and watch_cursor.go). Best-effort like
		// every Preseed: a failure costs the session its feed, never its create.
		Preseed: PreseedCursorHooks,
		PermMap: map[string][]string{
			PermModeDefault: nil,
			// cursor has no mid-tier posture; auto stays default until one exists.
			PermModeAuto: nil,
			PermModeSkip: {"--force"},
		},
		AuthHint: "run `cursor-agent login`",
		// cursorReadyRe is the authed composer placeholder ("→ Plan, search, build
		// anything", testdata/panes/cursor-ready.txt) — reused as the prompt anchor.
		PromptAnchor: cursorReadyRe,
		// cursor emits NO approval hook event (verified in the spike): a session blocked on
		// the allowlist prompt looks, on the hook stream, exactly like a long tool call. So
		// — as for codex — the pane is the only evidence, and it is what drives both the
		// needs_approval verdict and the input gate's refusal.
		ApprovalAnchor: cursorApprovalAnchorRe,
		// cursor draws its composer (disabled) UNDER the decision surface, so the prompt
		// anchor above keeps matching while an approval owns the keyboard — see the field's
		// doc for what the input gate does about it.
		ComposerUnderModal: true,
	},
	{
		Tool:         toolShell,
		Bin:          "",
		Kinds:        []Kind{KindShell},
		Lane:         LaneTUI,
		InnerCommand: innerCommandShell,
		Classify:     classifyShell,
		Preseed:      nil,
		// A shell has no permission posture; the generic modes are accepted (valid for
		// ALL kinds) but produce no flags.
		PermMap:      noPostureMap,
		PromptAnchor: shellPromptAnchorRe,
	},
}

// promptAnchorFor returns the kind's prompt-anchor regex, or nil for an unregistered
// kind or a spec that declares none. Used by the pane-stability engine to decide
// needs_input vs idle on a quiet pane.
func promptAnchorFor(k Kind) *regexp.Regexp {
	if spec, ok := specForKind(k); ok {
		return spec.PromptAnchor
	}
	return nil
}

// approvalAnchorFor returns the kind's approval-dialog regex, or nil for an unregistered
// kind or a spec that declares none (codex and cursor declare one — see
// AgentSpec.ApprovalAnchor). nil means "this kind has no pane-derived approval signal",
// which callers must treat as "no evidence", never as "not blocked".
func approvalAnchorFor(k Kind) *regexp.Regexp {
	if spec, ok := specForKind(k); ok {
		return spec.ApprovalAnchor
	}
	return nil
}

// composerUnderModal reports whether the kind's composer stays drawn while a modal owns
// the keyboard (AgentSpec.ComposerUnderModal). false for an unregistered kind — and for a
// kind with no prompt anchor at all the question never arises.
func composerUnderModal(k Kind) bool {
	spec, ok := specForKind(k)
	return ok && spec.ComposerUnderModal
}

// kindToSpec indexes the registry by kind for O(1) lookup. It is populated in init()
// rather than an initializer expression so the static reference chain
// (agentRegistry → inner-command builders → permFlagsFor → specForKind → kindToSpec)
// does not form a variable-initialization cycle: the inner-command builders resolve a
// kind's permission flags through the registry at RUN time, long after init.
var kindToSpec = map[Kind]*AgentSpec{}

func init() {
	for _, s := range agentRegistry {
		for _, k := range s.Kinds {
			kindToSpec[k] = s
		}
	}
}

// specForKind returns the AgentSpec backing a kind, ok=false for an unregistered
// (invalid) kind. Every IsValidKind kind resolves.
func specForKind(k Kind) (*AgentSpec, bool) {
	s, ok := kindToSpec[k]
	return s, ok
}

// permFlagsFor resolves a kind's permission-mode flags. ok=false for an unknown kind
// or a mode the kind does not accept.
func permFlagsFor(k Kind, mode string) ([]string, bool) {
	spec, ok := specForKind(k)
	if !ok {
		return nil, false
	}
	return spec.permFlags(mode)
}

// AuthHintFor returns the per-agent login remediation for a kind's needs-auth state
// (what to run in a terminal), with a neutral fallback for unknown kinds or tools
// without a specific hint. Clients embed it in their needs-auth messaging.
func AuthHintFor(k Kind) string {
	if spec, ok := specForKind(k); ok && spec.AuthHint != "" {
		return spec.AuthHint
	}
	return "log in to the agent in a terminal"
}

// innerCommandClaude builds the tmux command for the claude kinds. interactiveShell
// wraps it in `bash -ic` so a login rc-file loads PATH (nvm/asdf) before claude is
// exec'd (native machines); sheds bake claude into the system path. See the exported
// InnerCommand doc for the permission-mode / --remote-control form rationale. port is
// opencode-only (§ InnerCommand doc) and always ignored here.
func innerCommandClaude(kind Kind, displayName, permissionMode string, interactiveShell bool, _ int) string {
	flags, _ := permFlagsFor(kind, permissionMode) // validity pre-checked in Create
	var cmd string
	switch kind {
	case KindClaudeBroker:
		cmd = "claude remote-control --name " + shellQuote(displayName)
		if len(flags) > 0 {
			cmd += " " + strings.Join(flags, " ")
		}
		cmd += " --spawn same-dir"
	case KindClaudeRC:
		if len(flags) > 0 {
			// A posture is delivered via the --remote-control flag form (the bare `/rc`
			// slash command takes no flags); with no posture, keep the original `/rc`
			// form for backward compatibility.
			cmd = "claude --remote-control --name " + shellQuote(displayName) + " " + strings.Join(flags, " ")
		} else {
			cmd = "claude --name " + shellQuote(displayName) + " /rc"
		}
	default:
		return "bash -l"
	}
	if interactiveShell {
		return "bash -ic " + shellQuote(cmd)
	}
	return cmd
}

// innerCommandTUI builds a plain-TUI launcher for an agent whose kind is the bare
// tool (codex/opencode/cursor): `<bin> [posture flags…]`, optionally wrapped in
// `bash -ic` so a login rc-file loads PATH before the tool is exec'd (native
// machines; sheds bake the tools into the system path). The display name is metadata
// only — these TUIs take no --name.
//
// port is opencode's allocated loopback SSE/HTTP server port (0 = none / not
// opencode; see freeLoopbackPort, ops.go). When kind is KindOpencode and port != 0,
// `--port <port> --hostname 127.0.0.1` is appended to cmd BEFORE the optional
// `bash -ic` wrap below — WRAP-ORDER MATTERS: appending after the wrap would place
// `--port …` as a second argv token handed to bash itself, not inside the quoted
// string bash execs as opencode's command line, so opencode would never see the flag
// (and bash would likely reject the stray tokens). Building it into cmd first means
// it rides inside the `bash -ic '<cmd>'` quoting like every other flag. codex/cursor
// (and opencode with port == 0) never hit this branch, so a nonzero port passed for a
// non-opencode kind is silently a no-op — only opencode consumes it.
// baseFlags are emitted immediately after bin, BEFORE the permission flags —
// a fixed spec-owned posture (cursor's --trust), not caller input. The order is
// wire-visible in the tmux inner command, so the Rust port (rc_agents.rs)
// mirrors it exactly and the rc-parity argv transcripts pin it.
func innerCommandTUI(bin string, baseFlags ...string) func(kind Kind, displayName, permissionMode string, interactiveShell bool, port int) string {
	return func(kind Kind, _, permissionMode string, interactiveShell bool, port int) string {
		flags, _ := permFlagsFor(kind, permissionMode)
		cmd := bin
		if len(baseFlags) > 0 {
			cmd += " " + strings.Join(baseFlags, " ")
		}
		if len(flags) > 0 {
			cmd += " " + strings.Join(flags, " ")
		}
		if kind == KindOpencode && port != 0 {
			cmd += " --port " + strconv.Itoa(port) + " --hostname 127.0.0.1"
		}
		if interactiveShell {
			return "bash -ic " + shellQuote(cmd)
		}
		return cmd
	}
}

// innerCommandShell runs a plain login bash; it ignores permissionMode, the
// interactive-shell wrap (a shell is already a shell), and port (opencode-only).
func innerCommandShell(_ Kind, _, _ string, _ bool, _ int) string {
	return "bash -l"
}

var (
	brokerURLRe = regexp.MustCompile(`https?://claude\.ai/code\?environment=env_[A-Za-z0-9_-]+`)
	replURLRe   = regexp.MustCompile(`https?://claude\.ai/code/session_[A-Za-z0-9_-]+`)

	needsAuthRe    = regexp.MustCompile(`(?i)requires a claude\.ai subscription|not logged in|claude auth login`)
	reconnectingRe = regexp.MustCompile(`\bReconnecting\b`)
	connectedRe    = regexp.MustCompile(`\bConnected\b`)
	rcConnectingRe = regexp.MustCompile(`(?i)Remote Control connecting`)
	rcActiveRe     = regexp.MustCompile(`(?i)Remote Control active`)
)

// extractURL pulls the claude.ai remote-control URL for a claude kind (broker vs
// rc use different URL shapes); "" for kinds with no URL.
func extractURL(kind Kind, pane string) string {
	switch kind {
	case KindClaudeBroker:
		return brokerURLRe.FindString(pane)
	case KindClaudeRC:
		return replURLRe.FindString(pane)
	default:
		return ""
	}
}

// classifyClaude derives (state, url) from a captured claude pane. The trust/auth
// gates precede the per-kind ready logic (they can appear for either claude kind).
func classifyClaude(kind Kind, pane string) PaneResult {
	if IsTrustPrompt(pane) {
		return PaneResult{State: StateNeedsTrust, URL: extractURL(kind, pane)}
	}
	if needsAuthRe.MatchString(pane) {
		return PaneResult{State: StateNeedsAuth, URL: extractURL(kind, pane)}
	}
	switch kind {
	case KindClaudeBroker:
		url := extractURL(KindClaudeBroker, pane)
		if reconnectingRe.MatchString(pane) {
			return PaneResult{State: StateReconnecting, URL: url}
		}
		if connectedRe.MatchString(pane) && url != "" {
			return PaneResult{State: StateReady, URL: url}
		}
		if url != "" {
			return PaneResult{State: StateReady, URL: url}
		}
		return PaneResult{State: StateStarting}
	case KindClaudeRC:
		url := extractURL(KindClaudeRC, pane)
		if rcConnectingRe.MatchString(pane) && url == "" {
			return PaneResult{State: StateStarting}
		}
		if rcActiveRe.MatchString(pane) && url != "" {
			return PaneResult{State: StateReady, URL: url}
		}
		if url != "" {
			return PaneResult{State: StateReady, URL: url}
		}
		return PaneResult{State: StateStarting}
	default:
		return PaneResult{State: StateStarting}
	}
}

// codexComposerPlaceholder is the regex source for codex's empty-composer
// placeholder line ("› Find and fix a bug in @filename",
// testdata/panes/codex-ready.txt line 40). Hoisted so codexReadyRe (banner OR
// composer ⇒ ready) and codexPromptAnchorRe (composer only ⇒ needs_input anchor)
// share one literal and can't silently drift apart if codex rewords the hint.
const codexComposerPlaceholder = `Find and fix a bug in @filename`

var (
	// codex: the composer banner means codex is usable — it wins even over the MCP
	// token_expired warning below (that warning is an MCP-app sub-service failure, not
	// core auth, and appears inline on the working ready screen). Composed as the
	// version banner OR the shared composer placeholder.
	codexReadyRe = regexp.MustCompile(`>_ OpenAI Codex \(v` + `|` + codexComposerPlaceholder)
	codexTrustRe = regexp.MustCompile(`(?i)Do you trust the contents of this directory\?`)
	// codex core-auth failure / not-signed-in (only meaningful when the composer is
	// absent). A fresh, never-authed codex shows the "Sign in with ChatGPT" onboarding
	// picker (not a token_expired warning), so anchor on both: the sign-in picker for a
	// logged-out session and the expired-token text for a stale one.
	codexAuthRe = regexp.MustCompile(`Provided authentication token is expired|token_expired|Sign in with ChatGPT`)
)

// classifyCodex derives a codex pane's lifecycle state. url/id stay empty (codex has
// no remote-control URL). The dead signal is handled by ClassifyPane's shared
// shed-guest check before this runs.
func classifyCodex(_ Kind, pane string) PaneResult {
	if codexReadyRe.MatchString(pane) {
		return PaneResult{State: StateReady}
	}
	if codexTrustRe.MatchString(pane) {
		return PaneResult{State: StateNeedsTrust}
	}
	if codexAuthRe.MatchString(pane) {
		return PaneResult{State: StateNeedsAuth}
	}
	return PaneResult{State: StateStarting}
}

var (
	// opencodePlaceholderRe is the composer placeholder of the fresh,
	// pre-first-prompt screen — an unconditional ready signal. It disappears once a
	// conversation starts (which used to wobble a live session back to "starting"
	// after its first prompt).
	opencodePlaceholderRe = regexp.MustCompile(`Ask anything\.\.\.`)
	// opencodeFooterRe is the `ctrl+p commands` footer hint, drawn for the whole
	// life of the TUI (pre- and post-prompt) — the stable ready anchor once the
	// placeholder is gone.
	opencodeFooterRe = regexp.MustCompile(`ctrl\+p commands`)
	// opencodeAuthScreenRe guards the footer-only ready path. opencode's
	// logged-out/onboarding screen has never been captured live (a real fixture is
	// still wanted) — if it renders the same footer chrome, a footer-only match
	// would classify it ready and `create --wait` would deliver a prompt into a
	// login screen. Until a live fixture pins the exact text, a footer-only pane
	// that smells like an auth screen stays "starting" (conservative: a recheck
	// self-corrects, a wrong ready ships a prompt into the void). Kept narrow —
	// word-bounded phrases, not bare "login" — to avoid tripping on ordinary agent
	// chatter in a conversation.
	opencodeAuthScreenRe = regexp.MustCompile(`(?i)\bsign in\b|\blog ?in to\b|\bauthenticate\b|\bopencode auth\b`)

	// opencodeAuthDialogRe matches opencode's auto-opened "Connect a provider" dialog
	// — the widget opencode's TUI pops up UNPROMPTED the instant its sync effect sees
	// `sync.data.provider.length === 0` (packages/tui/src/app.tsx: "only trigger when
	// we transition into an empty-provider state"), which is exactly the state of a
	// freshly-baked shed image before `opencode auth login` has ever run — the "reads
	// starting → timeout" gap this anchor closes.
	//
	// SOURCE, not guesswork — read out of the opencode monorepo's TUI package
	// (packages/tui/src, commit 4643e65ad63 as checked out locally for this change):
	//
	//	component/dialog-provider.tsx — `<DialogSelect title="Connect a provider"
	//	                                 options={options()} />`; PROVIDER_PRIORITY
	//	                                 groups every built-in provider (opencode,
	//	                                 openai, anthropic, google, github-copilot,
	//	                                 opencode-go) under the category "Popular".
	//	ui/dialog-select.tsx          — the title renders bold, left-column, with
	//	                                 "esc" trailing on the SAME row (so the title
	//	                                 is not itself end-of-line-anchorable); each
	//	                                 category header renders BOLD, ALONE, on its
	//	                                 own line, a few rows below.
	//	app.tsx                       — `dialog.replace(() => <DialogProviderList />)`
	//	                                 gated on the empty-provider transition.
	//	ui/dialog.tsx                 — the Dialog wrapper is a full-screen
	//	                                 `position="absolute"` overlay (zIndex 3000),
	//	                                 so — like codex's approval overlay — it
	//	                                 REPLACES whatever the composer/home screen
	//	                                 would otherwise draw; opencodePlaceholderRe /
	//	                                 opencodeFooterRe never co-render with it on a
	//	                                 real pane.
	//
	// THE CONJUNCTION (same discipline as codex/cursor's approval anchors, § their
	// docs): the headline alone is not trusted — "Connect a provider" is ordinary
	// English an agent could in principle quote back — so the anchor additionally
	// requires the "Popular" category header, which appears alone on its own line a
	// few rows below the title and exists only while this exact widget is mounted
	// (see TestClassifyFalsePositives for the negative: the headline alone, without
	// the category header, must not classify as needs-auth).
	//
	// FIXTURE PROVENANCE: testdata/panes/opencode-needs-auth.txt is reconstructed
	// from the source above, NOT captured from a live pane (no fixture-capture shed
	// was spun up for this change) — same provenance discipline as cursor's approval
	// fixtures. Re-capture live when convenient.
	opencodeAuthDialogRe = regexp.MustCompile(
		`(?m)^[ \t]*Connect a provider\b` +
			`(?:[^\n]*\n){0,10}` +
			`[ \t]*Popular[ \t]*$`)
)

// classifyOpencode derives an opencode pane's lifecycle state. The auto-opened
// "Connect a provider" dialog (opencodeAuthDialogRe) is checked first — it is opencode's
// only captured POSITIVE needs-auth signal (a zero-provider session), and it fully
// replaces the composer/footer on screen, so it never races the ready checks below. The
// composer placeholder is unconditional ready; the persistent footer alone means ready
// only when the pane does not look like an auth/onboarding screen (opencodeAuthScreenRe,
// the pre-existing guard kept for any onboarding text this anchor doesn't cover).
func classifyOpencode(_ Kind, pane string) PaneResult {
	if opencodeAuthDialogRe.MatchString(pane) {
		return PaneResult{State: StateNeedsAuth}
	}
	if opencodePlaceholderRe.MatchString(pane) {
		return PaneResult{State: StateReady}
	}
	if opencodeFooterRe.MatchString(pane) && !opencodeAuthScreenRe.MatchString(pane) {
		return PaneResult{State: StateReady}
	}
	return PaneResult{State: StateStarting}
}

// cursorAuthRe anchors on cursor's two-stage login flow (splash "Press any key to log
// in", the browser/device link screen, and the post-exit "Authentication required"
// notice).
var cursorAuthRe = regexp.MustCompile(`(?i)Press any key to log in\.\.\.|Authentication required to use Cursor Agent|click this link to log in`)

// cursorReadyRe anchors on the authed composer's placeholder, line-anchored with its
// arrow prefix so the phrase quoted inside agent output can't read as ready. cursor
// swaps the placeholder text after the first exchange: a fresh composer shows
// "→ Plan, search, build anything" (testdata/panes/cursor-ready.txt), a
// mid-conversation composer shows "→ Add a follow-up"
// (testdata/panes/cursor-ready-active.txt) — both are the same "ready / waiting for
// input" chrome, so both must classify ready (a live capture proved the follow-up
// form dropped the session back to starting). Also reused as cursor's stability
// PromptAnchor, so needs_input fires at either composer.
var cursorReadyRe = regexp.MustCompile(`(?m)^\s*→ (?:Plan, search, build anything|Add a follow-up)\s*$`)

// classifyCursor derives a cursor-agent pane's lifecycle state. The auth screens and
// the authed composer are disjoint, so auth is checked first and ready second;
// anything else (booting, mid-conversation) reads as starting.
func classifyCursor(_ Kind, pane string) PaneResult {
	if cursorAuthRe.MatchString(pane) {
		return PaneResult{State: StateNeedsAuth}
	}
	if cursorReadyRe.MatchString(pane) {
		return PaneResult{State: StateReady}
	}
	return PaneResult{State: StateStarting}
}

// classifyShell reports ready as soon as the pane has drawn anything (a prompt),
// starting while still blank. A shell has no trust/auth/url states, and its shed
// prompt is the ready signal — never a death (so ClassifyPane skips the shared
// dead check for shell).
func classifyShell(_ Kind, pane string) PaneResult {
	if strings.TrimSpace(pane) != "" {
		return PaneResult{State: StateReady}
	}
	return PaneResult{State: StateStarting}
}

// shedShellPromptRe matches a line that IS a bare shed guest login-shell prompt
// "[shed:<name>] <cwd> $" and nothing else (fully anchored; applied to the trimmed
// line). When the LAST non-empty line of an agent pane is a bare prompt, the agent
// has exited back to the shell — the shed-guest secondary dead signal (the primary is
// capture failure). The anchoring matters twice over: a launch-line command echo
// ("[shed:x] ~ $ codex") has text after the $, and a running agent merely PRINTING a
// prompt-shaped line ("[shed:x] ~ $ make test") must not read as a death.
var shedShellPromptRe = regexp.MustCompile(`^\[shed:[^\]]+\][^$]*\$$`)

// exitedToShell reports whether the pane's last non-empty line is a bare shed shell
// prompt (the agent process returned to the login shell). Empty panes are NOT dead —
// a blank pane is a just-started/ambiguous session; real death of a whole-session
// exit surfaces as a capture failure at the ops layer.
func exitedToShell(pane string) bool {
	lines := strings.Split(pane, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		return shedShellPromptRe.MatchString(line)
	}
	return false
}
