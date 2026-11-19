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
	"crypto/x509"
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

// Refusal is implemented by an error that ends a Source for good: the entry the
// Source was built from must not be used at all (its stored certificate is for
// another user, or the server issued a certificate for a user the entry is not
// held to). This package cannot import the type that carries the details, so it
// recognizes it by this method alone.
//
// A Source that meets one never mints again in this process. It stops
// presenting anything — every later request fails with the same refusal,
// before any connection is made — unless the refusal came from a proactive
// refresh while the credential it holds is still valid for the entry's user
// (see EnsureFreshErr), in which case it presents that credential until it is
// no longer valid, and then ends.
type Refusal interface {
	error
	// EntryRefusal marks the error; it does nothing.
	EntryRefusal()
}

// IsRefusal reports whether err is, or wraps, a Refusal.
func IsRefusal(err error) bool {
	var r Refusal
	return errors.As(err, &r)
}

// timeNow is the clock the refusal decisions read (whether a held credential is
// still valid). A var so a test can move it past a certificate's NotAfter.
var timeNow = time.Now

// SetClockForTest replaces the clock the refusal decisions read (whether a
// kept certificate is still valid) and returns the function that puts the real
// one back. It is a test seam for packages that drive a Source through a
// client (cmd/shed); nothing outside a test calls it.
func SetClockForTest(now func() time.Time) (restore func()) {
	orig := timeNow
	timeNow = now
	return func() { timeNow = orig }
}

// afterSharedMint, when set, runs in refreshGen right after the shared mint
// returns to a caller and before that caller does anything else: the window in
// which a refusal must already be recorded. Test-only; nil in production.
var afterSharedMint func()

// heldInvalidation is implemented by a Refusal that also withdraws the
// credential the Source already holds, not only the replacement it was
// minting: the entry's recorded user changed under the caller (another
// process re-enrolled it for another user), or the entry's files and its
// recorded user may now disagree. Such a refusal ends the Source at once,
// however valid the held credential still is.
type heldInvalidation interface {
	InvalidatesHeld() bool
}

// invalidatesHeld reports whether err is, or wraps, a Refusal that withdraws
// the held credential too (heldInvalidation).
func invalidatesHeld(err error) bool {
	var h heldInvalidation
	return errors.As(err, &h) && h.InvalidatesHeld()
}

// stillValid reports whether cred may still be presented at now, judged from
// the credential itself: for a certificate, the NotAfter of the leaf actually
// held (never the cached ExpiresAt, which is a scheduling hint recorded beside
// it and can be stale or wrong); for a token, its recorded expiry, which is
// all a token has (zero: not known to be valid). A certificate whose leaf
// cannot be read is not valid.
func stillValid(cred Credential, now time.Time) bool {
	if !cred.Usable() {
		return false
	}
	if c := cred.ClientCertificate(); c != nil {
		leaf := c.Leaf
		if leaf == nil {
			if len(c.Certificate) == 0 {
				return false
			}
			parsed, err := x509.ParseCertificate(c.Certificate[0])
			if err != nil {
				return false
			}
			leaf = parsed
		}
		return now.Before(leaf.NotAfter)
	}
	return !cred.ExpiresAt.IsZero() && now.Before(cred.ExpiresAt)
}

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
	// credRejected records that the server REFUSED the credential of the
	// current generation while the replacement mint failed (Reject). While
	// set, EnsureFreshErr stops treating the credential as fresh: every call
	// re-attempts the mint and surfaces the mint error instead of vouching
	// for something the server already turned away. Cleared when a mint
	// advances the generation. Only meaningful for the generation it was set
	// against — both writes hold mu and check gen.
	credRejected bool
	// refused, when set, is the terminal state: the entry must not be used.
	// It is set at construction (Refused) or by a mint whose error is a
	// Refusal, and never cleared. While set the Source presents nothing and
	// never calls refresh. Guarded by mu.
	refused error
	// kept, when set, is a REFUSED REPLACEMENT: a proactive refresh was
	// refused (the server issued a certificate for another user) while the
	// credential held was still valid for the entry's user, so that
	// credential stays in use. It is never minted past: no later call runs
	// refresh again (each would ssh and be refused the same way). Once the
	// held credential is no longer valid, or the server rejects it (a
	// reactive Refresh), kept becomes the terminal refusal. Guarded by mu;
	// set only inside the shared mint (refreshGen), never cleared.
	kept error
}

