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
		{"exactly at the window edge refreshes", now.Add(tokenRefreshWindow), true},
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
func countingRefresh(count *int32, ttl time.Duration) func() (string, time.Time, error) {
	return func() (string, time.Time, error) {
		n := atomic.AddInt32(count, 1)
		return "tok" + strconv.Itoa(int(n)), time.Now().Add(ttl), nil
	}
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
	tok, err := s.Refresh("static-tok")
	if !errors.Is(err, ErrStatic) {
		t.Errorf("Refresh on static: err = %v, want ErrStatic", err)
	}
	if tok != "" && tok != "static-tok" {
		t.Errorf("Refresh on static returned unexpected token %q", tok)
	}
}

func TestEnsureFreshWithinWindowMintsOnce(t *testing.T) {
	var count int32
	// Expiry inside the window ⇒ EnsureFresh should mint.
	s := New("old", time.Now().Add(time.Hour), countingRefresh(&count, 24*time.Hour))
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
	s := New("old", time.Now().Add(24*time.Hour), countingRefresh(&count, 24*time.Hour))
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
	s := New("old", time.Time{}, countingRefresh(&count, 24*time.Hour))
	s.EnsureFresh()
	if got := atomic.LoadInt32(&count); got != 0 {
		t.Errorf("mint count = %d, want 0 (zero expiry)", got)
	}
}

func TestRefreshMintsAndUpdatesExpiry(t *testing.T) {
	var count int32
	exp := time.Now().Add(24 * time.Hour)
	s := New("old", time.Now().Add(time.Hour), func() (string, time.Time, error) {
		atomic.AddInt32(&count, 1)
		return "new", exp, nil
	})
	tok, err := s.Refresh("old")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok != "new" || s.Token() != "new" {
		t.Errorf("token = %q / %q, want new", tok, s.Token())
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
	s := New("old", time.Now().Add(time.Hour), countingRefresh(&count, 24*time.Hour))
	// First 401 with prev=old ⇒ mints (token becomes tok1).
	if _, err := s.Refresh("old"); err != nil {
		t.Fatal(err)
	}
	// A second, stale 401 that still thinks the token is "old" must NOT re-mint;
	// it should observe the already-advanced token.
	tok, err := s.Refresh("old")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok1" {
		t.Errorf("stale Refresh returned %q, want the current tok1", tok)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("mint count = %d, want 1 (generation-aware, no double mint)", got)
	}
}

func TestRefreshError(t *testing.T) {
	sentinel := errors.New("mint boom")
	s := New("old", time.Now().Add(time.Hour), func() (string, time.Time, error) {
		return "", time.Time{}, sentinel
	})
	if _, err := s.Refresh("old"); !errors.Is(err, sentinel) {
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
	s := New("old", time.Now().Add(time.Hour), func() (string, time.Time, error) {
		atomic.AddInt32(&count, 1)
		<-release // hold the mint open so concurrent callers pile onto singleflight
		return "new", time.Now().Add(24 * time.Hour), nil
	})

	const n = 16
	var wg sync.WaitGroup
	toks := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := s.Refresh("old")
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			toks[i] = tok
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
	s := New("cur", time.Now().Add(time.Hour), func() (string, time.Time, error) {
		atomic.AddInt32(&count, 1)
		once.Do(func() { close(mintStarted) })
		<-release
		return "minted", time.Now().Add(24 * time.Hour), nil
	})

	curDone := make(chan string, 1)
	go func() { tok, _ := s.Refresh("cur"); curDone <- tok }() // current gen → mints (blocks)
	<-mintStarted                                              // mint is in flight

	staleDone := make(chan string, 1)
	go func() { tok, _ := s.Refresh("stale"); staleDone <- tok }() // different gen → must not coalesce
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
	s := New("old", time.Now().Add(time.Hour), countingRefresh(&count, time.Hour))
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				s.EnsureFresh()
			} else {
				_, _ = s.Refresh(s.Token())
			}
			_ = s.Token()
			_ = s.ExpiresAt()
		}(i)
	}
	wg.Wait()
}
