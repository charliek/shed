package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/charliek/shed/sdk"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// testHostKey generates a fresh ed25519 SSH public key for use in known_hosts pins.
func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestKnownHostsPinned(t *testing.T) {
	dir := t.TempDir()
	hostPub := testHostKey(t)
	const host, port = "mini3", 2222

	// Write a known_hosts line exactly as OpenSSH/shed would, keyed by [host]:port.
	addr := knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))
	khPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(khPath, []byte(knownhosts.Line([]string{addr}, hostPub)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := knownHostsPinned(khPath, host, port); err != nil {
		t.Errorf("knownHostsPinned on a pinned host: %v", err)
	}
}

func TestKnownHostsPinnedErrors(t *testing.T) {
	dir := t.TempDir()
	hostPub := testHostKey(t)

	// A known_hosts that pins a different host than the one we query.
	khPath := filepath.Join(dir, "known_hosts")
	addr := knownhosts.Normalize(net.JoinHostPort("other", "2222"))
	if err := os.WriteFile(khPath, []byte(knownhosts.Line([]string{addr}, hostPub)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := knownHostsPinned(filepath.Join(dir, "absent"), "mini3", 2222); err == nil {
		t.Error("expected an error for a missing known_hosts file")
	}
	if err := knownHostsPinned(khPath, "mini3", 2222); err == nil {
		t.Error("expected an error when the host has no pinned key")
	}
}

func TestKnownHostsPinnedSkipsRevoked(t *testing.T) {
	dir := t.TempDir()
	hostPub := testHostKey(t)
	const host, port = "mini3", 2222
	addr := knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))

	// A @revoked line for the exact host must NOT count as a usable pin.
	khPath := filepath.Join(dir, "known_hosts")
	line := "@revoked " + knownhosts.Line([]string{addr}, hostPub) + "\n"
	if err := os.WriteFile(khPath, []byte(line), 0644); err != nil {
		t.Fatal(err)
	}
	if err := knownHostsPinned(khPath, host, port); err == nil {
		t.Error("a @revoked host key must not count as a pin")
	}
}

// writePinnedKnownHosts writes a known_hosts pinning a fresh host key for
// host:port and returns the file path plus the matching ServerTarget.
func writePinnedKnownHosts(t *testing.T, host string, port int) (string, ServerTarget) {
	t.Helper()
	dir := t.TempDir()
	hostPub := testHostKey(t)
	addr := knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))
	khPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(khPath, []byte(knownhosts.Line([]string{addr}, hostPub)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return khPath, ServerTarget{Name: "s", SSHHost: host, SSHPort: port}
}

func TestCredentialMinterMint(t *testing.T) {
	const host, port = "mini3", 2222

	t.Run("success passes params and returns the token", func(t *testing.T) {
		khPath, target := writePinnedKnownHosts(t, host, port)
		m := NewCredentialMinter(khPath)
		exp := time.Now().Add(time.Hour)
		m.bootstrapRun = func(_ context.Context, p sdkbootstrap.Params) (sdk.Bundle, error) {
			if p.Host != host || p.Port != port || p.KnownHostsPath != khPath ||
				p.Scope != scopeCredentials || p.ClientKind != "host-agent" {
				t.Errorf("unexpected bootstrap params: %+v", p)
			}
			return sdk.Bundle{Token: "tok", ExpiresAt: exp}, nil
		}
		tok, gotExp, err := m.Mint(context.Background(), target, scopeCredentials)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if tok != "tok" || !gotExp.Equal(exp) {
			t.Errorf("Mint = %q, %v; want tok, %v", tok, gotExp, exp)
		}
	})

	t.Run("a host-key mismatch propagates as terminal", func(t *testing.T) {
		khPath, target := writePinnedKnownHosts(t, host, port)
		m := NewCredentialMinter(khPath)
		m.bootstrapRun = func(context.Context, sdkbootstrap.Params) (sdk.Bundle, error) {
			return sdk.Bundle{}, fmt.Errorf("ssh: %w", sdkbootstrap.ErrHostKeyMismatch)
		}
		if _, _, err := m.Mint(context.Background(), target, scopeControl); !errors.Is(err, sdkbootstrap.ErrHostKeyMismatch) {
			t.Errorf("err = %v, want it to wrap ErrHostKeyMismatch", err)
		}
	})

	t.Run("a missing pin is non-terminal and never invokes ssh", func(t *testing.T) {
		khPath, _ := writePinnedKnownHosts(t, host, port)
		m := NewCredentialMinter(khPath)
		ran := false
		m.bootstrapRun = func(context.Context, sdkbootstrap.Params) (sdk.Bundle, error) {
			ran = true
			return sdk.Bundle{Token: "x"}, nil
		}
		// A different, unpinned server: the pre-check must fail before ssh runs.
		_, _, err := m.Mint(context.Background(), ServerTarget{Name: "s", SSHHost: "unpinned", SSHPort: 1}, scopeCredentials)
		if err == nil {
			t.Fatal("expected an error for an unpinned server")
		}
		if errors.Is(err, sdkbootstrap.ErrHostKeyMismatch) {
			t.Error("a missing pin must not be a terminal mismatch")
		}
		if ran {
			t.Error("bootstrapRun must not run when the server is not pinned")
		}
	})
}

// TestCredentialSourceMinterMismatchTerminal exercises the full chain: a real
// CredentialMinter whose ssh exchange reports a host-key change must latch the
// credentialSource terminal (no retry). This closes the gap that
// TestCredentialSourcePinMismatchTerminal only covers via a fake minter.
func TestCredentialSourceMinterMismatchTerminal(t *testing.T) {
	khPath, target := writePinnedKnownHosts(t, "mini3", 2222)
	m := NewCredentialMinter(khPath)
	var calls int32
	m.bootstrapRun = func(context.Context, sdkbootstrap.Params) (sdk.Bundle, error) {
		atomic.AddInt32(&calls, 1)
		return sdk.Bundle{}, fmt.Errorf("ssh: %w", sdkbootstrap.ErrHostKeyMismatch)
	}
	s := newCredentialSource(context.Background(), m, target, scopeCredentials)
	if _, err := s.Token(); err == nil {
		t.Fatal("expected a terminal error on a host-key mismatch")
	}
	if _, err := s.Token(); err == nil {
		t.Error("the terminal error must persist (no re-mint)")
	}
	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Errorf("bootstrapRun calls = %d, want 1 (a mismatch must never be retried)", c)
	}
}