// New returns a refreshing Source seeded with initial. A nil refresh yields a
// static Source.
func New(initial Credential, refresh func() (Credential, error)) *Source {
	return &Source{cred: initial, refresh: refresh}
}

// Refused returns a Source in the terminal state: it holds no credential, has
// no refresh, never runs ssh, and Err returns err. Every caller checks Err
// before it connects, so a request built on it fails before any dial.
func Refused(err error) *Source {
	return &Source{refused: err}
}

// Err returns the refusal that ended this Source, or nil while it is usable.
//
// It is the check every request path makes before it sends or dials (the CLI's
// sendWithReauth, Ping and Status, its transport gate, the Connect client's
// every attempt, the handshake's CertificateFor), so it is also where a KEPT
// credential's expiry is noticed: each call reads the held leaf's validity,
// and once it is no longer valid the remembered refusal ends the Source. An
// expired kept certificate is therefore never sent or dialled with, whichever
// path comes next, not only after another EnsureFreshErr.
func (s *Source) Err() error {
	now := timeNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireKeptLocked(now)
	return s.refused
}

// expireKeptLocked ends the Source with its kept refusal once the credential it
// kept is no longer valid at now (s.mu held).
func (s *Source) expireKeptLocked(now time.Time) {
	if s.refused == nil && s.kept != nil && !stillValid(s.cred, now) {
		s.refused = s.kept
	}
}

