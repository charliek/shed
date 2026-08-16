package rc

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// CapabilityVersion is the capabilities schema/protocol version advertised by
// `shed-ext-rc capabilities` and the `list` envelope. It is deliberately decoupled
// from SchemaVersion (SHED_RC_V, the on-session tmux-env metadata schema, which stays
// 2 — session metadata is unchanged): a client learns what a shed's binary can do
// from CapabilityVersion + the feature list, not from the metadata schema.
const CapabilityVersion = 4

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
//     last_message inside the rc block) derived by the hub.
//   - messages — the hub serves the codex message feed (GET /v1/sessions/{slug}/
//     messages + the message.appended SSE event) and gated feed input (POST
//     /v1/sessions/{slug}/input). Per-kind availability is in kind_features
//     (watch / input); this token says the endpoints exist on this binary.
//   - contract-v2 — the v2 wire contract: `lane` on every session DTO, the
//     feed/interrupt/attach hints in kind_features, the turn/interrupt/approvals hub
//     verbs (routed and fully specified — live for a kind whose kind_features row
//     advertises them, 409 not_supported elsewhere; see hub_verbs.go), the
//     `approval_request` feed row with its
//     approval block, and `pending_approvals` on the session. This token is the
//     client's ROUTE-EXISTENCE check: a server without it may 404 the new verbs at
//     the mux, so a client reads the token instead of interpreting a bare 404.
//     Advertised in the same change that shipped the routes — never ahead of them.
var capabilityFeatures = []string{"generic-perm", "plan-stdin", "prompt-b64", "serve", "activity", "messages", "contract-v2"}

// AgentInfo is one agent's install probe result under capabilities.agents. Version is
// omitted when the agent is not installed (or its version could not be read).
type AgentInfo struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
}

