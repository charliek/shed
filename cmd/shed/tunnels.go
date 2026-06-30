package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/tunnels"
)

// connectTargetFromEntry resolves how `shed forward` dials the Connect API for a
// server entry. When the entry pins a TLS cert on an https api_url, the tunnel
// dials that host:port over pinned TLS and sends the credentials-scoped token
// (the Connect route requires the credentials scope when auth is enforced).
// Otherwise it is the legacy plain-TCP dial to host:http_port, byte-identical.
func connectTargetFromEntry(entry *config.ServerEntry) (tunnels.ConnectTarget, error) {
	if entry.APIURL != "" {
		u, err := url.Parse(entry.APIURL)
		if err != nil {
			return tunnels.ConnectTarget{}, fmt.Errorf("invalid api_url %q: %w", entry.APIURL, err)
		}
		if strings.EqualFold(u.Scheme, "https") {
			if entry.TLSCertFingerprint == "" {
				return tunnels.ConnectTarget{}, fmt.Errorf(
					"server has an https api_url but no tls_cert_fingerprint to pin; re-add with `shed server add --https-port`")
			}
			if u.Port() == "" {
				return tunnels.ConnectTarget{}, fmt.Errorf("api_url %q must include a port (e.g. https://host:8443)", entry.APIURL)
			}
			return tunnels.ConnectTarget{
				Addr:   u.Host,
				TLSPin: entry.TLSCertFingerprint,
				Token:  entry.CredentialsToken,
			}, nil
		}
	}
	return tunnels.ConnectTarget{Addr: fmt.Sprintf("%s:%d", entry.Host, entry.HTTPPort)}, nil
}

var (
	tunnelProfiles   []string
	tunnelPorts      []string
	tunnelBackground bool
	tunnelDaemon     bool
	tunnelReplace    bool
	tunnelStopAll    bool
)

var tunnelsCmd = &cobra.Command{
	Use:   "tunnels",
	Short: "Manage tunnels to sheds",
	Long: `Manage port forwarding tunnels to shed containers.

Tunnels allow you to access services running inside sheds from your
local machine (e.g., web servers, databases, dev servers).

Tunnels use the shed-server Connect API to establish TCP streams
into VMs. Configuration is stored in ~/.shed/tunnels.yaml`,
}

var tunnelsStartCmd = &cobra.Command{
	Use:   "start <shed>",
	Short: "Start tunnels to a shed",
	Long: `Start port forwarding tunnels to a shed.

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
	Short: "Stop tunnels",
	Long: `Stop tunnels for a shed.

Examples:
  shed tunnels stop myproj    # Stop tunnels for myproj
  shed tunnels stop --all     # Stop all tunnels`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTunnelsStop,
}

var tunnelsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List active tunnels",
	Long: `List all active tunnels.

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

Shows the ports that would be forwarded.

Examples:
  shed tunnels config myproj          # Show default profile
  shed tunnels config myproj -p dev   # Show dev profile
  shed tunnels config myproj -t 5432  # Show with extra tunnel`,
	Args: cobra.ExactArgs(1),
	RunE: runTunnelsConfig,
}

func init() {
	tunnelsStartCmd.Flags().StringArrayVarP(&tunnelProfiles, "profile", "p", nil, "Profile to use (repeatable for merging)")
	tunnelsStartCmd.Flags().StringArrayVarP(&tunnelPorts, "tunnel", "t", nil, "Explicit tunnel - \"3000\" or \"3001:3000\" (repeatable)")
	tunnelsStartCmd.Flags().BoolVarP(&tunnelBackground, "background", "d", false, "Run as background daemon")
	tunnelsStartCmd.Flags().BoolVar(&tunnelReplace, "replace", false, "Replace existing tunnel without prompting")
	// --daemon is the internal re-exec marker: the detached worker spawned by -d.
	// Users never set it directly; it tells runTunnelsStart to be the worker
	// rather than fork another one.
	tunnelsStartCmd.Flags().BoolVar(&tunnelDaemon, "daemon", false, "internal: run as the detached daemon worker")
	_ = tunnelsStartCmd.Flags().MarkHidden("daemon")

	tunnelsStopCmd.Flags().BoolVar(&tunnelStopAll, "all", false, "Stop all tunnels")

	tunnelsConfigCmd.Flags().StringArrayVarP(&tunnelProfiles, "profile", "p", nil, "Profile to preview")
	tunnelsConfigCmd.Flags().StringArrayVarP(&tunnelPorts, "tunnel", "t", nil, "Additional tunnels to include")

	tunnelsCmd.AddCommand(tunnelsStartCmd)
	tunnelsCmd.AddCommand(tunnelsStopCmd)
	tunnelsCmd.AddCommand(tunnelsListCmd)
	tunnelsCmd.AddCommand(tunnelsConfigCmd)

	rootCmd.AddCommand(tunnelsCmd)
}

