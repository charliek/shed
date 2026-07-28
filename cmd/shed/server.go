package main

import (
	"crypto/tls"
	"errors"
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

First contact is over SSH. The command captures and pins the server's SSH host
key (~/.shed/known_hosts), then mints a credential over the reserved _bootstrap
SSH channel — a control token in auth.mode: token, a client certificate in
auth.mode: mtls — and verifies the resulting endpoint before saving the entry.

An auth.mode: open server, which issues no credential, is added over HTTP
(--port, or --https-port/--secure for TLS). So is a server whose SSH port cannot
be reached at all — but only if it reports auth.mode: open, since a server that
issues credentials over SSH cannot be added without reaching it.

The host-key scan dials directly. When it cannot reach the port, the command
does not stop: the bootstrap shells out to the real ssh client, which honors
~/.ssh/config (HostName/Port/ProxyJump/ProxyCommand) and verifies the host key
itself against ~/.shed/known_hosts. Pin the key there (ssh-keyscan) if you reach
this server through ~/.ssh/config.

Nothing survives a failed add: a host key this command pinned is removed again,
and no credential material or config entry is left behind.`,
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
out-of-band, or --refetch to re-read it from the server over the same SSH-first
path 'shed server add' uses (trust-on-first-use or interactive prompt), which
also re-enrolls the entry's credential.`,
	Args: cobra.ExactArgs(1),
	RunE: runServerUpdate,
}

var (
	serverAddPort           int
	serverAddSSHPort        int
	serverAddName           string
	serverAddFingerprint    string
	serverAddTrustTOFU      bool
	serverAddHTTPSPort      int
	serverAddTLSFingerprint string
	serverAddSecure         bool

	serverUpdateTLSFingerprint string
	serverUpdateRefetch        bool
	serverUpdateTrustTOFU      bool
)

func init() {
	serverAddCmd.Flags().IntVarP(&serverAddPort, "port", "p", 8080, "HTTP port; used only by the fallback for a server that issues no SSH credential (ignored when --https-port is set)")
	serverAddCmd.Flags().IntVar(&serverAddSSHPort, "ssh-port", defaultAddSSHPort, "SSH port used for first contact (host-key pin + credential bootstrap)")
	serverAddCmd.Flags().StringVarP(&serverAddName, "name", "n", "", "Name for the server (default: server's hostname)")
	serverAddCmd.Flags().StringVar(&serverAddFingerprint, "fingerprint", "", "Expected SSH host-key fingerprint (SHA256:...); verified out-of-band, fails on mismatch")
	serverAddCmd.Flags().BoolVar(&serverAddTrustTOFU, "trust-on-first-use", false, "Trust the server's SSH host key (and TLS cert) without prompting")
	serverAddCmd.Flags().IntVar(&serverAddHTTPSPort, "https-port", 0, "HTTPS port to reach the server on; overrides the port its bootstrap bundle reports, and selects the TLS endpoint for the no-credential fallback")
	serverAddCmd.Flags().StringVar(&serverAddTLSFingerprint, "tls-fingerprint", "", "Expected TLS cert fingerprint (sha256:...); verified out-of-band, fails on mismatch")
	serverAddCmd.Flags().BoolVar(&serverAddSecure, "secure", false, "Fallback only: when the server issues no SSH credential, go straight to TLS on the default secure port instead of probing plain HTTP")

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

// isStdoutTTY reports whether stdout is an interactive terminal. Uses
// term.IsTerminal (a real tty ioctl), not os.ModeCharDevice, so /dev/null — a
// char device that is not a tty — correctly reports false.
func isStdoutTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
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

// defaultAddHTTPSPort is the TLS port `shed server add` falls back to — and the
// port `--secure` bootstraps against — when no --https-port is given. It mirrors
// the server's token-mode default; kept as a var so tests can point the
// fallback at a test server.
var defaultAddHTTPSPort = 8443

// isUnreachableErr reports whether err is a transport-level failure to reach the
// server (connection refused, no route, DNS failure, timeout) rather than the
// server responding with an error — only the former should trigger the TLS
// fallback in selectAddTransport.
func isUnreachableErr(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr)
}

// bootstrapTLSClient sets up a pinned-TLS client for host:httpsPort: it fetches
// the presented cert, confirms + pins its fingerprint (interactive prompt, or
// --tls-fingerprint / --trust-on-first-use; never silent-trust under --json),
// and returns a client bound to the resulting https api_url.
func bootstrapTLSClient(host string, httpsPort int) (client *APIClient, apiURL, fingerprint string, err error) {
	fp, err := fetchTLSCertFingerprint(host, httpsPort)
	if err != nil {
		return nil, "", "", err
	}
	if err := confirmTLSCert(fp, serverAddTLSFingerprint, serverAddTrustTOFU, isStdinTTY(), jsonFlag, true); err != nil {
		return nil, "", "", err
	}
	apiURL = "https://" + net.JoinHostPort(host, strconv.Itoa(httpsPort))
	if verboseLevel > 0 {
		fmt.Printf("Connecting to %s (TLS, pinned)...\n", apiURL)
	}
	return newAPIClient(apiURL, "", fp, DefaultTimeout), apiURL, fp, nil
}

