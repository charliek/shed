// Package main is the entry point for the shed CLI.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/version"
)

var (
	// Global flags
	serverFlag   string
	verboseLevel int
	configFlag   string
	jsonFlag     bool

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
		// JSON mode suppresses verbose output to keep stdout machine-readable
		if jsonFlag {
			verboseLevel = 0
		}

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
	RunE: func(cmd *cobra.Command, args []string) error {
		if jsonFlag {
			return outputJSON(struct {
				Version   string `json:"version"`
				GitCommit string `json:"git_commit"`
				BuildDate string `json:"build_date"`
			}{
				Version:   version.Version,
				GitCommit: version.GitCommit,
				BuildDate: version.BuildDate,
			})
		}
		if verboseLevel > 0 {
			fmt.Println(version.FullInfo())
		} else {
			fmt.Printf("shed %s\n", version.Info())
		}
		return nil
	},
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().StringVarP(&serverFlag, "server", "s", "", "Server to use (default: configured default)")
	rootCmd.PersistentFlags().CountVarP(&verboseLevel, "verbose", "v", "Increase verbosity (-v, -vv)")
	rootCmd.PersistentFlags().StringVarP(&configFlag, "config", "c", "", "Path to config file")
	rootCmd.PersistentFlags().BoolVar(&jsonFlag, "json", false, "Output as JSON")

	// Add subcommand defined in this file
	rootCmd.AddCommand(versionCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		if jsonFlag {
			_ = outputError(err)
		} else {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
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

// confirmAction prints a prompt and returns true if the user confirms with y/yes.
func confirmAction(prompt string) bool {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	response, _ := reader.ReadString('\n')
	response = strings.TrimSpace(strings.ToLower(response))
	return response == "y" || response == "yes"
}

// printSuccess prints a success message with a checkmark.
// In JSON mode, this is a no-op; callers emit structured output instead.
func printSuccess(format string, args ...interface{}) {
	if jsonFlag {
		return
	}
	fmt.Printf("✓ "+format+"\n", args...)
}

// printError prints an error message with suggestions.
// In JSON mode, this is a no-op; errors are returned and handled by main().
func printError(msg string, suggestions ...string) {
	if jsonFlag {
		return
	}
	fmt.Fprintf(os.Stderr, "Error: %s\n", msg)
	if len(suggestions) > 0 {
		fmt.Fprintln(os.Stderr, "\nTry:")
		for _, s := range suggestions {
			fmt.Fprintf(os.Stderr, "  %s\n", s)
		}
	}
}

// ensureRunningShed gets a shed and starts it if not running.
// Returns the shed after ensuring it's running.
func ensureRunningShed(client *APIClient, name string) (*config.Shed, error) {
	shed, err := client.GetShed(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get shed status: %w", err)
	}

	if shed.Status == config.StatusRunning {
		return shed, nil
	}

	// Auto-start the shed
	if !jsonFlag {
		fmt.Printf("Starting shed %s...\n", name)
	}
	shed, err = client.StartShed(name)
	if err != nil {
		return nil, fmt.Errorf("failed to start shed: %w", err)
	}
	if !jsonFlag {
		printSuccess("Shed %s started", name)
	}
	return shed, nil
}
