package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var execSessionFlag string

var consoleCmd = &cobra.Command{
	Use:   "console <name>",
	Short: "Open an interactive console to a shed",
	Long: `Open an interactive SSH console to a shed.

This command replaces the current process with an SSH connection
to the specified shed. For tmux session support, use "shed attach" instead.`,
	Args: cobra.ExactArgs(1),
	RunE: runConsole,
}

var execCmd = &cobra.Command{
	Use:   "exec <name> <command...>",
	Short: "Execute a command in a shed",
	Long: `Execute a command in a shed via SSH.

This command replaces the current process with an SSH connection
that runs the specified command.

Use --session to run the command in the context of an existing tmux session.

Examples:
  shed exec myproj git status                     # Direct execution
  shed exec myproj --session default git status   # Run in tmux session`,
	Args: cobra.MinimumNArgs(2),
	RunE: runExec,
}

func init() {
	execCmd.Flags().StringVarP(&execSessionFlag, "session", "S", "", "Run command in tmux session context")
}

func runConsole(cmd *cobra.Command, args []string) error {
	name := args[0]
	return sshToShed(name, nil)
}

func runExec(cmd *cobra.Command, args []string) error {
	name := args[0]
	command := args[1:]

	// If --session flag is provided, wrap command in tmux send-keys
	if execSessionFlag != "" {
		if err := config.ValidateSessionName(execSessionFlag); err != nil {
			return fmt.Errorf("invalid session name: %w", err)
		}
		// Use tmux send-keys to run the command in the session
		// This sends the command to the session and presses Enter
		escapedCmd := strings.Join(command, " ")
		tmuxCmd := fmt.Sprintf("tmux send-keys -t %s '%s' Enter", execSessionFlag, escapedCmd)
		command = []string{"sh", "-c", tmuxCmd}
	}

	return sshToShed(name, command)
}

// sshToShed establishes an SSH connection to a shed.
// If command is nil, an interactive shell is opened.
// If command is provided, it is executed on the shed.
func sshToShed(name string, command []string) error {
	// Find the server hosting this shed
	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}

	// Verify the shed is running
	client := NewAPIClientFromEntry(entry)
	shed, err := client.GetShed(name)
	if err != nil {
		return fmt.Errorf("failed to get shed status: %w", err)
	}

	if shed.Status != config.StatusRunning {
		printError(fmt.Sprintf("shed %q is %s", name, shed.Status),
			"shed start "+name+"  # Start the shed first")
		return fmt.Errorf("shed %q is not running", name)
	}

	if verboseFlag {
		fmt.Printf("Connecting to %s on %s...\n", name, serverName)
	}

	// Build SSH command
	knownHostsPath := config.GetKnownHostsPath()

	sshArgs := []string{
		"ssh",
		"-t", // Force pseudo-terminal allocation
		"-p", strconv.Itoa(entry.SSHPort),
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"-o", "StrictHostKeyChecking=yes",
		name + "@" + entry.Host,
	}

	// Add command if provided
	if len(command) > 0 {
		sshArgs = append(sshArgs, command...)
	}

	// Find ssh binary
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found in PATH: %w", err)
	}

	// Replace current process with ssh
	if err := syscall.Exec(sshPath, sshArgs, os.Environ()); err != nil {
		return fmt.Errorf("failed to exec ssh: %w", err)
	}

	// This should never be reached
	return nil
}
