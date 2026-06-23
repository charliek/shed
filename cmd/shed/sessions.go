package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var sessionsAllFlag bool

var sessionsCmd = &cobra.Command{
	Use:   "sessions [shed-name]",
	Short: "List tmux sessions",
	Long: `List tmux sessions across sheds.

Without arguments, lists all sessions from all running sheds on the default server.
With a shed name, lists sessions only for that shed.

Examples:
  shed sessions                 # List all sessions on default server
  shed sessions myproj          # List sessions in specific shed
  shed sessions --all           # List across all servers
  shed sessions --json          # Output as JSON`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSessions,
}

var sessionsKillCmd = &cobra.Command{
	Use:   "kill <shed-name> <session-name>",
	Short: "Kill a tmux session",
	Long: `Terminate a tmux session in a shed.

Example:
  shed sessions kill myproj debug    # Kill the "debug" session in myproj`,
	Args: cobra.ExactArgs(2),
	RunE: runSessionsKill,
}

func init() {
	sessionsCmd.Flags().BoolVarP(&sessionsAllFlag, "all", "a", false, "List sessions from all servers")

	sessionsCmd.AddCommand(sessionsKillCmd)
	rootCmd.AddCommand(sessionsCmd)
}

func runSessions(cmd *cobra.Command, args []string) error {
	var allSessions []config.Session

	if sessionsAllFlag {
		// Query all servers
		for serverName, entry := range clientConfig.Servers {
			client := NewAPIClientFromEntry(&entry, DefaultTimeout)
			resp, err := client.ListAllSessions()
			if err != nil {
				if verboseLevel > 0 {
					fmt.Fprintf(os.Stderr, "Warning: could not query server %s: %v\n", serverName, err)
				}
				continue
			}
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
	} else {
		// Query single server
		entry, serverName, err := getServerEntry()
		if err != nil {
			return err
		}
		client := NewAPIClientFromEntry(entry, DefaultTimeout)

		if len(args) == 1 {
			// List sessions for a specific shed
			shedName := args[0]
			sessions, err := client.ListSessions(shedName)
			if err != nil {
				return fmt.Errorf("failed to list sessions for %s: %w", shedName, err)
			}
			for i := range sessions {
				sessions[i].ServerName = serverName
			}
			allSessions = sessions
		} else {
			// List all sessions on this server
			resp, err := client.ListAllSessions()
			if err != nil {
				return fmt.Errorf("failed to list sessions: %w", err)
			}
			// Display warnings about sheds that couldn't be queried
			for _, warning := range resp.Warnings {
				fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
			}
			for i := range resp.Sessions {
				resp.Sessions[i].ServerName = serverName
			}
			allSessions = resp.Sessions
		}
	}

	// Enrich rc-* sessions with RC Session Convention metadata in one pass over
	// the assembled list, so every code path above benefits and new branches
	// can't forget it. No-op (no dial) when there are no rc-* sessions.
	enrichSessionsRC(allSessions)

	if jsonFlag {
		if allSessions == nil {
			allSessions = make([]config.Session, 0)
		}
		return outputJSON(allSessions)
	}
	return printSessionsTable(allSessions)
}

func runSessionsKill(cmd *cobra.Command, args []string) error {
	shedName := args[0]
	sessionName := args[1]

	// Find the server hosting this shed
	_, entry, err := findShedServer(shedName)
	if err != nil {
		return err
	}

	client := NewAPIClientFromEntry(entry, DefaultTimeout)
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

func printSessionsTable(sessions []config.Session) error {
	if len(sessions) == 0 {
		fmt.Println("No sessions found")
		return nil
	}

	// Only widen the table with RC columns when at least one rc-* session is
	// present, so the common (non-RC) listing stays compact.
	showRC := false
	for _, s := range sessions {
		if strings.HasPrefix(s.Name, rcTmuxPrefix) {
			showRC = true
			break
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if showRC {
		fmt.Fprintln(w, "SHED\tSESSION\tSTATUS\tCREATED\tWINDOWS\tKIND\tRC-STATE")
	} else {
		fmt.Fprintln(w, "SHED\tSESSION\tSTATUS\tCREATED\tWINDOWS")
	}

	for _, s := range sessions {
		status := "detached"
		if s.Attached {
			status = "attached"
		}

		created := formatTimeAgo(s.CreatedAt)

		if showRC {
			kind, rcState := rcColumns(s)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				s.ShedName, s.Name, status, created, s.WindowCount, kind, rcState)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n",
				s.ShedName, s.Name, status, created, s.WindowCount)
		}
	}

	return w.Flush()
}

// rcColumns renders the KIND and RC-STATE cells for a session. Non-RC sessions
// render blank; rc-* sessions whose metadata couldn't be read render "-"; legacy
// (unmanaged) RC sessions are labelled.
func rcColumns(s config.Session) (kind, state string) {
	if s.RC == nil {
		if strings.HasPrefix(s.Name, rcTmuxPrefix) {
			return "-", "-"
		}
		return "", ""
	}
	kind = s.RC.Kind
	if kind == "" {
		kind = "?"
	}
	if !s.RC.Managed {
		kind += " (legacy)"
	}
	return kind, s.RC.State
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
