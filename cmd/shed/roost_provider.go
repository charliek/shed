package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/roostprovider"
)

// roostProviderCmd implements plan 019 §3.1's roost provider: `shed
// roost-provider list|activate` is the provider roost execs (through the
// launcher `--install` writes), and `--install/--dry-run/--uninstall` manage
// that launcher. See internal/roostprovider for the menu builders, token
// codec, ssh transport and wire client this file wires together — this file
// owns none of that logic itself, only the CLI plumbing and the launcher
// file.
var roostProviderCmd = &cobra.Command{
	Use:   "roost-provider [list|activate]",
	Short: "roost palette provider: start an agent on a shed or machine",
	Long: `Implements roost's provider contract (see roost's docs/guides/extending.md)
so roost's own command palette can start an agent — claude, codex, cursor,
opencode, gx, or grok — on a running shed or a configured machines: entry,
opened as a tab in that host's own roost-session.

Phases (run by roost itself, not typed by a person):

  shed roost-provider list      Print the top-level menu (running sheds and
                                 machines: entries). No ssh; stays inside
                                 roost's 5s default provider timeout.
  shed roost-provider activate  Act on ROOST_SELECTED_ID (env, or the same
                                 key on stdin JSON): probe the chosen host,
                                 list agents, list workdirs, and open a tab.
                                 Recursive — each drill-down step is another
                                 activate call.

The phase can also come from $ROOST_PROVIDER_PHASE, which is how roost
actually invokes this (the launcher forwards "$@" as $1 too, so either works).

Installation writes the launcher script roost discovers as a provider:

  shed roost-provider --install            Write the launcher
  shed roost-provider --install --dry-run  Preview without writing
  shed roost-provider --uninstall          Remove it (only if shed wrote it)

The launcher goes to <dir>/providers/shed, where <dir> is the parent of a
non-empty $ROOST_CONFIG (a file path), else $HOME/.config/roost — roost does
not consult $XDG_CONFIG_HOME, so this does not either. Re-run --install
whenever the shed binary moves (a Homebrew upgrade, a rebuild elsewhere):
the launcher pins the binary's resolved absolute path.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRoostProvider,
}

var (
	roostProviderInstall   bool
	roostProviderDryRun    bool
	roostProviderUninstall bool
)

func init() {
	roostProviderCmd.Flags().BoolVar(&roostProviderInstall, "install", false, "Write the roost provider launcher script")
	roostProviderCmd.Flags().BoolVar(&roostProviderDryRun, "dry-run", false, "Preview --install/--uninstall without writing")
	roostProviderCmd.Flags().BoolVar(&roostProviderUninstall, "uninstall", false, "Remove the roost provider launcher script")

	rootCmd.AddCommand(roostProviderCmd)
}

// Budgets.
//
// **Both of them must fit INSIDE roost's own provider timeout, which defaults
// to 5s** (raisable per provider in roost's config form, plan 019 §9). roost
// kills a provider that overruns it — so an internal budget longer than
// roost's is not "more room", it is a guarantee that the pinned row this side
// was about to print never reaches the palette, which breaks §3.2's whole
// "every expected state exits zero with a row" contract. 4s leaves a second
// of headroom to serialize a menu and flush it.
//
// `list` does no ssh and one bounded HTTP fan-out
// (internal/roostprovider.Inventory already caps each server at
// DefaultPerServerTimeout), so its 4s is a backstop under a hang that manages
// to slip past that per-server bound rather than the expected budget.
// `activate` runs several SEQUENTIAL round trips (an Inventory re-check for a
// shed, the probe, session.identify, tab.list, and sometimes tab.open) rather
// than list's one fan-out, so it is the phase that can genuinely want more
// than roost's default — and the way to get it is to raise BOTH numbers:
// `timeout=` on the provider in roost's config form, and
// $SHED_ROOST_PROVIDER_TIMEOUT here to match. Raising only this one just moves
// the kill; raising only roost's leaves this side giving up early.
const (
	roostProviderListBudget     = 4 * time.Second
	roostProviderActivateBudget = 4 * time.Second
)

// roostProviderTimeoutEnv overrides both budgets above, for an operator who
// raised the provider's own `timeout=` in roost's config form (plan 019 §9).
// A Go duration string ("8s", "1m500ms").
const roostProviderTimeoutEnv = "SHED_ROOST_PROVIDER_TIMEOUT"

// roostProviderBudget applies $SHED_ROOST_PROVIDER_TIMEOUT to a phase's
// default.
func roostProviderBudget(def time.Duration) time.Duration {
	return roostProviderBudgetFrom(os.Getenv(roostProviderTimeoutEnv), def, os.Stderr)
}

// roostProviderBudgetFrom is roostProviderBudget's injectable core.
//
// An unparseable or non-positive value falls back to the default rather than
// failing the phase: this is an environment variable read inside somebody
// else's process, and refusing to answer a palette because a stale export is
// malformed would be a worse outcome than answering on the default. The
// warning goes to STDERR, never stdout — stdout is the one JSON object §3.2
// pins, and roost parses it.
func roostProviderBudgetFrom(raw string, def time.Duration, warn io.Writer) time.Duration {
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		fmt.Fprintf(warn, "shed roost-provider: ignoring $%s=%q (want a positive Go duration, e.g. \"8s\"); using %s\n",
			roostProviderTimeoutEnv, raw, def)
		return def
	}
	return d
}

// maxRoostSelectedIDStdinBytes bounds the stdin fallback read in selectedID.
// roost's own stdin payload (plan 019 §"Provider contract": `{"v":1,"phase",
// "query","selected_id?","active_tab":{…},"socket"}`) is at most a few hundred
// bytes; this is generous headroom without being an unbounded read.
const maxRoostSelectedIDStdinBytes = 64 << 10

// runRoostProvider dispatches the flags and the phase.
//
// **Known, deliberately unfixed: ~/.shed/config.yaml is read before any budget
// exists.** cmd/shed/main.go's PersistentPreRunE loads the client config for
// every command (`config.LoadClientConfig`), which runs before this function
// and therefore before either phase's context. A config file that is a FIFO,
// or that lives on a stalled network mount, blocks there — past roost's 5s,
// with no row printed and nothing this file can do about it. Out of scope on
// purpose: shed does not defend against its own config file being hostile
// anywhere else in the CLI (`shed ls`, `shed create` and the rest read it the
// same way through the same pre-run), and singling out this one command would
// buy a guarantee the other forty do not make while restructuring the root
// command's pre-run for every one of them.
func runRoostProvider(cmd *cobra.Command, args []string) error {
	if roostProviderInstall && roostProviderUninstall {
		return fmt.Errorf("cannot specify both --install and --uninstall")
	}
	if roostProviderDryRun && !roostProviderInstall && !roostProviderUninstall {
		return fmt.Errorf("--dry-run requires --install or --uninstall")
	}
	if (roostProviderInstall || roostProviderUninstall) && len(args) > 0 {
		return fmt.Errorf("cannot combine --install/--uninstall with a phase argument")
	}

	if roostProviderUninstall {
		return runRoostProviderUninstall()
	}
	if roostProviderInstall {
		return runRoostProviderInstall()
	}

	switch phase := roostProviderPhase(args); phase {
	case "list":
		return runRoostProviderList()
	case "activate":
		return runRoostProviderActivate()
	case "":
		return fmt.Errorf("roost-provider: no phase given (expected $1 or $ROOST_PROVIDER_PHASE to be \"list\" or \"activate\")")
	default:
		return fmt.Errorf("roost-provider: unknown phase %q", phase)
	}
}

// roostProviderPhase reads the phase roost's own contract carries it in:
// argv[1] first (roost's discovered-script invocation is `[launcher, phase]`,
// which the launcher forwards to us verbatim as "$@"), else
// $ROOST_PROVIDER_PHASE (plan 019 §3.1, roost's `invocation_argv`/
// `invocation_env`, roost-ui-model/src/provider.rs — both are always set on a
// real invocation; the fallback exists for a manual `ROOST_PROVIDER_PHASE=list
// shed roost-provider` invocation with no argument).
func roostProviderPhase(args []string) string {
	if len(args) > 0 && args[0] != "" {
		return args[0]
	}
	return os.Getenv("ROOST_PROVIDER_PHASE")
}

// ---- list ----

func runRoostProviderList() error {
	ctx, cancel := context.WithTimeout(context.Background(), roostProviderBudget(roostProviderListBudget))
	defer cancel()

	inv := roostprovider.Inventory(ctx, clientConfig.Servers, roostprovider.DefaultPerServerTimeout)
	machines := clientConfig.DecodeMachines()
	return printMenu(roostprovider.ListMenu(inv, machines))
}

// ---- activate ----

func runRoostProviderActivate() error {
	id, err := selectedID()
	if err != nil {
		return err
	}
	tok, err := roostprovider.ParseToken(id)
	if err != nil {
		// A row id this process did not itself mint (or a corrupted one) is a
		// PROVIDER FAILURE, not a row: roost never activates a row it did not
		// just print (`actionable:false` rows are never activated either), so
		// a token that fails to parse means the contract broke somewhere, and
		// guessing a host to answer about would be worse than saying so.
		return err
	}
	host := tok.Host()

	remote, noSSHRow := newRemote()
	if noSSHRow != nil {
		return exitRow(*noSSHRow)
	}

	ctx, cancel := context.WithTimeout(context.Background(), roostProviderBudget(roostProviderActivateBudget))
	defer cancel()

	target, landingDir, unreachable := resolveTarget(ctx, host)
	if unreachable != nil {
		return exitRow(*unreachable)
	}

	step := roostprovider.NextStep(tok)
	if step == roostprovider.StepOpen {
		// **The protocol gate is re-applied here, not inherited.** roost runs
		// the provider AFRESH for every drill-down step, so the
		// `session.identify` this run's step 2 performed happened in a
		// different process against a far side that has had a whole human
		// interaction's worth of time to change underneath it — restarted at a
		// different version, upgraded, downgraded. Without this call a session
		// that went from protocol 4 to 3 between the agent menu and the
		// workdir row gets a tab opened against a wire this build does not
		// speak, instead of §3.2's pinned mismatch row. The call costs one
		// exec on the ControlMaster the same step's ssh options already
		// established (§3.3), not a handshake.
		if _, err := remote.Identify(ctx, target); err != nil {
			row, cerr := classifyReachErr(host, err)
			if cerr != nil {
				return cerr
			}
			return exitRow(*row)
		}
		return openTab(ctx, remote, target, tok, host)
	}

	probe, projects, row, err := reachHost(ctx, remote, target, host, landingDir)
	if err != nil {
		return err
	}
	if row != nil {
		return exitRow(*row)
	}
	if step == roostprovider.StepAgents {
		return printMenu(roostprovider.AgentMenu(host, probe))
	}

	candidates := roostprovider.WorkdirCandidates(projects, probe.Home, landingDir, probe.LandingDirExists)
	if opened, ok := roostprovider.CollapseWorkdirs(tok, candidates); ok {
		return openTab(ctx, remote, target, opened, host)
	}
	return printMenu(roostprovider.WorkdirMenu(tok, candidates))
}

// selectedID reads roost's activate selection.
//
// **$ROOST_SELECTED_ID wins over the stdin JSON carrying the same value.**
// roost sets both on every real activate (roost-ui-model/src/provider.rs:
// `invocation_env` sets the env var whenever `ctx.selected_id` is `Some`;
// `invocation_stdin` always writes the JSON object, with the same value under
// `selected_id`), so on a real invocation the two never disagree — the choice
// only matters for which one this reads. The env var is simpler (no parse, no
// risk of blocking on stdin) and is what every other piece of context roost
// hands a provider (ROOST_SOCKET, ROOST_QUERY, ROOST_ACTIVE_CWD, …) already
// does the same way, so reading it first keeps this package free of a JSON
// decode on the hot path. The stdin JSON is read only as a fallback — a
// hand-run `echo '{"selected_id":"…"}' | shed roost-provider activate` during
// development, or a future roost that stops setting the env var but keeps the
// object — and only when stdin is not a terminal (see stdinSelectedID),
// because roost always closes stdin right after writing that object
// (roost-engine/src/process.rs's `write_stdin` closure drops the handle once
// the write completes) but a bare interactive invocation would otherwise hang
// forever waiting for input nobody is going to send.
func selectedID() (string, error) {
	return selectedIDFrom(os.Getenv("ROOST_SELECTED_ID"), stdinSelectedID)
}

// selectedIDFrom is selectedID's injectable core: envVal is
// $ROOST_SELECTED_ID, already read; stdinFallback is tried only when envVal
// is empty.
func selectedIDFrom(envVal string, stdinFallback func() (string, bool)) (string, error) {
	if envVal != "" {
		return envVal, nil
	}
	if id, ok := stdinFallback(); ok {
		return id, nil
	}
	return "", fmt.Errorf("roost-provider activate: no ROOST_SELECTED_ID (env) and no selected_id on stdin")
}

// stdinSelectedIDDeadline bounds how long the stdin fallback waits.
//
// roost writes its stdin object and closes the pipe immediately
// (roost-engine/src/process.rs's `write_stdin` closure drops the handle once
// the write completes), so on a real invocation EOF arrives in microseconds
// and this timer is never approached. It exists for every other writer: the
// byte cap bounds how MUCH is read, not how long the read waits, so a peer
// that sends one JSON object and holds the pipe open would otherwise hang
// activate indefinitely — before its own timeout context is even created, and
// therefore with nothing to cancel it.
const stdinSelectedIDDeadline = 250 * time.Millisecond

// stdinSelectedID reads roost's activate stdin JSON
// (`{"v":1,…,"selected_id":"…",…}`) for its selected_id field. See selectedID
// for why this is only a fallback and why the terminal check is load-bearing:
// without it, a bare interactive `shed roost-provider activate` (no env var,
// no pipe) would block forever waiting for input nobody is going to send.
func stdinSelectedID() (string, bool) {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return "", false
	}
	return readSelectedIDWithin(os.Stdin, stdinSelectedIDDeadline)
}

// readSelectedIDWithin reads r's JSON object for its selected_id, giving up
// (and answering "no id") once d has passed.
//
// The read runs in a goroutine and the answer arrives over a BUFFERED channel,
// so an expired read has nothing to block on when its EOF finally comes — the
// goroutine sends and exits. It is deliberately not cancelled: there is no
// portable way to interrupt a blocking read on an inherited fd, and the only
// caller is a process whose next step is either to answer on the env var or to
// exit, so a goroutine parked on a pipe nobody will close costs one stack for
// the remaining milliseconds of the process's life.
func readSelectedIDWithin(r io.Reader, d time.Duration) (string, bool) {
	type answer struct {
		id string
		ok bool
	}
	ch := make(chan answer, 1)
	go func() {
		id, ok := parseSelectedIDJSON(io.LimitReader(r, maxRoostSelectedIDStdinBytes))
		ch <- answer{id, ok}
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case a := <-ch:
		return a.id, a.ok
	case <-timer.C:
		return "", false
	}
}

// parseSelectedIDJSON reads r fully and extracts its selected_id field. Split
// out from stdinSelectedID so a test can exercise the parse without an actual
// os.Stdin (a pipe stands in for "not a terminal").
func parseSelectedIDJSON(r io.Reader) (string, bool) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", false
	}
	var payload struct {
		SelectedID string `json:"selected_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.SelectedID == "" {
		return "", false
	}
	return payload.SelectedID, true
}

// newRemote resolves the ssh binary and the per-target ControlMaster scratch
// dir once per activate run. A nil *Row means a Remote was built; a non-nil
// one is the pinned "no local ssh" row (plan 019 §3.2) and there is nothing
// further to do.
func newRemote() (*roostprovider.Remote, *roostprovider.Row) {
	sshBin, err := roostprovider.ResolveSSH()
	if err != nil {
		row := roostprovider.NoSSHRow()
		return nil, &row
	}
	return &roostprovider.Remote{
		SSHBin:     sshBin,
		ControlDir: roostprovider.ControlDirFor(os.Getenv, os.Getuid()),
	}, nil
}

// resolveTarget builds the ssh Target for a host token and, for a shed, its
// current landing dir.
//
// **Design decision (plan 019 C3): re-run Inventory for this one server,
// rather than reconstructing the Target from ~/.shed/config.yaml's
// `servers:` entry.** Both would get the ssh identity right — config.yaml
// already has ServerHost/ServerSSHPort, and a shed's ssh identity never
// depends on the shed-server being reachable at the moment of activation.
// But WorkdirCandidates' landingDir does NOT live in config.yaml at all: it
// is only ever reported by the shed-server's `GET /api/sheds`, so an activate
// on a shed token has to reach the shed-server at least once regardless of
// which path is chosen for the ssh identity. Re-running Inventory for that
// one server (a map of size one, not the full fan-out `list` does) answers
// both questions in the one call, bounded at DefaultPerServerTimeout, and
// keeps "how do I reach this shed" defined in exactly one place
// (internal/roostprovider.ShedTarget) instead of two.
//
// It also turns "the shed stopped between `list` and `activate`" into a
// FAST, precise diagnosis: the shed-server says "not running" in well under a
// second, rather than this side waiting out ssh's own ConnectTimeout against
// a VM that is no longer listening. Either path ends on the same
// UnreachableRow, so the two are not wrong relative to each other — this one
// just gets there without spending the ssh timeout to learn it.
//
// A non-nil *Row here means the host could not be resolved to something worth
// dialing (a shed no longer running, or a server/machine hand-edited out of
// config.yaml since `list`); the caller prints it and returns, same as any
// other non-actionable state.
func resolveTarget(ctx context.Context, host roostprovider.Token) (roostprovider.Target, string, *roostprovider.Row) {
	if !host.IsShed() {
		for _, m := range clientConfig.DecodeMachines() {
			if m.Name == host.Machine {
				return roostprovider.MachineTarget(m), "", nil
			}
		}
		row := roostprovider.UnreachableRow(host, fmt.Sprintf("machine %q is no longer in ~/.shed/config.yaml", host.Machine))
		return roostprovider.Target{}, "", &row
	}

	entry, ok := clientConfig.Servers[host.Server]
	if !ok {
		row := roostprovider.UnreachableRow(host, fmt.Sprintf("server %q is no longer in ~/.shed/config.yaml", host.Server))
		return roostprovider.Target{}, "", &row
	}

	inv := roostprovider.Inventory(ctx, map[string]config.ServerEntry{host.Server: entry}, roostprovider.DefaultPerServerTimeout)
	for _, s := range inv.Sheds {
		if s.Name == host.Shed {
			return roostprovider.ShedTarget(s, config.GetKnownHostsPath()), s.LandingDir, nil
		}
	}
	if len(inv.Answered) == 0 {
		row := roostprovider.UnreachableRow(host, host.Server+" did not answer")
		return roostprovider.Target{}, "", &row
	}
	// The server answered and this shed is not among its running sheds: it
	// stopped (or was deleted) between `list` and `activate`. A stopped shed
	// cannot take a tab (§3.2 step 1's whole reason for hiding stopped sheds
	// from `list` in the first place), so this is exactly as non-actionable
	// as any other unreachable host.
	row := roostprovider.UnreachableRow(host, "the shed is no longer running")
	return roostprovider.Target{}, "", &row
}

// reachHost runs plan 019 §3.2 step 2's discovery round trip and gate against
// a host that still needs a submenu (StepAgents or StepWorkdirs): the
// discovery probe, then session.identify (the protocol gate) and tab.list.
//
// tab.list is fetched here even for StepAgents, which does not need its
// result. Splitting the two bridge calls by step would mean an agent-step
// activation and a workdir-step activation classify "is this host reachable"
// two different ways — and a session that cannot answer tab.list is no more
// useful for opening a tab than one that failed session.identify, so gating
// the agent menu on it too is not spurious strictness. A wizard's steps share
// one ControlMaster (§3.3), so the extra call costs one more exec on an
// already-warm connection, not one more handshake.
//
// A non-nil *Row is one of plan 019 §3.2's pinned non-actionable states; a
// non-nil error is a genuine provider failure (propagated to the caller,
// which exits non-zero).
func reachHost(ctx context.Context, remote *roostprovider.Remote, target roostprovider.Target, host roostprovider.Token, landingDir string) (roostprovider.Probe, []roostprovider.Project, *roostprovider.Row, error) {
	probe, err := remote.Probe(ctx, target, landingDir)
	if err == nil {
		_, err = remote.Identify(ctx, target)
	}
	var projects []roostprovider.Project
	if err == nil {
		projects, err = remote.TabList(ctx, target)
	}
	if err != nil {
		row, cerr := classifyReachErr(host, err)
		return roostprovider.Probe{}, nil, row, cerr
	}
	return probe, projects, nil, nil
}

// classifyReachErr maps a Remote call's error onto a pinned row via
// RowForError, or passes it through as a provider failure when
// RowForError says it is not one (a malformed response, a bug — see
// RowForError's own doc for why those must not become a row).
func classifyReachErr(host roostprovider.Token, err error) (*roostprovider.Row, error) {
	if row, ok := roostprovider.RowForError(host, err); ok {
		return &row, nil
	}
	return nil, err
}

// openTab builds and sends `tab.open` for a token that has reached StepOpen,
// and prints plan 019 §3.2 step 4's confirmation line on success.
//
// tok carries the completed choice (agent, cwd, project); host is tok's own
// Host() and is only used to label a failure row, never to reach the far
// side (target already does that).
func openTab(ctx context.Context, remote *roostprovider.Remote, target roostprovider.Target, tok, host roostprovider.Token) error {
	params, err := roostprovider.TabOpenFor(tok)
	if err != nil {
		// TabOpenFor only fails on a token missing a cwd or naming an unknown
		// agent kind — both mean this process built (or roost handed back) a
		// row id that does not describe a launchable choice. That is a
		// contract break, not a far-side state, so it is a provider failure.
		return err
	}
	id, err := remote.TabOpen(ctx, target, params)
	if err != nil {
		row, cerr := classifyReachErr(host, err)
		if cerr != nil {
			return cerr
		}
		return exitRow(*row)
	}
	fmt.Printf("opened tab %s on %s\n", id, host.HostLabel())
	return nil
}

// printMenu writes a provider menu as the ONE JSON object plan 019 §3.2
// requires on stdout — no leading or trailing text, no indentation debris.
func printMenu(m roostprovider.Menu) error {
	return json.NewEncoder(os.Stdout).Encode(m)
}

// singleRowMenu wraps one row (always one of the pinned non-actionable rows)
// as the menu a provider phase prints for it.
func singleRowMenu(row roostprovider.Row) roostprovider.Menu {
	return roostprovider.Menu{Items: []roostprovider.Row{row}}
}

// exitRow prints row as the single row a phase falls back to when it cannot
// proceed — always one of plan 019 §3.2's pinned non-actionable states. Like
// singleRowMenu, this always returns a nil error: printing a row IS the
// success path for these callers, matching the pinned "every expected
// failure state exits zero with a row" contract.
func exitRow(row roostprovider.Row) error {
	return printMenu(singleRowMenu(row))
}

// ---- --install / --uninstall / --dry-run ----

// roostProviderLauncherPrefix is every line of plan 019 §3.1's launcher
// script EXCEPT the final `exec` line, which is the one line that varies (the
// shed binary's absolute path). It is also the file's "did shed write this"
// marker (see hasRoostProviderLabel): `--uninstall` only ever removes a file
// whose content starts with exactly this text, and `--install` only ever
// overwrites one that does.
const roostProviderLauncherPrefix = `#!/bin/sh
# @roost.label: shed
# @roost.title: Start an agent on a shed or machine
# Written by ` + "`shed roost-provider --install`" + `; re-run it if shed moves. Roost runs
# this by absolute path with its own (possibly minimal) PATH, so the shed binary
# is pinned here and the usual install dirs are prefixed, as roost's example does.
PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:$HOME/.local/bin:$PATH"; export PATH
`

// roostProviderLauncherContent renders the launcher for shedPath, which must
// already be absolute (resolvedShedExecutable's job).
//
// shedPath goes through shellQuoteArg (cmd/shed/console.go), the same
// single-quote-with-escape this package already uses for ssh argv — a path
// containing a space or a shell metacharacter (an unusual but legal
// filesystem path) must still round-trip through `/bin/sh -s` as one word.
func roostProviderLauncherContent(shedPath string) string {
	return roostProviderLauncherPrefix + "exec " + shellQuoteArg(shedPath) + ` roost-provider "$@"` + "\n"
}

// hasRoostProviderLabel reports whether content is a launcher shed itself
// wrote — the label-header check §3.1's acceptance criteria require before
// `--uninstall` ever removes a file, and before `--install` ever overwrites
// one, at `<dir>/providers/shed`.
//
// A prefix match, not an exact match on the whole file: the one line the
// prefix excludes (the final `exec '<path>' …` line) is the line that is
// SUPPOSED to differ from one install to the next as the shed binary moves,
// so requiring the whole file to match byte-for-byte would make a previously
// shed-written launcher unrecognizable as soon as `shed` moved once — exactly
// the case `--install`'s re-run story exists to handle safely.
func hasRoostProviderLabel(content string) bool {
	return len(content) >= len(roostProviderLauncherPrefix) && content[:len(roostProviderLauncherPrefix)] == roostProviderLauncherPrefix
}

// roostProviderDir returns the directory a launcher's `providers/` subdir
// sits beside: the parent of a non-empty $ROOST_CONFIG (a FILE path, per
// roost's own contract), else $HOME/.config/roost.
//
// **Deliberately does not consult $XDG_CONFIG_HOME.** Every other shed
// dotfile path goes through internal/config's XDG-aware helpers, but roost's
// own `default_path()` (roost-ui-model/src/config.rs) does not look at it
// either — verified against roost's source at the pinned rev — so an
// XDG_CONFIG_HOME-aware launcher path here would write a file roost would
// never find whenever the two disagree.
//
// **Two shapes of $ROOST_CONFIG are refused rather than interpreted**, because
// for both of them `filepath.Dir` answers a question the variable did not ask:
//
//   - one that names an existing DIRECTORY. roost's contract makes
//     $ROOST_CONFIG the config FILE, so a directory means the variable is being
//     used for something else — and taking its parent would write the launcher
//     into that directory's neighbour, which nobody asked for and which roost
//     would never read.
//   - one whose parent is the filesystem root. `ROOST_CONFIG=/roost.conf`
//     yields `/providers/shed`: a root-owned path a normal user cannot write,
//     and a path a root-run shed WOULD write. Neither is a roost config
//     directory; both are what a variable that got set to a stray value looks
//     like.
func roostProviderDir() (string, error) {
	if cfg := os.Getenv("ROOST_CONFIG"); cfg != "" {
		// Lstat, not Stat: a symlink TO a directory is still not a config
		// file, and following it here would decide the launcher's home by a
		// link this process did not write.
		if info, err := os.Lstat(cfg); err == nil && info.IsDir() {
			return "", fmt.Errorf("$ROOST_CONFIG (%s) names a directory; roost reads it as the config FILE, and the launcher goes beside that file — point it at the file itself", cfg)
		}
		dir := filepath.Dir(cfg)
		if isFilesystemRoot(dir) {
			return "", fmt.Errorf("$ROOST_CONFIG (%s) sits at the filesystem root, so the launcher would go to %s; point it at a real roost config file", cfg, filepath.Join(dir, "providers", "shed"))
		}
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine $HOME for the default roost config directory: %w", err)
	}
	return filepath.Join(home, ".config", "roost"), nil
}

// isFilesystemRoot reports whether dir IS `/` — literally, or after resolving
// symlinks (`ROOST_CONFIG=/link-to-root/roost.conf` is the same mistake spelled
// through a link). EvalSymlinks failing is not an answer either way, so it only
// ever adds a refusal, never removes one.
func isFilesystemRoot(dir string) bool {
	root := string(filepath.Separator)
	if filepath.Clean(dir) == root {
		return true
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil && filepath.Clean(real) == root {
		return true
	}
	return false
}

// roostProviderLauncherPath is the one path both --install and --uninstall
// act on: roostProviderDir's <dir>/providers/shed. Factored out so the two
// callers compute it identically rather than each re-deriving it.
func roostProviderLauncherPath() (string, error) {
	dir, err := roostProviderDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "providers", "shed"), nil
}

// resolvedShedExecutable returns the running shed binary's resolved absolute
// path — what the launcher pins.
//
// Symlinks are resolved (EvalSymlinks), matching roost's own convention for
// exactly the same reason (roost-engine/src/process.rs's `executable_file`:
// "the answer is exported into child processes with cwds of their own, so a
// relative or symlink-spelled path is a path they cannot run"). This does mean
// a Homebrew upgrade — which relinks `/opt/homebrew/bin/shed` at a new Cellar
// path — leaves a stale launcher until `--install` is re-run; the launcher's
// own header line says so, and there is no way to have both "pin the real
// binary" and "never go stale under an upgrade" at once. EvalSymlinks failing
// (a dangling symlink, an unreadable path) falls back to the unresolved
// path rather than failing the whole install — a launcher pinning a symlink
// still works today, just not necessarily after the symlink target moves.
func resolvedShedExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("determine the running shed binary's path: %w", err)
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		return real, nil
	}
	return exe, nil
}

// roostProviderInstallResult is --install/--uninstall's JSON Details payload,
// following the shed ssh-config precedent (ActionResult{..., Details: …}).
type roostProviderInstallResult struct {
	Path   string `json:"path"`
	Action string `json:"action"` // "created" | "updated" | "unchanged" | "removed" | "not-found" | "not-ours"
}

// launcherState is what is at the launcher path right now.
type launcherState int

const (
	// launcherAbsent — nothing is there.
	launcherAbsent launcherState = iota
	// launcherOurs — a regular file carrying shed's label header.
	launcherOurs
	// launcherForeign — a regular file that is somebody else's.
	launcherForeign
)

// maxLauncherBytes bounds the header read. The launcher shed writes is under
// 600 bytes; this is enough headroom to recognize a hand-edited one and little
// enough that a `providers/shed` that is secretly a 4 GiB file is not read
// into memory to be told it is not ours.
const maxLauncherBytes = 64 << 10

// inspectLauncher reports what is at path, reading its content only when that
// content can mean something.
//
// **The whole of this function is the answer to "the installer must never
// destroy a file it did not write".** The old shape — `os.ReadFile` to check
// the header, then `os.WriteFile`/`os.Remove` to act — followed symlinks at
// every step, which made `providers/shed` → `~/.ssh/authorized_keys` a path by
// which an install could truncate the TARGET. So:
//
//   - Lstat, never Stat, decides what is there. A symlink is refused outright:
//     shed has never written one at this path, so one is always somebody
//     else's — including the DANGLING case, which `ReadFile`/`Stat` report as
//     "nothing there" and which an installer that believed them would answer
//     by creating the link's target.
//   - anything that is not a regular file (a directory, a FIFO, a device) is
//     refused for the same reason.
//   - the header read opens with O_NOFOLLOW, so the one window between the
//     Lstat and the open cannot be used to swap a symlink in behind it.
func inspectLauncher(path string) (string, launcherState, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return "", launcherAbsent, nil
	default:
		return "", launcherAbsent, fmt.Errorf("inspect %s: %w", path, err)
	}

	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return "", launcherAbsent, fmt.Errorf("%s is a symlink; `shed roost-provider --install` never writes one, so this is somebody else's file and writing through it would land on whatever it points at — move it aside and re-run", path)
	case !info.Mode().IsRegular():
		return "", launcherAbsent, fmt.Errorf("%s is not a regular file (mode %s); move it aside and re-run", path, info.Mode().Type())
	}

	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", launcherAbsent, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxLauncherBytes))
	if err != nil {
		return "", launcherAbsent, fmt.Errorf("read %s: %w", path, err)
	}
	if hasRoostProviderLabel(string(data)) {
		return string(data), launcherOurs, nil
	}
	return string(data), launcherForeign, nil
}

// writeLauncherAtomically writes content to path through a temp file in the
// SAME directory, chmod'd 0755 before it is renamed into place.
//
// Two properties, both load-bearing:
//
//   - **atomic.** `os.WriteFile` truncates first and writes second, so an
//     install interrupted in that window (a signal, a full disk, a killed
//     terminal) leaves a TRUNCATED mode-0755 launcher — a half script roost
//     will still happily execute. A rename is a single step: the path either
//     names the old file or the complete new one, never a partial one. The
//     mode is set on the temp file, before the rename, so the file is never
//     visible at the final path without its execute bit either.
//   - **it replaces a symlink instead of following one.** `rename(2)` operates
//     on the name, not on what the name resolves to, which is what closes the
//     "write lands on the symlink's target" hole for good — inspectLauncher's
//     refusal is the diagnosis, this is the guarantee behind it.
//
// The temp file must share the destination's directory: a rename across
// filesystems fails with EXDEV, and /tmp is routinely a different filesystem
// from $HOME.
//
// **Residual TOCTOU, accepted and deliberate:** between inspectLauncher's read
// and this rename, a regular file at the path could be swapped for a different
// regular file, which the rename would then replace. Closing that would mean
// holding the directory open and working through `openat`/`renameat`/
// `unlinkat` by directory fd — and it would buy nothing, because anyone able
// to write into the user's own roost config directory can simply write the
// launcher themselves, with no race and no shed involved. The symlink and
// non-regular-file refusals above are the cases that DO matter, because those
// redirect a write to a path OUTSIDE that directory.
func writeLauncherAtomically(path, content string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".shed-roost-provider-*.tmp")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// discard removes the temp file on every path out of here that is not the
	// successful rename. Closing twice is harmless (the second returns
	// ErrClosed, which is dropped); leaving a mode-0755 fragment beside a
	// launcher is not.
	discard := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if _, err := tmp.WriteString(content); err != nil {
		discard()
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	// Chmod on the HANDLE (fchmod), not on the path: the mode lands on the
	// file this function created, whatever else may have appeared at that name
	// meanwhile. CreateTemp makes it 0600, and roost must be able to exec it.
	if err := tmp.Chmod(0o755); err != nil {
		discard()
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}

func runRoostProviderInstall() error {
	shedPath, err := resolvedShedExecutable()
	if err != nil {
		return err
	}
	launcherPath, err := roostProviderLauncherPath()
	if err != nil {
		return err
	}
	content := roostProviderLauncherContent(shedPath)

	existing, state, err := inspectLauncher(launcherPath)
	if err != nil {
		return err
	}
	if state == launcherForeign {
		// Never overwrite a file shed did not write — the same rule
		// --uninstall applies to deletion, applied here to clobbering. A
		// user's own script happening to be named "shed" is exactly the shape
		// this refuses, in either direction.
		return fmt.Errorf("%s exists and was not written by `shed roost-provider --install`; move it aside and re-run", launcherPath)
	}

	action := "created"
	if state == launcherOurs {
		if existing == content {
			action = "unchanged"
		} else {
			action = "updated"
		}
	}

	if roostProviderDryRun || action == "unchanged" {
		return outputRoostProviderInstallResult(launcherPath, action, roostProviderDryRun)
	}

	if err := os.MkdirAll(filepath.Dir(launcherPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(launcherPath), err)
	}
	if err := writeLauncherAtomically(launcherPath, content); err != nil {
		return err
	}
	return outputRoostProviderInstallResult(launcherPath, action, false)
}

func runRoostProviderUninstall() error {
	launcherPath, err := roostProviderLauncherPath()
	if err != nil {
		return err
	}

	// Same inspection as --install, and for the same reason: `os.Remove` on a
	// symlink unlinks the LINK rather than its target, but a dangling or
	// hostile link at this path still means the file shed is being asked to
	// remove is not the file shed wrote, and the honest answer to that is to
	// say so rather than to tidy up somebody else's link.
	_, state, err := inspectLauncher(launcherPath)
	if err != nil {
		return err
	}
	switch state {
	case launcherAbsent:
		return outputRoostProviderInstallResult(launcherPath, "not-found", false)
	case launcherForeign:
		// Never delete a file shed did not write.
		return outputRoostProviderInstallResult(launcherPath, "not-ours", false)
	}

	if roostProviderDryRun {
		return outputRoostProviderInstallResult(launcherPath, "removed", true)
	}
	if err := os.Remove(launcherPath); err != nil {
		return fmt.Errorf("remove %s: %w", launcherPath, err)
	}
	return outputRoostProviderInstallResult(launcherPath, "removed", false)
}

// outputRoostProviderInstallResult prints an --install/--uninstall outcome in
// either shape, matching the ssh-config precedent: an ActionResult envelope
// in --json mode, plain English sentences otherwise.
func outputRoostProviderInstallResult(path, action string, dryRun bool) error {
	verb := "installed"
	if roostProviderUninstall {
		verb = "uninstalled"
	}
	if dryRun {
		verb = "dry-run"
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: verb,
			Details: roostProviderInstallResult{
				Path:   path,
				Action: action,
			},
		})
	}

	fmt.Printf("Provider launcher: %s\n\n", path)
	switch action {
	case "created":
		if dryRun {
			fmt.Println("File will be created.")
		} else {
			printSuccess("Wrote roost provider launcher at %s", path)
		}
	case "updated":
		if dryRun {
			fmt.Println("File will be updated (the pinned shed path changed).")
		} else {
			printSuccess("Updated roost provider launcher at %s", path)
		}
	case "unchanged":
		fmt.Println("Launcher is already up to date; nothing to do.")
	case "removed":
		if dryRun {
			fmt.Println("File will be removed.")
		} else {
			printSuccess("Removed roost provider launcher at %s", path)
		}
	case "not-found":
		fmt.Println("No roost provider launcher found; nothing to do.")
	case "not-ours":
		fmt.Println("A file exists at this path but was not written by `shed roost-provider --install`; leaving it alone.")
	}
	if dryRun {
		fmt.Println("\n(dry run - no changes made)")
	}
	return nil
}
