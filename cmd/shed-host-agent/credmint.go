package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	sdk "github.com/charliek/shed/sdk"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// CredentialMinter bootstraps the host-agent's own credentials-scoped token over
// a server's SSH _bootstrap channel by invoking the system ssh client (via
// sdk/bootstrap). The agent authenticates with a key already on the server's
// allowlist (the same key `shed server add` used) — resolved by ssh from the
// agent, macOS Keychain, 1Password/Secretive IdentityAgent, hardware keys, or
// ~/.ssh/config — and the server mints a short-TTL token bound to that key. No
// private key material is read by the host-agent itself.
type CredentialMinter struct {
	knownHostsPath string // where the server's SSH host key is pinned (~/.shed/known_hosts)

	// bootstrapRun runs the SSH bootstrap exchange; a field so tests can inject a
	// fake without spawning ssh or standing up a server. Defaults to sdkbootstrap.Run.
	bootstrapRun func(context.Context, sdkbootstrap.Params) (sdk.Bundle, error)
}

// NewCredentialMinter builds a minter from the known_hosts file that pins server
// host keys (tilde-expanded). The SSH identity is resolved by the system ssh
// client (agent/Keychain/IdentityAgent/config), not read from a fixed key file.
func NewCredentialMinter(knownHostsPath string) *CredentialMinter {
	return &CredentialMinter{
		knownHostsPath: expandTilde(knownHostsPath),
		bootstrapRun:   sdkbootstrap.Run,
	}
}

// Token scopes the host-agent mints over SSH: credentials for its own bus
// brokering, control for a token.get on the desktop's behalf.
const (
	scopeCredentials = "credentials"
	scopeControl     = "control"
)

// Mint bootstraps a fresh token of the given scope for t over its SSH endpoint
// and returns the token with its expiry. ssh verifies the server's host key
// against the pin already in known_hosts (the same trust `shed server add`
// established), so this never trusts an unpinned server.
func (m *CredentialMinter) Mint(ctx context.Context, t ServerTarget, scope string) (string, time.Time, error) {
	// Pre-check that the server is pinned. This is NOT a safety latch — a missing
	// pin is already non-terminal/retryable downstream (ssh + sdk/bootstrap
	// classify it as a verification failure, never as a host-key mismatch). It buys
	// two things: an actionable "run shed server add" error instead of ssh's opaque
	// "Host key verification failed", and skipping a doomed ssh spawn.
	if err := knownHostsPinned(m.knownHostsPath, t.SSHHost, t.SSHPort); err != nil {
		return "", time.Time{}, err
	}
	bundle, err := m.bootstrapRun(ctx, sdkbootstrap.Params{
		Host:           t.SSHHost,
		Port:           t.SSHPort,
		KnownHostsPath: m.knownHostsPath,
		Scope:          scope,
		ClientKind:     "host-agent",
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("bootstrapping %s token for %q: %w", scope, t.Name, err)
	}
	return bundle.Token, bundle.ExpiresAt, nil
}

// knownHostsPinned reports whether the known_hosts file has a usable (non-revoked,
// non-CA) host-key entry pinning host:port — the same trust anchor `shed server
// add` wrote via AddKnownHost. It is a presence predicate: ssh re-verifies the pin
// authoritatively during the exchange, so only the existence of an entry matters
// here, not its value. Returns nil when one is present, and an error when the file
// is unreadable, unparseable, or has no entry for the endpoint. (ssh prints the
// same "Host key verification failed" for a missing entry and a garbled file, so
// this in-process read is the only reliable way to give the actionable "run `shed
// server add` first" message instead.)
func knownHostsPinned(knownHostsPath, host string, port int) error {
	data, err := os.ReadFile(knownHostsPath)
	if err != nil {
		return fmt.Errorf("reading known_hosts %s: %w", knownHostsPath, err)
	}
	// knownhosts.Normalize yields the stored form: "[host]:port" for a non-22
	// port, bare "host" for port 22 — matching how OpenSSH (and shed) records it.
	want := knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))
	for len(data) > 0 {
		marker, hosts, _, _, rest, err := ssh.ParseKnownHosts(data)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("parsing known_hosts %s: %w", knownHostsPath, err)
		}
		data = rest
		// Skip marked lines: a @revoked key must never be used as a pin, and a
		// @cert-authority line is a CA, not a host-key pin.
		if marker != "" {
			continue
		}
		for _, h := range hosts {
			if h == want {
				return nil
			}
		}
	}
	return fmt.Errorf("no host key pinned for %s in %s (run `shed server add` first)", want, knownHostsPath)
}

