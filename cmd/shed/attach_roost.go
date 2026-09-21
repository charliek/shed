package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/roostctl"
	"github.com/charliek/shed/internal/roostprovider"
	"github.com/charliek/shed/internal/sshconfig"
)

// The roost-native half of `shed attach` (plan 022 §3.3), under owner decision
// D1: **roost enhances, tmux is the floor.** With a local roost app running,
// attaching opens the shed as a roost tab and the invoking terminal is left
// alone; with no roost — or with `--tmux` / `SHED_ATTACH=tmux` — nothing here
// runs at all and `attachPlain` produces the byte-identical argv
// cmd/shed/testdata/attach_tmux_argv.golden.json has pinned since C3.
//
// Two transports, deliberately: the LOCAL app is driven by exec'ing its own
// `roostctl` (internal/roostctl — see that package's header for why shed does
// not speak the UI socket), and the shed's OWN roost-session is reached over
// SSH through internal/roostprovider, the same transport `shed roost-provider`
// uses. Nothing in this file talks to a socket directly.

const (
	// defaultRoostConnectPoll / defaultRoostConnectCap bound §3.3 step 5's
	// generation fence: how often the saved host's status is re-read after a
	// `host connect`, and how long that is allowed to go on. 20s is generous
	// for an ssh handshake plus a session start on the far side, and short
	// enough that a wedged connect becomes a message rather than a hang.
	defaultRoostConnectPoll = 250 * time.Millisecond
	defaultRoostConnectCap  = 20 * time.Second

	// defaultRoostConnectSettle is how long a `disconnected`-with-a-reason
	// status has to PERSIST before the fence calls it a failure.
	//
	// **Measured, not chosen** (artifact
	// ~/.claude/plans/shed/022-roost-native-cli-and-s6/live-09-generation-fence.txt,
	// recorded against the live rig — local roost-iced at the pinned rev
	// connected to a shed running a protocol-6 roost-session). roost bumps
	// `generation` when the connect ATTEMPT starts, not when it lands, and
	// the attempt takes ~500 ms while the first poll lands at 250 ms:
	//
	//	baseline generation = 2
	//	  250ms  gen=3 state=disconnected   <-- reason is the PREVIOUS attempt's
	//	  500ms  gen=3 state=connected
	//
	// So on the ordinary SUCCESS path the first post-connect status is
	// `disconnected` at the new generation, carrying stale reason text. A
	// fence that treated "not connecting" as settled would fail ~250 ms
	// before the connect succeeded, quoting a reason that no longer applies —
	// in the recorded trace, "has no roost session running" about a host that
	// was about to connect fine.
	//
	// A GENUINE failure holds that same row unchanged for 24 s+, so
	// persistence — not the state word — is what separates the two. 2 s is
	// eight polls: far past the ~500 ms a real connect needs, and still fast
	// enough that a real failure is reported in about two seconds instead of
	// at the 20 s cap.
	defaultRoostConnectSettle = 2 * time.Second

	// defaultRoostSidebarPoll / defaultRoostSidebarCap bound §3.3 step 7's
	// wait for the UI mirror to list the tab that was just opened on the far
	// side. This is a local render catching up with a round trip that has
	// already happened, so the cap is much shorter than the connect's.
	defaultRoostSidebarPoll = 250 * time.Millisecond
	defaultRoostSidebarCap  = 5 * time.Second

	// defaultRoostRemoteCap bounds §3.3 step 6's REMOTE half as a whole — the
	// probe, the `tab.list` and the `tab.open` together, not one each.
	//
	// **ssh's ConnectTimeout is not this bound.** roostprovider's argv sets
	// `-o ConnectTimeout=…`, and that bounds the HANDSHAKE only: a connection
	// that lands and then meets a `roost-session` bridge which never answers
	// waits forever, because a `context.Background()` gives os/exec nothing to
	// kill the child with. The tmux floor's willingness to hang is not a
	// precedent for that — the floor hangs INSIDE an interactive session the
	// user is watching and can Ctrl-C, whereas this path is supposed to return
	// promptly and print one line.
	//
	// One budget for the whole half rather than one per call, because the
	// three calls share a single ControlMaster connection and are one logical
	// round trip: a probe that has already eaten 25 s has spent the attach's
	// patience, and handing `tab.open` a fresh 30 s after it would double the
	// worst case for nothing. 30 s is three times the LOCAL app's own per-exec
	// ceiling (roostctl.ExecTimeout, 10 s), which is the right proportion for
	// the half that has a network and a far-side process start in it.
	defaultRoostRemoteCap = 30 * time.Second
)

// aliasAmbiguityBudget bounds step 3's "does another server have a shed of
// this name" question — ALL of it, however many servers are configured. See
// shedNameIsAmbiguous for why it is not the API client's own 30 s ceiling. Two
// seconds is roostprovider.DefaultPerServerTimeout's value, chosen there for
// the same reason: one unreachable server must not spend a budget that belongs
// to the user.
//
// A var rather than a const SOLELY so its own test can shorten it — proving a
// ceiling fires takes a run that reaches it, and a two-second unit test is a
// test nobody runs. Nothing in production writes to it. (Same reasoning, and
// the same wording, as roostctl.ExecTimeout's.)
var aliasAmbiguityBudget = 2 * time.Second

