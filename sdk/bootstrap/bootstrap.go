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
// change) versus retryable, so callers like the host-agent can fail closed without
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
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

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
	// ErrNoSSHIdentities is a public-key auth failure: the daemon offered no
	// identity ssh could use, or the offered key is not on the server allowlist.
	// Surfaced distinctly so operators see an identity/allowlist problem rather
	// than mistaking it for a MITM. Retryable (the user may fix ssh config / load
	// the agent / get added to the allowlist).
	ErrNoSSHIdentities = errors.New("sdk/bootstrap: ssh could not authenticate with any available identity")

	// errNoCertificatePEM / errCertKeyMismatch report a malformed or wrong
	// client_cert in an mtls bundle (see clientKeyPair.matchesIssuedCert).
	errNoCertificatePEM = errors.New("sdk/bootstrap: bundle client_cert is not a PEM CERTIFICATE block")
	errCertKeyMismatch  = errors.New("sdk/bootstrap: issued certificate does not match the submitted key")
)

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
// agent can still offer the allowlisted key. But a matching Host stanza must not
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
		"-l", bootstrapUser,
		p.Host,
		p.Scope,
	}
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

// run is the shared body of Run and RunCredential.
func run(ctx context.Context, p Params) (sdk.Bundle, error) {
	if err := validate(p); err != nil {
		return sdk.Bundle{}, err
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
		return fmt.Errorf("%w (the daemon may have no SSH identity available — see IdentityAgent docs — or the key may not be on the server allowlist): %s", ErrNoSSHIdentities, msg)
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
		return errors.New("sdk/bootstrap: mtls bundle carries no client certificate")
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

// clip truncates s to at most n bytes, appending an ellipsis marker when cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
