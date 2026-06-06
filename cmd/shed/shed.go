package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/sync"
	"github.com/charliek/shed/internal/tunnels"
	"github.com/charliek/shed/internal/vmimage"
)

var createCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new shed",
	Long: `Create a new shed development environment.

If a repository URL is provided, it will be cloned into the shed.`,
	Args: cobra.ExactArgs(1),
	RunE: runCreate,
}

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List sheds",
	Long:    "List all sheds on the configured servers.",
	Args:    cobra.NoArgs,
	RunE:    runList,
}

var deleteCmd = &cobra.Command{
	Use:     "delete <name>",
	Aliases: []string{"rm"},
	Short:   "Delete a shed",
	Long:    "Delete a shed and optionally its data volume.",
	Args:    cobra.ExactArgs(1),
	RunE:    runDelete,
}

var startCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start a stopped shed",
	Long:  "Start a shed that was previously stopped.",
	Args:  cobra.ExactArgs(1),
	RunE:  runStart,
}

var stopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop a running shed",
	Long:  "Stop a running shed. The shed can be started again later.",
	Args:  cobra.ExactArgs(1),
	RunE:  runStop,
}

var (
	createRepo         string
	createImage        string
	createNoProvision  bool
	createNoSync       bool
	createSyncProfile  string
	createTimeout      time.Duration
	createBackend      string
	createCPUs         int
	createMemory       int
	createLocalDir     string
	createFromSnapshot string
	createUpperSize    string
	startTimeout       time.Duration
	listAll            bool
	deleteKeep         bool
	deleteForce        bool
)

