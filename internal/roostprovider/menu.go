package roostprovider

import (
	"errors"
	"fmt"

	"github.com/charliek/shed/internal/config"
)

// Row is one palette row, in roost's provider-output schema
// (`roost_ui_model::provider::ProviderOutputItem`).
//
// Actionable is a POINTER because roost reads it as `Option<bool>` with "absent
// ⇒ actionable": emitting `"actionable": true` on every ordinary row would be
// noise, and emitting `false` is the only thing that means anything. Row's zero
// value is therefore an ordinary, selectable row.
type Row struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Subtitle   string `json:"subtitle,omitempty"`
	Actionable *bool  `json:"actionable,omitempty"`
}

// Menu is a provider phase's whole stdout (`{"items":[…],"placeholder":"…"}`).
// roost accepts a bare array too; the object form is used here because only it
// can carry the placeholder.
type Menu struct {
	Placeholder string `json:"placeholder,omitempty"`
	// Items is never nil on the wire: `omitempty` is deliberately absent so a
	// menu with no rows serializes as `"items":[]` rather than vanishing.
	// Every builder here returns at least one row, so this is belt to braces.
	Items []Row `json:"items"`
}

// listPlaceholder is the palette's prompt text for the top-level menu
// (plan 019 §3.2 step 1).
const listPlaceholder = "Start an agent on…"

// falseVal backs every non-actionable row's Actionable pointer. One shared
// address is safe because nothing ever writes through it — Row is serialized,
// never mutated.
var falseVal = false

// NoneRow builds a non-actionable row. Every one of plan 019 §3.2's pinned
// empty/error states goes through here, so they all carry the same reserved
// id and the same `actionable:false`.
func NoneRow(title, subtitle string) Row {
	return Row{ID: NoneID, Title: title, Subtitle: subtitle, Actionable: &falseVal}
}

// ListMenu builds the top-level menu (plan 019 §3.2 step 1): every RUNNING shed
// across the configured servers, then every `machines:` entry.
//
// Stopped sheds are deliberately absent — roost's own convention for a provider
// list, and the practical reason is stronger: a stopped shed cannot take a tab,
// and `shed start` is slow work that belongs in a tab rather than inside a
// five-second `activate`.
func ListMenu(inv ShedInventory, machines []config.MachineEntry) Menu {
	items := make([]Row, 0, len(inv.Sheds)+len(machines))
	for _, s := range inv.Sheds {
		items = append(items, Row{
			ID:       Token{Shed: s.Name, Server: s.Server}.Encode(),
			Title:    "shed: " + s.Name,
			Subtitle: s.Server + " · " + landingLabel(s.LandingDir),
		})
	}
	for _, m := range machines {
		items = append(items, Row{
			ID:       Token{Machine: m.Name}.Encode(),
			Title:    "machine: " + m.Name,
			Subtitle: machineDest(m),
		})
	}
	if len(items) == 0 {
		items = append(items, emptyListRow(inv))
	}
	return Menu{Placeholder: listPlaceholder, Items: items}
}

// emptyListRow picks between plan 019 §3.2's two pinned empty-menu rows.
//
// "no shed-server answered" fires only when servers were configured and NONE of
// them answered — an unreachable fleet, which is a different problem from an
// idle one and deserves the names it tried. With no servers configured at all
// there is nothing to have failed, so the idle copy is the honest one.
func emptyListRow(inv ShedInventory) Row {
	if len(inv.Tried) > 0 && len(inv.Answered) == 0 {
		return NoneRow("no shed-server answered", joinNames(inv.Tried))
	}
	return NoneRow(
		"No running sheds or machines",
		"shed start <name>, or add a machines: entry to ~/.shed/config.yaml",
	)
}

// landingLabel renders a shed's landing dir for its list-row subtitle. An older
// shed-server that reports none, or a shed with no project mount, shows `~` —
// which is also what the workdir step falls back to (plan 019 §3.2 step 3).
func landingLabel(dir string) string {
	if dir == "" {
		return "~"
	}
	return dir
}

// machineDest renders a `machines:` entry as `[user@]host[:port]` for its
// list-row subtitle.
//
// Built from the Target that will actually be dialled rather than from the
// entry, so the claim "this subtitle describes the connection that will really
// be made" is structural. Composing `[user@]host` and the "no `:port` on 22"
// rule a second time here would be the same rule in two files, agreeing only
// as long as nobody edited one of them — and the list subtitle is precisely
// where a user would look to diagnose a wrong dial.
func machineDest(m config.MachineEntry) string {
	return MachineTarget(m).Label()
}

// AgentMenu builds step 2's menu: one row per agent the probe actually found on
// the far side.
//
// host must be a host-only token (Token.Host()); each row extends it with the
// chosen agent and the probed absolute $HOME, which is what lets step 3 run
// without re-probing.
func AgentMenu(host Token, p Probe) Menu {
	items := make([]Row, 0, len(agentTable))
	for _, a := range agentTable {
		path, ok := p.Found[a.Kind]
		if !ok {
			continue
		}
		tok := host
		tok.Agent = a.Kind
		tok.Home = p.Home
		items = append(items, Row{ID: tok.Encode(), Title: a.Title, Subtitle: path})
	}
	if len(items) == 0 {
		items = append(items, NoAgentsRow(host))
	}
	return Menu{Items: items}
}

