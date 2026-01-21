package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var uninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove shed-server systemd service",
	Long: `Remove the shed-server systemd service.

This command stops the service, disables it, removes the unit file,
and reloads the systemd daemon. Requires root privileges.`,
	RunE: runUninstall,
}

func runUninstall(cmd *cobra.Command, args []string) error {
	// Check for root privileges
	if os.Geteuid() != 0 {
		return fmt.Errorf("this command must be run as root (try: sudo shed-server uninstall)")
	}

	fmt.Println("Uninstalling shed-server systemd service...")

	// Stop the service (ignore errors - service might not be running)
	fmt.Println("Stopping shed-server service...")
	if err := runSystemctl("stop", "shed-server"); err != nil {
		fmt.Printf("Note: Could not stop service (may not be running): %v\n", err)
	}

	// Disable the service (ignore errors - service might not be enabled)
	fmt.Println("Disabling shed-server service...")
	if err := runSystemctl("disable", "shed-server"); err != nil {
		fmt.Printf("Note: Could not disable service (may not be enabled): %v\n", err)
	}

	// Remove the unit file
	if _, err := os.Stat(systemdUnitPath); err == nil {
		if err := os.Remove(systemdUnitPath); err != nil {
			return fmt.Errorf("failed to remove unit file: %w", err)
		}
		fmt.Printf("Removed systemd unit file: %s\n", systemdUnitPath)
	} else {
		fmt.Println("Note: Unit file does not exist")
	}

	// Reload systemd daemon
	if err := runSystemctl("daemon-reload"); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}
	fmt.Println("Reloaded systemd daemon")

	fmt.Println()
	fmt.Println("Uninstallation complete!")
	fmt.Println()
	fmt.Println("Note: The following were NOT removed:")
	fmt.Println("  - /etc/shed/ directory and host key")
	fmt.Println("  - Configuration files")
	fmt.Println("  - Docker volumes and containers")
	fmt.Println()
	fmt.Println("To remove these manually:")
	fmt.Println("  sudo rm -rf /etc/shed")
	fmt.Println("  rm -rf ~/.config/shed")

	return nil
}