func init() {
	createCmd.Flags().StringVarP(&createRepo, "repo", "r", "", "Git repository (owner/repo shorthand or full URL) to clone")
	createCmd.Flags().StringVarP(&createImage, "image", "i", "", "Image variant to use")
	createCmd.Flags().BoolVar(&createNoProvision, "no-provision", false, "Skip running provisioning hooks")
	createCmd.Flags().BoolVar(&createNoSync, "no-sync", false, "Skip syncing default profile")
	createCmd.Flags().StringVar(&createSyncProfile, "sync-profile", "", "Profile to sync after creation (default: 'default')")
	createCmd.Flags().DurationVar(&createTimeout, "timeout", 0, "Timeout for create operation (default: from config or 10m)")
	createCmd.Flags().StringVar(&createBackend, "backend", "", "Backend to use: firecracker or vz (default: server default)")
	createCmd.Flags().IntVar(&createCPUs, "cpus", 0, "Number of vCPUs (firecracker/vz only)")
	createCmd.Flags().IntVar(&createMemory, "memory", 0, "Memory in MB (firecracker/vz only)")
	createCmd.Flags().StringVar(&createLocalDir, "local-dir", "", "Mount a local directory as the workspace (mutually exclusive with --repo)")
	createCmd.Flags().StringVar(&createFromSnapshot, "from-snapshot", "", "Spawn from a snapshot's rootfs (mutually exclusive with --image and --repo)")
	createCmd.Flags().StringVar(&createUpperSize, "upper-size", "", "Logical size of the per-shed writable upper layer (e.g. 5G, 20G; default: server config)")

	startCmd.Flags().DurationVar(&startTimeout, "timeout", 0, "Timeout for start operation (default: from config or 10m)")

	listCmd.Flags().BoolVarP(&listAll, "all", "a", false, "List sheds from all servers")

	deleteCmd.Flags().BoolVar(&deleteKeep, "keep-volume", false, "Keep the data volume")
	deleteCmd.Flags().BoolVarP(&deleteForce, "force", "f", false, "Delete without confirmation")

	rootCmd.AddCommand(createCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(deleteCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
}

func runCreate(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Validate backend flag
	if createBackend != "" &&
		createBackend != config.BackendFirecracker &&
		createBackend != config.BackendVZ {
		return fmt.Errorf("invalid backend %q: must be %q or %q",
			createBackend, config.BackendFirecracker, config.BackendVZ)
	}

	if createCPUs < 0 {
		return fmt.Errorf("invalid cpus %d: must be at least 0", createCPUs)
	}
	if createMemory < 0 {
		return fmt.Errorf("invalid memory %d: must be at least 0 MB", createMemory)
	}

	// Validate --local-dir flag
	if createLocalDir != "" && createRepo != "" {
		return fmt.Errorf("--local-dir and --repo are mutually exclusive")
	}

	// Validate --from-snapshot flag
	if createFromSnapshot != "" {
		if createImage != "" {
			return fmt.Errorf("--from-snapshot and --image are mutually exclusive (snapshot rootfs is the source of truth)")
		}
		if createRepo != "" {
			return fmt.Errorf("--from-snapshot and --repo are mutually exclusive (snapshot is already provisioned)")
		}
		if err := config.ValidateSnapshotName(createFromSnapshot); err != nil {
			return fmt.Errorf("invalid --from-snapshot name: %w", err)
		}
	}
	if createLocalDir != "" {
		absDir, err := filepath.Abs(createLocalDir)
		if err != nil {
			return fmt.Errorf("invalid local-dir path: %w", err)
		}
		info, err := os.Stat(absDir)
		if err != nil {
			return fmt.Errorf("local-dir %q does not exist: %w", absDir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("local-dir %q is not a directory", absDir)
		}
		if strings.Contains(absDir, ",") {
			return fmt.Errorf("local-dir path must not contain commas (incompatible with VirtioFS device arguments)")
		}
		createLocalDir = absDir
	}

	entry, serverName, err := getServerEntry()
	if err != nil {
		printError("no server configured",
			"shed server add <hostname>  # Add a server first")
		return err
	}

	if verboseLevel > 0 {
		fmt.Printf("Creating shed %s on %s...\n", name, serverName)
	}

	// Determine timeout: CLI flag > config > default
	// Note: --timeout 0 is treated as "not set" and uses config/default value
	timeout := createTimeout
	if timeout == 0 {
		timeout = clientConfig.GetCreateTimeout()
	}

	client := NewAPIClientFromEntry(entry, timeout)

	info, err := client.GetInfo()
	if err != nil {
		return fmt.Errorf("failed to fetch server info: %w", err)
	}

	resolvedBackend, warning, err := resolveCreateBackend(info, createBackend, createCPUs, createMemory)
	if err != nil {
		return err
	}
	if warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}

	var upperSizeBytes int64
	if createUpperSize != "" {
		parsed, err := config.ParseUpperSize(createUpperSize)
		if err != nil {
			return fmt.Errorf("invalid --upper-size: %w", err)
		}
		upperSizeBytes = parsed
	}

	req := &config.CreateShedRequest{
		Name:           name,
		Repo:           createRepo,
		Image:          createImage,
		NoProvision:    createNoProvision,
		Backend:        resolvedBackend,
		CPUs:           createCPUs,
		MemoryMB:       createMemory,
		LocalDir:       createLocalDir,
		FromSnapshot:   createFromSnapshot,
		UpperSizeBytes: upperSizeBytes,
	}

	// On an interactive terminal, the image-pull leg renders as live per-blob
	// bars interleaved with the boot-phase lines; pipe/--json/old server fall
	// back to the plain line stream.
	onProgress, finish, wantBlob := progressSink(jsonFlag)
	shed, err := client.CreateShedWithProgress(req, wantBlob, onProgress)
	finish()
	if err != nil {
		return fmt.Errorf("failed to create shed: %w", err)
	}

	// Cache the shed location
	clientConfig.CacheShed(name, serverName, shed.Status)
	if err := clientConfig.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
	}

	// Auto-sync default profile unless --no-sync is set
	if !createNoSync {
		if err := autoSyncAfterCreate(cmd.Context(), name, entry); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: sync failed: %v\n", err)
		}
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status:  "ok",
			Action:  "created",
			Name:    name,
			Server:  serverName,
			Details: shed,
		})
	}

	printSuccess("Created shed %s on %s (backend: %s)", name, serverName, resolvedBackend)
	fmt.Printf("\nConnect with:\n  shed console %s\n", name)

	return nil
}

