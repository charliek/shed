// Package bootstrap mints a shed HTTP token over a server's reserved `_bootstrap`
// SSH channel by invoking the system `ssh` client. Running the real ssh client
// (rather than an in-process SSH library) means the exchange transparently honors
// the user's agent, macOS Keychain, 1Password/Secretive `IdentityAgent`,
// hardware keys, and `~/.ssh/config` — the same way `shed server add`, `shed
// attach`, and the rest of the CLI already reach a server.
//
// ssh remains the security enforcement point: `StrictHostKeyChecking=yes` against
// the shed-only `known_hosts` (with the global file disabled) is what refuses a
// MITM. This package only *classifies* a failure as terminal (a confirmed host-key
// change) versus retryable, so callers like a credential broker can fail closed without
// re-implementing host-key verification. It never logs or returns stdout, which
// carries the freshly minted token.
package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charliek/shed/sdk"
)

// bootstrapUser mirrors the server's reserved SSH username (internal/sshd
// reservedBootstrapUser): connecting as it mints + returns an HTTP token bundle.
const bootstrapUser = "_bootstrap"

// DefaultTimeout bounds the whole ssh exchange when the caller's ctx has no
// earlier deadline. A var so tests can shorten it. The bound is enforced even
// for a cancel-only daemon ctx, so a mint can never hang on a wedged
// ProxyCommand, a slow agent, or a touch-required key with no one present.
var DefaultTimeout = 15 * time.Second

// maxOutputBytes caps captured stdout/stderr so a misbehaving ssh, helper, or
// ProxyCommand can't balloon the daemon's memory. A bundle is well under this.
const maxOutputBytes = 64 << 10

// maxErrStderr bounds how much ssh stderr is echoed into a returned error, so a
// chatty helper can't dump unbounded (possibly sensitive) output into a log.
const maxErrStderr = 2 << 10

// hostKeyChangedMarker is OpenSSH's banner for a *changed* host key — the one
// failure mode that is a confirmed pin mismatch (possible MITM). It is stable,
// unlocalized text; we additionally force LC_ALL=C. A bare "Host key
// verification failed." (no banner) is NOT this — it also covers a missing entry
// — so only this marker latches terminal.
const hostKeyChangedMarker = "REMOTE HOST IDENTIFICATION HAS CHANGED"

// Outcome sentinels for Run, co-located with the function that produces them.
// Only ErrHostKeyMismatch is terminal; the rest are retryable so a caller fails
// closed without permanently wedging a healthy server.
var (
	// ErrHostKeyMismatch marks a confirmed server SSH host-key change — a hard,
	// fail-closed trust failure (a possible MITM). Callers can errors.Is on it to
	// treat the failure as terminal and refuse any fallback to a weaker credential,
	// distinguishing it from a transient/retryable failure.
	ErrHostKeyMismatch = errors.New("sdk/bootstrap: host key mismatch")
	// ErrHostKeyVerificationFailed is a host-key verification failure that is NOT
	// a confirmed change (e.g. a racing/garbled known_hosts, or a missing entry
	// when the caller did not pre-check). Retryable, never a terminal MITM latch.
	ErrHostKeyVerificationFailed = errors.New("sdk/bootstrap: ssh host key verification failed")
	// ErrNoSSHIdentities is a public-key auth failure: the server refused every
	// key ssh offered. The causes differ (no key ssh could reach, a key that is
	// no user's, a passphrase key not in the agent, a user list not in force, a
	// key listed moments ago), so the error names them all (noSSHIdentitiesHint)
	// rather than one. Surfaced distinctly so it is never mistaken for a MITM.
	// Retryable.
	ErrNoSSHIdentities = errors.New("sdk/bootstrap: the server refused every key ssh offered")

	// errNoCertificatePEM / errCertKeyMismatch report a malformed or wrong
	// client_cert in an mtls bundle (see clientKeyPair.matchesIssuedCert).
	errNoCertificatePEM = errors.New("sdk/bootstrap: bundle client_cert is not a PEM CERTIFICATE block")
	errCertKeyMismatch  = errors.New("sdk/bootstrap: issued certificate does not match the submitted key")
	// errNoClientCertificate is the bundle-shape check's error for an mtls
	// bundle whose client_cert is missing, empty or only whitespace.
	errNoClientCertificate = errors.New("sdk/bootstrap: mtls bundle carries no client certificate")
)

