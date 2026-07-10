package rc

import (
	"regexp"
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
	// InnerCommand builds the command the tmux session runs for one of the tool's
	// kinds. Signature mirrors the exported InnerCommand: display name (already
	// resolved by the caller), the generic/claude permission mode ("" = omit), and
	// whether to wrap in `bash -ic` (native machines / non-shed PATH).
	InnerCommand func(kind Kind, displayName, permissionMode string, interactiveShell bool) string
	// Classify derives a pane's lifecycle state (+ optional url/id) for one of the
	// tool's kinds. Takes the kind because a tool can back several kinds with
	// different ready/url regex sets (claude-broker vs claude-rc). The shared
	// shed-guest "exited to shell" dead signal is applied by ClassifyPane before this
	// runs, so a spec's Classify only handles its live states.
	Classify func(kind Kind, pane string) PaneResult
	// Preseed prepares on-disk tool config so a fresh session reaches ready
	// unattended (claude: trust + onboarding). nil when the tool needs none, or when
	// its trust gate is auto-accepted from the pane instead (codex).
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

// Tool-name tokens (AgentSpec.Tool).
const (
	toolClaude   = "claude"
	toolCodex    = "codex"
	toolOpencode = "opencode"
	toolCursor   = "cursor"
	toolShell    = "shell"
)

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
		ExtraModes: []string{"acceptEdits", "plan", "dontAsk", PermissionModeBypass},
		AuthHint:   "run `claude` \u2192 /login",
	},
	{
		Tool:         toolCodex,
		Bin:          "codex",
		Kinds:        []Kind{KindCodex},
		InnerCommand: innerCommandTUI("codex"),
		Classify:     classifyCodex,
		// codex's directory-trust gate is a pre-selected "Yes, continue" prompt, so it
		// is auto-accepted from the pane in waitUntilReady rather than config-preseeded.
		Preseed: nil,
		PermMap: map[string][]string{
			PermModeDefault: nil,
			PermModeAuto:    {"--full-auto"},
			PermModeSkip:    {"--dangerously-bypass-approvals-and-sandbox"},
		},
		AuthHint: "run `codex` and complete login (`codex login`)",
	},
	{
		Tool:         toolOpencode,
		Bin:          "opencode",
		Kinds:        []Kind{KindOpencode},
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
		AuthHint: "run `opencode auth login`",
	},
	{
		Tool:         toolCursor,
		Bin:          "cursor-agent",
		Kinds:        []Kind{KindCursor},
		InnerCommand: innerCommandTUI("cursor-agent"),
		Classify:     classifyCursor,
		Preseed:      nil,
		PermMap: map[string][]string{
			PermModeDefault: nil,
			// cursor has no mid-tier posture; auto stays default until one exists.
			PermModeAuto: nil,
			PermModeSkip: {"--force"},
		},
		AuthHint: "run `cursor-agent login`",
	},
	{
		Tool:         toolShell,
		Bin:          "",
		Kinds:        []Kind{KindShell},
		InnerCommand: innerCommandShell,
		Classify:     classifyShell,
		Preseed:      nil,
		// A shell has no permission posture; the generic modes are accepted (valid for
		// ALL kinds) but produce no flags.
		PermMap: noPostureMap,
	},
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
// InnerCommand doc for the permission-mode / --remote-control form rationale.
func innerCommandClaude(kind Kind, displayName, permissionMode string, interactiveShell bool) string {
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
func innerCommandTUI(bin string) func(kind Kind, displayName, permissionMode string, interactiveShell bool) string {
	return func(kind Kind, _, permissionMode string, interactiveShell bool) string {
		flags, _ := permFlagsFor(kind, permissionMode)
		cmd := bin
		if len(flags) > 0 {
			cmd += " " + strings.Join(flags, " ")
		}
		if interactiveShell {
			return "bash -ic " + shellQuote(cmd)
		}
		return cmd
	}
}

// innerCommandShell runs a plain login bash; it ignores permissionMode and the
// interactive-shell wrap (a shell is already a shell).
func innerCommandShell(_ Kind, _, _ string, _ bool) string {
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

var (
	// codex: the composer banner means codex is usable — it wins even over the MCP
	// token_expired warning below (that warning is an MCP-app sub-service failure, not
	// core auth, and appears inline on the working ready screen).
	codexReadyRe = regexp.MustCompile(`>_ OpenAI Codex \(v|Find and fix a bug in @filename`)
	codexTrustRe = regexp.MustCompile(`(?i)Do you trust the contents of this directory\?`)
	// codex core-auth failure (only meaningful when the composer is absent).
	codexAuthRe = regexp.MustCompile(`Provided authentication token is expired|token_expired`)
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

// opencodeReadyRe anchors on the composer placeholder of the ready screen.
var opencodeReadyRe = regexp.MustCompile(`Ask anything\.\.\.`)

// classifyOpencode derives an opencode pane's lifecycle state. opencode has no trust
// gate; its logged-out screen was not captured live, so needs-auth is left to the
// shared signals for now (a future anchor slots in here).
func classifyOpencode(_ Kind, pane string) PaneResult {
	if opencodeReadyRe.MatchString(pane) {
		return PaneResult{State: StateReady}
	}
	return PaneResult{State: StateStarting}
}

// cursorAuthRe anchors on cursor's two-stage login flow (splash "Press any key to log
// in", the browser/device link screen, and the post-exit "Authentication required"
// notice).
var cursorAuthRe = regexp.MustCompile(`(?i)Press any key to log in\.\.\.|Authentication required to use Cursor Agent|click this link to log in`)

// classifyCursor derives a cursor-agent pane's lifecycle state. Its authed/ready
// screen needs an interactive login and was not captured live, so a logged-in cursor
// currently reads as starting (tracked as an authed-fixture follow-up); needs-auth is
// the definitively-classified state.
func classifyCursor(_ Kind, pane string) PaneResult {
	if cursorAuthRe.MatchString(pane) {
		return PaneResult{State: StateNeedsAuth}
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
