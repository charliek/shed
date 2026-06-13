package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/charliek/shed/internal/config"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Manage shed servers",
	Long:  "Add, remove, and manage shed servers.",
}

var serverAddCmd = &cobra.Command{
	Use:   "add <host>",
	Short: "Add a new server",
	Long: `Add a new shed server by hostname or IP address.

The command will connect to the server to fetch its info and SSH host key,
then save the configuration locally.`,
	Args: cobra.ExactArgs(1),
	RunE: runServerAdd,
}

var serverListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured servers",
	Long:  "List all configured servers and their status.",
	Args:  cobra.NoArgs,
	RunE:  runServerList,
}

var serverRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a server",
	Long:  "Remove a server from the configuration.",
	Args:  cobra.ExactArgs(1),
	RunE:  runServerRemove,
}

var serverSetDefaultCmd = &cobra.Command{
	Use:   "set-default <name>",
	Short: "Set the default server",
	Long:  "Set which server to use by default.",
	Args:  cobra.ExactArgs(1),
	RunE:  runServerSetDefault,
}

var (
	serverAddPort        int
	serverAddName        string
	serverAddFingerprint string
	serverAddTrustTOFU   bool
)

func init() {
	serverAddCmd.Flags().IntVarP(&serverAddPort, "port", "p", 8080, "HTTP port of the server")
	serverAddCmd.Flags().StringVarP(&serverAddName, "name", "n", "", "Name for the server (default: server's hostname)")
	serverAddCmd.Flags().StringVar(&serverAddFingerprint, "fingerprint", "", "Expected SSH host-key fingerprint (SHA256:...); verified out-of-band, fails on mismatch")
	serverAddCmd.Flags().BoolVar(&serverAddTrustTOFU, "trust-on-first-use", false, "Trust the server's SSH host key without prompting")

	serverCmd.AddCommand(serverAddCmd)
	serverCmd.AddCommand(serverListCmd)
	serverCmd.AddCommand(serverRemoveCmd)
	serverCmd.AddCommand(serverSetDefaultCmd)

	rootCmd.AddCommand(serverCmd)
}

// isStdinTTY reports whether stdin is an interactive terminal.
func isStdinTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// confirmHostKey verifies or confirms the server's SSH host key before it is
// pinned, closing the add-time MITM window. With an expected fingerprint it
// verifies out-of-band (failing on mismatch). Otherwise: --trust-on-first-use
// accepts; --json refuses to silently trust (machine mode can't prompt, so it
// requires an explicit trust flag); an interactive terminal prompts; a
// non-interactive script trusts on first use so automation isn't blocked.
func confirmHostKey(hostKey, expectedFingerprint string, trustOnFirstUse, interactive, jsonMode bool) error {
	pub, _, _, _, err := gossh.ParseAuthorizedKey([]byte(hostKey))
	if err != nil {
		return fmt.Errorf("failed to parse server SSH host key: %w", err)
	}
	fp := gossh.FingerprintSHA256(pub)

	if expectedFingerprint != "" {
		want := expectedFingerprint
		if !strings.HasPrefix(want, "SHA256:") {
			want = "SHA256:" + want
		}
		if fp != want {
			return fmt.Errorf("SSH host-key fingerprint mismatch: server presented %s, expected %s", fp, want)
		}
		return nil
	}
	if trustOnFirstUse {
		fmt.Fprintf(os.Stderr, "Trusting server SSH host key %s\n", fp)
		return nil
	}
	if jsonMode {
		return fmt.Errorf("refusing to trust SSH host key %s in --json mode; pass --fingerprint or --trust-on-first-use", fp)
	}
	if !interactive {
		// Non-interactive script: trust on first use (preserves automation).
		fmt.Fprintf(os.Stderr, "Trusting server SSH host key %s\n", fp)
		return nil
	}
	fmt.Printf("Server SSH host key fingerprint:\n  %s\n", fp)
	fmt.Print("Trust this host key? [y/N]: ")
	var resp string
	_, _ = fmt.Scanln(&resp)
	if r := strings.ToLower(strings.TrimSpace(resp)); r != "y" && r != "yes" {
		return fmt.Errorf("aborted: server SSH host key not trusted")
	}
	return nil
}