// The six pinned non-actionable rows of plan 019 §3.2 step 2. Their copy is
// PINNED — these constructors exist so it is written once and asserted once,
// rather than formatted inline at three call sites in cmd/shed (C3) and drifting
// between them.

// NotInstalledRow — the bridge exited 127 / said `command not found`: roost's
// candidate ladder fell off its end. Until plan 019's S5 lands a session in a
// shed, every shed row lands here.
func NotInstalledRow(host Token) Row {
	return NoneRow(
		fmt.Sprintf("roost-session is not installed on %s", host.HostLabel()),
		"connect from the shed desktop or mobile app to install it",
	)
}

// NotRunningRow — the bridge ran and answered `client-bridge: no session`:
// installed, not running.
func NotRunningRow(host Token) Row {
	return NoneRow(
		fmt.Sprintf("roost-session is not running on %s", host.HostLabel()),
		"connect from the shed app to start it, or run roost-session start there",
	)
}

// ProtocolMismatchRow — the session answered `session.identify` with a protocol
// this build does not speak. Pin P6: REPORT it, never restart it. A running
// session belongs to whoever started it, and a provider that stopped one to fix
// a version skew would kill somebody's tabs.
func ProtocolMismatchRow(host Token, spoken int) Row {
	return NoneRow(
		fmt.Sprintf("roost-session on %s speaks protocol %d; this shed speaks %d",
			host.HostLabel(), spoken, SpokenProtocol),
		"upgrade whichever is older",
	)
}

// UnreachableRow — ssh itself failed. detail is ssh's last stderr line, which
// is where an auth or host-key failure states its own case (see ProviderRow for
// why those fold in here rather than growing rows of their own).
func UnreachableRow(host Token, detail string) Row {
	return NoneRow(fmt.Sprintf("%s is unreachable", host.HostLabel()), detail)
}

// NoSSHRow — there is no `ssh` binary this process can find. Local, and the
// only one of the six that says nothing about the far side, so it names the
// paths it looked in: roost runs a provider with roost's OWN environment, which
// for a Finder-launched app is the minimal macOS PATH, and "which paths were
// searched" is the whole diagnosis.
//
// The list is derived from what ResolveSSH actually searches, not handed in —
// same rule as NoAgentsRow below, so the sentence cannot drift from the search.
func NoSSHRow() Row {
	return NoneRow(
		"ssh is not installed where roost can see it",
		joinNames(append([]string{"$PATH"}, sshFallbackPaths...)),
	)
}

// NoAgentsRow — the probe succeeded and found none of the six binaries. The
// subtitle names both the binaries and the shell verb, because the usual cause
// is neither "not installed" nor "broken" but the login-PATH trap: an agent
// that an interactive shell finds and a login shell does not.
func NoAgentsRow(host Token) Row {
	return NoneRow(
		fmt.Sprintf("no agents found on %s", host.HostLabel()),
		fmt.Sprintf("looked for %s under bash -lc", agentBinaryList()),
	)
}

// RowForError maps an error from a Remote call onto one of plan 019 §3.2's
// pinned non-actionable rows.
//
// It lives here, with the constructors, rather than in remote.go: "which row
// does the user see" is this file's whole subject, and a transport module that
// built palette rows would be two altitudes in one place.
//
// ok=false means "this is not a row": a malformed probe, an unparseable
// response, a bug. Those stay PROVIDER FAILURES (acceptance criterion 3) —
// the provider exits non-zero and roost surfaces the error, because a row would
// be claiming to know something about the far side that this side did not
// actually learn. ErrNoSSH is deliberately not handled here either: it is not a
// Remote failure, it happens before a Remote exists, and its caller answers it
// with NoSSHRow directly.
func RowForError(host Token, err error) (Row, bool) {
	var mismatch *ProtocolMismatchError
	if errors.As(err, &mismatch) {
		return ProtocolMismatchRow(host, mismatch.Spoken), true
	}
	var reach *ReachError
	if !errors.As(err, &reach) {
		return Row{}, false
	}
	if reach.Phase == PhaseProbe {
		// Every probe failure is a reachability failure — see
		// ReachError.Phase.
		return UnreachableRow(host, reach.Detail()), true
	}
	// BridgeRow, not ProviderRow: on the bridge leg a timeout or ssh's own
	// exit 255 outranks whatever the stderr blob happened to contain (see
	// BridgeRow).
	switch BridgeRow(reach.Class, reach.ExitCode, reach.TimedOut) {
	case RowNotInstalled:
		return NotInstalledRow(host), true
	case RowNotRunning:
		return NotRunningRow(host), true
	default:
		return UnreachableRow(host, reach.Detail()), true
	}
}

// WorkdirMenu builds step 3's menu from the candidates WorkdirCandidates
// produced. Each row extends tok (which already carries the host, the agent and
// the probed $HOME) with the chosen cwd and, for a far-side project, its id.
//
// Never called with zero candidates — Home is always one — and never called
// with exactly one, because that case collapses (see CollapseWorkdirs).
func WorkdirMenu(tok Token, candidates []Workdir) Menu {
	items := make([]Row, 0, len(candidates))
	for _, c := range candidates {
		next := tok
		next.Cwd = c.Cwd
		next.Project = c.ProjectID
		items = append(items, Row{ID: next.Encode(), Title: c.Title, Subtitle: c.Cwd})
	}
	return Menu{Items: items}
}
