package main

// server_add.go owns first contact with a shed server: `shed server add` and
// the `shed server update --refetch` re-pin that follows the same path.
//
// The flow is SSH-FIRST. Every mode a server can run in answers on its SSH
// port, and only SSH — the reserved `_bootstrap` channel — can hand a client a
// credential it does not already have. The HTTP surface cannot: an
// `auth.mode: mtls` server's HTTPS listener requires a client certificate to
// complete the handshake, so an unenrolled client cannot even read `/api/info`
// there. Probing HTTP first (what this command used to do) therefore made an
// mtls server unaddable, and made every other mode's add depend on a plain-HTTP
// listener that a hardened server does not run.
//
// So the order is: capture and pin the SSH host key, bootstrap over that pinned
// channel, and let the returned bundle describe the server (ports, TLS pin,
// credential shape). The legacy HTTP probe survives as the fallback for exactly
// one case — a server that issues no credential over SSH ("bootstrap requires
// auth.ssh.mode: enforce", i.e. `auth.mode: open`), plus the degenerate case
// where the SSH port cannot be reached at all and there is therefore no SSH
// evidence to act on (see runServerAdd).
//
// Two disciplines run through the whole file:
//
//   - NOTHING SURVIVES A FAILED ADD. The host key this command pins and the
//     credential material it writes are both undone if any later step fails, so
//     a retry starts from the state the user was in before.
//   - THE CONFIG NEVER OUTRUNS THE CREDENTIAL STORE. The save and the credential
//     commit are ordered so that, at every instant, config.yaml names credential
//     material that exists and matches the auth mode recorded beside it (see
//     credentialTxn).

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// defaultAddSSHPort mirrors the server's own default ssh_port (see
// config.ServerConfig defaults). It is the port `shed server add` scans for a
// host key when --ssh-port is not given.
//
// It is a DEFAULT rather than a discovered value on purpose: the host key must
// be pinned before anything is trusted, so there is no authenticated channel to
// learn the port from first. A server on a non-default SSH port is reached with
// --ssh-port, and the failure to reach the default says so.
const defaultAddSSHPort = 2222

// hostKeyScanTimeout bounds the keyscan handshake. A var so tests can shorten
// it. It only has to cover a TCP connect plus a key exchange.
var hostKeyScanTimeout = 10 * time.Second

// keyscanUser is the username offered during the host-key scan. The scan never
// authenticates (it captures the key during the handshake, which completes
// before any auth), so the value is only what shows up in the server's log.
const keyscanUser = "_shed-keyscan"

// sshHostKeyScanFn captures a server's presented SSH host key. A var so tests
// can drive the add flow without standing up an sshd.
var sshHostKeyScanFn = scanSSHHostKey

// stageCredentialsFn stages issued client-certificate material. A var so tests
// can inject a write failure and assert the transaction ordering (see
// credentialTxn).
var stageCredentialsFn = config.StageClientCredentials

// scanSSHHostKey dials host:port, runs the SSH handshake far enough to see the
// server's host key, and returns it in authorized_keys form.
//
// The key is captured in the HostKeyCallback, which the SSH protocol runs
// during key exchange — i.e. BEFORE authentication. So the scan offers no
// credentials at all and treats the inevitable authentication failure as
// success: what matters is whether a key was presented. That is the same thing
// `ssh-keyscan` does, in-process, with a bound.
//
// It dials DIRECTLY (net.DialTimeout), which is not how the bootstrap that
// follows reaches the server: that shells out to the real `ssh`, which honors
// `~/.ssh/config` — HostName, Port, ProxyJump, ProxyCommand. A failure here is
// therefore not proof that the server is unreachable, only that it is not
// reachable this way, and the caller treats it as such (see ensureHostKeyPinned).
func scanSSHHostKey(host string, port int, timeout time.Duration) (string, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return "", fmt.Errorf("could not reach the shed server's SSH port at %s: %w "+
			"(pass --ssh-port if it does not listen on %d)", addr, err, defaultAddSSHPort)
	}
	defer conn.Close()
	// A deadline on the connection itself, not just ClientConfig.Timeout: a peer
	// that completes the TCP connect and then stalls mid-handshake must not hang
	// the command.
	_ = conn.SetDeadline(time.Now().Add(timeout))

	var captured gossh.PublicKey
	cfg := &gossh.ClientConfig{
		User: keyscanUser,
		HostKeyCallback: func(_ string, _ net.Addr, key gossh.PublicKey) error {
			captured = key
			return nil
		},
		Timeout: timeout,
	}
	client, chans, reqs, herr := gossh.NewClientConn(conn, addr, cfg)
	if herr == nil {
		// A server that somehow accepts an authentication-free connection leaves
		// live channels behind; drain and close rather than leak them.
		go gossh.DiscardRequests(reqs)
		go func() {
			for ch := range chans {
				_ = ch.Reject(gossh.Prohibited, "host key scan")
			}
		}()
		_ = client.Close()
	}
	if captured == nil {
		return "", fmt.Errorf("could not read the SSH host key from %s: %w", addr, herr)
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(captured))), nil
}

// normalizeAddHost strips the brackets an operator may wrap an IPv6 literal in.
//
// Everything downstream — net.JoinHostPort, known_hosts's "[host]:port" form,
// and the bare host argument handed to `ssh` — expects the UNbracketed literal
// and adds brackets itself where the syntax needs them. Accepting "[::1]" and
// normalizing it here means the user can paste either form and both produce the
// same known_hosts line and the same config entry.
func normalizeAddHost(host string) string {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return host[1 : len(host)-1]
	}
	return host
}

// scanAddr renders host:port the way every error message in this file names an
// SSH endpoint (bracketing an IPv6 literal).
func scanAddr(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// ---------------------------------------------------------------------------
// known_hosts
// ---------------------------------------------------------------------------

// knownHostStatus is what ~/.shed/known_hosts already says about an endpoint.
type knownHostStatus int

const (
	// hostKeyUnknown: no entry for this endpoint — a first contact, so the key
	// has to be confirmed and pinned.
	hostKeyUnknown knownHostStatus = iota
	// hostKeyPinned: an entry exists and matches the presented key.
	hostKeyPinned
)

// scanRemoteAddr satisfies net.Addr for the known_hosts callback, which takes
// both a hostname and a remote address. Both are the same dialed endpoint here.
type scanRemoteAddr string

func (a scanRemoteAddr) Network() string { return "tcp" }
func (a scanRemoteAddr) String() string  { return string(a) }

// knownHostStatusFor classifies key against the pins already in
// ~/.shed/known_hosts.
//
// A CHANGED key is an error, never a silent re-pin: it is either a MITM or a
// server whose host key was regenerated, and only the operator can tell those
// apart. The error names the file and the fingerprint so the fix is mechanical.
// A missing file is simply "unknown" — that is the normal first-ever add.
func knownHostStatusFor(host string, port int, key gossh.PublicKey) (knownHostStatus, error) {
	path := config.GetKnownHostsPath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return hostKeyUnknown, nil
		}
		return hostKeyUnknown, fmt.Errorf("could not read known_hosts %s: %w", path, err)
	}
	callback, err := knownhosts.New(path)
	if err != nil {
		return hostKeyUnknown, fmt.Errorf("could not parse known_hosts %s: %w", path, err)
	}
	addr := scanAddr(host, port)
	err = callback(addr, scanRemoteAddr(addr), key)
	if err == nil {
		return hostKeyPinned, nil
	}
	var revoked *knownhosts.RevokedError
	if errors.As(err, &revoked) {
		return hostKeyUnknown, fmt.Errorf("the SSH host key for %s is marked @revoked in %s; refusing to add this server", addr, path)
	}
	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) {
		if len(keyErr.Want) == 0 {
			return hostKeyUnknown, nil // no entry yet — first contact
		}
		return hostKeyUnknown, fmt.Errorf(
			"SSH host key mismatch for %s: the server presented %s, but %s already pins a different key. "+
				"Someone may be intercepting the connection, or the server's host key was regenerated. "+
				"Verify the new key out of band, remove the old line from %s, then retry",
			addr, gossh.FingerprintSHA256(key), path, path)
	}
	return hostKeyUnknown, fmt.Errorf("could not verify the SSH host key for %s against %s: %w", addr, path, err)
}

// pinnedHostKey is the outcome of first contact's host-key step: what was
// scanned, and what this command changed on disk.
//
// It carries the exact known_hosts line so a later failure can undo THAT line
// and nothing else, and it records whether the scan reached the server at all,
// which is what the decision table needs to tell "the server refused us" from
// "we never got to talk to the server".
type pinnedHostKey struct {
	host string
	port int
	// key is the presented host key in authorized_keys form, or "" when the
	// direct scan could not reach the endpoint.
	key string
	// added is true only when THIS command appended the known_hosts line — a key
	// that was already pinned is not ours to remove.
	added bool
}

