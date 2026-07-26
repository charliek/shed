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

	"github.com/charliek/shed/internal/clienttoken"
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
		m.bootstrapRun = func(_ context.Context, p sdkbootstrap.Params) (sdk.Credential, error) {
			if p.Host != host || p.Port != port || p.KnownHostsPath != khPath ||
				p.Scope != scopeCredentials || p.ClientKind != "host-agent" {
				t.Errorf("unexpected bootstrap params: %+v", p)
			}
			return tokenCredential("tok", exp), nil
		}
		got, err := m.Mint(context.Background(), target, scopeCredentials)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if got.Bundle.Token != "tok" || !got.Bundle.ExpiresAt.Equal(exp) {
			t.Errorf("Mint = %q, %v; want tok, %v", got.Bundle.Token, got.Bundle.ExpiresAt, exp)
		}
	})

	t.Run("a host-key mismatch propagates as terminal", func(t *testing.T) {
		khPath, target := writePinnedKnownHosts(t, host, port)
		m := NewCredentialMinter(khPath)
		m.bootstrapRun = func(context.Context, sdkbootstrap.Params) (sdk.Credential, error) {
			return sdk.Credential{}, fmt.Errorf("ssh: %w", sdkbootstrap.ErrHostKeyMismatch)
		}
		if _, err := m.Mint(context.Background(), target, scopeControl); !errors.Is(err, sdkbootstrap.ErrHostKeyMismatch) {
			t.Errorf("err = %v, want it to wrap ErrHostKeyMismatch", err)
		}
	})

	t.Run("a missing pin is non-terminal and never invokes ssh", func(t *testing.T) {
		khPath, _ := writePinnedKnownHosts(t, host, port)
		m := NewCredentialMinter(khPath)
		ran := false
		m.bootstrapRun = func(context.Context, sdkbootstrap.Params) (sdk.Credential, error) {
			ran = true
			return tokenCredential("x", time.Time{}), nil
		}
		// A different, unpinned server: the pre-check must fail before ssh runs.
		_, err := m.Mint(context.Background(), ServerTarget{Name: "s", SSHHost: "unpinned", SSHPort: 1}, scopeCredentials)
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
	m.bootstrapRun = func(context.Context, sdkbootstrap.Params) (sdk.Credential, error) {
		atomic.AddInt32(&calls, 1)
		return sdk.Credential{}, fmt.Errorf("ssh: %w", sdkbootstrap.ErrHostKeyMismatch)
	}
	s := newCredentialSource(context.Background(), m, target, scopeCredentials, nil, nil)
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
	cred sdk.Credential
	err  error
}

// tokenCredential/mtlsCredential build the two shapes a bootstrap can return, so
// a test names the SERVER's answer rather than assembling wire structs inline.
func tokenCredential(tok string, exp time.Time) sdk.Credential {
	return sdk.Credential{Bundle: sdk.Bundle{AuthMode: sdk.AuthModeToken, Token: tok, ExpiresAt: exp}}
}

func tokenMint(tok string, exp time.Time) mintResult {
	return mintResult{cred: tokenCredential(tok, exp)}
}

// fakeMinter returns canned results in sequence (repeating the last) and counts
// calls, so credentialSource can be tested without a live SSH server.
type fakeMinter struct {
	mu      sync.Mutex
	calls   int
	csrs    []string
	results []mintResult
}

func (f *fakeMinter) Mint(context.Context, ServerTarget, string) (sdk.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	f.calls++
	r := f.results[i]
	return r.cred, r.err
}

// MintRelayed lets the same fake stand in for the desktop relay path. The CSR is
// recorded so a test can assert it crossed unmodified; the canned answer is the
// same sequence Mint serves.
func (f *fakeMinter) MintRelayed(ctx context.Context, t ServerTarget, scope, csrBase64 string) (sdk.Bundle, error) {
	f.mu.Lock()
	f.csrs = append(f.csrs, csrBase64)
	f.mu.Unlock()
	cred, err := f.Mint(ctx, t, scope)
	return cred.Bundle, err
}

// relayedCSRs returns the CSRs MintRelayed was called with, in order.
func (f *fakeMinter) relayedCSRs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.csrs...)
}

func TestCredentialSourceCachesAndReMints(t *testing.T) {
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{tokenMint("tok1", far), tokenMint("tok2", far)}}
	s := newCredentialSource(context.Background(), fm, ServerTarget{Name: "s"}, scopeCredentials, nil, nil)

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

