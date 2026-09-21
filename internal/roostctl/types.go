package roostctl

// The documents `roostctl --json` prints, as Go.
//
// Every shape here is a SUBSET of the corresponding `roost_ipc::messages`
// struct at the rev `crates/Cargo.toml` pins, taken from `roostctl schema`.
// Subsetting is deliberate and the same choice `internal/roostprovider/wire.go`
// makes: a field roost adds costs nothing, and a field shed does not read
// cannot drift. What is NOT optional is the spelling of the fields that are
// here — roost's own JSON names, never a Go-shaped guess at them.

// Identify is `roostctl identify --json` — the running local roost UI's
// identity.
//
// **`ProtocolVersion` is the UI socket's protocol, NOT the session protocol.**
// The two numbers are separate generations of separate wires:
// `roostprovider.SpokenProtocol` gates the SESSION wire a shed's roost speaks
// over SSH, and nothing here is compared against it — or against anything
// else. It is carried because `identify` reports it, and it is not a gate.
// Gating on it would refuse a perfectly good local app over a number that
// describes a different protocol.
type Identify struct {
	// SocketPath is the UI socket roostctl reached. Never empty in a real
	// reply (roost's field is a plain `String`), which is what makes it a
	// usable "this really is an identify document" check — see Available.
	SocketPath string `json:"socket_path"`
	// PID is the running UI's process id.
	PID int `json:"pid"`
	// UIVersion is the app's own version string (e.g. "0.7.0").
	UIVersion string `json:"ui_version"`
	// ProtocolVersion is the UI protocol. See the note above: not a gate.
	ProtocolVersion int `json:"protocol_version"`
}

// The `state` spellings a saved host can be in (`roost_ipc::messages::
// host_state`). The whole vocabulary — five, and no more.
//
// **There is no "no session" state.** A host whose far side has no
// roost-session running is `StateDisconnected` with that fact in
// HostStatus.Reason; a reader looking for a sixth spelling will not find one,
// and a reader switching on state alone cannot tell "nothing is listening
// there" from "the network is down". The reason string is where that lives.
const (
	StateDisconnected = "disconnected"
	StateConnecting   = "connecting"
	StateConnected    = "connected"
	StateStopped      = "stopped"
	StateNeedsRestart = "needs-restart"
)

// Host is one saved host's REGISTRY row — what `host add` and `host connect`
// answer with. No connection state: that is HostStatus's job, and roost keeps
// the two apart on purpose so a reader always knows which op it read.
type Host struct {
	// ID is the saved host's opaque, stable id — what every `host …` verb is
	// addressed to.
	//
	// **It is not a tab reference.** `tab focus --tab` takes the
	// `h<incarnation>.<id>` spelling instead, and the incarnation is a number
	// this id does not contain; SidebarHostTab.Key is the only place it is
	// exposed.
	ID string `json:"id"`
	// Label is the display name the host was saved under.
	Label string `json:"label"`
	// Target is the SSH destination or socket path it connects to.
	Target string `json:"target"`
	// LastConnected is an RFC3339 timestamp, empty for a host that never has.
	// roost sends `null` there, which decodes to "".
	LastConnected string `json:"last_connected"`
}

// HostStatus is one saved host's LIVE connection state — `host status --json`
// and `host list --json` both answer with a list of these (list's rows carry
// the registry fields only; status fills the rest in).
type HostStatus struct {
	Host
	// Generation counts connection ATTEMPTS started, 0 before the first, and
	// it survives a disconnect. roost's own note calls it the monotonic edge a
	// poller waits on: two consecutive attempts can fail with byte-identical
	// reasons, so "disconnected, with this reason" cannot tell attempt N from
	// N−1 and this can.
	Generation uint64 `json:"generation"`
	// State is one of the five spellings above.
	State string `json:"state"`
	// Reason is the connection's own one-line reason, empty when there is
	// none. This is where "no session is listening over there" arrives — see
	// the note on the state constants.
	Reason string `json:"reason"`
	// Detail is the long form behind Reason, when there is one. Today exactly
	// one thing fills it: a localhost session that could not be started.
	Detail string `json:"detail"`
	// Tabs is how many tab rows this host's sidebar section is listing. roost
	// sends it always, 0 included, so a reader watching it across a reconnect
	// gets a number every time rather than a key that comes and goes.
	Tabs int `json:"tabs"`
}

// HostConnection is `host connect --json` — the host plus the state the
// attempt is in. Connect returns once the attempt is UNDER WAY, so this state
// is routinely `connecting`; the settled answer comes from HostStatus.
type HostConnection struct {
	Host  Host   `json:"host"`
	State string `json:"state"`
}

