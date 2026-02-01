package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/tunnels"
)

var (
	tunnelProfiles    []string
	tunnelPorts       []string
	tunnelBackground  bool
	tunnelReplace     bool
	tunnelStopAll     bool
	tunnelListVerbose bool
	tunnelListJSON    bool
)

var tunnelsCmd = &cobra.Command{
	Use:   "tunnels",
	Short: "Manage SSH tunnels to sheds",
	Long: `Manage SSH port forwarding tunnels to shed containers.

Tunnels allow you to access services running inside sheds from your
local machine (e.g., web servers, databases, OpenCode).

Configuration is stored in ~/.shed/tunnels.yaml`,
}

var tunnelsStartCmd = &cobra.Command{
	Use:   "start <shed>",
	Short: "Start SSH tunnels to a shed",
	Long: `Start SSH port forwarding tunnels to a shed.

By default, runs in the foreground (Ctrl+C to stop).
Use -d/--background to run as a daemon.

Examples:
  shed tunnels start myproj                    # Start with default profile
  shed tunnels start myproj -p dev             # Use dev profile
  shed tunnels start myproj -t 3000            # Forward port 3000
  shed tunnels start myproj -t 4501:4096       # Forward local 4501 to remote 4096
  shed tunnels start myproj -p dev -t 5432 -d  # Combine profile + extra port, background`,
	Args: cobra.ExactArgs(1),
	RunE: runTunnelsStart,
}

var tunnelsStopCmd = &cobra.Command{
	Use:   "stop [shed]",
	Short: "Stop SSH tunnels",
	Long: `Stop SSH tunnels for a shed.

Examples:
  shed tunnels stop myproj    # Stop tunnels for myproj
  shed tunnels stop --all     # Stop all tunnels`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTunnelsStop,
}

var tunnelsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active tunnels",
	Long: `List all active SSH tunnels.

Examples:
  shed tunnels list           # Basic list
  shed tunnels list -v        # Show port mappings
  shed tunnels list --json    # JSON output`,
	Args: cobra.NoArgs,
	RunE: runTunnelsList,
}

var tunnelsConfigCmd = &cobra.Command{
	Use:   "config <shed>",
	Short: "Preview tunnel configuration",
	Long: `Preview tunnel configuration without starting.

Shows the ports that would be forwarded and the SSH command
that would be executed.

Examples:
  shed tunnels config myproj          # Show default profile
  shed tunnels config myproj -p dev   # Show dev profile
  shed tunnels config myproj -t 5432  # Show with extra tunnel`,
	Args: cobra.ExactArgs(1),
	RunE: runTunnelsConfig,
}

func init() {
	// tunnels start flags
	tunnelsStartCmd.Flags().StringArrayVarP(&tunnelProfiles, "profile", "p", nil, "Profile to use (repeatable for merging)")
	tunnelsStartCmd.Flags().StringArrayVarP(&tunnelPorts, "tunnel", "t", nil, "Explicit tunnel - \"3000\" or \"3001:3000\" (repeatable)")
	tunnelsStartCmd.Flags().BoolVarP(&tunnelBackground, "background", "d", false, "Run as background daemon")
	tunnelsStartCmd.Flags().BoolVar(&tunnelReplace, "replace", false, "Replace existing tunnel without prompting")

	// tunnels stop flags
	tunnelsStopCmd.Flags().BoolVar(&tunnelStopAll, "all", false, "Stop all tunnels")

	// tunnels list flags
	tunnelsListCmd.Flags().BoolVarP(&tunnelListVerbose, "verbose", "v", false, "Show detailed port mappings")
	tunnelsListCmd.Flags().BoolVar(&tunnelListJSON, "json", false, "Output as JSON")

	// tunnels config flags
	tunnelsConfigCmd.Flags().StringArrayVarP(&tunnelProfiles, "profile", "p", nil, "Profile to preview")
	tunnelsConfigCmd.Flags().StringArrayVarP(&tunnelPorts, "tunnel", "t", nil, "Additional tunnels to include")

	// Add subcommands
	tunnelsCmd.AddCommand(tunnelsStartCmd)
	tunnelsCmd.AddCommand(tunnelsStopCmd)
	tunnelsCmd.AddCommand(tunnelsListCmd)
	tunnelsCmd.AddCommand(tunnelsConfigCmd)

	rootCmd.AddCommand(tunnelsCmd)
}

