// Package sshconfig provides parsing and writing of SSH config files.
package sshconfig

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
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

// MarkerError refuses a file whose managed-block marker lines do not frame
// exactly one block: more than one BEGIN or END line, one without the other,
// or the END line before the BEGIN line. shed cannot tell which lines are its
// own, so nothing is written. Path is shown escaped (sdkbootstrap.SafeText).
type MarkerError struct {
	Path   string
	Reason string
}

func (e *MarkerError) Error() string {
	return fmt.Sprintf("%s does not hold exactly one shed managed block (%s), so shed cannot tell which lines are its own; "+
		"nothing was written. Leave exactly one line %q followed by one line %q (or remove both and every line between them), "+
		"then retry", sdkbootstrap.SafeText(e.Path), e.Reason, BeginMarker, EndMarker)
}

// blockBounds locates the managed block by its marker LINES, never by a loose
// substring: a marker is a line that is exactly the marker, a trailing CR
// allowed, so marker text in a comment or inside a quoted value is never a
// boundary, and a marker with spaces or tabs around it is refused rather
// than read as either. begin and end are the offsets of the BEGIN and END lines, both -1
// when the file has neither. A file whose markers frame no single block is a
// *MarkerError (Path unset). Every reader and writer of the block uses this.
func blockBounds(content string) (begin, end int, err error) {
	var begins, ends []int
	offset := 0
	for _, line := range strings.SplitAfter(content, "\n") {
		switch strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r") {
		case BeginMarker:
			begins = append(begins, offset)
		case EndMarker:
			ends = append(ends, offset)
		default:
			// A marker with spaces or tabs around it is neither a
			// boundary nor ordinary text: reading it as text would add
			// a second block beside the one it frames.
			if t := strings.TrimSpace(line); t == BeginMarker || t == EndMarker {
				return -1, -1, &MarkerError{Reason: "a marker line has spaces or tabs around it"}
			}
		}
		offset += len(line)
	}
	switch {
	case len(begins) == 0 && len(ends) == 0:
		return -1, -1, nil
	case len(begins) != 1 || len(ends) != 1:
		return -1, -1, &MarkerError{Reason: fmt.Sprintf("%d BEGIN and %d END marker lines", len(begins), len(ends))}
	case ends[0] < begins[0]:
		return -1, -1, &MarkerError{Reason: "the END marker line comes before the BEGIN marker line"}
	}
	return begins[0], ends[0], nil
}

// CheckMarkers is blockBounds' refusal for the file at path: nil when content
// has exactly one managed block or none, else the *MarkerError naming path.
func CheckMarkers(path, content string) error {
	if _, _, err := blockBounds(content); err != nil {
		var me *MarkerError
		if errors.As(err, &me) {
			me.Path = path
		}
		return err
	}
	return nil
}

// Parse parses an SSH config file content and extracts the managed block,
// located by its marker lines (blockBounds). A file whose markers frame no
// single block is read as having none; a writer checks CheckMarkers first.
func Parse(content string) *ParsedConfig {
	result := &ParsedConfig{
		ManagedEntries: []ManagedEntry{},
	}

	beginIdx, endIdx, err := blockBounds(content)
	if err != nil || beginIdx == -1 {
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
