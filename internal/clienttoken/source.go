// Package clienttoken holds a bearer token that transparently re-mints itself,
// shared by every shed client that talks to a token-mode shed-server: the CLI
// API client and the tunnel Connect client. A Source pairs the current token
// with its expiry and an injected refresh callback (the actual SSH mint +
// optional persist lives in the caller, keeping this package a dependency-free
// leaf). Refresh is proactive (before expiry) and reactive (on a 401),
// generation-aware, and coalesced so concurrent callers mint at most once.
package clienttoken

import (
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// tokenRefreshWindow is how long before expiry a token is proactively re-minted
// by EnsureFresh, so a request never races the expiry. It is a fixed threshold,
// not derived from the token's TTL: a token_ttl at or below it makes EnsureFresh
// re-mint eagerly (up to once per call) — still correct, and reactive Refresh
// covers expiry regardless — but the default token_ttl (24h) sits far above it,
// so in normal operation a token is re-minted at most once, within its final 2h.
const tokenRefreshWindow = 2 * time.Hour

// ErrStatic is returned by Refresh when the Source has no refresh callback (a
// static token, an open server, or a plain-HTTP client). Callers use it — or,
// preferably, a prior Refreshable() check — to know a 401 must not be retried.
var ErrStatic = errors.New("clienttoken: source has no refresh callback")

// Source is a concurrency-safe holder for a bearer token and its expiry, with an
// optional refresh callback. The zero value is not usable; construct with New or
// Static.
type Source struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
	// refresh mints a new token (and its expiry). nil ⇒ static: the token never
	// changes. It is invoked without the mutex held, at most once per coalesced
	// batch (see Refresh).
	refresh func() (token string, exp time.Time, err error)
	sf      singleflight.Group
}

// New returns a refreshing Source seeded with token/expiresAt. A nil refresh
// yields a static Source (equivalent to Static).
func New(token string, expiresAt time.Time, refresh func() (string, time.Time, error)) *Source {
	return &Source{token: token, expiresAt: expiresAt, refresh: refresh}
}

// Static returns a Source that never mints — Token returns token, EnsureFresh is
// a no-op, and Refresh returns ErrStatic.
func Static(token string) *Source {
	return &Source{token: token}
}

// Token returns the current bearer token. It is cheap and safe to call on every
// request build (including a retry, which then picks up a just-refreshed token).
func (s *Source) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// ExpiresAt returns the current token's expiry (zero for a static token). Used to
// seed a sibling Source from a freshly-refreshed one.
func (s *Source) ExpiresAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiresAt
}

// Refreshable reports whether this Source can re-mint. Callers gate their on-401
// retry on this so a static token behaves exactly as before (no retry).
func (s *Source) Refreshable() bool {
	return s.refresh != nil
}

// needsRefresh reports whether a token with the given expiry should be
// proactively re-minted: expiry is set (non-zero) and now is within
// tokenRefreshWindow of it (or past it). A zero expiry never refreshes.
func needsRefresh(expiresAt, now time.Time) bool {
	if expiresAt.IsZero() {
		return false
	}
	return !now.Before(expiresAt.Add(-tokenRefreshWindow))
}

// EnsureFresh proactively re-mints when the current token is within the refresh
// window of its expiry, so the next request never races expiry. It is
// best-effort: a mint error leaves the existing token in place (the reactive
// Refresh path surfaces errors to a real request). No-op for a static Source.
func (s *Source) EnsureFresh() {
	if s.refresh == nil {
		return
	}
	s.mu.Lock()
	prev := s.token
	stale := needsRefresh(s.expiresAt, time.Now())
	s.mu.Unlock()
	if stale {
		_, _ = s.Refresh(prev)
	}
}

// Refresh re-mints the token and returns the new one. It is:
//   - generation-aware: if another caller already advanced the token past prev,
//     it returns the current token without minting again (so a stale 401 that
//     lost the race doesn't trigger a redundant mint);
//   - coalesced: concurrent Refresh/EnsureFresh calls share a single mint via
//     singleflight.
//
// prev is the token the caller used on the request that failed (or the token it
// observed before EnsureFresh). Returns ErrStatic when the Source cannot mint.
func (s *Source) Refresh(prev string) (string, error) {
	if s.refresh == nil {
		return s.Token(), ErrStatic
	}
	// Fast generation check BEFORE singleflight: a caller whose token was already
	// superseded (prev != current) just uses the current token, and must not enter
	// the shared mint — otherwise it could win the singleflight with a "no mint
	// needed" answer and hand it to a genuine current-generation caller that
	// coalesced on, starving a real refresh.
	if cur := s.Token(); cur != prev {
		return cur, nil
	}
	// Key the singleflight by the generation being replaced (prev == the current
	// token here). Same-generation callers coalesce onto one mint; a caller
	// replacing a *different* generation uses a different key and gets its own
	// mint rather than a stale answer.
	v, err, _ := s.sf.Do(prev, func() (interface{}, error) {
		if cur := s.Token(); cur != prev {
			return cur, nil // another batch minted past prev between the check above and here
		}
		// Mint without the mutex held (a network round-trip); singleflight
		// guarantees only one goroutine runs this at a time for this generation.
		tok, exp, mintErr := s.refresh()
		if mintErr != nil {
			return "", mintErr
		}
		s.mu.Lock()
		s.token = tok
		s.expiresAt = exp
		s.mu.Unlock()
		return tok, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}