// collectPortMappings gathers port mappings from profiles and explicit ports.
// Returns the port mappings, a profile name string, and any error.
func collectPortMappings(mgr *tunnels.Manager, shedName string, profiles, ports []string, cmdName string) ([]tunnels.PortMapping, string, error) {
	var allPorts []tunnels.PortMapping

	// If no profiles specified and no explicit tunnels, use "default" profile
	effectiveProfiles := profiles
	if len(profiles) == 0 && len(ports) == 0 {
		effectiveProfiles = []string{"default"}
	}

	// Load ports from profiles
	profileName := ""
	if len(effectiveProfiles) > 0 {
		for _, profile := range effectiveProfiles {
			mappings, err := mgr.Config().GetPortMappings(shedName, profile)
			if err != nil {
				// Check if it's just no config for this shed
				if errors.Is(err, tunnels.ErrNoTunnelConfig) {
					printError(fmt.Sprintf("No tunnel config for shed %q", shedName),
						"Add configuration to ~/.shed/tunnels.yaml",
						fmt.Sprintf("Or use -t flag: shed tunnels %s %s -t 3000", cmdName, shedName))
					return nil, "", err
				}
				return nil, "", err
			}
			allPorts = tunnels.MergePortMappings(allPorts, mappings)
		}
		profileName = strings.Join(effectiveProfiles, "+")
	}

	// Add explicit tunnels
	if len(ports) > 0 {
		explicitPorts, err := tunnels.ParsePortMappings(ports)
		if err != nil {
			return nil, "", fmt.Errorf("invalid tunnel port: %w", err)
		}
		allPorts = tunnels.MergePortMappings(allPorts, explicitPorts)
		if profileName != "" {
			profileName += "+explicit"
		} else {
			profileName = "explicit"
		}
	}

	return allPorts, profileName, nil
}

func runTunnelsStart(cmd *cobra.Command, args []string) error {
	shedName := args[0]

	// Find the server hosting this shed
	serverName, entry, err := findShedServer(shedName)
	if err != nil {
		return err
	}

	// Ensure the shed is running (auto-start if stopped)
	client := NewAPIClientFromEntry(entry)
	if _, err := ensureRunningShed(client, shedName); err != nil {
		return err
	}

	// Create tunnel manager
	mgr, err := tunnels.NewManager()
	if err != nil {
		return fmt.Errorf("failed to initialize tunnel manager: %w", err)
	}

	// Clean up dead tunnels first
	if err := mgr.CleanupDeadTunnels(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to clean up dead tunnels: %v\n", err)
	}

	// Check for existing tunnel
	if existingEntry, ok := mgr.State().GetTunnel(shedName); ok {
		// Check if process is still alive
		alive, err := mgr.CheckHealth(shedName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to check tunnel health: %v\n", err)
		}
		if alive {
			if !tunnelReplace {
				// Prompt for replacement
				fmt.Printf("Tunnel already running for %s (profile: %s, PID %d).\n",
					shedName, existingEntry.Profile, existingEntry.PID)

				// Check if stdin is interactive before prompting
				if stat, err := os.Stdin.Stat(); err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
					return fmt.Errorf("tunnel already running for %s; use --replace in non-interactive mode", shedName)
				}

				fmt.Print("Replace? [y/N] ")

				reader := bufio.NewReader(os.Stdin)
				response, _ := reader.ReadString('\n')
				response = strings.TrimSpace(strings.ToLower(response))
				if response != "y" && response != "yes" {
					fmt.Println("Cancelled.")
					return nil
				}
			}

			fmt.Println("Stopping existing tunnel...")
			if err := mgr.Stop(shedName); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to stop existing tunnel: %v\n", err)
			}
		}
	}

	// Collect port mappings
	allPorts, profileName, err := collectPortMappings(mgr, shedName, tunnelProfiles, tunnelPorts, "start")
	if err != nil {
		return err
	}

	if len(allPorts) == 0 {
		return fmt.Errorf("no ports to forward")
	}

	// Check for port conflicts
	if err := mgr.CheckPortConflicts(allPorts); err != nil {
		return err
	}

	// Show what we're doing
	if verboseFlag {
		fmt.Printf("Starting tunnel to %s on %s\n", shedName, serverName)
		fmt.Println("Port mappings:")
		for _, pm := range allPorts {
			fmt.Printf("  localhost:%d -> container:%d\n", pm.Local, pm.Remote)
		}
		fmt.Println()
		fmt.Println("SSH command:")
		fmt.Printf("  %s\n", strings.Join(mgr.BuildSSHArgs(shedName, entry, allPorts), " "))
		fmt.Println()
	}

	if tunnelBackground {
		// Start as daemon
		if err := mgr.StartBackground(shedName, serverName, entry, allPorts, profileName); err != nil {
			return fmt.Errorf("failed to start tunnel: %w", err)
		}
		printSuccess("Tunnel started for %s (profile: %s)", shedName, profileName)
		fmt.Println("Forwarding:")
		for _, pm := range allPorts {
			fmt.Printf("  localhost:%d -> %s:%d\n", pm.Local, shedName, pm.Remote)
		}
	} else {
		// Start in foreground
		fmt.Printf("Starting tunnel to %s (Ctrl+C to stop)...\n", shedName)
		for _, pm := range allPorts {
			fmt.Printf("  localhost:%d -> %s:%d\n", pm.Local, shedName, pm.Remote)
		}
		fmt.Println()

		// This replaces the current process
		if err := mgr.StartForeground(shedName, serverName, entry, allPorts, profileName); err != nil {
			return fmt.Errorf("failed to start tunnel: %w", err)
		}
	}

	return nil
}