type mintResult struct {
	tok string
	exp time.Time
	err error
}

// fakeMinter returns canned results in sequence (repeating the last) and counts
// calls, so credentialSource can be tested without a live SSH server.
type fakeMinter struct {
	mu      sync.Mutex
	calls   int
	results []mintResult
}

func (f *fakeMinter) Mint(context.Context, ServerTarget, string) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	f.calls++
	r := f.results[i]
	return r.tok, r.exp, r.err
}

func TestCredentialSourceCachesAndReMints(t *testing.T) {
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{{tok: "tok1", exp: far}, {tok: "tok2", exp: far}}}
	s := newCredentialSource(context.Background(), fm, ServerTarget{Name: "s"}, scopeCredentials)

	if got, _ := s.Token(); got != "tok1" {
		t.Fatalf("Token = %q, want tok1", got)
	}
	if got, _ := s.Token(); got != "tok1" {
		t.Errorf("cached Token = %q, want tok1", got)
	}
	if fm.calls != 1 {
		t.Errorf("mint calls = %d, want 1 (second Token served from cache)", fm.calls)
	}
	s.Invalidate()
	if got, _ := s.Token(); got != "tok2" {
		t.Errorf("post-invalidate Token = %q, want tok2 (re-mint)", got)
	}
	if fm.calls != 2 {
		t.Errorf("mint calls = %d, want 2", fm.calls)
	}
}

func TestCredentialSourcePinMismatchTerminal(t *testing.T) {
	fm := &fakeMinter{results: []mintResult{{err: fmt.Errorf("bootstrap: %w", sdkbootstrap.ErrHostKeyMismatch)}}}
	s := newCredentialSource(context.Background(), fm, ServerTarget{Name: "s"}, scopeCredentials)

	if _, err := s.Token(); err == nil {
		t.Fatal("expected a terminal error on a host-key pin mismatch")
	}
	// A pin mismatch is terminal: the second call fails closed WITHOUT re-minting.
	if _, err := s.Token(); err == nil {
		t.Error("expected the terminal error to persist")
	}
	if fm.calls != 1 {
		t.Errorf("mint calls = %d, want 1 (a pin mismatch must never be retried)", fm.calls)
	}
}

func TestCredentialSourceReMintsNearExpiry(t *testing.T) {
	near := time.Now().Add(tokenRefreshWindow / 2) // inside the refresh window
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{{tok: "near", exp: near}, {tok: "fresh", exp: far}}}
	s := newCredentialSource(context.Background(), fm, ServerTarget{Name: "s"}, scopeCredentials)

	if got, _ := s.Token(); got != "near" {
		t.Fatalf("Token = %q, want near", got)
	}
	// The cached token is within tokenRefreshWindow of expiry → the next Token re-mints.
	if got, _ := s.Token(); got != "fresh" {
		t.Errorf("Token = %q, want fresh (near-expiry re-mint)", got)
	}
	if fm.calls != 2 {
		t.Errorf("mint calls = %d, want 2", fm.calls)
	}
}

// minterFunc adapts a function to the minter interface.
type minterFunc func(context.Context, ServerTarget, string) (string, time.Time, error)

func (f minterFunc) Mint(ctx context.Context, t ServerTarget, scope string) (string, time.Time, error) {
	return f(ctx, t, scope)
}

func TestCredentialSourceSingleFlight(t *testing.T) {
	release := make(chan struct{})
	var calls int32
	mf := minterFunc(func(context.Context, ServerTarget, string) (string, time.Time, error) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the mint open while concurrent callers pile up
		return "tok", time.Now().Add(24 * time.Hour), nil
	})
	s := newCredentialSource(context.Background(), mf, ServerTarget{Name: "s"}, scopeCredentials)

	const n = 8
	var wg sync.WaitGroup
	got := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _ = s.Token()
		}(i)
	}
	time.Sleep(50 * time.Millisecond) // let all n goroutines join the single mint
	close(release)
	wg.Wait()

	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Errorf("mint calls = %d, want 1 (single-flight collapses concurrent callers)", c)
	}
	for i, tok := range got {
		if tok != "tok" {
			t.Errorf("got[%d] = %q, want tok", i, tok)
		}
	}
}

func TestCredentialSourceProactiveRefresh(t *testing.T) {
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{{tok: "first", exp: far}, {tok: "second", exp: far}}}
	s := newCredentialSource(context.Background(), fm, ServerTarget{Name: "s"}, scopeCredentials)

	if tok, _ := s.Token(); tok != "first" {
		t.Fatalf("Token = %q, want first", tok)
	}
	s.refresh() // proactive re-mint, even though the cached token is still valid
	if fm.calls != 2 {
		t.Errorf("mint calls = %d, want 2 (proactive refresh re-mints)", fm.calls)
	}
	if tok, _ := s.Token(); tok != "second" {
		t.Errorf("post-refresh Token = %q, want second", tok)
	}
}