// UnusableCertificateError is RunCredential's error for an mtls bundle whose
// client_cert cannot be used: it is missing, empty or only whitespace, it is
// not a PEM CERTIFICATE block, its DER does not parse, or it does not certify
// the key RunCredential submitted. The
// server answered and issued something, so a caller that holds a user to its
// certificates can tell this apart from a failed exchange (errors.As) and
// treat it as an unreadable issuance. Its text is the underlying error's,
// unchanged, and it unwraps to it.
type UnusableCertificateError struct {
	Err error
}

func (e *UnusableCertificateError) Error() string { return e.Err.Error() }

// Unwrap returns the reason the certificate cannot be used.
func (e *UnusableCertificateError) Unwrap() error { return e.Err }

// Params describes one bootstrap exchange. Host/Port address the server's SSH
// endpoint; KnownHostsPath is the pinned trust root (~/.shed/known_hosts);
// Scope is the credential scope ("control"/"credentials"); ClientKind is
// advisory audit metadata ("cli"/"host-agent"/"desktop") and may be empty.
type Params struct {
	Host           string
	Port           int
	KnownHostsPath string
	Scope          string
	ClientKind     string
	// CSRBase64 is a standard-base64 PKCS#10 CertificationRequest DER, sent as
	// the `csr=<value>` request argument so an mtls-mode server can issue a
	// client certificate. Empty means "no CSR" — the legacy, token-only shape.
	//
	// Callers do not build this themselves: RunCredential generates the keypair
	// and fills it in. It is a Params field (rather than an internal detail)
	// only so validate() covers it on the same path as every other argv element.
	CSRBase64 string
	// IdentityFile, when set, is the one private (or public) key file ssh may
	// offer: the argv gains `-i <IdentityFile> -o IdentitiesOnly=yes -o
	// ControlMaster=no -o ControlPath=none` (IdentityArgs) after the fixed
	// options and before `-l`. It is a server entry's identity_file, so an
	// entry enrols with that key first and with no agent-only key, never over
	// a shared connection another key opened. It must
	// pass ValidateIdentityFile (validate refuses it otherwise), and Run checks
	// it is a readable regular file before ssh is spawned (CheckIdentityFile).
	// Empty leaves the argv exactly as it is without the field.
	IdentityFile string
}

// IdentityFileError is a value of Params.IdentityFile (a server entry's
// identity_file) that cannot be handed to ssh. Reason says why, in words that
// follow "cannot be used: ".
type IdentityFileError struct {
	Path   string
	Reason string
}

func (e *IdentityFileError) Error() string {
	return fmt.Sprintf("identity_file %q cannot be used: %s", e.Path, SafeText(e.Reason))
}

// SafeText returns s with every character that is not printable (a control
// character, a line separator, a stray byte that is not UTF-8) written as a Go
// escape (an ESC as backslash-x1b, U+2028 as backslash-u2028, a stray byte as
// backslash-xff), so a message built from a path or a file system error never
// puts a raw control sequence on a terminal. Printable text, spaces included, is
// kept as it is.
func SafeText(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&sb, `\x%02x`, s[i])
		case r == ' ' || unicode.IsPrint(r):
			sb.WriteRune(r)
		default:
			quoted := strconv.QuoteRune(r)
			sb.WriteString(quoted[1 : len(quoted)-1])
		}
		i += size
	}
	return sb.String()
}

// identityFileRefused names each character an identity file path may not
// hold, and why. ssh expands `%` tokens and `${…}` environment references and
// unescapes `\` in both `-i` and an ssh_config `IdentityFile` line; `"` would
// end the quoted `IdentityFile "<path>"` value a generated ssh config writes;
// the desktop's config reader cuts a value at `#`; NUL, CR and LF cannot be
// carried by an argv element or a config line. The rule is the same in every
// language that reads the field (the Rust readers mirror it), so a value one
// side accepts the other never refuses.
var identityFileRefused = []struct {
	char   rune
	reason string
}{
	{0, "it contains a NUL byte"},
	{'\r', "it contains a carriage return"},
	{'\n', "it contains a line feed"},
	{'"', `it contains a double quote ("), which would end the quoted IdentityFile value`},
	{'\\', `it contains a backslash (\), which ssh unescapes`},
	{'#', "it contains #, which a config reader takes as the start of a comment"},
	{'%', "it contains %, which ssh expands as a token"},
	{'$', "it contains $, which ssh expands as an environment variable"},
}

