# Phase 8: SSH Config Management

## Overview
- **Phase**: 8 of 9
- **Epic**: [setup-epic.md](./setup-epic.md)
- **Status**: NOT STARTED
- **Estimated Effort**: Medium
- **Dependencies**: Phase 7 complete (CLI commands exist)

## Objective
Implement SSH config parsing and generation for IDE integration. This enables developers to use Cursor, VS Code Remote-SSH, and other SSH-based tools to connect to sheds by simply selecting `shed-{name}` from their SSH hosts.

## Prerequisites
- Phase 7 complete (CLI framework and basic commands exist)
- Client configuration system working (`~/.shed/config.yaml`)
- Shed cache populated from server queries

## Context for New Engineers

### Why SSH Config Management?

IDEs like Cursor and VS Code use `~/.ssh/config` to discover available SSH hosts. By generating SSH config entries for each shed, developers can:

1. Open Cursor/VS Code
2. Run "Remote-SSH: Connect to Host"
3. Select `shed-codelens` from the dropdown
4. Immediately start coding in the remote container

Without this feature, developers would need to manually maintain their SSH config, which is error-prone and tedious.

### The Managed Block Approach

Rather than replacing the entire `~/.ssh/config`, we manage a clearly delimited block:

```
# User's existing config (preserved)
Host github.com
    IdentityFile ~/.ssh/github_key

# --- BEGIN SHED MANAGED BLOCK ---
# Do not edit manually - managed by shed CLI
# Last updated: 2026-01-20T10:30:00Z

Host shed-codelens
    HostName mini-desktop.tailnet.ts.net
    Port 2222
    User codelens
    UserKnownHostsFile ~/.shed/known_hosts

Host shed-stbot
    HostName cloud-vps.tailnet.ts.net
    Port 2222
    User stbot
    UserKnownHostsFile ~/.shed/known_hosts

# --- END SHED MANAGED BLOCK ---

# More user config (also preserved)
Host work-server
    HostName work.example.com
```

This approach:
- Never touches user-managed content outside the block
- Makes it clear which entries are auto-generated
- Allows safe updates without manual editing

### SSH Config Entry Format

Each shed gets an entry with this structure:

```
Host shed-{name}
    HostName {server_host}
    Port {ssh_port}
    User {shed_name}
    UserKnownHostsFile ~/.shed/known_hosts
```

Example for shed "codelens" on server "mini-desktop":
```
Host shed-codelens
    HostName mini-desktop.tailnet.ts.net
    Port 2222
    User codelens
    UserKnownHostsFile ~/.shed/known_hosts
```

### Command Behavior

The `shed ssh-config` command operates in several modes:

| Command | Behavior |
|---------|----------|
| `shed ssh-config codelens` | Print config for one shed |
| `shed ssh-config --all` | Print config for all known sheds |
| `shed ssh-config --all --install` | Write to ~/.ssh/config |
| `shed ssh-config --all --install --dry-run` | Show what would change |
| `shed ssh-config --uninstall` | Remove managed block |

---

## Progress Tracker

| Task | Status | Notes |
|------|--------|-------|
| 8.1 Create parser.go | NOT STARTED | |
| 8.2 Create writer.go | NOT STARTED | |
| 8.3 Implement ssh-config command | NOT STARTED | |
| 8.4 Write unit tests | NOT STARTED | |
| 8.5 Manual testing | NOT STARTED | |

---

## Detailed Tasks

### 8.1 Create SSH Config Parser

**File**: `internal/sshconfig/parser.go`

The parser reads an existing `~/.ssh/config` file and extracts:
1. Content before the managed block
2. The managed block itself (if present)
3. Content after the managed block

