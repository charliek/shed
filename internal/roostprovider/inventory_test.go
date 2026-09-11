package roostprovider

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

// captureStderr redirects os.Stderr for the duration of fn and returns
// everything written to it. This is the local twin of
// internal/config's captureStderr (auth_mode_test.go) — that helper is
// unexported and lives in a different package, so it can't be imported
// across the package boundary; this copy keeps the two decoupled rather
// than reaching into config's internals.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

// generateSelfSignedCert returns a fresh self-signed certificate and its
// matching private key, PEM encoded, for use as a client certificate in an
// mtls test. It is self-signed rather than CA-issued because the mtls
// success test's server uses tls.RequireAnyClientCert (accepts any
// certificate, verifies no chain) — the point of that test is that a
// certificate got PRESENTED at all, not that shed's own CA issuance path was
// exercised (that path is covered elsewhere).
func generateSelfSignedCert(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// shedsHandler serves a fixed config.ShedsResponse as GET /api/sheds and
// counts how many times it was hit, so a test can assert a credential-less
// entry never dialed at all.
func shedsHandler(t *testing.T, sheds ...config.Shed) (http.HandlerFunc, *int32) {
	t.Helper()
	var hits int32
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/api/sheds" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(config.ShedsResponse{Sheds: sheds})
	}, &hits
}

// openEntry builds a plain-HTTP (unauthenticated) server entry pointed at
// ts, matching what `shed server add` writes for an open server.
func openEntry(ts *httptest.Server) config.ServerEntry {
	return config.ServerEntry{
		Host:    "127.0.0.1",
		APIURL:  ts.URL,
		SSHPort: 2222,
	}
}

func TestList_RunningShedsOnly(t *testing.T) {
	handler, _ := shedsHandler(t,
		config.Shed{Name: "a", Status: config.StatusRunning, LandingDir: "/home/shed/proj"},
		config.Shed{Name: "b", Status: "stopped"},
	)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	got := List(context.Background(), map[string]config.ServerEntry{"srv": openEntry(ts)}, 0)
	if len(got) != 1 {
		t.Fatalf("want 1 running shed, got %#v", got)
	}
	want := RunningShed{Name: "a", Server: "srv", ServerHost: "127.0.0.1", ServerSSHPort: 2222, LandingDir: "/home/shed/proj"}
	if got[0] != want {
		t.Fatalf("got %#v, want %#v", got[0], want)
	}
}

func TestList_BearerTokenIsSent(t *testing.T) {
	var gotAuth string
	handler, _ := shedsHandler(t, config.Shed{Name: "a", Status: config.StatusRunning})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		handler(w, r)
	}))
	defer ts.Close()

	entry := openEntry(ts)
	entry.ControlToken = "shed_control_abc123"

	got := List(context.Background(), map[string]config.ServerEntry{"srv": entry}, 0)
	if len(got) != 1 {
		t.Fatalf("want 1 running shed, got %#v", got)
	}
	if gotAuth != "Bearer shed_control_abc123" {
		t.Fatalf("Authorization header = %q, want the stored token", gotAuth)
	}
}

// TestList_ServerErrorIsSkippedSilently pins BOTH halves of "skipped
// silently": the row is dropped, AND nothing is written to stderr. A version
// of this test that only checked the former would keep passing if the code
// started logging one line per unreachable server — exactly the noise the
// doc comment on List says roost's palette (no stderr a human is watching)
// can't afford.
func TestList_ServerErrorIsSkippedSilently(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	var got []RunningShed
	stderr := captureStderr(t, func() {
		got = List(context.Background(), map[string]config.ServerEntry{"srv": openEntry(ts)}, 0)
	})
	if len(got) != 0 {
		t.Fatalf("a 500 must be skipped, got %#v", got)
	}
	if stderr != "" {
		t.Fatalf("a per-server failure must be silent, got stderr %q", stderr)
	}
}