// TestCredentialSourceSurvivingTokenAfterFailedReMint pins whole-branch
// review finding 2, counting real MINT attempts (not Invalidate calls): a
// rejection whose replacement mint FAILS returns the surviving token
// alongside the error — the server might still accept it, and presenting it
// beats presenting nothing — and the next successful mint recovers.
func TestCredentialSourceSurvivingTokenAfterFailedReMint(t *testing.T) {
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{
		tokenMint("tok1", far),
		{err: fmt.Errorf("ssh down")}, // Invalidate's re-mint
		{err: fmt.Errorf("ssh down")}, // the next Token's forced re-attempt
		tokenMint("tok2", far),        // recovery
	}}
	s := newCredentialSource(context.Background(), fm, ServerTarget{Name: "s"}, scopeCredentials, nil, nil)

	if got, err := s.Token(); got != "tok1" || err != nil {
		t.Fatalf("Token = %q, %v; want tok1, nil", got, err)
	}
	s.Invalidate() // server rejected tok1; the replacement mint fails
	got, err := s.Token()
	if err == nil {
		t.Error("want the mint error surfaced while the mint keeps failing")
	}
	if got != "tok1" {
		t.Errorf("Token = %q, want the SURVIVING tok1 presented alongside the error", got)
	}
	got, err = s.Token() // the minter healed
	if err != nil || got != "tok2" {
		t.Errorf("Token = %q, %v; want tok2, nil (recovery clears the rejection)", got, err)
	}
	if fm.calls != 4 {
		t.Errorf("mint attempts = %d, want exactly 4 (initial, Invalidate, forced re-attempt, recovery)", fm.calls)
	}
	// Steady state after recovery: cached, no further mints.
	if _, err := s.Token(); err != nil {
		t.Errorf("post-recovery Token: %v", err)
	}
	if fm.calls != 4 {
		t.Errorf("mint attempts = %d after recovery, want still 4 (cache restored)", fm.calls)
	}
}

// TestCredentialSourceTerminalWithdrawsHeldCredential: the surviving-credential
// contract INVERTS on a latched host-key mismatch. A possible MITM is the one
// state where presenting anything is wrong — the held token and the armed
// certificate are both withdrawn, not just the re-mint suppressed. (Production
// commonly already holds a credential when the replacement mint goes terminal,
// which the empty-state terminal test cannot catch.)
func TestCredentialSourceTerminalWithdrawsHeldCredential(t *testing.T) {
	near := time.Now().Add(clienttoken.RefreshWindow / 2) // forces the next Token to re-mint
	fm := &fakeMinter{results: []mintResult{
		tokenMint("tok1", near),
		{err: fmt.Errorf("bootstrap: %w", sdkbootstrap.ErrHostKeyMismatch)},
	}}
	s := newCredentialSource(context.Background(), fm, ServerTarget{Name: "s"}, scopeCredentials, nil, nil)

	if got, err := s.Token(); got != "tok1" || err != nil {
		t.Fatalf("Token = %q, %v; want tok1, nil", got, err)
	}
	got, err := s.Token() // near expiry → re-mint → host-key mismatch → terminal
	if err == nil {
		t.Fatal("want the terminal error surfaced")
	}
	if got != "" {
		t.Errorf("Token = %q, want empty — a possible MITM withdraws the held credential", got)
	}
	if s.ClientCertificate() != nil {
		t.Error("ClientCertificate must present nothing in the terminal state")
	}
}

func TestCredentialSourcePinMismatchTerminal(t *testing.T) {
	fm := &fakeMinter{results: []mintResult{{err: fmt.Errorf("bootstrap: %w", sdkbootstrap.ErrHostKeyMismatch)}}}
	s := newCredentialSource(context.Background(), fm, ServerTarget{Name: "s"}, scopeCredentials, nil, nil)

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
	near := time.Now().Add(clienttoken.RefreshWindow / 2) // inside the refresh window
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{tokenMint("near", near), tokenMint("fresh", far)}}
	s := newCredentialSource(context.Background(), fm, ServerTarget{Name: "s"}, scopeCredentials, nil, nil)

	if got, _ := s.Token(); got != "near" {
		t.Fatalf("Token = %q, want near", got)
	}
	// The cached token is within clienttoken.RefreshWindow of expiry → the next Token re-mints.
	if got, _ := s.Token(); got != "fresh" {
		t.Errorf("Token = %q, want fresh (near-expiry re-mint)", got)
	}
	if fm.calls != 2 {
		t.Errorf("mint calls = %d, want 2", fm.calls)
	}
}

// minterFunc adapts a function to the minter interface.
type minterFunc func(context.Context, ServerTarget, string) (sdk.Credential, error)

func (f minterFunc) Mint(ctx context.Context, t ServerTarget, scope string) (sdk.Credential, error) {
	return f(ctx, t, scope)
}

// MintRelayed lets the same function stand in for the desktop relay path; the
// CSR is ignored, which is what a token-mode server does with it too.
func (f minterFunc) MintRelayed(ctx context.Context, t ServerTarget, scope, _ string) (sdk.Bundle, error) {
	cred, err := f(ctx, t, scope)
	return cred.Bundle, err
}

func TestCredentialSourceSingleFlight(t *testing.T) {
	release := make(chan struct{})
	var calls int32
	mf := minterFunc(func(context.Context, ServerTarget, string) (sdk.Credential, error) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the mint open while concurrent callers pile up
		return tokenCredential("tok", time.Now().Add(24*time.Hour)), nil
	})
	s := newCredentialSource(context.Background(), mf, ServerTarget{Name: "s"}, scopeCredentials, nil, nil)

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
	fm := &fakeMinter{results: []mintResult{tokenMint("first", far), tokenMint("second", far)}}
	s := newCredentialSource(context.Background(), fm, ServerTarget{Name: "s"}, scopeCredentials, nil, nil)

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