func resolveCreateBackend(info *config.ServerInfo, requested string, cpus, memory int) (string, string, error) {
	if info == nil {
		return "", "", fmt.Errorf("server info is required")
	}
	if info.Backend == "" {
		return "", "", fmt.Errorf("server does not advertise a backend")
	}

	backend := requested
	if backend == "" {
		backend = info.Backend
	}

	if backend != info.Backend {
		return "", "", fmt.Errorf("backend %q does not match server backend %q", backend, info.Backend)
	}

	switch backend {
	case config.BackendFirecracker:
		if cpus != 0 && cpus < 1 {
			return "", "", fmt.Errorf("invalid cpus %d: must be at least 1", cpus)
		}
		if memory != 0 && memory < 128 {
			return "", "", fmt.Errorf("invalid memory %d: must be at least 128 MB", memory)
		}
	case config.BackendVZ:
		if cpus != 0 && cpus < 1 {
			return "", "", fmt.Errorf("invalid cpus %d: must be at least 1", cpus)
		}
		if cpus > config.MaxVZCPUs {
			return "", "", fmt.Errorf("invalid cpus %d: must be at most %d", cpus, config.MaxVZCPUs)
		}
		if memory != 0 && memory < 128 {
			return "", "", fmt.Errorf("invalid memory %d: must be at least 128 MB", memory)
		}
		if memory > config.MaxVZMemoryMB {
			return "", "", fmt.Errorf("invalid memory %d: must be at most %d MB", memory, config.MaxVZMemoryMB)
		}
	}

	return backend, "", nil
}