// scanned reports whether we obtained the server's host key ourselves.
func (p pinnedHostKey) scanned() bool { return p.key != "" }

// rollback removes the known_hosts line this command added.
//
// A pinned host key is a trust decision, and a `server add` that failed made no
// trust decision worth keeping: leaving the line behind silently pre-approves
// that endpoint for the next attempt — including the attempt the user makes
// after fixing whatever went wrong, which is exactly when they would want to
// look at the fingerprint again. A line that was already there (added == false)
// is left strictly alone.
func (p pinnedHostKey) rollback() {
	if !p.added {
		return
	}
	if err := config.RemoveKnownHost(p.host, p.port, p.key); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not un-pin the SSH host key for %s: %v\n",
			scanAddr(p.host, p.port), err)
	}
}

// ensureHostKeyPinned performs step one of first contact: capture the server's
// SSH host key, confirm it, and pin it in ~/.shed/known_hosts.
//
// It runs BEFORE any credential exchange, because the bootstrap that follows
// runs under `StrictHostKeyChecking=yes` against that very file — the pin is
// what makes the credential exchange safe. The confirmation semantics are
// confirmHostKey's, unchanged: an expected fingerprint verifies out-of-band,
// --trust-on-first-use accepts, --json refuses to silently trust, an
// interactive terminal prompts.
//
// An endpoint that is ALREADY pinned with the same key is a no-op (no duplicate
// line, no second prompt); an endpoint pinned with a different key is a hard
// error from knownHostStatusFor.
//
// A scan that cannot REACH the endpoint is NOT fatal. The scan dials directly,
// while the bootstrap shells out to `ssh` and so honors the user's ~/.ssh/config
// (HostName, Port, ProxyJump, ProxyCommand) — a server reached through any of
// those is invisible to the scan and perfectly reachable to the bootstrap. In
// that case this returns a pinnedHostKey with no key and lets the exchange
// proceed: ssh remains the enforcement point (StrictHostKeyChecking=yes against
// ~/.shed/known_hosts, with the global known_hosts and every ssh_config-supplied
// host-key source disabled — see sdk/bootstrap.sshArgs), so an unpinned host is
// refused BY SSH rather than silently trusted here. An explicit --fingerprint is
// the exception: the user asked for an out-of-band check that can only be done
// against a key we scanned ourselves, so failing to scan fails the command.
func ensureHostKeyPinned(host string, sshPort int, expectedFingerprint string, trustOnFirstUse bool) (pinnedHostKey, error) {
	pin := pinnedHostKey{host: host, port: sshPort}
	hostKey, err := sshHostKeyScanFn(host, sshPort, hostKeyScanTimeout)
	if err != nil {
		if expectedFingerprint != "" {
			return pin, fmt.Errorf("cannot verify --fingerprint: %w", err)
		}
		fmt.Fprintf(os.Stderr,
			"warning: could not scan the SSH host key at %s: %v\n"+
				"  Continuing: the bootstrap runs the real ssh client, which honors ~/.ssh/config\n"+
				"  (HostName/Port/ProxyJump/ProxyCommand) and verifies the host key itself against\n"+
				"  %s. If ssh reaches this server through ~/.ssh/config, pin its key there first:\n"+
				"    ssh-keyscan -p <port> <hostname> >> %s\n",
			scanAddr(host, sshPort), err, config.GetKnownHostsPath(), config.GetKnownHostsPath())
		return pin, nil
	}
	pin.key = hostKey

	pub, _, _, _, perr := gossh.ParseAuthorizedKey([]byte(hostKey))
	if perr != nil {
		return pin, fmt.Errorf("could not parse the SSH host key presented by %s: %w", scanAddr(host, sshPort), perr)
	}
	status, err := knownHostStatusFor(host, sshPort, pub)
	if err != nil {
		return pin, err
	}
	if status == hostKeyPinned {
		// Already trusted. An explicit --fingerprint is still verified — the user
		// asked for that check, and a stale pin is exactly when it matters.
		if expectedFingerprint != "" {
			return pin, confirmHostKey(hostKey, expectedFingerprint, false, false, jsonFlag)
		}
		if verboseLevel > 0 {
			fmt.Fprintf(os.Stderr, "SSH host key for %s already pinned (%s)\n",
				scanAddr(host, sshPort), gossh.FingerprintSHA256(pub))
		}
		return pin, nil
	}
	if err := confirmHostKey(hostKey, expectedFingerprint, trustOnFirstUse, isStdinTTY(), jsonFlag); err != nil {
		return pin, err
	}
	if err := config.AddKnownHost(host, sshPort, hostKey); err != nil {
		return pin, fmt.Errorf("failed to save SSH host key: %w", err)
	}
	pin.added = true
	return pin, nil
}

// ---------------------------------------------------------------------------
// bootstrap outcome classification
// ---------------------------------------------------------------------------

// addBootstrapOutcome is the decision table for what the SSH bootstrap said.
// Each outcome has exactly one behavior, and only ONE of them falls back to the
// HTTP probe on its own.
type addBootstrapOutcome int

const (
	// bootstrapIssued: a bundle came back (token or client certificate).
	bootstrapIssued addBootstrapOutcome = iota
	// bootstrapOpenServer: the server issues no credentials because it runs
	// auth.mode: open. The only outcome that falls back to the HTTP probe on the
	// strength of what the SERVER said.
	bootstrapOpenServer
	// bootstrapKeyNotAuthorized: the server reached us and refused our SSH key.
	// A hard error — falling back here would silently write an entry with no
	// credential against a server that requires one, and the user would meet the
	// real failure later, somewhere less legible.
	bootstrapKeyNotAuthorized
	// bootstrapUnreachable: ssh could not reach or complete a connection.
	bootstrapUnreachable
	// bootstrapFailed: anything else (a malformed bundle, a host-key verification
	// failure, a server-side error).
	bootstrapFailed
)

// Markers matched against the bootstrap error. Both are stable strings the
// server emits on the `_bootstrap` channel's stderr (internal/sshd/bootstrap.go
// mintBootstrap).
const (
	openServerMarker       = "bootstrap requires auth.ssh.mode: enforce"
	keyNotAuthorizedMarker = "key not authorized"
)

// sshExitWrapper matches how sdk/bootstrap wraps ssh's stderr
// ("sdk/bootstrap: ssh exited 1: <stderr>"). Stripping it is what lets the
// open-mode marker be matched as a WHOLE LINE rather than as a substring.
var sshExitWrapper = regexp.MustCompile(`^sdk/bootstrap: ssh exited -?\d+: `)

// sshUnreachableMarkers are OpenSSH's (LC_ALL=C, forced by sdk/bootstrap)
// transport-level failures. They are matched textually because the exchange
// runs in a subprocess — there is no net.Error to inspect.
var sshUnreachableMarkers = []string{
	"connection refused",
	"connection timed out",
	"connection closed by remote host",
	"no route to host",
	"network is unreachable",
	"could not resolve hostname",
	"name or service not known",
	"nodename nor servname provided",
	"operation timed out",
	"ssh exchange aborted",
}

// keysRefusedError is the bootstrapKeyNotAuthorized refusal of `shed server
// add` and `shed server update --refetch`: the server refused every key ssh
// offered for _bootstrap (plan 037 §3.3). ssh cannot say why, so it names each
// cause a person can act on, and no server setting: a server may take its users
// from its config, from GitHub or from an account service, and only its
// operator knows which. identityFile is the key the entry offers ("" for
// none), named because ssh offers it first. err is the bootstrap's own error,
// kept as the cause.
func keysRefusedError(host, identityFile string, err error) error {
	offered := "every SSH key ssh offered"
	if identityFile != "" {
		offered = fmt.Sprintf("the entry's SSH key %q (and any other key ssh offered)", identityFile)
	}
	return fmt.Errorf("the server refused %s for _bootstrap@%s: none of them is a user's key there, "+
		"or the right key has a passphrase and is not in your agent (shed runs ssh with BatchMode=yes: ssh-add it), "+
		"or the server's user list is not in force and only its local admins can log in. "+
		"A key listed for a user moments ago can be refused once while the server fetches it: try again in a minute. %w",
		offered, sdkbootstrap.SafeText(host), err)
}