// roostAttach is one `shed attach` run's roost path: the two transports, the
// destinations it writes to, the four poll bounds, and the remote half's
// deadline.
//
// Every field a test needs to move is a field rather than a package-level
// knob, so two tests can run concurrently with different fixtures — the shim
// is on PATH per-test (t.Setenv), and the ssh binary, the ssh config file and
// the bounds travel in here.
type roostAttach struct {
	// ctl drives the LOCAL roost app. Never nil.
	ctl *roostctl.Client
	// sshBin is the ssh binary the provider transport execs. Empty means
	// resolve it at use (roostprovider.ResolveSSH).
	sshBin string
	// controlDir enables the provider transport's ControlMaster. Empty
	// disables muxing, which is correct but pays a handshake per call.
	controlDir string
	// sshConfigPath is the config file the alias is added to.
	sshConfigPath string
	// out carries the two lines this path prints on success; errOut carries
	// the warnings that are not failures.
	out    io.Writer
	errOut io.Writer

	connectPoll   time.Duration
	connectCap    time.Duration
	connectSettle time.Duration
	sidebarPoll   time.Duration
	sidebarCap    time.Duration
	// remoteCap bounds the provider (ssh) half of step 6 as a whole. Unlike
	// the four poll bounds above it is spent on REAL time rather than on the
	// clock seam — it bounds a network call, not a poll, so a fake clock that
	// shortened it would be asserting something about os/exec.
	remoteCap time.Duration

	// now and sleep are the clock. Production wires them to time.Now and
	// time.Sleep; a test hands both to one fake clock whose sleep advances
	// its own "now", so a 20 s cap and a 2 s settle window are asserted
	// exactly and cost nothing to run.
	now   func() time.Time
	sleep func(time.Duration)
}

// newRoostAttach builds the production roost path. A var so a test can hand
// attachShed a rig-backed one; never reassigned outside tests.
var newRoostAttach = func() *roostAttach {
	return &roostAttach{
		ctl:           &roostctl.Client{},
		controlDir:    roostprovider.ControlDirFor(os.Getenv, os.Getuid()),
		sshConfigPath: sshconfig.GetSSHConfigPath(),
		out:           os.Stdout,
		errOut:        os.Stderr,
		connectPoll:   defaultRoostConnectPoll,
		connectCap:    defaultRoostConnectCap,
		connectSettle: defaultRoostConnectSettle,
		sidebarPoll:   defaultRoostSidebarPoll,
		sidebarCap:    defaultRoostSidebarCap,
		remoteCap:     defaultRoostRemoteCap,
		now:           time.Now,
		sleep:         time.Sleep,
	}
}

// wantsTmuxFloor reports whether the user asked for the tmux floor
// explicitly: that command's own `--tmux`, or `SHED_ATTACH=tmux` in the
// environment.
//
// The env var is the form that survives being set once in a shell profile or
// a tmux config, which is the whole point — a person who lives in tmux sets it
// and never thinks about roost again. Any OTHER value of SHED_ATTACH means
// nothing and is ignored: this is not a mode selector with a "roost" spelling,
// it is one escape hatch.
//
// ONE function for every roost-native command (`attach`, `sessions`,
// `sessions kill`), taking that command's flag: the env var is a single
// promise about this machine, and two copies of this rule would be two ways
// for `SHED_ATTACH` to mean something slightly different.
func wantsTmuxFloor(flag bool) bool {
	return flag || strings.EqualFold(strings.TrimSpace(os.Getenv("SHED_ATTACH")), "tmux")
}

// attachWantsTmux is `shed attach`'s reading of the floor gate.
func attachWantsTmux() bool { return wantsTmuxFloor(attachTmuxFlag) }

// attachShed is §3.3 step 2, the gate: the roost path iff a local roost app is
// reachable through roostctl and the tmux floor was not asked for.
//
// **Every negative is the floor, and none of them is an error.** No roostctl
// on PATH, no app running, an app that answers with something unreadable, an
// app that cannot serve the host family, a roostctl that hangs — a machine
// with no roost is the ordinary case, and it must attach exactly as it always
// has. That is why Available returns a bool and this function has no error
// branch above attachPlain.
//
// The UI protocol version is NOT compared to anything. It is the UI socket's
// protocol, not the session protocol the shed's own roost speaks
// (roostprovider.SpokenProtocol), and gating on it would refuse a perfectly
// good local app over a number describing a different wire.
func attachShed(name, serverName string, entry *config.ServerEntry, shed *config.Shed) error {
	if attachWantsTmux() {
		return attachPlain(name, serverName, entry, shed)
	}
	ctx := context.Background()
	roost := newRoostAttach()
	if !roost.ctl.Available(ctx) {
		return attachPlain(name, serverName, entry, shed)
	}
	return roost.attach(ctx, name, serverName, entry, shed, attachSessionFlag, attachNewFlag)
}

// attach runs §3.3 steps 3-8 against an already-running shed.
//
// title is `-S/--session` (the roost TAB TITLE on this path, the tmux session
// name on the other); forceNew is `--new`.
func (a *roostAttach) attach(ctx context.Context, name, serverName string, entry *config.ServerEntry, shed *config.Shed, title string, forceNew bool) error {
	alias := a.aliasFor(ctx, name, serverName) // step 3, naming
	if err := a.ensureSSHAlias(alias, name, entry); err != nil {
		return err
	}
	host, err := a.ensureSavedHost(ctx, alias) // step 4
	if err != nil {
		return err
	}
	if err := a.connectHost(ctx, alias, host.ID); err != nil { // step 5
		return err
	}
	tabID, err := a.ensureTab(ctx, name, entry, shed, title, forceNew) // step 6
	if err != nil {
		return err
	}
	if err := a.focusTab(ctx, alias, host.ID, tabID, title); err != nil { // step 7
		return err
	}
	// Step 8. The invoking terminal never becomes the session — this line and
	// a zero exit are the whole of what the shell sees.
	fmt.Fprintf(a.out, "attached %s › %s in roost\n", alias, title)
	return nil
}

