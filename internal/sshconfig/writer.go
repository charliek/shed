package sshconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// IdentityFile, when set, is the key ssh offers first for this host, and
	// no agent-only key; the stanza gains IdentityLines: `IdentityFile
	// "<path>"`, `IdentitiesOnly yes`, `ControlMaster no` and `ControlPath
	// none`. The value is checked first (the caller's
	// config.ServerEntry.SSHIdentityArgs, and every writer's Entry.Validate): it
	// is written inside double quotes as it is, which is safe only because that
	// rule refuses `"`, `\`, `%`, `$`, `#`, CR, LF and NUL. Empty writes the
	// stanza exactly as it was without the field.
	IdentityFile string
}

// Diff represents the difference between current and desired SSH config entries.
type Diff struct {
	// Additions are entries that will be added.
	Additions []string
	// Removals are entries that will be removed.
	Removals []string
	// Changed are entries present on both sides whose stanza must be written
	// again: the desired entry has an identity file, and the block holds its
	// alias more than once, or a stanza that does not carry exactly its
	// identity lines (IdentityLinesDiffer). Without an identity file an entry
	// is never Changed: it is compared by name only, as before the field
	// existed. A change of any kind regenerates the whole block.
	Changed []string
	// Unchanged are entries that remain the same.
	Unchanged []string
}

// GenerateEntry generates an SSH config block for a single entry.
func GenerateEntry(entry Entry) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Host %s\n", entry.Name)
	fmt.Fprintf(&sb, "    HostName %s\n", entry.Host)
	fmt.Fprintf(&sb, "    Port %d\n", entry.Port)
	fmt.Fprintf(&sb, "    User %s\n", entry.User)
	if entry.KnownHostsFile != "" {
		// Quoted, so a home directory with a space is one file name to ssh
		// (the keyword takes a list); Entry.Validate keeps the path free of
		// anything a quoted value would still expand or end on.
		fmt.Fprintf(&sb, "    UserKnownHostsFile \"%s\"\n", entry.KnownHostsFile)
	}
	sb.WriteString(IdentityLines(entry.IdentityFile, "    "))

	return sb.String()
}

// IdentityLines is the ssh config lines an identity file adds to a Host
// stanza, each prefixed by indent and ending in a newline: `IdentityFile
// "<path>"` and `IdentitiesOnly yes` (that key first, no agent-only key), then
// `ControlMaster no` and `ControlPath none` (no shared connection, so an ssh
// through this stanza never rides a master another key opened); "" for no
// identity file. The path is written as it is (see Entry.IdentityFile for why
// that is safe). In an ssh config the first value found for an option wins,
// so a stanza earlier in the file that sets one of these for the same host
// prevails; the docs say so.
func IdentityLines(identityFile, indent string) string {
	if identityFile == "" {
		return ""
	}
	return fmt.Sprintf("%sIdentityFile \"%s\"\n%sIdentitiesOnly yes\n%sControlMaster no\n%sControlPath none\n",
		indent, identityFile, indent, indent, indent)
}

// keywordOf is the option keyword a stanza line starts with, after its
// indentation: the text up to the first space, tab or `=`.
func keywordOf(line string) string {
	line = strings.TrimSpace(line)
	if end := strings.IndexAny(line, " \t="); end >= 0 {
		return line[:end]
	}
	return line
}

// keywordsOf is the set of option keywords the lines of text start with.
func keywordsOf(text string) map[string]bool {
	set := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		set[keywordOf(line)] = true
	}
	return set
}

// identityKeywords are the options IdentityLines writes, lower-cased, read
// from IdentityLines itself.
var identityKeywords = func() map[string]bool {
	set := map[string]bool{}
	for keyword := range keywordsOf(IdentityLines("x", "")) {
		set[strings.ToLower(keyword)] = true
	}
	return set
}()

// identityLinesOf returns the lines of a stanza that set one of the options
// IdentityLines writes, each trimmed, with CRLF read as LF.
func identityLinesOf(stanza string) []string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(stanza, "\r\n", "\n"), "\n") {
		if identityKeywords[strings.ToLower(keywordOf(line))] {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

// IdentityLinesDiffer reports whether a stanza on disk (raw) does not carry
// exactly the identity lines entry calls for: the same lines, in the same
// order, and no other line that sets one of those options. It is false for an
// entry with no identity file, whatever raw holds (the name-only comparison
// that predates the field).
func IdentityLinesDiffer(raw string, entry Entry) bool {
	if entry.IdentityFile == "" {
		return false
	}
	return !slices.Equal(identityLinesOf(raw), identityLinesOf(IdentityLines(entry.IdentityFile, "")))
}

// StanzaDiffers reports whether a stanza on disk (raw) differs from the one
// entry generates, after trimming and with CRLF read as LF.
func StanzaDiffers(raw string, entry Entry) bool {
	norm := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	return norm(raw) != norm(GenerateEntry(entry))
}

// GenerateManagedBlock generates the complete managed block content.
func GenerateManagedBlock(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}

	var sb strings.Builder

	sb.WriteString(BeginMarker + "\n")
	sb.WriteString("# Do not edit manually - managed by shed CLI\n")
	fmt.Fprintf(&sb, "# Last updated: %s\n", time.Now().UTC().Format(time.RFC3339))
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
		Changed:   []string{},
		Unchanged: []string{},
	}

	// Create maps for easier lookup: each current entry's stanza by name, and
	// how many stanzas the block holds for it.
	currentNames := make(map[string]string)
	stanzas := make(map[string]int)
	for _, entry := range current {
		currentNames[entry.Name] = entry.RawContent
		stanzas[entry.Name]++
	}

	desiredNames := make(map[string]bool)
	for _, entry := range desired {
		desiredNames[entry.Name] = true
	}

	// Find additions (in desired but not in current), and entries with an
	// identity file held twice or whose stanza does not carry its identity
	// lines.
	for _, entry := range desired {
		raw, ok := currentNames[entry.Name]
		switch {
		case !ok:
			diff.Additions = append(diff.Additions, entry.Name)
		case entry.IdentityFile != "" && stanzas[entry.Name] > 1, IdentityLinesDiffer(raw, entry):
			diff.Changed = append(diff.Changed, entry.Name)
		default:
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
	sort.Strings(diff.Changed)
	sort.Strings(diff.Unchanged)

	return diff
}

// HasChanges returns true if the diff contains any additions, removals or
// changed entries.
func (d Diff) HasChanges() bool {
	return len(d.Additions) > 0 || len(d.Removals) > 0 || len(d.Changed) > 0
}

// Write writes the SSH config file with the managed block.
// It performs an atomic write by writing to a temp file first. Every entry is
// checked first (Entry.Validate): one that fails it is refused and nothing is
// written.
func Write(path string, before string, entries []Entry, after string) error {
	for _, entry := range entries {
		if err := entry.Validate(); err != nil {
			return err
		}
	}

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