```go
package sshconfig

import (
    "bufio"
    "os"
    "strings"
    "time"
)

const (
    // Block markers
    BeginMarker = "# --- BEGIN SHED MANAGED BLOCK ---"
    EndMarker   = "# --- END SHED MANAGED BLOCK ---"
)

// ParsedConfig represents a parsed ~/.ssh/config file
type ParsedConfig struct {
    // Content before the managed block (preserved exactly)
    BeforeBlock string

    // Content after the managed block (preserved exactly)
    AfterBlock string

    // Existing entries in the managed block (for diff calculation)
    ManagedEntries []HostEntry

    // Whether a managed block was found
    HasManagedBlock bool
}

// HostEntry represents a single Host entry in SSH config
type HostEntry struct {
    Name     string // The host alias (e.g., "shed-codelens")
    HostName string // The actual hostname
    Port     int    // SSH port
    User     string // SSH username
    // Other fields as needed
}

// ParseFile reads and parses an SSH config file
// Returns empty ParsedConfig if file doesn't exist (not an error)
func ParseFile(path string) (*ParsedConfig, error) {
    // 1. Check if file exists; if not, return empty ParsedConfig
    // 2. Read file line by line
    // 3. Track state: before block, in block, after block
    // 4. When in block, parse Host entries
    // 5. Return ParsedConfig
}

// parseHostEntry parses a Host block from lines
func parseHostEntry(lines []string) (*HostEntry, error) {
    // Parse lines like:
    // Host shed-codelens
    //     HostName mini-desktop.tailnet.ts.net
    //     Port 2222
    //     User codelens
    //     UserKnownHostsFile ~/.shed/known_hosts
}

// ExtractShedName extracts the shed name from a host alias
// "shed-codelens" -> "codelens"
// Returns empty string if not a shed host
func ExtractShedName(hostAlias string) string {
    if strings.HasPrefix(hostAlias, "shed-") {
        return strings.TrimPrefix(hostAlias, "shed-")
    }
    return ""
}
```

**Key Implementation Details:**

1. **Line-by-line parsing**: Read the file line by line to accurately track position relative to the managed block markers.

2. **State machine**: Use a simple state machine with states:
   - `beforeBlock`: Accumulating content before the managed block
   - `inBlock`: Inside the managed block, parsing host entries
   - `afterBlock`: Accumulating content after the managed block

3. **Preserve formatting**: Keep exact whitespace and formatting of content outside the managed block.

4. **Handle missing file**: If `~/.ssh/config` doesn't exist, return an empty `ParsedConfig` (not an error).

5. **Handle missing block**: If the file exists but has no managed block, `HasManagedBlock` is false and `BeforeBlock` contains the entire file content.

### 8.2 Create SSH Config Writer

**File**: `internal/sshconfig/writer.go`

The writer generates new SSH config entries and computes diffs for the `--dry-run` output.

```go
package sshconfig

import (
    "fmt"
    "os"
    "path/filepath"
    "time"

    "github.com/charliek/shed/internal/config"
)

// ShedInfo contains the information needed to generate an SSH config entry
type ShedInfo struct {
    Name       string // Shed name (e.g., "codelens")
    ServerHost string // Server hostname (e.g., "mini-desktop.tailnet.ts.net")
    SSHPort    int    // SSH port (e.g., 2222)
}

// Diff represents changes to be made to the SSH config
type Diff struct {
    Additions []ShedInfo // New sheds to add
    Removals  []string   // Shed names to remove
    Unchanged []string   // Shed names that are already correct
}

// GenerateEntry generates an SSH config entry for a single shed
func GenerateEntry(shed ShedInfo) string {
    return fmt.Sprintf(`Host shed-%s
    HostName %s
    Port %d
    User %s
    UserKnownHostsFile ~/.shed/known_hosts
`, shed.Name, shed.ServerHost, shed.SSHPort, shed.Name)
}

// GenerateManagedBlock generates the complete managed block content
func GenerateManagedBlock(sheds []ShedInfo) string {
    // 1. Start with BEGIN marker
    // 2. Add "Do not edit manually" comment
    // 3. Add "Last updated" timestamp
    // 4. Add blank line
    // 5. Generate entry for each shed
    // 6. End with END marker
}

// ComputeDiff computes what changes are needed
func ComputeDiff(existing []HostEntry, desired []ShedInfo) *Diff {
    // 1. Build maps of existing and desired entries
    // 2. Additions: in desired but not in existing
    // 3. Removals: in existing but not in desired
    // 4. Unchanged: in both and identical
    // 5. Changed: in both but different (treat as removal + addition)
}

// WriteConfig writes the updated SSH config
func WriteConfig(path string, parsed *ParsedConfig, sheds []ShedInfo) error {
    // 1. Generate new managed block
    // 2. Combine: BeforeBlock + managed block + AfterBlock
    // 3. Write atomically (temp file + rename)
    // 4. Preserve or set file mode to 0600
}

// writeAtomic writes content to a file atomically
func writeAtomic(path string, content string, mode os.FileMode) error {
    // 1. Create temp file in same directory
    // 2. Write content to temp file
    // 3. Sync and close temp file
    // 4. Rename temp file to target path
}

// EnsureConfigExists creates ~/.ssh/config with mode 0600 if it doesn't exist
func EnsureConfigExists(path string) error {
    // 1. Check if directory exists, create if not (mode 0700)
    // 2. Check if file exists
    // 3. If not, create empty file with mode 0600
}

// RemoveManagedBlock removes the managed block from the config
func RemoveManagedBlock(path string, parsed *ParsedConfig) error {
    // 1. Combine: BeforeBlock + AfterBlock (no managed block)
    // 2. Write atomically
}
```