func runList(cmd *cobra.Command, args []string) error {
	entry, serverName, err := getServerEntry()
	if err != nil && !listAll {
		printError("no server configured",
			"shed server add <hostname>  # Add a server first",
			"shed list --all             # List from all servers")
		return err
	}

	type shedWithServer struct {
		shed   config.Shed
		server string
	}

	var allSheds []shedWithServer

	if listAll {
		// Query all servers
		for name, e := range clientConfig.Servers {
			client := NewAPIClientFromEntry(&e, DefaultTimeout)
			resp, err := client.ListSheds()
			if err != nil {
				if verboseLevel > 0 {
					fmt.Fprintf(os.Stderr, "Warning: could not reach %s: %v\n", name, err)
				}
				continue
			}
			for _, shed := range resp.Sheds {
				allSheds = append(allSheds, shedWithServer{shed: shed, server: name})
				// Update cache
				clientConfig.CacheShed(shed.Name, name, shed.Status)
			}
		}
	} else {
		client := NewAPIClientFromEntry(entry, DefaultTimeout)
		resp, err := client.ListSheds()
		if err != nil {
			return fmt.Errorf("failed to list sheds: %w", err)
		}
		for _, shed := range resp.Sheds {
			allSheds = append(allSheds, shedWithServer{shed: shed, server: serverName})
			// Update cache
			clientConfig.CacheShed(shed.Name, serverName, shed.Status)
		}
	}

	// Save updated cache
	if err := clientConfig.Save(); err != nil {
		if verboseLevel > 0 {
			fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
		}
	}

	// Sort by name (applies to both JSON and table output)
	sort.Slice(allSheds, func(i, j int) bool {
		return allSheds[i].shed.Name < allSheds[j].shed.Name
	})

	if jsonFlag {
		type shedJSON struct {
			Name        string                                `json:"name"`
			Server      string                                `json:"server"`
			Status      string                                `json:"status"`
			CreatedAt   time.Time                             `json:"created_at"`
			Backend     string                                `json:"backend,omitempty"`
			IPAddress   string                                `json:"ip_address,omitempty"`
			CPUs        int                                   `json:"cpus,omitempty"`
			MemoryMB    int                                   `json:"memory_mb,omitempty"`
			Repo        string                                `json:"repo,omitempty"`
			PID         int                                   `json:"pid,omitempty"`
			RootfsPath  string                                `json:"rootfs_path,omitempty"`
			LocalDir    string                                `json:"local_dir,omitempty"`
			SSH         string                                `json:"ssh,omitempty"`
			Uptime      string                                `json:"uptime,omitempty"`
			LastHealthy *time.Time                            `json:"last_healthy,omitempty"`
			StartedAt   *time.Time                            `json:"started_at,omitempty"`
			Extensions  map[string]config.ExtensionHealthInfo `json:"extensions,omitempty"`
		}
		result := make([]shedJSON, 0, len(allSheds))
		for _, s := range allSheds {
			sj := shedJSON{
				Name:       s.shed.Name,
				Server:     s.server,
				Status:     s.shed.Status,
				CreatedAt:  s.shed.CreatedAt,
				Backend:    s.shed.Backend,
				IPAddress:  s.shed.IPAddress,
				CPUs:       s.shed.CPUs,
				MemoryMB:   s.shed.MemoryMB,
				Repo:       s.shed.Repo,
				PID:        s.shed.PID,
				RootfsPath: s.shed.RootfsPath,
				LocalDir:   s.shed.LocalDir,
			}
			if s.shed.Status == config.StatusRunning {
				sj.SSH = shedSSHString(s.shed.Name, s.server)
				sj.LastHealthy = s.shed.LastHealthy
				sj.StartedAt = s.shed.StartedAt
				sj.Extensions = s.shed.Extensions
				// Use agent boot time for uptime if available, fall back to creation time
				if s.shed.StartedAt != nil {
					sj.Uptime = formatUptime(*s.shed.StartedAt)
				} else {
					sj.Uptime = formatUptime(s.shed.CreatedAt)
				}
			}
			result = append(result, sj)
		}
		return outputJSON(result)
	}

	if len(allSheds) == 0 {
		fmt.Println("No sheds found.")
		fmt.Println("\nTo create a shed:")
		fmt.Println("  shed create <name>")
		return nil
	}

	// Tier 3: key-value display (verboseLevel >= 2)
	if verboseLevel >= 2 {
		for i, s := range allSheds {
			if i > 0 {
				fmt.Println("---")
			}
			fmt.Printf("Name:           %s\n", s.shed.Name)
			fmt.Printf("Status:         %s\n", s.shed.Status)
			fmt.Printf("Backend:        %s\n", valueOrDash(s.shed.Backend))
			if listAll {
				fmt.Printf("Server:         %s\n", s.server)
			}
			fmt.Printf("Created:        %s\n", s.shed.CreatedAt.Format("2006-01-02 15:04:05"))
			if img := formatShedImage(s.shed); img != "" {
				fmt.Printf("Image:          %s\n", img)
			}
			if s.shed.Status == config.StatusRunning {
				// Use agent boot time for uptime if available
				if s.shed.StartedAt != nil {
					fmt.Printf("Uptime:         %s\n", formatUptime(*s.shed.StartedAt))
				} else {
					fmt.Printf("Uptime:         %s\n", formatUptime(s.shed.CreatedAt))
				}
				// Show health status
				if s.shed.LastHealthy != nil {
					fmt.Printf("Health:         %s ago\n", formatUptime(*s.shed.LastHealthy))
				} else {
					fmt.Printf("Health:         unknown\n")
				}

				// Show extension health (sorted for consistent output)
				if len(s.shed.Extensions) > 0 {
					fmt.Println()
					fmt.Println("Extensions:")
					names := slices.Sorted(maps.Keys(s.shed.Extensions))
					for _, ns := range names {
						ext := s.shed.Extensions[ns]
						fmt.Printf("  %-20s guest=%-8s host=%s\n", ns+":", ext.Guest, ext.Host)
					}
				}
			}

			// Network section
			if s.shed.IPAddress != "" || s.shed.Status == config.StatusRunning {
				fmt.Println()
				fmt.Println("Network:")
				if s.shed.IPAddress != "" {
					fmt.Printf("  IP:           %s\n", s.shed.IPAddress)
				}
				if s.shed.Status == config.StatusRunning {
					fmt.Printf("  SSH:          %s\n", shedSSHString(s.shed.Name, s.server))
				}
			}

			// Resources section
			if s.shed.CPUs > 0 || s.shed.MemoryMB > 0 {
				fmt.Println()
				fmt.Println("Resources:")
				if s.shed.CPUs > 0 {
					fmt.Printf("  CPUs:         %d\n", s.shed.CPUs)
				}
				if s.shed.MemoryMB > 0 {
					fmt.Printf("  Memory:       %d MB\n", s.shed.MemoryMB)
				}
			}

			// Repo / Local Dir
			if s.shed.Repo != "" {
				fmt.Println()
				fmt.Printf("Repo:           %s\n", s.shed.Repo)
			}
			if s.shed.LocalDir != "" {
				fmt.Println()
				fmt.Printf("Local Dir:      %s\n", s.shed.LocalDir)
			}

			// Runtime section
			if s.shed.PID > 0 || s.shed.ContainerID != "" || s.shed.RootfsPath != "" {
				fmt.Println()
				fmt.Println("Runtime:")
				if s.shed.PID > 0 {
					fmt.Printf("  PID:          %d\n", s.shed.PID)
				}
				if s.shed.ContainerID != "" {
					fmt.Printf("  Container ID: %s\n", s.shed.ContainerID)
				}
				if s.shed.RootfsPath != "" {
					fmt.Printf("  Rootfs:       %s\n", s.shed.RootfsPath)
				}
			}
			fmt.Println()
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	// Tier 2: verbose table (verboseLevel == 1)
	if verboseLevel >= 1 {
		if listAll {
			fmt.Fprintln(w, "NAME\tSERVER\tBACKEND\tSTATUS\tSSH\tIP\tRESOURCES\tSOURCE\tUPTIME")
		} else {
			fmt.Fprintln(w, "NAME\tBACKEND\tSTATUS\tSSH\tIP\tRESOURCES\tSOURCE\tUPTIME")
		}
		for _, s := range allSheds {
			ssh := "-"
			uptime := "-"
			if s.shed.Status == config.StatusRunning {
				ssh = shedSSHString(s.shed.Name, s.server)
				if s.shed.StartedAt != nil {
					uptime = formatUptime(*s.shed.StartedAt)
				} else {
					uptime = formatUptime(s.shed.CreatedAt)
				}
			}
			ip := valueOrDash(s.shed.IPAddress)
			resources := "-"
			if s.shed.CPUs > 0 && s.shed.MemoryMB > 0 {
				resources = fmt.Sprintf("%dc/%dMB", s.shed.CPUs, s.shed.MemoryMB)
			} else if s.shed.CPUs > 0 {
				resources = fmt.Sprintf("%dc", s.shed.CPUs)
			} else if s.shed.MemoryMB > 0 {
				resources = fmt.Sprintf("%dMB", s.shed.MemoryMB)
			}
			source := valueOrDash(s.shed.Repo)
			if s.shed.LocalDir != "" {
				source = s.shed.LocalDir
			}
			if listAll {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					s.shed.Name, s.server, valueOrDash(s.shed.Backend), s.shed.Status, ssh, ip, resources, source, uptime)
			} else {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					s.shed.Name, valueOrDash(s.shed.Backend), s.shed.Status, ssh, ip, resources, source, uptime)
			}
		}
		w.Flush()
		return nil
	}

	// Tier 1: default table
	if listAll {
		fmt.Fprintln(w, "NAME\tSERVER\tBACKEND\tSTATUS\tSSH\tCREATED")
	} else {
		fmt.Fprintln(w, "NAME\tBACKEND\tSTATUS\tSSH\tCREATED")
	}

	for _, s := range allSheds {
		created := s.shed.CreatedAt.Format("2006-01-02 15:04")
		ssh := "-"
		if s.shed.Status == config.StatusRunning {
			ssh = shedSSHString(s.shed.Name, s.server)
		}
		if listAll {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				s.shed.Name, s.server, valueOrDash(s.shed.Backend), s.shed.Status, ssh, created)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				s.shed.Name, valueOrDash(s.shed.Backend), s.shed.Status, ssh, created)
		}
	}

	w.Flush()
	return nil
}