// collectPortMappings gathers port mappings from profiles and explicit ports.
func collectPortMappings(mgr *tunnels.Manager, shedName string, profiles, ports []string, cmdName string) ([]tunnels.PortMapping, string, error) {
	var allPorts []tunnels.PortMapping

	effectiveProfiles := profiles
	if len(profiles) == 0 && len(ports) == 0 {
		effectiveProfiles = []string{"default"}
	}

	profileName := ""
	if len(effectiveProfiles) > 0 {
		for _, profile := range effectiveProfiles {
			mappings, err := mgr.Config().GetPortMappings(shedName, profile)
			if err != nil {
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

	if jsonFlag {
		return fmt.Errorf("--json is not supported for tunnel start")
	}

	serverName, entry, err := findShedServer(shedName)
	if err != nil {
		return err
	}

	// Ensure the shed is running
	client := NewAPIClientFromEntry(entry, clientConfig.GetCreateTimeout())
	if _, err := ensureRunningShed(client, shedName); err != nil {
		return err
	}

	mgr, err := tunnels.NewManager()
	if err != nil {
		return fmt.Errorf("failed to initialize tunnel manager: %w", err)
	}

	if err := mgr.CleanupDeadTunnels(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to clean up dead tunnels: %v\n", err)
	}

	// Check for existing tunnel. Skipped for the detached worker: the parent
	// already handled replacement, and the worker has no stdin to prompt on.
	if !tunnelDaemon {
		if existingEntry, ok := mgr.State().GetTunnel(shedName); ok {
			alive, err := mgr.CheckHealth(shedName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to check tunnel health: %v\n", err)
			}
			if alive {
				if !tunnelReplace {
					if jsonFlag {
						return fmt.Errorf("tunnel already running for %s; use --replace with --json", shedName)
					}
					fmt.Printf("Tunnel already running for %s (profile: %s, PID %d).\n",
						shedName, existingEntry.Profile, existingEntry.PID)
					if stat, err := os.Stdin.Stat(); err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
						return fmt.Errorf("tunnel already running for %s; use --replace in non-interactive mode", shedName)
					}
					if !confirmAction("Replace? [y/N] ") {
						fmt.Println("Cancelled.")
						return nil
					}
				}
				if !jsonFlag {
					fmt.Println("Stopping existing tunnel...")
				}
				if err := mgr.Stop(shedName); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to stop existing tunnel: %v\n", err)
				}
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
	if err := mgr.CheckPortConflicts(allPorts); err != nil {
		return err
	}

	// Resolve how to reach the Connect API: pinned TLS + credentials token when
	// the entry has an https api_url, else the legacy plain-TCP dial. Done here
	// (before the daemon fork) so a misconfigured target fails in the user's
	// terminal; the detached worker re-resolves it from config itself, keeping
	// the credentials token off its command line.
	target, err := connectTargetFromEntry(entry)
	if err != nil {
		return err
	}

	switch {
	case tunnelBackground && !tunnelDaemon:
		// Parent: re-exec a detached worker, wait for it to start, then return.
		return spawnTunnelDaemon(shedName, allPorts)
	case tunnelDaemon:
		// Detached worker: start tunnels, report readiness, block on signal.
		return runDaemonWorker(mgr, shedName, serverName, target, allPorts, profileName)
	default:
		// Foreground mode: start tunnels and block on signal.
		return runForegroundTunnel(mgr, shedName, target, allPorts, profileName)
	}
}

func runForegroundTunnel(mgr *tunnels.Manager, shedName string, target tunnels.ConnectTarget, ports []tunnels.PortMapping, profile string) error {
	activeTunnels, err := mgr.StartTunnels(target, shedName, ports)
	if err != nil {
		return fmt.Errorf("failed to start tunnels: %w", err)
	}

	fmt.Printf("Tunnels to %s (Ctrl+C to stop):\n", shedName)
	for _, t := range activeTunnels {
		pm := t.Port()
		fmt.Printf("  localhost:%d -> %s:%d\n", pm.Local, shedName, pm.Remote)
	}
	fmt.Println()

	// Block until signal
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	// Restore default signal handling before draining, so a second Ctrl+C
	// force-quits instead of being swallowed if a connection is slow to close.
	stop()

	fmt.Println("\nStopping tunnels...")
	for _, t := range activeTunnels {
		t.Stop()
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
			if jsonFlag {
				return outputJSON(ActionResult{Status: "ok", Action: "stopped"})
			}
			fmt.Println("No tunnels running.")
			return nil
		}

		stopped := make([]string, 0)
		for shedName := range allTunnels {
			if err := mgr.Stop(shedName); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to stop tunnel for %s: %v\n", shedName, err)
			} else {
				stopped = append(stopped, shedName)
				if !jsonFlag {
					printSuccess("Stopped tunnel for %s", shedName)
				}
			}
		}
		sort.Strings(stopped)
		if jsonFlag {
			return outputJSON(ActionResult{
				Status: "ok",
				Action: "stopped",
				Details: struct {
					Stopped []string `json:"stopped"`
				}{Stopped: stopped},
			})
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

	if jsonFlag {
		return outputJSON(ActionResult{Status: "ok", Action: "stopped", Name: shedName})
	}
	printSuccess("Stopped tunnel for %s", shedName)
	return nil
}

func runTunnelsList(cmd *cobra.Command, args []string) error {
	mgr, err := tunnels.NewManager()
	if err != nil {
		return fmt.Errorf("failed to initialize tunnel manager: %w", err)
	}

	if err := mgr.CleanupDeadTunnels(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to clean up dead tunnels: %v\n", err)
	}

	allTunnels := mgr.State().GetAllTunnels()
	if allTunnels == nil {
		allTunnels = make(map[string]tunnels.TunnelEntry)
	}

	if jsonFlag {
		return outputJSON(allTunnels)
	}

	if len(allTunnels) == 0 {
		fmt.Println("No tunnels running.")
		return nil
	}

	names := make([]string, 0, len(allTunnels))
	for name := range allTunnels {
		names = append(names, name)
	}
	sort.Strings(names)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if verboseLevel > 0 {
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

	serverName, entry, err := findShedServer(shedName)
	if err != nil {
		return err
	}

	mgr, err := tunnels.NewManager()
	if err != nil {
		return fmt.Errorf("failed to initialize tunnel manager: %w", err)
	}

	allPorts, profileName, err := collectPortMappings(mgr, shedName, tunnelProfiles, tunnelPorts, "config")
	if err != nil {
		return err
	}

	// Show the address the tunnel would actually dial (the https api_url
	// endpoint when the entry is TLS-pinned, else host:http_port).
	target, err := connectTargetFromEntry(entry)
	if err != nil {
		return err
	}
	serverAddr := target.Addr

	if jsonFlag {
		return outputJSON(struct {
			Shed       string                `json:"shed"`
			Server     string                `json:"server"`
			ServerAddr string                `json:"server_addr"`
			TLS        bool                  `json:"tls"`
			Profile    string                `json:"profile"`
			Ports      []tunnels.PortMapping `json:"ports"`
		}{
			Shed:       shedName,
			Server:     serverName,
			ServerAddr: serverAddr,
			TLS:        target.TLSPin != "",
			Profile:    profileName,
			Ports:      allPorts,
		})
	}

	fmt.Printf("Tunnel configuration for %s\n", shedName)
	fmt.Printf("Server: %s (%s)\n", serverName, serverAddr)
	fmt.Printf("Profile: %s\n", profileName)
	fmt.Println()

	fmt.Println("Port mappings:")
	for _, pm := range allPorts {
		fmt.Printf("  localhost:%d -> %s:%d\n", pm.Local, shedName, pm.Remote)
	}

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