**Key Implementation Details:**

1. **Atomic writes**: Always write to a temp file first, then rename. This prevents corruption if the process is interrupted.

2. **File permissions**: SSH config must be mode 0600 (owner read/write only). Check and fix permissions if needed.

3. **Directory creation**: If `~/.ssh` doesn't exist, create it with mode 0700.

4. **Diff calculation**: Compare existing entries to desired entries:
   - Compare by shed name (host alias)
   - An entry is "changed" if any field differs (hostname, port, etc.)
   - Changed entries appear in both Removals and Additions for clarity

5. **Timestamp**: Include the current timestamp in the managed block header for debugging.

### 8.3 Implement ssh-config Command

**File**: `cmd/shed/ssh_config.go`

```go
package main

import (
    "fmt"
    "os"
    "path/filepath"

    "github.com/spf13/cobra"
    "github.com/charliek/shed/internal/config"
    "github.com/charliek/shed/internal/sshconfig"
)

var sshConfigCmd = &cobra.Command{
    Use:   "ssh-config [name]",
    Short: "Generate or install SSH config for IDE integration",
    Long: `Generate SSH config entries for sheds to enable IDE integration.

Examples:
  # Print config for one shed
  shed ssh-config codelens

  # Print config for all known sheds
  shed ssh-config --all

  # Show what changes would be made
  shed ssh-config --all --install --dry-run

  # Install config entries to ~/.ssh/config
  shed ssh-config --all --install

  # Remove all managed entries
  shed ssh-config --uninstall`,
    Args: cobra.MaximumNArgs(1),
    RunE: runSSHConfig,
}

var (
    sshConfigAll       bool
    sshConfigInstall   bool
    sshConfigDryRun    bool
    sshConfigUninstall bool
)

func init() {
    sshConfigCmd.Flags().BoolVar(&sshConfigAll, "all", false, "Generate for all known sheds")
    sshConfigCmd.Flags().BoolVar(&sshConfigInstall, "install", false, "Write to ~/.ssh/config")
    sshConfigCmd.Flags().BoolVar(&sshConfigDryRun, "dry-run", false, "Show changes without applying")
    sshConfigCmd.Flags().BoolVar(&sshConfigUninstall, "uninstall", false, "Remove managed entries")

    rootCmd.AddCommand(sshConfigCmd)
}

func runSSHConfig(cmd *cobra.Command, args []string) error {
    // 1. Load client config to get shed cache and server info
    clientCfg, err := config.LoadClientConfig()
    if err != nil {
        return fmt.Errorf("failed to load config: %w", err)
    }

    // 2. Handle --uninstall
    if sshConfigUninstall {
        return handleUninstall()
    }

    // 3. Determine which sheds to include
    var sheds []sshconfig.ShedInfo
    if len(args) == 1 {
        // Single shed specified
        sheds, err = getShedsForName(clientCfg, args[0])
    } else if sshConfigAll {
        // All known sheds
        sheds, err = getAllKnownSheds(clientCfg)
    } else {
        return fmt.Errorf("specify a shed name or use --all")
    }
    if err != nil {
        return err
    }

    // 4. If not installing, just print the config
    if !sshConfigInstall {
        return printSSHConfig(sheds)
    }

    // 5. Parse existing SSH config
    sshConfigPath := filepath.Join(os.Getenv("HOME"), ".ssh", "config")
    parsed, err := sshconfig.ParseFile(sshConfigPath)
    if err != nil {
        return fmt.Errorf("failed to parse SSH config: %w", err)
    }

    // 6. Compute diff
    diff := sshconfig.ComputeDiff(parsed.ManagedEntries, sheds)

    // 7. Handle --dry-run
    if sshConfigDryRun {
        return printDryRun(sshConfigPath, diff)
    }

    // 8. Write updated config
    if err := sshconfig.WriteConfig(sshConfigPath, parsed, sheds); err != nil {
        return fmt.Errorf("failed to write SSH config: %w", err)
    }

    // 9. Print summary
    fmt.Printf("✓ Updated %s (added %d, removed %d, unchanged %d)\n",
        sshConfigPath,
        len(diff.Additions),
        len(diff.Removals),
        len(diff.Unchanged))

    return nil
}

// getShedsForName returns ShedInfo for a single named shed
func getShedsForName(cfg *config.ClientConfig, name string) ([]sshconfig.ShedInfo, error) {
    // Look up shed in cache
    // Get server info for the shed
    // Return single-element slice
}

// getAllKnownSheds returns ShedInfo for all cached sheds
func getAllKnownSheds(cfg *config.ClientConfig) ([]sshconfig.ShedInfo, error) {
    // Iterate over cfg.Sheds
    // For each shed, look up its server to get host/port
    // Return slice of ShedInfo
}

// printSSHConfig prints SSH config entries to stdout
func printSSHConfig(sheds []sshconfig.ShedInfo) error {
    fmt.Println("# Add to ~/.ssh/config:")
    fmt.Println()
    for _, shed := range sheds {
        fmt.Print(sshconfig.GenerateEntry(shed))
        fmt.Println()
    }
    return nil
}

// printDryRun prints what changes would be made
func printDryRun(path string, diff *sshconfig.Diff) error {
    fmt.Printf("Would modify: %s\n\n", path)

    if len(diff.Additions) > 0 {
        fmt.Println("--- ADDITIONS ---")
        for _, shed := range diff.Additions {
            lines := strings.Split(sshconfig.GenerateEntry(shed), "\n")
            for _, line := range lines {
                if line != "" {
                    fmt.Printf("+ %s\n", line)
                }
            }
            fmt.Println()
        }
    }

    if len(diff.Removals) > 0 {
        fmt.Println("--- REMOVALS ---")
        for _, name := range diff.Removals {
            fmt.Printf("- Host shed-%s    (shed no longer exists)\n", name)
        }
        fmt.Println()
    }

    if len(diff.Unchanged) > 0 {
        fmt.Println("--- UNCHANGED ---")
        for _, name := range diff.Unchanged {
            fmt.Printf("  Host shed-%s       (already correct)\n", name)
        }
        fmt.Println()
    }

    fmt.Println("Run without --dry-run to apply changes.")
    return nil
}

// handleUninstall removes the managed block
func handleUninstall() error {
    sshConfigPath := filepath.Join(os.Getenv("HOME"), ".ssh", "config")

    parsed, err := sshconfig.ParseFile(sshConfigPath)
    if err != nil {
        return fmt.Errorf("failed to parse SSH config: %w", err)
    }

    if !parsed.HasManagedBlock {
        fmt.Println("No managed block found in SSH config")
        return nil
    }

    if err := sshconfig.RemoveManagedBlock(sshConfigPath, parsed); err != nil {
        return fmt.Errorf("failed to remove managed block: %w", err)
    }

    fmt.Printf("✓ Removed managed block from %s\n", sshConfigPath)
    return nil
}
```

