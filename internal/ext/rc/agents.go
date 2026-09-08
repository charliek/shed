package rc

import (
	"regexp"
	"strconv"
	"strings"
)

// AgentSpec is the single per-tool table entry that gathers everything
// agent-specific behind one lookup: how a kind's inner tmux command is built, its
// optional trust/onboarding pre-seed, and the permission modes it accepts. One spec
// backs one or more Kinds (claude backs both claude-broker and claude-rc). This is
// the seam every agent (claude/codex/opencode/cursor/shell) slots into without
// touching the ops core.
//
// It carries NO pane classifiers or anchors: S2 (charliek/shed#324) retired pane
// scraping as a status source — a shed row's `state` is liveness, and roost is the
// status authority for machine rows.
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
	// Preseed prepares on-disk tool config before the session launches: claude's trust +
	// onboarding gates (so a fresh session reaches ready unattended). nil when the tool
	// needs none, or when its trust gate is auto-accepted from the pane instead (codex).
	// cursor's hook-relay preseed was removed with A6 (charliek/shed#322) along with the
	// ingest lane it fed.
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
		Lane:         LaneTUI,
		InnerCommand: innerCommandTUI("codex"),
		// codex's directory-trust gate is a pre-selected "Yes, continue" prompt, so it
		// is auto-accepted from the pane by waitUntilLive's CONTROL matcher
		// (IsCodexTrustPrompt, rc.go) rather than config-preseeded.
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
		AuthHint: "run `codex` and complete login (`codex login`)",
	},
	{
		Tool:         toolOpencode,
		Bin:          "opencode",
		Kinds:        []Kind{KindOpencode},
		Lane:         LaneTUI,
		InnerCommand: innerCommandTUI("opencode"),
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
		// cursor has no trust/onboarding gate to preseed (--trust above covers the one
		// dialog it draws); the hook relay that used to live here went with A6
		// (charliek/shed#322).
		Preseed: nil,
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
		Lane:         LaneTUI,
		InnerCommand: innerCommandShell,
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