// shedSSHString returns the SSH connection string for a shed.
func shedSSHString(shedName, serverName string) string {
	if entry, ok := clientConfig.Servers[serverName]; ok {
		return fmt.Sprintf("%s@%s:%d", shedName, entry.Host, entry.SSHPort)
	}
	return shedName
}

// formatUptime formats the time since creation as a human-readable duration.
func formatUptime(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		return "0m"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

// valueOrDash returns the value if non-empty, otherwise "-".
func valueOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// formatShedImage renders the image a shed runs as "<label> (sha256:short)".
// The label is the variant/ref the shed was created from; the short digest
// pins the exact manifest. Returns "" when neither is known. Use
// `shed image inspect <digest>` for the full ref.
func formatShedImage(s config.Shed) string {
	label := s.Image
	if s.ImageDigest == "" {
		return label
	}
	short := vmimage.ShortDigest(s.ImageDigest)
	if label == "" {
		return short
	}
	return fmt.Sprintf("%s (%s)", label, short)
}

func runDelete(cmd *cobra.Command, args []string) error {
	name := args[0]

	// In JSON mode, require --force to avoid interactive prompts
	if jsonFlag && !deleteForce {
		return fmt.Errorf("--force is required when using --json")
	}

	// Find the server for this shed
	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}

	// Confirm deletion unless --force
	if !deleteForce {
		prompt := fmt.Sprintf("Delete shed %q on %s? ", name, serverName)
		if !deleteKeep {
			prompt += "This will also delete the data volume. "
		}
		prompt += "[y/N] "
		if !confirmAction(prompt) {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	// Stop any associated tunnels first
	stopTunnelsForShed(name)

	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	if err := client.DeleteShed(name, deleteKeep); err != nil {
		return fmt.Errorf("failed to delete shed: %w", err)
	}

	// Remove from cache
	clientConfig.RemoveShedCache(name)
	if err := clientConfig.Save(); err != nil {
		if verboseLevel > 0 {
			fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
		}
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "deleted",
			Name:   name,
			Server: serverName,
		})
	}

	printSuccess("Deleted shed %s", name)
	return nil
}