### 8.4 Write Unit Tests

**File**: `internal/sshconfig/parser_test.go`

```go
package sshconfig

import (
    "os"
    "path/filepath"
    "testing"
)

func TestParseFileNotExists(t *testing.T) {
    parsed, err := ParseFile("/nonexistent/path")
    if err != nil {
        t.Fatalf("expected no error for missing file, got: %v", err)
    }
    if parsed.HasManagedBlock {
        t.Error("expected HasManagedBlock to be false")
    }
    if parsed.BeforeBlock != "" {
        t.Error("expected BeforeBlock to be empty")
    }
}

func TestParseFileNoManagedBlock(t *testing.T) {
    // Create temp file with no managed block
    content := `Host github.com
    IdentityFile ~/.ssh/github_key

Host work
    HostName work.example.com
`
    // Write to temp file
    // Parse
    // Verify: HasManagedBlock=false, BeforeBlock=content, AfterBlock=""
}

func TestParseFileWithManagedBlock(t *testing.T) {
    content := `Host github.com
    IdentityFile ~/.ssh/github_key

# --- BEGIN SHED MANAGED BLOCK ---
# Do not edit manually - managed by shed CLI
# Last updated: 2026-01-20T10:30:00Z

Host shed-codelens
    HostName mini-desktop.tailnet.ts.net
    Port 2222
    User codelens
    UserKnownHostsFile ~/.shed/known_hosts

