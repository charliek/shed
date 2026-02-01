package sync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/provision"
)

// SyncErrors collects multiple sync errors from a profile sync.
type SyncErrors struct {
	Errors []error
}

func (e *SyncErrors) Error() string {
	if len(e.Errors) == 1 {
		return e.Errors[0].Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d features failed to sync:", len(e.Errors))
	for _, err := range e.Errors {
		fmt.Fprintf(&b, "\n  - %s", err.Error())
	}
	return b.String()
}

// Unwrap returns the underlying errors for errors.Is/As compatibility.
func (e *SyncErrors) Unwrap() []error {
	return e.Errors
}

// Syncer handles file synchronization to shed containers.
type Syncer struct {
	cfg    *Config
	output io.Writer
}

// NewSyncer creates a new file syncer.
func NewSyncer(cfg *Config) *Syncer {
	return &Syncer{
		cfg:    cfg,
		output: os.Stdout,
	}
}

// SetOutput sets the output writer for sync messages.
func (s *Syncer) SetOutput(w io.Writer) {
	s.output = w
}

// SyncProfile syncs all features in a profile in the order they are listed.
// It continues syncing remaining features even if some fail, collecting all errors.
func (s *Syncer) SyncProfile(ctx context.Context, profileName string, shedName string, serverEntry *config.ServerEntry, dryRun bool) error {
	profile, err := s.cfg.GetProfile(profileName)
	if err != nil {
		return err
	}

	var syncErrors []error

	// Iterate in profile order (not map order) for deterministic behavior
	for _, featureName := range profile.Features {
		// Check for context cancellation between features
		if err := ctx.Err(); err != nil {
			syncErrors = append(syncErrors, fmt.Errorf("sync cancelled: %w", err))
			break // Stop on cancellation
		}

		feature, err := s.cfg.GetFeature(featureName)
		if err != nil {
			syncErrors = append(syncErrors, fmt.Errorf("profile %q references unknown feature %q", profileName, featureName))
			continue // Continue with remaining features
		}
		if err := s.syncFeature(ctx, featureName, feature, shedName, serverEntry, dryRun); err != nil {
			fmt.Fprintf(s.output, "  ✗ Feature %s failed: %v\n", featureName, err)
			syncErrors = append(syncErrors, fmt.Errorf("failed to sync feature %s: %w", featureName, err))
			continue // Continue with remaining features
		}
	}

	if len(syncErrors) > 0 {
		return &SyncErrors{Errors: syncErrors}
	}
	return nil
}

// SyncFeature syncs a single feature.
func (s *Syncer) SyncFeature(ctx context.Context, featureName string, shedName string, serverEntry *config.ServerEntry, dryRun bool) error {
	feature, err := s.cfg.GetFeature(featureName)
	if err != nil {
		return err
	}

	return s.syncFeature(ctx, featureName, feature, shedName, serverEntry, dryRun)
}

// syncFeature syncs a single feature to the container.
func (s *Syncer) syncFeature(ctx context.Context, featureName string, feature *Feature, shedName string, serverEntry *config.ServerEntry, dryRun bool) error {
	fmt.Fprintf(s.output, "Syncing feature: %s\n", featureName)
	if feature.Description != "" {
		fmt.Fprintf(s.output, "  %s\n", feature.Description)
	}

	// Sync each path mapping
	for _, pm := range feature.Paths {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("sync cancelled: %w", err)
		}
		if err := s.syncPath(ctx, pm, shedName, serverEntry, dryRun); err != nil {
			return fmt.Errorf("failed to sync %s -> %s: %w", pm.Source, pm.Target, err)
		}
	}

	// Run post-sync hooks
	if len(feature.PostSync) > 0 && !dryRun {
		fmt.Fprintf(s.output, "  Running post-sync hooks...\n")
		for _, hook := range feature.PostSync {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("sync cancelled: %w", err)
			}
			if err := s.runPostSyncHook(ctx, hook, shedName, serverEntry); err != nil {
				return fmt.Errorf("post-sync hook failed: %w", err)
			}
		}
	}

	return nil
}

