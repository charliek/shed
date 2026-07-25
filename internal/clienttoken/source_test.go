package clienttoken

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNeedsRefresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"zero expiry (static token / open server) never refreshes", time.Time{}, false},
		{"far-future expiry does not refresh", now.Add(24 * time.Hour), false},
		{"within the refresh window refreshes", now.Add(time.Hour), true},
		{"exactly at the window edge refreshes", now.Add(refreshWindow), true},
		{"expired refreshes", now.Add(-time.Minute), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsRefresh(tt.expiresAt, now); got != tt.want {
				t.Errorf("needsRefresh = %v, want %v", got, tt.want)
			}
		})
	}
}

// countingRefresh returns a refresh callback that mints "tokN" tokens and counts
// invocations.
func countingRefresh(count *int32, ttl time.Duration) func() (Credential, error) {
	return func() (Credential, error) {
		n := atomic.AddInt32(count, 1)
		return TokenCredential("tok"+strconv.Itoa(int(n)), time.Now().Add(ttl)), nil
	}
}

// gen returns the source's current generation, for the prevGen argument.
func gen(s *Source) uint64 {
	_, g := s.Current()
	return g
}

func TestStaticSourceNeverMints(t *testing.T) {
	s := Static("static-tok")
	if s.Refreshable() {
		t.Error("static source must not be refreshable")
	}
	if s.Token() != "static-tok" {
		t.Errorf("Token = %q, want static-tok", s.Token())
	}
	s.EnsureFresh() // no-op
	if s.Token() != "static-tok" {
		t.Errorf("EnsureFresh mutated a static token: %q", s.Token())
	}
	cred, err := s.Refresh(gen(s))
	if !errors.Is(err, ErrStatic) {
		t.Errorf("Refresh on static: err = %v, want ErrStatic", err)
	}
	if cred.Token != "static-tok" {
		t.Errorf("Refresh on static returned unexpected token %q", cred.Token)
	}
}

func TestEnsureFreshWithinWindowMintsOnce(t *testing.T) {
	var count int32
	// Expiry inside the window ⇒ EnsureFresh should mint.
	s := New(TokenCredential("old", time.Now().Add(time.Hour)), countingRefresh(&count, 24*time.Hour))
	s.EnsureFresh()
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("mint count = %d, want 1", got)
	}
	if s.Token() != "tok1" {
		t.Errorf("token = %q, want tok1", s.Token())
	}
	// Now far from expiry ⇒ a second EnsureFresh must NOT mint again.
	s.EnsureFresh()
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("mint count = %d after fresh EnsureFresh, want 1 (no re-mint)", got)
	}
}

func TestEnsureFreshFarFromExpiryDoesNotMint(t *testing.T) {
	var count int32
	s := New(TokenCredential("old", time.Now().Add(24*time.Hour)), countingRefresh(&count, 24*time.Hour))
	s.EnsureFresh()
	if got := atomic.LoadInt32(&count); got != 0 {
		t.Errorf("mint count = %d, want 0 (far from expiry)", got)
	}
	if s.Token() != "old" {
		t.Errorf("token = %q, want old (unchanged)", s.Token())
	}
}

func TestEnsureFreshZeroExpiryDoesNotMint(t *testing.T) {
	var count int32
	// Refreshable but zero expiry: proactive refresh must not fire.
	s := New(TokenCredential("old", time.Time{}), countingRefresh(&count, 24*time.Hour))
	s.EnsureFresh()
	if got := atomic.LoadInt32(&count); got != 0 {
		t.Errorf("mint count = %d, want 0 (zero expiry)", got)
	}
}

