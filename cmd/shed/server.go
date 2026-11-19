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
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
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
	Short: "Update a server's pinned TLS cert fingerprint, credential or SSH key",
	Long: `Rotate the pinned TLS certificate fingerprint for a server, re-enrol its
credential, or set the SSH key it uses.

Pass --tls-fingerprint <sha256:...> to pin a new fingerprint verified
out-of-band, or --refetch to re-read it from the server over the same SSH-first
path 'shed server add' uses (trust-on-first-use or interactive prompt), which
also re-enrolls the entry's credential.

An entry is held to the user its certificates name. --refetch refuses a new
certificate for another user and writes nothing; add --allow-user-change to
make the entry that user. --refetch is also how an entry the CLI refuses (its
certificate names no user, or a user the entry does not record) is enrolled
again.

--identity-file <path> sets the SSH key the entry uses. A certificate is then
accepted for the entry only when the server made it for that key; every ssh
the CLI runs for it offers that key first and no key that is only in your agent
(-i <path> -o IdentitiesOnly=yes), and never rides a shared connection
(-o ControlMaster=no -o ControlPath=none). ssh can still offer an IdentityFile
your own ssh config names for the host, after this key. On its own the flag
changes only that field, and the entry re-enrols with this key on its next use
(a stored certificate made for another key is never presented); with --refetch
the re-enrolment uses the new key now, and nothing is saved unless it succeeds. --identity-file "" clears it. The path is stored absolute (~ is
expanded); it may not contain whitespace at either end, or any of NUL, CR, LF,
" \ # % $; the file must be readable, and its public key must be learnable
without a passphrase (an OpenSSH-format key, or a .pub beside it).`,
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
	serverAddIdentityFile   string

	serverUpdateTLSFingerprint  string
	serverUpdateRefetch         bool
	serverUpdateTrustTOFU       bool
	serverUpdateAllowUserChange bool
	// serverUpdateIdentityFile is --identity-file on `server update`: set
	// tells "not given" from "given as empty" (which clears the field).
	serverUpdateIdentityFile optionalString
)

// optionalString is a string flag that records whether it was given at all,
// so an explicitly empty value (`--identity-file ""`, which clears a field)
// is not mistaken for an absent flag.
type optionalString struct {
	value string
	set   bool
}

func (o *optionalString) String() string { return o.value }
func (o *optionalString) Type() string   { return "string" }
func (o *optionalString) Set(v string) error {
	o.value, o.set = v, true
	return nil
}

