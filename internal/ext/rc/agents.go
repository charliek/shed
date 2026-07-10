package rc

import (
	"regexp"
	"strings"
)

// AgentSpec is the single per-tool table entry that gathers everything
// agent-specific behind one lookup: how a kind's inner tmux command is built, how
// its pane is classified into a lifecycle state, its optional trust/onboarding
// pre-seed, and the permission modes it accepts. One spec backs one or more Kinds
// (claude backs both claude-broker and claude-rc). This is the seam that future
// codex/opencode/cursor kinds slot into without touching the ops/classify core.
type AgentSpec struct {
	// Tool is the underlying CLI's stable name ("claude", "shell").
	Tool string
	// Kinds are the session kinds this tool provides. Disjoint across specs.
	Kinds []Kind
	// InnerCommand builds the command the tmux session runs for one of the tool's
	// kinds. Signature mirrors the exported InnerCommand: display name (already
	// resolved by the caller), claude's --permission-mode ("" = omit), and whether
	// to wrap in `bash -ic` (native machines).
	InnerCommand func(kind Kind, displayName, permissionMode string, interactiveShell bool) string
	// Classify derives a pane's lifecycle state (+ optional url/id) for one of the
	// tool's kinds. Takes the kind because a tool can back several kinds with
	// different ready/url regex sets (claude-broker vs claude-rc).
	Classify func(kind Kind, pane string) PaneResult
	// Preseed prepares on-disk tool config so a fresh session reaches ready
	// unattended (claude: trust + onboarding). nil when the tool needs none (shell).
	Preseed func(workdir string, getenv func(string) string) error
	// ValidModes is the tool's accepted permission-mode value set (claude's
	// --permission-mode values). Empty for tools with no permission posture.
	ValidModes []string
}

// PaneResult is a Classify outcome: the derived lifecycle state plus the optional
// remote-control url and session id. url/id are claude-remote-control-specific and
// stay empty for other tools (the DTO omits empties).
type PaneResult struct {
	State State
	URL   string
	ID    string
}

// validMode reports whether m is one of the tool's accepted permission modes.
func (s *AgentSpec) validMode(m string) bool {
	for _, v := range s.ValidModes {
		if v == m {
			return true
		}
	}
	return false
}

// Tool-name tokens (AgentSpec.Tool).
const (
	toolClaude = "claude"
	toolShell  = "shell"
)

// agentRegistry is the canonical per-tool table. Each spec's Kinds are disjoint,
// and together they cover every IsValidKind kind (asserted by agents_test.go).
var agentRegistry = []*AgentSpec{
	{
		Tool:         toolClaude,
		Kinds:        []Kind{KindClaudeBroker, KindClaudeRC},
		InnerCommand: innerCommandClaude,
		Classify:     classifyClaude,
		Preseed:      PreseedClaudeConfig,
		// claude's --permission-mode value set. An empty mode means "don't pass the
		// flag" (claude's own default) and is intentionally NOT a member here.
		ValidModes: []string{
			"default",
			"acceptEdits",
			"plan",
			"auto",
			"dontAsk",
			"bypassPermissions",
		},
	},
	{
		Tool:         toolShell,
		Kinds:        []Kind{KindShell},
		InnerCommand: innerCommandShell,
		Classify:     classifyShell,
		Preseed:      nil,
		ValidModes:   nil,
	},
}

// kindToSpec indexes the registry by kind for O(1) lookup.
var kindToSpec = func() map[Kind]*AgentSpec {
	m := make(map[Kind]*AgentSpec, len(agentRegistry)*2)
	for _, s := range agentRegistry {
		for _, k := range s.Kinds {
			m[k] = s
		}
	}
	return m
}()

// specForKind returns the AgentSpec backing a kind, ok=false for an unregistered
// (invalid) kind. Every IsValidKind kind resolves.
func specForKind(k Kind) (*AgentSpec, bool) {
	s, ok := kindToSpec[k]
	return s, ok
}

// innerCommandClaude builds the tmux command for the claude kinds. interactiveShell
// wraps it in `bash -ic` so a login rc-file loads PATH (nvm/asdf) before claude is
// exec'd (native machines); sheds bake claude into the system path. See the exported
// InnerCommand doc for the permission-mode / --remote-control form rationale.
func innerCommandClaude(kind Kind, displayName, permissionMode string, interactiveShell bool) string {
	var cmd string
	switch kind {
	case KindClaudeBroker:
		cmd = "claude remote-control --name " + shellQuote(displayName)
		if permissionMode != "" {
			cmd += " --permission-mode " + permissionMode
		}
		cmd += " --spawn same-dir"
	case KindClaudeRC:
		if permissionMode != "" {
			cmd = "claude --remote-control --name " + shellQuote(displayName) +
				" --permission-mode " + permissionMode
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

// classifyShell reports ready as soon as the pane has drawn anything (a prompt),
// starting while still blank. A shell has no trust/auth/url states.
func classifyShell(_ Kind, pane string) PaneResult {
	if strings.TrimSpace(pane) != "" {
		return PaneResult{State: StateReady}
	}
	return PaneResult{State: StateStarting}
}
