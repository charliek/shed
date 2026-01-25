package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var (
	sessionsAllFlag  bool
	sessionsJSONFlag bool
)

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
	sessionsCmd.Flags().BoolVar(&sessionsJSONFlag, "json", false, "Output as JSON")

	sessionsCmd.AddCommand(sessionsKillCmd)
	rootCmd.AddCommand(sessionsCmd)
}

func runSessions(cmd *cobra.Command, args []string) error {
	var allSessions []config.Session

	if sessionsAllFlag {
		// Query all servers
		for serverName, entry := range clientConfig.Servers {
			client := NewAPIClientFromEntry(&entry)
			sessions, err := client.ListAllSessions()
			if err != nil {
				if verboseFlag {
					fmt.Fprintf(os.Stderr, "Warning: could not query server %s: %v\n", serverName, err)
				}
				continue
			}
			// Add server name to each session
			for i := range sessions {
				sessions[i].ServerName = serverName
			}
			allSessions = append(allSessions, sessions...)
		}
	} else {
		// Query single server
		entry, serverName, err := getServerEntry()
		if err != nil {
			return err
		}
		client := NewAPIClientFromEntry(entry)

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
			sessions, err := client.ListAllSessions()
			if err != nil {
				return fmt.Errorf("failed to list sessions: %w", err)
			}
			for i := range sessions {
				sessions[i].ServerName = serverName
			}
			allSessions = sessions
		}
	}

	if sessionsJSONFlag {
		return printSessionsJSON(allSessions)
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

	client := NewAPIClientFromEntry(entry)
	if err := client.KillSession(shedName, sessionName); err != nil {
		return fmt.Errorf("failed to kill session: %w", err)
	}

	printSuccess("Killed session %q in shed %q", sessionName, shedName)
	return nil
}

func printSessionsTable(sessions []config.Session) error {
	if len(sessions) == 0 {
		fmt.Println("No sessions found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SHED\tSESSION\tSTATUS\tCREATED\tWINDOWS")

	for _, s := range sessions {
		status := "detached"
		if s.Attached {
			status = "attached"
		}

		created := formatTimeAgo(s.CreatedAt)

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n",
			s.ShedName,
			s.Name,
			status,
			created,
			s.WindowCount,
		)
	}

	return w.Flush()
}

func printSessionsJSON(sessions []config.Session) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(sessions)
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