func init() {
	serverAddCmd.Flags().IntVarP(&serverAddPort, "port", "p", 8080, "HTTP port; used only by the fallback for a server that issues no SSH credential (ignored when --https-port is set)")
	serverAddCmd.Flags().IntVar(&serverAddSSHPort, "ssh-port", defaultAddSSHPort, "SSH port used for first contact (host-key pin + credential bootstrap)")
	serverAddCmd.Flags().StringVarP(&serverAddName, "name", "n", "", "Name for the server (default: server's hostname)")
	serverAddCmd.Flags().StringVar(&serverAddFingerprint, "fingerprint", "", "Expected SSH host-key fingerprint (SHA256:...); verified out-of-band, fails on mismatch")
	serverAddCmd.Flags().BoolVar(&serverAddTrustTOFU, "trust-on-first-use", false, "Trust the server's SSH host key (and TLS cert) without prompting")
	serverAddCmd.Flags().IntVar(&serverAddHTTPSPort, "https-port", 0, "HTTPS port to reach the server on; overrides the port its bootstrap bundle reports, and selects the TLS endpoint for the no-credential fallback")
	serverAddCmd.Flags().StringVar(&serverAddTLSFingerprint, "tls-fingerprint", "", "Expected TLS cert fingerprint (sha256:...); verified out-of-band, fails on mismatch")
	serverAddCmd.Flags().BoolVar(&serverAddSecure, "secure", false, "Fallback only: when the server issues no SSH credential, go straight to TLS on the default secure port instead of probing plain HTTP")
	serverAddCmd.Flags().StringVar(&serverAddIdentityFile, "identity-file", "", "SSH private key this entry uses: its certificates must be made for that key, and every ssh for the entry offers it first and no agent-only key; stored as an absolute path, ~ expanded")

	serverUpdateCmd.Flags().StringVar(&serverUpdateTLSFingerprint, "tls-fingerprint", "", "New pinned TLS cert fingerprint (sha256:...) to rotate to")
	serverUpdateCmd.Flags().BoolVar(&serverUpdateRefetch, "refetch", false, "Re-fetch the cert from the server's api_url and re-pin it (TOFU/prompt)")
	serverUpdateCmd.Flags().BoolVar(&serverUpdateTrustTOFU, "trust-on-first-use", false, "With --refetch, trust the re-fetched TLS cert without prompting")
	serverUpdateCmd.Flags().BoolVar(&serverUpdateAllowUserChange, "allow-user-change", false, "With --refetch, let the entry change to the user the server issues the new certificate for")
	serverUpdateCmd.Flags().Var(&serverUpdateIdentityFile, "identity-file", "Set the SSH private key this entry uses (\"\" clears it); with --refetch, the re-enrolment uses it")

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
			// User is the user name the entry records (omitted when it
			// records none, or none that is a valid user name); UserImplicit
			// is whether its stored certificate is the implicit user's. The
			// text's `(implicit)` and `-` are never carried here.
			User         string `json:"user,omitempty"`
			UserImplicit bool   `json:"user_implicit"`
			Status       string `json:"status"`
			Default      bool   `json:"default"`
		}
		result := make([]serverJSON, 0, len(names))
		for _, name := range names {
			entry := clientConfig.Servers[name]
			// The probe first: its re-enrolment can record the entry's user,
			// which listedEntryUser then reads from disk.
			status := probeServerStatus(name, entry)
			user := listedEntryUser(name, entry)
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
				User:               user.name,
				UserImplicit:       user.implicit,
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
	fmt.Fprintln(w, "NAME\tENDPOINT\tSSH\tSECURITY\tUSER\tSTATUS\tDEFAULT")

	for _, name := range names {
		entry := clientConfig.Servers[name]

		// Reachability is probed over the entry's real endpoint (BaseURL +
		// pinned cert), so STATUS is correct for secure servers too.
		status := probeServerStatus(name, entry)
		// After the probe: its re-enrolment can record the entry's user.
		user := listedEntryUser(name, entry).label()

		defaultMark := ""
		if name == clientConfig.DefaultServer {
			defaultMark = "*"
		}

		sshAddr := fmt.Sprintf("%s:%d", entry.Host, entry.SSHPort)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			name, entry.BaseURL(), sshAddr, securityLabel(entry), user, status, defaultMark)
	}

	w.Flush()
	return nil
}

// The text USER value of an entry with no user name to show (plan 037 §3.3).
// They are text only: the JSON carries the facts (entryUser), never these.
const (
	// serverUserImplicit: the entry records no user and its stored
	// certificate is the implicit user's (a server with no user list).
	serverUserImplicit = "(implicit)"
	// serverUserNone: no user to show — not an mtls entry (plain HTTP, or a
	// control token), no certificate on file, one that cannot be read or that
	// the CLI refuses, a recorded user that is not a valid user name, or a
	// row that could not be read again.
	serverUserNone = "-"
)

// entryUser is who a server entry is, for `shed server add` and `shed server
// list`, worked out offline and writing nothing (no request, no lock, no
// directory, no permission change).
//
// A certificate's own name is never taken as the entry's user: an entry that
// records no user and holds a certificate for a named user is one the CLI
// refuses (config.LoadEntryCertificate); it has no name here and shows `-`
// beside STATUS `refused`. An entry that records a user is that user whatever
// its certificate says: it is the only user the CLI will ever act as for it.
type entryUser struct {
	// name is the user the entry records, when it is a valid user name (the
	// grammar the server enforces, config.ValidUserName); "" otherwise.
	name string
	// recorded: the entry records a user, valid or not. A recorded value that
	// is not a valid user name (a hand edit) is never shown and never taken
	// as the label: the text shows `-`.
	recorded bool
	// implicit: the entry's stored certificate, read and checked as every
	// reader checks it, is the implicit user's.
	implicit bool
}

// label is the text value: the user's name; `(implicit)` for an entry that
// records no user and holds the implicit user's certificate; else `-`. It is
// built only from a valid user name and the two constants above, so no
// control character or other text from config.yaml can reach the terminal.
func (u entryUser) label() string {
	switch {
	case u.name != "":
		return u.name
	case !u.recorded && u.implicit:
		return serverUserImplicit
	}
	return serverUserNone
}

