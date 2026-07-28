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
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
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
// The rule that gets there is one line long: the DESTRUCTIVE step runs after the
// save. Adopting new material is destructive only when it overwrites or deletes
// material the currently-saved config still points at; when it does not (a fresh
// add, a token→mtls flip), it can run first, and then the save happens with the
// files already in place and there is no window at all. See commitAround.
type credentialTxn struct {
	name string
	// staged is the newly issued pair, written but not yet adopted. nil in
	// token/open mode, where there is no material to write — only, possibly, some
	// to remove.
	staged *config.StagedClientCredentials
	// replacing reports whether committing would overwrite or delete credential
	// material the currently-saved config entry still points at. It is the only
	// input to the ordering decision.
	replacing bool
	// certPEM is the certificate this transaction staged, kept so the commit can
	// prove — under the server's credential lock — that what ended up on disk is
	// still THIS enrollment's material and not a concurrent one's.
	certPEM []byte
	// prevCert / prevKey are the material the entry had before this transaction,
	// read before anything was overwritten, so a failed save on the replacing
	// path can put it back.
	prevCert, prevKey []byte
}

// stageCredentials writes the entry's new credential material somewhere
// harmless and points the entry at the paths it will occupy once committed.
//
// Nothing the server currently has is touched here, which is the point: a failed
// write (a full disk, a bad permission, an unwritable creds root) has to be
// survivable, and the way to make it survivable is to discover it before
// anything of value has been disturbed.
//
// prev is the entry as currently SAVED (nil for an add, which by definition has
// no predecessor); it is what decides whether the commit is destructive.
func stageCredentials(entry *config.ServerEntry, name string, res sdk.Credential, prev *config.ServerEntry) (*credentialTxn, error) {
	txn := &credentialTxn{
		name:      name,
		replacing: prev != nil && (prev.ClientCertFile != "" || prev.ClientKeyFile != ""),
	}
	if res.Mode() != sdk.AuthModeMTLS {
		entry.ClientCertFile, entry.ClientKeyFile = "", ""
		return txn, nil
	}
	staged, err := stageCredentialsFn(name, []byte(res.Bundle.ClientCert), res.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("could not store the issued client certificate: %w", err)
	}
	txn.staged = staged
	txn.certPEM = []byte(res.Bundle.ClientCert)
	if prev != nil {
		// Best effort, and read BEFORE the commit overwrites anything: it is
		// only used to undo a commit whose save then failed.
		txn.prevCert, _ = os.ReadFile(prev.ClientCertFile)
		txn.prevKey, _ = os.ReadFile(prev.ClientKeyFile)
	}
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
// LOCK ORDERING: credential lock first, config lock underneath (inside save) —
// the order pinned in cmd/shed/client.go, and never the reverse anywhere.
//
// The mtls path cannot hold ONE lock across both halves, because
// StagedClientCredentials.Commit takes the same per-server lock itself
// (sdk/creds) and flock is not re-entrant — taking it here first would deadlock
// the process against itself. The equivalent guarantee is bought differently:
// commit, then lock, then PROVE the committed material is still ours, then
// save. A loser of the file race finds someone else's certificate under the
// lock and aborts without writing a config row, so no row is ever saved beside
// another enrollment's material.
func (t *credentialTxn) commitAround(save func() error) error {
	if t.staged == nil {
		// Token/open mode: the "commit" is a directory removal, which takes no
		// lock of its own, so the whole transaction fits under a single hold.
		// The removal runs AFTER the save in both cases — it is destructive
		// whenever there is anything to destroy, and ordering it last costs
		// nothing when there is not.
		unlock, err := config.LockServerCredentials(t.name)
		if err != nil {
			return err
		}
		defer unlock()
		if err := save(); err != nil {
			return err
		}
		return t.commit()
	}

	if err := t.commit(); err != nil {
		t.discard()
		return err
	}

	unlock, err := config.LockServerCredentials(t.name)
	if err != nil {
		t.rollback()
		return err
	}
	ownsMaterial := t.committedMaterialIsOurs()
	var saveErr error
	if ownsMaterial == nil {
		saveErr = save()
	}
	// Released BEFORE unwinding: restorePrevious writes through the credential
	// store, which takes this very lock.
	unlock()

	if ownsMaterial != nil {
		// Another enrollment for this name won the race and its material is
		// what is on disk. Ours is superseded, not lost data — the caller
		// re-runs and gets a coherent pair.
		return ownsMaterial
	}
	if saveErr != nil {
		t.restorePrevious()
		return saveErr
	}
	return nil
}

// committedMaterialIsOurs reports whether the certificate now at the entry's
// path is the one this transaction committed. It must be called under the
// server's credential lock, which is what makes the answer stay true through
// the config save that follows.
func (t *credentialTxn) committedMaterialIsOurs() error {
	certPath, _ := t.staged.Paths()
	onDisk, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("the client certificate stored for %q went away before it could be recorded "+
			"(another process is enrolling the same server name): %w", t.name, err)
	}
	if !bytes.Equal(bytes.TrimSpace(onDisk), bytes.TrimSpace(t.certPEM)) {
		return fmt.Errorf("another process enrolled %q at the same time and its client certificate is the one on disk; "+
			"this add was abandoned rather than record an entry describing a certificate that is not there", t.name)
	}
	return nil
}