// aliasFor is §3.3 step 3's naming rule: `shed-<name>`, unless another
// configured server also has a shed of that name, in which case BOTH the ssh
// alias and the roost label become `shed-<server>-<name>`.
//
// The short form is kept only when the name is globally unambiguous, because
// both things it names are global namespaces: `~/.ssh/config` has one `Host
// shed-web` for the whole machine, and roost rejects a duplicate label
// outright. `generateEntries` (the `shed ssh-config install` path) still
// ignores the server entirely, so two servers with a same-named shed already
// collide there; this path is the one that has to not make that worse.
//
// **The alias this shed ALREADY has wins, and nothing is probed.** The
// ambiguity probe is a network question under a two-second budget, so its
// answer is not stable across runs: an attach whose probe timed out mints
// `shed-web`, and the next attach — same shed, same machine, a fast answer
// this time — would mint `shed-mini3-web`, leaving two `Host` entries and two
// saved roost hosts for one shed that nothing ever collapses. Reading back the
// name this shed is already known by makes the FIRST run's answer stick,
// whatever it was, which is the property that matters: an alias is an identity,
// not a fact about the fleet.
//
// **Why not "fail toward the long alias on timeout" instead.** That makes the
// probe's answer safe in one direction but not stable: any configured server
// that is briefly slow — a VPN reconnecting, a laptop asleep — would qualify
// every alias minted while it was, so a machine with one flaky server would
// end up with long aliases forever, for sheds whose names were never ambiguous
// at all. Stability is the fix; reliability of the probe is not available.
//
// Now a method rather than a free function, because "what is this shed already
// called" is a question about the two places this flow writes to — the ssh
// config it was handed, and the local app's saved hosts.
func (a *roostAttach) aliasFor(ctx context.Context, name, serverName string) string {
	short := "shed-" + name
	qualified := "shed-" + serverName + "-" + name
	if existing, ok := a.existingAlias(ctx, qualified, short); ok {
		return existing
	}
	if shedNameIsAmbiguous(name, serverName) {
		return qualified
	}
	return short
}

// existingAlias returns the first candidate this machine already knows, in the
// order given.
//
// **Qualified before short**, which is the order aliasFor passes them in: a
// `shed-mini3-web` can only ever have been minted for mini3's `web`, whereas a
// bare `shed-web` may well belong to a same-named shed on another server (that
// is precisely the collision the qualified form exists to resolve). If both
// are present the machine is already carrying the duplication this fix
// prevents, and the unambiguous one is the better of the two to settle on.
//
// Two places are asked, in cost order. The ssh config is a local file read and
// is also the STRICTER signal: every attach writes the `Host` entry (step 3)
// before it saves the roost host (step 4), so a run that got far enough to save
// a host necessarily wrote the entry first. The saved hosts are asked second
// and only cover the case where the user has since removed the ssh entry by
// hand.
//
// Every failure here is a miss, never an error. This decides how a name is
// SPELLED; an unreadable ssh config or a roost that will not list its hosts is
// reported a moment later by the steps that actually need them
// (ensureSSHAlias, ensureSavedHost), and turning it into a failure here would
// only report it twice.
func (a *roostAttach) existingAlias(ctx context.Context, candidates ...string) (string, bool) {
	for _, candidate := range candidates {
		if declared, err := sshconfig.AliasDeclared(a.sshConfigPath, candidate); err == nil && declared {
			return candidate, true
		}
	}
	hosts, err := a.ctl.HostList(ctx)
	if err != nil {
		return "", false
	}
	for _, candidate := range candidates {
		for _, host := range hosts {
			if host.Target == candidate {
				return candidate, true
			}
		}
	}
	return "", false
}

// shedNameIsAmbiguous reports whether a server OTHER than serverName also has
// a shed called name.
//
// A server that cannot be reached IN TIME answers nothing and so makes nothing
// ambiguous — the short form is kept. That is the right failure direction: a
// qualified alias minted because a VPN was down would be a second `Host` entry
// and a second saved roost host for the same shed, which nothing later
// collapses. The cost of the other direction is one collision the user can see
// and fix; the cost of this one is silent duplication.
//
// **Fanned out, and bounded as a whole.** The API client's own ceiling is 30 s,
// which is the right number for a command the user asked for and entirely the
// wrong one for a question that only decides how an alias is SPELLED: one
// configured server behind a dead VPN would otherwise add half a minute to
// every attach. So the probes run concurrently against a budget that bounds
// them all together, the same shape (and for the same reason) as
// roostprovider.Inventory's per-server fan-out. A worker that answers after
// the budget sends into a buffered channel and exits; nothing waits for it.
//
// Real time, not roostAttach's clock seam: this bounds a NETWORK call, not a
// poll, and a test that shortened it would be testing the http client.
//
// **The whole map is snapshotted under configMu BEFORE the first worker
// starts.** A worker builds an API client, and building one can re-mint a
// near-expiry credential and write it straight back into
// `clientConfig.Servers` (updateClientConfig, which holds configMu and applies
// the mutation to this very map). A launching loop that read
// `clientConfig.Servers[otherName]` as it spawned was doing an unsynchronised
// map READ concurrently with that write — which in Go is not a stale value but
// a fatal "concurrent map read and map write" that takes `shed` down. Copying
// every entry out first, under the lock every other reader in this package
// takes, means each worker is handed its own value and nothing touches the
// shared map once a goroutine exists.
//
// The lock is released before anything is spawned: the workers themselves take
// configMu (verifiedServerName does), so holding it across the fan-out would
// deadlock the first one.
func shedNameIsAmbiguous(name, serverName string) bool {
	type probeTarget struct {
		name  string
		entry config.ServerEntry
	}
	configMu.Lock()
	others := make([]probeTarget, 0, len(clientConfig.Servers))
	for otherName, entry := range clientConfig.Servers {
		if otherName != serverName {
			others = append(others, probeTarget{name: otherName, entry: entry})
		}
	}
	configMu.Unlock()
	if len(others) == 0 {
		return false
	}

	// Buffered to exactly len(others) so a late worker can always send.
	answers := make(chan bool, len(others))
	for _, other := range others {
		// The entry is the snapshot's COPY rather than a pointer into the
		// map: clientConfig is a map this process can reload, and a worker
		// that outlives this call must not be reading it.
		go func(otherName string, entry config.ServerEntry) {
			client := NewAPIClientFromNamedEntry(otherName, &entry, DefaultTimeout)
			_, err := client.GetShed(name)
			answers <- err == nil
		}(other.name, other.entry)
	}

	budget := time.After(aliasAmbiguityBudget)
	for range others {
		select {
		case found := <-answers:
			if found {
				return true
			}
		case <-budget:
			return false
		}
	}
	return false
}