// entryUserOf works out who entry is from the row it is given and the stored
// certificate the row names, writing nothing (config.ReadEntryCertificate).
func entryUserOf(name string, entry config.ServerEntry) entryUser {
	u := entryUser{recorded: entry.User != ""}
	if config.ValidUserName(entry.User) {
		u.name = entry.User
	}
	if entry.IsMTLS() {
		// Checked against no recorded user, the stored certificate is returned
		// only when it is the implicit user's; a named one is refused, and one
		// that names no user, is unreadable or is missing is an error.
		probe := entry
		probe.User = ""
		if _, err := config.ReadEntryCertificate(name, &probe); err == nil {
			u.implicit = true
		}
	}
	return u
}

// listedEntryUser is who an entry is, for `shed server list`, read after the
// entry was probed: the probe's re-enrolment can record the entry's first
// user, its certificate paths and its auth mode, so the WHOLE row is read
// again from disk (config.ClientConfig.ReadOnDisk, no lock) and the user is
// worked out from that row. A row that cannot be read again, is gone, or no
// longer names the endpoint the listing shows (snapshot) vouches for
// nothing: the zero entryUser, `-` in text and no `user` in JSON.
func listedEntryUser(name string, snapshot config.ServerEntry) entryUser {
	disk, err := clientConfig.ReadOnDisk()
	if err != nil {
		return entryUser{}
	}
	fresh, ok := disk.Servers[name]
	if !ok || !sameServerEndpoint(fresh, snapshot) {
		return entryUser{}
	}
	return entryUserOf(name, fresh)
}