// TestList_HangingServerIsBoundedByTimeout is the injectable-timeout seam
// the plan calls for: rather than sleeping the production 2s default, it
// passes a tiny timeout so the hanging-server cases stay fast while still
// proving the bound is enforced end-to-end.
func TestList_HangingServerIsBoundedByTimeout(t *testing.T) {
	t.Run("before headers", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer ts.Close()

		start := time.Now()
		got := List(context.Background(), map[string]config.ServerEntry{"srv": openEntry(ts)}, 30*time.Millisecond)
		elapsed := time.Since(start)

		if len(got) != 0 {
			t.Fatalf("a hung server must be skipped, got %#v", got)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("List took %s; the per-server timeout should have bounded it well under that", elapsed)
		}
	})

	// "before headers" alone would still pass a fetchSheds that only bounded
	// the dial/handshake/header wait and then handed res.Body to an
	// unbounded read — this is what proves the SAME per-server timeout also
	// covers a response that starts fine (200, headers flushed) and then
	// stalls partway through the body, per fix 1/2's "the bound covers the
	// decode, not just the connect" requirement.
	t.Run("mid body", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"sheds":[`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		}))
		defer ts.Close()

		start := time.Now()
		got := List(context.Background(), map[string]config.ServerEntry{"srv": openEntry(ts)}, 30*time.Millisecond)
		elapsed := time.Since(start)

		if len(got) != 0 {
			t.Fatalf("a server stalled mid-body must be skipped, got %#v", got)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("List took %s; the per-server timeout should have bounded the body read too", elapsed)
		}
	})
}

// TestList_ReturnsPromptlyWhenParentContextIsCancelled covers fix 1: List
// must return the moment its parent ctx is done, not wait on every worker.
// stashedCredential's file reads (config.LoadClientCredentials) are not
// context-aware at all, so a ClientCertFile/ClientKeyFile pointing at
// something that blocks on open/read forever — here, a FIFO nothing ever
// writes to, standing in for the "a stalled network mount" case named in the
// finding — blocks that worker before any per-server timeout has a chance
// to apply. Paired with an HTTP-handler-blocks entry too (belt and braces:
// the ordinary hung-server case must also not stall the return), and a
// parent ctx cancelled shortly after the call starts.
func TestList_ReturnsPromptlyWhenParentContextIsCancelled(t *testing.T) {
	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "cert.fifo")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	// The worker blocked reading the FIFO is abandoned when List returns
	// early (that's the whole point of the fix) — unblock it here so the
	// goroutine can actually exit instead of blocking for the rest of the
	// test binary's life.
	t.Cleanup(func() {
		if w, err := os.OpenFile(fifoPath, os.O_WRONLY, 0); err == nil {
			_ = w.Close()
		}
	})

	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer blocked.Close()

	fifoEntry := config.ServerEntry{
		Host:           "127.0.0.1",
		APIURL:         blocked.URL,
		SSHPort:        2222,
		AuthMode:       config.AuthModeMTLS,
		ClientCertFile: fifoPath, // os.ReadFile on this blocks forever: no writer, no context
		ClientKeyFile:  fifoPath,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	got := List(ctx, map[string]config.ServerEntry{
		"stuck-credential": fifoEntry,
		"stuck-http":       openEntry(blocked),
	}, 5*time.Second)
	elapsed := time.Since(start)

	if len(got) != 0 {
		t.Fatalf("no server ever answered, got %#v", got)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("List took %s; a cancelled parent ctx should have returned it well under that", elapsed)
	}
}

// TestFetchSheds_BodyExceedingLimitIsRejected covers fix 2: the per-server
// timeout bounds time, not bytes, so a fast server streaming a body past
// maxShedsResponseBody must fail rather than being silently truncated into a
// half-decoded list.
func TestFetchSheds_BodyExceedingLimitIsRejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A syntactically well-formed (if it were ever fully written)
		// ShedsResponse, padded past the limit with a giant name field —
		// proves the rejection is a genuine size check, not just "large
		// bodies happen to produce invalid JSON when cut off".
		_, _ = w.Write([]byte(`{"sheds":[{"name":"`))
		_, _ = io.CopyN(w, zeroReader{}, maxShedsResponseBody+1)
		_, _ = w.Write([]byte(`","status":"running"}]}`))
	}))
	defer ts.Close()

	if _, err := fetchSheds(context.Background(), openEntry(ts), 5*time.Second); err == nil {
		t.Fatal("expected an error for a response exceeding the byte limit, got nil")
	}
}

// zeroReader streams an endless run of 'x' bytes, for padding a test body
// past maxShedsResponseBody without holding the whole padding in memory.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestList_FingerprintOverPlaintextIsSkipped covers fix 3: an entry with a
// stored TLS fingerprint (and therefore a stored, real credential) but a
// plaintext (http://) base URL must never be dialed — sending that
// credential would leak it in cleartext, and the pinned transport's
// certificate check never even runs against an http:// URL.
func TestList_FingerprintOverPlaintextIsSkipped(t *testing.T) {
	handler, hits := shedsHandler(t, config.Shed{Name: "a", Status: config.StatusRunning})
	ts := httptest.NewServer(http.HandlerFunc(handler))
	defer ts.Close()

	entry := config.ServerEntry{
		Host:               "127.0.0.1",
		APIURL:             ts.URL, // deliberately http://, not https://
		SSHPort:            2222,
		ControlToken:       "shed_control_abc123",
		TLSCertFingerprint: "sha256:deadbeef",
	}

	got := List(context.Background(), map[string]config.ServerEntry{"srv": entry}, 0)
	if len(got) != 0 {
		t.Fatalf("a fingerprinted entry over plaintext must be skipped, got %#v", got)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("expected zero requests when refusing to send a credential over plaintext, got %d", n)
	}
}

// TestList_MTLSEntrySendsClientCertificateAndSucceeds is the real mtls
// success test the package was missing entirely: every other mtls-shaped
// test here stops before ever dialing (no cert files recorded), so a
// regression that made src.CertificateFor a no-op (returning nil, e.g. a
// typo'd condition in stashedCredential or fetchSheds) would leave every
// prior test green. This one runs a TLS server that REQUIRES a client
// certificate and asserts the server actually saw one, on the security-
// critical path.
func TestList_MTLSEntrySendsClientCertificateAndSucceeds(t *testing.T) {
	var sawClientCert atomic.Bool
	handler, hits := shedsHandler(t, config.Shed{Name: "a", Status: config.StatusRunning})
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			sawClientCert.Store(true)
		}
		handler(w, r)
	}))
	ts.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	ts.StartTLS()
	defer ts.Close()

	certPEM, keyPEM := generateSelfSignedCert(t, "roostprovider-test-client")
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write client cert: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}

	entry := config.ServerEntry{
		Host:               "127.0.0.1",
		APIURL:             ts.URL,
		SSHPort:            2222,
		AuthMode:           config.AuthModeMTLS,
		TLSCertFingerprint: servertls.Fingerprint(ts.Certificate().Raw),
		ClientCertFile:     certPath,
		ClientKeyFile:      keyPath,
	}

	got := List(context.Background(), map[string]config.ServerEntry{"srv": entry}, 0)
	if len(got) != 1 {
		t.Fatalf("want 1 running shed over mtls, got %#v", got)
	}
	if !sawClientCert.Load() {
		t.Fatal("server never saw a client certificate")
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("expected exactly one request, got %d", n)
	}
}

