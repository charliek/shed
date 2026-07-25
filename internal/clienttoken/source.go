// Package clienttoken holds the credential a shed client presents to a
// shed-server and transparently re-mints it. It is shared by every in-tree
// client that talks to a secure server: the CLI API client and the tunnel
// Connect client.
//
// A Source is a two-state machine over the shapes a shed-server issues:
//
//	token(bearer, expiry)     — auth.mode: token (and legacy/open servers)
//	mtls(certificate, expiry) — auth.mode: mtls
//
// Which state it is in is decided by the SERVER, at each bootstrap, not by
// local configuration: the injected refresh callback returns whichever
// Credential the server minted, and the Source adopts it. An operator flipping
// a server between token and mtls therefore does not require clients to be
// reconfigured — the next re-mint moves the Source (and, above it, the stored
// config entry) to the other state. That is why refresh returns a Credential
// and not a token string, and why the transport above never branches on the
// state it *expects* to be in.
//
// Refresh is proactive (before expiry) and reactive (on an auth-shaped
// failure), generation-aware, and coalesced so concurrent callers mint at most
// once. The actual SSH mint + optional persist lives in the caller, keeping
// this package a near-leaf (crypto/tls for the certificate type, and nothing
// else from the tree).
package clienttoken

import (
	"context"
	"crypto/tls"
	"errors"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Credential modes. They mirror the server's config.AuthMode* / sdk.AuthMode*
// literals for the two shapes a bootstrap can return.
const (
	ModeToken = "token"
	ModeMTLS  = "mtls"
)

// RefreshWindow is how long before expiry a credential is proactively re-minted
// by EnsureFresh, so a request never races the expiry. It is a fixed threshold,
// not derived from the TTL: a token_ttl at or below it makes EnsureFresh
// re-mint eagerly (up to once per call) — still correct, and reactive Refresh
// covers expiry regardless — but the default token_ttl (24h) sits far above it,
// so in normal operation a credential is re-minted at most once, within its
// final 2h. The same window governs client certificates, whose TTL is the same
// auth.token_ttl knob.
const RefreshWindow = 2 * time.Hour

// ErrStatic is returned by Refresh when the Source has no refresh callback (a
// static token, an open server, or a plain-HTTP client). Callers use it — or,
// preferably, a prior Refreshable() check — to know an auth failure must not be
// retried.
var ErrStatic = errors.New("clienttoken: source has no refresh callback")

// Credential is one minted authentication credential: EITHER a bearer token OR
// a client certificate, plus when it expires.
//
// The zero value is a usable "no credential" (an open server): Mode is empty,
// which is neither ModeToken nor ModeMTLS, and both accessors return nothing.
type Credential struct {
	// Mode is ModeToken or ModeMTLS (or "" for no credential at all).
	Mode string
	// Token is the bearer token. Meaningful only when Mode is ModeToken.
	Token string
	// Cert is the client certificate, already assembled for the TLS stack
	// (leaf + private key). Meaningful only when Mode is ModeMTLS. Treated as
	// immutable once stored: it is handed to concurrent handshakes by pointer.
	Cert *tls.Certificate
	// ExpiresAt is when the credential stops being accepted. Zero means "never
	// proactively re-mint" — a static or legacy credential.
	ExpiresAt time.Time
}

// TokenCredential builds a bearer-token credential.
func TokenCredential(token string, expiresAt time.Time) Credential {
	return Credential{Mode: ModeToken, Token: token, ExpiresAt: expiresAt}
}

// MTLSCredential builds a client-certificate credential.
func MTLSCredential(cert *tls.Certificate, expiresAt time.Time) Credential {
	return Credential{Mode: ModeMTLS, Cert: cert, ExpiresAt: expiresAt}
}

// BearerToken returns the token to send in an Authorization header, or "" when
// this credential is not a bearer token.
//
// The mode gate is load-bearing, not defensive tidiness: in mtls mode the
// server never reads the Authorization header, and a client that kept sending
// a stale bearer alongside its certificate would be shipping a live credential
// to an endpoint that ignores it — pure downside.
func (c Credential) BearerToken() string {
	if c.Mode != ModeToken {
		return ""
	}
	return c.Token
}

// ClientCertificate returns the certificate to present at a TLS handshake, or
// nil when this credential is not a certificate.
func (c Credential) ClientCertificate() *tls.Certificate {
	if c.Mode != ModeMTLS {
		return nil
	}
	return c.Cert
}

// Usable reports whether this credential has anything to present to a server: a
// bearer token in token state, a certificate in mtls state.
//
// The zero Credential is NOT usable, and neither is a mode whose payload is
// missing (an mtls credential with no certificate — the shape a stored entry
// degrades to when its certificate files are gone). Both mean the same thing to
// a request: it will be sent unauthenticated, and a server that demands a
// credential will refuse it.
//
// It is what EnsureFresh keys the "mint before the first request" decision on,
// because expiry alone cannot express it: a credential we never had has no
// expiry to be near.
func (c Credential) Usable() bool {
	return c.BearerToken() != "" || c.ClientCertificate() != nil
}

// Source is a concurrency-safe holder for the current Credential, with an
// optional refresh callback. The zero value is not usable; construct with New
// or Static.
type Source struct {
	mu   sync.Mutex
	cred Credential
	// gen counts credential generations. It replaces the old "compare the token
	// string" identity check: a certificate has no natural string form to
	// compare, and after a mode flip the two generations are not even the same
	// kind of value. A monotonic counter is the one identity that survives both.
	gen uint64
	// refresh mints a new credential. nil ⇒ static: the credential never
	// changes. It is invoked without the mutex held, at most once per coalesced
	// batch (see Refresh).
	refresh func() (Credential, error)
	sf      singleflight.Group
}

// New returns a refreshing Source seeded with initial. A nil refresh yields a
// static Source.
func New(initial Credential, refresh func() (Credential, error)) *Source {
	return &Source{cred: initial, refresh: refresh}
}

// Static returns a Source that never mints — Token returns token, EnsureFresh
// is a no-op, and Refresh returns ErrStatic. An empty token yields a Source
// with no credential at all (an open server).
func Static(token string) *Source {
	if token == "" {
		return &Source{}
	}
	return &Source{cred: Credential{Mode: ModeToken, Token: token}}
}

// Current returns the live credential and the generation it belongs to. The
// generation is what a caller hands back to Refresh after an auth failure, so a
// request that lost a race against a concurrent re-mint does not trigger a
// second one.
func (s *Source) Current() (Credential, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred, s.gen
}

// Token returns the current bearer token ("" in mtls state or with no
// credential). Cheap and safe to call on every request build.
func (s *Source) Token() string {
	cred, _ := s.Current()
	return cred.BearerToken()
}

// ClientCertificate returns the current client certificate (nil in token
// state). It is the function installed as tls.Config.GetClientCertificate's
// backing source, so it is called on every handshake from arbitrary goroutines.
func (s *Source) ClientCertificate() *tls.Certificate {
	cred, _ := s.Current()
	return cred.ClientCertificate()
}

// pinnedKey is the context key under which one request carries the exact
// Credential it captured. Unexported and of a private type, so nothing outside
// this package can set or collide with it.
type pinnedKey struct{}

// WithPinned returns a context that pins cred as THE credential for the request
// it is attached to.
//
// It exists because a request transmits its credential through two independent
// channels — the Authorization header (built when the request is assembled) and
// the TLS client certificate (fetched by the TLS stack during the handshake) —
// and each of them, left to itself, re-reads the live Source at a DIFFERENT
// moment. A concurrent re-mint landing between those two reads means the
// generation a caller recorded is not the generation it sent, and the reactive
// retry then re-sends the credential the server just rejected (Refresh sees the
// recorded generation as stale and skips the mint).
//
// Pinning closes that window: the caller captures (credential, generation)
// once, atomically, and both channels read back the SAME captured value. The
// generation recorded is provably the generation transmitted.
//
// For an http.Transport dial the TLS handshake runs under the context of the
// request that triggered it, which is what makes the certificate half reachable
// — see Source.CertificateFor.
func WithPinned(ctx context.Context, cred Credential) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pinnedKey{}, cred)
}

