package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/roostctl"
	"github.com/charliek/shed/internal/roostprovider"
)

// The roost-native half of `shed sessions` (plan 022 §3.3, C5c), under the
// same owner decision D1 `shed attach` runs under: **roost enhances, tmux is
// the floor.** With a local roost app running, a listing shows each shed's
// roost tabs above its tmux sessions and `sessions kill` can close a tab; with
// no roost — or with `--tmux` / `SHED_ATTACH=tmux` — nothing here runs at all
// and the command behaves exactly as it did before roost existed.
//
// Two transports, the same two `shed attach` uses and for the same reasons:
// the LOCAL app is the GATE only (internal/roostctl, exec'd), and the tabs
// themselves are read from each shed's OWN roost-session over SSH
// (internal/roostprovider). Nothing here talks to a socket directly.
//
// **Listing is cheaper than attaching, deliberately.** `shed attach` runs the
// whole local `host …` family (list, add, connect, focus) because it has to
// put a tab in front of the user; a listing only needs `tab.list` on each
// shed, so the local app is asked one question — "are you there?" — and the
// rest is the provider transport. That is why a shed with no roost-session
// costs one failed ssh and a warning rather than a failure.

const (
	// defaultRoostTabsPerShed bounds ONE shed's `tab.list` — the whole of it,
	// ssh handshake included.
	//
	// Much shorter than the attach path's 30 s remote budget (see
	// defaultRoostRemoteCap), because the two commands are not asking for the
	// same patience. `shed attach` is a request to DO something to one named
	// shed, and a user who typed it will wait for the far side to wake up. A
	// listing is a glance at the whole fleet, every row of it optional: a shed
	// that cannot answer in three seconds is simply listed with its tmux rows
	// and a warning, and making the user wait longer for that outcome buys
	// nothing. Three seconds is also inside roost's own 5 s `list`-phase
	// budget (roostprovider.DefaultPerServerTimeout's neighbourhood), which is
	// the closest thing to a house number for "a listing round trip".
	defaultRoostTabsPerShed = 3 * time.Second

	// defaultRoostTabsWorkers is how many sheds are asked at once.
	//
	// The budget above is PER SHED, so the width is what keeps a fleet's
	// listing from being the sum of its sheds: four workers turn twelve sheds
	// into three rounds. Not unbounded, because each worker is an ssh process
	// with a connection behind it and a fleet-sized fan-out would open every
	// one of them at once — on a laptop behind a VPN that is a stampede, not a
	// speed-up.
	defaultRoostTabsWorkers = 4
)

// roostSessions is the roost half of one `shed sessions` (or `sessions kill`)
// run: the local gate, the provider transport's two knobs, the warning sink
// and the fan-out's bounds.
//
// Every field a test needs to move is a field rather than a package-level
// knob, the same shape roostAttach uses — the roostctl shim is on PATH
// per-test, and the ssh binary, the warning sink and the bounds travel in
// here.
type roostSessions struct {
	// ctl drives the LOCAL roost app, and only to answer the gate. Never nil.
	ctl *roostctl.Client
	// sshBin is the ssh binary the provider transport execs. Empty means
	// resolve it at use (roostprovider.ResolveSSH).
	sshBin string
	// controlDir enables the provider transport's ControlMaster. Empty
	// disables muxing.
	controlDir string
	// errOut carries the degradation warnings, which are never failures.
	errOut io.Writer

	// perShed bounds one shed's `tab.list` (and one `tab.close`).
	perShed time.Duration
	// workers is the fan-out's width.
	workers int
}

// newRoostSessions builds the production roost half. A var so a test can hand
// the commands a rig-backed one; never reassigned outside tests.
var newRoostSessions = func() *roostSessions {
	return &roostSessions{
		ctl:        &roostctl.Client{},
		controlDir: roostprovider.ControlDirFor(os.Getenv, os.Getuid()),
		errOut:     os.Stderr,
		perShed:    defaultRoostTabsPerShed,
		workers:    defaultRoostTabsWorkers,
	}
}

// sessionsWantsTmux / sessionsKillWantsTmux are the two commands' readings of
// the floor gate — the same rule `shed attach` applies, with each command's
// own `--tmux`.
func sessionsWantsTmux() bool     { return wantsTmuxFloor(sessionsTmuxFlag) }
func sessionsKillWantsTmux() bool { return wantsTmuxFloor(sessionsKillTmuxFlag) }

