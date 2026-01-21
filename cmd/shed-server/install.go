package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const (
	// systemdUnitPath is where the systemd unit file will be installed
	systemdUnitPath = "/etc/systemd/system/shed-server.service"

	// defaultBinaryPath is the expected location of the shed-server binary
	defaultBinaryPath = "/usr/local/bin/shed-server"
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install shed-server as a systemd service",
	Long: `Install shed-server as a systemd service.

This command creates a systemd unit file and enables the service.
Requires root privileges.`,
	RunE: runInstall,
}

func runInstall(cmd *cobra.Command, args []string) error {
	// Check for root privileges
	if os.Geteuid() != 0 {
		return fmt.Errorf("this command must be run as root (try: sudo shed-server install)")
	}

	// Get current user info (the user who invoked sudo)
	currentUser, err := getCurrentUser()
	if err != nil {
		return fmt.Errorf("failed to get current user: %w", err)
	}

	fmt.Printf("Installing shed-server as systemd service for user %s...\n", currentUser.Username)

	// Generate systemd unit file content
	unitContent := generateUnitFile(currentUser)

	// Ensure the directory exists
	unitDir := filepath.Dir(systemdUnitPath)
	if err := os.MkdirAll(unitDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", unitDir, err)
	}

	// Write the unit file
	if err := os.WriteFile(systemdUnitPath, []byte(unitContent), 0644); err != nil {
		return fmt.Errorf("failed to write unit file: %w", err)
	}
	fmt.Printf("Created systemd unit file: %s\n", systemdUnitPath)

	// Ensure /etc/shed directory exists for host key
	if err := os.MkdirAll("/etc/shed", 0755); err != nil {
		return fmt.Errorf("failed to create /etc/shed directory: %w", err)
	}
	fmt.Println("Created /etc/shed directory")

	// Run systemctl daemon-reload
	if err := runSystemctl("daemon-reload"); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}
	fmt.Println("Reloaded systemd daemon")

	// Enable the service
	if err := runSystemctl("enable", "shed-server"); err != nil {
		return fmt.Errorf("failed to enable service: %w", err)
	}
	fmt.Println("Enabled shed-server service")

	fmt.Println()
	fmt.Println("Installation complete!")
	fmt.Println()
	fmt.Println("To start the service now, run:")
	fmt.Println("  sudo systemctl start shed-server")
	fmt.Println()
	fmt.Println("To check the service status:")
	fmt.Println("  sudo systemctl status shed-server")
	fmt.Println()
	fmt.Println("To view logs:")
	fmt.Println("  sudo journalctl -u shed-server -f")

	return nil
}

// getCurrentUser returns the user who invoked sudo, or the current user if not running under sudo.
func getCurrentUser() (*user.User, error) {
	// Check for SUDO_USER environment variable
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser != "" {
		return user.Lookup(sudoUser)
	}

	// Fall back to current user
	return user.Current()
}

// generateUnitFile generates the systemd unit file content.
func generateUnitFile(u *user.User) string {
	// Get primary group for user
	group, err := user.LookupGroupId(u.Gid)
	groupName := u.Gid
	if err == nil {
		groupName = group.Name
	}

	template := `[Unit]
Description=Shed Development Environment Server
After=network.target docker.service
Requires=docker.service

[Service]
Type=simple
User={user}
Group={group}
ExecStart={binary} serve
Restart=on-failure
RestartSec=5
Environment=HOME={home}

[Install]
WantedBy=multi-user.target
`

	// Replace placeholders
	content := template
	content = strings.ReplaceAll(content, "{user}", u.Username)
	content = strings.ReplaceAll(content, "{group}", groupName)
	content = strings.ReplaceAll(content, "{binary}", defaultBinaryPath)
	content = strings.ReplaceAll(content, "{home}", u.HomeDir)

	return content
}

// runSystemctl runs a systemctl command with the given arguments.
func runSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
