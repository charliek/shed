package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var (
	attachSessionFlag string
	attachNewFlag     bool
)

var attachCmd = &cobra.Command{
	Use:   "attach <name>",
	Short: "Attach to a tmux session in a shed",
	Long: `Attach to a tmux session in a shed container.

By default, attaches to or creates a session named "default".
Use --session to specify a different session name.

This command uses tmux for session persistence, allowing you to:
- Detach from a running session (Ctrl-B D)
- Reconnect later to continue where you left off
- Run multiple named sessions in the same shed

Examples:
  shed attach myproj                    # Attach to default session
  shed attach myproj --session debug    # Attach to "debug" session
  shed attach myproj --new --session exp # Create new "exp" session`,
	Args: cobra.ExactArgs(1),
	RunE: runAttach,
}

func init() {
	attachCmd.Flags().StringVarP(&attachSessionFlag, "session", "S", config.DefaultSessionName, "Session name to attach to")
	attachCmd.Flags().BoolVar(&attachNewFlag, "new", false, "Force create a new session (error if exists)")

	rootCmd.AddCommand(attachCmd)
}

func runAttach(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Validate session name
	if err := config.ValidateSessionName(attachSessionFlag); err != nil {
		return fmt.Errorf("invalid session name: %w", err)
	}

	// Find the server hosting this shed
	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}

	// Verify the shed is running
	client := NewAPIClientFromEntry(entry)
	if _, err := requireRunningShed(client, name); err != nil {
		return err
	}

	// If --new flag is set, check that session doesn't exist
	if attachNewFlag {
		sessions, err := client.ListSessions(name)
		if err == nil {
			for _, s := range sessions {
				if s.Name == attachSessionFlag {
					return fmt.Errorf("session %q already exists (use without --new to attach)", attachSessionFlag)
				}
			}
		}
	}

	if verboseFlag {
		fmt.Printf("Attaching to session %q in %s on %s...\n", attachSessionFlag, name, serverName)
	}

	// Build SSH command with tmux
	knownHostsPath := config.GetKnownHostsPath()

	// Build the tmux command to run on the remote
	// tmux new-session -A -s <session> -c /workspace
	// -A: attach if exists, create if not
	// -s: session name
	// -c: start directory
	tmuxCmd := fmt.Sprintf("tmux new-session -A -s %s -c /workspace", attachSessionFlag)

	sshArgs := []string{
		"ssh",
		"-t", // Force pseudo-terminal allocation
		"-p", strconv.Itoa(entry.SSHPort),
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"-o", "StrictHostKeyChecking=yes",
		name + "@" + entry.Host,
		"--", // Separator for remote command
		tmuxCmd,
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