// warn prints one degradation line to stderr, in the `Warning: …` shape
// `shed sessions` already uses for a shed the server could not query.
//
// **Always one line.** A provider failure can carry a far side's stderr, and a
// multi-line warning in the middle of a table turns a degraded listing into
// something that reads like a crash. oneLine folds it.
func (r *roostSessions) warn(format string, args ...any) {
	fmt.Fprintf(r.errOut, "Warning: "+format+"\n", args...)
}

// oneLine folds a multi-line message into one line, so a warning printed
// beside a table stays one row of it.
func oneLine(err error) string {
	return strings.Join(strings.Split(strings.TrimSpace(err.Error()), "\n"), "; ")
}

// -----------------------------------------------------------------------
// Reading the tabs.
// -----------------------------------------------------------------------

// shedTabs is one shed's answer: its tabs, or the reason there are none.
//
// The error is carried rather than returned because it is NOT a failure —
// every unreachable shed still contributes its tmux rows, and its error
// becomes one warning line. A shed that answered with no tabs and a shed that
// could not be asked are different states, and collapsing them would silently
// tell a user their tabs are gone.
type shedTabs struct {
	shed roostprovider.RunningShed
	tabs []roostprovider.Tab
	err  error
}

// runningSheds asks each queried server which sheds are RUNNING, filtered to
// `only` when the user named one.
//
// **Not derived from the tmux rows.** The obvious shortcut — "ask about the
// sheds that already have sessions" — is wrong in exactly the case this
// command exists to serve: a shed used entirely through roost tabs has no tmux
// sessions at all, so it would never be asked and its tabs would never appear.
//
// A server that cannot answer is one warning and no rows: the tmux half has
// already reported (or returned) whatever it made of the same server, and a
// second failure about the same unreachable machine helps nobody.
func (r *roostSessions) runningSheds(servers []namedServer, only string) []roostprovider.RunningShed {
	var out []roostprovider.RunningShed
	for _, server := range servers {
		entry := server.entry
		resp, err := NewAPIClientFromNamedEntry(server.name, &entry, DefaultTimeout).ListSheds()
		if err != nil {
			r.warn("could not list %s's sheds for roost tabs: %s", server.name, oneLine(err))
			continue
		}
		for _, shed := range resp.Sheds {
			if shed.Status != config.StatusRunning {
				continue
			}
			if only != "" && shed.Name != only {
				continue
			}
			out = append(out, roostprovider.RunningShed{
				Name:          shed.Name,
				Server:        server.name,
				ServerHost:    entry.Host,
				ServerSSHPort: entry.SSHPort,
				LandingDir:    shed.LandingDir,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// tabsFor reads every shed's tabs, at most workers at a time, each under its
// own perShed budget.
//
// Results land in a slice indexed by the shed's position, which is what makes
// the answer ORDER-STABLE without a lock: each worker owns exactly one cell,
// and the listing's order is the one runningSheds already sorted. A map plus a
// mutex would produce the same rows in a different order run to run, and a
// table that reshuffles itself between two invocations is a table nobody
// trusts.
func (r *roostSessions) tabsFor(ctx context.Context, sheds []roostprovider.RunningShed) []shedTabs {
	out := make([]shedTabs, len(sheds))
	width := r.workers
	if width <= 0 {
		width = defaultRoostTabsWorkers
	}
	sem := make(chan struct{}, width)
	var wg sync.WaitGroup
	for i, shed := range sheds {
		wg.Add(1)
		go func(i int, shed roostprovider.RunningShed) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = r.tabsForShed(ctx, shed)
		}(i, shed)
	}
	wg.Wait()
	return out
}

// tabsForShed reads ONE shed's tabs over the provider transport.
//
// `tab.list` alone — no `session.identify` and no probe. Attaching needs the
// probe because it has to resolve a cwd that exists on the far side; a listing
// renders the cwd each tab already carries, so the extra round trip would cost
// a shed's whole budget to learn nothing. A far side that speaks a protocol
// this build does not fails the call itself, which is the same warning by
// another name.
func (r *roostSessions) tabsForShed(ctx context.Context, shed roostprovider.RunningShed) shedTabs {
	remote, target, err := roostRemoteFor(r.sshBin, r.controlDir, shed)
	if err != nil {
		return shedTabs{shed: shed, err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, r.perShed)
	defer cancel()
	projects, err := remote.TabList(ctx, target)
	if err != nil {
		return shedTabs{shed: shed, err: err}
	}
	return shedTabs{shed: shed, tabs: flattenTabs(projects)}
}

// flattenTabs is every tab of every project, in the far side's own order.
//
// The project a tab is filed under is not rendered: a shed's session holds one
// machine's projects, the tab's own cwd says where it is, and shed opens its
// tabs under roost's `"0"` sentinel and so never knows which project its own
// landed in (see reusableTab). Grouping a listing by a key this side cannot
// predict would be noise.
func flattenTabs(projects []roostprovider.Project) []roostprovider.Tab {
	var out []roostprovider.Tab
	for _, project := range projects {
		out = append(out, project.Tabs...)
	}
	return out
}

// reportTabFailures prints one warning per shed that could not be asked, in
// listing order, and is the ONLY place a tab-listing failure becomes visible.
//
// One line per shed, never per attempt: a fleet behind a dead VPN prints as
// many lines as it has sheds, which is already the most a user wants, and
// anything richer belongs to `shed attach`'s own diagnosis of one named shed.
func (r *roostSessions) reportTabFailures(results []shedTabs) {
	for _, result := range results {
		if result.err != nil {
			r.warn("could not list %s's roost tabs: %s", result.shed.Name, oneLine(result.err))
		}
	}
}

// -----------------------------------------------------------------------
// Merging and rendering.
// -----------------------------------------------------------------------

// shedListing is one shed's merged rows: its roost tabs, then its tmux
// sessions. The order inside is the product rule — roost first, tmux under it
// — and the order between listings is (server, shed).
type shedListing struct {
	server string
	shed   string
	tabs   []roostprovider.Tab
	tmux   []config.Session
}

// mergeSessions joins the two halves into the per-shed listings the renderers
// walk.
//
// Keyed on (server, shed), never on the shed name alone: two servers can each
// have a `web`, and merging them would file one machine's tabs under the
// other's sessions. A tmux row whose shed answered no tabs still gets a
// listing, which is what makes an unreachable shed degrade to exactly its old
// output.
func mergeSessions(tabs []shedTabs, sessions []config.Session) []shedListing {
	index := map[string]*shedListing{}
	key := func(server, shed string) string { return server + "\x00" + shed }
	at := func(server, shed string) *shedListing {
		k := key(server, shed)
		if existing, ok := index[k]; ok {
			return existing
		}
		listing := &shedListing{server: server, shed: shed}
		index[k] = listing
		return listing
	}
	for _, result := range tabs {
		listing := at(result.shed.Server, result.shed.Name)
		listing.tabs = append(listing.tabs, result.tabs...)
	}
	for _, session := range sessions {
		listing := at(session.ServerName, session.ShedName)
		listing.tmux = append(listing.tmux, session)
	}

	out := make([]shedListing, 0, len(index))
	for _, listing := range index {
		out = append(out, *listing)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].server != out[j].server {
			return out[i].server < out[j].server
		}
		return out[i].shed < out[j].shed
	})
	return out
}

// The two column sets. The tmux one is today's, unchanged and shared with the
// floor's own renderer so the two can never drift apart.
const (
	roostSessionsHeader = "SHED\tTAB\tSTATE\tAGENT\tCWD\tCREATED"
	tmuxSessionsHeader  = "SHED\tSESSION\tSTATUS\tCREATED\tWINDOWS"
)

// printMergedSessions renders the merged listing: per shed, the roost tabs and
// then the tmux rows.
//
// **A header is printed when the column set CHANGES, not per shed.** The two
// row shapes are different (`TAB STATE AGENT CWD` against `SESSION STATUS
// WINDOWS`), so they cannot share one header — but repeating both headers for
// every shed would turn an ordinary fleet listing into a wall of them. Keying
// the header on the shape means a fleet with no roost tabs anywhere prints the
// single tmux table it always printed, and a shed with tabs prints its roost
// block above its tmux block. The tabwriter is flushed at each switch so each
// run of rows is aligned against its own header rather than against the other
// shape's columns.
func printMergedSessions(w io.Writer, listings []shedListing) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	open := ""
	// A mid-listing flush failure is REMEMBERED rather than returned from
	// here: bailing out of the switch would leave `open` pointing at the
	// previous header and the next rows would be written under the wrong
	// columns. The rest of the table costs nothing to write to a destination
	// that is already failing, and the error still reaches the caller.
	var flushErr error
	section := func(header string) {
		if header == open {
			return
		}
		if open != "" {
			if err := tw.Flush(); err != nil && flushErr == nil {
				flushErr = err
			}
			fmt.Fprintln(w)
		}
		open = header
		fmt.Fprintln(tw, header)
	}

	for _, listing := range listings {
		for _, tab := range listing.tabs {
			section(roostSessionsHeader)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				listing.shed, orDash(tab.Title), orDash(tab.State),
				orDash(tab.AgentLifecycle), orDash(tab.Cwd), formatTimeAgo(tabCreatedAt(tab)))
		}
		for _, session := range listing.tmux {
			section(tmuxSessionsHeader)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n",
				session.ShedName, session.Name, sessionStatus(session),
				formatTimeAgo(session.CreatedAt), session.WindowCount)
		}
	}
	if open == "" {
		fmt.Fprintln(w, "No sessions found")
		return nil
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return flushErr
}

// sessionStatus is the tmux STATUS cell, shared with the floor's renderer.
func sessionStatus(s config.Session) string {
	if s.Attached {
		return "attached"
	}
	return "detached"
}

// tabCreatedAt reads a tab's `created_at` as a time.
//
// roost sends unix SECONDS; a session that did not send one at all decodes to
// 0, and that is not midnight 1970 — it is "this far side did not say". Zero
// time is what formatTimeAgo renders as `unknown`.
//
// **UTC, not local.** The CREATED column is relative ("3 hours ago") and could
// not care either way, but this same value is what `--json` serializes — and
// time.Unix hands back a LOCAL time, whose offset would then travel into the
// document. A `created_at` that reads `+02:00` on one laptop and `Z` on
// another describes the same instant and still breaks every byte comparison
// anyone makes of it, the golden here first.
func tabCreatedAt(tab roostprovider.Tab) time.Time {
	if tab.CreatedAt <= 0 {
		return time.Time{}
	}
	return time.Unix(tab.CreatedAt, 0).UTC()
}

// orDash renders an empty cell as `-`, so a row with a missing field still has
// the same number of visible columns as its neighbours. Both fields that can
// legitimately be empty are roost's own optional ones (`agent_lifecycle` on an
// older session, a title a tab never got).
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// -----------------------------------------------------------------------
// --json.
// -----------------------------------------------------------------------

// The two `source` spellings. A row without the key at all is the tmux FLOOR's
// own output, which is byte-identical to what it has always printed — see
// runSessions.
const (
	sessionSourceTmux  = "tmux"
	sessionSourceRoost = "roost"
)

// sessionRow is one entry of the roost path's `--json` array.
//
// **The array stays FLAT and the tmux entry stays what it was.** A consumer
// that reads `.[] | .name` across a fleet keeps working; `source` is added so
// one that cares can tell the two apart, and everything roost-specific is
// nested under `roost` rather than spread through the top level, so a new tab
// field can never collide with a session one.
//
// config.Session is EMBEDDED rather than copied field by field: that is what
// makes a tmux row here literally the same document it was, and it means a
// field added to config.Session appears in both shapes without this file
// knowing about it.
type sessionRow struct {
	config.Session
	// Source is "tmux" or "roost".
	Source string `json:"source"`
	// Roost is the tab's own half, absent on a tmux row.
	Roost *roostTabJSON `json:"roost,omitempty"`
}

// roostTabJSON is a roost row's tab block: the four facts a tmux session has
// no equivalent for. The title is NOT repeated here — it is the row's `name`,
// which is what makes `sessions kill <shed> <name>` addressable from this
// document.
type roostTabJSON struct {
	TabID string `json:"tab_id"`
	State string `json:"state"`
	Agent string `json:"agent"`
	Cwd   string `json:"cwd"`
}

// mergedSessionRows is the `--json` array, in the table's own order: per shed,
// the roost tabs and then the tmux rows.
//
// A roost row borrows the session shape it does not have facts for rather than
// inventing them: `attached` is false (a tab's liveness is `roost.state`, and
// guessing at "attached" would be a second, contradictory answer) and
// `window_count` is omitted entirely (`omitempty` — a tab has no windows, and
// a hard 0 would read as "no windows" rather than "not a thing tabs have").
func mergedSessionRows(listings []shedListing) []sessionRow {
	rows := make([]sessionRow, 0)
	for _, listing := range listings {
		for _, tab := range listing.tabs {
			rows = append(rows, sessionRow{
				Session: config.Session{
					Name:       tab.Title,
					ShedName:   listing.shed,
					ServerName: listing.server,
					CreatedAt:  tabCreatedAt(tab),
				},
				Source: sessionSourceRoost,
				Roost: &roostTabJSON{
					TabID: tab.ID,
					State: tab.State,
					Agent: tab.AgentLifecycle,
					Cwd:   tab.Cwd,
				},
			})
		}
		for _, session := range listing.tmux {
			rows = append(rows, sessionRow{Session: session, Source: sessionSourceTmux})
		}
	}
	return rows
}

// -----------------------------------------------------------------------
// `sessions kill` on a roost tab.
// -----------------------------------------------------------------------

// tabsTitled is every tab of this shed carrying exactly this title.
//
// Exact, never case-folded or trimmed: a tab title is a string roost stores
// verbatim, `shed attach -S <name>` writes it verbatim, and a kill that
// matched `Default` against `default` would close a tab the user did not name.
func tabsTitled(tabs []roostprovider.Tab, title string) []roostprovider.Tab {
	var out []roostprovider.Tab
	for _, tab := range tabs {
		if tab.Title == title {
			out = append(out, tab)
		}
	}
	return out
}

// closeTab closes one roost tab and prints the outcome.
func (r *roostSessions) closeTab(ctx context.Context, shed roostprovider.RunningShed, tab roostprovider.Tab) error {
	remote, target, err := roostRemoteFor(r.sshBin, r.controlDir, shed)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, r.perShed)
	defer cancel()
	if err := remote.TabClose(ctx, target, tab.ID); err != nil {
		return fmt.Errorf("failed to close roost tab %s in %s: %w", tab.ID, shed.Name, err)
	}
	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "closed",
			Name:   tab.Title,
			Details: struct {
				Shed   string `json:"shed"`
				TabID  string `json:"tab_id"`
				Source string `json:"source"`
			}{Shed: shed.Name, TabID: tab.ID, Source: sessionSourceRoost},
		})
	}
	printSuccess("Closed roost tab %q (%s) in shed %q", tab.Title, tab.ID, shed.Name)
	return nil
}