// ensureSSHAlias is §3.3 step 3's second half: make sure `ssh <alias>` reaches
// this shed, without ever touching an alias that already exists.
//
// roost connects to a saved host by handing the target string to ssh, so the
// alias has to resolve in `~/.ssh/config` — and it is the ONE thing this flow
// writes to the user's own files. An existing `Host <alias>`, managed or
// hand-written, is left exactly as it is: a user who wrote their own entry
// with a ProxyJump or a different IdentityFile meant it, and a shed whose
// alias was already written by an earlier attach needs nothing done.
//
// The entry's shape is `generateEntries`', including its omissions: no
// IdentityFile, so the default key and the ssh agent decide — documented in
// the reference docs rather than special-cased here.
func (a *roostAttach) ensureSSHAlias(alias, shedName string, entry *config.ServerEntry) error {
	outcome, err := sshconfig.AddEntryIfAbsent(a.sshConfigPath, sshconfig.Entry{
		Name:           alias,
		Host:           entry.Host,
		Port:           entry.SSHPort,
		User:           shedName,
		KnownHostsFile: config.GetKnownHostsPath(),
	})
	if err != nil {
		return fmt.Errorf("failed to add Host %s to %s: %w", alias, tildePath(a.sshConfigPath), err)
	}
	if outcome == sshconfig.AddedEntry {
		fmt.Fprintf(a.out, "wrote Host %s to %s\n", alias, tildePath(a.sshConfigPath))
	}
	return nil
}

// tildePath renders a path under the user's home as `~/…`, so the line this
// prints reads the way the user would type it. Anything else is printed
// verbatim.
func tildePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "~" + string(filepath.Separator) + rel
	}
	return path
}

// ensureSavedHost is §3.3 step 4: find the saved roost host for this alias, or
// save one.
//
// **Matched on TARGET, and only on target.** The target is what the host
// actually reaches; a label is a display name the user can rename in roost's
// own dialog. Matching the label first would mint a duplicate host for a shed
// the user had merely renamed, and worse, one pointing at the same target.
//
// **A label that matches while the target does NOT is a refusal, not a
// fallback.** A saved host labelled `shed-web` but targeting a different
// machine is somebody else's host that happens to share a display name, and
// connecting it would connect the wrong machine — after which step 6 still
// opens the tab on the real shed over its own direct ssh, and step 7 goes
// looking for that tab id under the wrong host's section, where a tab of the
// same numeric id may well exist. The result is `attached …` and exit 0 with a
// stranger's tab focused. Adding a second host under the same label is not the
// alternative either: roost rejects duplicate labels case-insensitively, so
// the add would fail with roost's own refusal and no explanation of which
// existing row caused it. Naming both the label and the target roost has is
// the only outcome the user can act on — hence the case-insensitive compare
// here, which is roost's own rule for when two labels collide.
//
// `host add` runs WITHOUT `--verify`: verification asks the far side for a
// `session.identify` before saving, and a shed that has just started may have
// no roost-session listening yet — which step 5's connect is what starts, and
// step 5 is also where that failure gets its own message. Refusing to save the
// host here would turn a recoverable state into a dead end.
//
// Two `shed attach`es racing here produce one duplicate saved host at worst
// (both read an empty list, both add). That is documented, not handled: roost
// has no compare-and-swap on the registry, the duplicate is visible and
// removable in the app, and the alternative — a local lock file — would be a
// second lock protecting somebody else's state.
func (a *roostAttach) ensureSavedHost(ctx context.Context, alias string) (roostctl.Host, error) {
	hosts, err := a.ctl.HostList(ctx)
	if err != nil {
		return roostctl.Host{}, fmt.Errorf("listing roost's saved hosts: %w", err)
	}
	for _, host := range hosts {
		if host.Target == alias {
			return host.Host, nil
		}
	}
	for _, host := range hosts {
		if strings.EqualFold(host.Label, alias) {
			return roostctl.Host{}, fmt.Errorf(
				"roost already has a saved host labelled %q, and it targets %q rather than %q; "+
					"rename or repoint that host in roost (or remove it), then attach again",
				host.Label, host.Target, alias)
		}
	}
	host, err := a.ctl.HostAdd(ctx, alias, alias, false)
	if err != nil {
		return roostctl.Host{}, fmt.Errorf("saving %s as a roost host: %w", alias, err)
	}
	return host, nil
}