# --- END SHED MANAGED BLOCK ---

Host work
    HostName work.example.com
`
    // Write to temp file
    // Parse
    // Verify BeforeBlock, AfterBlock, ManagedEntries
}

func TestParseFilePreservesWhitespace(t *testing.T) {
    // Content with unusual whitespace
    // Verify it's preserved exactly in BeforeBlock/AfterBlock
}

func TestExtractShedName(t *testing.T) {
    tests := []struct {
        input    string
        expected string
    }{
        {"shed-codelens", "codelens"},
        {"shed-my-project", "my-project"},
        {"shed-", ""},
        {"github.com", ""},
        {"work-server", ""},
    }
    for _, tt := range tests {
        result := ExtractShedName(tt.input)
        if result != tt.expected {
            t.Errorf("ExtractShedName(%q) = %q, want %q", tt.input, result, tt.expected)
        }
    }
}
```

**File**: `internal/sshconfig/writer_test.go`

```go
package sshconfig

import (
    "os"
    "path/filepath"
    "strings"
    "testing"
)

func TestGenerateEntry(t *testing.T) {
    shed := ShedInfo{
        Name:       "codelens",
        ServerHost: "mini-desktop.tailnet.ts.net",
        SSHPort:    2222,
    }

    result := GenerateEntry(shed)

    // Verify all expected lines are present
    if !strings.Contains(result, "Host shed-codelens") {
        t.Error("missing Host line")
    }
    if !strings.Contains(result, "HostName mini-desktop.tailnet.ts.net") {
        t.Error("missing HostName line")
    }
    if !strings.Contains(result, "Port 2222") {
        t.Error("missing Port line")
    }
    if !strings.Contains(result, "User codelens") {
        t.Error("missing User line")
    }
    if !strings.Contains(result, "UserKnownHostsFile ~/.shed/known_hosts") {
        t.Error("missing UserKnownHostsFile line")
    }
}

func TestGenerateManagedBlock(t *testing.T) {
    sheds := []ShedInfo{
        {Name: "codelens", ServerHost: "server1.example.com", SSHPort: 2222},
        {Name: "stbot", ServerHost: "server2.example.com", SSHPort: 2222},
    }

    result := GenerateManagedBlock(sheds)

    if !strings.HasPrefix(result, BeginMarker) {
        t.Error("missing BEGIN marker")
    }
    if !strings.HasSuffix(strings.TrimSpace(result), EndMarker) {
        t.Error("missing END marker")
    }
    if !strings.Contains(result, "Host shed-codelens") {
        t.Error("missing codelens entry")
    }
    if !strings.Contains(result, "Host shed-stbot") {
        t.Error("missing stbot entry")
    }
}

func TestComputeDiff(t *testing.T) {
    existing := []HostEntry{
        {Name: "shed-codelens", HostName: "server1.example.com", Port: 2222, User: "codelens"},
        {Name: "shed-old-project", HostName: "server1.example.com", Port: 2222, User: "old-project"},
    }

    desired := []ShedInfo{
        {Name: "codelens", ServerHost: "server1.example.com", SSHPort: 2222},
        {Name: "stbot", ServerHost: "server2.example.com", SSHPort: 2222},
    }

    diff := ComputeDiff(existing, desired)

    // codelens should be unchanged
    // old-project should be removed
    // stbot should be added

    if len(diff.Additions) != 1 || diff.Additions[0].Name != "stbot" {
        t.Errorf("unexpected additions: %v", diff.Additions)
    }
    if len(diff.Removals) != 1 || diff.Removals[0] != "old-project" {
        t.Errorf("unexpected removals: %v", diff.Removals)
    }
    if len(diff.Unchanged) != 1 || diff.Unchanged[0] != "codelens" {
        t.Errorf("unexpected unchanged: %v", diff.Unchanged)
    }
}

func TestWriteConfigPreservesUserContent(t *testing.T) {
    // Create temp directory
    // Create SSH config with user content + managed block
    // Write new sheds
    // Verify user content is preserved exactly
}

func TestWriteConfigAtomicOnError(t *testing.T) {
    // Attempt to write to a directory that doesn't exist
    // Verify original file is unchanged
}

func TestWriteConfigCreatesFileIfMissing(t *testing.T) {
    // Write to a path that doesn't exist
    // Verify file is created with mode 0600
}

func TestEnsureConfigExists(t *testing.T) {
    // Create temp directory
    // Call EnsureConfigExists for ~/.ssh/config path in temp dir
    // Verify ~/.ssh directory created with mode 0700
    // Verify config file created with mode 0600
}
```