// killAmbiguityMessage is the refusal: what matched, and which flag picks one.
//
// **Refusing is the whole point.** `shed attach --new -S default` legitimately
// mints a second tab of a title, and a shed can carry a tmux session and a
// roost tab of the same name at once — so "kill the one called `default`" can
// name two or three different things, and acting on any of them would be a
// coin flip the user cannot see. The ids are printed because `--tab` is
// addressed by id, so the message has to contain the answer to the question it
// asks.
func killAmbiguityMessage(shedName, sessionName string, tabs []roostprovider.Tab, hasTmux bool) string {
	ids := make([]string, 0, len(tabs))
	for _, tab := range tabs {
		ids = append(ids, tab.ID)
	}
	var matched string
	if len(tabs) == 1 {
		matched = fmt.Sprintf("a roost tab titled %q (tab %s)", sessionName, ids[0])
	} else {
		matched = fmt.Sprintf("%d roost tabs titled %q (tabs %s)", len(tabs), sessionName, strings.Join(ids, ", "))
	}
	if hasTmux {
		matched += fmt.Sprintf(" and a tmux session called %q", sessionName)
	}
	remedy := "pass --tab <id> to close a tab"
	if hasTmux {
		remedy += ", or --tmux to kill the tmux session"
	}
	return fmt.Sprintf("shed %q has %s; %s", shedName, matched, remedy)
}