// KindFeatures describes what a client can do with a kind's sessions — the whole
// point being that a client (notably mobile) renders watch/steer/approve affordances
// from capabilities alone, without a per-kind table of its own. A kind is
// lane-homogeneous (see AgentSpec.Lane), so this kind-keyed row is a COMPLETE
// description of every session of that kind.
//
//   - post_input — a typed line can be delivered to the session's pane (the
//     prompt/attach kickoff path). NOT superseded by anything in v2: it describes
//     pane-delivered input, which the feed-input surface below does not replace.
//   - approvals — where approvals are answered: "tui" (in the terminal) or "remote"
//     (through the hub's POST /approvals/{id} verb — opencode today).
//   - watch — DEPRECATED by feed; retained until clients migrate. The producer holds
//     `watch == (feed == "messages")` in lockstep (invariant-tested in
//     capabilities_test.go), so a v1 client reading watch and a v2 client reading
//     feed see the same thing. Removed once no client reads it.
//   - input — the feed-input posting mode, SINGLE-VALUED: "gated" means POST /input is
//     accepted only while the session is waiting (the hub's acceptance re-check),
//     "turn" means the lane takes whole turns through POST /turn (and POST /input no
//     longer applies — opencode today), "" means no feed input at all (the TUI-only
//     post_input path still applies).
//   - feed — what the hub can stream for the kind: "messages" (a normalized
//     conversation feed: GET /messages + message.appended), "activity" (the activity
//     dimension only — the stability/transcript engines derive it, but there is no
//     message feed), or "none" (no hub signal at all).
//   - interrupt — the interrupt verb is supported (opencode today; false elsewhere).
//   - attach — how a terminal reaches the session: "tmux" (attach to the rc-tmux
//     session), "native-remote" (the agent's own remote surface), or "none".
//
// feed and attach carry omitempty but are NEVER empty in this binary's own output
// (kindFeatures always assigns both, and the strict golden pins them present) — the
// omitempty exists for RE-EMISSION fidelity: a newer server decoding an OLDER guest's
// capabilities and marshaling them onward (overview embeds this struct raw) must
// re-emit the fields as ABSENT, not as "", so the client-side absent-field fallbacks
// (absent feed falls back to watch; absent attach means "tmux") apply cleanly on
// mixed-version fleets and the out-of-matrix values ""/"" never appear on any wire.
// interrupt stays unconditional: false is its real matrix value, and an absent bool
// already decodes to the same default everywhere.
type KindFeatures struct {
	PostInput bool   `json:"post_input"`
	Approvals string `json:"approvals"`
	Watch     bool   `json:"watch,omitempty"`
	Input     string `json:"input,omitempty"`
	Feed      string `json:"feed,omitempty"`
	Interrupt bool   `json:"interrupt"`
	Attach    string `json:"attach,omitempty"`
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

// kindFeatures returns the per-kind UI hints for the agent kinds the hub can watch and
// steer. claude-broker (driven from claude.ai,
// not the pane) and shell (no agent approval surface) are OMITTED entirely — an absent
// entry means "no feed/input/approval affordances", exactly the client behavior those
// two kinds already have.
//
// The emitted matrix (pinned exhaustively by capabilities_test.go):
//
//	kind      | post_input | approvals | watch | input | feed     | interrupt | attach
//	claude-rc | true       | tui       | false | ""    | activity | false     | tmux
//	codex     | true       | tui       | true  | gated | messages | false     | tmux
//	opencode  | true       | remote    | true  | turn  | messages | true      | tmux
//	cursor    | true       | tui       | false | ""    | activity | false     | tmux
func kindFeatures() map[Kind]KindFeatures {
	out := map[Kind]KindFeatures{}
	for _, k := range allKinds {
		if k == KindClaudeBroker || k == KindShell {
			continue
		}
		// The BASE row is a TUI-lane session: approvals answered on the pane, a terminal
		// reaching it by attaching to tmux, no turn/interrupt verb, no feed input.
		// "activity" is the feed floor — the hub's stability/transcript engines derive the
		// activity dimension for every watched kind even where no message feed exists.
		// Each divergent kind then states its WHOLE row once (no layered overrides), so a
		// field's value is readable without simulating the assignments above it.
		kf := KindFeatures{
			PostInput: AcceptsTypedInput(k),
			Approvals: "tui",
			Feed:      "activity",
			Attach:    "tmux",
		}
		switch k {
		case KindCodex:
			// codex's rollout JSONL is folded into a normalized message feed, and its
			// composer anchor gates POST /input acceptance.
			kf.Feed, kf.Input = "messages", inputModeGated
		case KindOpencode:
			// opencode is the first LIVE lane: its TUI runs an embedded HTTP+SSE server
			// the hub steers through (watch_opencode_transport.go's verb lane), so whole
			// turns, interrupts and approvals all go through the hub rather than the pane.
			// `input` is single-valued, so "turn" REPLACES the "gated" codex spelling:
			// POST /input no longer applies to opencode (a behavior break for hub clients
			// — the turn verb is the steering surface, and the create/prompt kickoff path
			// still delivers the first prompt via post_input). The divergence from codex
			// is deliberate; the two rows are no longer asserted equal.
			kf.Feed, kf.Input = "messages", inputModeTurn
			kf.Approvals, kf.Interrupt = approvalsRemote, true
		}
		// watch is the deprecated spelling of feed == "messages"; derived here rather
		// than set by hand so the two cannot drift (invariant-tested besides).
		kf.Watch = kf.Feed == "messages"
		out[k] = kf
	}
	return out
}

// kindFeatureRows memoizes the derived matrix for the REQUEST paths (the gates in
// handleInput and verbTarget), which read a single row per request and must not rebuild
// the whole table to do it. kindFeatures() is pure, so the memoization is invisible;
// BuildCapabilities keeps calling it directly for a FRESH map, because that map is handed
// to callers and serialized and so must never alias this shared one.
var kindFeatureRows = sync.OnceValue(kindFeatures)

// kindFeatureRow returns a kind's advertised row — the single accessor every capability
// gate reads, so a gate can never disagree with what a client was told. An unregistered
// kind (or a newer client's) yields the ZERO row, which advertises no affordance at all.
func kindFeatureRow(k Kind) KindFeatures { return kindFeatureRows()[k] }

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
