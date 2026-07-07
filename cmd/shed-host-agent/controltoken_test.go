package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeShedConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

const prodSecureShedConfig = `
servers:
  prod:
    api_url: https://prod.example:8443
    host: prod.example
    ssh_port: 2222
`

// Every token.get mints a FRESH control token — the per-server source's completed
// cache is never served. A token cached here can be silently invalidated by the
// target server restarting (which regenerates its token authority) with no signal
// to the agent; serving the cached copy is what wedged a restarted secure server
// at 401 in the desktop until the host-agent itself was restarted.
func TestControlTokenProviderAlwaysMintsFresh(t *testing.T) {
	cfg := writeShedConfig(t, prodSecureShedConfig)
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{{tok: "ctl-1", exp: far}, {tok: "ctl-2", exp: far}}}
	p := newControlTokenProvider(context.Background(), fm, cfg)

	tok, exp, err := p.Token("prod")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ctl-1" {
		t.Errorf("token = %q, want ctl-1", tok)
	}
	if !exp.Equal(far) {
		t.Errorf("expiry = %v, want %v", exp, far)
	}
	// A second sequential call re-mints — no completed-cache short-circuit.
	tok2, _, err := p.Token("prod")
	if err != nil {
		t.Fatalf("Token (second): %v", err)
	}
	if tok2 != "ctl-2" {
		t.Errorf("second token = %q, want ctl-2 (fresh mint, not cache)", tok2)
	}
	if fm.calls != 2 {
		t.Errorf("mint calls = %d, want 2 (every call mints fresh)", fm.calls)
	}
}

// After the target server restarts — its old token now 401s — the next token.get
// returns a freshly minted token rather than wedging on the stale cached one.
func TestControlTokenProviderRefreshesAfterServerRestart(t *testing.T) {
	cfg := writeShedConfig(t, prodSecureShedConfig)
	far := time.Now().Add(24 * time.Hour)
	fm := &fakeMinter{results: []mintResult{{tok: "before-restart", exp: far}, {tok: "after-restart", exp: far}}}
	p := newControlTokenProvider(context.Background(), fm, cfg)

	if tok, _, err := p.Token("prod"); err != nil || tok != "before-restart" {
		t.Fatalf("first Token = %q, %v; want before-restart", tok, err)
	}
	// Server restarts; the minter now yields the post-restart token.
	if tok, _, err := p.Token("prod"); err != nil || tok != "after-restart" {
		t.Errorf("post-restart Token = %q, %v; want after-restart", tok, err)
	}
}

// Concurrent token.get calls that overlap a single in-flight mint still collapse
// to ONE mint (single-flight preserved) and all receive the same fresh token.
// This asserts overlap-coalescing + completed-cache-bypass — NOT elimination of
// in-flight staleness (a caller that joins a mint across a restart instant can be
// stale once; that self-heals and is out of scope for this assertion).
func TestControlTokenProviderConcurrentOverlapSingleFlight(t *testing.T) {
	cfg := writeShedConfig(t, prodSecureShedConfig)
	started := make(chan struct{}) // closed once the single mint is actually in flight
	release := make(chan struct{}) // held open so the other callers pile onto that mint
	var calls int32
	mf := minterFunc(func(context.Context, ServerTarget, string) (string, time.Time, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(started) // first (and only, if single-flight holds) mint has begun
		}
		<-release
		return "tok", time.Now().Add(24 * time.Hour), nil
	})
	p := newControlTokenProvider(context.Background(), mf, cfg)

	const n = 8
	var wg sync.WaitGroup
	got := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _, _ = p.Token("prod")
		}(i)
	}
	// Gate on the mint actually being in flight (not a wall-clock guess), THEN
	// give the other callers a window to join it before releasing — so the gate
	// can't close before the single mint even starts.
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Errorf("mint calls = %d, want 1 (overlapping callers share one mint)", c)
	}
	for i, tok := range got {
		if tok != "tok" {
			t.Errorf("got[%d] = %q, want tok", i, tok)
		}
	}
}

func TestControlTokenProviderErrors(t *testing.T) {
	cfg := writeShedConfig(t, `
servers:
  open-no-ssh:
    host: open1.example
    http_port: 8080
  open-http:
    host: open2.example
    http_port: 8080
    ssh_port: 2222
`)
	fm := &fakeMinter{results: []mintResult{{tok: "x", exp: time.Now().Add(time.Hour)}}}
	p := newControlTokenProvider(context.Background(), fm, cfg)

	// All three error before any mint. "open-http" is the case the new https gate
	// adds: it has an ssh endpoint, so the old SSHPort>0 gate would have let it
	// through and attempted a doomed mint.
	cases := []struct {
		name, server string
	}{
		{"unknown server", "missing"},
		{"no ssh endpoint", "open-no-ssh"},
		{"open http with ssh endpoint", "open-http"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := p.Token(tc.server); err == nil {
				t.Errorf("expected an error for server %q", tc.server)
			}
		})
	}
	if fm.calls != 0 {
		t.Errorf("mint calls = %d, want 0 (all error before minting)", fm.calls)
	}
}
