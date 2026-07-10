package rc

import (
	"regexp"
	"strings"
)

// CapabilityVersion is the capabilities schema/protocol version advertised by
// `shed-ext-rc capabilities` and the `list` envelope. It is deliberately decoupled
// from SchemaVersion (SHED_RC_V, the on-session tmux-env metadata schema, which stays
// 2 — session metadata is unchanged): a client learns what a shed's binary can do
// from CapabilityVersion + the feature list, not from the metadata schema.
const CapabilityVersion = 3

// capabilityFeatures is the feature list (a set of stable tokens) advertised to
// clients for capability discovery, replacing error-string sniffing. It advertises
// ONLY what this binary actually supports — a feature token is appended in the same
// change that ships the feature (planned next: "plan-stdin" and "prompt-b64" with the
// plan-delivery verbs, "serve" with the rc hub):
//   - generic-perm — the generic default|auto|skip permission tri-state for all kinds.
var capabilityFeatures = []string{"generic-perm"}

// AgentInfo is one agent's install probe result under capabilities.agents. Version is
// omitted when the agent is not installed (or its version could not be read).
type AgentInfo struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
}

// KindFeatures describes what a client can do with a kind's sessions. v1 agents are
// TUI-only, so approvals happen in the terminal ("tui"); post_input reports whether a
// typed line can be delivered to the pane. (watch/live-feed features arrive with the
// rc hub in a later phase.)
type KindFeatures struct {
	PostInput bool   `json:"post_input"`
	Approvals string `json:"approvals"`
}

// Capabilities is the `shed-ext-rc capabilities` payload and the block embedded in the
// `list` envelope. It tells a client which kinds a shed offers, which agents are
// actually installed (and at what version), the feature set, and per-kind UI hints.
type Capabilities struct {
	RCVersion    int                   `json:"rc_version"`
	Kinds        []Kind                `json:"kinds"`
	Agents       map[string]AgentInfo  `json:"agents"`
	Features     []string              `json:"features"`
	KindFeatures map[Kind]KindFeatures `json:"kind_features"`
}

// AgentProbe reports an agent binary's installed state + version. Injected so the
// probe (which shells out to `command -v` + `--version`) is testable.
type AgentProbe func(bin string) AgentInfo

// BuildCapabilities assembles the capabilities payload, probing each registered agent
// binary through probe (shell has no binary and is skipped in agents{}). The kinds
// list and kind_features are registry-derived so they stay in lockstep with the
// specs.
func BuildCapabilities(probe AgentProbe) Capabilities {
	agents := map[string]AgentInfo{}
	for _, spec := range agentRegistry {
		if spec.Bin == "" {
			continue // shell: nothing to probe
		}
		if _, seen := agents[spec.Tool]; seen {
			continue
		}
		agents[spec.Tool] = probe(spec.Bin)
	}
	return Capabilities{
		RCVersion:    CapabilityVersion,
		Kinds:        append([]Kind(nil), allKinds...),
		Agents:       agents,
		Features:     append([]string(nil), capabilityFeatures...),
		KindFeatures: kindFeatures(),
	}
}

// kindFeatures returns the per-kind UI hints for the agent kinds that accept a typed
// kickoff and drive approvals through the TUI. claude-broker (driven from claude.ai,
// not the pane) and shell (no agent approval surface) are omitted.
func kindFeatures() map[Kind]KindFeatures {
	out := map[Kind]KindFeatures{}
	for _, k := range allKinds {
		switch k {
		case KindClaudeBroker, KindShell:
			continue
		default:
			out[k] = KindFeatures{PostInput: AcceptsTypedInput(k), Approvals: "tui"}
		}
	}
	return out
}

// versionRe extracts a version substring from an agent's `--version` output, tolerant
// of a leading "v" and a trailing build suffix. Matches "2.1.196 (Claude Code)",
// "codex-cli 0.142.4", "1.17.11", and "v2026.07.09-a3815c0".
var versionRe = regexp.MustCompile(`v?(\d+\.\d+(?:\.\d+)?[\w.\-]*)`)

// ParseAgentVersion pulls a clean version string out of raw `--version` output, or
// falls back to the trimmed first line when no version-shaped token is present.
func ParseAgentVersion(out string) string {
	out = strings.TrimSpace(out)
	if m := versionRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[:i]
	}
	return strings.TrimSpace(out)
}
