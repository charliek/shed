// Package main is the entry point for the shed-server daemon.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/version"
)

var (
	// configPath is the path to the server config file
	configPath string
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "shed-server",
	Short: "Shed development environment server",
	Long: `Shed server manages development environment containers.

It provides an HTTP API for managing sheds and an SSH server for connecting to them.`,
	Version: version.FullInfo(),
}

func init() {
	// Global flags
	rootCmd.PersistentFlags().StringVarP(&configPath, "config", "c", "", "path to config file")

	// Override version template
	rootCmd.SetVersionTemplate("{{.Version}}\n")

	// Add subcommands
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(installCmd)
	rootCmd.AddCommand(uninstallCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