// restorePrevious undoes a commit whose save then failed.
//
// On a fresh add there is nothing to restore, so the material this command
// created is removed (that is exactly the old rollback). On a replace, the
// previous pair — captured before the commit overwrote it — is written back, so
// the saved config still names material that exists and matches it. That is the
// invariant the old save-then-commit ordering bought on the replacing path, now
// bought by an undo instead, because the commit has to precede the save for the
// ownership check to mean anything.
func (t *credentialTxn) restorePrevious() {
	if len(t.prevCert) > 0 && len(t.prevKey) > 0 {
		if _, _, err := config.WriteClientCredentials(t.name, t.prevCert, t.prevKey); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not restore the previous client certificate for %q: %v\n", t.name, err)
		}
		return
	}
	t.rollback()
}

// commit adopts the staged material — or, in token/open mode, removes the
// credential directory the entry no longer references.
//
// The removal is a warning rather than an error: by the time it runs the config
// is already correct, and a leftover file that nothing points at must not fail a
// command that otherwise succeeded. (`shed server rm` takes the same position,
// for the same reason.)
func (t *credentialTxn) commit() error {
	if t.staged != nil {
		return t.staged.Commit()
	}
	if err := config.RemoveServerCredentials(t.name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	return nil
}

// discard throws the staged material away, leaving everything else untouched.
func (t *credentialTxn) discard() {
	t.staged.Discard()
}

// rollback undoes a commit that ran BEFORE a save that then failed. It is only
// reachable on the non-replacing path, where the material it removes is
// material this command created in a location the saved config never named — so
// removing it restores exactly the prior state.
func (t *credentialTxn) rollback() {
	t.discard()
	if t.staged == nil {
		return
	}
	if err := config.RemoveServerCredentials(t.name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
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
	if err := addAfterHostKey(host, sshPort, pin); err != nil {
		// An add that did not finish leaves no trust decision behind.
		pin.rollback()
		return err
	}
	return nil
}

// addAfterHostKey is steps two and three of the add: ask the server for a
// credential, and act on what it said.
func addAfterHostKey(host string, sshPort int, pin pinnedHostKey) error {
	// The bootstrap always submits a CSR, so a token-mode server answers with a
	// token and an mtls-mode server answers with a signed certificate — the client
	// never has to know which in advance (see sdk/bootstrap.RunCredential).
	res, err := bootstrapCredentialFn(host, sshPort, "control", "cli")
	switch classifyAddBootstrap(err) {
	case bootstrapIssued:
		return addFromBundle(host, sshPort, res)
	case bootstrapOpenServer:
		if !jsonFlag {
			fmt.Fprintf(os.Stderr, "%s issues no credentials over SSH (auth.mode: open); adding it over HTTP...\n", host)
		}
		return addOverHTTP(host, sshPort)
	case bootstrapKeyNotAuthorized:
		return fmt.Errorf("your SSH key is not in this server's allowlist "+
			"(auth.ssh.github_users / authorized_keys) — add it on %s, then retry: %w", host, err)
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
			return addOverHTTP(host, sshPort)
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
func addFromBundle(host string, sshPort int, res sdk.Credential) error {
	entry := config.ServerEntry{Host: host, SSHPort: sshPort}
	cred, err := stampBundle(&entry, host, res, serverAddHTTPSPort)
	if err != nil {
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

	txn, err := stageCredentials(&entry, name, res, nil)
	if err != nil {
		return err
	}
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
		return nil, fmt.Errorf("bootstrapped a credential over SSH, but the server's API at %s did not answer: %w",
			entry.BaseURL(), err)
	}
	return info, nil
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
func addOverHTTP(host string, sshPort int) error {
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
		// AuthMode stays empty: an open server issues no credential, and
		// "absent means token" only matters once one exists.
	}
	// An open server has no credential to store, but a same-named predecessor may
	// have left one behind; the empty transaction clears it in the right order.
	txn := &credentialTxn{name: name}
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
// describes is what is on disk.
func reportAddedServer(name string, entry config.ServerEntry) error {
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
				Default            bool   `json:"default"`
			}{
				Host:               entry.Host,
				HTTPPort:           entry.HTTPPort,
				SSHPort:            entry.SSHPort,
				APIURL:             entry.APIURL,
				TLSCertFingerprint: entry.TLSCertFingerprint,
				AuthMode:           entry.AuthMode,
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
	case entry.ControlToken != "":
		fmt.Println("  Bootstrapped a control token over SSH (token mode)")
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
func refetchServerCredential(name string, entry *config.ServerEntry) error {
	if entry.SSHPort < 1 || entry.SSHPort > 65535 {
		// An entry with no usable SSH endpoint (hand-edited, or written by a very
		// old client) can still have its pin re-read the legacy way.
		return refetchTLSPinOverHTTPS(name, entry)
	}
	pin, err := ensureHostKeyPinned(entry.Host, entry.SSHPort, "", serverUpdateTrustTOFU)
	if err != nil {
		pin.rollback()
		return err
	}
	if err := refetchAfterHostKey(name, entry, pin); err != nil {
		pin.rollback()
		return err
	}
	return nil
}

func refetchAfterHostKey(name string, entry *config.ServerEntry, pin pinnedHostKey) error {
	res, err := bootstrapCredentialFn(entry.Host, entry.SSHPort, "control", "cli")
	switch classifyAddBootstrap(err) {
	case bootstrapIssued:
	case bootstrapOpenServer:
		// An open server has no bundle to read a pin from; the legacy TLS
		// re-fetch is the only thing that can help it.
		return refetchTLSPinOverHTTPS(name, entry)
	case bootstrapKeyNotAuthorized:
		return fmt.Errorf("your SSH key is not in this server's allowlist "+
			"(auth.ssh.github_users / authorized_keys) — add it on %s, then retry: %w", entry.Host, err)
	case bootstrapUnreachable:
		return fmt.Errorf("could not complete the SSH bootstrap with %s: %w%s",
			scanAddr(entry.Host, entry.SSHPort), err, unscannedHostKeyHint(pin))
	default:
		return fmt.Errorf("bootstrap over SSH to %s failed: %w%s",
			scanAddr(entry.Host, entry.SSHPort), err, unscannedHostKeyHint(pin))
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
	// No --https-port override on this path: `shed server update` has no such
	// flag, and an entry being re-pinned already carries whatever endpoint the
	// operator chose when they added it.
	cred, err := stampBundle(&updated, updated.Host, res, 0)
	if err != nil {
		return err
	}
	if _, err := verifyServerEntry(&updated, cred); err != nil {
		return err
	}
	txn, err := stageCredentials(&updated, name, res, entry)
	if err != nil {
		return err
	}
	if err := txn.commitAround(func() error { return saveUpdatedServer(name, *entry, updated) }); err != nil {
		return err
	}

	if jsonFlag {
		return outputJSON(ActionResult{Status: "ok", Action: "updated", Name: name})
	}
	if unchanged {
		printSuccess("Server %s TLS pin unchanged: %s", name, updated.TLSCertFingerprint)
	} else {
		printSuccess("Updated server %s TLS pin: %s", name, updated.TLSCertFingerprint)
	}
	if updated.IsMTLS() {
		fmt.Println("  Re-enrolled a client certificate over SSH (mtls mode)")
	}
	return nil
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
func saveUpdatedServer(name string, was, updated config.ServerEntry) error {
	var gone error
	err := updateClientConfigChecked(
		func(c *config.ClientConfig) error {
			gone = checkStillNames(c, name, was)
			return gone
		},
		func(c *config.ClientConfig) {
			if _, ok := c.Servers[name]; !ok {
				return // in-memory only; see saveServerEntry
			}
			c.Servers[name] = updated
		})
	if err != nil {
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
func refetchTLSPinOverHTTPS(name string, entry *config.ServerEntry) error {
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
	if fp == entry.TLSCertFingerprint {
		if !jsonFlag {
			printSuccess("Server %s TLS pin unchanged: %s", name, fp)
		}
		return nil
	}
	if err := confirmTLSCert(fp, "", serverUpdateTrustTOFU, isStdinTTY(), jsonFlag, entry.TLSCertFingerprint == ""); err != nil {
		return err
	}
	updated := *entry
	updated.TLSCertFingerprint = fp
	if err := saveUpdatedServer(name, *entry, updated); err != nil {
		return err
	}
	if jsonFlag {
		return outputJSON(ActionResult{Status: "ok", Action: "updated", Name: name})
	}
	printSuccess("Updated server %s TLS pin: %s", name, fp)
	return nil
}