// classifyAddBootstrap maps a bootstrap result onto the decision table.
//
// ORDER: every hard outcome is decided FIRST, and open-mode — the one
// recoverable outcome, the one that can downgrade an add to a credential-less
// entry — is what is left. A refused key, an unreachable endpoint, or a
// host-key trust failure can therefore never be read as "this server is open",
// however that failure's text happens to be worded.
func classifyAddBootstrap(err error) addBootstrapOutcome {
	if err == nil {
		return bootstrapIssued
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, keyNotAuthorizedMarker),
		errors.Is(err, sdkbootstrap.ErrNoSSHIdentities):
		return bootstrapKeyNotAuthorized
	case errors.Is(err, sdkbootstrap.ErrHostKeyMismatch),
		errors.Is(err, sdkbootstrap.ErrHostKeyVerificationFailed):
		// A trust failure is never a fallback: ssh refused to talk to this server
		// at all, so nothing it "said" is attributable to the server.
		return bootstrapFailed
	case errors.Is(err, context.DeadlineExceeded), containsAny(msg, sshUnreachableMarkers):
		return bootstrapUnreachable
	case isOpenServerRefusal(err):
		return bootstrapOpenServer
	default:
		return bootstrapFailed
	}
}

// isOpenServerRefusal reports whether the server's answer WAS the open-mode
// refusal, rather than merely containing its text.
//
// The match is line-wise and exact (after stripping sdk/bootstrap's
// "ssh exited N:" wrapper): some line of the failure must BE the marker. A
// substring test is too generous in three ways that all end with a
// credential-requiring server being added without a credential —
// "...enforcement" matches it, so does a longer sentence that quotes the marker,
// and so does a fatal error that happens to be reported on the same line as it.
// Whole-line equality rejects all three while still tolerating the extra lines
// ssh itself can emit around a session (a server banner, a warning).
func isOpenServerRefusal(err error) bool {
	for _, line := range strings.Split(err.Error(), "\n") {
		line = strings.TrimSpace(sshExitWrapper.ReplaceAllString(strings.TrimSpace(line), ""))
		if strings.EqualFold(line, openServerMarker) {
			return true
		}
	}
	return false
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// the credential transaction
// ---------------------------------------------------------------------------

// credentialTxn keeps config.yaml and the credential store consistent across an
// add or a --refetch, both of which have to update the two together.
//
// The invariant it protects: at every instant, the config on disk names
// credential material that EXISTS and matches the auth mode recorded beside it.
// Breaking it strands the user — an entry that says mtls and points at a file
// that was deleted (or that was replaced by a token-mode server's nothing) fails
// on their next command, after the command that caused it reported an error
// about something else entirely.
//
// It is ONE TRANSACTION under the server's credential lock, with the config
// lock nested inside it (the order pinned in cmd/shed/client.go), the same
// shape as the refresh's persistMTLSCredential:
//
//  1. take the credential lock;
//  2. read the row on disk under the config lock and run decide on it: for an
//     add, the name must still be free; for a --refetch, the row must still
//     name the endpoint and still record the user the command started from,
//     so a concurrent --allow-user-change is never overwritten;
//  3. if decide refuses, release and return the refusal, with nothing written;
//  4. commit the staged files with the lock-held commit (or, in token/open
//     mode, nothing yet);
//  5. save the row, re-checking the same condition under the config lock; a
//     failed save puts the previous files back;
//  6. in token/open mode, remove the credential files the saved row no longer
//     names; release.
//
// Holding the one lock across all of it is what keeps another enrolment of
// the same name from interleaving its files with this one's row.
//
// It is not crash-atomic, and keeps no journal, on purpose. A kill between the
// commit (step 4) and the save (step 5) leaves the new pair beside the old row
// (or, for an add, beside no row). The invariant is that this FAILS CLOSED:
// every reader checks the pair against the row it holds
// (config.LoadEntryCertificate), so a pair whose halves do not match is never
// presented, and a certificate for a user the row does not record refuses the
// entry, sending nothing, until `shed server update <name> --refetch` (or, for
// an orphan with no row, until the next add replaces it). No state this
// transaction can leave on disk is ever used to send a request as a user other
// than the one the entry records, and none is adopted silently: a journal
// would only replace that refusal with a silent repair. See
// persistMTLSCredential for the same rule on the refresh path. A failed save
// whose previous files cannot be put back is an error, never a warning.
type credentialTxn struct {
	name string
	// staged is the newly issued pair, written but not yet adopted. nil in
	// token/open mode, where there is no material to write — only, possibly, some
	// to remove.
	staged *config.StagedClientCredentials
	// decide is the condition this transaction is decided against (step 2). nil
	// decides nothing beyond what save checks itself.
	decide func(*config.ClientConfig) error
	// adding says the transaction adds a new entry (no row exists yet), which
	// changes only what a failed restoration tells the user.
	adding bool
}

// stageCredentials writes the entry's new credential material somewhere
// harmless and points the entry at the paths it will occupy once committed.
//
// Nothing the server currently has is touched here, which is the point: a failed
// write (a full disk, a bad permission, an unwritable creds root) has to be
// survivable, and the way to make it survivable is to discover it before
// anything of value has been disturbed.
func stageCredentials(entry *config.ServerEntry, name string, res sdk.Credential, decide func(*config.ClientConfig) error) (*credentialTxn, error) {
	txn := &credentialTxn{name: name, decide: decide}
	if res.Mode() != sdk.AuthModeMTLS {
		entry.ClientCertFile, entry.ClientKeyFile = "", ""
		return txn, nil
	}
	staged, err := stageCredentialsFn(name, []byte(res.Bundle.ClientCert), res.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("could not store the issued client certificate: %w", err)
	}
	txn.staged = staged
	entry.ClientCertFile, entry.ClientKeyFile = staged.Paths()
	return txn, nil
}

// commitAround runs the credential commit and the config save as one
// transaction, serialized against any other process enrolling this same server
// name by the per-server credential lock.
//
// The lock is what makes an interleaved pair of same-name enrollments safe.
// Without it, two `shed server add srv` runs can both commit their material —
// the second overwriting the first at the same fixed paths — and then both save
// their config rows, leaving whichever row lands last describing the OTHER
// enrollment's certificate: an entry whose recorded serial, expiry and auth
// mode belong to a credential the file no longer holds.
//
// LOCK ORDERING: credential lock first, config lock underneath (inside the
// decide read and the save) — the order pinned in cmd/shed/client.go, and never
// the reverse anywhere. The commit is the lock-held variant (CommitHeld): flock
// is not re-entrant, so the ordinary Commit, which takes the lock itself, would
// deadlock the process against its own hold.
func (t *credentialTxn) commitAround(save func() error) error {
	unlock, err := config.LockServerCredentials(t.name)
	if err != nil {
		t.discard()
		return err
	}
	defer unlock()

	if t.decide != nil {
		if err := readClientConfigLocked(t.decide); err != nil {
			t.discard()
			return err
		}
	}

	if t.staged == nil {
		// Token/open mode: there is nothing to write, only, possibly, a
		// directory to remove. The removal runs AFTER the save — it is
		// destructive whenever there is anything to destroy, and ordering it
		// last costs nothing when there is not.
		if err := save(); err != nil {
			return err
		}
		if err := config.RemoveServerCredentials(t.name); err != nil {
			// The config is already correct; a leftover file that nothing
			// points at must not fail a command that otherwise succeeded.
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
		return nil
	}

	// A pair that cannot be read cannot be put back: stop before writing.
	prev, err := storedPairOf(t.name)
	if err != nil {
		t.discard()
		return err
	}
	if err := commitStagedHeldFn(t.staged); err != nil {
		t.discard()
		if rerr := prev.restoreHeld(t.name); rerr != nil {
			return t.restoreFailed(rerr)
		}
		return err
	}
	if err := save(); err != nil {
		// The saved row still names what it named before; put those files
		// back (on a fresh add, there were none, and the directory goes).
		if rerr := prev.restoreHeld(t.name); rerr != nil {
			return t.restoreFailed(rerr)
		}
		return err
	}
	return nil
}

// restoreFailed is the error when a failed commit or save could not put the
// previous files back: never only a warning. For a --refetch, the entry's
// files and its recorded user may now disagree (restoreFailed in client.go,
// which names the refetch). For an add there is no row: nothing reads the
// files left behind, and adding the server again replaces them.
func (t *credentialTxn) restoreFailed(cause error) error {
	if t.adding {
		return fmt.Errorf("could not put back the client credential files of %q after the add failed (%v): "+
			"no entry names them, and `shed server add` again replaces them", t.name, cause)
	}
	return restoreFailed(t.name, cause)
}

// discard throws the staged material away, leaving everything else untouched.
func (t *credentialTxn) discard() {
	t.staged.Discard()
}

// addDecide is the add's step-2 condition: the name is still free on disk.
func addDecide(name string) func(*config.ClientConfig) error {
	return func(c *config.ClientConfig) error {
		if _, exists := c.Servers[name]; exists {
			return fmt.Errorf("server '%s' already exists", name)
		}
		return nil
	}
}

// refetchDecide is a --refetch's condition, at step 2 and again at the save:
// the row still names the endpoint the command read at its start (was), and it
// still records the same user, so a concurrent `--refetch --allow-user-change`
// in another process is never overwritten with this one's view.
func refetchDecide(name string, was config.ServerEntry) func(*config.ClientConfig) error {
	return func(c *config.ClientConfig) error {
		if err := checkStillNames(c, name, was); err != nil {
			return err
		}
		if stored := c.Servers[name]; !sameHeldIdentity(stored, was) {
			return &config.EntryRefusedError{Entry: name, Kind: config.RefusedEntryChanged}
		}
		return nil
	}
}

// andKeyHolds is decide followed by keyHolds (issuedKeyHolds for this
// issuance), so the transaction learns the identity file's key again under its
// locks, just before anything is committed.
func andKeyHolds(decide func(*config.ClientConfig) error, keyHolds func() error) func(*config.ClientConfig) error {
	return func(c *config.ClientConfig) error {
		if err := decide(c); err != nil {
			return err
		}
		return keyHolds()
	}
}

// issuedKeyHolds is the add's and the refetch's check of the identity file at
// each point the issuance would be presented to the server (its first check),
// staged, or committed: the key learned from identityFile NOW must still be entryKey,
// the key learned before the bootstrap ran, and the key the certificate was
// made for (its subject CN). A key replaced at the same path while the command
// ran fails it: nothing is presented or saved, and no copy is written to
// staging unless the key still held when staging began (a copy staged just
// before a later change is discarded by the check under the locks)
// (RefusedEntryUnreadable, the refusal the refresh path uses for the same
// change; or the file's *config.IdentityFileError when its key cannot be
// learned now). No identity file, or no certificate, reads no file.
func issuedKeyHolds(name, host, identityFile, entryKey string, res sdk.Credential) error {
	if identityFile == "" || res.Mode() != sdk.AuthModeMTLS {
		return nil
	}
	now, err := config.IdentityFileKey(name, identityFile)
	if err != nil {
		return err
	}
	if now != entryKey {
		return entryUnreadable(name, errors.New("the key in its identity_file changed while this command ran"))
	}
	if refusal := config.IssuedCertificateKey(name, host, identityFile, now, []byte(res.Bundle.ClientCert)); refusal != nil {
		return refusal
	}
	return nil
}

// checkRowUnderLock runs decide against the row on disk under the server's
// credential lock and the config lock (the transaction's order), writing
// nothing. A refusal is returned as it is; any other failure is wrapped as an
// update that cannot go ahead.
func checkRowUnderLock(name string, decide func(*config.ClientConfig) error) error {
	unlock, err := config.LockServerCredentials(name)
	if err != nil {
		return err
	}
	defer unlock()
	if err := readClientConfigLocked(decide); err != nil {
		if _, refused := config.AsEntryRefused(err); refused {
			return err
		}
		return fmt.Errorf("cannot update server %q: %w", name, err)
	}
	return nil
}

// sameHeldIdentity reports whether two views of one entry hold it to the same
// user with the same SSH key: the fields a --refetch decides against. A
// concurrent `--allow-user-change` changes the first, a concurrent
// `--identity-file` the second; either makes this process's view stale, and
// writing it back would undo the other command.
func sameHeldIdentity(a, b config.ServerEntry) bool {
	return a.User == b.User && sameIdentityFile(a, b)
}

// sameIdentityFile is sameHeldIdentity's key half, which every enrolment's
// decision makes too (enrolledIdentityHolds): the two views name the same
// identity_file.
func sameIdentityFile(a, b config.ServerEntry) bool {
	return a.IdentityFile == b.IdentityFile
}

// ---------------------------------------------------------------------------
// shed server add
// ---------------------------------------------------------------------------

func runServerAdd(cmd *cobra.Command, args []string) error {
	host := normalizeAddHost(args[0])
	if host == "" {
		return fmt.Errorf("a host is required")
	}
	sshPort := serverAddSSHPort
	if sshPort < 1 || sshPort > 65535 {
		return fmt.Errorf("invalid --ssh-port %d", sshPort)
	}
	// The key this entry will use, checked before anything is pinned or
	// spawned: the bootstrap below already offers only it.
	identityFile, err := config.IdentityFileFromFlag(serverAddName, serverAddIdentityFile)
	if err != nil {
		return err
	}

	// A duplicate name is cheap to catch here and expensive to discover after a
	// bootstrap has already minted a credential. When --name is absent the name
	// comes from the server, so that check happens after the exchange.
	if serverAddName != "" {
		if _, exists := clientConfig.Servers[serverAddName]; exists {
			return fmt.Errorf("server '%s' already exists", serverAddName)
		}
	}

	// Step 1: pin the SSH host key. Everything after this runs over a channel
	// verified against that pin.
	pin, err := ensureHostKeyPinned(host, sshPort, serverAddFingerprint, serverAddTrustTOFU)
	if err != nil {
		pin.rollback()
		return err
	}
	if err := addAfterHostKey(host, sshPort, identityFile, pin); err != nil {
		// An add that did not finish leaves no trust decision behind.
		pin.rollback()
		return err
	}
	return nil
}

// addAfterHostKey is steps two and three of the add: ask the server for a
// credential, and act on what it said. identityFile is the entry's
// --identity-file ("" for none): the bootstrap offers only that key, and the
// entry stores it.
func addAfterHostKey(host string, sshPort int, identityFile string, pin pinnedHostKey) error {
	// The bootstrap always submits a CSR, so a token-mode server answers with a
	// token and an mtls-mode server answers with a signed certificate — the client
	// never has to know which in advance (see sdk/bootstrap.RunCredential).
	res, entryKey, err := bootstrapEntryCredential(serverAddName, host, sshPort, identityFile)
	if _, bad := asIdentityFileError(err); bad {
		// The key cannot be used (found by type, whichever layer found it):
		// never classified as a bootstrap outcome, so no words inside a path
		// can turn it into an unreachable or open server and a fallback.
		return err
	}
	switch classifyAddBootstrap(err) {
	case bootstrapIssued:
		return addFromBundle(host, sshPort, identityFile, entryKey, res)
	case bootstrapOpenServer:
		if !jsonFlag {
			fmt.Fprintf(os.Stderr, "%s issues no credentials over SSH (auth.mode: open); adding it over HTTP...\n", host)
		}
		return addOverHTTP(host, sshPort, identityFile)
	case bootstrapKeyNotAuthorized:
		return keysRefusedError(host, identityFile, err)
	case bootstrapUnreachable:
		if !pin.scanned() {
			// Neither our direct dial nor ssh itself could reach the SSH port, so
			// there is no SSH evidence about this server at all — not even that it
			// runs one. Falling back to the HTTP probe is what this command did
			// before first contact moved to SSH, and addOverHTTP refuses to write a
			// credential-less entry for a server that reports a credentialed mode,
			// so the fallback can only succeed where it was always correct.
			if !jsonFlag {
				fmt.Fprintf(os.Stderr, "the SSH port %s is unreachable (%v); trying HTTP...\n",
					scanAddr(host, sshPort), err)
			}
			return addOverHTTP(host, sshPort, identityFile)
		}
		return fmt.Errorf("could not complete the SSH bootstrap with %s: %w", scanAddr(host, sshPort), err)
	default:
		return fmt.Errorf("bootstrap over SSH to %s failed: %w%s", scanAddr(host, sshPort), err, unscannedHostKeyHint(pin))
	}
}

// unscannedHostKeyHint explains the most likely cause of a bootstrap failure
// against an endpoint we could not scan: ssh reached the server (through
// ~/.ssh/config, most likely) but found no pin for it in ~/.shed/known_hosts,
// which is the one thing this command normally puts there and could not.
func unscannedHostKeyHint(pin pinnedHostKey) string {
	if pin.scanned() {
		return ""
	}
	return fmt.Sprintf("\n(the host key could not be scanned directly, so nothing was pinned; "+
		"if ssh reaches this server through ~/.ssh/config, add its key to %s — "+
		"`ssh-keyscan -p <port> <hostname> >> %s` — and retry)",
		config.GetKnownHostsPath(), config.GetKnownHostsPath())
}

// addFromBundle writes the server entry described by a bootstrap bundle.
//
// The bundle is authenticated data — it arrived over the pinned SSH channel —
// so it, not a plain-HTTP probe, is what the entry is built from: the API
// endpoint, the TLS pin, and the credential shape all come from it. The entry
// is only persisted after an authenticated GET /api/info proves the resulting
// transport actually works, so a half-usable entry never lands in config.yaml.
func addFromBundle(host string, sshPort int, identityFile, entryKey string, res sdk.Credential) error {
	entry := config.ServerEntry{Host: host, SSHPort: sshPort, IdentityFile: identityFile}
	// The user the certificate is for becomes the user the entry is held to.
	// A certificate that names no user (a server older than 0.10.0), or whose
	// user cannot be read, is refused here, before anything is checked or
	// written. With an identity file the certificate must also have been made
	// for that key (entryKey), and a control token, which names no key, is
	// refused (plan 037 P-a).
	if err := checkIssuedForEntry(serverAddName, host, identityFile, entryKey, res); err != nil {
		return err
	}
	if res.Mode() == sdk.AuthModeMTLS {
		user, _, _ := config.IssuedCertificateUser(serverAddName, []byte(res.Bundle.ClientCert))
		entry.User = user // "" for the implicit user
	}
	cred, err := stampBundle(&entry, host, res, serverAddHTTPSPort)
	if err != nil {
		return err
	}

	// The key is learned again before the certificate is first presented, and
	// again under the locks before it is committed (issuedKeyHolds).
	if err := issuedKeyHolds(serverAddName, host, identityFile, entryKey, res); err != nil {
		return err
	}
	info, err := verifyServerEntry(&entry, cred)
	if err != nil {
		return err
	}
	warnSSHPortMismatch(info, sshPort)

	name := addedServerName(info, host)
	if _, exists := clientConfig.Servers[name]; exists {
		return fmt.Errorf("server '%s' already exists", name)
	}

	// And once more before a copy is written to staging: the network check
	// above waited on the server.
	if err := issuedKeyHolds(name, host, identityFile, entryKey, res); err != nil {
		return err
	}
	txn, err := stageCredentials(&entry, name, res, andKeyHolds(addDecide(name),
		func() error { return issuedKeyHolds(name, host, identityFile, entryKey, res) }))
	if err != nil {
		return err
	}
	txn.adding = true
	if err := txn.commitAround(func() error { return saveAddedServer(name, entry) }); err != nil {
		return err
	}
	return reportAddedServer(name, entry)
}

// addedServerName resolves the name the entry is filed under: --name wins, then
// the name the server reports, then the host as given.
func addedServerName(info *config.ServerInfo, host string) string {
	if serverAddName != "" {
		return serverAddName
	}
	if info != nil && info.Name != "" {
		return info.Name
	}
	return host
}

// stampBundle copies everything a bootstrap bundle says about a server onto an
// entry — endpoint, TLS pin, auth mode, credential — and returns the credential
// in the form a client presents.
//
// It touches no files: the mtls certificate is assembled in memory so the entry
// can be VERIFIED before anything is written to the credential store. Each mode
// clears the other's fields, which is what makes this reusable for
// `--refetch` on an entry whose server has since flipped modes.
//
// httpsPortOverride is an explicitly-passed --https-port: the port the operator
// says they reach this server's TLS listener on, which wins over the port the
// server reports for the api_url (a DNAT/port-mapped deployment). The PIN is
// never overridden — it is authenticated data from the bundle, and the
// verification GET that follows is what proves the combination works.
func stampBundle(entry *config.ServerEntry, host string, res sdk.Credential, httpsPortOverride int) (clienttoken.Credential, error) {
	b := res.Bundle
	// An operator-supplied --tls-fingerprint is verified against the pin the
	// server itself reported over the authenticated channel. It cannot make the
	// pin safer (it is already authenticated), but it catches an operator who
	// believes they are adding a different machine.
	if serverAddTLSFingerprint != "" && !tlsFingerprintEqual(b.TLSCertFingerprint, serverAddTLSFingerprint) {
		return clienttoken.Credential{}, fmt.Errorf(
			"TLS cert fingerprint mismatch: the server's bootstrap bundle pins %s, expected %s",
			b.TLSCertFingerprint, serverAddTLSFingerprint)
	}

	entry.APIURL = apiURLFromBundle(host, b, httpsPortOverride)
	entry.HTTPPort = b.HTTPPort
	entry.TLSCertFingerprint = b.TLSCertFingerprint
	entry.AuthMode = b.Mode()

	if res.Mode() == sdk.AuthModeMTLS {
		cert, err := tls.X509KeyPair([]byte(b.ClientCert), res.KeyPEM)
		if err != nil {
			return clienttoken.Credential{}, fmt.Errorf("assemble the issued client certificate: %w", err)
		}
		entry.ControlToken, entry.ControlTokenExpiresAt = "", time.Time{}
		entry.ClientCertExpiresAt = b.ExpiresAt
		return clienttoken.MTLSCredential(&cert, b.ExpiresAt), nil
	}
	entry.ControlToken = b.Token
	entry.ControlTokenExpiresAt = b.ExpiresAt
	entry.ClientCertFile, entry.ClientKeyFile = "", ""
	entry.ClientCertExpiresAt = time.Time{}
	return clienttoken.TokenCredential(b.Token, b.ExpiresAt), nil
}

// apiURLFromBundle builds the control-plane URL from the ports the server
// reported. HTTPS wins when both are present — it is the pinned endpoint, and
// in token/mtls mode it is the only one served. httpsPortOverride > 0 replaces
// the reported HTTPS port (see stampBundle).
func apiURLFromBundle(host string, b sdk.Bundle, httpsPortOverride int) string {
	httpsPort := b.HTTPSPort
	if httpsPortOverride > 0 {
		httpsPort = httpsPortOverride
	}
	if httpsPort > 0 {
		return "https://" + net.JoinHostPort(host, strconv.Itoa(httpsPort))
	}
	if b.HTTPPort > 0 {
		return "http://" + net.JoinHostPort(host, strconv.Itoa(b.HTTPPort))
	}
	return ""
}

// verifyServerEntry proves the entry works before it is persisted: one
// authenticated GET /api/info over exactly the transport the entry describes
// (pinned TLS, presenting the credential just issued).
//
// This is the step that turns "the server handed me something" into "I can talk
// to this server". A pin that does not match, an endpoint that is not listening,
// or a certificate the server will not accept fails HERE — with the bootstrap
// still fresh in the user's mind — instead of on their next command.
func verifyServerEntry(entry *config.ServerEntry, cred clienttoken.Credential) (*config.ServerInfo, error) {
	client := newAPIClientWithSource(entry.BaseURL(), entry.TLSCertFingerprint,
		clienttoken.New(cred, nil), DefaultTimeout)
	info, err := client.GetInfo()
	if err != nil {
		// The api_url comes from config.yaml (a refetch) or the command line,
		// so it is escaped as every other line that shows one.
		at := sdkbootstrap.SafeText(entry.BaseURL())
		var status *apiStatusError
		switch {
		case errors.As(err, &status):
			return nil, fmt.Errorf("bootstrapped a credential over SSH, but the server's API at %s answered HTTP %d: %w",
				at, status.Status, err)
		case endpointUnreached(err):
			return nil, fmt.Errorf("bootstrapped a credential over SSH, but the server's API at %s did not answer: %w",
				at, err)
		case clienttoken.IsAuthFailure(0, err):
			return nil, fmt.Errorf("bootstrapped a credential over SSH, but the server's API at %s refused it: %w",
				at, err)
		default:
			return nil, fmt.Errorf("bootstrapped a credential over SSH, but the server's API at %s could not be used: %w",
				at, err)
		}
	}
	return info, nil
}

// endpointUnreached reports whether a request to the server's API failed
// before the server answered it or proved who it is: the request was sent to
// the network (a *url.Error from the HTTP client, not one from parsing the
// URL) and produced no HTTP response, and the endpoint did not refuse the
// credential with a TLS alert. That is a dial failure (connection refused, a
// name that does not resolve), a timeout, or a TLS handshake that did not
// authenticate the pinned server (the pin did not match, or the peer hung up
// or does not speak TLS). It is decided by the error's type, never its text.
//
// Everything else is an answer from the endpoint: an HTTP status the request
// did not expect (*apiStatusError: 401, 403, a 3xx, a 5xx), a body that does
// not decode, or a TLS alert refusing the client certificate. The client
// presents a certificate only after the pin check passed, so that refusal
// comes from the server it pinned; the same alert from anything else is
// treated alike, which can only stop a refetch, never move one.
func endpointUnreached(err error) bool {
	var ue *url.Error
	if !errors.As(err, &ue) || ue.Op == "parse" {
		return false
	}
	var status *apiStatusError
	if errors.As(err, &status) {
		return false
	}
	return !clienttoken.IsAuthFailure(0, err)
}

// warnSSHPortMismatch notes a server whose reported ssh_port differs from the
// port whose host key we pinned. The pinned port wins (it is the one that was
// actually verified), but the mismatch usually means --ssh-port pointed at a
// proxy or the wrong daemon, and silence would make that very hard to see.
func warnSSHPortMismatch(info *config.ServerInfo, sshPort int) {
	if info == nil || info.SSHPort == 0 || info.SSHPort == sshPort || jsonFlag {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: the server reports ssh_port %d but the host key was pinned for port %d; keeping %d\n",
		info.SSHPort, sshPort, sshPort)
}

// addOverHTTP is the pre-SSH-first path, now reached only when the SSH exchange
// produced no credential: an `auth.mode: open` server (which issues none), or an
// SSH port that could not be reached at all. The host key is pinned by the time
// this runs if it could be scanned, so this only has to find the API endpoint
// (plain HTTP, or pinned TLS via --https-port/--secure).
func addOverHTTP(host string, sshPort int, identityFile string) error {
	_, apiURL, tlsFingerprint, info, err := selectAddTransport(host)
	if err != nil {
		return err
	}
	// The guard that makes this fallback safe: a server that reports a
	// credentialed mode issues its credential over SSH and nowhere else, so
	// writing an entry for it here would produce a config that cannot
	// authenticate — a failure the user would meet on their next command instead
	// of this one.
	if mode := info.AuthMode; mode == config.AuthModeToken || mode == config.AuthModeMTLS {
		return fmt.Errorf("%s reports auth.mode: %s, so its client credential is issued over SSH — "+
			"adding it over HTTP would write an entry with no credential. "+
			"Make its SSH port reachable (--ssh-port, default %d) and retry", host, mode, defaultAddSSHPort)
	}
	warnSSHPortMismatch(info, sshPort)

	name := addedServerName(info, host)
	if _, exists := clientConfig.Servers[name]; exists {
		return fmt.Errorf("server '%s' already exists", name)
	}

	entry := config.ServerEntry{
		Host:               host,
		HTTPPort:           info.HTTPPort,
		SSHPort:            sshPort,
		APIURL:             apiURL,         // empty for the plain-HTTP path
		TLSCertFingerprint: tlsFingerprint, // empty for the plain-HTTP path
		// An open server issues no credential, but console, exec, attach and
		// sync still reach its sheds over ssh, with this key.
		IdentityFile: identityFile,
		// AuthMode stays empty: an open server issues no credential, and
		// "absent means token" only matters once one exists.
	}
	// An open server has no credential to store, but a same-named predecessor may
	// have left one behind; the empty transaction clears it in the right order.
	txn := &credentialTxn{name: name, decide: addDecide(name), adding: true}
	if err := txn.commitAround(func() error { return saveAddedServer(name, entry) }); err != nil {
		return err
	}
	return reportAddedServer(name, entry)
}

// saveAddedServer commits the new entry to config.yaml through the locked
// update primitive, so the add merges into whatever a concurrent `shed` process
// has committed rather than renaming a stale whole-file snapshot over it.
//
// A failed add leaves the process's view of the config identical to the file's
// with no explicit rollback: Update mutates the in-memory config only AFTER the
// file write succeeds.
//
// ClientConfig.AddServer is deliberately not used: it stamps its own
// time.Now() (Update applies a mutation twice, so that writes two different
// instants to the file and to this process) and it can fail (an error on the
// second application would report a FAILED add for a row already durably on
// disk). The two halves are split instead — the duplicate check becomes a
// precondition evaluated ON THE FRESH SNAPSHOT under the lock, so two
// concurrent adds of one name cannot both see "free"; the mutation is a plain
// assignment of one fully-formed entry, stamped once, here.
func saveAddedServer(name string, entry config.ServerEntry) error {
	entry.AddedAt = time.Now()
	// The check runs exactly once (that is UpdateChecked's contract), so
	// capturing its verdict here is safe — unlike capturing from a mutation,
	// which runs twice.
	var duplicate error
	err := updateClientConfigChecked(
		func(c *config.ClientConfig) error {
			if _, exists := c.Servers[name]; exists {
				duplicate = fmt.Errorf("server '%s' already exists", name)
			}
			return duplicate
		},
		func(c *config.ClientConfig) {
			c.Servers[name] = entry
			// First server wins the default, per snapshot: if another process
			// has meanwhile set one on disk, that choice stands and only this
			// process's (defaultless) view adopts the new entry.
			if c.DefaultServer == "" {
				c.DefaultServer = name
			}
		})
	if err != nil {
		if duplicate != nil {
			return duplicate
		}
		return fmt.Errorf("failed to save config: %w", err)
	}
	return nil
}

// reportAddedServer prints what was added, in whichever output mode is active.
// It runs only after both halves of the transaction have committed, so what it
// describes is what is on disk: the user is worked out as `shed server list`
// works it out (entryUserOf), from the saved entry and its stored certificate,
// writing nothing.
func reportAddedServer(name string, entry config.ServerEntry) error {
	user := entryUserOf(name, entry)
	if jsonFlag {
		return outputJSON(ActionResult{
			Status: "ok",
			Action: "added",
			Name:   name,
			Details: struct {
				Host               string `json:"host"`
				HTTPPort           int    `json:"http_port,omitempty"`
				SSHPort            int    `json:"ssh_port"`
				APIURL             string `json:"api_url,omitempty"`
				TLSCertFingerprint string `json:"tls_cert_fingerprint,omitempty"`
				AuthMode           string `json:"auth_mode,omitempty"`
				// As in `shed server list --json`: the user name the
				// entry records (omitted when none), and whether its
				// certificate is the implicit user's. Never `(implicit)`
				// or `-`, which are text only.
				User         string `json:"user,omitempty"`
				UserImplicit bool   `json:"user_implicit"`
				IdentityFile string `json:"identity_file,omitempty"`
				Default      bool   `json:"default"`
			}{
				Host:               entry.Host,
				HTTPPort:           entry.HTTPPort,
				SSHPort:            entry.SSHPort,
				APIURL:             entry.APIURL,
				TLSCertFingerprint: entry.TLSCertFingerprint,
				AuthMode:           entry.AuthMode,
				User:               user.name,
				UserImplicit:       user.implicit,
				IdentityFile:       entry.IdentityFile,
				Default:            clientConfig.DefaultServer == name,
			},
		})
	}

	if entry.APIURL != "" {
		if entry.TLSCertFingerprint != "" {
			printSuccess("Added server %s (%s, TLS pinned)", name, entry.APIURL)
		} else {
			printSuccess("Added server %s (%s)", name, entry.APIURL)
		}
	} else {
		printSuccess("Added server %s (%s)", name, entry.BaseURL())
	}
	switch {
	case entry.IsMTLS():
		fmt.Println("  Enrolled a client certificate over SSH (mtls mode)")
		// Who the entry is: the user it is held to, or (implicit) for a
		// server with no user list. Check it is the user you expect.
		fmt.Printf("  User: %s\n", user.label())
	case entry.ControlToken != "":
		fmt.Println("  Bootstrapped a control token over SSH (token mode)")
	}
	if entry.IdentityFile != "" {
		printIdentityFileLine(entry.IdentityFile)
	}
	if clientConfig.DefaultServer == name {
		fmt.Println("  Set as default server")
	}
	return nil
}

// ---------------------------------------------------------------------------
// shed server update --refetch
// ---------------------------------------------------------------------------

// refetchServerCredential re-pins a server over the same SSH-first path `shed
// server add` uses: scan + verify the host key, bootstrap, and take the TLS pin
// from the bundle rather than from an unauthenticated TLS dial.
//
// Going through the bootstrap (rather than re-reading the cert off the HTTPS
// listener) is what makes --refetch work at all in mtls mode, where an
// unenrolled TLS dial cannot complete a handshake. It also re-enrolls the
// credential as a side effect, so an entry whose server changed mode since it
// was added lands on the right one.
//
// identityFile is the entry's identity_file after the command: its own, or the
// value of --identity-file. The bootstrap offers only that key, and the saved
// row records it; nothing is saved unless the whole re-enrolment succeeds.
func refetchServerCredential(name string, entry *config.ServerEntry, identityFile string) error {
	// Checked before anything is pinned or spawned (a key removed since it was
	// set fails here, with its own message).
	if _, err := config.IdentityFileArgs(name, identityFile); err != nil {
		return err
	}
	if entry.SSHPort < 1 || entry.SSHPort > 65535 {
		// An entry with no usable SSH endpoint (hand-edited, or written by a very
		// old client) can still have its pin re-read the legacy way.
		return refetchTLSPinOverHTTPS(name, entry, identityFile)
	}
	pin, err := ensureHostKeyPinned(entry.Host, entry.SSHPort, "", serverUpdateTrustTOFU)
	if err != nil {
		pin.rollback()
		return err
	}
	if err := refetchAfterHostKey(name, entry, identityFile, pin); err != nil {
		pin.rollback()
		return err
	}
	return nil
}

func refetchAfterHostKey(name string, entry *config.ServerEntry, identityFile string, pin pinnedHostKey) error {
	res, entryKey, err := bootstrapEntryCredential(name, entry.Host, entry.SSHPort, identityFile)
	if _, bad := asIdentityFileError(err); bad {
		return err // the key cannot be used (by type, from any layer): never a bootstrap outcome
	}
	switch classifyAddBootstrap(err) {
	case bootstrapIssued:
	case bootstrapOpenServer:
		// An open server has no bundle to read a pin from; the legacy TLS
		// re-fetch is the only thing that can help it.
		return refetchTLSPinOverHTTPS(name, entry, identityFile)
	case bootstrapKeyNotAuthorized:
		return keysRefusedError(entry.Host, identityFile, err)
	case bootstrapUnreachable:
		return fmt.Errorf("could not complete the SSH bootstrap with %s: %w%s",
			scanAddr(entry.Host, entry.SSHPort), err, unscannedHostKeyHint(pin))
	default:
		return fmt.Errorf("bootstrap over SSH to %s failed: %w%s",
			scanAddr(entry.Host, entry.SSHPort), err, unscannedHostKeyHint(pin))
	}

	// The user the new certificate is for, compared with the user the entry is
	// held to right after the bootstrap: a change is refused here, before the
	// TLS pin is confirmed, before the network check and before anything is
	// written, unless --allow-user-change asks for it. The transaction compares
	// again, under the lock, against the row on disk (refetchDecide).
	// An implicit certificate (a server with no user list) neither sets nor
	// clears the user. With an identity file the certificate must have been
	// made for that key, and a token is refused (checkIssuedForEntry).
	if err := checkIssuedForEntry(name, entry.Host, identityFile, entryKey, res); err != nil {
		return err
	}
	newUser := entry.User
	if res.Mode() == sdk.AuthModeMTLS {
		user, implicit, _ := config.IssuedCertificateUser(name, []byte(res.Bundle.ClientCert))
		if !serverUpdateAllowUserChange && !config.HeldUserAccepts(entry.User, user, implicit) {
			return &config.EntryRefusedError{Entry: name, Kind: config.RefusedIssuedUserChange,
				CertUser: user, EntryUser: entry.User}
		}
		if !implicit {
			newUser = user
		}
	}
	// A refusal that is already knowable costs no network round trip: the row
	// on disk is checked now, under the locks, as well as at the commit (where
	// it is checked again, since the network check below takes time). Another
	// process that changed the entry's user while this bootstrap ran is found
	// here, before the TLS pin, the network check or any staging.
	// Each decision also learns the identity file's key again (issuedKeyHolds):
	// a key replaced at its path while the bootstrap ran is refused before the
	// certificate is presented, staged or committed.
	keyHolds := func() error { return issuedKeyHolds(name, entry.Host, identityFile, entryKey, res) }
	decide := andKeyHolds(refetchDecide(name, *entry), keyHolds)
	if err := checkRowUnderLock(name, decide); err != nil {
		return err
	}

	newPin := res.Bundle.TLSCertFingerprint
	unchanged := tlsFingerprintEqual(newPin, entry.TLSCertFingerprint)
	if !unchanged {
		// firstUse only when there is no prior pin; rotating an existing one must
		// not be silently accepted in a non-interactive session.
		if err := confirmTLSCert(newPin, "", serverUpdateTrustTOFU, isStdinTTY(), jsonFlag, entry.TLSCertFingerprint == ""); err != nil {
			return err
		}
	}

	updated := *entry
	// stampBundle builds the api_url from the port the server REPORTS: it has
	// no override here, because `shed server update` has no --https-port. The
	// endpoint the entry already has (an add's --https-port, for a server
	// reached through a port mapping) is kept, when it still answers, by
	// verifyRefetchedEndpoint below, never by stampBundle.
	cred, err := stampBundle(&updated, updated.Host, res, 0)
	if err != nil {
		return err
	}
	updated.User = newUser
	updated.IdentityFile = identityFile
	// Learned again here: a TLS pin confirmation above may have waited on a
	// person. verifyRefetchedEndpoint learns it again before each further
	// presentation.
	if err := keyHolds(); err != nil {
		return err
	}
	previousAPIURL, err := verifyRefetchedEndpoint(&updated, entry.APIURL, cred, keyHolds)
	if err != nil {
		return err
	}
	// And once more before a copy is written to staging: the network check
	// above waited on the server.
	if err := keyHolds(); err != nil {
		return err
	}
	txn, err := stageCredentials(&updated, name, res, decide)
	if err != nil {
		return err
	}
	if err := txn.commitAround(func() error { return saveUpdatedServer(name, *entry, updated) }); err != nil {
		return err
	}

	previousUser := ""
	if entry.User != updated.User {
		previousUser = entry.User
	}
	if jsonFlag {
		return outputJSON(refetchResult{Status: "ok", Action: "updated", Name: name,
			User: updated.User, PreviousUser: previousUser, IdentityFile: updated.IdentityFile,
			APIURL: updated.APIURL, PreviousAPIURL: previousAPIURL})
	}
	if unchanged {
		printSuccess("Server %s TLS pin unchanged: %s", name, updated.TLSCertFingerprint)
	} else {
		printSuccess("Updated server %s TLS pin: %s", name, updated.TLSCertFingerprint)
	}
	if updated.IsMTLS() {
		fmt.Println("  Re-enrolled a client certificate over SSH (mtls mode)")
	}
	if previousAPIURL != "" {
		fmt.Println(movedEndpointLine(previousAPIURL, updated.APIURL))
	}
	switch {
	case previousUser != "":
		fmt.Printf("  User changed from %q to %q\n", previousUser, updated.User)
	case updated.User != "":
		fmt.Printf("  User: %s\n", updated.User)
	}
	if updated.IdentityFile != entry.IdentityFile {
		printIdentityFileLine(updated.IdentityFile)
	}
	return nil
}

// refetchResult is the JSON result of `shed server update --refetch`: the
// ActionResult members, the user the entry is held to after it, and, when the
// refetch changed it (--allow-user-change), the user before.
type refetchResult struct {
	Status       string `json:"status"`
	Action       string `json:"action"`
	Name         string `json:"name"`
	User         string `json:"user,omitempty"`
	PreviousUser string `json:"previous_user,omitempty"`
	// IdentityFile is the entry's identity_file after the refetch (omitted
	// when it has none).
	IdentityFile string `json:"identity_file,omitempty"`
	// APIURL is the entry's api_url after the refetch, and PreviousAPIURL,
	// when the refetch moved it (the entry's own endpoint did not answer and
	// the port the server reports did: verifyRefetchedEndpoint), the one
	// before.
	APIURL         string `json:"api_url,omitempty"`
	PreviousAPIURL string `json:"previous_api_url,omitempty"`
}

// verifyRefetchedEndpoint is a refetch's network check, and decides which
// endpoint the re-stamped entry keeps. updated has been stamped from the
// bundle (stampBundle): its api_url is the one the server REPORTS. stored is
// the api_url the entry had before the refetch.
//
//  1. An entry whose stored api_url is https with an explicit port keeps it
//     when it still answers: the new credential and the bundle's pin are
//     verified there first, exactly as verifyServerEntry does. This keeps an
//     add's --https-port (a server reached through a port mapping) across a
//     refetch.
//  2. Only when it could not be reached or did not prove it is the server (a
//     dial failure, a timeout, a TLS handshake the pin did not pass:
//     endpointUnreached), and differs from the endpoint the server reports,
//     is the reported one tried the same way; if that one verifies, the
//     entry moves to it, and the stored api_url is returned as previous (the
//     command says so). This heals a server that really changed its port.
//     An ANSWER from the stored endpoint (a 401, a 403, a 3xx, a 5xx, a body
//     that does not decode, a TLS refusal of the certificate) is no sign of a
//     moved port: the refetch is refused there, naming that endpoint and what
//     it answered, the reported endpoint is not tried, and the caller writes
//     nothing.
//  3. When neither verifies, the error names both, and the caller writes
//     nothing.
//  4. An entry whose api_url is plain http or has no port, or a bundle that
//     reports no pinned HTTPS endpoint, is checked at the reported endpoint
//     alone, as before.
//
// Why the first attempt is safe: it sends the new credential only to the
// endpoint the entry already sent its credential to, and only over TLS pinned
// to the fingerprint the server reported over the authenticated SSH channel.
// A listener at that endpoint that is not this server cannot complete the
// handshake: the pin is checked before the client presents its certificate,
// and a bearer token is sent only after the handshake. The second attempt is
// the endpoint a refetch has always used.
//
// keyHolds is the refetch's identity-file check (issuedKeyHolds), run again
// before the second presentation, since the first may have waited on the
// network; the caller runs it before the first.
func verifyRefetchedEndpoint(updated *config.ServerEntry, stored string, cred clienttoken.Credential,
	keyHolds func() error) (previous string, err error) {
	reported := updated.APIURL
	kept, ok := keptRefetchEndpoint(stored, reported, updated.TLSCertFingerprint)
	if !ok {
		_, err := verifyServerEntry(updated, cred)
		return "", err
	}
	updated.APIURL = kept
	_, keptErr := verifyServerEntry(updated, cred)
	if keptErr == nil {
		return "", nil
	}
	if sameEndpoint(kept, reported) {
		return "", keptErr
	}
	if !endpointUnreached(keptErr) {
		return "", fmt.Errorf("%w; nothing was saved (an answer is no sign of a moved port, so the endpoint the server reports, %s, was not tried)",
			keptErr, sdkbootstrap.SafeText(reported))
	}
	if err := keyHolds(); err != nil {
		return "", err
	}
	updated.APIURL = reported
	if _, reportedErr := verifyServerEntry(updated, cred); reportedErr != nil {
		updated.APIURL = kept
		return "", fmt.Errorf("bootstrapped a credential over SSH, but the server's API answered at neither "+
			"the entry's endpoint %s (%v) nor the endpoint the server reports, %s (%v); nothing was saved",
			sdkbootstrap.SafeText(strings.TrimRight(kept, "/")), apiErrorCause(keptErr),
			sdkbootstrap.SafeText(reported), apiErrorCause(reportedErr))
	}
	return kept, nil
}

// keptRefetchEndpoint returns the stored api_url a refetch tries first
// (verifyRefetchedEndpoint rule 1): stored, when it is https with an explicit
// port and the bundle reports a pinned https endpoint. ok is false otherwise
// (rule 4).
func keptRefetchEndpoint(stored, reported, pin string) (string, bool) {
	if !isHTTPSURL(stored) || !isHTTPSURL(reported) || strings.TrimSpace(pin) == "" {
		return "", false
	}
	u, err := url.Parse(strings.TrimSpace(stored))
	if err != nil || u.Hostname() == "" || u.Port() == "" {
		return "", false
	}
	return stored, true
}

// sameEndpoint reports whether two https api_urls name the same host and port.
func sameEndpoint(a, b string) bool {
	ua, errA := url.Parse(strings.TrimSpace(a))
	ub, errB := url.Parse(strings.TrimSpace(b))
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(ua.Hostname(), ub.Hostname()) && ua.Port() == ub.Port()
}

// apiErrorCause is verifyServerEntry's error without its leading sentence, so
// a refusal that names two endpoints says each once.
func apiErrorCause(err error) error {
	if inner := errors.Unwrap(err); inner != nil {
		return inner
	}
	return err
}

// movedEndpointLine is the text a refetch prints when it moved the entry to
// the port the server reports (verifyRefetchedEndpoint rule 2). Each api_url
// comes from config.yaml or is built from its host, so it is escaped as the
// other lines that show one are.
func movedEndpointLine(previous, current string) string {
	return fmt.Sprintf("  HTTPS port changed from %s to %s: %s did not answer, and the server reports %s",
		sdkbootstrap.SafeText(portOfURL(previous)), sdkbootstrap.SafeText(portOfURL(current)),
		sdkbootstrap.SafeText(strings.TrimRight(previous, "/")), sdkbootstrap.SafeText(current))
}

// portOfURL is the port an api_url names ("" when it names none).
func portOfURL(apiURL string) string {
	u, err := url.Parse(strings.TrimSpace(apiURL))
	if err != nil {
		return ""
	}
	return u.Port()
}

// saveUpdatedServer writes a modified entry back to config.yaml through the
// locked update primitive. No explicit in-memory rollback is needed: Update
// applies the mutation in memory only after the file write has succeeded, so a
// failure leaves this process's view exactly as the file's.
//
// was is the entry as it stood when this update was decided, and it is checked
// against the FRESH on-disk row under the lock — the same guard the credential
// persist uses, for the same reason. A `--refetch` is an SSH round-trip; a
// `shed server rm` or a re-add pointing the name somewhere else can land inside
// it, and writing `updated` regardless would resurrect the removed entry or
// staple this endpoint's pin and certificate onto a different server.
//
// The same check also refuses a row whose user changed since was was read
// (refetchDecide): writing `updated` would undo another process's
// --allow-user-change.
func saveUpdatedServer(name string, was, updated config.ServerEntry) error {
	var gone error
	decide := refetchDecide(name, was)
	err := updateClientConfigChecked(
		func(c *config.ClientConfig) error {
			gone = decide(c)
			return gone
		},
		func(c *config.ClientConfig) {
			if _, ok := c.Servers[name]; !ok {
				return // in-memory only; see saveServerEntry
			}
			c.Servers[name] = updated
		})
	if err != nil {
		if _, refused := config.AsEntryRefused(gone); refused {
			return gone
		}
		if gone != nil {
			return fmt.Errorf("cannot update server %q: %w", name, gone)
		}
		return fmt.Errorf("failed to save config: %w", err)
	}
	return nil
}

// refetchTLSPinOverHTTPS is the legacy re-pin: read the certificate off the
// entry's https api_url and re-pin it. It survives for `auth.mode: open`
// servers with TLS in front of them, which have no bootstrap bundle to carry a
// pin — and it is the reason the "no https api_url" guard still exists.
//
// identityFile is the entry's identity_file after the command (see
// refetchServerCredential): a changed one is saved with the pin, or alone when
// the pin is unchanged.
func refetchTLSPinOverHTTPS(name string, entry *config.ServerEntry, identityFile string) error {
	if !isHTTPSURL(entry.APIURL) {
		return fmt.Errorf("server %q has no https api_url; a TLS pin only applies over https — re-add it with --https-port", name)
	}
	host, port, err := hostPortFromAPIURL(entry.APIURL)
	if err != nil {
		return err
	}
	fp, err := fetchTLSCertFingerprint(host, port)
	if err != nil {
		return err
	}
	identityChanged := identityFile != entry.IdentityFile
	pinChanged := fp != entry.TLSCertFingerprint
	if !pinChanged && !identityChanged {
		if !jsonFlag {
			printSuccess("Server %s TLS pin unchanged: %s", name, fp)
		}
		return nil
	}
	if pinChanged {
		if err := confirmTLSCert(fp, "", serverUpdateTrustTOFU, isStdinTTY(), jsonFlag, entry.TLSCertFingerprint == ""); err != nil {
			return err
		}
	}
	updated := *entry
	updated.TLSCertFingerprint = fp
	updated.IdentityFile = identityFile
	if err := saveUpdatedServer(name, *entry, updated); err != nil {
		return err
	}
	if jsonFlag {
		// The same result as the SSH-first refetch's (refetchResult).
		return outputJSON(refetchResult{Status: "ok", Action: "updated", Name: name,
			User: updated.User, IdentityFile: updated.IdentityFile, APIURL: updated.APIURL})
	}
	if pinChanged {
		printSuccess("Updated server %s TLS pin: %s", name, fp)
	} else {
		printSuccess("Server %s TLS pin unchanged: %s", name, fp)
	}
	if identityChanged {
		printIdentityFileLine(identityFile)
	}
	return nil
}

// asIdentityFileError reports whether err is an identity_file that cannot be
// used (config.IdentityFileError, which bootstrapEntryCredential makes of the
// SDK's own): returned as it is, never classified as a bootstrap outcome.
func asIdentityFileError(err error) (*config.IdentityFileError, bool) {
	var ie *config.IdentityFileError
	if errors.As(err, &ie) {
		return ie, true
	}
	return nil, false
}

// checkIssuedForEntry is the add's and the refetch's check of what the
// bootstrap issued, before anything is checked over the network or written:
// a certificate must name a user (config.IssuedCertificateUser) and, for an
// entry with an identity file, must have been made for that key (entryKey,
// config.IssuedCertificateKey); and such an entry never takes a control
// token, which names no key (plan 037 P-a). name, host and identityFile name
// things in the refusal.
func checkIssuedForEntry(name, host, identityFile, entryKey string, res sdk.Credential) error {
	if res.Mode() != sdk.AuthModeMTLS {
		if identityFile != "" {
			return &config.EntryRefusedError{Entry: name, Kind: config.RefusedIssuedTokenForKey, IdentityFile: identityFile}
		}
		return nil
	}
	if _, _, refusal := config.IssuedCertificateUser(name, []byte(res.Bundle.ClientCert)); refusal != nil {
		return refusal
	}
	if refusal := config.IssuedCertificateKey(name, host, identityFile, entryKey, []byte(res.Bundle.ClientCert)); refusal != nil {
		return refusal
	}
	return nil
}
