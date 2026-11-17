package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

// usersRefreshTimeout covers the server's own bound on the fetch (60 s) plus
// the round trip.
const usersRefreshTimeout = 90 * time.Second

var usersCmd = &cobra.Command{
	Use:   "users",
	Short: "Manage the users of a shed server",
	Long:  "Manage the users of a shed server and the SSH keys they log in with.",
}

var usersRefreshCmd = &cobra.Command{
	Use:   "refresh",
	Short: "Fetch the server's GitHub logins again (admin)",
	Long: `Make the server fetch the SSH keys of every GitHub login it follows now,
instead of at its next scheduled refresh, and print what changed.

Each login is checked against the GitHub account id the server pinned it
to: a login that now names another account, or none, has its keys dropped
(status "mismatch"). When GitHub cannot be reached the last known keys stay
and no new key is accepted (status "unreachable").

On a server whose users come from an account service (auth.directory), it
pulls the service's user list now and prints the pull's row: good, or failed
with why and the list in force (the list the server held, while its trust
time lasts; otherwise only the local admins). A failed pull exits non-zero.

Admin only. Use -s/--server to pick the server.`,
	Args: cobra.NoArgs,
	RunE: runUsersRefresh,
}

var usersStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show where the server's users come from (admin)",
	Long: `Show where the server's users come from (config, an account service's
list, or none), who they are (name, admin flag and source; never a key),
and, on a server whose users come from an account service, the state of its
list: whether it is trusted and why not, the last good pull, its version, how
long it stays trusted (an estimate by this machine's clock; the server's
state is what counts), the last attempt and its error, the next attempt, the
kept copy, and the list entries the server skips. Text that came from the
account service is quoted; control and format characters from the server
are shown escaped, in --json too.

Admin only. Use -s/--server to pick the server.`,
	Args: cobra.NoArgs,
	RunE: runUsersStatus,
}

func init() {
	usersCmd.AddCommand(usersRefreshCmd)
	usersCmd.AddCommand(usersStatusCmd)
	rootCmd.AddCommand(usersCmd)
}

func runUsersRefresh(cmd *cobra.Command, args []string) error {
	entry, name, err := getServerEntry()
	if err != nil {
		return err
	}
	client := NewAPIClientFromNamedEntry(name, entry, usersRefreshTimeout)
	resp, err := client.RefreshUsers()
	if err != nil {
		return fmt.Errorf("refresh users on %s: %w", name, err)
	}
	out := cmd.OutOrStdout()
	switch {
	case jsonFlag && resp.Directory == nil:
		// A server with no directory: the JSON as it always was.
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return err
		}
	case jsonFlag:
		if err := writeTerminalSafeJSON(out, resp); err != nil {
			return err
		}
	default:
		renderUsersRefresh(out, *resp)
	}
	return directoryPullError(name, resp.Directory)
}

// directoryPullError is the error `shed users refresh` exits with after
// printing a directory pull's row (nil for none, or a good pull): a failed
// pull, and what is in force after it.
func directoryPullError(name string, row *config.DirectoryRefreshRow) error {
	if row == nil || row.Status == config.DirectoryPullGood {
		return nil
	}
	if listOf(*row) == config.DirectoryListInForce {
		return fmt.Errorf("refresh users on %s: the directory pull failed; the list in force is unchanged", name)
	}
	return fmt.Errorf("refresh users on %s: the directory pull failed; only the local admins are admitted", name)
}

func runUsersStatus(cmd *cobra.Command, args []string) error {
	entry, name, err := getServerEntry()
	if err != nil {
		return err
	}
	client := NewAPIClientFromNamedEntry(name, entry, DefaultTimeout)
	resp, err := client.UsersStatus()
	if err != nil {
		return usersStatusError(name, err)
	}
	if jsonFlag {
		return writeTerminalSafeJSON(cmd.OutOrStdout(), resp)
	}
	renderUsersStatus(cmd.OutOrStdout(), *resp, time.Now())
	return nil
}

// usersStatusError is err from asking server name for its users' status.
func usersStatusError(name string, err error) error {
	if errors.Is(err, errUsersStatusUnsupported) {
		return fmt.Errorf("%s: %w (it is older than 'shed users status')", name, err)
	}
	return fmt.Errorf("users status on %s: %w", name, err)
}