func runServerAdd(cmd *cobra.Command, args []string) error {
	host := args[0]

	if verboseLevel > 0 {
		fmt.Printf("Connecting to %s:%d...\n", host, serverAddPort)
	}

	// Connect and get server info
	client := NewAPIClient(host, serverAddPort, DefaultTimeout)
	info, err := client.GetInfo()
	if err != nil {
		return fmt.Errorf("failed to get server info: %w", err)
	}

	// Get SSH host key
	hostKeyResp, err := client.GetSSHHostKey()
	if err != nil {
		return fmt.Errorf("failed to get SSH host key: %w", err)
	}

	// Verify / confirm the SSH host key before pinning it (closes the
	// add-time MITM window the plain-HTTP bootstrap otherwise leaves open).
	if err := confirmHostKey(hostKeyResp.HostKey, serverAddFingerprint, serverAddTrustTOFU, isStdinTTY(), jsonFlag); err != nil {
		return err
	}

	// Pin the host key now, before persisting the server entry. A failure
	// here is fatal: a server entry without a pinned host key is unusable
	// under strict host-key checking, and a rerun would hit the duplicate check.
	if err := config.AddKnownHost(host, info.SSHPort, hostKeyResp.HostKey); err != nil {
		return fmt.Errorf("failed to save SSH host key: %w", err)
	}

	// Determine server name
	name := serverAddName
	if name == "" {
		name = info.Name
	}

	// Check if name already exists
	if _, exists := clientConfig.Servers[name]; exists {
		return fmt.Errorf("server '%s' already exists", name)
	}

	// Add to config
	entry := config.ServerEntry{
		Host:     host,
		HTTPPort: info.HTTPPort,
		SSHPort:  info.SSHPort,
	}
	if err := clientConfig.AddServer(name, entry); err != nil {
		return err
	}

	// Save config
	if err := clientConfig.Save(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "added",
			Name:   name,
			Details: struct {
				Host     string `json:"host"`
				HTTPPort int    `json:"http_port"`
				SSHPort  int    `json:"ssh_port"`
				Default  bool   `json:"default"`
			}{
				Host:     host,
				HTTPPort: info.HTTPPort,
				SSHPort:  info.SSHPort,
				Default:  clientConfig.DefaultServer == name,
			},
		})
	}

	printSuccess("Added server %s (%s:%d)", name, host, info.HTTPPort)
	if clientConfig.DefaultServer == name {
		fmt.Println("  Set as default server")
	}

	return nil
}

func runServerList(cmd *cobra.Command, args []string) error {
	// Sort server names for consistent output
	names := make([]string, 0, len(clientConfig.Servers))
	for name := range clientConfig.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	if jsonFlag {
		type serverJSON struct {
			Name     string `json:"name"`
			Host     string `json:"host"`
			HTTPPort int    `json:"http_port"`
			SSHPort  int    `json:"ssh_port"`
			Status   string `json:"status"`
			Default  bool   `json:"default"`
		}
		result := make([]serverJSON, 0, len(names))
		for _, name := range names {
			entry := clientConfig.Servers[name]
			client := NewAPIClientFromEntry(&entry, DefaultTimeout)
			status := "offline"
			if client.Ping() {
				status = "online"
			}
			result = append(result, serverJSON{
				Name:     name,
				Host:     entry.Host,
				HTTPPort: entry.HTTPPort,
				SSHPort:  entry.SSHPort,
				Status:   status,
				Default:  name == clientConfig.DefaultServer,
			})
		}
		return outputJSON(result)
	}

	if len(clientConfig.Servers) == 0 {
		fmt.Println("No servers configured.")
		fmt.Println("\nTo add a server:")
		fmt.Println("  shed server add <hostname>")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tHOST\tHTTP\tSSH\tSTATUS\tDEFAULT")

	for _, name := range names {
		entry := clientConfig.Servers[name]

		// Check if server is online
		client := NewAPIClientFromEntry(&entry, DefaultTimeout)
		status := "offline"
		if client.Ping() {
			status = "online"
		}

		// Check if default
		defaultMark := ""
		if name == clientConfig.DefaultServer {
			defaultMark = "*"
		}

		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n",
			name, entry.Host, entry.HTTPPort, entry.SSHPort, status, defaultMark)
	}

	w.Flush()
	return nil
}

func runServerRemove(cmd *cobra.Command, args []string) error {
	name := args[0]

	if err := clientConfig.RemoveServer(name); err != nil {
		return err
	}

	if err := clientConfig.Save(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "removed",
			Name:   name,
		})
	}

	printSuccess("Removed server %s", name)
	return nil
}

func runServerSetDefault(cmd *cobra.Command, args []string) error {
	name := args[0]

	if err := clientConfig.SetDefaultServer(name); err != nil {
		return err
	}

	if err := clientConfig.Save(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "set-default",
			Name:   name,
		})
	}

	printSuccess("Set %s as default server", name)
	return nil
}
