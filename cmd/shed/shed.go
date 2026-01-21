package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
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
	Use:   "list",
	Short: "List sheds",
	Long:  "List all sheds on the configured servers.",
	Args:  cobra.NoArgs,
	RunE:  runList,
}

var deleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a shed",
	Long:  "Delete a shed and optionally its data volume.",
	Args:  cobra.ExactArgs(1),
	RunE:  runDelete,
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
	createRepo   string
	createImage  string
	listAll      bool
	deleteKeep   bool
	deleteForce  bool
)

func init() {
	createCmd.Flags().StringVarP(&createRepo, "repo", "r", "", "Git repository URL to clone")
	createCmd.Flags().StringVarP(&createImage, "image", "i", "", "Docker image to use")

	listCmd.Flags().BoolVarP(&listAll, "all", "a", false, "List sheds from all servers")

	deleteCmd.Flags().BoolVar(&deleteKeep, "keep-volume", false, "Keep the data volume")
	deleteCmd.Flags().BoolVarP(&deleteForce, "force", "f", false, "Delete without confirmation")
}

func runCreate(cmd *cobra.Command, args []string) error {
	name := args[0]

	entry, serverName, err := getServerEntry()
	if err != nil {
		printError("no server configured",
			"shed server add <hostname>  # Add a server first")
		return err
	}

	if verboseFlag {
		fmt.Printf("Creating shed %s on %s...\n", name, serverName)
	}

	client := NewAPIClientFromEntry(entry)
	req := &config.CreateShedRequest{
		Name:  name,
		Repo:  createRepo,
		Image: createImage,
	}

	shed, err := client.CreateShed(req)
	if err != nil {
		return fmt.Errorf("failed to create shed: %w", err)
	}

	// Cache the shed location
	clientConfig.CacheShed(name, serverName, shed.Status)
	if err := clientConfig.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
	}

	printSuccess("Created shed %s on %s", name, serverName)
	fmt.Printf("\nConnect with:\n  shed console %s\n", name)

	return nil
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
			client := NewAPIClientFromEntry(&e)
			resp, err := client.ListSheds()
			if err != nil {
				if verboseFlag {
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
		client := NewAPIClientFromEntry(entry)
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
		if verboseFlag {
			fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
		}
	}

	if len(allSheds) == 0 {
		fmt.Println("No sheds found.")
		fmt.Println("\nTo create a shed:")
		fmt.Println("  shed create <name>")
		return nil
	}

	// Sort by name
	sort.Slice(allSheds, func(i, j int) bool {
		return allSheds[i].shed.Name < allSheds[j].shed.Name
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if listAll {
		fmt.Fprintln(w, "NAME\tSERVER\tSTATUS\tCREATED")
	} else {
		fmt.Fprintln(w, "NAME\tSTATUS\tCREATED")
	}

	for _, s := range allSheds {
		created := s.shed.CreatedAt.Format("2006-01-02 15:04")
		if listAll {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.shed.Name, s.server, s.shed.Status, created)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\n", s.shed.Name, s.shed.Status, created)
		}
	}

	w.Flush()
	return nil
}

func runDelete(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Find the server for this shed
	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}

	// Confirm deletion unless --force
	if !deleteForce {
		fmt.Printf("Delete shed %q on %s? ", name, serverName)
		if !deleteKeep {
			fmt.Print("This will also delete the data volume. ")
		}
		fmt.Print("[y/N] ")

		reader := bufio.NewReader(os.Stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))
		if response != "y" && response != "yes" {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	client := NewAPIClientFromEntry(entry)
	if err := client.DeleteShed(name, deleteKeep); err != nil {
		return fmt.Errorf("failed to delete shed: %w", err)
	}

	// Remove from cache
	clientConfig.RemoveShedCache(name)
	if err := clientConfig.Save(); err != nil {
		if verboseFlag {
			fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
		}
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

	if verboseFlag {
		fmt.Printf("Starting shed %s on %s...\n", name, serverName)
	}

	client := NewAPIClientFromEntry(entry)
	shed, err := client.StartShed(name)
	if err != nil {
		return fmt.Errorf("failed to start shed: %w", err)
	}

	// Update cache
	clientConfig.CacheShed(name, serverName, shed.Status)
	if err := clientConfig.Save(); err != nil {
		if verboseFlag {
			fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
		}
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

	if verboseFlag {
		fmt.Printf("Stopping shed %s on %s...\n", name, serverName)
	}

	client := NewAPIClientFromEntry(entry)
	shed, err := client.StopShed(name)
	if err != nil {
		return fmt.Errorf("failed to stop shed: %w", err)
	}

	// Update cache
	clientConfig.CacheShed(name, serverName, shed.Status)
	if err := clientConfig.Save(); err != nil {
		if verboseFlag {
			fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
		}
	}

	printSuccess("Stopped shed %s", name)
	return nil
}

// findShedServer finds which server hosts a shed.
// It first checks the cache, then queries servers if not found.
func findShedServer(name string) (string, *config.ServerEntry, error) {
	// Check cache first
	if cachedServer, err := clientConfig.GetShedServer(name); err == nil {
		entry, err := clientConfig.GetServer(cachedServer)
		if err == nil {
			// Verify the shed still exists
			client := NewAPIClientFromEntry(entry)
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
		client := NewAPIClientFromEntry(entry)
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
			client := NewAPIClientFromEntry(entry)
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
		client := NewAPIClientFromEntry(&entry)
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