// renderUsersRefresh prints the directory pull's row, when the server made
// one, then one row per GitHub login, then each added or removed key and
// each error under it.
func renderUsersRefresh(w io.Writer, resp config.UsersRefreshResponse) {
	if resp.Directory != nil {
		renderDirectoryRefresh(w, *resp.Directory)
		if len(resp.Logins) == 0 {
			return
		}
		fmt.Fprintln(w)
	}
	if len(resp.Logins) == 0 {
		fmt.Fprintln(w, "This server follows no GitHub logins.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tGITHUB\tACCOUNT ID\tSTATUS\tKEYS\tADDED\tREMOVED")
	for _, row := range resp.Logins {
		user := row.User
		if user == "" {
			user = "-"
		}
		id := "-"
		if row.GitHubID != 0 {
			id = fmt.Sprintf("%d", row.GitHubID)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\n",
			user, row.Login, id, row.Status, row.Keys, len(row.Added), len(row.Removed))
	}
	tw.Flush()

	var notes []string
	for _, row := range resp.Logins {
		for _, fp := range row.Added {
			notes = append(notes, fmt.Sprintf("%s: added %s", row.Login, fp))
		}
		for _, fp := range row.Removed {
			notes = append(notes, fmt.Sprintf("%s: removed %s", row.Login, fp))
		}
		if row.Status == "mismatch" {
			seen := "no account"
			if row.SeenID != 0 {
				seen = fmt.Sprintf("account %d", row.SeenID)
			}
			notes = append(notes, fmt.Sprintf("%s: pinned to account %d but now names %s; its keys are dropped", row.Login, row.GitHubID, seen))
		}
		if row.Error != "" {
			notes = append(notes, fmt.Sprintf("%s: %s", row.Login, row.Error))
		}
	}
	if len(notes) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, strings.Join(notes, "\n"))
	}
}

// renderDirectoryRefresh prints a directory pull's row: good, with the list
// it brought, or failed, with why; then, when no list is in force after it
// (its trust time ended, it was withdrawn, or none was ever held), that only
// the local admins can log in. A list past its trust time is never shown as
// held.
func renderDirectoryRefresh(w io.Writer, row config.DirectoryRefreshRow) {
	list := listOf(row)
	if row.Status == config.DirectoryPullGood {
		if list == config.DirectoryListInForce {
			fmt.Fprintf(w, "Directory pull: good: %s\n", heldList(row))
			return
		}
		fmt.Fprintln(w, "Directory pull: good")
		fmt.Fprintln(w, noListLine(list))
		return
	}
	reason := row.Error
	if reason == "" {
		reason = "no reason given"
	}
	fmt.Fprintf(w, "Directory pull: %s: %s\n", serverText(row.Status), serverText(reason))
	if list != config.DirectoryListInForce {
		fmt.Fprintln(w, noListLine(list))
		return
	}
	fmt.Fprintf(w, "The list held stays: %s\n", heldList(row))
}

// listOf is whether a list is in force after the pull of row: its List, or,
// from a server that does not say, in force exactly when the row names a
// version.
func listOf(row config.DirectoryRefreshRow) string {
	switch {
	case row.List != "":
		return row.List
	case row.Version != "":
		return config.DirectoryListInForce
	}
	return config.DirectoryListNone
}

// noListLine says that no list is in force after a pull, and why.
func noListLine(list string) string {
	switch list {
	case config.DirectoryListTrustEnded:
		return "The list held is past its trust time: only the local admins can log in."
	case config.DirectoryListWithdrawn:
		return "The list held is withdrawn: only the local admins can log in."
	}
	return "No list held: only the local admins can log in."
}

// heldList is the list in force a directory row describes: its version and
// counts.
func heldList(row config.DirectoryRefreshRow) string {
	return fmt.Sprintf("version %s, %s (%d skipped)", serverText(row.Version), plural(row.Users, "user"), row.Skipped)
}

// renderUsersStatus prints GET /api/users/status for an admin: the source,
// then on a directory server the state of its list, then the users and the
// entries skipped. now is when the trust time left is counted from.
func renderUsersStatus(w io.Writer, st config.UsersStatusResponse, now time.Time) {
	switch st.Source {
	case config.UsersSourceNone:
		fmt.Fprintln(w, "Source: none (no user list: every accepted key is the implicit user, an admin)")
		return
	case config.UsersSourceConfig:
		fmt.Fprintln(w, "Source: config (auth.users)")
	case config.UsersSourceDirectory:
		fmt.Fprintln(w, "Source: directory (an account service's list, beside the local admins)")
	default:
		fmt.Fprintf(w, "Source: %s\n", serverText(st.Source))
	}
	if d := st.Directory; d != nil {
		renderDirectoryStatus(w, *d, now)
	}

	fmt.Fprintln(w)
	if len(st.Users) == 0 {
		fmt.Fprintln(w, "Users: none")
	} else {
		fmt.Fprintln(w, "Users:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  USER\tADMIN\tSOURCE")
		for _, u := range st.Users {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", userName(u), yesNo(u.Admin), serverText(u.Source))
		}
		tw.Flush()
	}

	if d := st.Directory; d != nil {
		fmt.Fprintln(w)
		if len(d.Skipped) == 0 {
			fmt.Fprintln(w, "Skipped entries: none")
			return
		}
		fmt.Fprintf(w, "Skipped entries (%d):\n", len(d.Skipped))
		for _, s := range d.Skipped {
			why := s.Description
			if why == "" {
				why = s.Reason
			}
			fmt.Fprintf(w, "  %q: %s\n", s.Name, serverText(why))
		}
	}
}