// TestCredentialUsable pins the predicate EnsureFresh uses to decide "we hold
// nothing — mint before the first request". It is deliberately about the
// PAYLOAD, not the mode: a recorded mode with its material missing is exactly
// as unpresentable as no credential at all.
func TestCredentialUsable(t *testing.T) {
	tests := []struct {
		name string
		cred Credential
		want bool
	}{
		{"a bearer token is usable", TokenCredential("tok", time.Now()), true},
		{"a certificate is usable", MTLSCredential(certFor("c1"), time.Now()), true},
		{"the zero credential holds nothing", Credential{}, false},
		{"mtls with no certificate holds nothing", Credential{Mode: ModeMTLS}, false},
		{"token mode with an empty token holds nothing", Credential{Mode: ModeToken}, false},
		{"a certificate recorded under token mode is not presentable", Credential{Mode: ModeToken, Cert: certFor("c1")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cred.Usable(); got != tt.want {
				t.Errorf("Usable = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEnsureFreshWithNoCredentialMints is the state-machine gap that made a
// credential-less config entry fail forever: a Source that CAN mint but holds
// nothing has no expiry to be near, so an expiry-only proactive check never
// fires — and, holding nothing, it also has no credential for a server to
// reject, so the reactive path has nothing to trigger on either. It must mint
// up front.
func TestEnsureFreshWithNoCredentialMints(t *testing.T) {
	tests := []struct {
		name    string
		initial Credential
	}{
		{"no credential at all (entry stripped by an older client)", Credential{}},
		{"mtls mode with no certificate (credential files gone)", Credential{Mode: ModeMTLS}},
		{"mtls mode with no certificate and a far-future expiry", Credential{Mode: ModeMTLS, ExpiresAt: time.Now().Add(24 * time.Hour)}},
		{"token mode with an empty token", Credential{Mode: ModeToken, ExpiresAt: time.Now().Add(24 * time.Hour)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var count int32
			s := New(tt.initial, countingRefresh(&count, 24*time.Hour))
			s.EnsureFresh()
			if got := atomic.LoadInt32(&count); got != 1 {
				t.Fatalf("mint count = %d, want 1 (the source held nothing to present)", got)
			}
			if s.Token() != "tok1" {
				t.Errorf("token = %q, want tok1", s.Token())
			}
			// And exactly once: the minted credential is usable and far from
			// expiry, so a second call must be a no-op rather than an SSH
			// round-trip per client construction.
			s.EnsureFresh()
			if got := atomic.LoadInt32(&count); got != 1 {
				t.Errorf("mint count = %d after a successful mint, want 1", got)
			}
		})
	}
}

// TestEnsureFreshWithNoCredentialStaysStatic: an open server holds nothing
// either, and must NOT pay a mint for it. The gate is the refresh callback (a
// static Source has none), not the credential.
func TestEnsureFreshWithNoCredentialStaysStatic(t *testing.T) {
	s := Static("")
	if s.Refreshable() {
		t.Fatal("an empty Static source must not be refreshable")
	}
	s.EnsureFresh() // must not panic, must not mint
	if cred, _ := s.Current(); cred.Usable() {
		t.Errorf("an open-server source acquired a credential: %+v", cred)
	}
}

func TestRefreshMintsAndUpdatesExpiry(t *testing.T) {
	var count int32
	exp := time.Now().Add(24 * time.Hour)
	s := New(TokenCredential("old", time.Now().Add(time.Hour)), func() (Credential, error) {
		atomic.AddInt32(&count, 1)
		return TokenCredential("new", exp), nil
	})
	cred, err := s.Refresh(gen(s))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if cred.Token != "new" || s.Token() != "new" {
		t.Errorf("token = %q / %q, want new", cred.Token, s.Token())
	}
	if !s.ExpiresAt().Equal(exp) {
		t.Errorf("expiry = %v, want %v (updated on refresh)", s.ExpiresAt(), exp)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("mint count = %d, want 1", got)
	}
}

func TestRefreshGenerationAware(t *testing.T) {
	var count int32
	s := New(TokenCredential("old", time.Now().Add(time.Hour)), countingRefresh(&count, 24*time.Hour))
	gen0 := gen(s)
	// First auth failure at generation 0 ⇒ mints (token becomes tok1).
	if _, err := s.Refresh(gen0); err != nil {
		t.Fatal(err)
	}
	// A second, stale auth failure that still names generation 0 must NOT
	// re-mint; it should observe the already-advanced credential.
	cred, err := s.Refresh(gen0)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Token != "tok1" {
		t.Errorf("stale Refresh returned %q, want the current tok1", cred.Token)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("mint count = %d, want 1 (generation-aware, no double mint)", got)
	}
}

func TestRefreshError(t *testing.T) {
	sentinel := errors.New("mint boom")
	s := New(TokenCredential("old", time.Now().Add(time.Hour)), func() (Credential, error) {
		return Credential{}, sentinel
	})
	if _, err := s.Refresh(gen(s)); !errors.Is(err, sentinel) {
		t.Errorf("Refresh err = %v, want the mint error", err)
	}
	if s.Token() != "old" {
		t.Errorf("token = %q, want old (unchanged after a failed mint)", s.Token())
	}
}

// TestRefreshConcurrentCoalesces fires many goroutines that all 401 with the same
// prev token; singleflight + the generation check must yield exactly one mint.
// Run with -race for the concurrency guarantee.
func TestRefreshConcurrentCoalesces(t *testing.T) {
	var count int32
	release := make(chan struct{})
	s := New(TokenCredential("old", time.Now().Add(time.Hour)), func() (Credential, error) {
		atomic.AddInt32(&count, 1)
		<-release // hold the mint open so concurrent callers pile onto singleflight
		return TokenCredential("new", time.Now().Add(24*time.Hour)), nil
	})
	gen0 := gen(s)

	const n = 16
	var wg sync.WaitGroup
	toks := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cred, err := s.Refresh(gen0)
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			toks[i] = cred.Token
		}(i)
	}
	// Let the goroutines enter Refresh, then release the single in-flight mint.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("mint count = %d, want 1 (coalesced)", got)
	}
	for i, tok := range toks {
		if tok != "new" {
			t.Errorf("goroutine %d got %q, want new", i, tok)
		}
	}
}

// TestRefreshDifferentGenerationsDoNotCoalesce proves a stale-generation caller
// cannot starve a current-generation mint. While a current-gen mint (prev==cur)
// is in flight, a stale caller (prev != cur) must return the current token
// immediately without blocking on — or coalescing with — that mint. (This fails
// against a constant singleflight key, which would coalesce the two.)
func TestRefreshDifferentGenerationsDoNotCoalesce(t *testing.T) {
	var count int32
	var once sync.Once
	mintStarted := make(chan struct{})
	release := make(chan struct{})
	s := New(TokenCredential("cur", time.Now().Add(time.Hour)), func() (Credential, error) {
		atomic.AddInt32(&count, 1)
		once.Do(func() { close(mintStarted) })
		<-release
		return TokenCredential("minted", time.Now().Add(24*time.Hour)), nil
	})
	gen0 := gen(s)

	curDone := make(chan string, 1)
	go func() { c, _ := s.Refresh(gen0); curDone <- c.Token }() // current gen → mints (blocks)
	<-mintStarted                                               // mint is in flight

	staleDone := make(chan string, 1)
	// A DIFFERENT (already-superseded) generation must not coalesce onto it.
	go func() { c, _ := s.Refresh(gen0 + 99); staleDone <- c.Token }()
	select {
	case tok := <-staleDone:
		if tok != "cur" {
			t.Errorf("stale caller got %q, want cur (no mint, no coalesce)", tok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale caller blocked on the in-flight current-gen mint — generations coalesced")
	}

	close(release)
	if tok := <-curDone; tok != "minted" {
		t.Errorf("current caller got %q, want minted", tok)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("mint count = %d, want 1", got)
	}
}

// TestEnsureFreshAndRefreshRace exercises proactive + reactive paths together
// under -race with no assertion beyond "no data race / no panic".
func TestEnsureFreshAndRefreshRace(t *testing.T) {
	var count int32
	s := New(TokenCredential("old", time.Now().Add(time.Hour)), countingRefresh(&count, time.Hour))
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				s.EnsureFresh()
			} else {
				_, _ = s.Refresh(gen(s))
			}
			_ = s.Token()
			_ = s.ExpiresAt()
		}(i)
	}
	wg.Wait()
}