// connectHost is §3.3 step 5, the generation fence, as amended by the live
// measurement in live-09-generation-fence.txt.
//
// **Why a generation fence at all.** `host connect` returns once the attempt
// is UNDER WAY, so the settled answer has to be polled for — and the obvious
// poll ("read status until it says connected") is wrong in a way that is
// invisible in the happy case: the status read right after the connect can
// still describe the PREVIOUS attempt. A host that was connected a moment ago
// and dropped answers `connected` from the old generation, and a state-only
// poll ends the wait there and opens a tab on a connection that is gone.
// roost's own `HostStatus.generation` doc calls it "the monotonic edge a
// poller waits on" for exactly this reason: it counts attempts STARTED, so a
// status whose generation still equals the baseline has nothing to say about
// the attempt this function just began. A baseline of 0 is a real baseline —
// it is what a host that has never connected reports — so the comparison is
// `>`, never "is there any data".
//
// **Why the generation alone is not enough.** It is bumped by the attempt, not
// by the outcome, and the attempt outlives the bump by a few hundred
// milliseconds. Measured: at the first 250 ms poll the new generation is
// already there with `state=disconnected` and the PREVIOUS attempt's reason
// still attached; `connected` arrives at 500 ms. Reading "not connecting" as
// settled therefore fails the ORDINARY success path, quoting a stale reason. A
// genuine failure holds the identical row for 24 s+, so the discriminator is
// persistence: a `disconnected`-with-a-reason row is PROVISIONAL until it has
// survived connectSettle.
//
// `stopped` and `needs-restart` are not provisional. They are roost saying the
// host will not come up without being asked again, and no amount of waiting
// changes them.
func (a *roostAttach) connectHost(ctx context.Context, alias, id string) error {
	status, err := a.hostStatus(ctx, id)
	if err != nil {
		return err
	}
	// Already connected: nothing to wait for, and connecting again would
	// displace nothing but would burn a handshake.
	if status.State == roostctl.StateConnected {
		return nil
	}
	baseline := status.Generation

	if _, err := a.ctl.HostConnect(ctx, id); err != nil {
		return fmt.Errorf("connecting roost to %s: %w", alias, err)
	}

	deadline := a.now().Add(a.connectCap)
	// **The cap is a deadline on the calls, not only a check between them.**
	// Each `host status` can itself block for roostctl.ExecTimeout (10 s), so
	// a loop that only compared the clock AFTER a call returned advertised
	// 20 s and could take 30: nineteen seconds of polling, then one wedged
	// call that runs the ceiling out on its own. Deriving the polling context
	// from the same instant means no individual read can outlive the budget,
	// and the cap message fires AT the cap.
	//
	// Real time here, deliberately, while `deadline` is on the clock seam:
	// both say "connectCap from now", and in production they are the same
	// instant. A fake clock simply reaches its own deadline first, which is
	// what makes the cap assertable without a test that waits 20 s.
	pollCtx, cancelPoll := context.WithTimeout(ctx, a.connectCap)
	defer cancelPoll()

	// The provisional failure being watched, and when it was first seen. The
	// key is (generation, state, reason): any change at all is a DIFFERENT
	// observation and restarts the window, because "this exact row has not
	// moved for two seconds" is the whole claim being made.
	var provisionalKey string
	var provisionalSince time.Time

	for {
		a.sleep(a.connectPoll)
		status, err := a.hostStatus(pollCtx, id)
		if err != nil {
			// A read that failed BECAUSE the budget ran out is the cap, not a
			// roostctl fault: the user gets the cap's own message and its
			// remedy rather than an exec error naming a timeout they never
			// set.
			if pollCtx.Err() != nil {
				return connectCapError(alias, id)
			}
			return err
		}
		if status.Generation > baseline {
			switch status.State {
			case roostctl.StateConnected:
				return nil
			case roostctl.StateStopped, roostctl.StateNeedsRestart:
				return errors.New(connectFailureMessage(alias, status))
			case roostctl.StateDisconnected:
				// Disconnected with nothing to say is not a diagnosis. It is
				// what the gap between "the attempt started" and "the
				// transport came up" looks like, so it waits like any other
				// in-flight state.
				if status.Reason == "" {
					provisionalKey, provisionalSince = "", time.Time{}
				} else {
					key := fmt.Sprintf("%d\x00%s\x00%s", status.Generation, status.State, status.Reason)
					if key != provisionalKey {
						provisionalKey, provisionalSince = key, a.now()
					}
					if a.now().Sub(provisionalSince) >= a.connectSettle {
						return errors.New(connectFailureMessage(alias, status))
					}
				}
			default:
				// `connecting`: this attempt, still in flight.
				provisionalKey, provisionalSince = "", time.Time{}
			}
		}
		if !a.now().Before(deadline) {
			return connectCapError(alias, id)
		}
	}
}

// connectCapError is the one message the connect cap produces, from either of
// the two places that can reach it — the between-polls clock check and a read
// the budget cut short. One function so the two cannot drift into two
// different sentences for the same outcome.
func connectCapError(alias, id string) error {
	return fmt.Errorf("still connecting to %s; check roostctl host status --id %s or retry", alias, id)
}

// hostStatus reads one saved host's live status.
//
// The row is matched by id rather than taken as "the first row": `host status
// --id` narrows to one host, and a roost that answered with something else
// would otherwise have its answer read as this host's.
func (a *roostAttach) hostStatus(ctx context.Context, id string) (roostctl.HostStatus, error) {
	rows, err := a.ctl.HostStatus(ctx, id)
	if err != nil {
		return roostctl.HostStatus{}, fmt.Errorf("reading roost's status for host %s: %w", id, err)
	}
	for _, row := range rows {
		if row.ID == id {
			return row, nil
		}
	}
	// An empty list is how roost says "no such host" — it answers the op
	// rather than refusing it — so this is the shape a host removed in the
	// app mid-attach arrives as.
	return roostctl.HostStatus{}, fmt.Errorf("roost no longer knows the saved host %s", id)
}