// renderDirectoryStatus prints the state of a directory server's list.
func renderDirectoryStatus(w io.Writer, d config.DirectoryStatus, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	row("Account service:", serverText(d.URL))
	row("Organization:", orNone(d.Organization, "not known until enrolment"))
	row("Client id:", orNone(d.ClientID, "none (not enrolled)"))
	state := serverText(d.State)
	if d.Reason != "" {
		state += " (" + serverText(d.Reason) + ")"
	}
	row("State:", state)
	if d.LastGoodPull != nil {
		row("Last good pull:", stampOf(*d.LastGoodPull))
	} else {
		row("Last good pull:", "never")
	}
	row("Version:", orNone(d.Version, "none"))
	row("Trusted until:", trustLine(d, now))
	switch {
	case d.LastAttempt == nil:
		row("Last attempt:", "none yet")
	case d.LastError == "":
		row("Last attempt:", stampOf(*d.LastAttempt)+", good")
	default:
		row("Last attempt:", stampOf(*d.LastAttempt)+", failed")
		row("Last error:", serverText(d.LastError))
	}
	if d.NextAttempt != nil {
		row("Next attempt:", stampOf(*d.NextAttempt))
	} else {
		row("Next attempt:", "due now")
	}
	copyLine := serverText(d.Copy)
	if d.CopyError != "" {
		copyLine += " (" + serverText(d.CopyError) + ")"
	}
	row("Kept copy:", copyLine)
	tw.Flush()
	if len(d.ExpiredLocalAdmins) > 0 {
		fmt.Fprintf(w, "WARNING: every key of every local admin has expired (%s): no local admin can log in, and with the account service unreachable nobody can\n",
			serverText(strings.Join(d.ExpiredLocalAdmins, ", ")))
	}
}

// trustLine is when the list held is trusted until and, while the server
// says it is trusted, about how long that is from now. The server's state is
// authoritative; the time left is an estimate by this machine's clock, so
// beside a trusted state it is never "ended": at or past the instant it is
// "due".
func trustLine(d config.DirectoryStatus, now time.Time) string {
	if d.TrustedUntil == nil {
		return "no list held"
	}
	until := *d.TrustedUntil
	switch d.State {
	case config.DirectoryStateTrusted:
		left := until.Sub(now)
		switch {
		case left <= 0:
			return stampOf(until) + " (due, by this machine's clock)"
		case left < time.Minute:
			return stampOf(until) + " (less than a minute left, by this machine's clock)"
		}
		return stampOf(until) + " (about " + remaining(left) + " left, by this machine's clock)"
	case config.DirectoryStateExpired:
		return stampOf(until) + " (ended)"
	}
	return stampOf(until) + " (not in force)"
}

// remaining is d, a time left of a minute or more, in whole days, hours and
// minutes (rounded down).
func remaining(d time.Duration) string {
	m := int(d / time.Minute)
	days, hours, mins := m/(24*60), m/60%24, m%60
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	return strings.Join(parts, " ")
}

// userName is a user's name for the status table: a name from the account
// service's list is its text, so it is quoted.
func userName(u config.UserStatus) string {
	if u.Source == config.UserFromDirectory {
		return fmt.Sprintf("%q", u.Name)
	}
	return serverText(u.Name)
}

// stampOf is t in UTC, to the second.
func stampOf(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// orNone is s as server text, or none when s is empty.
func orNone(s, none string) string {
	if s == "" {
		return none
	}
	return serverText(s)
}

// plural is "n thing" or "n things".
func plural(n int, thing string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, thing)
	}
	return fmt.Sprintf("%d %ss", n, thing)
}

// The Unicode line and paragraph separators: some terminals break a line at
// them.
const (
	lineSeparator      = rune(0x2028)
	paragraphSeparator = rune(0x2029)
)

// unsafeForTerminal reports whether r must not reach a terminal as itself:
// a C0 or C1 control character or DEL, a format character (the
// bidirectional controls, the zero-width characters and the rest of
// Unicode's Cf), or the Unicode line and paragraph separators.
func unsafeForTerminal(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == lineSeparator || r == paragraphSeparator
}

// serverText is text the server sent (a reason, an error), made safe to
// print on a terminal: every character unsafeForTerminal reports is shown
// escaped, so the text can neither forge a line, drive the terminal, nor
// reorder or hide what is printed around it. Text the account service sent
// inside it is already quoted by the server. Nothing is cut.
func serverText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x80 && unicode.IsControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case unsafeForTerminal(r) && r > 0xffff:
			fmt.Fprintf(&b, `\U%08x`, r)
		case unsafeForTerminal(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// writeTerminalSafeJSON writes v to w as outputJSON does (indented JSON),
// except that every character unsafeForTerminal reports, which the JSON
// encoder leaves as itself (DEL, C1 controls, format characters), is written
// as a \uXXXX escape (a surrogate pair beyond the BMP). The document stays
// valid JSON and decodes to the same strings; only its bytes on the
// terminal change. (The encoder already escapes C0 controls and the line
// and paragraph separators inside strings, and JSON's structure is ASCII, so
// every character escaped here is inside a string.)
func writeTerminalSafeJSON(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	var out bytes.Buffer
	for _, r := range buf.String() {
		if r < 0x7f || !unsafeForTerminal(r) {
			out.WriteRune(r)
			continue
		}
		for _, u := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&out, `\u%04x`, u)
		}
	}
	_, err := w.Write(out.Bytes())
	return err
}
