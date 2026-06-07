package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	resetForce bool
)

var resetCmd = &cobra.Command{
	Use:   "reset <name>",
	Short: "Reset a shed's writable upper layer",
	Long: `Delete and recreate the shed's per-shed writable upper layer.

The shed must be stopped. The lower image (shared, read-only) is
untouched, so the next boot starts from the same image content but
without any modifications the previous boot wrote.

The workspace (mounted post-boot from outside the overlay) is also
unaffected — only the upper is reset.

This is the equivalent of "throw away anything I wrote inside the VM
and start fresh from the same image".`,
	Args: cobra.ExactArgs(1),
	RunE: runReset,
}

func init() {
	resetCmd.Flags().BoolVarP(&resetForce, "force", "f", false, "Reset without confirmation")
	rootCmd.AddCommand(resetCmd)
}

func runReset(cmd *cobra.Command, args []string) error {
	name := args[0]

	if jsonFlag && !resetForce {
		return fmt.Errorf("--force is required when using --json")
	}

	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}

	if !resetForce {
		prompt := fmt.Sprintf(
			"Reset shed %q on %s? This wipes the writable upper (the home directory, "+
				"including any cloned repo and uncommitted changes) but keeps "+
				"--local-dir/--add-dir mounts and the lower image. [y/N] ", name, serverName)
		if !confirmAction(prompt) {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	shed, err := client.ResetShed(name)
	if err != nil {
		return fmt.Errorf("failed to reset shed: %w", err)
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status:  "ok",
			Action:  "reset",
			Name:    name,
			Server:  serverName,
			Details: shed,
		})
	}

	printSuccess("Reset shed %s on %s (upper layer recreated)", name, serverName)
	fmt.Fprintln(os.Stderr, "Note: workspace contents and the lower image are unchanged.")
	return nil
}