const (
	// tokenRefreshWindow is how long before expiry an on-demand Token re-mints.
	tokenRefreshWindow = 2 * time.Hour
	// The proactive refresh loop re-mints at ~50% of the time remaining until
	// expiry, jittered, clamped to [minRefreshDelay, maxRefreshDelay]; it uses
	// defaultRefreshDelay until a token has been minted.
	defaultRefreshDelay = time.Hour
	minRefreshDelay     = time.Minute
	maxRefreshDelay     = 12 * time.Hour
	// jitterFraction de-synchronizes a fleet re-minting together: the delay is
	// spread ±jitterFraction around its base.
	jitterFraction = 0.25
)

// minter is the subset of CredentialMinter that credentialSource needs; an
// interface so tests can inject a fake without a live SSH server.
type minter interface {
	Mint(ctx context.Context, t ServerTarget, scope string) (string, time.Time, error)
}

// inflightMint coordinates concurrent mints: only one runs at a time, joiners
// wait on done then read token/err. This is the single-flight that both keeps a
// re-mint from being duplicated and lets the network call run off s.mu (so a
// proactive refresh doesn't block a Token caller serving the still-valid token).
type inflightMint struct {
	done   chan struct{}
	token  string
	expiry time.Time
	err    error
}

// credentialSource is an sdk.TokenProvider backed by the SSH credential minter:
// it caches a minted token and re-mints on demand (near expiry, or after a 401
// via Invalidate) and proactively (refreshLoop). A host-key pin mismatch is
// TERMINAL — a possible MITM — so the source fails closed and never serves a
// token for that server again, rather than downgrading to a weaker credential.
type credentialSource struct {
	ctx    context.Context
	mint   minter
	target ServerTarget
	scope  string // scopeCredentials | scopeControl

	mu          sync.Mutex
	token       string
	expiry      time.Time
	terminalErr error
	inflight    *inflightMint
}

func newCredentialSource(ctx context.Context, m minter, t ServerTarget, scope string) *credentialSource {
	return &credentialSource{ctx: ctx, mint: m, target: t, scope: scope}
}

// Token returns the current token, minting or re-minting as needed. Implements
// sdk.TokenProvider.
func (s *credentialSource) Token() (string, error) {
	tok, _, err := s.tokenWithExpiry()
	return tok, err
}

// tokenWithExpiry is Token plus the token's expiry — the form the SDK callers
// need so they know when to ask again. It serves the cached token while fresh.
// A zero expiry means the server returned none.
func (s *credentialSource) tokenWithExpiry() (string, time.Time, error) {
	return s.obtainTokenWithExpiry(false)
}

// forceTokenWithExpiry drops any completed cached token and mints fresh, while
// still coalescing callers that overlap a single in-flight mint (they join it).
// The clear + obtain happen under ONE lock acquisition, so — unlike Invalidate()
// followed by a separate tokenWithExpiry() — a caller can never return a token
// left over from another caller's already-completed mint. The control-token path
// (token.get) uses this: a control token can be silently invalidated out-of-band
// by the target server restarting (it regenerates its token authority), and the
// agent has no signal for that, so it must never serve a cached copy.
//
// One residual race at this layer is accepted (and self-heals): a caller that
// JOINS a mint already in flight across the exact restart instant receives that
// mint's now-stale token once; the next caller re-mints clean. (A server can
// also restart after a fresh mint returns but before the token is used — an
// inherent property of any bearer token, likewise covered by the caller's 401
// retry.)
func (s *credentialSource) forceTokenWithExpiry() (string, time.Time, error) {
	return s.obtainTokenWithExpiry(true)
}