// ValidateIdentityFile is the one rule for the value of an identity file, on
// every path that reads it (the CLI's --identity-file flag, a hand-edited
// config.yaml, and Params.IdentityFile): it must be an absolute path (it
// begins with "/"); it may not be empty, only whitespace, or begin or end
// with whitespace (Unicode White_Space); and it may not hold NUL, CR, LF, `"`,
// `\`, `#`, `%` or `$`. It must be valid UTF-8 (YAML and the Rust readers
// cannot carry anything else, so a value one side accepts the other never
// refuses). It says nothing about the file itself: that is CheckIdentityFile,
// at the time of use. The error is an *IdentityFileError.
func ValidateIdentityFile(path string) error {
	refuse := func(reason string) error { return &IdentityFileError{Path: path, Reason: reason} }
	if !utf8.ValidString(path) {
		return refuse("it is not valid UTF-8")
	}
	if strings.TrimSpace(path) == "" {
		if path == "" {
			return refuse("it is empty")
		}
		return refuse("it is only whitespace")
	}
	for _, r := range identityFileRefused {
		if strings.ContainsRune(path, r.char) {
			return refuse(r.reason)
		}
	}
	if first, _ := utf8.DecodeRuneInString(path); unicode.IsSpace(first) {
		return refuse("it begins with whitespace")
	}
	if last, _ := utf8.DecodeLastRuneInString(path); unicode.IsSpace(last) {
		return refuse("it ends with whitespace")
	}
	if !strings.HasPrefix(path, "/") {
		return refuse("it is not an absolute path")
	}
	return nil
}

// CheckIdentityFile is ValidateIdentityFile plus the file itself, checked at
// the time of use: it must exist, be a regular file (a symbolic link to one
// is followed), and be readable by this process (OpenIdentityFile). A public
// key file passes (ssh then offers the agent's copy of that key). The error
// is an *IdentityFileError.
func CheckIdentityFile(path string) error {
	f, err := OpenIdentityFile(path)
	if err != nil {
		return err
	}
	_ = f.Close()
	return nil
}

// OpenIdentityFile is ValidateIdentityFile, then opens the file for reading
// and returns it once the OPENED file is known to be a regular file. The open
// never blocks (O_NONBLOCK: a FIFO or a device swapped in for the key is
// opened at once, then refused by the type check), and the type is checked on
// the descriptor (fstat), not on the name, so a swap between a check and the
// open cannot get anything but a regular file past it. The caller closes the
// file. The error is an *IdentityFileError, whose reason never repeats the
// path or carries a raw control character (SafeText).
func OpenIdentityFile(path string) (*os.File, error) {
	if err := ValidateIdentityFile(path); err != nil {
		return nil, err
	}
	refuse := func(reason string) error { return &IdentityFileError{Path: path, Reason: reason} }
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, refuse("the file does not exist")
		}
		return nil, refuse("the file cannot be opened (" + fsReason(err) + ")")
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, refuse("the file cannot be read (" + fsReason(err) + ")")
	}
	switch {
	case info.IsDir():
		_ = f.Close()
		return nil, refuse("it is a directory, not a key file")
	case !info.Mode().IsRegular():
		_ = f.Close()
		return nil, refuse("it is not a regular file")
	}
	return f, nil
}

// fsReason is a file system error's own words without the path it names (a
// *fs.PathError repeats the path, which the message already shows quoted),
// with any control character escaped.
func fsReason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return SafeText(err.Error())
}

// IdentityArgs is the ssh argv an identity file adds, or nil for "":
// IdentityKeyArgs (`-i <path> -o IdentitiesOnly=yes`) and then `-o
// ControlMaster=no -o ControlPath=none`, so the ssh never rides (or opens) a
// shared connection: a master another key opened, through a ControlMaster /
// ControlPath in the person's own ssh config, would let this ssh reach the
// server without authenticating with the entry's key at all. Options given on
// the command line win over every ssh config file. It checks nothing: callers
// validate first (ValidateIdentityFile, CheckIdentityFile). It is the one
// spelling, shared by the bootstrap argv and every CLI ssh argv.
func IdentityArgs(path string) []string {
	if path == "" {
		return nil
	}
	return append(IdentityKeyArgs(path), "-o", "ControlMaster=no", "-o", "ControlPath=none")
}

// IdentityKeyArgs is the key half of IdentityArgs, `-i <path> -o
// IdentitiesOnly=yes`, or nil for "": for an ssh that keeps a shared
// connection of its own, keyed by the identity file too, and sets its own
// ControlMaster and ControlPath (the roost provider's transport).
func IdentityKeyArgs(path string) []string {
	if path == "" {
		return nil
	}
	return []string{"-i", path, "-o", "IdentitiesOnly=yes"}
}

// Args returns the ssh arguments (without the ssh binary) of a bootstrap
// exchange with p, after validating p as Run does (the identity file's value
// included; not the file itself, which Run checks before it spawns ssh). It
// lets a caller show or compare the exact argv a bootstrap runs.
func Args(p Params) ([]string, error) {
	if err := validate(p); err != nil {
		return nil, err
	}
	return sshArgs(p), nil
}