// hostsResult is the `{"hosts":[…]}` envelope both `host list` and
// `host status` answer with.
type hostsResult struct {
	Hosts []HostStatus `json:"hosts"`
}

// hostResult is the `{"host":{…}}` envelope `host add` answers with.
type hostResult struct {
	Host Host `json:"host"`
}

// SidebarDump is `rpc app.sidebar_dump` — the sidebar's LAST-RENDERED rows,
// read from the same per-project cache the sidebar paints from rather than
// re-derived from the workspace snapshot.
//
// This is the op shed reads to find a host's tabs, because of one field:
// SidebarHostTab.Key.
type SidebarDump struct {
	// AgentsVisible is the config/feature toggle only. Projects stays
	// populated when it is off.
	AgentsVisible bool `json:"agents_visible"`
	// Hosts are the saved hosts' sections below LOCAL, in sidebar order. A
	// host that has never connected has no projects.
	Hosts []SidebarHost `json:"hosts"`
	// Projects are the LOCAL workspace's projects and their agent rows, in
	// sidebar order, including ones with zero agents.
	Projects []SidebarProject `json:"projects"`
	// Sections is the section strip's bands, in sidebar order, including the
	// local one. Empty (and so absent) when the sidebar draws its classic
	// single sticky PROJECTS header instead of a strip.
	Sections []SidebarSection `json:"sections"`
}

// SidebarHost is one host section of the sidebar.
type SidebarHost struct {
	// ID is the saved host's id — the same string Host.ID carries, and what a
	// `host …` verb takes.
	ID string `json:"id"`
	// Label is the saved label.
	Label string `json:"label"`
	// State is the same wire spelling `host status` reports.
	State string `json:"state"`
	// Projects are this host's projects, in the mirror's order.
	Projects []SidebarHostProject `json:"projects"`
}

// SidebarHostProject is one project of a host section.
type SidebarHostProject struct {
	// Key is the host-qualified `h<incarnation>.<id>` spelling — see
	// SidebarHostTab.Key.
	Key string `json:"key"`
	// Name is the project's name.
	Name string `json:"name"`
	// Tabs are its tabs, in the mirror's order.
	Tabs []SidebarHostTab `json:"tabs"`
}

// SidebarHostTab is one tab of a host project.
type SidebarHostTab struct {
	// Key is the `h<incarnation>.<id>` spelling, and it is the reason this op
	// is read at all.
	//
	// **It is the ONLY place the numeric host incarnation is exposed.** A
	// saved host carries an opaque string id (Host.ID) that `tab focus --tab`
	// will not accept; the incarnation is the UI's own per-connection number,
	// and it appears nowhere else on this wire. So the route from "shed X is a
	// saved host" to "focus shed X's tab" runs through a sidebar dump — there
	// is no way to compose this reference from a host id and a tab id.
	Key string `json:"key"`
	// Title is the tab's title, as drawn.
	Title string `json:"title"`
}

// SidebarProject is one LOCAL project's agent rows.
type SidebarProject struct {
	ProjectID string         `json:"project_id"`
	Agents    []SidebarAgent `json:"agents"`
}

// SidebarAgent is one rendered agent row, exactly as the sidebar drew it —
// StatusText and TimeText are display strings, not values to parse.
type SidebarAgent struct {
	TabID      string `json:"tab_id"`
	Name       string `json:"name"`
	Lifecycle  string `json:"lifecycle"`
	StatusText string `json:"status_text"`
	TimeText   string `json:"time_text"`
	IsActive   bool   `json:"is_active"`
}

// SidebarSection is one band of the sidebar's section strip.
type SidebarSection struct {
	// Role tells the three kinds of band apart: "local", "session", "host".
	Role string `json:"role"`
	// Label is the band's header text, as drawn.
	Label string `json:"label"`
	// State is the same wire spelling `host status` reports.
	State string `json:"state"`
	// Dot is "connected" | "pending" | "offline".
	Dot string `json:"dot"`
	// SavedID is the saved host this band renders, and the ONLY pairing
	// between a band and a saved host — under `local-backend = session` the
	// leading band is itself a host, so position says nothing. Empty for the
	// in-process band and for the session placeholder.
	SavedID string `json:"saved_id"`
	// ReconnectRow is whether this band offers the inline reconnect row.
	ReconnectRow bool `json:"reconnect_row"`
	// Fidelity is "update" | "restart" | "manual", empty when the band draws
	// no pill.
	Fidelity string `json:"fidelity"`
}