// TestList_NoStoredCredentialIsSkippedWithoutDialing pins the "never mint,
// never enroll" contract: a secure entry with no token recorded at all is a
// server that WOULD need an SSH round trip to become usable, and this
// package must never trigger one — so it must not even attempt the HTTP
// call.
//
// The handler-hit assertion alone would also pass if an ssh mint failed
// EARLIER for some unrelated reason — it doesn't prove List never even
// attempted to enroll. The on-disk-config-unchanged and entry-unmutated
// assertions below prove the stronger claim: List truly never persists or
// touches anything, not just that this one request never landed.
func TestList_NoStoredCredentialIsSkippedWithoutDialing(t *testing.T) {
	handler, hits := shedsHandler(t, config.Shed{Name: "a", Status: config.StatusRunning})
	ts := httptest.NewServer(http.HandlerFunc(handler))
	defer ts.Close()

	entry := config.ServerEntry{
		Host:               "127.0.0.1",
		APIURL:             ts.URL,
		SSHPort:            2222,
		TLSCertFingerprint: "sha256:deadbeef", // UsesTLS() true, no token recorded
	}
	beforeEntry := entry

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.ClientConfig{Servers: map[string]config.ServerEntry{"srv": entry}}
	if err := cfg.SaveToPath(cfgPath); err != nil {
		t.Fatalf("save fixture config: %v", err)
	}
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read fixture config: %v", err)
	}

	got := List(context.Background(), map[string]config.ServerEntry{"srv": entry}, 0)
	if len(got) != 0 {
		t.Fatalf("no stored credential must be skipped, got %#v", got)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("expected zero requests for a credential-less entry, got %d", n)
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("re-read fixture config: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("List must never write to config.yaml:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if entry != beforeEntry {
		t.Fatalf("List must never mutate the caller's entry: got %#v, want %#v", entry, beforeEntry)
	}
}

// TestList_MTLSEntryWithNoCertFilesIsSkipped is the mtls twin of the above:
// AuthMode mtls but no cert/key path recorded (or an unreadable pair) is
// also a "would need to enroll" shape, never dialed — and, as above,
// strengthened to prove no persistence and no mutation rather than only
// "the handler was never hit".
func TestList_MTLSEntryWithNoCertFilesIsSkipped(t *testing.T) {
	handler, hits := shedsHandler(t, config.Shed{Name: "a", Status: config.StatusRunning})
	ts := httptest.NewServer(http.HandlerFunc(handler))
	defer ts.Close()

	entry := config.ServerEntry{
		Host:     "127.0.0.1",
		APIURL:   ts.URL,
		SSHPort:  2222,
		AuthMode: config.AuthModeMTLS,
		// ClientCertFile/ClientKeyFile deliberately unset.
	}
	beforeEntry := entry

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.ClientConfig{Servers: map[string]config.ServerEntry{"srv": entry}}
	if err := cfg.SaveToPath(cfgPath); err != nil {
		t.Fatalf("save fixture config: %v", err)
	}
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read fixture config: %v", err)
	}

	got := List(context.Background(), map[string]config.ServerEntry{"srv": entry}, 0)
	if len(got) != 0 {
		t.Fatalf("an mtls entry with no cert files must be skipped, got %#v", got)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("expected zero requests for a credential-less mtls entry, got %d", n)
	}

	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("re-read fixture config: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("List must never write to config.yaml:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if entry != beforeEntry {
		t.Fatalf("List must never mutate the caller's entry: got %#v, want %#v", entry, beforeEntry)
	}
}

func TestList_ConcurrentAcrossServers(t *testing.T) {
	const delay = 150 * time.Millisecond
	slow := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(config.ShedsResponse{
				Sheds: []config.Shed{{Name: name, Status: config.StatusRunning}},
			})
		}))
	}
	ts1, ts2 := slow("one"), slow("two")
	defer ts1.Close()
	defer ts2.Close()

	start := time.Now()
	got := List(context.Background(), map[string]config.ServerEntry{
		"srv1": openEntry(ts1),
		"srv2": openEntry(ts2),
	}, 0)
	elapsed := time.Since(start)

	if len(got) != 2 {
		t.Fatalf("want 2 running sheds, got %#v", got)
	}
	// Sequential would be >= 2*delay; concurrent fan-out should land well
	// under that even with scheduling slack.
	if elapsed >= 2*delay {
		t.Fatalf("List took %s across 2 servers with a %s delay each; fan-out does not look concurrent", elapsed, delay)
	}
}

func TestList_OrderedByServerThenShedName(t *testing.T) {
	handlerA, _ := shedsHandler(t,
		config.Shed{Name: "zeta", Status: config.StatusRunning},
		config.Shed{Name: "alpha", Status: config.StatusRunning},
	)
	tsA := httptest.NewServer(handlerA)
	defer tsA.Close()
	handlerB, _ := shedsHandler(t, config.Shed{Name: "middle", Status: config.StatusRunning})
	tsB := httptest.NewServer(handlerB)
	defer tsB.Close()

	got := List(context.Background(), map[string]config.ServerEntry{
		"bserver": openEntry(tsB),
		"aserver": openEntry(tsA),
	}, 0)

	var order [][2]string
	for _, r := range got {
		order = append(order, [2]string{r.Server, r.Name})
	}
	want := [][2]string{{"aserver", "alpha"}, {"aserver", "zeta"}, {"bserver", "middle"}}
	if len(order) != len(want) {
		t.Fatalf("got %#v, want %#v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("got %#v, want %#v", order, want)
		}
	}
}