// sshArgs builds the `ssh` argv for a bootstrap exchange (asserted directly by
// the package's internal tests). The options pin ssh to a strict,
// non-interactive, publickey-only exchange against the shed known_hosts as the
// SOLE trust root:
//   - GlobalKnownHostsFile=/dev/null, VerifyHostKeyDNS=no, KnownHostsCommand=none:
//     the shed known_hosts is the SOLE host-key trust root — a user ~/.ssh/config
//     must not be able to add /etc/ssh/ssh_known_hosts, a DNS SSHFP record, or a
//     helper command as an alternative source.
//   - publickey-only + BatchMode + no password/kbd-interactive: never prompts,
//     fails closed in an unattended daemon.
//   - -l _bootstrap + bare host (not _bootstrap@host) avoids username-parsing
//     ambiguity in the host argument.
//
// `~/.ssh/config` is intentionally NOT disabled (no `-F none`): it is how a user
// points the daemon at a 1Password/Secretive `IdentityAgent` or a specific
// `IdentityFile`. IdentitiesOnly is intentionally left unset so a multi-key
// agent can still offer the allowlisted key — unless p.IdentityFile names the
// one key to offer, which adds `-i <path> -o IdentitiesOnly=yes -o
// ControlMaster=no -o ControlPath=none` (IdentityArgs) after the fixed options
// and before `-l`. But a matching Host stanza must not
// introduce side effects during the unattended mint, so agent forwarding,
// port forwardings, and LocalCommand hooks are force-disabled.
func sshArgs(p Params) []string {
	args := []string{
		"-T",
		"-p", strconv.Itoa(p.Port),
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + p.KnownHostsPath,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "VerifyHostKeyDNS=no", // ~/.ssh/config must not add a DNS trust source
		"-o", "KnownHostsCommand=none", // ...nor a config-provided host-key command
		"-o", "UpdateHostKeys=no",
		"-o", "CheckHostIP=no",
		"-o", "PreferredAuthentications=publickey",
		"-o", "PubkeyAuthentication=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "ChallengeResponseAuthentication=no",
		"-o", "NumberOfPasswordPrompts=0",
		"-o", "ForwardAgent=no", // a matching ~/.ssh/config Host stanza must not
		"-o", "ClearAllForwardings=yes", // forward the agent, open tunnels,
		"-o", "PermitLocalCommand=no", // or run a local hook during the mint
	}
	args = append(args, IdentityArgs(p.IdentityFile)...)
	args = append(args,
		"-l", bootstrapUser,
		p.Host,
		p.Scope,
	)
	if p.ClientKind != "" {
		args = append(args, p.ClientKind)
	}
	// The server parses everything after the scope order-independently, so the
	// CSR is appended last and a kind-less request (`control csr=...`) is equally
	// valid. A pre-mtls server only ever inspected position 1, so the extra
	// argument is silently ignored there — which is what makes always sending it
	// safe against every server generation.
	if p.CSRBase64 != "" {
		args = append(args, csrArgPrefix+p.CSRBase64)
	}
	return args
}

// csrArgPrefix names the CSR request argument. It MUST match the server's
// csrArgKey in internal/sshd/bootstrap.go.
const csrArgPrefix = "csr="

// validate rejects inputs that could break argv construction or inject ssh
// options before they reach exec. A host that is empty, contains whitespace/an
// `@`, or starts with `-` (option injection) is refused; ports must be in range;
// scope/clientKind must be single tokens (the server parses "<scope> [<kind>]").
func validate(p Params) error {
	switch {
	case p.Host == "":
		return errors.New("sdk/bootstrap: host required")
	case strings.HasPrefix(p.Host, "-"):
		return fmt.Errorf("sdk/bootstrap: invalid host %q (looks like an option)", p.Host)
	case strings.ContainsAny(p.Host, " \t\r\n\x00@"):
		return fmt.Errorf("sdk/bootstrap: invalid host %q", p.Host)
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("sdk/bootstrap: invalid port %d", p.Port)
	}
	if p.KnownHostsPath == "" {
		return errors.New("sdk/bootstrap: known_hosts path required")
	}
	if p.Scope == "" || strings.ContainsAny(p.Scope, " \t\r\n") {
		return fmt.Errorf("sdk/bootstrap: invalid scope %q", p.Scope)
	}
	if strings.ContainsAny(p.ClientKind, " \t\r\n") {
		return fmt.Errorf("sdk/bootstrap: invalid client kind %q", p.ClientKind)
	}
	if p.IdentityFile != "" {
		if err := ValidateIdentityFile(p.IdentityFile); err != nil {
			return fmt.Errorf("sdk/bootstrap: %w", err)
		}
	}
	// The CSR rides in a single argv element (`csr=<base64>`) that the server
	// splits on the FIRST "=" and then base64-decodes. Whitespace would split it
	// into two request arguments (the second of which the server would read as a
	// client kind), and a NUL would truncate the argv element at exec. Neither
	// can occur in standard base64, so anything outside that alphabet means the
	// value was not produced by newClientKeyPair and must not reach exec.
	return validateCSRArg(p.CSRBase64)
}