// probeServerStatus is one entry's STATUS for `shed server list`: online,
// offline, or refused. It probes through the request path (APIClient.Status),
// so a certificate the server rejects is re-enrolled once, as for any request.
// An entry the CLI refuses — its stored certificate names no user or another
// user, which is known without a request, or a re-enrolment issued a
// certificate for another user — shows `refused`, and its message is printed
// once to stderr.
func probeServerStatus(name string, entry config.ServerEntry) string {
	client := NewAPIClientFromNamedEntry(name, &entry, DefaultTimeout)
	status, refusal := client.Status()
	if refusal != nil {
		fmt.Fprintf(os.Stderr, "%v\n", refusal)
	}
	return status
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

	// "Is there such a server to remove" is answered from this process's view,
	// deliberately, and NOT re-checked under the lock. Removal is the one
	// mutation here that is idempotent in the direction the user is asking for:
	// if a concurrent `shed server rm` got there first, the requested end state
	// already holds, and failing would be pedantry. So no precondition — just a
	// mutation that tolerates an absent row on either snapshot.
	if _, err := clientConfig.GetServer(name); err != nil {
		return err
	}
	if err := updateClientConfig(func(c *config.ClientConfig) {
		if _, ok := c.Servers[name]; ok {
			// The only error RemoveServer can return is not-found, which this
			// branch has just ruled out.
			_ = c.RemoveServer(name)
		}
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

	// The target must exist in the config as it is ON DISK at the moment of the
	// write, not merely in this process's view of it: `shed server rm other` +
	// `shed server set-default other` racing would otherwise leave
	// default_server naming a row nobody has, and every later command failing
	// on a default the user never chose.
	var missing error
	if err := updateClientConfigChecked(
		func(c *config.ClientConfig) error {
			if _, ok := c.Servers[name]; !ok {
				missing = fmt.Errorf("server '%s' not found", name)
			}
			return missing
		},
		func(c *config.ClientConfig) { c.DefaultServer = name },
	); err != nil {
		if missing != nil {
			return missing
		}
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
	if serverUpdateAllowUserChange && !serverUpdateRefetch {
		return fmt.Errorf("--allow-user-change only applies with --refetch")
	}
	entry, err := clientConfig.GetServer(name)
	if err != nil {
		return err
	}

	// The identity file the entry uses after this command: the flag's value
	// (checked now, the same rule as a hand-edited config.yaml, plus the file
	// itself), or the entry's own when the flag is not given.
	identityFile := entry.IdentityFile
	if serverUpdateIdentityFile.set {
		if identityFile, err = config.IdentityFileFromFlag(name, serverUpdateIdentityFile.value); err != nil {
			return err
		}
	}

	// --refetch is SSH-first (see refetchServerCredential): it reads the pin out
	// of a bootstrap bundle, which is the only way to re-pin an mtls server —
	// there, an unenrolled TLS dial cannot complete a handshake at all. With
	// --identity-file it enrols with the new key, and saves it with the rest.
	if serverUpdateRefetch {
		return refetchServerCredential(name, entry, identityFile)
	}

	// --identity-file on its own changes only that field, on any entry: it is
	// read by ssh, not by the TLS client, so the https check below is not its
	// business.
	if serverUpdateIdentityFile.set && serverUpdateTLSFingerprint == "" {
		return updateIdentityFile(name, *entry, identityFile)
	}

	// A manually supplied pin only takes effect over https — the client never
	// does a TLS handshake on a plain-http api_url, so a pin there is unused.
	if !isHTTPSURL(entry.APIURL) {
		return fmt.Errorf("server %q has no https api_url; a TLS pin only applies over https — re-add it with --https-port", name)
	}
	if serverUpdateTLSFingerprint == "" {
		return fmt.Errorf("specify --tls-fingerprint <sha256:...>, --refetch, or --identity-file <path>")
	}
	newFingerprint := normalizeTLSFingerprint(serverUpdateTLSFingerprint)

	// Write only the pin, onto whatever the entry currently is — not the whole
	// `entry` snapshot read above. A credential refresh in another process may
	// have rewritten this row's token or certificate paths since; re-pinning
	// must not roll those back.
	//
	// The row must still be there, and still be the same server, in the config
	// as it is ON DISK — the GetServer above only proves it was there when this
	// process loaded. Re-creating a row from a lone fingerprint would resurrect
	// a server the user just removed; writing it onto a re-added one would pin
	// a certificate the operator never saw.
	var gone error
	if err := updateClientConfigChecked(
		func(c *config.ClientConfig) error {
			gone = checkStillNames(c, name, *entry)
			return gone
		},
		func(c *config.ClientConfig) {
			stored, ok := c.Servers[name]
			if !ok {
				return // in-memory only; see saveServerEntry
			}
			stored.TLSCertFingerprint = newFingerprint
			if serverUpdateIdentityFile.set {
				stored.IdentityFile = identityFile
			}
			c.Servers[name] = stored
		},
	); err != nil {
		if gone != nil {
			return fmt.Errorf("cannot re-pin server %q: %w", name, gone)
		}
		return fmt.Errorf("failed to save config: %w", err)
	}

	if jsonFlag {
		result := ActionResult{Status: "ok", Action: "updated", Name: name}
		if serverUpdateIdentityFile.set {
			// As `--identity-file` on its own reports it (updateIdentityFile).
			result.Details = struct {
				IdentityFile string `json:"identity_file"`
			}{identityFile}
		}
		return outputJSON(result)
	}
	printSuccess("Updated server %s TLS pin: %s", name, newFingerprint)
	if serverUpdateIdentityFile.set {
		printIdentityFileLine(identityFile)
	}
	return nil
}

// updateIdentityFile is `shed server update <name> --identity-file <path>` on
// its own: it writes only identity_file, onto the row as it is on disk, which
// must still name the endpoint this command read (was). Nothing else in the
// row is touched, so a credential another process wrote meanwhile stays. A
// --refetch running in another process at the same time is refused at its
// commit (refetchDecide compares identity_file), so it never writes back the
// key this command replaced.
func updateIdentityFile(name string, was config.ServerEntry, identityFile string) error {
	var gone error
	if err := updateClientConfigChecked(
		func(c *config.ClientConfig) error {
			gone = checkStillNames(c, name, was)
			return gone
		},
		func(c *config.ClientConfig) {
			stored, ok := c.Servers[name]
			if !ok {
				return // in-memory only; see saveServerEntry
			}
			stored.IdentityFile = identityFile
			c.Servers[name] = stored
		},
	); err != nil {
		if gone != nil {
			return fmt.Errorf("cannot update server %q: %w", name, gone)
		}
		return fmt.Errorf("failed to save config: %w", err)
	}
	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "updated",
			Name:   name,
			Details: struct {
				IdentityFile string `json:"identity_file"`
			}{identityFile},
		})
	}
	if identityFile == "" {
		printSuccess("Cleared the identity file of server %s", name)
		return nil
	}
	printSuccess("Updated server %s identity file: %s", name, sdkbootstrap.SafeText(identityFile))
	return nil
}

// printIdentityFileLine prints the indented line a command that also set the
// identity file adds to its report (`server update`, `server add`, a
// refetch). The path is shown with any control character escaped
// (sdkbootstrap.SafeText, as the typed identity errors show theirs): the rule
// lets a path hold one, and it must not reach the terminal raw.
func printIdentityFileLine(identityFile string) {
	if identityFile == "" {
		fmt.Println("  Identity file: cleared")
		return
	}
	fmt.Printf("  Identity file: %s\n", sdkbootstrap.SafeText(identityFile))
}
