package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/roostprovider"
)

var (
	sessionsAllFlag      bool
	sessionsTmuxFlag     bool
	sessionsKillTmuxFlag bool
	sessionsKillTabFlag  string
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions [shed-name]",
	Short: "List a shed's roost tabs and tmux sessions",
	Long: `List sessions across sheds.

With a local roost app running, each shed's roost tabs are listed above its
tmux sessions. Without one -- or with --tmux, or SHED_ATTACH=tmux -- this lists
the tmux sessions alone, exactly as it always has.

A shed that cannot be reached, or that is running no roost session, contributes
its tmux rows and one warning; it is never a failure.

Examples:
  shed sessions                 # All sessions on default server
  shed sessions myproj          # List sessions in specific shed
  shed sessions --all           # List across all servers
  shed sessions --tmux          # tmux rows only, even with roost running
  shed sessions --json          # Output as JSON`,
	Args: sessionsArgs,
	RunE: runSessions,
}

// sessionsArgs refuses `--all` together with a positional shed argument,
// before either half of runSessions makes a single API or roostctl call.
//
// Cobra parses flags before it calls Args (Command.execute in
// github.com/spf13/cobra: c.ParseFlags(a) runs, then c.ValidateArgs(argWoFlags)
// -- which calls c.Args(c, args) -- runs after), so the flag is already set by
// the time this validator reads it.
//
// `--all` lists every server's sheds; a positional names exactly one. The two
// used to coexist silently -- the tmux half ignored the argument outright,
// and the roost half filtered on it, so one command answered two different
// questions in the same output. Refusing the combination is the honest fix:
// the bug was the silence, not the missing filter.
func sessionsArgs(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	if all && len(args) > 0 {
		return fmt.Errorf("--all lists every shed; drop the argument or drop --all")
	}
	return cobra.MaximumNArgs(1)(cmd, args)
}

var sessionsKillCmd = &cobra.Command{
	Use:   "kill <shed-name> <session-name>",
	Short: "Close a roost tab, or kill a tmux session",
	Long: `Terminate a session in a shed.

With a local roost app running, a roost tab of that title is closed; otherwise
-- or with --tmux, or SHED_ATTACH=tmux -- the tmux session of that name is
killed.

If the name matches several roost tabs, or both a tab and a tmux session, the
command refuses and prints the tab ids: pass --tab <id> to close one tab, or
--tmux to kill the tmux session.

Example:
  shed sessions kill myproj debug            # Close the "debug" tab, or kill the session
  shed sessions kill myproj debug --tab 7    # Close that one roost tab
  shed sessions kill myproj debug --tmux     # Kill the tmux session`,
	Args: cobra.ExactArgs(2),
	RunE: runSessionsKill,
}

func init() {
	sessionsCmd.Flags().BoolVarP(&sessionsAllFlag, "all", "a", false, "List sessions from all servers")
	sessionsCmd.Flags().BoolVar(&sessionsTmuxFlag, "tmux", false, "List tmux sessions only, even when a local roost app is running (same as SHED_ATTACH=tmux)")

	sessionsKillCmd.Flags().BoolVar(&sessionsKillTmuxFlag, "tmux", false, "Kill the tmux session, even when a local roost app is running (same as SHED_ATTACH=tmux)")
	sessionsKillCmd.Flags().StringVar(&sessionsKillTabFlag, "tab", "", "Close this roost tab id (disambiguates several tabs of one title)")

	sessionsCmd.AddCommand(sessionsKillCmd)
	rootCmd.AddCommand(sessionsCmd)
}

// namedServer is one server this listing queried: the `servers:` entry name
// and a COPY of its entry.
//
// A copy rather than a pointer into clientConfig.Servers, for the reason
// shedNameIsAmbiguous spells out at length: building an API client can re-mint
// a near-expiry credential and write it straight back into that map, so a
// reader holding a pointer into it is racing a writer.
type namedServer struct {
	name  string
	entry config.ServerEntry
}

// runSessions lists a shed's (or the fleet's) sessions.
//
// The gate is the same one `shed attach` applies, in the same order: the floor
// first, then the local app. Asking roost anything at all happens strictly
// after the tmux rows are in hand, so `--tmux` and a machine with no roost
// take exactly the path — and produce exactly the output — they always have.
func runSessions(cmd *cobra.Command, args []string) error {
	shedName := ""
	if len(args) == 1 {
		shedName = args[0]
	}

	allSessions, servers, err := collectSessions(shedName)
	if err != nil {
		return err
	}

	if sessionsWantsTmux() {
		return outputTmuxSessions(allSessions)
	}
	ctx := context.Background()
	roost := newRoostSessions()
	if !roost.ctl.Available(ctx) {
		return outputTmuxSessions(allSessions)
	}

	results := roost.tabsFor(ctx, roost.runningSheds(servers, shedName))
	roost.reportTabFailures(results)
	listings := mergeSessions(results, allSessions)
	if jsonFlag {
		return outputJSON(mergedSessionRows(listings))
	}
	return printMergedSessions(os.Stdout, listings)
}