// validateCSRArg enforces that the CSR argument is a single argv token of
// standard-base64 characters. Empty (no CSR) is valid.
//
// It scans the alphabet rather than attempting a decode: encoding/base64
// silently SKIPS "\r" and "\n" while decoding, so a successful decode would
// prove nothing about the argv element staying one token.
func validateCSRArg(csr string) error {
	if csr == "" {
		return nil
	}
	for i := 0; i < len(csr); i++ {
		c := csr[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '+', c == '/', c == '=':
		default:
			// The value itself is not echoed: it is long, and a corrupted one is
			// not more legible for being printed in full.
			return fmt.Errorf("sdk/bootstrap: invalid csr argument: byte %d is not standard base64", i)
		}
	}
	return nil
}

// Run executes the bootstrap exchange via the system ssh client and returns the
// minted bundle. ssh enforces the host-key pin; Run classifies the outcome:
//
//   - terminal sdk.ErrHostKeyMismatch only when ssh exits 255 AND prints the
//     "REMOTE HOST IDENTIFICATION HAS CHANGED" banner (a confirmed change);
//   - ErrHostKeyVerificationFailed / ErrNoSSHIdentities and other non-terminal
//     errors otherwise.
//
// stdout (which carries the credential) is never placed in an error or logged.
//
// Run sends no CSR, so an mtls-mode server refuses it with the upgrade message.
// Use RunCredential to speak to a server of either mode.
func Run(ctx context.Context, p Params) (sdk.Bundle, error) {
	p.CSRBase64 = "" // Run is the token-only entry point; RunCredential owns CSRs
	return run(ctx, p)
}

// RunCredential is the mode-agnostic bootstrap: it generates a fresh P-256
// keypair, submits the CSR alongside the request, and returns whichever
// credential the server issued — a bearer token or a client certificate + the
// matching private key.
//
// The CSR is sent UNCONDITIONALLY, to every server, because the client cannot
// know the server's mode in advance and the mode can change under it. That is
// safe in all three directions:
//
//   - a pre-mtls server only ever read request position 1, so the extra
//     argument is ignored and a token comes back;
//   - a token-mode server accepts and then deliberately ignores csr= for exactly
//     that legacy parity, so a token comes back;
//   - an mtls-mode server signs it.
//
// Always sending it is therefore what makes a mode flip a non-event for the
// caller: whichever mode the server is in today, one exchange returns the
// credential that mode issues.
func RunCredential(ctx context.Context, p Params) (sdk.Credential, error) {
	kp, err := newClientKeyPair()
	if err != nil {
		return sdk.Credential{}, err
	}
	p.CSRBase64 = kp.csrB64

	bundle, err := run(ctx, p)
	if errors.Is(err, errNoClientCertificate) {
		// The server answered with an mtls bundle that carries no certificate:
		// as unusable as a malformed one (matchesIssuedCert), and typed the same.
		// Run and RunWithCSR keep the plain error.
		return sdk.Credential{}, &UnusableCertificateError{Err: err}
	}
	if err != nil {
		return sdk.Credential{}, err
	}
	if bundle.Mode() != sdk.AuthModeMTLS {
		// Token mode: the generated key is simply discarded. Generating it
		// unconditionally costs a P-256 keygen (microseconds) and buys a single
		// code path that does not have to predict the server's mode.
		return sdk.Credential{Bundle: bundle}, nil
	}
	if err := kp.matchesIssuedCert([]byte(bundle.ClientCert)); err != nil {
		return sdk.Credential{}, err
	}
	return sdk.Credential{Bundle: bundle, KeyPEM: kp.keyPEM}, nil
}