### 8.5 Manual Testing

After implementation, verify:

1. **Basic output (no install)**:
   ```bash
   # Should print SSH config for a specific shed
   shed ssh-config codelens

   # Should print SSH config for all cached sheds
   shed ssh-config --all
   ```

2. **Dry run**:
   ```bash
   # Should show additions/removals/unchanged
   shed ssh-config --all --install --dry-run
   ```

3. **Install**:
   ```bash
   # Backup existing config first!
   cp ~/.ssh/config ~/.ssh/config.backup

   # Install entries
   shed ssh-config --all --install

   # Verify managed block exists
   grep "BEGIN SHED MANAGED BLOCK" ~/.ssh/config

   # Verify user content preserved (compare with backup)
   diff ~/.ssh/config.backup.userparts ~/.ssh/config.userparts
   ```

4. **Update existing block**:
   ```bash
   # Create a new shed
   shed create new-test

   # Update SSH config
   shed ssh-config --all --install --dry-run  # Should show addition
   shed ssh-config --all --install

   # Delete the shed
   shed delete new-test --force

   # Update SSH config
   shed ssh-config --all --install --dry-run  # Should show removal
   shed ssh-config --all --install
   ```

5. **Uninstall**:
   ```bash
   shed ssh-config --uninstall
   # Verify managed block removed, user content intact
   ```

6. **IDE integration**:
   ```bash
   # After installing entries
   # Open Cursor or VS Code
   # Cmd+Shift+P -> "Remote-SSH: Connect to Host"
   # Verify shed-{name} appears in the list
   # Select and connect
   # Verify terminal works and /workspace is accessible
   ```

7. **Edge cases**:
   ```bash
   # Missing ~/.ssh/config (should create it)
   mv ~/.ssh/config ~/.ssh/config.backup
   shed ssh-config --all --install
   ls -la ~/.ssh/config  # Should be mode 0600

   # Restore backup
   mv ~/.ssh/config.backup ~/.ssh/config
   shed ssh-config --all --install
   ```

---

## Deliverables Checklist

- [ ] `internal/sshconfig/parser.go` implemented
- [ ] `internal/sshconfig/writer.go` implemented
- [ ] `cmd/shed/ssh_config.go` implemented
- [ ] `shed ssh-config [name]` prints config for single shed
- [ ] `shed ssh-config --all` prints config for all sheds
- [ ] `shed ssh-config --all --install --dry-run` shows diff
- [ ] `shed ssh-config --all --install` writes to ~/.ssh/config
- [ ] `shed ssh-config --uninstall` removes managed block
- [ ] User content outside managed block is preserved exactly
- [ ] File permissions are correct (0600 for config, 0700 for ~/.ssh)
- [ ] Atomic writes prevent corruption
- [ ] Unit tests passing
- [ ] Manual testing with IDE complete

---

## Deviations & Notes

| Item | Description |
|------|-------------|
| | |

---

## Completion Criteria
- All deliverables checked off
- `go test ./internal/sshconfig/...` passes
- Can successfully install and use SSH config with Cursor or VS Code
- Update epic progress tracker to mark Phase 8 complete