// collectSessions is the tmux half, unchanged: the server API's session rows,
// its warnings, and its errors, exactly as `shed sessions` has always produced
// them. It additionally reports WHICH servers it queried, which is the set the
// roost half then asks for sheds.
func collectSessions(shedName string) ([]config.Session, []namedServer, error) {
	var allSessions []config.Session
	var servers []namedServer

	if sessionsAllFlag {
		// Query all servers
		for serverName, entry := range clientConfig.Servers {
			client := NewAPIClientFromNamedEntry(serverName, &entry, DefaultTimeout)
			resp, err := client.ListAllSessions()
			if err != nil {
				if verboseLevel > 0 {
					fmt.Fprintf(os.Stderr, "Warning: could not query server %s: %v\n", serverName, err)
				}
				continue
			}
			// Recorded only once it ANSWERED. `--all` keeps quiet about a
			// server it could not reach (that warning is behind -v), and a
			// roost half that then asked the same dead server for its sheds
			// would both spend a second doomed round trip and print the
			// warning this branch deliberately withholds.
			servers = append(servers, namedServer{name: serverName, entry: entry})
			// Display warnings about sheds that couldn't be queried
			for _, warning := range resp.Warnings {
				fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
			}
			// Add server name to each session
			for i := range resp.Sessions {
				resp.Sessions[i].ServerName = serverName
			}
			allSessions = append(allSessions, resp.Sessions...)
		}
		return allSessions, servers, nil
	}

	// Query single server
	entry, serverName, err := getServerEntry()
	if err != nil {
		return nil, nil, err
	}
	servers = append(servers, namedServer{name: serverName, entry: *entry})
	client := NewAPIClientFromNamedEntry(serverName, entry, DefaultTimeout)

	if shedName != "" {
		// List sessions for a specific shed
		resp, err := client.ListSessions(shedName)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to list sessions for %s: %w", shedName, err)
		}
		// Surface warnings (e.g. a shed that couldn't be queried) exactly like
		// the aggregate paths do, so a degraded listing is never silent.
		for _, warning := range resp.Warnings {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
		}
		for i := range resp.Sessions {
			resp.Sessions[i].ServerName = serverName
		}
		return resp.Sessions, servers, nil
	}

	// List all sessions on this server
	resp, err := client.ListAllSessions()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	// Display warnings about sheds that couldn't be queried
	for _, warning := range resp.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
	}
	for i := range resp.Sessions {
		resp.Sessions[i].ServerName = serverName
	}
	return resp.Sessions, servers, nil
}

// outputTmuxSessions is the FLOOR's output — the table and the JSON array
// `shed sessions` printed before roost existed, down to the absent `source`
// key. Nothing about it is conditional on roost, which is the point: `--tmux`
// is a promise that today's consumers keep working unchanged.
func outputTmuxSessions(sessions []config.Session) error {
	if jsonFlag {
		if sessions == nil {
			sessions = make([]config.Session, 0)
		}
		return outputJSON(sessions)
	}
	return printSessionsTable(sessions)
}

