package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Manage shed servers",
	Long:  "Add, remove, and manage shed servers.",
}

var serverAddCmd = &cobra.Command{
	Use:   "add <host>",
	Short: "Add a new server",
	Long: `Add a new shed server by hostname or IP address.

The command will connect to the server to fetch its info and SSH host key,
then save the configuration locally.`,
	Args: cobra.ExactArgs(1),
	RunE: runServerAdd,
}

var serverListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured servers",
	Long:  "List all configured servers and their status.",
	Args:  cobra.NoArgs,
	RunE:  runServerList,
}

var serverRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a server",
	Long:  "Remove a server from the configuration.",
	Args:  cobra.ExactArgs(1),
	RunE:  runServerRemove,
}

var serverSetDefaultCmd = &cobra.Command{
	Use:   "set-default <name>",
	Short: "Set the default server",
	Long:  "Set which server to use by default.",
	Args:  cobra.ExactArgs(1),
	RunE:  runServerSetDefault,
}

var (
	serverAddPort int
	serverAddName string
)

func init() {
	serverAddCmd.Flags().IntVarP(&serverAddPort, "port", "p", 8080, "HTTP port of the server")
	serverAddCmd.Flags().StringVarP(&serverAddName, "name", "n", "", "Name for the server (default: server's hostname)")

	serverCmd.AddCommand(serverAddCmd)
	serverCmd.AddCommand(serverListCmd)
	serverCmd.AddCommand(serverRemoveCmd)
	serverCmd.AddCommand(serverSetDefaultCmd)
}

func runServerAdd(cmd *cobra.Command, args []string) error {
	host := args[0]

	if verboseFlag {
		fmt.Printf("Connecting to %s:%d...\n", host, serverAddPort)
	}

	// Connect and get server info
	client := NewAPIClient(host, serverAddPort)
	info, err := client.GetInfo()
	if err != nil {
		return fmt.Errorf("failed to get server info: %w", err)
	}

	// Get SSH host key
	hostKeyResp, err := client.GetSSHHostKey()
	if err != nil {
		return fmt.Errorf("failed to get SSH host key: %w", err)
	}

	// Determine server name
	name := serverAddName
	if name == "" {
		name = info.Name
	}

	// Check if name already exists
	if _, exists := clientConfig.Servers[name]; exists {
		return fmt.Errorf("server '%s' already exists", name)
	}

	// Add to config
	entry := config.ServerEntry{
		Host:     host,
		HTTPPort: info.HTTPPort,
		SSHPort:  info.SSHPort,
	}
	if err := clientConfig.AddServer(name, entry); err != nil {
		return err
	}

	// Save known host
	if err := config.AddKnownHost(host, info.SSHPort, hostKeyResp.HostKey); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save SSH host key: %v\n", err)
	}

	// Save config
	if err := clientConfig.Save(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	printSuccess("Added server %s (%s:%d)", name, host, info.HTTPPort)
	if clientConfig.DefaultServer == name {
		fmt.Println("  Set as default server")
	}

	return nil
}

func runServerList(cmd *cobra.Command, args []string) error {
	if len(clientConfig.Servers) == 0 {
		fmt.Println("No servers configured.")
		fmt.Println("\nTo add a server:")
		fmt.Println("  shed server add <hostname>")
		return nil
	}

	// Sort server names for consistent output
	names := make([]string, 0, len(clientConfig.Servers))
	for name := range clientConfig.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tHOST\tHTTP\tSSH\tSTATUS\tDEFAULT")

	for _, name := range names {
		entry := clientConfig.Servers[name]

		// Check if server is online
		client := NewAPIClientFromEntry(&entry)
		status := "offline"
		if client.Ping() {
			status = "online"
		}

		// Check if default
		defaultMark := ""
		if name == clientConfig.DefaultServer {
			defaultMark = "*"
		}

		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n",
			name, entry.Host, entry.HTTPPort, entry.SSHPort, status, defaultMark)
	}

	w.Flush()
	return nil
}

func runServerRemove(cmd *cobra.Command, args []string) error {
	name := args[0]

	if err := clientConfig.RemoveServer(name); err != nil {
		return err
	}

	if err := clientConfig.Save(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	printSuccess("Removed server %s", name)
	return nil
}

func runServerSetDefault(cmd *cobra.Command, args []string) error {
	name := args[0]

	if err := clientConfig.SetDefaultServer(name); err != nil {
		return err
	}

	if err := clientConfig.Save(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	printSuccess("Set %s as default server", name)
	return nil
}
