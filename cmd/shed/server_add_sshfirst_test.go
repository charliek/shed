package main

// Tests for the SSH-first `shed server add` (and the --refetch that shares its
// path). The two things under test are the ORDER (host key pinned before any
// credential exchange) and the DECISION TABLE (what each bootstrap outcome does),
// because those are what make an mtls server addable and an unauthorized key a
// hard failure rather than a quietly broken entry.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/sdk"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// scanCall records what the stubbed keyscan was asked for, so a test can assert
// the port first contact actually dialed.
type scanCall struct {
	calls atomic.Int32
	host  atomic.Value // string
	port  atomic.Int32
}

// stubHostKeyScan installs a keyscan that returns hostKey (or err) without a
// live sshd, and records the endpoint it was asked to scan.
func stubHostKeyScan(t *testing.T, hostKey string, err error) *scanCall {
	t.Helper()
	rec := &scanCall{}
	rec.host.Store("")
	orig := sshHostKeyScanFn
	t.Cleanup(func() { sshHostKeyScanFn = orig })
	sshHostKeyScanFn = func(host string, port int, _ time.Duration) (string, error) {
		rec.calls.Add(1)
		rec.host.Store(host)
		rec.port.Store(int32(port))
		// The real scan returns a single trimmed line; match that so known_hosts
		// assertions see what production writes.
		return strings.TrimSpace(hostKey), err
	}
	return rec
}

// bootstrapStub installs a bootstrapCredentialFn returning cred/err and counts
// calls. testClientConfig must run first (it restores the original).
type bootstrapStub struct {
	calls atomic.Int32
	// pinnedAtCall records ~/.shed/known_hosts as it stood WHEN the exchange ran.
	// A failed add rolls its pin back, so this is the only way to assert both
	// halves of that behavior — pinned before the exchange, gone after it — in one
	// test.
	pinnedAtCall atomic.Value // []string
}

// pinned returns the known_hosts lines the bootstrap saw.
func (s *bootstrapStub) pinned() []string {
	lines, _ := s.pinnedAtCall.Load().([]string)
	return lines
}

func stubBootstrapResult(cred sdk.Credential, err error) *bootstrapStub {
	s := &bootstrapStub{}
	bootstrapCredentialFn = func(string, int, string, string) (sdk.Credential, error) {
		s.calls.Add(1)
		s.pinnedAtCall.Store(readKnownHostsLines())
		return cred, err
	}
	return s
}

// readKnownHostsLines returns the non-empty lines of ~/.shed/known_hosts (nil
// when the file does not exist). Unlike knownHostsLines it takes no *testing.T,
// so the bootstrap stub can call it mid-exchange.
func readKnownHostsLines() []string {
	data, err := os.ReadFile(config.GetKnownHostsPath())
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

// knownHostsLines returns the non-empty lines of ~/.shed/known_hosts.
func knownHostsLines(t *testing.T) []string {
	t.Helper()
	if _, err := os.Stat(config.GetKnownHostsPath()); err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat known_hosts: %v", err)
	}
	return readKnownHostsLines()
}