// obtainTokenWithExpiry is the shared body of tokenWithExpiry / forceTokenWithExpiry.
// It fails closed on a terminal error (host-key pin mismatch — a possible MITM),
// then either serves a fresh cached token or mints via the single-flight
// obtainLocked. force skips the cache short-circuit and clears any completed
// token so it is never served (the control path needs that — see
// forceTokenWithExpiry). Keeping the fail-closed guard and the obtain/wait tail
// in one place means the security invariant has a single home.
func (s *credentialSource) obtainTokenWithExpiry(force bool) (string, time.Time, error) {
	s.mu.Lock()
	if s.terminalErr != nil {
		err := s.terminalErr
		s.mu.Unlock()
		return "", time.Time{}, err
	}
	switch {
	case force:
		s.token = "" // force a re-mint; never serve a completed cached token
	case s.token != "" && !s.staleLocked():
		tok, exp := s.token, s.expiry
		s.mu.Unlock()
		return tok, exp, nil
	}
	call := s.obtainLocked() // start a mint, or join one already in flight
	s.mu.Unlock()
	<-call.done
	return call.token, call.expiry, call.err
}

// Invalidate clears the cached token so the next Token re-mints. Called by the
// SDK after a 401 (the server rejected the cached token).
func (s *credentialSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = ""
}

// refresh proactively re-mints, best-effort (errors surface on the next Token).
// Driven by refreshLoop so an idle server's token stays fresh.
func (s *credentialSource) refresh() {
	s.mu.Lock()
	if s.terminalErr != nil {
		s.mu.Unlock()
		return
	}
	call := s.obtainLocked()
	s.mu.Unlock()
	<-call.done
}

// staleLocked reports whether the cached token is within tokenRefreshWindow of
// expiry (a zero expiry is a non-expiring token). The caller holds s.mu.
func (s *credentialSource) staleLocked() bool {
	return !s.expiry.IsZero() && !time.Now().Before(s.expiry.Add(-tokenRefreshWindow))
}

// obtainLocked returns the in-flight mint, starting one (off s.mu, in a goroutine)
// if none is running so N concurrent callers share ONE mint. The caller holds s.mu.
func (s *credentialSource) obtainLocked() *inflightMint {
	if s.inflight != nil {
		return s.inflight
	}
	call := &inflightMint{done: make(chan struct{})}
	s.inflight = call
	go s.doMint(call)
	return call
}

// doMint performs the mint off s.mu, then stores the result under it. A host-key
// pin mismatch is recorded as terminal (fail closed) so it is never retried.
func (s *credentialSource) doMint(call *inflightMint) {
	tok, exp, err := s.mint.Mint(s.ctx, s.target, s.scope)
	s.mu.Lock()
	s.inflight = nil
	switch {
	case err == nil:
		s.token, s.expiry = tok, exp
		call.token, call.expiry = tok, exp // error paths leave call.token "" (zero)
	case errors.Is(err, sdkbootstrap.ErrHostKeyMismatch):
		s.terminalErr = fmt.Errorf("refusing to broker %q: SSH host key pin mismatch (possible MITM): %w", s.target.Name, err)
		err = s.terminalErr
	}
	call.err = err
	s.mu.Unlock()
	close(call.done)
}

// refreshLoop proactively re-mints at ~50% of the time until expiry (jittered),
// so an idle server's token stays fresh and a fleet re-minting after a server
// restart is spread out rather than thundering. Stops when ctx is done.
func (s *credentialSource) refreshLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.nextRefreshDelay()):
		}
		if ctx.Err() != nil {
			return
		}
		s.refresh()
	}
}

// nextRefreshDelay is ~50% of the time until the cached token expires, jittered
// ±25%, clamped to [minRefreshDelay, maxRefreshDelay]; defaultRefreshDelay when
// no token has been minted yet.
func (s *credentialSource) nextRefreshDelay() time.Duration {
	s.mu.Lock()
	exp := s.expiry
	s.mu.Unlock()

	base := defaultRefreshDelay
	if !exp.IsZero() {
		if remaining := time.Until(exp); remaining > 0 {
			base = remaining / 2
		} else {
			base = minRefreshDelay
		}
	}
	// (2*rand-1) is uniform in [-1,1), so this spreads d by ±jitterFraction of base.
	d := base + time.Duration((2*rand.Float64()-1)*jitterFraction*float64(base))
	return min(max(d, minRefreshDelay), maxRefreshDelay)
}
