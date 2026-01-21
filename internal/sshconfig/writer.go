package sshconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry represents an SSH config entry for a shed.
type Entry struct {
	// Name is the SSH host alias (e.g., "shed-myproject").
	Name string
	// Host is the actual hostname or IP address.
	Host string
	// Port is the SSH port.
	Port int
	// User is the SSH user.
	User string
	// KnownHostsFile is the path to the known_hosts file to use.
	KnownHostsFile string
}

// Diff represents the difference between current and desired SSH config entries.
type Diff struct {
	// Additions are entries that will be added.
	Additions []string
	// Removals are entries that will be removed.
	Removals []string
	// Unchanged are entries that remain the same.
	Unchanged []string
}

// GenerateEntry generates an SSH config block for a single entry.
func GenerateEntry(entry Entry) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Host %s\n", entry.Name))
	sb.WriteString(fmt.Sprintf("    HostName %s\n", entry.Host))
	sb.WriteString(fmt.Sprintf("    Port %d\n", entry.Port))
	sb.WriteString(fmt.Sprintf("    User %s\n", entry.User))
	if entry.KnownHostsFile != "" {
		sb.WriteString(fmt.Sprintf("    UserKnownHostsFile %s\n", entry.KnownHostsFile))
	}

	return sb.String()
}

// GenerateManagedBlock generates the complete managed block content.
func GenerateManagedBlock(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}

	var sb strings.Builder

	sb.WriteString(BeginMarker + "\n")
	sb.WriteString("# Do not edit manually - managed by shed CLI\n")
	sb.WriteString(fmt.Sprintf("# Last updated: %s\n", time.Now().UTC().Format(time.RFC3339)))
	sb.WriteString("\n")

	// Sort entries by name for consistent output
	sortedEntries := make([]Entry, len(entries))
	copy(sortedEntries, entries)
	sort.Slice(sortedEntries, func(i, j int) bool {
		return sortedEntries[i].Name < sortedEntries[j].Name
	})

	for i, entry := range sortedEntries {
		sb.WriteString(GenerateEntry(entry))
		if i < len(sortedEntries)-1 {
			sb.WriteString("\n")
		}
	}

	sb.WriteString(EndMarker + "\n")

	return sb.String()
}

// ComputeDiff computes the difference between current and desired entries.
func ComputeDiff(current []ManagedEntry, desired []Entry) Diff {
	diff := Diff{
		Additions: []string{},
		Removals:  []string{},
		Unchanged: []string{},
	}

	// Create maps for easier lookup
	currentNames := make(map[string]bool)
	for _, entry := range current {
		currentNames[entry.Name] = true
	}

	desiredNames := make(map[string]bool)
	for _, entry := range desired {
		desiredNames[entry.Name] = true
	}

	// Find additions (in desired but not in current)
	for _, entry := range desired {
		if !currentNames[entry.Name] {
			diff.Additions = append(diff.Additions, entry.Name)
		} else {
			diff.Unchanged = append(diff.Unchanged, entry.Name)
		}
	}

	// Find removals (in current but not in desired)
	for _, entry := range current {
		if !desiredNames[entry.Name] {
			diff.Removals = append(diff.Removals, entry.Name)
		}
	}

	// Sort all slices for consistent output
	sort.Strings(diff.Additions)
	sort.Strings(diff.Removals)
	sort.Strings(diff.Unchanged)

	return diff
}

// HasChanges returns true if the diff contains any additions or removals.
func (d Diff) HasChanges() bool {
	return len(d.Additions) > 0 || len(d.Removals) > 0
}

// Write writes the SSH config file with the managed block.
// It performs an atomic write by writing to a temp file first.
func Write(path string, before string, entries []Entry, after string) error {
	// Ensure the .ssh directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create .ssh directory: %w", err)
	}

	// Build the complete content
	var sb strings.Builder

	// Add content before managed block
	if before != "" {
		sb.WriteString(before)
		if !strings.HasSuffix(before, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// Add the managed block (only if there are entries)
	if len(entries) > 0 {
		sb.WriteString(GenerateManagedBlock(entries))
	}

	// Add content after managed block
	if after != "" {
		if len(entries) > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(after)
	}

	content := sb.String()

	// Atomic write via temp file
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) // Clean up on failure
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

// Remove removes the managed block from the SSH config file.
func Remove(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Nothing to remove
		}
		return fmt.Errorf("failed to read SSH config: %w", err)
	}

	parsed := Parse(string(content))
	if !parsed.HasManagedBlock {
		return nil // No managed block to remove
	}

	// Write back without the managed block
	return Write(path, parsed.BeforeBlock, nil, parsed.AfterBlock)
}

// GetSSHConfigPath returns the default SSH config path.
func GetSSHConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/.ssh/config"
	}
	return filepath.Join(home, ".ssh", "config")
}
