// Package main is the entry point for the shed CLI.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/version"
)

var (
	// Global flags
	serverFlag  string
	verboseFlag bool
	configFlag  string

	// Loaded configuration
	clientConfig *config.ClientConfig
)

var rootCmd = &cobra.Command{
	Use:   "shed",
	Short: "Manage remote development environments",
	Long: `Shed manages remote development environments running on shed servers.

Use shed to create, manage, and connect to development containers
on one or more shed servers.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Skip config loading for version command
		if cmd.Name() == "version" {
			return nil
		}

		var err error
		if configFlag != "" {
			clientConfig, err = config.LoadClientConfigFromPath(configFlag)
		} else {
			clientConfig, err = config.LoadClientConfig()
		}
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		return nil
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		if verboseFlag {
			fmt.Println(version.FullInfo())
		} else {
			fmt.Printf("shed %s\n", version.Info())
		}
	},
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().StringVarP(&serverFlag, "server", "s", "", "Server to use (default: configured default)")
	rootCmd.PersistentFlags().BoolVarP(&verboseFlag, "verbose", "v", false, "Enable verbose output")
	rootCmd.PersistentFlags().StringVarP(&configFlag, "config", "c", "", "Path to config file")

	// Add subcommands
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(serverCmd)
	rootCmd.AddCommand(createCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(deleteCmd)
	rootCmd.AddCommand(startCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(consoleCmd)
	rootCmd.AddCommand(execCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// getServerEntry returns the server entry based on --server flag or default.
func getServerEntry() (*config.ServerEntry, string, error) {
	if serverFlag != "" {
		entry, err := clientConfig.GetServer(serverFlag)
		if err != nil {
			return nil, "", err
		}
		return entry, serverFlag, nil
	}
	return clientConfig.GetDefaultServer()
}

// printSuccess prints a success message with a checkmark.
func printSuccess(format string, args ...interface{}) {
	fmt.Printf("\u2713 "+format+"\n", args...)
}

// printError prints an error message with suggestions.
func printError(msg string, suggestions ...string) {
	fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
	if len(suggestions) > 0 {
		fmt.Fprintln(os.Stderr, "\nTry:")
		for _, s := range suggestions {
			fmt.Fprintf(os.Stderr, "  %s\n", s)
		}
	}
}

// requireRunningShed gets a shed and verifies it's running.
// Returns the shed if running, or prints a helpful error and returns an error if not.
func requireRunningShed(client *APIClient, name string) (*config.Shed, error) {
	shed, err := client.GetShed(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get shed status: %w", err)
	}

	if shed.Status != config.StatusRunning {
		printError(fmt.Sprintf("shed %q is %s", name, shed.Status),
			"shed start "+name+"  # Start the shed first")
		return nil, fmt.Errorf("shed %q is not running", name)
	}

	return shed, nil
}
