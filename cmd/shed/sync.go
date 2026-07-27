package main

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/sync"
)

var syncCmd = &cobra.Command{
	Use:   "sync <name>",
	Short: "Sync local files to a shed",
	Long: `Sync files from your local machine to a shed container.

Sync configuration is defined in ~/.shed/sync.yaml. Features define
collections of files to sync, and profiles enable groups of features.

By default, the 'default' profile is synced. Use --profile to sync
a different profile, or --feature to sync a single feature.`,
	Args: cobra.ExactArgs(1),
	RunE: runSync,
}

var (
	syncProfile string
	syncFeature string
	syncDryRun  bool
)

func init() {
	syncCmd.Flags().StringVarP(&syncProfile, "profile", "p", "", "Profile to sync (default: 'default')")
	syncCmd.Flags().StringVarP(&syncFeature, "feature", "f", "", "Single feature to sync")
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "Show what would be synced without syncing")

	rootCmd.AddCommand(syncCmd)
}

func runSync(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Find the server for this shed
	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}

	// Load sync config
	cfg, err := sync.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load sync config: %w", err)
	}

	if cfg.IsEmpty() {
		return fmt.Errorf("no sync configuration found; create ~/.shed/sync.yaml with features and profiles")
	}

	// Validate flags
	if syncProfile != "" && syncFeature != "" {
		return fmt.Errorf("cannot specify both --profile and --feature")
	}

	// Ensure shed is running
	client := NewAPIClientFromNamedEntry(serverName, entry, clientConfig.GetCreateTimeout())
	if _, err := ensureRunningShed(client, name); err != nil {
		return err
	}

	// Create syncer with output capture for logging
	syncer := sync.NewSyncer(cfg)

	// Capture output for log file
	var logBuffer bytes.Buffer
	var multiWriter io.Writer
	if jsonFlag {
		// In JSON mode, only capture to log buffer, suppress stdout
		multiWriter = &logBuffer
	} else {
		multiWriter = io.MultiWriter(os.Stdout, &logBuffer)
	}
	syncer.SetOutput(multiWriter)

	if verboseLevel > 0 {
		fmt.Printf("Syncing to shed %s on %s...\n", name, serverName)
	}

	ctx := cmd.Context()

	var syncErr error
	if syncFeature != "" {
		// Sync single feature
		syncErr = syncer.SyncFeature(ctx, syncFeature, name, entry, syncDryRun)
	} else {
		// Sync profile
		profile := syncProfile
		if profile == "" {
			profile = cfg.GetDefaultProfile()
			if profile == "" {
				printError("no default profile found",
					"Check ~/.shed/sync.yaml for available profiles",
					"Use --profile to specify a profile",
					"Use --feature to sync a single feature")
				return fmt.Errorf("profile %q not found", "default")
			}
		} else {
			// Check if explicitly specified profile exists
			if _, err := cfg.GetProfile(profile); err != nil {
				printError(fmt.Sprintf("profile %q not found", profile),
					"Check ~/.shed/sync.yaml for available profiles",
					"Use --feature to sync a single feature")
				return err
			}
		}

		syncErr = syncer.SyncProfile(ctx, profile, name, entry, syncDryRun)
	}

	if syncErr != nil {
		return fmt.Errorf("sync failed: %w", syncErr)
	}

	// Save sync log to container
	if !syncDryRun && logBuffer.Len() > 0 {
		if err := syncer.SaveSyncLog(ctx, logBuffer.String(), name, entry); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to save sync log: %v\n", err)
		}
	}

	if jsonFlag {
		action := "synced"
		if syncDryRun {
			action = "dry-run"
		}
		return outputJSON(ActionResult{
			Status: "ok",
			Action: action,
			Name:   name,
			Server: serverName,
		})
	}

	if syncDryRun {
		fmt.Println("\n(dry-run: no files were actually synced)")
	} else {
		printSuccess("Sync complete")
	}

	return nil
}