// PinnedCredential returns the credential WithPinned attached to ctx, and
// whether there was one. A context with no pin (a background handshake, a
// pooled connection re-handshaking outside any request) reports false, and
// callers fall back to the Source's current credential.
func PinnedCredential(ctx context.Context) (Credential, bool) {
	if ctx == nil {
		return Credential{}, false
	}
	cred, ok := ctx.Value(pinnedKey{}).(Credential)
	return cred, ok
}

// CertificateFor is the per-handshake client-certificate source installed on a
// pinned TLS config (servertls.ClientCertSource). It prefers the credential
// pinned on the handshake's context — so a request presents the very
// certificate whose generation it recorded — and falls back to the Source's
// current credential when the handshake carries no pin.
//
// The fallback is not a corner case to be tolerated but the behavior that keeps
// a connection pool correct: a connection dialed for one request is reused by
// others, and a handshake that happens outside any pinned request must still
// present something usable.
func (s *Source) CertificateFor(ctx context.Context) *tls.Certificate {
	if cred, ok := PinnedCredential(ctx); ok {
		return cred.ClientCertificate()
	}
	return s.ClientCertificate()
}

// Mode returns the current credential's mode (ModeToken, ModeMTLS, or "").
func (s *Source) Mode() string {
	cred, _ := s.Current()
	return cred.Mode
}

