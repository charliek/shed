package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
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

var serverUpdateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Update a server's pinned TLS cert fingerprint",
	Long: `Rotate the pinned TLS certificate fingerprint for a server.

Pass --tls-fingerprint <sha256:...> to pin a new fingerprint verified
out-of-band, or --refetch to re-fetch the cert from the server's api_url
and re-pin it (trust-on-first-use or interactive prompt).`,
	Args: cobra.ExactArgs(1),
	RunE: runServerUpdate,
}

var (
	serverAddPort           int
	serverAddName           string
	serverAddFingerprint    string
	serverAddTrustTOFU      bool
	serverAddHTTPSPort      int
	serverAddTLSFingerprint string

	serverUpdateTLSFingerprint string
	serverUpdateRefetch        bool
	serverUpdateTrustTOFU      bool
)

func init() {
	serverAddCmd.Flags().IntVarP(&serverAddPort, "port", "p", 8080, "HTTP port of the server (bootstrap; ignored when --https-port is set)")
	serverAddCmd.Flags().StringVarP(&serverAddName, "name", "n", "", "Name for the server (default: server's hostname)")
	serverAddCmd.Flags().StringVar(&serverAddFingerprint, "fingerprint", "", "Expected SSH host-key fingerprint (SHA256:...); verified out-of-band, fails on mismatch")
	serverAddCmd.Flags().BoolVar(&serverAddTrustTOFU, "trust-on-first-use", false, "Trust the server's SSH host key (and TLS cert) without prompting")
	serverAddCmd.Flags().IntVar(&serverAddHTTPSPort, "https-port", 0, "HTTPS port; when set, bootstrap over TLS and pin the server's self-signed cert")
	serverAddCmd.Flags().StringVar(&serverAddTLSFingerprint, "tls-fingerprint", "", "Expected TLS cert fingerprint (sha256:...); verified out-of-band, fails on mismatch")

	serverUpdateCmd.Flags().StringVar(&serverUpdateTLSFingerprint, "tls-fingerprint", "", "New pinned TLS cert fingerprint (sha256:...) to rotate to")
	serverUpdateCmd.Flags().BoolVar(&serverUpdateRefetch, "refetch", false, "Re-fetch the cert from the server's api_url and re-pin it (TOFU/prompt)")
	serverUpdateCmd.Flags().BoolVar(&serverUpdateTrustTOFU, "trust-on-first-use", false, "With --refetch, trust the re-fetched TLS cert without prompting")

	serverCmd.AddCommand(serverAddCmd)
	serverCmd.AddCommand(serverListCmd)
	serverCmd.AddCommand(serverRemoveCmd)
	serverCmd.AddCommand(serverSetDefaultCmd)
	serverCmd.AddCommand(serverUpdateCmd)

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

// fetchTLSCertFingerprint dials host:port over TLS without verifying (to
// capture the presented self-signed cert), returning its "sha256:<hex>"
// pin. The cert is not trusted yet — the caller confirms the pin first.
func fetchTLSCertFingerprint(host string, port int) (string, error) {
	dialer := &net.Dialer{Timeout: DefaultTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, strconv.Itoa(port)), &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // capturing the cert to pin it, not trusting it yet
	})
	if err != nil {
		return "", fmt.Errorf("failed to connect over TLS to %s:%d: %w", host, port, err)
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", fmt.Errorf("server presented no TLS certificate")
	}
	return servertls.Fingerprint(certs[0].Raw), nil
}

// tlsFingerprintEqual compares two "sha256:<hex>" pins, tolerating a missing
// prefix and case/whitespace differences in the operator-supplied value.
func tlsFingerprintEqual(got, expected string) bool {
	return normalizeTLSFingerprint(got) == normalizeTLSFingerprint(expected)
}

// confirmTLSCert verifies or confirms the server's TLS cert fingerprint before
// it is pinned, mirroring confirmHostKey's discipline: an expected value is
// checked out-of-band (fail on mismatch); otherwise --trust-on-first-use
// accepts, --json refuses to silently trust, and an interactive terminal
// prompts. firstUse distinguishes a first trust (a non-interactive script
// trusts on first use, preserving automation) from a rotation of an existing
// pin (a silent non-interactive re-pin is refused — a MITM at fetch time could
// otherwise permanently repin the client).
func confirmTLSCert(fingerprint, expected string, trustOnFirstUse, interactive, jsonMode, firstUse bool) error {
	if expected != "" {
		if !tlsFingerprintEqual(fingerprint, expected) {
			return fmt.Errorf("TLS cert fingerprint mismatch: server presented %s, expected %s", fingerprint, expected)
		}
		return nil
	}
	if trustOnFirstUse {
		fmt.Fprintf(os.Stderr, "Trusting server TLS cert %s\n", fingerprint)
		return nil
	}
	if jsonMode {
		return fmt.Errorf("refusing to trust TLS cert %s in --json mode; pass --tls-fingerprint or --trust-on-first-use", fingerprint)
	}
	if !interactive {
		if !firstUse {
			return fmt.Errorf("refusing to silently rotate the TLS pin to %s in a non-interactive session; pass --tls-fingerprint or --trust-on-first-use", fingerprint)
		}
		fmt.Fprintf(os.Stderr, "Trusting server TLS cert %s\n", fingerprint)
		return nil
	}
	fmt.Printf("Server TLS cert fingerprint:\n  %s\n", fingerprint)
	fmt.Print("Trust this TLS certificate? [y/N]: ")
	var resp string
	_, _ = fmt.Scanln(&resp)
	if r := strings.ToLower(strings.TrimSpace(resp)); r != "y" && r != "yes" {
		return fmt.Errorf("aborted: server TLS cert not trusted")
	}
	return nil
}