func runTunnelsStop(cmd *cobra.Command, args []string) error {
	mgr, err := tunnels.NewManager()
	if err != nil {
		return fmt.Errorf("failed to initialize tunnel manager: %w", err)
	}

	if tunnelStopAll {
		allTunnels := mgr.State().GetAllTunnels()
		if len(allTunnels) == 0 {
			fmt.Println("No tunnels running.")
			return nil
		}

		for shedName := range allTunnels {
			if err := mgr.Stop(shedName); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to stop tunnel for %s: %v\n", shedName, err)
			} else {
				printSuccess("Stopped tunnel for %s", shedName)
			}
		}
		return nil
	}

	if len(args) == 0 {
		return fmt.Errorf("shed name required (or use --all)")
	}

	shedName := args[0]

	if _, ok := mgr.State().GetTunnel(shedName); !ok {
		return fmt.Errorf("no tunnel running for shed %q", shedName)
	}

	if err := mgr.Stop(shedName); err != nil {
		return fmt.Errorf("failed to stop tunnel: %w", err)
	}

	printSuccess("Stopped tunnel for %s", shedName)
	return nil
}

func runTunnelsList(cmd *cobra.Command, args []string) error {
	mgr, err := tunnels.NewManager()
	if err != nil {
		return fmt.Errorf("failed to initialize tunnel manager: %w", err)
	}

	// Clean up dead tunnels first
	if err := mgr.CleanupDeadTunnels(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to clean up dead tunnels: %v\n", err)
	}

	allTunnels := mgr.State().GetAllTunnels()

	if tunnelListJSON {
		return outputTunnelsJSON(allTunnels)
	}

	if len(allTunnels) == 0 {
		fmt.Println("No tunnels running.")
		return nil
	}

	// Sort by shed name
	names := make([]string, 0, len(allTunnels))
	for name := range allTunnels {
		names = append(names, name)
	}
	sort.Strings(names)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	if tunnelListVerbose {
		fmt.Fprintln(w, "SHED\tPROFILE\tPID\tPORTS\tSTARTED\tSERVER")
		for _, name := range names {
			entry := allTunnels[name]
			ports := formatPortMappings(entry.Ports)
			started := formatRelativeTime(entry.StartedAt)
			fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
				name, entry.Profile, entry.PID, ports, started, entry.ServerName)
		}
	} else {
		fmt.Fprintln(w, "SHED\tPROFILE\tPORTS\tSTARTED")
		for _, name := range names {
			entry := allTunnels[name]
			portCount := fmt.Sprintf("%d port(s)", len(entry.Ports))
			started := formatRelativeTime(entry.StartedAt)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				name, entry.Profile, portCount, started)
		}
	}

	w.Flush()
	return nil
}

func runTunnelsConfig(cmd *cobra.Command, args []string) error {
	shedName := args[0]

	// Find the server hosting this shed
	serverName, entry, err := findShedServer(shedName)
	if err != nil {
		return err
	}

	mgr, err := tunnels.NewManager()
	if err != nil {
		return fmt.Errorf("failed to initialize tunnel manager: %w", err)
	}

	// Collect port mappings
	allPorts, profileName, err := collectPortMappings(mgr, shedName, tunnelProfiles, tunnelPorts, "config")
	if err != nil {
		return err
	}

	fmt.Printf("Tunnel configuration for %s\n", shedName)
	fmt.Printf("Server: %s\n", serverName)
	fmt.Printf("Profile: %s\n", profileName)
	fmt.Println()

	fmt.Println("Port mappings:")
	for _, pm := range allPorts {
		fmt.Printf("  localhost:%d -> %s:%d\n", pm.Local, shedName, pm.Remote)
	}
	fmt.Println()

	if verboseFlag {
		fmt.Println("SSH command:")
		fmt.Printf("  %s\n", strings.Join(mgr.BuildSSHArgs(shedName, entry, allPorts), " "))
	}

	return nil
}

func outputTunnelsJSON(entries map[string]tunnels.TunnelEntry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}
	fmt.Println(string(data))
	return nil
}

func formatPortMappings(ports []tunnels.PortMapping) string {
	strs := make([]string, len(ports))
	for i, pm := range ports {
		if pm.Local == pm.Remote {
			strs[i] = fmt.Sprintf("%d", pm.Local)
		} else {
			strs[i] = fmt.Sprintf("%d:%d", pm.Local, pm.Remote)
		}
	}
	return strings.Join(strs, ",")
}

func formatRelativeTime(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		mins := int(d.Minutes())
		if mins == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", mins)
	}
	if d < 24*time.Hour {
		hours := int(d.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	}
	days := int(d.Hours() / 24)
	if days == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", days)
}