// ExpiresAt returns the current credential's expiry (zero for a static one).
// Used to seed a sibling Source from a freshly-refreshed one.
func (s *Source) ExpiresAt() time.Time {
	cred, _ := s.Current()
	return cred.ExpiresAt
}

// Refreshable reports whether this Source can re-mint. Callers gate their
// reactive retry on it so a static credential behaves exactly as before (no
// retry).
func (s *Source) Refreshable() bool {
	return s.refresh != nil
}

// needsRefresh reports whether a credential with the given expiry should be
// proactively re-minted: expiry is set (non-zero) and now is within
// RefreshWindow of it (or past it). A zero expiry never refreshes.
func needsRefresh(expiresAt, now time.Time) bool {
	if expiresAt.IsZero() {
		return false
	}
	return !now.Before(expiresAt.Add(-RefreshWindow))
}

// EnsureFresh proactively mints before the next request, in the two situations
// where letting the request go out as-is is known to fail:
//
//   - the current credential is within RefreshWindow of its expiry (or past it),
//     so the request would race expiry;
//   - the Source holds NOTHING usable at all (Credential.Usable is false) while
//     being able to mint. That is not a hypothetical: it is the state a stored
//     config entry degrades to when an older client rewrote it and dropped the
//     credential fields it did not understand, or when the credential files it
//     points at are gone. Expiry cannot express that case — a credential that was
//     never held has no expiry to be near — so it is checked separately, and
//     checking it here is what makes the recovery PROACTIVE rather than dependent
//     on a rejection that some request paths (streaming, long-timeout) have no
//     retry to act on.
//
// It is best-effort: a mint error leaves the existing credential in place (the
// reactive Refresh path surfaces errors to a real request). No-op for a static
// Source — an open server holds nothing usable either, and has nothing to mint.
func (s *Source) EnsureFresh() { _ = s.EnsureFreshErr() }

// EnsureFreshErr is EnsureFresh with the mint error returned instead of
// dropped.
//
// The CLI genuinely does not want the error: it mints ahead of a command that
// will surface any real problem itself, and a warning there would be noise. A
// long-lived daemon does want it — the host-agent's bus asks its credential
// source for something to present and has to be able to say WHY there is
// nothing, rather than silently sending an unauthenticated request and
// reporting the resulting 401 as the problem.
//
// nil means "there is a usable, non-expiring-soon credential now" — either
// because there already was one, or because this call minted it.
func (s *Source) EnsureFreshErr() error {
	if s.refresh == nil {
		return nil
	}
	cred, gen := s.Current()
	if cred.Usable() && !needsRefresh(cred.ExpiresAt, time.Now()) {
		return nil
	}
	_, err := s.Refresh(gen)
	return err
}

// Refresh re-mints the credential and returns the new one. It is:
//   - generation-aware: if another caller already advanced past prevGen, it
//     returns the current credential without minting again (so a stale auth
//     failure that lost the race doesn't trigger a redundant mint);
//   - coalesced: concurrent Refresh/EnsureFresh calls share a single mint via
//     singleflight.
//
// prevGen is the generation the caller used on the request that failed (or the
// generation it observed before EnsureFresh). Returns ErrStatic when the Source
// cannot mint.
func (s *Source) Refresh(prevGen uint64) (Credential, error) {
	if s.refresh == nil {
		cur, _ := s.Current()
		return cur, ErrStatic
	}
	// Fast generation check BEFORE singleflight: a caller whose credential was
	// already superseded just uses the current one, and must not enter the
	// shared mint — otherwise it could win the singleflight with a "no mint
	// needed" answer and hand it to a genuine current-generation caller that
	// coalesced on, starving a real refresh.
	cred, gen := s.Current()
	if gen != prevGen {
		return cred, nil
	}
	// Key the singleflight by the generation being replaced. Same-generation
	// callers coalesce onto one mint; a caller replacing a DIFFERENT generation
	// uses a different key and gets its own mint rather than a stale answer.
	v, err, _ := s.sf.Do(strconv.FormatUint(prevGen, 10), func() (interface{}, error) {
		if cur, curGen := s.Current(); curGen != prevGen {
			return cur, nil // another batch minted past prevGen between the check above and here
		}
		// Mint without the mutex held (a network round-trip); singleflight
		// guarantees only one goroutine runs this at a time for this generation.
		fresh, mintErr := s.refresh()
		if mintErr != nil {
			return Credential{}, mintErr
		}
		s.mu.Lock()
		s.cred = fresh
		s.gen++
		s.mu.Unlock()
		return fresh, nil
	})
	if err != nil {
		return Credential{}, err
	}
	return v.(Credential), nil
}