// selectAddTransport chooses how `shed server add` reaches the server over HTTP
// and returns a ready client, the pinned TLS api_url + fingerprint (empty for a
// plain-HTTP server), and the fetched /api/info.
//
// This is the FALLBACK path only (see addOverHTTP): first contact is SSH-first,
// and a server that issues a credential over SSH is described by its bootstrap
// bundle, not by this probe. --port, --https-port, and --secure steer this
// choice; --https-port additionally overrides the api_url port on the SSH path.
//
//   - --https-port N  → pinned TLS on N.
//   - --secure        → pinned TLS on the default secure port.
//   - (default)       → try plain HTTP; if it is refused (a TLS-only
//     token-mode server serves no plain-HTTP listener), auto-retry pinned TLS
//     on the default secure port before giving up.
func selectAddTransport(host string) (client *APIClient, apiURL, fingerprint string, info *config.ServerInfo, err error) {
	switch {
	case serverAddHTTPSPort > 0:
		client, apiURL, fingerprint, err = bootstrapTLSClient(host, serverAddHTTPSPort)
	case serverAddSecure:
		client, apiURL, fingerprint, err = bootstrapTLSClient(host, defaultAddHTTPSPort)
	default:
		if verboseLevel > 0 {
			fmt.Printf("Connecting to %s:%d...\n", host, serverAddPort)
		}
		plain := NewAPIClient(host, serverAddPort, DefaultTimeout)
		pinfo, perr := plain.GetInfo()
		if perr == nil {
			return plain, "", "", pinfo, nil
		}
		// Only fall back to TLS when plain HTTP was unreachable; a server that
		// responded with an error is surfaced as-is, not masked by a TLS retry.
		if !isUnreachableErr(perr) {
			return nil, "", "", nil, fmt.Errorf("failed to get server info over plain HTTP: %w", perr)
		}
		// Plain HTTP is unreachable — likely a TLS-only token-mode server.
		// Auto-retry pinned TLS on the default secure port before surfacing an error.
		if !jsonFlag {
			fmt.Fprintf(os.Stderr, "Plain HTTP on %s:%d is unreachable; trying secure TLS on :%d...\n", host, serverAddPort, defaultAddHTTPSPort)
		}
		client, apiURL, fingerprint, err = bootstrapTLSClient(host, defaultAddHTTPSPort)
		if err != nil {
			return nil, "", "", nil, fmt.Errorf("could not reach %s: plain HTTP on :%d failed (%v) and secure TLS on :%d failed (%w) — pass --https-port if the server uses a non-default TLS port", host, serverAddPort, perr, defaultAddHTTPSPort, err)
		}
	}
	if err != nil {
		return nil, "", "", nil, err
	}
	info, err = client.GetInfo()
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("failed to get server info: %w", err)
	}
	return client, apiURL, fingerprint, info, nil
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
			Name               string `json:"name"`
			Host               string `json:"host"`
			Endpoint           string `json:"endpoint"`
			HTTPPort           int    `json:"http_port,omitempty"`
			SSHPort            int    `json:"ssh_port"`
			HTTPSPort          int    `json:"https_port,omitempty"`
			Security           string `json:"security"`
			TLSPinned          bool   `json:"tls_pinned"`
			TLSCertFingerprint string `json:"tls_cert_fingerprint,omitempty"`
			Status             string `json:"status"`
			Default            bool   `json:"default"`
		}
		result := make([]serverJSON, 0, len(names))
		for _, name := range names {
			entry := clientConfig.Servers[name]
			client := NewAPIClientFromNamedEntry(name, &entry, DefaultTimeout)
			status := "offline"
			if client.Ping() {
				status = "online"
			}
			result = append(result, serverJSON{
				Name:               name,
				Host:               entry.Host,
				Endpoint:           entry.BaseURL(),
				HTTPPort:           entry.HTTPPort,
				SSHPort:            entry.SSHPort,
				HTTPSPort:          httpsPortFromAPIURL(entry.APIURL),
				Security:           securityLabel(entry),
				TLSPinned:          entry.TLSCertFingerprint != "",
				TLSCertFingerprint: entry.TLSCertFingerprint,
				Status:             status,
				Default:            name == clientConfig.DefaultServer,
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
	fmt.Fprintln(w, "NAME\tENDPOINT\tSSH\tSECURITY\tSTATUS\tDEFAULT")

	for _, name := range names {
		entry := clientConfig.Servers[name]

		// Reachability is probed over the entry's real endpoint (BaseURL +
		// pinned cert), so STATUS is correct for secure servers too.
		client := NewAPIClientFromNamedEntry(name, &entry, DefaultTimeout)
		status := "offline"
		if client.Ping() {
			status = "online"
		}

		defaultMark := ""
		if name == clientConfig.DefaultServer {
			defaultMark = "*"
		}

		sshAddr := fmt.Sprintf("%s:%d", entry.Host, entry.SSHPort)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			name, entry.BaseURL(), sshAddr, securityLabel(entry), status, defaultMark)
	}

	w.Flush()
	return nil
}

// securityLabel classifies a server entry's control-plane transport for the
// `shed server list` SECURITY column, derived purely from local config (no
// network call, so the list stays fast and works offline):
//
//	secure   — pinned TLS (https api_url + a pinned cert fingerprint)
//	unpinned — https api_url with no pinned fingerprint (the client can't
//	           verify the cert, so requests fail closed — misconfigured)
//	open     — plain http (the trusted-network LAN/tailnet default)
func securityLabel(entry config.ServerEntry) string {
	if isHTTPSURL(entry.APIURL) {
		if entry.TLSCertFingerprint != "" {
			return "secure"
		}
		return "unpinned"
	}
	return "open"
}

// httpsPortFromAPIURL returns the TLS port from an https api_url (443 when the
// url omits one), or 0 when the entry isn't https. Derived from the pinned
// api_url rather than a live /api/info call.
func httpsPortFromAPIURL(apiURL string) int {
	if !isHTTPSURL(apiURL) {
		return 0
	}
	u, err := url.Parse(apiURL)
	if err != nil {
		return 0
	}
	if port, err := strconv.Atoi(u.Port()); err == nil && port > 0 {
		return port
	}
	return 443
}

// hostPortFromAPIURL splits an api_url into the host and port a TLS dial needs.
// It is the parsing half of the legacy --refetch path, kept separate so the
// error wording lives in one place.
func hostPortFromAPIURL(apiURL string) (string, int, error) {
	u, err := url.Parse(apiURL)
	if err != nil || u.Hostname() == "" {
		return "", 0, fmt.Errorf("invalid api_url %q: %w", apiURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 {
		return "", 0, fmt.Errorf("api_url %q has no valid port", apiURL)
	}
	return u.Hostname(), port, nil
}

func runServerRemove(cmd *cobra.Command, args []string) error {
	name := args[0]

	// "Is there such a server" is decided ONCE, here. Inside the update it
	// could not be: Update applies its mutation to the fresh on-disk snapshot
	// and then to the in-memory one, so a not-found error raised on the second
	// application would report a failure for a removal already committed to
	// disk. The mutation itself is idempotent — removing an absent entry is the
	// state the caller asked for — so the two applications always agree.
	if _, err := clientConfig.GetServer(name); err != nil {
		return err
	}
	if err := updateClientConfig(func(c *config.ClientConfig) error {
		if _, ok := c.Servers[name]; ok {
			// Ignores only the not-found error, which the guard above rules out
			// for the in-memory snapshot and the check here rules out for disk.
			_ = c.RemoveServer(name)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	// Delete any client certificate + private key held for this server. Done
	// AFTER the save, so a removal failure leaves an orphan file rather than a
	// config entry pointing at a deleted credential. It is a warning, not an
	// error: the server is already gone from the config and the command must not
	// fail on cleanup of material that is now inert.
	if err := config.RemoveServerCredentials(name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
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

	// Validated once, outside the update, for the same reason as the removal
	// above: a mutation that can fail on its second application would report a
	// failure for a change already on disk. What is left is a plain assignment.
	if _, err := clientConfig.GetServer(name); err != nil {
		return err
	}
	if err := updateClientConfig(func(c *config.ClientConfig) error {
		c.DefaultServer = name
		return nil
	}); err != nil {
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

	// --refetch is SSH-first (see refetchServerCredential): it reads the pin out
	// of a bootstrap bundle, which is the only way to re-pin an mtls server —
	// there, an unenrolled TLS dial cannot complete a handshake at all.
	if serverUpdateRefetch {
		return refetchServerCredential(name, entry)
	}

	// A manually supplied pin only takes effect over https — the client never
	// does a TLS handshake on a plain-http api_url, so a pin there is unused.
	if !isHTTPSURL(entry.APIURL) {
		return fmt.Errorf("server %q has no https api_url; a TLS pin only applies over https — re-add it with --https-port", name)
	}
	if serverUpdateTLSFingerprint == "" {
		return fmt.Errorf("specify --tls-fingerprint <sha256:...> or --refetch")
	}
	newFingerprint := normalizeTLSFingerprint(serverUpdateTLSFingerprint)

	// Write only the pin, onto whatever the entry currently is — not the whole
	// `entry` snapshot read above. A credential refresh in another process may
	// have rewritten this row's token or certificate paths since; re-pinning
	// must not roll those back.
	//
	// The entry's existence was established by the GetServer at the top of this
	// function; a row that has vanished from a snapshot since is skipped rather
	// than raised, because Update applies this twice and an error on the second
	// application would report a failure for a pin already written. Skipping is
	// also the right merge: re-creating the row from a lone fingerprint would
	// resurrect a server the user just removed.
	if err := updateClientConfig(func(c *config.ClientConfig) error {
		stored, ok := c.Servers[name]
		if !ok {
			return nil
		}
		stored.TLSCertFingerprint = newFingerprint
		c.Servers[name] = stored
		return nil
	}); err != nil {
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