// RunWithCSR submits a CSR the CALLER generated and returns the raw bundle.
//
// It is the relay shape, and it exists for exactly one reason: one part of a
// program holds a credential whose private key must never leave it, while the
// SSH channel that can get that key certified belongs to another part (the
// desktop app's broker relays the app's control credential this way, in its
// own Rust code). The key holder generates the keypair and hands over only the
// CSR, and the relaying side runs this — passing the CSR through untouched and
// handing back the certificate the server issued. No private key exists on the
// relaying side of that exchange, which is why RunCredential (which generates
// its own keypair and would return a certificate the key holder cannot use) is
// the wrong entry point for it.
//
// An empty p.CSRBase64 makes this identical to Run, and against an mtls server
// the result is the server's own explicit upgrade error — the honest answer for
// a caller that had no CSR to relay.
//
// The returned Bundle is validated for shape (an mtls bundle must carry a
// certificate, a token bundle a token) but NOT against a private key: this side
// does not have one. Verifying that the certificate matches the key is the
// caller's job, at the end of the relay, where the key is.
func RunWithCSR(ctx context.Context, p Params) (sdk.Bundle, error) {
	return run(ctx, p)
}

// run is the shared body of Run, RunCredential and RunWithCSR.
func run(ctx context.Context, p Params) (sdk.Bundle, error) {
	if err := validate(p); err != nil {
		return sdk.Bundle{}, err
	}
	// The identity file is checked at the time of use, before ssh is spawned:
	// a key removed since it was set fails here with its own reason, not as
	// an ssh that "refused every key".
	if p.IdentityFile != "" {
		if err := CheckIdentityFile(p.IdentityFile); err != nil {
			return sdk.Bundle{}, fmt.Errorf("sdk/bootstrap: %w", err)
		}
	}

	sshPath, err := lookSSH()
	if err != nil {
		return sdk.Bundle{}, err
	}

	// Enforce a bound even when the caller's ctx has none (a cancel-only daemon
	// ctx) — exec.CommandContext alone would let ssh hang indefinitely. A shorter
	// caller deadline still wins.
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, sshPath, sshArgs(p)...)
	cmd.Env = cLocaleEnv()
	// On ctx expiry exec kills ssh, but a surviving ProxyCommand/helper child can
	// hold the stdout/stderr pipes open and make cmd.Run() block past the deadline.
	// WaitDelay forces the pipes closed shortly after, so the bound is real.
	cmd.WaitDelay = 3 * time.Second

	var stdout, stderr capWriter
	stdout.max, stderr.max = maxOutputBytes, maxOutputBytes
	// Detect the host-key-CHANGED banner over the FULL stderr stream (not just the
	// capped head), so a noisy ProxyCommand can't bury the one terminal signal.
	stderr.marker = []byte(hostKeyChangedMarker)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if runErr != nil {
		// Distinguish our own timeout / a caller cancel from an ssh-reported
		// failure; both are non-terminal (the server is not implicated).
		if cerr := ctx.Err(); cerr != nil {
			return sdk.Bundle{}, fmt.Errorf("sdk/bootstrap: ssh exchange aborted: %w", cerr)
		}
		exit := -1
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exit = ee.ExitCode()
		}
		return sdk.Bundle{}, classify(runErr, exit, stderr.markerSeen, stderr.String())
	}

	return decodeBundle(stdout.Bytes(), p.Scope)
}

// noSSHIdentitiesHint follows ErrNoSSHIdentities in classify's error: the
// causes of a refusal of every key, none of which ssh can tell apart. It names
// no server setting, because where a key is listed (the server's config, GitHub,
// an account service) is the server's own choice. The Rust broker's
// BootstrapError::NoSshIdentities text is the same, byte for byte
// (TestNoSSHIdentitiesTextMatchesRust).
const noSSHIdentitiesHint = "(no key ssh can reach, for example an agent ssh cannot find — see IdentityAgent; " +
	"not a user's key; a passphrase key not in the agent; or the server's user list not in force. " +
	"A key listed moments ago can be refused once: try again)"

// classify maps a non-zero ssh exit to a typed error. Only a confirmed host-key
// change (exit 255 + the CHANGED banner, detected over the full stderr stream) is
// the terminal sdk.ErrHostKeyMismatch; everything else is retryable. The stderr
// surfaced in an error is clipped — stdout (the token) is never referenced here.
// exit is ssh's exit code (-1 when the process did not exit normally).
func classify(runErr error, exit int, hostKeyChanged bool, stderrText string) error {
	msg := clip(strings.TrimSpace(stderrText), maxErrStderr)

	switch {
	case exit == 255 && hostKeyChanged:
		return fmt.Errorf("%w: %s", ErrHostKeyMismatch, firstLine(msg))
	case strings.Contains(stderrText, "Host key verification failed"):
		return fmt.Errorf("%w: %s", ErrHostKeyVerificationFailed, msg)
	case strings.Contains(stderrText, "Permission denied (publickey") ||
		strings.Contains(stderrText, "No more authentication methods"):
		// The refusal of every key keeps ssh's words as its cause, and a
		// command prints them: shown escaped (diagText), never raw, so an ESC
		// or a line break a ProxyCommand wrote cannot reach the terminal.
		return fmt.Errorf("%w %s: %s", ErrNoSSHIdentities, noSSHIdentitiesHint,
			diagText(strings.TrimSpace(stderrText), maxErrStderr))
	case msg != "":
		return fmt.Errorf("sdk/bootstrap: ssh exited %d: %s", exit, msg)
	default:
		return fmt.Errorf("sdk/bootstrap: ssh failed: %w", runErr)
	}
}