// tokenServer stands up a TLS listener that answers /api/info only for the
// given bearer token — a token-mode shed-server, from the CLI's point of view.
func tokenServer(t *testing.T, name, token string) (*httptest.Server, string, int, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"name":%q,"ssh_port":2222,"auth_mode":"token"}`, name))
	}))
	t.Cleanup(srv.Close)
	host, port := hostPort(t, srv.URL)
	return srv, host, port, servertls.Fingerprint(srv.Certificate().Raw)
}

// tokenBundle is what a token-mode server's bootstrap returns for the endpoint
// tokenServer created.
func tokenBundle(token, pin string, httpsPort int, expires time.Time) sdk.Credential {
	return sdk.Credential{Bundle: sdk.Bundle{
		AuthMode:           sdk.AuthModeToken,
		HTTPSPort:          httpsPort,
		TLSCertFingerprint: pin,
		Token:              token,
		Scope:              "control",
		ExpiresAt:          expires,
	}}
}

// mtlsCredentialFor issues a credential from the in-process mtls fixture and
// points its bundle at that fixture's real HTTPS port, so `server add` builds an
// api_url the test server actually answers on.
func mtlsCredentialFor(t *testing.T, m *mtlsServer) sdk.Credential {
	t.Helper()
	cred := m.credential(t, "cli-key", farFromExpiry)
	_, port := hostPort(t, m.srv.URL)
	cred.Bundle.HTTPSPort = port
	return cred
}

// ---------------------------------------------------------------------------
// the keyscan itself
// ---------------------------------------------------------------------------

// startKeyscanTarget runs a minimal SSH listener that presents a host key and
// then refuses every authentication attempt — exactly the shape the keyscan has
// to cope with, since it offers no credentials at all.
func startKeyscanTarget(t *testing.T) (host string, port int, fingerprint string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &gossh.ServerConfig{
		PublicKeyCallback: func(gossh.ConnMetadata, gossh.PublicKey) (*gossh.Permissions, error) {
			return nil, errors.New("no")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _, _, _ = gossh.NewServerConn(conn, cfg)
			}()
		}
	}()
	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port, gossh.FingerprintSHA256(signer.PublicKey())
}

func TestScanSSHHostKey(t *testing.T) {
	host, port, want := startKeyscanTarget(t)

	t.Run("captures the presented host key despite the auth failure", func(t *testing.T) {
		key, err := scanSSHHostKey(host, port, 5*time.Second)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		pub, _, _, _, err := gossh.ParseAuthorizedKey([]byte(key))
		if err != nil {
			t.Fatalf("parse scanned key %q: %v", key, err)
		}
		if got := gossh.FingerprintSHA256(pub); got != want {
			t.Errorf("scanned fingerprint = %s, want %s", got, want)
		}
		if strings.Contains(key, "\n") {
			t.Errorf("scanned key must be a single known_hosts-ready line, got %q", key)
		}
	})

	t.Run("an unreachable endpoint names host:port", func(t *testing.T) {
		closed := freeClosedPort(t)
		_, err := scanSSHHostKey(host, closed, 2*time.Second)
		if err == nil {
			t.Fatal("expected an error for a closed port")
		}
		if !strings.Contains(err.Error(), net.JoinHostPort(host, strconv.Itoa(closed))) {
			t.Errorf("error must name the endpoint dialed, got: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// step one: the host key is pinned before anything else happens
// ---------------------------------------------------------------------------

func TestServerAddPinsHostKeyFirst(t *testing.T) {
	hostKey, fp := testHostKey(t)

	// setup mutates the flags for one case; want* describe the outcome.
	tests := []struct {
		name         string
		setup        func(t *testing.T)
		scanErr      error
		wantErr      string
		wantKHPrefix string // known_hosts prefix the line pinned for the exchange must carry
		wantScanPort int
	}{
		{
			name:         "default ssh port",
			wantKHPrefix: "[example.test]:2222 ",
			wantScanPort: defaultAddSSHPort,
		},
		{
			name:         "custom --ssh-port",
			setup:        func(*testing.T) { serverAddSSHPort = 2022 },
			wantKHPrefix: "[example.test]:2022 ",
			wantScanPort: 2022,
		},
		{
			name:         "--fingerprint match",
			setup:        func(*testing.T) { serverAddFingerprint = fp },
			wantKHPrefix: "[example.test]:2222 ",
			wantScanPort: defaultAddSSHPort,
		},
		{
			name:    "--fingerprint mismatch is fatal",
			setup:   func(*testing.T) { serverAddFingerprint = "SHA256:deadbeefdeadbeef" },
			wantErr: "fingerprint mismatch",
		},
		{
			name:    "--json refuses to trust without a pre-pin or --trust-on-first-use",
			setup:   func(*testing.T) { jsonFlag = true },
			wantErr: "refusing to trust SSH host key",
		},
		{
			name:         "--json accepts with --trust-on-first-use",
			setup:        func(*testing.T) { jsonFlag, serverAddTrustTOFU = true, true },
			wantKHPrefix: "[example.test]:2222 ",
			wantScanPort: defaultAddSSHPort,
		},
		{
			name:    "--fingerprint cannot be verified without a scan",
			setup:   func(*testing.T) { serverAddFingerprint = fp },
			scanErr: errors.New("could not reach the shed server's SSH port at example.test:2222: connection refused"),
			wantErr: "cannot verify --fingerprint",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testClientConfig(t)
			withCleanAddFlags(t)
			if tc.setup != nil {
				tc.setup(t)
			}
			scan := stubHostKeyScan(t, hostKey, tc.scanErr)
			// The bootstrap must never run when the host key step fails: that
			// ordering is the whole point of pinning first.
			boot := stubBootstrapResult(sdk.Credential{}, errors.New("bootstrap: key not authorized"))

			err := runServerAdd(nil, []string{"example.test"})

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				if len(knownHostsLines(t)) != 0 {
					t.Errorf("nothing may be pinned when the host-key step fails: %v", knownHostsLines(t))
				}
				if got := boot.calls.Load(); got != 0 {
					t.Errorf("bootstrap ran %d times before the host key was trusted, want 0", got)
				}
				return
			}
			// The bootstrap stub fails, so the add fails — but only AFTER the key
			// was pinned for the exchange, and the pin is then rolled back. Both
			// halves are asserted: the ordering from what the bootstrap SAW, the
			// rollback from what is left behind.
			if err == nil {
				t.Fatal("expected the stubbed bootstrap failure to surface")
			}
			if got := scan.port.Load(); int(got) != tc.wantScanPort {
				t.Errorf("scanned port %d, want %d", got, tc.wantScanPort)
			}
			seen := boot.pinned()
			if len(seen) != 1 || !strings.HasPrefix(seen[0], tc.wantKHPrefix) {
				t.Fatalf("known_hosts during the bootstrap = %v, want one line starting %q", seen, tc.wantKHPrefix)
			}
			if lines := knownHostsLines(t); len(lines) != 0 {
				t.Errorf("a failed add left the host key pinned: %v", lines)
			}
		})
	}
}

// TestServerAddRollsBackOnlyItsOwnPin: the rollback removes the line this add
// wrote and nothing else — not another server's entry, and not a second key
// pinned for the same host by some earlier decision.
func TestServerAddRollsBackOnlyItsOwnPin(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true

	otherKey, _ := testHostKey(t)
	otherKey = strings.TrimSpace(otherKey)
	if err := config.AddKnownHost("other.test", 2222, otherKey); err != nil {
		t.Fatal(err)
	}
	// A different port on the SAME host: a distinct known_hosts line the add must
	// not touch when it un-pins [example.test]:2222.
	if err := config.AddKnownHost("example.test", 2022, otherKey); err != nil {
		t.Fatal(err)
	}
	before := knownHostsLines(t)

	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	boot := stubBootstrapResult(sdk.Credential{}, errors.New("sdk/bootstrap: ssh produced no valid bootstrap bundle"))

	if err := runServerAdd(nil, []string{"example.test"}); err == nil {
		t.Fatal("expected the stubbed bootstrap failure to surface")
	}
	if got := len(boot.pinned()); got != 3 {
		t.Errorf("known_hosts had %d lines during the bootstrap, want 3 (the two pre-existing plus ours)", got)
	}
	after := knownHostsLines(t)
	if len(after) != len(before) {
		t.Fatalf("known_hosts = %v, want the pre-existing %v", after, before)
	}
	for i := range before {
		if after[i] != before[i] {
			t.Errorf("known_hosts line %d = %q, want %q", i, after[i], before[i])
		}
	}
}

// TestServerAddRejectsChangedHostKey: a host already pinned with a DIFFERENT key
// is a hard failure — never a silent re-pin, and never a bootstrap against a
// server whose identity just changed.
func TestServerAddRejectsChangedHostKey(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true

	oldKey, _ := testHostKey(t)
	oldKey = strings.TrimSpace(oldKey)
	if err := config.AddKnownHost("example.test", defaultAddSSHPort, oldKey); err != nil {
		t.Fatal(err)
	}
	newKey, newFP := testHostKey(t)
	stubHostKeyScan(t, newKey, nil)
	boot := stubBootstrapResult(sdk.Credential{}, nil)

	err := runServerAdd(nil, []string{"example.test"})
	if err == nil {
		t.Fatal("a changed host key must fail closed")
	}
	for _, want := range []string{"host key mismatch", newFP, "example.test:2222"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
	if got := boot.calls.Load(); got != 0 {
		t.Errorf("bootstrap ran %d times against a server whose key changed, want 0", got)
	}
	if lines := knownHostsLines(t); len(lines) != 1 || lines[0] != "[example.test]:2222 "+oldKey {
		t.Errorf("known_hosts was rewritten: %v", lines)
	}
}

// TestServerAddAlreadyPinnedHostKeyIsNotDuplicated: re-adding a host whose key
// is already pinned (after a `server rm`, say) must not append a second line or
// re-prompt.
func TestServerAddAlreadyPinnedHostKeyIsNotDuplicated(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	jsonFlag = true // would refuse to TOFU; an already-pinned key needs no trust decision

	hostKey, _ := testHostKey(t)
	if err := config.AddKnownHost("example.test", defaultAddSSHPort, hostKey); err != nil {
		t.Fatal(err)
	}
	stubHostKeyScan(t, hostKey, nil)
	boot := stubBootstrapResult(sdk.Credential{}, errors.New("boom"))

	_ = runServerAdd(nil, []string{"example.test"})
	if got := boot.calls.Load(); got != 1 {
		t.Errorf("bootstrap ran %d times, want 1 — an already-pinned key must proceed", got)
	}
	if lines := knownHostsLines(t); len(lines) != 1 {
		t.Errorf("known_hosts gained a duplicate line: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// step two: the bootstrap decision table
// ---------------------------------------------------------------------------

func TestClassifyAddBootstrap(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want addBootstrapOutcome
	}{
		{"no error", nil, bootstrapIssued},
		{"open server", errors.New("sdk/bootstrap: ssh exited 1: bootstrap requires auth.ssh.mode: enforce"), bootstrapOpenServer},
		{"key not authorized", errors.New("sdk/bootstrap: ssh exited 1: bootstrap: key not authorized"), bootstrapKeyNotAuthorized},
		{"no usable identity", fmt.Errorf("%w: Permission denied (publickey)", sdkbootstrap.ErrNoSSHIdentities), bootstrapKeyNotAuthorized},
		{"refused", errors.New("sdk/bootstrap: ssh exited 255: ssh: connect to host x port 2222: Connection refused"), bootstrapUnreachable},
		{"timed out", errors.New("sdk/bootstrap: ssh exited 255: ssh: connect to host x port 2222: Operation timed out"), bootstrapUnreachable},
		{"host key changed", fmt.Errorf("%w: REMOTE HOST IDENTIFICATION HAS CHANGED", sdkbootstrap.ErrHostKeyMismatch), bootstrapFailed},
		{"host key not verified", fmt.Errorf("%w: Host key verification failed", sdkbootstrap.ErrHostKeyVerificationFailed), bootstrapFailed},
		{"malformed bundle", errors.New("sdk/bootstrap: ssh produced no valid bootstrap bundle"), bootstrapFailed},

		// The open-mode marker is the ONE recoverable outcome, so matching it
		// loosely is how a credential-requiring server gets added without a
		// credential. Each of these contains the marker's text and must NOT
		// classify as open.
		{
			"a longer word starting with the marker",
			errors.New("sdk/bootstrap: ssh exited 1: bootstrap requires auth.ssh.mode: enforcement"),
			bootstrapFailed,
		},
		{
			"the marker alongside another fatal error on the same line",
			errors.New("sdk/bootstrap: ssh exited 1: bootstrap requires auth.ssh.mode: enforce; fatal: cannot write token store"),
			bootstrapFailed,
		},
		{
			"the marker quoted inside a larger message",
			errors.New("sdk/bootstrap: ssh exited 1: the server logged \"bootstrap requires auth.ssh.mode: enforce\" but then panicked"),
			bootstrapFailed,
		},
		{
			"a wording near-miss",
			errors.New("sdk/bootstrap: ssh exited 1: bootstrap requires auth.ssh.mode enforce"),
			bootstrapFailed,
		},
		{
			// A refused key reported by a server that ALSO mentions the marker must
			// still be the hard outcome: the hard branches are classified first.
			"a refused key that also carries the marker",
			errors.New("sdk/bootstrap: ssh exited 1: bootstrap: key not authorized\nbootstrap requires auth.ssh.mode: enforce"),
			bootstrapKeyNotAuthorized,
		},
		{
			// ssh may print a server banner or a warning around the session; the
			// marker on a line of its own is still the server's refusal.
			"the marker on its own line under an ssh banner",
			errors.New("sdk/bootstrap: ssh exited 1: ###########################\nbootstrap requires auth.ssh.mode: enforce"),
			bootstrapOpenServer,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAddBootstrap(tc.err); got != tc.want {
				t.Errorf("classifyAddBootstrap(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestServerAddBootstrapDecisionTable drives the four failure/fallback branches
// end to end through runServerAdd, asserting the exact stderr each one produces
// and — for every branch that is not the open-mode fallback — that NO server
// entry was written.
func TestServerAddBootstrapDecisionTable(t *testing.T) {
	hostKey, _ := testHostKey(t)

	tests := []struct {
		name     string
		bootErr  error
		wantErr  []string
		wantNoKH bool
	}{
		{
			name:    "key not authorized names the allowlist and never falls back",
			bootErr: errors.New("sdk/bootstrap: ssh exited 1: bootstrap: key not authorized"),
			wantErr: []string{"not in this server's allowlist", "auth.ssh.github_users", "authorized_keys"},
		},
		{
			name:    "an ssh dial failure names host:ssh-port",
			bootErr: errors.New("sdk/bootstrap: ssh exited 255: ssh: connect to host example.test port 2222: Connection refused"),
			wantErr: []string{"could not complete the SSH bootstrap with example.test:2222", "Connection refused"},
		},
		{
			name:    "any other bootstrap error is passed through",
			bootErr: errors.New("sdk/bootstrap: ssh produced no valid bootstrap bundle"),
			wantErr: []string{"bootstrap over SSH to example.test:2222 failed", "no valid bootstrap bundle"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testClientConfig(t)
			withCleanAddFlags(t)
			serverAddTrustTOFU = true
			stubHostKeyScan(t, hostKey, nil)
			stubBootstrapResult(sdk.Credential{}, tc.bootErr)
			// A reachable plain-HTTP server on the fallback port: if a branch
			// wrongly fell back, the add would SUCCEED, and this is what proves it
			// did not.
			plain := httptest.NewServer(infoHandler())
			defer plain.Close()
			_, plainPort := hostPort(t, plain.URL)
			serverAddPort = plainPort

			err := runServerAdd(nil, []string{"example.test"})
			if err == nil {
				t.Fatal("expected a hard error, not a fallback")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must mention %q", err, want)
				}
			}
			if len(clientConfig.Servers) != 0 {
				t.Errorf("a server entry was written despite the failure: %+v", clientConfig.Servers)
			}
		})
	}
}

// TestServerAddOpenModeFallsBackToHTTP: the ONE outcome that falls back. The
// server refuses to issue a credential because it runs auth.mode: open, so the
// entry is built from the HTTP probe — with the host key already pinned over
// SSH.
func TestServerAddOpenModeFallsBackToHTTP(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true

	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	stubBootstrapResult(sdk.Credential{}, errors.New("sdk/bootstrap: ssh exited 1: bootstrap requires auth.ssh.mode: enforce"))

	plain := httptest.NewServer(infoHandler())
	defer plain.Close()
	plainHost, plainPort := hostPort(t, plain.URL)
	serverAddPort = plainPort

	if err := runServerAdd(nil, []string{plainHost}); err != nil {
		t.Fatalf("open-mode add: %v", err)
	}
	entry, ok := clientConfig.Servers["test-server"]
	if !ok {
		t.Fatalf("expected an entry named after the server, got %+v", clientConfig.Servers)
	}
	if entry.APIURL != "" || entry.TLSCertFingerprint != "" {
		t.Errorf("plain-HTTP entry must carry no api_url/pin: %+v", entry)
	}
	if entry.ControlToken != "" || entry.AuthMode != "" {
		t.Errorf("an open server issues no credential: %+v", entry)
	}
	if entry.SSHPort != defaultAddSSHPort {
		t.Errorf("ssh_port = %d, want the pinned %d", entry.SSHPort, defaultAddSSHPort)
	}
	if lines := knownHostsLines(t); len(lines) != 1 {
		t.Errorf("the host key must still be pinned over SSH: %v", lines)
	}
}

// ---------------------------------------------------------------------------
// the credential paths
// ---------------------------------------------------------------------------

// TestServerAddMTLSEndToEnd is the point of the whole change: an mtls server —
// which serves no plain HTTP and refuses the TLS handshake to an unenrolled
// client — can now be added at all. The S7 tests hand-built the entry this
// produces; here `server add` produces it, against a real mtls listener.
func TestServerAddMTLSEndToEnd(t *testing.T) {
	cfgPath := testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true

	m := newMTLSServer(t)
	host, port := hostPort(t, m.srv.URL)
	cred := mtlsCredentialFor(t, m)

	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	stubBootstrapResult(cred, nil)

	if err := runServerAdd(nil, []string{host}); err != nil {
		t.Fatalf("mtls add: %v", err)
	}
	// The verification GET /api/info really happened, over mtls.
	if got := m.requests.Load(); got != 1 {
		t.Errorf("server saw %d requests, want 1 (the post-add verification)", got)
	}
	if got := m.lastAuth.Load().(string); got != "" {
		t.Errorf("the verification sent Authorization %q; an mtls credential belongs in the handshake", got)
	}

	entry, ok := clientConfig.Servers["srv"] // the fixture's /api/info name
	if !ok {
		t.Fatalf("no entry written: %+v", clientConfig.Servers)
	}
	wantURL := "https://" + net.JoinHostPort(host, strconv.Itoa(port))
	switch {
	case entry.AuthMode != config.AuthModeMTLS:
		t.Errorf("auth_mode = %q, want mtls", entry.AuthMode)
	case entry.APIURL != wantURL:
		t.Errorf("api_url = %q, want %q", entry.APIURL, wantURL)
	case entry.TLSCertFingerprint != m.pin:
		t.Errorf("tls pin = %q, want %q", entry.TLSCertFingerprint, m.pin)
	case entry.SSHPort != defaultAddSSHPort:
		t.Errorf("ssh_port = %d, want %d", entry.SSHPort, defaultAddSSHPort)
	case entry.ControlToken != "" || !entry.ControlTokenExpiresAt.IsZero():
		t.Errorf("an mtls entry must carry no bearer token: %+v", entry)
	case !entry.ClientCertExpiresAt.Equal(cred.Bundle.ExpiresAt):
		t.Errorf("client_cert_expires_at = %v, want %v", entry.ClientCertExpiresAt, cred.Bundle.ExpiresAt)
	}

	// The credential is on disk, in the store, with tight permissions.
	wantCert, wantKey := config.ClientCredentialPaths("srv")
	if entry.ClientCertFile != wantCert || entry.ClientKeyFile != wantKey {
		t.Fatalf("entry points at %s/%s, want %s/%s", entry.ClientCertFile, entry.ClientKeyFile, wantCert, wantKey)
	}
	for _, p := range []string{wantCert, wantKey} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if fi.Mode().Perm() != 0600 {
			t.Errorf("%s mode = %v, want 0600", p, fi.Mode().Perm())
		}
	}
	if fi, err := os.Stat(config.ServerCredsDir("srv")); err != nil {
		t.Fatalf("stat creds dir: %v", err)
	} else if fi.Mode().Perm() != 0700 {
		t.Errorf("creds dir mode = %v, want 0700", fi.Mode().Perm())
	}
	if _, err := tls.LoadX509KeyPair(wantCert, wantKey); err != nil {
		t.Errorf("the stored pair does not load: %v", err)
	}

	// The persisted entry works: a client built from the reloaded config reaches
	// the server without another bootstrap.
	reloaded, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	saved := reloaded.Servers["srv"]
	bootstrapCredentialFn = func(string, int, string, string) (sdk.Credential, error) {
		t.Error("the saved entry must not need a fresh bootstrap")
		return sdk.Credential{}, errors.New("unexpected")
	}
	if _, err := NewAPIClientFromEntry(&saved, DefaultTimeout).GetInfo(); err != nil {
		t.Errorf("the saved mtls entry does not work: %v", err)
	}
}

// TestServerAddTokenEntryShape is the regression guard: a token-mode add must
// still produce exactly the entry it produced before first contact moved to SSH.
func TestServerAddTokenEntryShape(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true

	_, host, port, pin := tokenServer(t, "tok", "shed_ctl_fresh")
	expires := time.Now().Add(24 * time.Hour).Round(time.Second)
	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	stubBootstrapResult(tokenBundle("shed_ctl_fresh", pin, port, expires), nil)

	if err := runServerAdd(nil, []string{host}); err != nil {
		t.Fatalf("token add: %v", err)
	}
	entry, ok := clientConfig.Servers["tok"]
	if !ok {
		t.Fatalf("no entry written: %+v", clientConfig.Servers)
	}
	wantURL := "https://" + net.JoinHostPort(host, strconv.Itoa(port))
	switch {
	case entry.AuthMode != config.AuthModeToken:
		t.Errorf("auth_mode = %q, want token", entry.AuthMode)
	case entry.APIURL != wantURL:
		t.Errorf("api_url = %q, want %q", entry.APIURL, wantURL)
	case entry.TLSCertFingerprint != pin:
		t.Errorf("tls pin = %q, want %q", entry.TLSCertFingerprint, pin)
	case entry.ControlToken != "shed_ctl_fresh":
		t.Errorf("control_token = %q, want the minted one", entry.ControlToken)
	case !entry.ControlTokenExpiresAt.Equal(expires):
		t.Errorf("control_token_expires_at = %v, want %v", entry.ControlTokenExpiresAt, expires)
	case entry.ClientCertFile != "" || entry.ClientKeyFile != "":
		t.Errorf("a token entry must reference no certificate: %+v", entry)
	case entry.SSHPort != defaultAddSSHPort:
		t.Errorf("ssh_port = %d, want %d", entry.SSHPort, defaultAddSSHPort)
	}
	if clientConfig.DefaultServer != "tok" {
		t.Errorf("default_server = %q, want the first-added server", clientConfig.DefaultServer)
	}
}

// TestServerAddVerificationFailureWritesNothing: the authenticated GET
// /api/info is a gate, not a formality — a bundle that points somewhere
// unreachable must not leave an entry (or a private key) behind.
func TestServerAddVerificationFailureWritesNothing(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true

	m := newMTLSServer(t)
	cred := mtlsCredentialFor(t, m)
	cred.Bundle.HTTPSPort = freeClosedPort(t) // nothing is listening there

	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	stubBootstrapResult(cred, nil)

	err := runServerAdd(nil, []string{"127.0.0.1"})
	if err == nil {
		t.Fatal("expected the verification to fail")
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("error should name the failed verification, got: %v", err)
	}
	if len(clientConfig.Servers) != 0 {
		t.Errorf("an unverified entry was written: %+v", clientConfig.Servers)
	}
	if _, err := os.Stat(config.GetCredsDir()); !os.IsNotExist(err) {
		t.Errorf("credential material was written for an unverified server (err=%v)", err)
	}
	if lines := knownHostsLines(t); len(lines) != 0 {
		t.Errorf("an unverified server left its host key pinned: %v", lines)
	}
}

// TestServerAddDuplicateNameIsRefusedBeforeBootstrap: an explicit --name that
// already exists fails before an SSH exchange mints anything.
func TestServerAddDuplicateNameIsRefusedBeforeBootstrap(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddName = "dupe"
	clientConfig.Servers["dupe"] = config.ServerEntry{Host: "other", SSHPort: 2222}

	scan := stubHostKeyScan(t, "", errors.New("must not scan"))
	boot := stubBootstrapResult(sdk.Credential{}, nil)

	err := runServerAdd(nil, []string{"example.test"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want an already-exists error", err)
	}
	if scan.calls.Load() != 0 || boot.calls.Load() != 0 {
		t.Error("a duplicate name must be caught before any network work")
	}
}

// TestServerAddReplacesStaleCredentials: `shed server rm` deletes the creds
// directory, and re-adding the same name lands a fresh pair rather than
// inheriting the old one.
func TestServerAddReplacesStaleCredentials(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true

	m := newMTLSServer(t)
	host, _ := hostPort(t, m.srv.URL)
	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)

	first := mtlsCredentialFor(t, m)
	stubBootstrapResult(first, nil)
	if err := runServerAdd(nil, []string{host}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	certPath, keyPath := config.ClientCredentialPaths("srv")
	firstCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}

	// Remove it: the credential directory must go with the entry.
	if err := runServerRemove(nil, []string{"srv"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(config.ServerCredsDir("srv")); !os.IsNotExist(err) {
		t.Fatalf("`server rm` left the credential directory behind (err=%v)", err)
	}

	// Re-add: a distinct certificate replaces the old one cleanly.
	second := mtlsCredentialFor(t, m)
	if second.Bundle.CertSerial == first.Bundle.CertSerial {
		t.Fatal("test setup: the two credentials must differ")
	}
	stubBootstrapResult(second, nil)
	if err := runServerAdd(nil, []string{host}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	secondCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(secondCert) == string(firstCert) {
		t.Error("the re-add reused the previous certificate")
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Errorf("the replaced pair does not load: %v", err)
	}
}

// ---------------------------------------------------------------------------
// IPv6 literals
// ---------------------------------------------------------------------------

func TestIPv6HostHandling(t *testing.T) {
	t.Run("normalizeAddHost unwraps a bracketed literal", func(t *testing.T) {
		for in, want := range map[string]string{
			"[::1]":              "::1",
			"::1":                "::1",
			" [fd00::1] ":        "fd00::1",
			"example.test":       "example.test",
			"[not-an-ip-really]": "not-an-ip-really",
		} {
			if got := normalizeAddHost(in); got != want {
				t.Errorf("normalizeAddHost(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("api_url brackets the literal", func(t *testing.T) {
		got := apiURLFromBundle("::1", sdk.Bundle{HTTPSPort: 8443}, 0)
		if got != "https://[::1]:8443" {
			t.Errorf("apiURLFromBundle = %q, want https://[::1]:8443", got)
		}
		if _, err := url.Parse(got); err != nil {
			t.Errorf("api_url does not parse: %v", err)
		}
	})

	t.Run("BaseURL brackets the literal for a plain-http entry", func(t *testing.T) {
		entry := config.ServerEntry{Host: "::1", HTTPPort: 8080}
		got := entry.BaseURL()
		if got != "http://[::1]:8080" {
			t.Errorf("BaseURL = %q, want http://[::1]:8080", got)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("BaseURL does not parse: %v", err)
		}
		if u.Hostname() != "::1" || u.Port() != "8080" {
			t.Errorf("parsed %q as host %q port %q", got, u.Hostname(), u.Port())
		}
	})

	t.Run("the host key is pinned under the bracketed known_hosts form", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true
		hostKey, _ := testHostKey(t)
		scan := stubHostKeyScan(t, hostKey, nil)
		boot := stubBootstrapResult(sdk.Credential{}, errors.New("stop here"))

		_ = runServerAdd(nil, []string{"[::1]"})

		if got := scan.host.Load().(string); got != "::1" {
			t.Errorf("scanned host = %q, want the unbracketed literal", got)
		}
		// The failed add rolls the pin back, so the line is asserted as the
		// bootstrap saw it.
		lines := boot.pinned()
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "[::1]:2222 ") {
			t.Fatalf("known_hosts during the bootstrap = %v, want a [::1]:2222 line", lines)
		}

		// The bracketed form also round-trips through the known_hosts LOOKUP,
		// which is what the bootstrap's StrictHostKeyChecking depends on.
		if err := config.AddKnownHost("::1", 2222, hostKey); err != nil {
			t.Fatal(err)
		}
		pub, _, _, _, err := gossh.ParseAuthorizedKey([]byte(hostKey))
		if err != nil {
			t.Fatal(err)
		}
		status, err := knownHostStatusFor("::1", 2222, pub)
		if err != nil || status != hostKeyPinned {
			t.Errorf("knownHostStatusFor = (%v, %v), want (pinned, nil)", status, err)
		}
	})

	// TestIPv6 open-mode add, end to end over a real ::1 listener: the entry an
	// open server produces carries no api_url, so every later request builds its
	// URL from Host+HTTPPort — the construction that an unbracketed literal
	// silently breaks.
	t.Run("an open-mode add over ::1 produces a usable entry", func(t *testing.T) {
		ln, err := net.Listen("tcp", "[::1]:0")
		if err != nil {
			t.Skipf("no IPv6 loopback in this environment: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		// The entry's http_port comes from what the server REPORTS, so the fixture
		// has to report the port it actually listens on for the saved entry to be
		// reachable.
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/info" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, fmt.Sprintf(
				`{"name":"test-server","http_port":%d,"ssh_port":2222,"auth_mode":"open"}`, port))
		}))
		_ = srv.Listener.Close()
		srv.Listener = ln
		srv.Start()
		defer srv.Close()

		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true
		serverAddPort = port

		hostKey, _ := testHostKey(t)
		stubHostKeyScan(t, hostKey, nil)
		stubBootstrapResult(sdk.Credential{}, errors.New("sdk/bootstrap: ssh exited 1: bootstrap requires auth.ssh.mode: enforce"))

		if err := runServerAdd(nil, []string{"[::1]"}); err != nil {
			t.Fatalf("open-mode add over IPv6: %v", err)
		}
		entry, ok := clientConfig.Servers["test-server"]
		if !ok {
			t.Fatalf("no entry written: %+v", clientConfig.Servers)
		}
		if entry.Host != "::1" {
			t.Errorf("host = %q, want the unbracketed literal", entry.Host)
		}
		if want := "http://[::1]:" + strconv.Itoa(port); entry.BaseURL() != want {
			t.Fatalf("BaseURL = %q, want %q", entry.BaseURL(), want)
		}
		if _, err := NewAPIClientFromEntry(&entry, DefaultTimeout).GetInfo(); err != nil {
			t.Errorf("the saved IPv6 entry does not work: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// shed server update --refetch
// ---------------------------------------------------------------------------

// TestServerUpdateRefetchIsSSHFirst: --refetch re-bootstraps over SSH and takes
// the pin from the bundle, which is the only way it can work against an mtls
// server (an unenrolled TLS dial cannot complete a handshake there). It also
// re-enrolls the credential.
func TestServerUpdateRefetchIsSSHFirst(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true

	m := newMTLSServer(t)
	host, _ := hostPort(t, m.srv.URL)
	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	stubBootstrapResult(mtlsCredentialFor(t, m), nil)
	if err := runServerAdd(nil, []string{host}); err != nil {
		t.Fatalf("add: %v", err)
	}
	certPath, _ := config.ClientCredentialPaths("srv")
	before, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}

	serverUpdateRefetch = true
	serverUpdateTrustTOFU = true
	stubBootstrapResult(mtlsCredentialFor(t, m), nil)
	if err := runServerUpdate(nil, []string{"srv"}); err != nil {
		t.Fatalf("refetch: %v", err)
	}
	after, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == string(before) {
		t.Error("--refetch must re-enroll the credential, not just re-read the pin")
	}
	entry := clientConfig.Servers["srv"]
	if entry.TLSCertFingerprint != m.pin || entry.AuthMode != config.AuthModeMTLS {
		t.Errorf("entry lost its pin/mode across --refetch: %+v", entry)
	}
}

// ---------------------------------------------------------------------------
// the add/refetch transaction: config.yaml and the credential store
// ---------------------------------------------------------------------------

// breakConfigSave points clientConfig at a path that cannot be written (its
// parent is a regular file), preserving the entries the current config holds.
// Every subsequent Save fails; nothing else changes.
func breakConfigSave(t *testing.T) {
	t.Helper()
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	broken, err := config.LoadClientConfigFromPath(filepath.Join(blocked, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Only now put a regular FILE where the config's directory has to be: the load
	// above had to succeed (there is simply no config there yet), and every Save
	// from here on fails on the mkdir.
	if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, entry := range clientConfig.Servers {
		broken.Servers[name] = entry
	}
	broken.DefaultServer = clientConfig.DefaultServer
	clientConfig = broken
	if err := clientConfig.Update(func(*config.ClientConfig) error { return nil }); err == nil {
		t.Fatal("test setup: the config save was supposed to fail")
	}
}

// failCredentialStaging makes the credential write fail, without touching
// anything already on disk.
func failCredentialStaging(t *testing.T) {
	t.Helper()
	orig := stageCredentialsFn
	t.Cleanup(func() { stageCredentialsFn = orig })
	stageCredentialsFn = func(string, []byte, []byte) (*config.StagedClientCredentials, error) {
		return nil, errors.New("injected: no space left on device")
	}
}

// addedMTLSServer runs a successful mtls add and returns the state a working
// entry consists of: the config file, the stored certificate, and the entry.
func addedMTLSServer(t *testing.T, m *mtlsServer) (cfgPath string, certPEM []byte, entry config.ServerEntry) {
	t.Helper()
	host, _ := hostPort(t, m.srv.URL)
	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	stubBootstrapResult(mtlsCredentialFor(t, m), nil)
	if err := runServerAdd(nil, []string{host}); err != nil {
		t.Fatalf("add: %v", err)
	}
	certPath, _ := config.ClientCredentialPaths("srv")
	cert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	return config.GetClientConfigPath(), cert, clientConfig.Servers["srv"]
}

// assertUnchangedMTLSState re-reads everything a working mtls entry consists of
// and fails if any of it moved.
func assertUnchangedMTLSState(t *testing.T, cfgPath string, wantCert []byte, want config.ServerEntry) {
	t.Helper()
	certPath, keyPath := config.ClientCredentialPaths("srv")
	got, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("the previous client certificate is gone: %v", err)
	}
	if string(got) != string(wantCert) {
		t.Error("the previous client certificate was replaced by a failed operation")
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Errorf("the previous credential pair no longer loads: %v", err)
	}

	saved, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := saved.Servers["srv"]
	if !ok {
		t.Fatalf("the saved entry is gone: %+v", saved.Servers)
	}
	switch {
	case entry.AuthMode != want.AuthMode:
		t.Errorf("saved auth_mode = %q, want %q", entry.AuthMode, want.AuthMode)
	case entry.TLSCertFingerprint != want.TLSCertFingerprint:
		t.Errorf("saved tls pin = %q, want %q", entry.TLSCertFingerprint, want.TLSCertFingerprint)
	case entry.ClientCertFile != want.ClientCertFile || entry.ClientKeyFile != want.ClientKeyFile:
		t.Errorf("saved entry points at %s/%s, want %s/%s",
			entry.ClientCertFile, entry.ClientKeyFile, want.ClientCertFile, want.ClientKeyFile)
	}
}

// TestServerAddIsATransaction: neither half of an add — the config entry or the
// credential material — may survive the other half failing.
func TestServerAddIsATransaction(t *testing.T) {
	t.Run("a credential-write failure writes nothing", func(t *testing.T) {
		cfgPath := testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true

		m := newMTLSServer(t)
		host, _ := hostPort(t, m.srv.URL)
		hostKey, _ := testHostKey(t)
		stubHostKeyScan(t, hostKey, nil)
		stubBootstrapResult(mtlsCredentialFor(t, m), nil)
		failCredentialStaging(t)

		err := runServerAdd(nil, []string{host})
		if err == nil || !strings.Contains(err.Error(), "no space left on device") {
			t.Fatalf("err = %v, want the injected credential-write failure", err)
		}
		if len(clientConfig.Servers) != 0 {
			t.Errorf("an entry was written for a credential that was never stored: %+v", clientConfig.Servers)
		}
		if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
			t.Errorf("config.yaml was written (err=%v)", err)
		}
		if lines := knownHostsLines(t); len(lines) != 0 {
			t.Errorf("the host key pin survived a failed add: %v", lines)
		}
	})

	t.Run("a config-save failure leaves no credential behind", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true

		m := newMTLSServer(t)
		host, _ := hostPort(t, m.srv.URL)
		hostKey, _ := testHostKey(t)
		stubHostKeyScan(t, hostKey, nil)
		stubBootstrapResult(mtlsCredentialFor(t, m), nil)
		breakConfigSave(t)

		err := runServerAdd(nil, []string{host})
		if err == nil || !strings.Contains(err.Error(), "failed to save config") {
			t.Fatalf("err = %v, want the config save to fail", err)
		}
		if len(clientConfig.Servers) != 0 {
			t.Errorf("the in-memory config kept an entry the file never got: %+v", clientConfig.Servers)
		}
		certPath, keyPath := config.ClientCredentialPaths("srv")
		for _, p := range []string{certPath, keyPath} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s outlived the failed add (err=%v)", p, err)
			}
		}
		if lines := knownHostsLines(t); len(lines) != 0 {
			t.Errorf("the host key pin survived a failed add: %v", lines)
		}
	})
}

// TestServerUpdateRefetchIsATransaction is the same property on the path that
// actually has something to lose: a --refetch runs against a WORKING entry, and
// a failure anywhere in it must leave that entry working.
func TestServerUpdateRefetchIsATransaction(t *testing.T) {
	t.Run("a credential-write failure keeps the previous credential", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true

		m := newMTLSServer(t)
		cfgPath, cert, entry := addedMTLSServer(t, m)

		serverUpdateRefetch = true
		serverUpdateTrustTOFU = true
		stubBootstrapResult(mtlsCredentialFor(t, m), nil)
		failCredentialStaging(t)

		err := runServerUpdate(nil, []string{"srv"})
		if err == nil || !strings.Contains(err.Error(), "no space left on device") {
			t.Fatalf("err = %v, want the injected credential-write failure", err)
		}
		assertUnchangedMTLSState(t, cfgPath, cert, entry)
	})

	t.Run("a config-save failure keeps the previous credential", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true

		m := newMTLSServer(t)
		cfgPath, cert, entry := addedMTLSServer(t, m)

		serverUpdateRefetch = true
		serverUpdateTrustTOFU = true
		stubBootstrapResult(mtlsCredentialFor(t, m), nil)
		breakConfigSave(t)

		err := runServerUpdate(nil, []string{"srv"})
		if err == nil || !strings.Contains(err.Error(), "failed to save config") {
			t.Fatalf("err = %v, want the config save to fail", err)
		}
		assertUnchangedMTLSState(t, cfgPath, cert, entry)
	})

	// The mtls→token flip is where the old ordering was worst: it deleted the
	// certificate the config still pointed at, so a failed save stranded the entry
	// on a file that no longer existed.
	t.Run("a config-save failure on a mode flip keeps the previous credential", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true

		m := newMTLSServer(t)
		cfgPath, cert, entry := addedMTLSServer(t, m)

		// The server has switched to token mode: same endpoint, a bearer token.
		_, tokHost, tokPort, tokPin := tokenServer(t, "srv", "shed_ctl_flip")
		saved := clientConfig.Servers["srv"]
		saved.Host = tokHost
		clientConfig.Servers["srv"] = saved

		serverUpdateRefetch = true
		serverUpdateTrustTOFU = true
		stubBootstrapResult(tokenBundle("shed_ctl_flip", tokPin, tokPort, time.Now().Add(time.Hour)), nil)
		breakConfigSave(t)

		err := runServerUpdate(nil, []string{"srv"})
		if err == nil || !strings.Contains(err.Error(), "failed to save config") {
			t.Fatalf("err = %v, want the config save to fail", err)
		}
		assertUnchangedMTLSState(t, cfgPath, cert, entry)
	})
}

// ---------------------------------------------------------------------------
// a host key the direct scan cannot reach
// ---------------------------------------------------------------------------

// TestServerAddWithUnscannableHostKey: the keyscan dials directly, but the
// bootstrap shells out to `ssh`, which honors ~/.ssh/config (HostName, Port,
// ProxyJump, ProxyCommand). A scan that cannot connect therefore proves nothing
// about the server, and must not end the command.
func TestServerAddWithUnscannableHostKey(t *testing.T) {
	dialFailed := errors.New("could not reach the shed server's SSH port at example.test:2222: connection refused")

	t.Run("the bootstrap still runs and can succeed", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true

		m := newMTLSServer(t)
		host, _ := hostPort(t, m.srv.URL)
		stubHostKeyScan(t, "", dialFailed)
		boot := stubBootstrapResult(mtlsCredentialFor(t, m), nil)

		if err := runServerAdd(nil, []string{host}); err != nil {
			t.Fatalf("add: %v", err)
		}
		if got := boot.calls.Load(); got != 1 {
			t.Errorf("bootstrap ran %d times, want 1 — a failed scan must not end the add", got)
		}
		if _, ok := clientConfig.Servers["srv"]; !ok {
			t.Errorf("no entry written: %+v", clientConfig.Servers)
		}
		// We pinned nothing; ssh verified the host key itself against
		// ~/.shed/known_hosts, so this command must not have invented an entry.
		if lines := knownHostsLines(t); len(lines) != 0 {
			t.Errorf("a key that was never scanned was pinned anyway: %v", lines)
		}
	})

	t.Run("an SSH port that is unreachable both ways still reaches the HTTP fallback", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)

		plain := httptest.NewServer(infoHandler())
		defer plain.Close()
		plainHost, plainPort := hostPort(t, plain.URL)
		serverAddPort = plainPort

		stubHostKeyScan(t, "", dialFailed)
		boot := stubBootstrapResult(sdk.Credential{},
			errors.New("sdk/bootstrap: ssh exited 255: ssh: connect to host x port 2222: Connection refused"))

		if err := runServerAdd(nil, []string{plainHost}); err != nil {
			t.Fatalf("open-mode add over HTTP: %v", err)
		}
		if got := boot.calls.Load(); got != 1 {
			t.Errorf("bootstrap ran %d times, want 1", got)
		}
		if _, ok := clientConfig.Servers["test-server"]; !ok {
			t.Errorf("no entry written: %+v", clientConfig.Servers)
		}
	})

	t.Run("an unreachable SSH port is still fatal for a credentialed server", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)

		// A reachable HTTP listener that reports token mode: the fallback finds it
		// and must refuse, because its credential only comes over SSH.
		tokenInfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"name":"tok","http_port":8080,"ssh_port":2222,"auth_mode":"token"}`)
		}))
		defer tokenInfo.Close()
		host, port := hostPort(t, tokenInfo.URL)
		serverAddPort = port

		stubHostKeyScan(t, "", dialFailed)
		stubBootstrapResult(sdk.Credential{},
			errors.New("sdk/bootstrap: ssh exited 255: ssh: connect to host x port 2222: Connection refused"))

		err := runServerAdd(nil, []string{host})
		if err == nil || !strings.Contains(err.Error(), "auth.mode: token") {
			t.Fatalf("err = %v, want a refusal naming the server's auth mode", err)
		}
		if len(clientConfig.Servers) != 0 {
			t.Errorf("a credential-less entry was written for a token server: %+v", clientConfig.Servers)
		}
	})

	t.Run("a bootstrap failure explains the missing pin", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)

		stubHostKeyScan(t, "", dialFailed)
		stubBootstrapResult(sdk.Credential{},
			fmt.Errorf("%w: Host key verification failed", sdkbootstrap.ErrHostKeyVerificationFailed))

		err := runServerAdd(nil, []string{"example.test"})
		if err == nil {
			t.Fatal("expected the host-key verification failure to surface")
		}
		for _, want := range []string{"Host key verification failed", "ssh-keyscan", config.GetKnownHostsPath()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q must mention %q", err, want)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// --https-port on the SSH path
// ---------------------------------------------------------------------------

// TestServerAddHTTPSPortOverridesBundlePort: an explicitly-passed --https-port
// is the port the OPERATOR reaches the server on (a port-mapped or externally
// published deployment), and it wins over the port the bundle advertises. The
// pin still comes from the bundle, and the verification GET still has to pass —
// so a wrong override fails here rather than on the user's next command.
func TestServerAddHTTPSPortOverridesBundlePort(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true

	_, host, port, pin := tokenServer(t, "tok", "shed_ctl_mapped")
	expires := time.Now().Add(time.Hour).Round(time.Second)
	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	// The bundle advertises a port nothing listens on; --https-port names the real one.
	stubBootstrapResult(tokenBundle("shed_ctl_mapped", pin, freeClosedPort(t), expires), nil)
	serverAddHTTPSPort = port

	if err := runServerAdd(nil, []string{host}); err != nil {
		t.Fatalf("add with --https-port: %v", err)
	}
	entry := clientConfig.Servers["tok"]
	if want := "https://" + net.JoinHostPort(host, strconv.Itoa(port)); entry.APIURL != want {
		t.Errorf("api_url = %q, want the --https-port endpoint %q", entry.APIURL, want)
	}
	if entry.TLSCertFingerprint != pin {
		t.Errorf("tls pin = %q, want the bundle's %q", entry.TLSCertFingerprint, pin)
	}
}

// TestServerUpdateRefetchKeyNotAuthorized: the refetch path shares the decision
// table, so an unauthorized key is the same hard failure there.
func TestServerUpdateRefetchKeyNotAuthorized(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverUpdateRefetch = true

	clientConfig.Servers["srv"] = config.ServerEntry{
		Host: "example.test", SSHPort: 2222,
		APIURL: "https://example.test:8443", TLSCertFingerprint: "sha256:old",
	}
	hostKey, _ := testHostKey(t)
	if err := config.AddKnownHost("example.test", 2222, hostKey); err != nil {
		t.Fatal(err)
	}
	stubHostKeyScan(t, hostKey, nil)
	stubBootstrapResult(sdk.Credential{}, errors.New("sdk/bootstrap: ssh exited 1: bootstrap: key not authorized"))

	err := runServerUpdate(nil, []string{"srv"})
	if err == nil || !strings.Contains(err.Error(), "not in this server's allowlist") {
		t.Fatalf("err = %v, want the allowlist error", err)
	}
	if got := clientConfig.Servers["srv"].TLSCertFingerprint; got != "sha256:old" {
		t.Errorf("the pin was rewritten despite the failure: %q", got)
	}
}
