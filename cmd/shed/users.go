package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

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

Admin only. Use -s/--server to pick the server.`,
	Args: cobra.NoArgs,
	RunE: runUsersRefresh,
}

func init() {
	usersCmd.AddCommand(usersRefreshCmd)
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
	if jsonFlag {
		return outputJSON(resp)
	}
	renderUsersRefresh(os.Stdout, *resp)
	return nil
}

// renderUsersRefresh prints one row per GitHub login, then each added or
// removed key and each error under it.
func renderUsersRefresh(w io.Writer, resp config.UsersRefreshResponse) {
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