// noRoostSessionReasons are the substrings that mean the far side has no
// roost-session to attach to, as opposed to any other connection failure.
//
// Read out of roost's own classifier (roost-ipc/src/ssh.rs
// `SshFailure`/`classify_ssh_failure`, lines 609-690 at the pinned rev). They
// are matched as SUBSTRINGS of `reason` because `reason` is a sentence roost
// composes, not a code — roost's codes live on `host.*` refusals, and a
// connection that failed carries prose.
//
// The two shapes that reach us are NOT interchangeable, and which one it is
// decides how the message is built (see connectFailureMessage):
//
//   - `NoSession` — "<target> is reachable but has no roost session running.
//     Run `roostctl session start` on that machine, then try again."
//   - `NotFound`  — "roost-session isn't installed on <target> (or isn't on
//     the non-interactive PATH ssh uses there) — connect from the Roost app
//     to install it."
//
// `command not found` is kept for a different reason than the other two: it
// never appears in a rendered `reason` at all. roost matches it (and exit 127)
// in ssh's STDERR and then replaces it with the `NotFound` sentence above, so
// by the time shed sees `reason` the phrase is gone. It stays in this set
// because a `Transport(line)` reason passes ssh's stderr through verbatim and
// could still carry it.
//
// **There is no "no session" STATE to check instead.** roost's five host
// states (disconnected/connecting/connected/stopped/needs-restart) cannot tell
// "nothing is listening over there" from "the network is down"; the reason
// string is the only place that distinction exists.
var noRoostSessionReasons = []string{
	"no roost session running",
	"isn't installed on",
	"command not found",
}

// roostOwnRemedyReasons are the no-session reasons whose text roost has
// deliberately authored a remedy into, and which shed therefore passes through
// rather than replacing.
//
// `NotFound` is the case: roost's own comment beside it calls the remedy
// "deliberately one sentence and one place", and the sentence names the
// **non-interactive PATH** — which is the actual cause often enough that
// throwing it away for a generic message would cost a user real time. shed
// adds only what roost's copy omits and owner decision D4 requires: the shed
// desktop as a second door.
var roostOwnRemedyReasons = []string{
	"isn't installed on",
}

// connectFailureMessage turns a settled, not-connected status into the line
// the user sees.
//
// One case is rewritten and the rest are passed through. "There is no
// roost-session on that shed" is the one failure with a REMEDY shed knows
// (roost's own copy points at `roostctl session start` on the far side, which
// is not how a shed gets one), so it gets shed's own sentence. Everything else
// — a changed host key, a refused connection, an auth failure — is roost's
// diagnosis, and rewording it here would only put shed's vocabulary between
// the user and the tool that actually knows what happened.
//
// NOTE: roost's `NotFound` copy ("roost-session isn't installed on <target>…")
// does not itself contain either substring above, so a reason worded that way
// is printed verbatim. That is deliberate and it is fine: that sentence is
// already actionable. The substrings match the two shapes the fence actually
// sees — the NoSession sentence, and the exec chain's raw `command not found`.
func connectFailureMessage(alias string, status roostctl.HostStatus) string {
	for _, fragment := range roostOwnRemedyReasons {
		if strings.Contains(status.Reason, fragment) {
			// roost already said what to do, and said it better; add only the
			// door it does not know about.
			return status.Reason + "\nOr connect to it from the shed desktop."
		}
	}
	for _, fragment := range noRoostSessionReasons {
		if strings.Contains(status.Reason, fragment) {
			return fmt.Sprintf(
				"%s has no roost-session. In roost: Cmd/Alt-Shift-P → %q installs and starts one. "+
					"Or connect to it from the shed desktop.",
				alias, "Connect Host: "+alias)
		}
	}
	switch {
	case status.Reason != "" && status.Detail != "":
		return status.Reason + "\n" + status.Detail
	case status.Reason != "":
		return status.Reason
	default:
		// roost settled without saying why. Naming the state is all there is,
		// and it still beats an empty error line.
		return fmt.Sprintf("roost could not connect to %s (state %q)", alias, status.State)
	}
}

