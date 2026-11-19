package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/directory/refserver"
)

// TestRefusesNonLoopback checks the command listens on a loopback address
// only.
func TestRefusesNonLoopback(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"0.0.0.0:0", "[::]:0", ":0", "192.0.2.1:0", "localhost:0", "accounts.example.test:0", "127.0.0.1"} {
		// A server that wrongly starts serves until this context ends and
		// exits 0, which fails the case rather than hanging it.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		stderr := &lockedBuffer{}
		code := run(ctx, []string{"--control", dir, "--listen", addr}, io.Discard, stderr)
		cancel()
		if code == 0 {
			t.Errorf("--listen %s: exit 0", addr)
		}
		if !strings.Contains(stderr.String(), refserver.Banner) {
			t.Errorf("--listen %s: the banner was not printed", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "127.0.0.2:0", "[::1]:0"} {
		if err := checkLoopback(addr); err != nil {
			t.Errorf("%s refused: %v", addr, err)
		}
	}
}

// TestRefusesBadControlDir checks a control directory others can write, or
// none, stops the start.
func TestRefusesBadControlDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--control", dir}, {"--control", filepath.Join(dir, "none")}, {}} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		code := run(ctx, args, io.Discard, io.Discard)
		cancel()
		if code == 0 {
			t.Errorf("%v: exit 0", args)
		}
	}
}

// TestBannerOnEveryInvocation checks the banner is the first line of every
// invocation, a flag error and a request for help included, and that the
// help text states the ledger's behaviour after the clock moves back.
func TestBannerOnEveryInvocation(t *testing.T) {
	for _, args := range [][]string{{"--no-such-flag"}, {"-h"}, {"--listen"}, {}, {"--control"}, {"extra"}} {
		stderr := &lockedBuffer{}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		code := run(ctx, args, io.Discard, stderr)
		cancel()
		if code == 0 {
			t.Errorf("%v: exit 0", args)
		}
		if first, _, _ := strings.Cut(stderr.String(), "\n"); first != refserver.Banner {
			t.Errorf("%v: the first line is %q, not the banner", args, first)
		}
	}
	stderr := &lockedBuffer{}
	run(context.Background(), []string{"-h"}, io.Discard, stderr)
	if !strings.Contains(strings.Join(strings.Fields(stderr.String()), " "), "After the clock moves back, an assertion whose acceptance window ended before the latest time the server has seen is refused until the clock catches up.") {
		t.Errorf("the help text does not state the ledger's behaviour:\n%s", stderr.String())
	}
}

// lockedBuffer is a buffer the server's goroutines and the test share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestServes starts the command on loopback, with the fixture signing key,
// reads the issuer it prints, fetches its key set and stops it.
func TestServes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join("..", "..", "..", "..", "..", "tests", "directory-contract", "v1", "fixtures", "test-keys", "token-signing-key.pem")
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--control", dir, "--signing-key", key, "--key-id", "test-signing-1"}, stdout, stderr)
	}()
	var issuer string
	deadline := time.Now().Add(10 * time.Second)
	for issuer == "" && time.Now().Before(deadline) {
		sc := bufio.NewScanner(strings.NewReader(stdout.String()))
		for sc.Scan() {
			if u, ok := strings.CutPrefix(sc.Text(), "listening on "); ok {
				issuer = u
			}
		}
		select {
		case code := <-done:
			t.Fatalf("exited %d: %s", code, stderr.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !strings.HasPrefix(issuer, "http://127.0.0.1:") {
		t.Fatalf("issuer %q; stderr %s", issuer, stderr.String())
	}
	res, err := http.Get(issuer + "/.well-known/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"kid":"test-signing-1"`) {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop")
	}
	if !strings.Contains(stderr.String(), refserver.Banner) {
		t.Error("the banner was not printed")
	}
}