// decodeBundle validates ssh stdout: a single JSON object, no trailing garbage,
// the credential its declared mode requires, and (when the server echoes one) a
// matching scope. The raw stdout is NEVER included in an error — it carries the
// credential.
//
// Mode dispatch is the one subtle part. A bundle with NO auth_mode key is a
// pre-mtls server's token bundle and must validate exactly as it always did —
// see sdk.Bundle.Mode, which owns that rule.
func decodeBundle(out []byte, wantScope string) (sdk.Bundle, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var b sdk.Bundle
	if err := dec.Decode(&b); err != nil {
		return sdk.Bundle{}, errors.New("sdk/bootstrap: ssh produced no valid bootstrap bundle")
	}
	// Require EOF after the single object — Decoder.More() does not reliably
	// reject trailing data at the top level, so read the next token and insist
	// the stream is exhausted (modulo trailing whitespace).
	if _, err := dec.Token(); err != io.EOF {
		return sdk.Bundle{}, errors.New("sdk/bootstrap: unexpected trailing data after bootstrap bundle")
	}
	if b.Mode() == sdk.AuthModeMTLS {
		if err := validateMTLSBundle(b); err != nil {
			return sdk.Bundle{}, err
		}
	} else if err := validateTokenBundle(b); err != nil {
		return sdk.Bundle{}, err
	}
	if b.Scope != "" && b.Scope != wantScope {
		return sdk.Bundle{}, fmt.Errorf("sdk/bootstrap: scope mismatch: requested %q, got %q", wantScope, b.Scope)
	}
	return b, nil
}

// validateTokenBundle is the pre-mtls validation ladder, unchanged: it is what
// a legacy bundle (no auth_mode) and an explicit token bundle both go through.
func validateTokenBundle(b sdk.Bundle) error {
	if strings.TrimSpace(b.Token) == "" {
		return errors.New("sdk/bootstrap: bootstrap returned an empty token")
	}
	// The bundle must carry a reachable API endpoint — the caller builds the base
	// URL from the dialed host plus HTTPSPort (preferred) or HTTPPort. A token with
	// neither is unusable, so reject it here rather than failing opaquely later.
	if b.HTTPSPort == 0 && b.HTTPPort == 0 {
		return errors.New("sdk/bootstrap: bootstrap bundle has no usable API port")
	}
	// HTTPS is pinned via the TLS fingerprint (see Bundle); an HTTPS port without
	// one can't be verified, so reject it rather than risk an unpinned downgrade.
	if b.HTTPSPort != 0 && strings.TrimSpace(b.TLSCertFingerprint) == "" {
		return errors.New("sdk/bootstrap: bootstrap bundle advertises HTTPS without a TLS fingerprint to pin")
	}
	return nil
}

// validateMTLSBundle checks what an mtls bundle must carry. The requirements
// are strictly tighter than the token shape's: the certificate is the entire
// credential, and mtls exists only on the TLS listener, so an https port and a
// pin are not optional the way they are for a token (which a legacy server may
// still serve over plain HTTP).
func validateMTLSBundle(b sdk.Bundle) error {
	if strings.TrimSpace(b.ClientCert) == "" {
		return errNoClientCertificate
	}
	if b.HTTPSPort == 0 {
		return errors.New("sdk/bootstrap: mtls bundle has no HTTPS port (mtls is only served over TLS)")
	}
	if strings.TrimSpace(b.TLSCertFingerprint) == "" {
		return errors.New("sdk/bootstrap: mtls bundle advertises HTTPS without a TLS fingerprint to pin")
	}
	// A bearer token alongside a certificate would be a server that does not
	// know its own mode; the mtls middleware never reads the Authorization
	// header, so carrying one could only mislead.
	if strings.TrimSpace(b.Token) != "" {
		return errors.New("sdk/bootstrap: mtls bundle unexpectedly carries a bearer token")
	}
	return nil
}

