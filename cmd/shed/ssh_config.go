package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/sshconfig"
)

var sshConfigCmd = &cobra.Command{
	Use:   "ssh-config [name]",
	Short: "Manage SSH config for sheds",
	Long: `Manage SSH config entries for connecting to sheds.

Without flags, prints the SSH config for a shed (or all sheds with --all).
Use --install to add entries to ~/.ssh/config.
Use --dry-run to preview changes without applying them.
Use --uninstall to remove all shed-managed entries.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSSHConfig,
}

var (
	sshConfigAll       bool
	sshConfigInstall   bool
	sshConfigDryRun    bool
	sshConfigUninstall bool
)

func init() {
	sshConfigCmd.Flags().BoolVarP(&sshConfigAll, "all", "a", false, "Generate config for all sheds")
	sshConfigCmd.Flags().BoolVar(&sshConfigInstall, "install", false, "Install entries to ~/.ssh/config")
	sshConfigCmd.Flags().BoolVar(&sshConfigDryRun, "dry-run", false, "Show what would be changed without making changes")
	sshConfigCmd.Flags().BoolVar(&sshConfigUninstall, "uninstall", false, "Remove all shed-managed entries from ~/.ssh/config")

	rootCmd.AddCommand(sshConfigCmd)
}

func runSSHConfig(cmd *cobra.Command, args []string) error {
	// Handle uninstall
	if sshConfigUninstall {
		return runSSHConfigUninstall()
	}

	// Determine which sheds to generate config for
	var sheds []shedInfo
	var err error

	if len(args) > 0 {
		// Specific shed requested
		sheds, err = getShedInfo(args[0])
	} else if sshConfigAll {
		// All sheds
		sheds, err = getAllShedsInfo()
	} else {
		// Default: all sheds
		sheds, err = getAllShedsInfo()
	}

	if err != nil {
		return err
	}

	if len(sheds) == 0 {
		fmt.Println("No sheds found.")
		fmt.Println("\nTo create a shed:")
		fmt.Println("  shed create <name>")
		return nil
	}

	// Generate entries
	entries := generateEntries(sheds)

	// Handle different modes
	if sshConfigInstall || sshConfigDryRun {
		return runSSHConfigInstall(entries)
	}

	// Default: print config
	printSSHConfig(entries)
	return nil
}

type shedInfo struct {
	name       string
	serverName string
	server     *config.ServerEntry
}

func getShedInfo(name string) ([]shedInfo, error) {
	serverName, entry, err := findShedServer(name)
	if err != nil {
		return nil, err
	}

	return []shedInfo{
		{
			name:       name,
			serverName: serverName,
			server:     entry,
		},
	}, nil
}

func getAllShedsInfo() ([]shedInfo, error) {
	var result []shedInfo

	// Query all servers for their sheds
	for serverName, entry := range clientConfig.Servers {
		entryCopy := entry
		client := NewAPIClientFromEntry(&entryCopy)
		resp, err := client.ListSheds()
		if err != nil {
			if verboseFlag {
				fmt.Fprintf(os.Stderr, "Warning: could not reach %s: %v\n", serverName, err)
			}
			continue
		}

		for _, shed := range resp.Sheds {
			result = append(result, shedInfo{
				name:       shed.Name,
				serverName: serverName,
				server:     &entryCopy,
			})
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

	// Sort by name for consistent output
	sort.Slice(result, func(i, j int) bool {
		return result[i].name < result[j].name
	})

	return result, nil
}

func generateEntries(sheds []shedInfo) []sshconfig.Entry {
	entries := make([]sshconfig.Entry, 0, len(sheds))
	knownHostsPath := config.GetKnownHostsPath()

	for _, shed := range sheds {
		entry := sshconfig.Entry{
			Name:           "shed-" + shed.name,
			Host:           shed.server.Host,
			Port:           shed.server.SSHPort,
			User:           shed.name,
			KnownHostsFile: knownHostsPath,
		}
		entries = append(entries, entry)
	}

	return entries
}

func printSSHConfig(entries []sshconfig.Entry) {
	for i, entry := range entries {
		fmt.Print(sshconfig.GenerateEntry(entry))
		if i < len(entries)-1 {
			fmt.Println()
		}
	}
}

func runSSHConfigInstall(entries []sshconfig.Entry) error {
	sshConfigPath := sshconfig.GetSSHConfigPath()

	// Read existing config
	var content string
	data, err := os.ReadFile(sshConfigPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read SSH config: %w", err)
		}
		// File doesn't exist, start fresh
		content = ""
	} else {
		content = string(data)
	}

	// Parse existing config
	parsed := sshconfig.Parse(content)

	// Compute diff
	diff := sshconfig.ComputeDiff(parsed.ManagedEntries, entries)

	// Show diff
	if !diff.HasChanges() && parsed.HasManagedBlock {
		fmt.Println("SSH config is already up to date.")
		return nil
	}

	fmt.Printf("SSH config file: %s\n\n", sshConfigPath)

	if len(diff.Additions) > 0 {
		fmt.Println("Entries to add:")
		for _, name := range diff.Additions {
			fmt.Printf("  + %s\n", name)
		}
	}

	if len(diff.Removals) > 0 {
		fmt.Println("Entries to remove:")
		for _, name := range diff.Removals {
			fmt.Printf("  - %s\n", name)
		}
	}

	if len(diff.Unchanged) > 0 && verboseFlag {
		fmt.Println("Entries unchanged:")
		for _, name := range diff.Unchanged {
			fmt.Printf("  = %s\n", name)
		}
	}

	if !parsed.HasManagedBlock && len(entries) > 0 {
		fmt.Println("\nManaged block will be created.")
	}

	// Dry run - stop here
	if sshConfigDryRun {
		fmt.Println("\n(dry run - no changes made)")
		return nil
	}

	// Write the config
	err = sshconfig.Write(sshConfigPath, parsed.BeforeBlock, entries, parsed.AfterBlock)
	if err != nil {
		return fmt.Errorf("failed to write SSH config: %w", err)
	}

	printSuccess("Updated SSH config at %s", sshConfigPath)

	// Print usage hint
	if len(entries) > 0 {
		fmt.Println("\nYou can now connect to sheds with:")
		for _, entry := range entries {
			fmt.Printf("  ssh %s\n", entry.Name)
		}
	}

	return nil
}

func runSSHConfigUninstall() error {
	sshConfigPath := sshconfig.GetSSHConfigPath()

	// Read existing config
	data, err := os.ReadFile(sshConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No SSH config file found.")
			return nil
		}
		return fmt.Errorf("failed to read SSH config: %w", err)
	}

	// Parse existing config
	parsed := sshconfig.Parse(string(data))

	if !parsed.HasManagedBlock {
		fmt.Println("No shed-managed entries found in SSH config.")
		return nil
	}

	// Show what will be removed
	fmt.Printf("SSH config file: %s\n\n", sshConfigPath)
	fmt.Println("Entries to remove:")
	for _, entry := range parsed.ManagedEntries {
		fmt.Printf("  - %s\n", entry.Name)
	}

	// Dry run - stop here
	if sshConfigDryRun {
		fmt.Println("\n(dry run - no changes made)")
		return nil
	}

	// Remove the managed block
	err = sshconfig.Remove(sshConfigPath)
	if err != nil {
		return fmt.Errorf("failed to update SSH config: %w", err)
	}

	printSuccess("Removed shed-managed entries from %s", sshConfigPath)
	return nil
}
