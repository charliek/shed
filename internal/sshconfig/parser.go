// Package sshconfig provides parsing and writing of SSH config files.
package sshconfig

import (
	"regexp"
	"strings"
)

const (
	// BeginMarker marks the start of the managed block.
	BeginMarker = "# --- BEGIN SHED MANAGED BLOCK ---"
	// EndMarker marks the end of the managed block.
	EndMarker = "# --- END SHED MANAGED BLOCK ---"
)

// ParsedConfig represents a parsed SSH config file with the managed block separated.
type ParsedConfig struct {
	// BeforeBlock contains all content before the managed block.
	BeforeBlock string
	// ManagedEntries contains the individual Host entries from the managed block.
	ManagedEntries []ManagedEntry
	// AfterBlock contains all content after the managed block.
	AfterBlock string
	// HasManagedBlock indicates whether a managed block was found.
	HasManagedBlock bool
}

// ManagedEntry represents a single Host entry from the managed block.
type ManagedEntry struct {
	// Name is the SSH host alias (e.g., "shed-myproject").
	Name string
	// RawContent is the full text of the Host entry.
	RawContent string
}

// Parse parses an SSH config file content and extracts the managed block.
func Parse(content string) *ParsedConfig {
	result := &ParsedConfig{
		ManagedEntries: []ManagedEntry{},
	}

	// Find the managed block markers
	beginIdx := strings.Index(content, BeginMarker)
	endIdx := strings.Index(content, EndMarker)

	if beginIdx == -1 || endIdx == -1 || endIdx <= beginIdx {
		// No valid managed block found
		result.BeforeBlock = content
		result.HasManagedBlock = false
		return result
	}

	result.HasManagedBlock = true

	// Extract the three sections
	result.BeforeBlock = content[:beginIdx]
	managedContent := content[beginIdx+len(BeginMarker) : endIdx]
	result.AfterBlock = content[endIdx+len(EndMarker):]

	// Trim trailing newline from BeforeBlock if present (we'll add it back when writing)
	result.BeforeBlock = strings.TrimSuffix(result.BeforeBlock, "\n")

	// Trim leading newline from AfterBlock if present
	result.AfterBlock = strings.TrimPrefix(result.AfterBlock, "\n")

	// Parse individual Host entries from the managed content
	result.ManagedEntries = parseHostEntries(managedContent)

	return result
}

// parseHostEntries extracts individual Host entries from the managed block content.
func parseHostEntries(content string) []ManagedEntry {
	var entries []ManagedEntry

	// Split on "Host " to find individual entries
	// The regex matches "Host " at the start of a line
	hostPattern := regexp.MustCompile(`(?m)^Host\s+`)
	indices := hostPattern.FindAllStringIndex(content, -1)

	if len(indices) == 0 {
		return entries
	}

	for i, idx := range indices {
		var entryContent string
		if i == len(indices)-1 {
			// Last entry - goes to end of content
			entryContent = content[idx[0]:]
		} else {
			// Entry ends where next one begins
			entryContent = content[idx[0]:indices[i+1][0]]
		}

		// Clean up the entry
		entryContent = strings.TrimSpace(entryContent)
		if entryContent == "" {
			continue
		}

		// Extract the host name from "Host <name>"
		lines := strings.SplitN(entryContent, "\n", 2)
		if len(lines) == 0 {
			continue
		}

		hostLine := strings.TrimSpace(lines[0])
		parts := strings.Fields(hostLine)
		if len(parts) < 2 {
			continue
		}

		name := parts[1] // The host alias

		entries = append(entries, ManagedEntry{
			Name:       name,
			RawContent: entryContent,
		})
	}

	return entries
}

// GetEntryNames returns the names of all managed entries.
func (p *ParsedConfig) GetEntryNames() []string {
	names := make([]string, len(p.ManagedEntries))
	for i, entry := range p.ManagedEntries {
		names[i] = entry.Name
	}
	return names
}

// FindEntry finds a managed entry by name.
func (p *ParsedConfig) FindEntry(name string) *ManagedEntry {
	for i := range p.ManagedEntries {
		if p.ManagedEntries[i].Name == name {
			return &p.ManagedEntries[i]
		}
	}
	return nil
}