// syncPath syncs a single path mapping to the container.
func (s *Syncer) syncPath(ctx context.Context, pm PathMapping, shedName string, serverEntry *config.ServerEntry, dryRun bool) error {
	source := pm.Source
	target := pm.Target

	// Check if source exists
	info, err := os.Stat(source)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(s.output, "  Skipping %s (does not exist)\n", source)
			return nil
		}
		return fmt.Errorf("failed to stat %s: %w", source, err)
	}

	if dryRun {
		if info.IsDir() {
			fmt.Fprintf(s.output, "  Would sync directory: %s -> %s", source, target)
		} else {
			fmt.Fprintf(s.output, "  Would sync file: %s -> %s", source, target)
		}
		if pm.Include != "" {
			fmt.Fprintf(s.output, " (include: %s)", pm.Include)
		}
		fmt.Fprintln(s.output)
		return nil
	}

	fmt.Fprintf(s.output, "  Syncing: %s -> %s\n", source, target)

	// Create tar archive of source
	tarData, err := s.createTar(ctx, source, pm.Include)
	if err != nil {
		return fmt.Errorf("failed to create tar: %w", err)
	}

	// Both files and directories: extract to parent, use basename
	extractDir := filepath.Dir(target)
	targetName := filepath.Base(target)

	// Transfer and extract via SSH
	return s.transferViaSsh(ctx, tarData, extractDir, source, targetName, info.IsDir(), shedName, serverEntry)
}

// createTar creates a tar archive of the source path.
func (s *Syncer) createTar(ctx context.Context, source string, include string) ([]byte, error) {
	info, err := os.Stat(source)
	if err != nil {
		return nil, err
	}

	var args []string

	// Use gzip compression and preserve permissions
	args = append(args, "-cpzf", "-")

	if info.IsDir() {
		// Change to parent directory
		args = append(args, "-C", filepath.Dir(source))

		if include != "" {
			// For include patterns, we use find and pipe to tar
			// This is more portable than --include
			return s.createTarWithPattern(ctx, source, include)
		}

		// Add the directory name
		args = append(args, filepath.Base(source))
	} else {
		// For files, change to parent directory and tar the filename
		args = append(args, "-C", filepath.Dir(source), filepath.Base(source))
	}

	cmd := exec.CommandContext(ctx, "tar", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("tar cancelled: %w", ctx.Err())
		}
		return nil, fmt.Errorf("tar failed: %s", stderr.String())
	}

	return stdout.Bytes(), nil
}

// createTarWithPattern creates a tar archive using a glob pattern to filter files.
func (s *Syncer) createTarWithPattern(ctx context.Context, source string, pattern string) ([]byte, error) {
	// Get list of matching files
	matches, err := filepath.Glob(filepath.Join(source, pattern))
	if err != nil {
		return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}

	if len(matches) == 0 {
		fmt.Fprintf(s.output, "    ⚠ Warning: no files matched pattern %q in %s\n", pattern, source)
		// Return empty gzipped tar for no matches
		var args = []string{"-czf", "-", "-T", "/dev/null"}
		cmd := exec.CommandContext(ctx, "tar", args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		// Tar with empty file list may fail on some systems; this is expected
		// and not actionable since we're intentionally creating an empty archive.
		if err := cmd.Run(); err != nil {
			// Log the error for debugging, but continue with empty output
			// since this is expected behavior when no files match.
			if stderr.Len() > 0 {
				fmt.Fprintf(s.output, "    Note: tar returned: %s\n", strings.TrimSpace(stderr.String()))
			}
		}
		return stdout.Bytes(), nil
	}

	// Convert to relative paths
	relPaths := make([]string, 0, len(matches))
	for _, match := range matches {
		rel, err := filepath.Rel(filepath.Dir(source), match)
		if err != nil {
			fmt.Fprintf(s.output, "    Warning: skipping %s: %v\n", match, err)
			continue
		}
		relPaths = append(relPaths, rel)
	}

	// Build tar command with file list
	args := []string{"-cpzf", "-", "-C", filepath.Dir(source)}
	args = append(args, relPaths...)

	cmd := exec.CommandContext(ctx, "tar", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("tar cancelled: %w", ctx.Err())
		}
		return nil, fmt.Errorf("tar failed: %s", stderr.String())
	}

	return stdout.Bytes(), nil
}

