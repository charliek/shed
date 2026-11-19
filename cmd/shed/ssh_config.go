package main

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/sshconfig"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

var sshConfigCmd = &cobra.Command{
	Use:   "ssh-config [name]",
	Short: "Manage SSH config for sheds",
	Long: `Manage SSH config entries for connecting to sheds.

Without flags, prints the SSH config for a shed (or all sheds with --all).
Use --install to add entries to ~/.ssh/config.
Use --dry-run to preview changes without applying them.
Use --uninstall to remove all shed-managed entries.`,
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
	sshConfigCmd.Flags().BoolVarP(&sshConfigAll, "all", "a", false, "Generate config for all sheds")
	sshConfigCmd.Flags().BoolVar(&sshConfigInstall, "install", false, "Install entries to ~/.ssh/config")
	sshConfigCmd.Flags().BoolVar(&sshConfigDryRun, "dry-run", false, "Show what would be changed without making changes")
	sshConfigCmd.Flags().BoolVar(&sshConfigUninstall, "uninstall", false, "Remove all shed-managed entries from ~/.ssh/config")

	rootCmd.AddCommand(sshConfigCmd)
}

type sshEntryJSON struct {
	Name           string `json:"name"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	KnownHostsFile string `json:"known_hosts_file"`
	IdentityFile   string `json:"identity_file,omitempty"`
}

type sshConfigInstallResult struct {
	Path    string   `json:"path"`
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	// Changed names entries whose stanza was rewritten (omitted when none).
	Changed []string `json:"changed,omitempty"`
}

type sshConfigUninstallResult struct {
	Path    string   `json:"path"`
	Removed []string `json:"removed"`
}

func runSSHConfig(cmd *cobra.Command, args []string) error {
	// Validate mutually exclusive flags
	if sshConfigInstall && sshConfigUninstall {
		return fmt.Errorf("cannot specify both --install and --uninstall")
	}

	// Handle uninstall
	if sshConfigUninstall {
		return runSSHConfigUninstall()
	}

	// Determine which sheds to generate config for
	var sheds []shedInfo
	var err error

	if len(args) > 0 {
		// Specific shed requested
		sheds, err = getShedInfo(args[0])
	} else if sshConfigAll {
		// All sheds
		sheds, err = getAllShedsInfo()
	} else {
		// Default: all sheds
		sheds, err = getAllShedsInfo()
	}

	if err != nil {
		return err
	}

	if len(sheds) == 0 {
		if jsonFlag {
			return outputJSON(make([]sshEntryJSON, 0))
		}
		fmt.Println("No sheds found.")
		fmt.Println("\nTo create a shed:")
		fmt.Println("  shed create NAME")
		return nil
	}

	// Generate entries
	entries, sheds, err := generateEntries(sheds)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		// Every shed was skipped, each with its warning above: nothing is
		// printed or written, as when there is no shed at all.
		return fmt.Errorf("no shed's ssh config entry could be written (see the warnings above)")
	}

	// Handle different modes
	if sshConfigInstall || sshConfigDryRun {
		return runSSHConfigInstall(entries, sheds)
	}

	// Default: print config (or JSON)
	if jsonFlag {
		result := make([]sshEntryJSON, 0, len(entries))
		for _, e := range entries {
			result = append(result, sshEntryJSON{
				Name:           e.Name,
				Host:           e.Host,
				Port:           e.Port,
				User:           e.User,
				KnownHostsFile: e.KnownHostsFile,
				IdentityFile:   e.IdentityFile,
			})
		}
		return outputJSON(result)
	}

	printSSHConfig(entries)
	return nil
}

type shedInfo struct {
	name       string
	serverName string
	server     *config.ServerEntry
}

func getShedInfo(name string) ([]shedInfo, error) {
	serverName, entry, err := findShedServer(name)
	if err != nil {
		return nil, err
	}

	return []shedInfo{
		{
			name:       name,
			serverName: serverName,
			server:     entry,
		},
	}, nil
}

func getAllShedsInfo() ([]shedInfo, error) {
	var result []shedInfo
	// Cache rows are collected during the scan and written in ONE locked
	// update at the end: the mutation closure has to be a pure function of the
	// config it is handed (it runs against the fresh on-disk snapshot and then
	// against the in-memory one), so it cannot be interleaved with the HTTP
	// calls that produce the rows.
	type cacheRow struct{ name, server, status string }
	var cacheRows []cacheRow

	// A server whose entry the CLI refuses (clienttoken.IsRefusal) is not one
	// that has no sheds: its refusal is printed, once, whatever the verbosity,
	// and when no server answered at all the command fails rather than print
	// "No sheds found".
	answered, refused := 0, 0

	// Query all servers for their sheds
	for serverName, entry := range clientConfig.Servers {
		entryCopy := entry
		client := NewAPIClientFromNamedEntry(serverName, &entryCopy, DefaultTimeout)
		resp, err := client.ListSheds()
		if err != nil {
			if clienttoken.IsRefusal(err) {
				refused++
				fmt.Fprintf(os.Stderr, "%v\n", err)
				continue
			}
			if verboseLevel > 0 {
				// The entry's name comes from config.yaml: shown escaped.
				fmt.Fprintf(os.Stderr, "Warning: could not reach %s: %v\n", sdkbootstrap.SafeText(serverName), err)
			}
			continue
		}
		answered++

		for _, shed := range resp.Sheds {
			result = append(result, shedInfo{
				name:       shed.Name,
				serverName: serverName,
				server:     &entryCopy,
			})
			// Update cache
			cacheRows = append(cacheRows, cacheRow{shed.Name, serverName, shed.Status})
		}
	}
	if answered == 0 && refused > 0 {
		// Each refusal is printed above; this only says the command failed.
		return nil, fmt.Errorf("no configured server could be used: %d refused (see above)", refused)
	}

	// Save updated cache. One timestamp for the whole batch: Update applies the
	// closure to the fresh on-disk snapshot and then to the in-memory one, and a
	// per-call time.Now() would make those two disagree.
	cachedAt := time.Now()
	if err := updateClientConfig(func(c *config.ClientConfig) {
		for _, r := range cacheRows {
			c.CacheShedAt(r.name, r.server, r.status, cachedAt)
		}
	}); err != nil {
		if verboseLevel > 0 {
			fmt.Fprintf(os.Stderr, "Warning: failed to save cache: %v\n", err)
		}
	}

	// Sort by name for consistent output
	sort.Slice(result, func(i, j int) bool {
		return result[i].name < result[j].name
	})

	return result, nil
}

// generateEntries builds the ssh config stanza for each shed, and returns the
// stanzas and the sheds they are for. A server entry with an identity_file
// gives its sheds' stanzas its identity lines (sshconfig.IdentityLines); the
// value and the file are checked first (ServerEntry.SSHIdentityArgs), and one
// that cannot be used fails the command, named for its entry, before anything
// is printed or written.
//
// A shed whose stanza the ssh config writer refuses (sshconfig.Entry.Validate:
// above all a name from the server's shed list that is not a shed name, which
// could otherwise add lines such as a ProxyCommand to ~/.ssh/config) is
// skipped, with one warning line on standard error naming it escaped. It is
// never printed, written or used to build an alias; the other sheds are.
func generateEntries(sheds []shedInfo) ([]sshconfig.Entry, []shedInfo, error) {
	entries := make([]sshconfig.Entry, 0, len(sheds))
	kept := make([]shedInfo, 0, len(sheds))
	knownHostsPath := config.GetKnownHostsPath()

	for _, shed := range sheds {
		if _, err := shed.server.SSHIdentityArgs(shed.serverName); err != nil {
			return nil, nil, err
		}
		entry := shedSSHConfigEntry(shed.name, shed.server, knownHostsPath)
		if err := entry.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipped shed %s on server %s: %v\n",
				sdkbootstrap.SafeText(shed.name), sdkbootstrap.SafeText(shed.serverName), err)
			continue
		}
		entries = append(entries, entry)
		kept = append(kept, shed)
	}

	return entries, kept, nil
}

// shedSSHConfigEntry is the stanza of one shed on server, checking nothing:
// generateEntries checks the server entry's identity file first.
func shedSSHConfigEntry(shedName string, server *config.ServerEntry, knownHostsPath string) sshconfig.Entry {
	return sshconfig.Entry{
		Name:           "shed-" + shedName,
		Host:           server.Host,
		Port:           server.SSHPort,
		User:           shedName,
		KnownHostsFile: knownHostsPath,
		IdentityFile:   server.IdentityFile,
	}
}

// sshAliasHint is the line `ssh-config --install` prints to say how to reach
// alias: `ssh <alias>` only when the alias is one shed itself would generate
// (sshHostAlias), else the alias named escaped in words, with no command line.
// Shell quoting alone is not enough here: ssh reads its destination again, so
// `ssh 'shed-pat@other-host'` means user shed-pat on other-host, and a name
// with a space is no host name at all.
func sshAliasHint(alias string) string {
	if sshHostAlias(alias) {
		return "ssh " + alias
	}
	return fmt.Sprintf("the Host alias %q (its name is not one ssh reads as a plain host alias, so no command line is shown)", alias)
}

// sshHostAlias reports whether alias is a Host alias of the shape shed
// generates (`shed-<shed>`, `shed-<server>-<shed>`): sshconfig.PlainAlias,
// the rule the ssh config writer holds every alias to. Such a name is one
// shell word as it is, and ssh reads it as a host alias with no user part
// (`@`) and no port.
func sshHostAlias(alias string) bool {
	return sshconfig.PlainAlias(alias)
}

func printSSHConfig(entries []sshconfig.Entry) {
	for i, entry := range entries {
		fmt.Print(sshconfig.GenerateEntry(entry))
		if i < len(entries)-1 {
			fmt.Println()
		}
	}
}

// roostAliasEntries is the stanza of each qualified roost alias
// (`shed-<server>-<shed>`, the name `shed attach` gives a shed whose name is
// on two servers) that the managed block already holds for one of sheds whose
// server entry has an identity_file. Install keeps such an alias, generated
// from the entry as `shed attach` generates it, so a stale one is rewritten
// with the entry's identity lines (as attach rewrites it) rather than kept or
// left for roost to reach the shed through. An alias of a shed whose entry has
// no identity_file is handled as before the field existed.
func roostAliasEntries(parsed *sshconfig.ParsedConfig, sheds []shedInfo, entries []sshconfig.Entry) []sshconfig.Entry {
	have := make(map[string]bool, len(entries))
	for _, e := range entries {
		have[e.Name] = true
	}
	knownHostsPath := config.GetKnownHostsPath()
	var extra []sshconfig.Entry
	for _, shed := range sheds {
		if shed.server.IdentityFile == "" {
			continue
		}
		alias := shedSSHConfigEntry(shed.name, shed.server, knownHostsPath)
		alias.Name = "shed-" + shed.serverName + "-" + shed.name
		// An alias the writer refuses (a server entry name that makes it no
		// plain host alias) is not kept: the regenerated block drops it.
		if have[alias.Name] || parsed.FindEntry(alias.Name) == nil || alias.Validate() != nil {
			continue
		}
		have[alias.Name] = true
		extra = append(extra, alias)
	}
	return extra
}

// runSSHConfigInstall regenerates the whole managed block from entries
// whenever it must change: an addition, a removal, or an entry with an
// identity file whose alias the block holds twice or without exactly its
// identity lines (sshconfig.ComputeDiff). Lines edited by hand inside the
// block are not kept then. A file whose marker lines frame no single block is
// refused, untouched (sshconfig.CheckMarkers).
func runSSHConfigInstall(entries []sshconfig.Entry, sheds []shedInfo) error {
	sshConfigPath := sshconfig.GetSSHConfigPath()

	// Read existing config
	var content string
	data, err := os.ReadFile(sshConfigPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read SSH config: %w", err)
		}
		// File doesn't exist, start fresh
		content = ""
	} else {
		content = string(data)
	}

	if err := sshconfig.CheckMarkers(sshConfigPath, content); err != nil {
		return err
	}

	// Parse existing config
	parsed := sshconfig.Parse(content)
	// Qualified roost aliases of sheds on an entry with an identity file are
	// kept, with that file's lines (generateEntries checked each entry).
	entries = append(entries, roostAliasEntries(parsed, sheds, entries)...)

	// Compute diff
	diff := sshconfig.ComputeDiff(parsed.ManagedEntries, entries)

	// Show diff
	if !diff.HasChanges() && parsed.HasManagedBlock {
		if jsonFlag {
			return outputJSON(ActionResult{
				Status: "ok",
				Action: "installed",
				Details: sshConfigInstallResult{
					Path:    sshConfigPath,
					Added:   []string{},
					Removed: []string{},
				},
			})
		}
		fmt.Println("SSH config is already up to date.")
		return nil
	}

	if !jsonFlag {
		fmt.Printf("SSH config file: %s\n\n", sshConfigPath)

		// Entry and alias names come from shed names, server entry names and
		// whatever the managed block holds, so each is shown escaped
		// (SafeText): a control character never reaches the terminal raw.
		if len(diff.Additions) > 0 {
			fmt.Println("Entries to add:")
			for _, name := range diff.Additions {
				fmt.Printf("  + %s\n", sdkbootstrap.SafeText(name))
			}
		}

		if len(diff.Removals) > 0 {
			fmt.Println("Entries to remove:")
			for _, name := range diff.Removals {
				fmt.Printf("  - %s\n", sdkbootstrap.SafeText(name))
			}
		}

		if len(diff.Changed) > 0 {
			fmt.Println("Entries to update:")
			for _, name := range diff.Changed {
				fmt.Printf("  ~ %s\n", sdkbootstrap.SafeText(name))
			}
		}

		if len(diff.Unchanged) > 0 && verboseLevel > 0 {
			fmt.Println("Entries unchanged:")
			for _, name := range diff.Unchanged {
				fmt.Printf("  = %s\n", sdkbootstrap.SafeText(name))
			}
		}

		if !parsed.HasManagedBlock && len(entries) > 0 {
			fmt.Println("\nManaged block will be created.")
		}
	}

	// Dry run - stop here
	if sshConfigDryRun {
		if jsonFlag {
			return outputJSON(ActionResult{
				Status: "ok",
				Action: "dry-run",
				Details: sshConfigInstallResult{
					Path:    sshConfigPath,
					Added:   diff.Additions,
					Removed: diff.Removals,
					Changed: diff.Changed,
				},
			})
		}
		fmt.Println("\n(dry run - no changes made)")
		return nil
	}

	// Write the config
	err = sshconfig.Write(sshConfigPath, parsed.BeforeBlock, entries, parsed.AfterBlock)
	if err != nil {
		return fmt.Errorf("failed to write SSH config: %w", err)
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "installed",
			Details: sshConfigInstallResult{
				Path:    sshConfigPath,
				Added:   diff.Additions,
				Removed: diff.Removals,
				Changed: diff.Changed,
			},
		})
	}

	printSuccess("Updated SSH config at %s", sshConfigPath)

	// Print usage hint
	if len(entries) > 0 {
		fmt.Println("\nYou can now connect to sheds with:")
		for _, entry := range entries {
			fmt.Println("  " + sshAliasHint(entry.Name))
		}
	}

	return nil
}

func runSSHConfigUninstall() error {
	sshConfigPath := sshconfig.GetSSHConfigPath()

	// Read existing config
	data, err := os.ReadFile(sshConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			if jsonFlag {
				return outputJSON(ActionResult{
					Status: "ok",
					Action: "uninstalled",
					Details: sshConfigUninstallResult{
						Path:    sshConfigPath,
						Removed: []string{},
					},
				})
			}
			fmt.Println("No SSH config file found.")
			return nil
		}
		return fmt.Errorf("failed to read SSH config: %w", err)
	}

	// Parse existing config
	parsed := sshconfig.Parse(string(data))

	if !parsed.HasManagedBlock {
		if jsonFlag {
			return outputJSON(ActionResult{
				Status: "ok",
				Action: "uninstalled",
				Details: sshConfigUninstallResult{
					Path:    sshConfigPath,
					Removed: []string{},
				},
			})
		}
		fmt.Println("No shed-managed entries found in SSH config.")
		return nil
	}

	names := make([]string, 0, len(parsed.ManagedEntries))
	for _, e := range parsed.ManagedEntries {
		names = append(names, e.Name)
	}

	if !jsonFlag {
		// Show what will be removed
		fmt.Printf("SSH config file: %s\n\n", sshConfigPath)
		fmt.Println("Entries to remove:")
		for _, name := range names {
			fmt.Printf("  - %s\n", sdkbootstrap.SafeText(name))
		}
	}

	// Dry run - stop here
	if sshConfigDryRun {
		if jsonFlag {
			return outputJSON(ActionResult{
				Status: "ok",
				Action: "dry-run",
				Details: sshConfigUninstallResult{
					Path:    sshConfigPath,
					Removed: names,
				},
			})
		}
		fmt.Println("\n(dry run - no changes made)")
		return nil
	}

	// Remove the managed block
	err = sshconfig.Remove(sshConfigPath)
	if err != nil {
		return fmt.Errorf("failed to update SSH config: %w", err)
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "uninstalled",
			Details: sshConfigUninstallResult{
				Path:    sshConfigPath,
				Removed: names,
			},
		})
	}

	printSuccess("Removed shed-managed entries from %s", sshConfigPath)
	return nil
}