func runServerAdd(cmd *cobra.Command, args []string) error {
	host := args[0]

	// When --https-port is set, bootstrap over TLS: fetch the presented cert,
	// confirm + pin its fingerprint, then drive /api/info + /api/ssh-host-key
	// over the pinned connection. Otherwise the legacy plain-HTTP path runs.
	var apiURL, tlsFingerprint string
	var client *APIClient
	if serverAddHTTPSPort > 0 {
		fp, err := fetchTLSCertFingerprint(host, serverAddHTTPSPort)
		if err != nil {
			return err
		}
		if err := confirmTLSCert(fp, serverAddTLSFingerprint, serverAddTrustTOFU, isStdinTTY(), jsonFlag, true); err != nil {
			return err
		}
		tlsFingerprint = fp
		apiURL = "https://" + net.JoinHostPort(host, strconv.Itoa(serverAddHTTPSPort))
		if verboseLevel > 0 {
			fmt.Printf("Connecting to %s (TLS, pinned)...\n", apiURL)
		}
		client = newAPIClient(apiURL, "", tlsFingerprint, DefaultTimeout)
	} else {
		if verboseLevel > 0 {
			fmt.Printf("Connecting to %s:%d...\n", host, serverAddPort)
		}
		client = NewAPIClient(host, serverAddPort, DefaultTimeout)
	}

	// Connect and get server info
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
		Host:               host,
		HTTPPort:           info.HTTPPort,
		SSHPort:            info.SSHPort,
		APIURL:             apiURL,         // empty for the plain-HTTP path
		TLSCertFingerprint: tlsFingerprint, // empty for the plain-HTTP path
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
				Host               string `json:"host"`
				HTTPPort           int    `json:"http_port"`
				SSHPort            int    `json:"ssh_port"`
				APIURL             string `json:"api_url,omitempty"`
				TLSCertFingerprint string `json:"tls_cert_fingerprint,omitempty"`
				Default            bool   `json:"default"`
			}{
				Host:               host,
				HTTPPort:           info.HTTPPort,
				SSHPort:            info.SSHPort,
				APIURL:             apiURL,
				TLSCertFingerprint: tlsFingerprint,
				Default:            clientConfig.DefaultServer == name,
			},
		})
	}

	if apiURL != "" {
		printSuccess("Added server %s (%s, TLS pinned)", name, apiURL)
	} else {
		printSuccess("Added server %s (%s:%d)", name, host, info.HTTPPort)
	}
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

// normalizeTLSFingerprint canonicalizes an operator-supplied pin to the stored
// "sha256:<lowercase-hex>" form that servertls.Fingerprint emits, so the exact
// string compare in the pinning transport matches.
func normalizeTLSFingerprint(s string) string {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "sha256:")
	return "sha256:" + s
}

// isHTTPSURL reports whether s is an https:// URL — the only scheme a TLS pin
// can apply to (a plain-http client never performs a TLS handshake).
func isHTTPSURL(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "https://")
}

func runServerUpdate(cmd *cobra.Command, args []string) error {
	name := args[0]
	entry, err := clientConfig.GetServer(name)
	if err != nil {
		return err
	}

	// A pin only takes effect over https — the client never does a TLS
	// handshake on a plain-http api_url, so a pin there is silently unused.
	if !isHTTPSURL(entry.APIURL) {
		return fmt.Errorf("server %q has no https api_url; a TLS pin only applies over https — re-add it with --https-port", name)
	}

	var newFingerprint string
	switch {
	case serverUpdateTLSFingerprint != "":
		newFingerprint = normalizeTLSFingerprint(serverUpdateTLSFingerprint)
	case serverUpdateRefetch:
		u, err := url.Parse(entry.APIURL)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("invalid api_url %q: %w", entry.APIURL, err)
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil || port <= 0 {
			return fmt.Errorf("api_url %q has no valid port", entry.APIURL)
		}
		fp, err := fetchTLSCertFingerprint(u.Hostname(), port)
		if err != nil {
			return err
		}
		if fp == entry.TLSCertFingerprint {
			if !jsonFlag {
				printSuccess("Server %s TLS pin unchanged: %s", name, fp)
			}
			return nil
		}
		// firstUse only when there is no prior pin; rotating an existing one
		// must not be silently accepted in a non-interactive session.
		firstUse := entry.TLSCertFingerprint == ""
		if err := confirmTLSCert(fp, "", serverUpdateTrustTOFU, isStdinTTY(), jsonFlag, firstUse); err != nil {
			return err
		}
		newFingerprint = fp
	default:
		return fmt.Errorf("specify --tls-fingerprint <sha256:...> or --refetch")
	}

	entry.TLSCertFingerprint = newFingerprint
	clientConfig.Servers[name] = *entry
	if err := clientConfig.Save(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "updated",
			Name:   name,
		})
	}
	printSuccess("Updated server %s TLS pin: %s", name, newFingerprint)
	return nil
}
