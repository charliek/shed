package roostprovider

import "strings"

// Agent is one row of the agent table (plan 019 §3.2 step 2): the three
// spellings of one coding agent, which are NOT interchangeable.
//
//   - Kind is the RC wire vocabulary (`shed_core::rc::RcKind::as_str`) — what
//     goes in a row id's `agent=` key and what every other shed surface calls
//     this agent. Note `claude-rc`, not `claude`.
//   - Binary is what `command -v` looks for on the far side and what the tab's
//     argv execs (`shed_core::roost::model::launch_argv`). Note `cursor-agent`,
//     not `cursor`.
//   - Title is the palette row's text, and the opened tab's title. Short and
//     human, because it is read in roost's palette beside five siblings.
//
// All three are pinned together by crates/fixtures/roost-vectors/agent-table.json,
// asserted from Go (goldens_test.go) and from Rust (crates/shed-core/tests/
// roost_provider_vectors.rs, against `launch_argv` and `roost_capabilities`).
type Agent struct {
	Kind   string
	Binary string
	Title  string
}

// agentTable is THE table, in the provider's display order.
//
// That order is `claude codex cursor-agent opencode gx grok` — pinned twice by
// plan 019 §3.2 (the probe's `command -v` list, and the "no agents found"
// subtitle's copy) — and it deliberately differs from
// `roost_capabilities().kinds`, which orders cursor and opencode the other way
// around. The two lists are asserted equal as SETS, never as sequences: one is
// a capabilities advertisement whose order means nothing, the other is a menu a
// human reads. See the Rust twin test for the same note from the other side.
var agentTable = []Agent{
	{Kind: "claude-rc", Binary: "claude", Title: "claude"},
	{Kind: "codex", Binary: "codex", Title: "codex"},
	{Kind: "cursor", Binary: "cursor-agent", Title: "cursor"},
	{Kind: "opencode", Binary: "opencode", Title: "opencode"},
	{Kind: "gx", Binary: "gx", Title: "gx"},
	{Kind: "grok", Binary: "grok", Title: "grok"},
}

// AgentByKind looks an agent up by its RC kind string (a row id's `agent=`
// value). ok=false for anything not in the table — including `shell` and
// `claude-broker`, which are real RcKinds with no launch recipe, and any kind a
// hand-edited or stale row id might carry.
func AgentByKind(kind string) (Agent, bool) {
	for _, a := range agentTable {
		if a.Kind == kind {
			return a, true
		}
	}
	return Agent{}, false
}

// agentBinaryList renders the table's binaries as the "no agents found on
// <host>" subtitle spells them: comma-separated, in display order. Pinned copy
// (plan 019 §3.2), derived from the table rather than typed out again so the
// sentence cannot drift from what the probe actually looked for.
func agentBinaryList() string {
	names := make([]string, len(agentTable))
	for i, a := range agentTable {
		names[i] = a.Binary
	}
	return strings.Join(names, ", ")
}