// transferViaSsh transfers tar data to the container and extracts it.
func (s *Syncer) transferViaSsh(ctx context.Context, tarData []byte, extractDir, sourcePath, targetName string, isDir bool, shedName string, serverEntry *config.ServerEntry) error {
	if serverEntry == nil {
		return fmt.Errorf("server entry is required for SSH transfer")
	}

	knownHostsPath := config.GetKnownHostsPath()

	// Build SSH command to extract tar
	// We need to handle the case where source and target names might differ
	sourceBaseName := filepath.Base(sourcePath)

	var extractCmd string
	if sourceBaseName == targetName || isDir {
		// Names match or it's a directory - simple extraction
		// Use %q to properly quote paths with special characters
		extractCmd = fmt.Sprintf("mkdir -p %q && tar xzpf - -C %q", extractDir, extractDir)
	} else {
		// File with different name - extract and rename
		// Use %q to properly quote paths with special characters
		extractCmd = fmt.Sprintf("mkdir -p %q && tar xzpf - -C %q && mv %q/%q %q/%q",
			extractDir, extractDir, extractDir, sourceBaseName, extractDir, targetName)
	}

	args := []string{
		"-p", strconv.Itoa(serverEntry.SSHPort),
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		shedName + "@" + serverEntry.Host,
		extractCmd,
	}

	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = bytes.NewReader(tarData)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ssh cancelled: %w", ctx.Err())
		}
		return fmt.Errorf("ssh transfer to %s failed: %s", extractDir, strings.TrimSpace(stderr.String()))
	}

	return nil
}

// runPostSyncHook runs a post-sync hook command in the container.
func (s *Syncer) runPostSyncHook(ctx context.Context, hook PostSyncHook, shedName string, serverEntry *config.ServerEntry) error {
	if serverEntry == nil {
		return fmt.Errorf("server entry is required for post-sync hooks")
	}

	knownHostsPath := config.GetKnownHostsPath()

	// Note: hook.Run is user-defined in sync.yaml, so we pass it as-is without escaping.
	// Users expect their shell command to run exactly as written.
	args := []string{
		"-p", strconv.Itoa(serverEntry.SSHPort),
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		shedName + "@" + serverEntry.Host,
		hook.Run,
	}

	cmd := exec.CommandContext(ctx, "ssh", args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("hook cancelled: %w", ctx.Err())
		}
		// Combine non-empty outputs with newlines
		var parts []string
		if out := strings.TrimSpace(stdout.String()); out != "" {
			parts = append(parts, out)
		}
		if out := strings.TrimSpace(stderr.String()); out != "" {
			parts = append(parts, out)
		}
		output := strings.Join(parts, "\n")
		return fmt.Errorf("hook %q failed: %s", hook.Run, output)
	}

	return nil
}

// SaveSyncLog saves sync output to the container's log file.
func (s *Syncer) SaveSyncLog(ctx context.Context, logContent string, shedName string, serverEntry *config.ServerEntry) error {
	if serverEntry == nil {
		return fmt.Errorf("server entry is required to save sync log")
	}

	knownHostsPath := config.GetKnownHostsPath()

	// Create log directory and write log
	cmdStr := fmt.Sprintf("mkdir -p %s && cat > %s", filepath.Dir(provision.SyncLog), provision.SyncLog)

	args := []string{
		"-p", strconv.Itoa(serverEntry.SSHPort),
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		shedName + "@" + serverEntry.Host,
		cmdStr,
	}

	sshCmd := exec.CommandContext(ctx, "ssh", args...)
	sshCmd.Stdin = strings.NewReader(logContent)

	return sshCmd.Run()
}