// lookSSH resolves the ssh binary, falling back to the standard macOS path so a
// launchd/Homebrew daemon with a sparse PATH still finds it.
func lookSSH() (string, error) {
	if p, err := exec.LookPath("ssh"); err == nil {
		return p, nil
	}
	const fallback = "/usr/bin/ssh"
	if fi, err := os.Stat(fallback); err == nil && !fi.IsDir() {
		return fallback, nil
	}
	return "", errors.New("sdk/bootstrap: ssh binary not found on PATH")
}

// cLocaleEnv returns the process environment with the locale forced to C so ssh
// emits stable, English diagnostics for classification, while preserving HOME,
// SSH_AUTH_SOCK, PATH, etc. that the daemon needs to reach the agent/config. The
// full environment is forwarded (not an allowlist) because a user ProxyCommand
// may need arbitrary vars — this is the user's own environment reaching the
// user's own ssh/config, the same as `shed attach`/`shed server add`. Appending
// LC_ALL=C is sufficient: it overrides every locale category, and Go's exec uses
// the last value for a duplicate key.
func cLocaleEnv() []string {
	return append(os.Environ(), "LC_ALL=C")
}

// firstLine returns the first non-empty line of s, for a compact error.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return s
}

// capWriter buffers up to max bytes and silently drops the rest, reporting a full
// write so ssh never blocks or errors on a short write. Bounds memory against a
// chatty ssh/ProxyCommand. When marker is set, it additionally reports (via
// markerSeen) whether that byte sequence appeared anywhere in the FULL stream —
// even past the cap and even when split across writes — so a critical signal
// (the host-key-CHANGED banner) is never missed because earlier output filled
// the buffer.
type capWriter struct {
	buf        bytes.Buffer
	max        int
	marker     []byte
	markerSeen bool
	carry      []byte // tail of the previous write, to catch a marker split across writes
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.marker != nil && !c.markerSeen {
		hay := p
		if len(c.carry) > 0 {
			hay = append(append([]byte(nil), c.carry...), p...)
		}
		if bytes.Contains(hay, c.marker) {
			c.markerSeen = true
			c.carry = nil
		} else if n := len(c.marker) - 1; n > 0 {
			if n > len(hay) {
				n = len(hay)
			}
			c.carry = append(c.carry[:0], hay[len(hay)-n:]...)
		}
	}
	if rem := c.max - c.buf.Len(); rem > 0 {
		if len(p) > rem {
			c.buf.Write(p[:rem])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *capWriter) String() string { return c.buf.String() }
func (c *capWriter) Bytes() []byte  { return c.buf.Bytes() }

// clipMarker ends a text clip cut.
const clipMarker = "…(truncated)"

// clip truncates s to at most n bytes, appending clipMarker when cut.
func clip(s string, n int) string {
	head, cut := clipHead(s, n)
	if cut {
		return head + clipMarker
	}
	return s
}

// clipHead is s cut to at most n bytes on a character boundary, and whether it
// was cut. It never cuts a UTF-8 character in two: it backs up over at most
// three continuation bytes, as the Rust broker's clip backs up to a char
// boundary (for valid UTF-8 the two cut at the same byte).
func clipHead(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	end := n
	for end > 0 && end > n-(utf8.UTFMax-1) && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}

// diagText is ssh's stderr as a refusal keeps it for display: cut to at most
// n bytes on a character boundary (clipHead), then escaped, then marked when
// cut. The rule is the Rust broker's diag_text, byte for byte, held to the
// shared table crates/fixtures/bootstrap-stderr/cases.json:
//
//   - printable ASCII (0x20 to 0x7E) other than `\` is itself;
//   - `\` is `\\`, a line feed `\n`, a carriage return `\r`, a tab `\t`;
//   - every other character, a control character such as ESC and any
//     non-ASCII character alike, is `\u{<hex>}` (lowercase, no leading zeros);
//     a byte that is not UTF-8 is `\u{fffd}`, as Rust's lossy decoding makes it.
//
// So the text cannot move the cursor, colour the terminal, start a new line or
// reorder what follows (a bidi control), and an escape in ssh's own words
// cannot pass for one this function wrote.
func diagText(s string, n int) string {
	head, cut := clipHead(s, n)
	var b strings.Builder
	for i := 0; i < len(head); {
		r, size := utf8.DecodeRuneInString(head[i:])
		i += size
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r >= 0x20 && r <= 0x7e:
			b.WriteByte(byte(r))
		default:
			fmt.Fprintf(&b, `\u{%x}`, r)
		}
	}
	if cut {
		b.WriteString(clipMarker)
	}
	return b.String()
}