// ensureTab is §3.3 step 6: reuse the shed's tab of this title, or open one.
//
// This half runs over the PROVIDER transport — ssh to the shed's own
// roost-session — not through the local app: `tab.open` on the UI socket is
// local-only, and the tab being opened belongs to the far side's session.
//
// The reuse rule is by TITLE AND CWD (see reusableTab), which is what makes
// `shed attach web` twice land in the same tab. A tab the user has renamed in
// roost no longer matches and yields a new tab; that is documented rather than
// defended against, because the alternative is shed writing a marker into
// somebody else's session state.
func (a *roostAttach) ensureTab(ctx context.Context, name string, entry *config.ServerEntry, shed *config.Shed, title string, forceNew bool) (string, error) {
	remote, target, err := a.remoteFor(name, entry)
	if err != nil {
		return "", err
	}

	// **One deadline over the whole remote half** — the probe, the list and
	// the open together. Without it these three run under a
	// context.Background() that can never cancel them, and a far side whose
	// bridge connects and then goes quiet hangs `shed attach` outright. See
	// defaultRoostRemoteCap for why the budget is shared rather than per-call.
	ctx, cancel := context.WithTimeout(ctx, a.remoteCap)
	defer cancel()

	// The probe is what makes the cwd real: `tab.open` stores a non-empty cwd
	// VERBATIM and hands it to the PTY unexpanded, so a landing dir that does
	// not exist on the far side is a tab that dies on open. Home is the
	// fallback, and it is the far side's absolute $HOME, never `~`.
	probe, err := remote.Probe(ctx, target, shed.LandingDir)
	if err != nil {
		return "", fmt.Errorf("probing %s: %w", name, err)
	}
	cwd := probe.Home
	if shed.LandingDir != "" && probe.LandingDirExists {
		cwd = shed.LandingDir
	}

	if !forceNew {
		projects, err := remote.TabList(ctx, target)
		if err != nil {
			return "", fmt.Errorf("listing %s's roost tabs: %w", name, err)
		}
		if tabID, ok := reusableTab(projects, title, cwd); ok {
			return tabID, nil
		}
	}

	// No argv: the tab runs the shed's own login shell, which is what `shed
	// attach` has always given the user. `[]string{}` and not nil — roost's
	// `TabOpenParams.argv` is a `Vec<String>` whose serde default covers a
	// MISSING key, not a null one, so the nil slice's `"argv":null` would be
	// refused by the far side.
	//
	// Project "0" is roost's "no project named" sentinel: it files the tab
	// under the session's first project (or a fresh `Default`), which is only
	// about grouping — cwd above is explicit and absolute either way.
	tabID, err := remote.TabOpen(ctx, target, roostprovider.TabOpenParams{
		ProjectID: "0",
		Cwd:       cwd,
		Title:     title,
		Argv:      []string{},
	})
	if err != nil {
		return "", fmt.Errorf("opening a roost tab on %s: %w", name, err)
	}

	// LOCK the title, or the next attach opens another tab.
	//
	// A tab opened with a title comes up `user_titled: false`, and the login
	// shell inside it immediately emits an OSC title sequence that overwrites
	// it with the cwd. Measured on a live session: a tab opened as `default`
	// read back as `/home/shed`, so the reuse-by-title check above could never
	// match and a second `shed attach` opened a third tab (live-11). Setting
	// the title explicitly marks it `user_titled: true`, which is what makes
	// it survive.
	//
	// A failure here is NOT fatal: the tab is open and usable, and the only
	// cost is that the next attach will not recognise it. Say so and carry on
	// rather than failing an attach that otherwise worked.
	if err := remote.TabSetTitle(ctx, target, tabID, title); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not title roost tab %s %q on %s: %v\n"+
			"  (the tab is open; a later `shed attach` will not recognise it and will open another)\n",
			tabID, title, name, err)
	}
	return tabID, nil
}

// roostTabFinished is roost's `TabState` spelling for a tab whose shell has
// exited. Every other spelling is a live row; this is the only dead one.
const roostTabFinished = "finished"

// reusableTab picks the tab `shed attach` should land in, out of every tab the
// far side's session is running.
//
// **Title AND cwd, never title alone.** A shed's roost session holds every
// project on that machine, and the default title is `default` — so "the first
// tab titled `default`" is routinely another project's tab, and reusing it
// focuses a shell sitting in somebody else's directory while reporting
// success. Matching the cwd too is what ties the tab to THIS shed's landing
// dir, and the cwd compared is the one this run resolved (the landing dir, or
// the far side's $HOME when the landing dir does not exist over there) — i.e.
// exactly what `tab.open` would have been given, so reuse and open agree.
//
// The project id is deliberately NOT part of the match. shed opens tabs under
// roost's `"0"` sentinel, which files them under whichever project the session
// happens to have first; shed therefore never knows which project id its own
// tab landed in, and a project id is not a stable key from this side. The cwd
// is.
//
// **Tie-break: a live tab always beats a finished one.** Several tabs can
// legitimately share a title and a cwd — `--new` mints them on purpose — and a
// `finished` row is a dead shell, so handing one back would focus a tab the
// user cannot type in. A finished tab is still a candidate rather than being
// skipped outright, because skipping it would open a fresh tab beside the dead
// one on every attach, which is the duplication the title rule exists to
// prevent; it is simply always the last choice. Within each class the far
// side's own order wins (`tab.list` reports projects and tabs in roost's
// order), so repeated attaches land in the same row rather than wandering.
func reusableTab(projects []roostprovider.Project, title, cwd string) (string, bool) {
	var finished string
	for _, project := range projects {
		for _, tab := range project.Tabs {
			if tab.Title != title || tab.Cwd != cwd {
				continue
			}
			if tab.State != roostTabFinished {
				return tab.ID, true
			}
			if finished == "" {
				finished = tab.ID
			}
		}
	}
	return finished, finished != ""
}

// remoteFor builds the provider transport for one shed.
func (a *roostAttach) remoteFor(name string, entry *config.ServerEntry) (*roostprovider.Remote, roostprovider.Target, error) {
	return roostRemoteFor(a.sshBin, a.controlDir, roostprovider.RunningShed{
		Name:          name,
		ServerHost:    entry.Host,
		ServerSSHPort: entry.SSHPort,
	})
}

// roostRemoteFor builds the provider transport — the ssh half — for one shed.
//
// A shed's ssh identity is always `<shed>@<server host> -p <server ssh port>`
// pinned against `~/.shed/known_hosts` — shed mints those host keys itself —
// which is exactly what ShedTarget spells. The `Host <alias>` entry `shed
// attach` writes is for ROOST's ssh, not for this one, which is why nothing
// here reads the ssh config.
//
// A free function rather than a method because both roost-native commands
// need it and neither owns it: `attach` reaches one shed, `sessions` fans out
// over the fleet, and the reach rule is the same one either way. sshBin empty
// means resolve it here (roostprovider.ResolveSSH); controlDir empty disables
// ssh muxing, which is correct but pays a handshake per call.
func roostRemoteFor(sshBin, controlDir string, shed roostprovider.RunningShed) (*roostprovider.Remote, roostprovider.Target, error) {
	bin := sshBin
	if bin == "" {
		resolved, err := roostprovider.ResolveSSH()
		if err != nil {
			return nil, roostprovider.Target{}, err
		}
		bin = resolved
	}
	target := roostprovider.ShedTarget(shed, config.GetKnownHostsPath())
	return &roostprovider.Remote{SSHBin: bin, ControlDir: controlDir}, target, nil
}

