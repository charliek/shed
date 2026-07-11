package rc

import (
	"regexp"
	"strings"
	"time"
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
// change that ships the feature:
//   - generic-perm — the generic default|auto|skip permission tri-state for all kinds.
//   - plan-stdin — `create --plan-stdin` writes a plan to a per-kind HOME-rooted file
//     and composes+delivers a kickoff referencing it.
//   - prompt-b64 — `create --plan-stdin --prompt-b64 <base64>` prepends decoded caller
//     framing to the composed plan kickoff (stdin stays reserved for the plan).
//   - serve — `shed-ext-rc serve` runs the resident rc activity hub (loopback HTTP:
//     GET /v1/sessions + SSE /v1/events), spawned on demand and self-exiting.
//   - activity — sessions carry the live activity dimension (activity/activity_at/
//     last_message inside the rc block) derived by the hub. (The message-feed token
//     "messages" is withheld until the /messages + /input endpoints ship.)
var capabilityFeatures = []string{"generic-perm", "plan-stdin", "prompt-b64", "serve", "activity"}

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

// InstalledProbe is the fast install-only check (`command -v` in a login shell —
// normally milliseconds) used to degrade an agent whose full probe outruns
// probeBudget: installed state is still reported, version is omitted. It runs
// CONCURRENTLY with the full probe inside the same budgeted flight — never
// synchronously after expiry — so even a slow login shell cannot push assembly
// past the budget; if it too is still pending at the budget, the agent degrades
// to installed:false. nil skips the fallback entirely.
type InstalledProbe func(bin string) bool

// probeBudget is the total wall-clock budget for ALL agent probes during
// capabilities assembly. The full probe runs `<bin> --version`, which some agent
// CLIs take seconds to answer; the `list` envelope embeds capabilities and is
// consumed on the server's session-listing hot path under a ~2s exec timeout, so
// probing must never come close to that. Probes run concurrently; any still
// pending at the budget degrade to the fast installed-only result.
const probeBudget = 750 * time.Millisecond

// BuildCapabilities assembles the capabilities payload, probing each registered
// agent binary through probe (shell has no binary and is skipped in agents{}).
// Probes run concurrently under a shared probeBudget; a laggard agent reports
// installed (via the fast installed check) with version omitted — Version is
// omitempty, so the wire shape is unchanged — and one slow `--version` can never
// stall `list`/`capabilities`. The kinds list and kind_features are
// registry-derived so they stay in lockstep with the specs.
func BuildCapabilities(probe AgentProbe, installed InstalledProbe) Capabilities {
	type probeSlot struct {
		tool, bin string
		done      chan AgentInfo
		instDone  chan bool
	}
	var slots []probeSlot
	seen := map[string]bool{}
	for _, spec := range agentRegistry {
		if spec.Bin == "" || seen[spec.Tool] {
			continue // shell: nothing to probe; one probe per tool
		}
		seen[spec.Tool] = true
		s := probeSlot{tool: spec.Tool, bin: spec.Bin, done: make(chan AgentInfo, 1)}
		if installed != nil {
			s.instDone = make(chan bool, 1)
		}
		slots = append(slots, s)
		// Buffered channels: a laggard probe finishing after the budget just
		// parks its result and the goroutine exits — no leak. The fast
		// installed-only check launches alongside the full probe so its result
		// is (almost always) already waiting if the full probe misses the
		// budget — the fallback never runs synchronously after expiry.
		go func() { s.done <- probe(s.bin) }()
		if s.instDone != nil {
			go func() { s.instDone <- installed(s.bin) }()
		}
	}

	deadline := time.NewTimer(probeBudget)
	defer deadline.Stop()
	expired := false
	agents := map[string]AgentInfo{}
	for _, s := range slots {
		if !expired {
			select {
			case info := <-s.done:
				agents[s.tool] = info
				continue
			case <-deadline.C:
				expired = true
			}
		}
		// Budget exhausted: take a completed full result if it raced in,
		// otherwise the already-flying installed-only result. Both reads are
		// non-blocking — if even the fast check hasn't finished (a hung login
		// shell), the agent degrades to installed:false rather than stalling
		// past the budget.
		select {
		case info := <-s.done:
			agents[s.tool] = info
		default:
			var info AgentInfo
			if s.instDone != nil {
				select {
				case ok := <-s.instDone:
					info.Installed = ok
				default:
				}
			}
			agents[s.tool] = info
		}
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