// refuse records err as the terminal state, unless one is already recorded.
func (s *Source) refuse(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refused == nil {
		s.refused = err
	}
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
	if s.Err() != nil {
		// A refused Source presents nothing, even for a request that pinned a
		// credential before the refusal landed. (Every caller checks Err before
		// it dials; this is the backstop.)
		return nil
	}
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
// long-lived process does want it — a broker's bus client asks its credential
// source for something to present and has to be able to say WHY there is
// nothing, rather than silently sending an unauthenticated request and
// reporting the resulting 401 as the problem.
//
// nil means "there is a usable, non-expiring-soon credential now" — either
// because there already was one, or because this call minted it.
//
// A mint that fails with a Refusal ends the Source (Err) unless the credential
// it holds is still valid for the entry's user (stillValid: the leaf's own
// NotAfter, not the cached ExpiresAt) and the refusal does not withdraw it
// (heldInvalidation). A kept credential stays in use and the refusal is
// remembered: later calls return it WITHOUT minting again, until the held
// credential is no longer valid, when the Source ends.
//
// The proactive decision is scheduled on the cached ExpiresAt, as before. A
// ZERO cached expiry therefore never refreshes proactively (needsRefresh), so a
// refusal can only come from the reactive path, which always ends the Source.
func (s *Source) EnsureFreshErr() error {
	now := timeNow()
	s.mu.Lock()
	s.expireKeptLocked(now)
	cred, gen, rejected, refused, kept := s.cred, s.gen, s.credRejected, s.refused, s.kept
	s.mu.Unlock()
	if refused != nil {
		return refused
	}
	if kept != nil {
		return kept // the held credential is still valid; never mint past a refused refresh
	}
	if s.refresh == nil {
		return nil
	}
	// A rejected credential is disqualified from the fast path even when its
	// local expiry looks fine: the server refused it and the replacement mint
	// failed (Reject), so vouching for it here would hand the caller the very
	// credential the server just turned away — and swallow the reason why.
	if cred.Usable() && !needsRefresh(cred.ExpiresAt, now) && !rejected {
		return nil
	}
	_, err := s.refreshGen(gen, false)
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
//
// A mint that fails with a Refusal ends the Source: Err returns it from then on
// and every later Refresh returns it without minting. A refused Source returns
// its refusal at once. A reactive Refresh after a refused proactive refresh
// (EnsureFreshErr kept the held credential) does not mint either: the server
// has now rejected what was kept, so the remembered refusal ends the Source.
func (s *Source) Refresh(prevGen uint64) (Credential, error) {
	return s.refreshGen(prevGen, true)
}

// refreshGen is Refresh; reactive says the caller's credential was rejected by
// the server (Refresh, Reject), as opposed to a proactive EnsureFreshErr.
//
// A REFUSAL IS RECORDED INSIDE THE SHARED MINT, before the mint returns to any
// caller, so no later caller can observe the Source unrefused and start a
// second mint: the Source is either ended (refused) or holds a refused
// replacement (kept), and both stop every later mint. The shared mint also
// checks for a recorded refusal before it mints and again before it installs a
// result, so a mint that finishes after the Source ended installs nothing.
func (s *Source) refreshGen(prevGen uint64, reactive bool) (Credential, error) {
	if err := s.Err(); err != nil {
		return Credential{}, err
	}
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
		s.mu.Lock()
		if s.refused != nil {
			refused := s.refused
			s.mu.Unlock()
			return Credential{}, refused
		}
		if s.gen != prevGen {
			cur := s.cred
			s.mu.Unlock()
			return cur, nil // another batch minted past prevGen between the check above and here
		}
		if s.kept != nil {
			// A refused replacement is already known: never mint past it. A
			// reactive leader, or a held credential no longer valid, ends the
			// Source with it.
			kept := s.kept
			if reactive || !stillValid(s.cred, timeNow()) {
				s.refused = kept
			}
			s.mu.Unlock()
			return Credential{}, kept
		}
		s.mu.Unlock()

		// Mint without the mutex held (a network round-trip); singleflight
		// guarantees only one goroutine runs this at a time for this generation.
		fresh, mintErr := s.refresh()

		s.mu.Lock()
		defer s.mu.Unlock()
		if s.refused != nil {
			return Credential{}, s.refused // ended while this mint ran: install nothing
		}
		if mintErr != nil {
			if IsRefusal(mintErr) {
				s.recordRefusalLocked(mintErr, reactive)
			}
			return Credential{}, mintErr
		}
		s.cred = fresh
		s.gen++
		s.credRejected = false // a fresh credential has no rejection history
		return fresh, nil
	})
	if afterSharedMint != nil {
		afterSharedMint()
	}
	if err != nil {
		// A reactive caller that coalesced onto a proactive leader's mint: the
		// leader may have kept the held credential, but the server has rejected
		// it for this caller, so the refusal ends the Source. (No mint can start
		// in between: kept already stops every mint.)
		if reactive && IsRefusal(err) {
			s.refuse(err)
		}
		return Credential{}, err
	}
	return v.(Credential), nil
}

// recordRefusalLocked records a Refusal a mint returned (s.mu held): it ends
// the Source when the caller is reactive (the server rejected what is held),
// when the refusal withdraws the held credential too (heldInvalidation), or
// when the held credential is not still valid (stillValid) or was rejected
// (Reject); otherwise the held credential is kept and the refusal remembered.
func (s *Source) recordRefusalLocked(err error, reactive bool) {
	if reactive || invalidatesHeld(err) || s.credRejected || !stillValid(s.cred, timeNow()) {
		s.refused = err
		return
	}
	s.kept = err
}

// Reject is Refresh for the reactive path: the server REFUSED the credential
// belonging to prevGen (a 401, or a TLS alert naming the certificate). The
// difference is what a FAILED mint leaves behind. Refresh keeps quiet — a
// proactive refresh failing says nothing about the credential it would have
// replaced, which the server may still be accepting happily. Reject remembers
// the rejection: the credential itself stays in place to be presented (a
// rejection can be transient — a mid-rotation allowlist refresh — and
// presenting something the server might still accept beats presenting
// nothing), but EnsureFreshErr stops vouching for it, so every subsequent
// request re-attempts the mint and surfaces the mint error instead of
// silently re-sending what the server already refused. Recovery is the next
// mint that succeeds — it advances the generation and clears the mark.
func (s *Source) Reject(prevGen uint64) (Credential, error) {
	cred, err := s.Refresh(prevGen)
	if err != nil {
		s.mu.Lock()
		if s.gen == prevGen {
			s.credRejected = true
		}
		s.mu.Unlock()
	}
	return cred, err
}