// focusTab is §3.3 step 7: wait for the UI mirror to list the tab, focus it,
// and bring roost forward.
//
// **The wait is not politeness, it is the only route to the reference.**
// `tab focus --tab` takes the `h<incarnation>.<id>` spelling, and the
// incarnation is the UI's own per-connection number that appears NOWHERE else
// on the wire — a saved host's opaque string id will not do. So the key has to
// be read out of the sidebar dump, and the sidebar cannot show a tab the
// mirror has not processed yet. Focusing before then answers TabNotFound.
//
// **Focusing is the attach; raising the window is not.** The two halves fail
// differently on purpose. `app.activate` failing means roost stayed behind
// another window — one click away, and the tab is focused underneath — so it
// is a warning. `tab.focus` failing means the thing `shed attach` says it did
// did not happen: the user is looking at whatever tab was already selected,
// and printing `attached …` and exiting 0 over that is a lie the shell has no
// way to notice. So a focus that ultimately fails is exit 1, naming the tab —
// the same treatment, for the same reason, as never finding the key at all.
func (a *roostAttach) focusTab(ctx context.Context, alias, hostID, tabID, title string) error {
	key, err := a.waitForSidebarKey(ctx, alias, hostID, tabID, title)
	if err != nil {
		return err
	}
	if err := a.focusWithRetry(ctx, alias, hostID, tabID, title, key); err != nil {
		return err
	}
	if err := a.ctl.Activate(ctx); err != nil {
		fmt.Fprintf(a.errOut, "warning: roost did not come to the front: %v\n", err)
	}
	return nil
}

// waitForSidebarKey polls the sidebar dump until the host section for hostID
// lists the tab this run opened, and returns that row's key.
func (a *roostAttach) waitForSidebarKey(ctx context.Context, alias, hostID, tabID, title string) (string, error) {
	deadline := a.now().Add(a.sidebarCap)
	for {
		dump, err := a.ctl.SidebarDump(ctx)
		if err != nil {
			return "", fmt.Errorf("reading roost's sidebar: %w", err)
		}
		if key, ok := sidebarTabKey(dump, hostID, tabID); ok {
			return key, nil
		}
		if !a.now().Before(deadline) {
			// The tab is left open and named on purpose: it is a real tab
			// with the user's work about to happen in it, and closing it to
			// make the failure tidy would throw that away. Naming it is what
			// makes the row findable by hand.
			return "", fmt.Errorf(
				"roost has not listed tab %s (%q) under %s yet; the tab is open on the shed — select it in roost's sidebar",
				tabID, title, alias)
		}
		a.sleep(a.sidebarPoll)
	}
}

// sidebarTabKey finds the `h<incarnation>.<id>` key for tabID inside hostID's
// section of a sidebar dump.
func sidebarTabKey(dump roostctl.SidebarDump, hostID, tabID string) (string, bool) {
	if tabID == "" {
		return "", false
	}
	for _, host := range dump.Hosts {
		if host.ID != hostID {
			continue
		}
		for _, project := range host.Projects {
			for _, tab := range project.Tabs {
				// The id is the part after the incarnation: a key with no
				// `.` is not one of these keys at all.
				if _, id, ok := strings.Cut(tab.Key, "."); ok && id == tabID {
					return tab.Key, true
				}
			}
		}
	}
	return "", false
}

// focusWithRetry focuses the tab, retrying ONCE on a TabNotFound — with a key
// read FRESH, never the one that was just refused — and owns the failure
// message, which names the key it actually last asked for.
//
// The sidebar dump is the UI's LAST-RENDERED rows, so a key read out of it can
// disagree with the router that `tab.focus` resolves against — in two ways,
// and only one of them is a frame skew. The other is the incarnation: `h3.5`
// and `h4.5` are the same session-side tab across a reconnect, the dump can
// still be painting the old number, and `tab focus --tab h3.5` is then
// permanently wrong. Resubmitting the identical key would retry a reference
// that cannot start working; re-reading the dump is what picks up `h4.5`, and
// it covers the plain frame skew just as well. One retry, because after a
// fresh read the condition is already supposed to hold — more would be a poll
// for something the first wait has already proved.
func (a *roostAttach) focusWithRetry(ctx context.Context, alias, hostID, tabID, title, key string) error {
	err := a.ctl.TabFocus(ctx, key)
	if err != nil && isTabNotFound(err) {
		a.sleep(a.sidebarPoll)
		fresh, waitErr := a.waitForSidebarKey(ctx, alias, hostID, tabID, title)
		if waitErr != nil {
			// The mirror no longer lists the tab at all. That message already
			// names the tab and says it is open on the shed, so it stands on
			// its own rather than being wrapped in a second copy of itself.
			return waitErr
		}
		key, err = fresh, a.ctl.TabFocus(ctx, fresh)
	}
	if err == nil {
		return nil
	}
	// Named the way the sidebar timeout names it: the tab is real, it is open
	// on the shed, and the row is findable by hand.
	return fmt.Errorf(
		"roost did not focus tab %s (%q) under %s: %w; the tab is open on the shed — select it in roost's sidebar",
		key, title, alias, err)
}

// isTabNotFound reports whether err is roost refusing a tab reference it
// cannot resolve.
//
// Only a *ServerError can be one: that is roost's own refusal envelope. The
// code is roost's `tab_not_found` (roost-engine/src/facade.rs), accepted in
// either separator spelling because the two are trivially confusable and
// getting this wrong only costs a retry.
func isTabNotFound(err error) bool {
	var serverErr *roostctl.ServerError
	if !errors.As(err, &serverErr) {
		return false
	}
	return strings.ReplaceAll(strings.ToLower(serverErr.Code), "-", "_") == "tab_not_found"
}