func runStart(cmd *cobra.Command, args []string) error {
	name := args[0]

	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}

	if verboseLevel > 0 {
		fmt.Printf("Starting shed %s on %s...\n", name, serverName)
	}

	// Determine timeout: CLI flag > config > default
	// Note: --timeout 0 is treated as "not set" and uses config/default value
	timeout := startTimeout
	if timeout == 0 {
		timeout = clientConfig.GetCreateTimeout()
	}

	client := NewAPIClientFromEntry(entry, timeout)
	shed, err := client.StartShed(name)
	if err != nil {
		return fmt.Errorf("failed to start shed: %w", err)
	}

	// Update cache
	clientConfig.CacheShed(name, serverName, shed.Status)
	if err := clientConfig.Save(); err != nil {
		if verboseLevel > 0 {
			fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
		}
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status:  "ok",
			Action:  "started",
			Name:    name,
			Server:  serverName,
			Details: shed,
		})
	}

	printSuccess("Started shed %s", name)
	return nil
}

func runStop(cmd *cobra.Command, args []string) error {
	name := args[0]

	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}

	if verboseLevel > 0 {
		fmt.Printf("Stopping shed %s on %s...\n", name, serverName)
	}

	// Stop any associated tunnels first
	stopTunnelsForShed(name)

	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	shed, err := client.StopShed(name)
	if err != nil {
		return fmt.Errorf("failed to stop shed: %w", err)
	}

	// Update cache
	clientConfig.CacheShed(name, serverName, shed.Status)
	if err := clientConfig.Save(); err != nil {
		if verboseLevel > 0 {
			fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
		}
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status:  "ok",
			Action:  "stopped",
			Name:    name,
			Server:  serverName,
			Details: shed,
		})
	}

	printSuccess("Stopped shed %s", name)
	return nil
}