// runSessionsKill closes a roost tab, or kills a tmux session.
//
// The shed is resolved first, exactly as before: a name that is not a shed on
// any server fails the same way whatever transport would have handled it.
func runSessionsKill(cmd *cobra.Command, args []string) error {
	shedName := args[0]
	sessionName := args[1]

	if sessionsKillTabFlag != "" && sessionsKillWantsTmux() {
		return fmt.Errorf("--tab %s names a roost tab and --tmux names the tmux session; pass only one",
			sessionsKillTabFlag)
	}

	// Find the server hosting this shed
	serverName, entry, err := findShedServer(shedName)
	if err != nil {
		return err
	}

	if sessionsKillWantsTmux() {
		return killTmuxSession(serverName, entry, shedName, sessionName)
	}
	ctx := context.Background()
	roost := newRoostSessions()
	if !roost.ctl.Available(ctx) {
		if sessionsKillTabFlag != "" {
			return fmt.Errorf("--tab %s names a roost tab, but no local roost app is running", sessionsKillTabFlag)
		}
		return killTmuxSession(serverName, entry, shedName, sessionName)
	}

	shed := roostprovider.RunningShed{
		Name:          shedName,
		Server:        serverName,
		ServerHost:    entry.Host,
		ServerSSHPort: entry.SSHPort,
	}
	found := roost.tabsForShed(ctx, shed)
	if found.err != nil {
		// A shed whose tabs cannot be read is the listing's degradation, not a
		// failure — unless the user named a tab id, in which case the thing
		// they asked for demonstrably did not happen and silently killing a
		// tmux session instead would be the wrong command entirely.
		if sessionsKillTabFlag != "" {
			return fmt.Errorf("failed to list %s's roost tabs: %w", shedName, found.err)
		}
		roost.warn("could not list %s's roost tabs: %s", shedName, oneLine(found.err))
		return killTmuxSession(serverName, entry, shedName, sessionName)
	}

	matches := tabsTitled(found.tabs, sessionName)
	if sessionsKillTabFlag != "" {
		for _, tab := range matches {
			if tab.ID == sessionsKillTabFlag {
				return roost.closeTab(ctx, shed, tab)
			}
		}
		return fmt.Errorf("shed %q has no roost tab %s titled %q", shedName, sessionsKillTabFlag, sessionName)
	}
	if len(matches) == 0 {
		// Nothing roost knows by that name: the tmux path owns this, including
		// the error it produces when there is no such session either.
		return killTmuxSession(serverName, entry, shedName, sessionName)
	}

	// A tab matched, so whether a tmux session ALSO carries the name decides
	// between acting and refusing — it is asked for only now, and only here.
	hasTmux, known, err := tmuxSessionExists(serverName, entry, shedName, sessionName)
	if err != nil {
		return fmt.Errorf("failed to list sessions for %s: %w", shedName, err)
	}
	if len(matches) > 1 || hasTmux {
		return errors.New(killAmbiguityMessage(shedName, sessionName, matches, hasTmux))
	}
	if !known {
		// The server answered, but with warnings — so "no tmux session of that
		// name" is not something it actually established. Refusing is the only
		// honest move: the alternative is closing a tab on the strength of an
		// answer nobody gave.
		return fmt.Errorf("shed %q has a roost tab titled %q (tab %s), and whether a tmux "+
			"session of that name also exists could not be determined; pass --tab %s to close "+
			"the tab, or --tmux to kill the tmux session",
			shedName, sessionName, matches[0].ID, matches[0].ID)
	}
	return roost.closeTab(ctx, shed, matches[0])
}

// killTmuxSession is the FLOOR's kill, unchanged — including the error a
// missing session produces, which is the server's own.
func killTmuxSession(serverName string, entry *config.ServerEntry, shedName, sessionName string) error {
	client := NewAPIClientFromNamedEntry(serverName, entry, DefaultTimeout)
	if err := client.KillSession(shedName, sessionName); err != nil {
		return fmt.Errorf("failed to kill session: %w", err)
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "killed",
			Name:   sessionName,
			Details: struct {
				Shed string `json:"shed"`
			}{Shed: shedName},
		})
	}

	printSuccess("Killed session %q in shed %q", sessionName, shedName)
	return nil
}

// tmuxSessionExists reports whether this shed has a tmux session of this name.
//
// The third return says whether the answer is TRUSTWORTHY. `GET /sessions`
// degrades rather than failing — a shed whose tmux could not be reached comes
// back as an empty list plus a warning — and this function's only caller is
// about to destroy something on the strength of it. Reading "no rows" as "no
// such session" there would close a roost tab while a same-named tmux session
// sat behind an unread warning, which is precisely the ambiguity the refusal
// exists to prevent. So an answer carrying warnings is reported as unknown,
// and the caller refuses instead of guessing.
func tmuxSessionExists(serverName string, entry *config.ServerEntry, shedName, sessionName string) (exists, known bool, err error) {
	resp, err := NewAPIClientFromNamedEntry(serverName, entry, DefaultTimeout).ListSessions(shedName)
	if err != nil {
		return false, false, err
	}
	for _, session := range resp.Sessions {
		if session.Name == sessionName {
			return true, true, nil
		}
	}
	return false, len(resp.Warnings) == 0, nil
}

// printSessionsTable is the floor's table: `SHED SESSION STATUS CREATED
// WINDOWS`, the columns `shed sessions` has always printed. The roost path
// prints these same rows through printMergedSessions, from the same header
// constant and the same status cell.
func printSessionsTable(sessions []config.Session) error {
	if len(sessions) == 0 {
		fmt.Println("No sessions found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, tmuxSessionsHeader)

	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n",
			s.ShedName, s.Name, sessionStatus(s), formatTimeAgo(s.CreatedAt), s.WindowCount)
	}

	return w.Flush()
}

func formatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}

	duration := time.Since(t)

	// Handle future times (e.g., clock skew between host and container)
	if duration < 0 {
		return "just now"
	}

	switch {
	case duration < time.Minute:
		return "just now"
	case duration < time.Hour:
		mins := int(duration.Minutes())
		if mins == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", mins)
	case duration < 24*time.Hour:
		hours := int(duration.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	default:
		days := int(duration.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}
