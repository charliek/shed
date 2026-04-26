package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
)

var snapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Manage shed snapshots",
	Long: `Manage shed snapshots.

A snapshot captures a stopped shed's rootfs as a named, immutable artifact
that can be used to spawn new sheds with 'shed create --from-snapshot <name>'.
Snapshots survive deletion of the source shed.`,
}

var snapshotCreateCmd = &cobra.Command{
	Use:   "create <shed-name> <snapshot-name>",
	Short: "Create a snapshot from a stopped shed",
	Long: `Create a snapshot of a stopped shed's rootfs.

The shed must be stopped before snapshotting. Snapshot names follow the same
rules as shed names (lowercase alphanumeric with hyphens, max 63 chars).`,
	Args: cobra.ExactArgs(2),
	RunE: runSnapshotCreate,
}

var snapshotListCmd = &cobra.Command{
	Use:   "list",
	Short: "List snapshots",
	Args:  cobra.NoArgs,
	RunE:  runSnapshotList,
}

var snapshotInfoCmd = &cobra.Command{
	Use:   "info <snapshot-name>",
	Short: "Show details of a snapshot",
	Args:  cobra.ExactArgs(1),
	RunE:  runSnapshotInfo,
}

var snapshotDeleteCmd = &cobra.Command{
	Use:   "delete <snapshot-name>",
	Short: "Delete a snapshot",
	Args:  cobra.ExactArgs(1),
	RunE:  runSnapshotDelete,
}

var (
	snapshotCreateComment string
	snapshotDeleteForce   bool
)

func init() {
	snapshotCreateCmd.Flags().StringVar(&snapshotCreateComment, "comment", "", "Optional comment describing the snapshot")
	snapshotDeleteCmd.Flags().BoolVarP(&snapshotDeleteForce, "force", "f", false, "Delete without confirmation")

	snapshotCmd.AddCommand(snapshotCreateCmd)
	snapshotCmd.AddCommand(snapshotListCmd)
	snapshotCmd.AddCommand(snapshotInfoCmd)
	snapshotCmd.AddCommand(snapshotDeleteCmd)

	rootCmd.AddCommand(snapshotCmd)
}

func runSnapshotCreate(cmd *cobra.Command, args []string) error {
	shedName, snapName := args[0], args[1]

	if err := config.ValidateShedName(shedName); err != nil {
		return fmt.Errorf("invalid shed name: %w", err)
	}
	if err := config.ValidateSnapshotName(snapName); err != nil {
		return fmt.Errorf("invalid snapshot name: %w", err)
	}

	// Find the server hosting the source shed (snapshot lives on the same server).
	serverName, entry, err := findShedServer(shedName)
	if err != nil {
		return err
	}

	timeout := clientConfig.GetCreateTimeout()
	client := NewAPIClientFromEntry(entry, timeout)

	if verboseLevel > 0 {
		fmt.Printf("Creating snapshot %s of %s on %s...\n", snapName, shedName, serverName)
	}

	req := &config.SnapshotCreateRequest{
		Name:       snapName,
		SourceShed: shedName,
		Comment:    snapshotCreateComment,
	}
	snap, err := client.CreateSnapshot(req)
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %w", err)
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status:  "ok",
			Action:  "created",
			Name:    snap.Name,
			Server:  serverName,
			Details: snap,
		})
	}

	printSuccess("Created snapshot %s (%s) from %s on %s",
		snap.Name, formatSize(snap.SizeBytes), shedName, serverName)
	return nil
}

func runSnapshotList(cmd *cobra.Command, args []string) error {
	entry, serverName, err := getServerEntry()
	if err != nil {
		printError("no server configured",
			"shed server add <hostname>  # Add a server first")
		return err
	}

	client := NewAPIClientFromEntry(entry, DefaultTimeout)
	resp, err := client.ListSnapshots()
	if err != nil {
		return fmt.Errorf("failed to list snapshots: %w", err)
	}

	snaps := resp.Snapshots
	sort.Slice(snaps, func(i, j int) bool {
		return snaps[i].Name < snaps[j].Name
	})

	if jsonFlag {
		return outputJSON(snaps)
	}

	if len(snaps) == 0 {
		fmt.Printf("No snapshots on %s.\n", serverName)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tBACKEND\tSOURCE\tSIZE\tCREATED\tCOMMENT")
	for _, s := range snaps {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name,
			valueOrDash(s.Backend),
			valueOrDash(s.SourceShed),
			formatSize(s.SizeBytes),
			s.CreatedAt.Format("2006-01-02 15:04"),
			valueOrDash(s.Comment),
		)
	}
	w.Flush()
	return nil
}

func runSnapshotInfo(cmd *cobra.Command, args []string) error {
	name := args[0]

	entry, serverName, err := getServerEntry()
	if err != nil {
		return err
	}
	client := NewAPIClientFromEntry(entry, DefaultTimeout)

	snap, err := client.GetSnapshot(name)
	if err != nil {
		return fmt.Errorf("failed to get snapshot: %w", err)
	}

	if jsonFlag {
		return outputJSON(snap)
	}

	fmt.Printf("Name:           %s\n", snap.Name)
	fmt.Printf("Server:         %s\n", serverName)
	fmt.Printf("Backend:        %s\n", snap.Backend)
	fmt.Printf("Version:        %d\n", snap.Version)
	fmt.Printf("Source shed:    %s\n", valueOrDash(snap.SourceShed))
	if snap.SourceImage != "" {
		fmt.Printf("Source image:   %s\n", snap.SourceImage)
	}
	if snap.SourceLocalDir != "" {
		fmt.Printf("Source local:   %s (NOT included in snapshot)\n", snap.SourceLocalDir)
	}
	fmt.Printf("Size:           %s\n", formatSize(snap.SizeBytes))
	fmt.Printf("Created:        %s\n", snap.CreatedAt.Format("2006-01-02 15:04:05"))
	if snap.Comment != "" {
		fmt.Printf("Comment:        %s\n", snap.Comment)
	}
	return nil
}

func runSnapshotDelete(cmd *cobra.Command, args []string) error {
	name := args[0]

	if jsonFlag && !snapshotDeleteForce {
		return fmt.Errorf("--force is required when using --json")
	}

	entry, serverName, err := getServerEntry()
	if err != nil {
		return err
	}
	client := NewAPIClientFromEntry(entry, DefaultTimeout)

	if !snapshotDeleteForce {
		prompt := fmt.Sprintf("Delete snapshot %q on %s? [y/N] ", name, serverName)
		if !confirmAction(prompt) {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	if err := client.DeleteSnapshot(name); err != nil {
		return fmt.Errorf("failed to delete snapshot: %w", err)
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "deleted",
			Name:   name,
			Server: serverName,
		})
	}

	printSuccess("Deleted snapshot %s on %s", name, serverName)
	return nil
}