// stopTunnelsForShed stops any running tunnels for a shed.
// Errors are logged but not returned since tunnel cleanup is best-effort.
func stopTunnelsForShed(name string) {
	mgr, err := tunnels.NewManager()
	if err != nil {
		if verboseLevel > 0 {
			fmt.Fprintf(os.Stderr, "Warning: failed to initialize tunnel manager: %v\n", err)
		}
		return
	}
	if err := mgr.StopAllTunnelsForShed(name); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to stop tunnel for %s: %v\n", name, err)
	} else if verboseLevel > 0 {
		fmt.Printf("Stopped tunnel for %s\n", name)
	}
}

// findShedServer finds which server hosts a shed.
// It first checks the cache, then queries servers if not found.
func findShedServer(name string) (string, *config.ServerEntry, error) {
	// Check cache first
	if cachedServer, err := clientConfig.GetShedServer(name); err == nil {
		entry, err := clientConfig.GetServer(cachedServer)
		if err == nil {
			// Verify the shed still exists
			client := NewAPIClientFromEntry(entry, DefaultTimeout)
			if _, err := client.GetShed(name); err == nil {
				return cachedServer, entry, nil
			}
			// Shed not found on cached server, clear cache and search
			clientConfig.RemoveShedCache(name)
		}
	}

	// If --server flag is set, only check that server
	if serverFlag != "" {
		entry, err := clientConfig.GetServer(serverFlag)
		if err != nil {
			return "", nil, err
		}
		client := NewAPIClientFromEntry(entry, DefaultTimeout)
		if _, err := client.GetShed(name); err != nil {
			printError(fmt.Sprintf("shed %q not found on %s", name, serverFlag),
				"shed list --all       # Find which server has it",
				"shed create "+name+"  # Create a new shed")
			return "", nil, fmt.Errorf("shed %q not found on %s", name, serverFlag)
		}
		return serverFlag, entry, nil
	}

	// Try default server first
	if clientConfig.DefaultServer != "" {
		entry, _ := clientConfig.GetServer(clientConfig.DefaultServer)
		if entry != nil {
			client := NewAPIClientFromEntry(entry, DefaultTimeout)
			if _, err := client.GetShed(name); err == nil {
				clientConfig.CacheShed(name, clientConfig.DefaultServer, "")
				return clientConfig.DefaultServer, entry, nil
			}
		}
	}

	// Search all servers
	for serverName, entry := range clientConfig.Servers {
		if serverName == clientConfig.DefaultServer {
			continue // Already checked
		}
		client := NewAPIClientFromEntry(&entry, DefaultTimeout)
		if _, err := client.GetShed(name); err == nil {
			// Update cache
			clientConfig.CacheShed(name, serverName, "")
			entryCopy := entry
			return serverName, &entryCopy, nil
		}
	}

	// Not found anywhere
	defaultServer := clientConfig.DefaultServer
	if defaultServer == "" {
		defaultServer = "any configured server"
	}
	printError(fmt.Sprintf("shed %q not found on %s", name, defaultServer),
		"shed list --all       # Find which server has it",
		"shed create "+name+"  # Create a new shed")
	return "", nil, fmt.Errorf("shed %q not found", name)
}

// autoSyncAfterCreate syncs files after shed creation.
func autoSyncAfterCreate(ctx context.Context, shedName string, entry *config.ServerEntry) error {
	// Load sync config
	cfg, err := sync.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load sync config: %w", err)
	}

	if cfg.IsEmpty() {
		return nil
	}

	// Determine profile to sync
	profile := createSyncProfile
	if profile == "" {
		profile = cfg.GetDefaultProfile()
		if profile == "" {
			// No default profile, skip silently
			return nil
		}
	} else {
		// Check if explicitly specified profile exists
		if _, err := cfg.GetProfile(profile); err != nil {
			return fmt.Errorf("profile %q not found", profile)
		}
	}

	if !jsonFlag {
		fmt.Printf("Syncing %s profile...\n", profile)
	}

	syncer := sync.NewSyncer(cfg)
	if jsonFlag {
		syncer.SetOutput(io.Discard)
	}
	return syncer.SyncProfile(ctx, profile, shedName, entry, false)
}
