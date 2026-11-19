package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/tunnels"
	"github.com/charliek/shed/sdk"
)

// ---------------------------------------------------------------------------
// The held user (plan 037 §3.1).
//
// An entry is held to the user its certificates name. A newly issued
// certificate for another user is refused before anything is written; a stored
// certificate that names no user, or a user the entry does not record, makes
// the entry refused, and a refused entry sends nothing at all — no TCP
// connection, no ssh. Every certificate write is one transaction under the
// server's credential lock, deciding against the row on disk.
// ---------------------------------------------------------------------------

// fileSnapshot is the bytes and identity of a set of files, so a test can
// prove they were neither changed nor rewritten (a rewrite with the same bytes
// still replaces the inode: every write in this package renames a new file in).
type fileSnapshot struct {
	data  map[string][]byte
	infos map[string]os.FileInfo
}

func snapshotFiles(t *testing.T, paths ...string) fileSnapshot {
	t.Helper()
	s := fileSnapshot{data: map[string][]byte{}, infos: map[string]os.FileInfo{}}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("snapshot %s: %v", p, err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		s.data[p], s.infos[p] = b, fi
	}
	return s
}

func (s fileSnapshot) assertUnchanged(t *testing.T) {
	t.Helper()
	for p, want := range s.data {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("%s is gone: %v", p, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s changed", p)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(fi, s.infos[p]) {
			t.Errorf("%s was rewritten (a new file replaced it), want it untouched", p)
		}
	}
}

// freezeCreds fails the test on any write of the server's credential files
// from now on (the lock-held write and the staged commit, the only two this
// package makes inside its transaction), and makes the write fail, so a path
// that must write nothing is proven to. Comparing bytes cannot see a rewrite of
// the same bytes, and comparing inodes is not proof either: a file renamed
// away frees its inode, and the next file created may reuse the number.
func freezeCreds(t *testing.T, name string) {
	t.Helper()
	origWrite, origCommit := writeCredentialsHeldFn, commitStagedHeldFn
	t.Cleanup(func() { writeCredentialsHeldFn, commitStagedHeldFn = origWrite, origCommit })
	writeCredentialsHeldFn = func(n string, certPEM, keyPEM []byte) (string, string, error) {
		if n == name {
			t.Errorf("the credential files of %q were written", n)
			return "", "", errors.New("test: credential writes are frozen")
		}
		return origWrite(n, certPEM, keyPEM)
	}
	commitStagedHeldFn = func(s *config.StagedClientCredentials) error {
		t.Errorf("staged credential files were committed")
		return errors.New("test: credential writes are frozen")
	}
}

// selfSignedCredential is a client certificate the test CA did not issue, with
// the given subject attributes (none: a certificate that names no user, the
// shape of one a pre-0.10 server issued; a bad value or two: a malformed user
// attribute). Nothing that reads a certificate's user verifies its chain, and
// nothing in these tests presents it to a server.
func selfSignedCredential(t *testing.T, pin string, attrs ...pkix.AttributeTypeAndValue) sdk.Credential {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: sshFingerprint("cli-key"), ExtraNames: attrs},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return bundleOf(pin,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		tmpl.SerialNumber.Text(16), farFromExpiry)
}

// tlsPair assembles an issued credential for the TLS stack.
func tlsPair(cred sdk.Credential) (*tls.Certificate, error) {
	cert, err := tls.X509KeyPair([]byte(cred.Bundle.ClientCert), cred.KeyPEM)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

func userAttr(v string) pkix.AttributeTypeAndValue {
	return pkix.AttributeTypeAndValue{Type: servertls.ClientUserOID, Value: v}
}

// asRefusal asserts err is a *config.EntryRefusedError of kind, returned
// UNWRAPPED: its text is the refusal's own, with nothing in front of it.
func asRefusal(t *testing.T, err error, kind config.RefusalKind) *config.EntryRefusedError {
	t.Helper()
	r, ok := config.AsEntryRefused(err)
	if !ok {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if r.Kind != kind {
		t.Fatalf("refusal kind = %d (%v), want %d", r.Kind, r, kind)
	}
	if err.Error() != r.Error() {
		t.Errorf("the refusal was wrapped: %q", err.Error())
	}
	return r
}

// expiredEntry is an mtls entry whose recorded expiry has passed, so building
// a client from it re-mints at once (the proactive window). Whether a refused
// re-mint may keep presenting the stored certificate is read from the
// certificate itself: pass one issued alreadyExpired for "nothing it may keep".
func expiredEntry(t *testing.T, m *mtlsServer, name string, stored sdk.Credential) config.ServerEntry {
	t.Helper()
	e := mtlsEntry(t, m, name, stored)
	e.ClientCertExpiresAt = time.Now().Add(-time.Hour)
	return e
}

// ---------------------------------------------------------------------------
// the rule on a newly issued certificate
// ---------------------------------------------------------------------------

// TestRefreshRefusesAUserChange: the server issues a certificate for another
// user than the one the entry is held to (ssh offered a second key). The
// command fails with a message naming both users, nothing is written, nothing
// is adopted, no request is sent, and the client stays refused: a second
// request fails without running ssh again.
func TestRefreshRefusesAUserChange(t *testing.T) {
	cfgPath := testClientConfig(t)
	m := newMTLSServer(t)
	putServerEntry(t, "srv", expiredEntry(t, m, "srv", m.credential(t, "cli-key", alreadyExpired)))
	certPath, keyPath := config.ClientCredentialPaths("srv")
	before := snapshotFiles(t, cfgPath, certPath, keyPath)
	freezeCreds(t, "srv")

	mints := stubBootstrap(func() sdk.Credential { return m.credentialFor(t, "admin-key", "admin", farFromExpiry) })

	e := clientConfig.Servers["srv"]
	c := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
	_, err := c.GetInfo()
	r := asRefusal(t, err, config.RefusedIssuedUserChange)
	if r.CertUser != "admin" || r.EntryUser != testUser {
		t.Errorf("refusal names %q and %q, want admin and pat", r.CertUser, r.EntryUser)
	}
	for _, want := range []string{`user "admin"`, `held to user "pat"`, "nothing was saved",
		"shed server update srv --refetch --allow-user-change"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, err)
		}
	}
	before.assertUnchanged(t)
	if got := clientConfig.Servers["srv"].User; got != testUser {
		t.Errorf("in-memory user = %q, want pat", got)
	}

	if _, err := c.GetInfo(); err == nil {
		t.Fatal("a refused client sent a second request")
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1 (a refused client never runs ssh again)", got)
	}
	if got := m.accepted.Load(); got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0", got)
	}
}

// TestRefusedRefreshKeepsAStillValidCertificate: the proactive refresh of a
// certificate that is near expiry but still valid for the held user is refused
// (another user's certificate came back); the client keeps presenting the one
// it holds rather than refusing a command that can still succeed, and it does
// not run ssh again for later requests (each would be refused the same way).
// Whether the certificate is still valid is read from the certificate: here
// the cached expiry in the entry is even stale (in the past), and the
// certificate is kept all the same.
func TestRefusedRefreshKeepsAStillValidCertificate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cached time.Duration
	}{
		{"cached expiry inside the window", time.Hour},
		{"cached expiry stale, in the past", -time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := testClientConfig(t)
			m := newMTLSServer(t)
			stored := m.credential(t, "cli-key", farFromExpiry)
			entry := mtlsEntry(t, m, "srv", stored)
			entry.ClientCertExpiresAt = time.Now().Add(tc.cached)
			putServerEntry(t, "srv", entry)
			certPath, keyPath := config.ClientCredentialPaths("srv")
			before := snapshotFiles(t, cfgPath, certPath, keyPath)

			mints := stubBootstrap(func() sdk.Credential { return m.credentialFor(t, "admin-key", "admin", farFromExpiry) })

			e := clientConfig.Servers["srv"]
			c := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
			for i := 0; i < 2; i++ {
				if _, err := c.GetInfo(); err != nil {
					t.Fatalf("GetInfo #%d with the still-valid certificate: %v", i+1, err)
				}
				c.tokens.EnsureFresh() // what a tunnel's capture does before each dial
			}
			if got := m.lastSerial.Load().(string); got != stored.Bundle.CertSerial {
				t.Errorf("presented serial %s, want the held user's %s", got, stored.Bundle.CertSerial)
			}
			if got := mints.Load(); got != 1 {
				t.Errorf("mints = %d, want 1 (a refused refresh is not run again)", got)
			}
			before.assertUnchanged(t)
		})
	}
}

// TestRefreshRefusesACertificateItCannotHold: a new certificate that names no
// user (only a server older than 0.10.0 issues one) is refused with the
// server-too-old message, and one whose user attribute is malformed is refused
// as unreadable (fail closed). Nothing is written either way.
func TestRefreshRefusesACertificateItCannotHold(t *testing.T) {
	cases := []struct {
		name  string
		attrs []pkix.AttributeTypeAndValue
		kind  config.RefusalKind
		says  string
	}{
		{"names no user", nil, config.RefusedIssuedNoUser,
			"names no user: it runs a shed-server older than 0.10.0; nothing was saved. Upgrade the server to 0.10.0, then retry."},
		{"a malformed value", []pkix.AttributeTypeAndValue{userAttr("Pat")}, config.RefusedIssuedUnreadable,
			"cannot be read"},
		{"two user attributes", []pkix.AttributeTypeAndValue{userAttr("pat"), userAttr("admin")}, config.RefusedIssuedUnreadable,
			"cannot be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := testClientConfig(t)
			m := newMTLSServer(t)
			putServerEntry(t, "srv", expiredEntry(t, m, "srv", m.credential(t, "cli-key", alreadyExpired)))
			certPath, keyPath := config.ClientCredentialPaths("srv")
			before := snapshotFiles(t, cfgPath, certPath, keyPath)
			freezeCreds(t, "srv")

			issued := selfSignedCredential(t, m.pin, tc.attrs...)
			stubBootstrap(func() sdk.Credential { return issued })

			e := clientConfig.Servers["srv"]
			_, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo()
			asRefusal(t, err, tc.kind)
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("message = %q, want it to say %q", err, tc.says)
			}
			before.assertUnchanged(t)
			if got := m.accepted.Load(); got != 0 {
				t.Errorf("the server accepted %d connection(s), want 0", got)
			}
		})
	}
}

// TestRefreshAdoptsAndRecords covers what is NOT a mismatch: the same user
// (persisted as today), an entry with no user taking its first named one (the
// user is recorded, in memory and on disk: implicit to named is how a server
// gains a user list), and a named entry issued an implicit certificate (the
// server dropped its list: adopted, and the entry stays held to its user).
func TestRefreshAdoptsAndRecords(t *testing.T) {
	cases := []struct {
		name       string
		storedUser string // the user of the stored certificate and of the entry
		issuedUser string
		wantUser   string
	}{
		{"the same user", "pat", "pat", "pat"},
		{"implicit to named records the user", "", "pat", "pat"},
		{"named to implicit keeps the user", "pat", "", "pat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := testClientConfig(t)
			m := newMTLSServer(t)
			putServerEntry(t, "srv", expiredEntry(t, m, "srv", m.credentialFor(t, "cli-key", tc.storedUser, farFromExpiry)))
			issued := m.credentialFor(t, "cli-key", tc.issuedUser, farFromExpiry)
			mints := stubBootstrap(func() sdk.Credential { return issued })

			e := clientConfig.Servers["srv"]
			if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err != nil {
				t.Fatalf("GetInfo: %v", err)
			}
			if got := mints.Load(); got != 1 {
				t.Errorf("mints = %d, want 1", got)
			}
			for _, cfg := range loadedAndInMemory(t, cfgPath) {
				if got := cfg.Servers["srv"].User; got != tc.wantUser {
					t.Errorf("user = %q, want %q", got, tc.wantUser)
				}
			}
			certPath, _ := config.ClientCredentialPaths("srv")
			if got, _ := os.ReadFile(certPath); string(got) != issued.Bundle.ClientCert {
				t.Error("the issued certificate was not persisted")
			}
			// The persisted entry stands on its own.
			next := clientConfig.Servers["srv"]
			if _, err := NewAPIClientFromNamedEntry("srv", &next, DefaultTimeout).GetInfo(); err != nil {
				t.Fatalf("the persisted entry did not work on its own: %v", err)
			}
			if got := mints.Load(); got != 1 {
				t.Errorf("mints = %d after re-use, want 1", got)
			}
		})
	}
}

// TestCredentialLessEntryRecordsItsFirstUser: an entry with no certificate and
// no user takes the first named user it is issued, and records it.
func TestCredentialLessEntryRecordsItsFirstUser(t *testing.T) {
	cfgPath := testClientConfig(t)
	m := newMTLSServer(t)
	putServerEntry(t, "srv", config.ServerEntry{
		Host: "127.0.0.1", SSHPort: 2222, APIURL: m.srv.URL, TLSCertFingerprint: m.pin,
	})
	stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })

	e := clientConfig.Servers["srv"]
	if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	for _, cfg := range loadedAndInMemory(t, cfgPath) {
		if got := cfg.Servers["srv"].User; got != testUser {
			t.Errorf("user = %q, want pat", got)
		}
	}
}

// ---------------------------------------------------------------------------
// a refused stored certificate sends nothing
// ---------------------------------------------------------------------------

// TestStoredCertificateRefusalSendsNothing: an entry whose stored certificate
// names no user, or a user the entry does not record, fails every command with
// its own message, unwrapped, before any connection: zero bootstrap calls and
// zero TCP connections accepted by the server, through a JSON request, a
// create stream, the tunnel's Connect probe and dial, and Ping and Status.
func TestStoredCertificateRefusalSendsNothing(t *testing.T) {
	cases := []struct {
		name      string
		stored    func(m *mtlsServer) sdk.Credential
		entryUser string
		kind      config.RefusalKind
		says      string
	}{
		{"its certificate names no user", func(m *mtlsServer) sdk.Credential { return selfSignedCredential(t, m.pin) }, "",
			config.RefusedStoredNoUser,
			`entry "srv" cannot be used: its certificate names no user (issued by a shed-server older than 0.10.0); ` +
				"no request was sent. Enrol it again: shed server update srv --refetch"},
		{"its certificate names a user the entry does not record", func(m *mtlsServer) sdk.Credential {
			return m.credential(t, "cli-key", farFromExpiry)
		}, "", config.RefusedStoredUserMismatch,
			`entry "srv" cannot be used: its certificate is for user "pat" but the entry records no user ` +
				"(written by a shed CLI older than 0.10.0, edited by hand, or an interrupted or concurrent enrolment); " +
				"no request was sent. Enrol it again: shed server update srv --refetch"},
		{"its certificate names another user than the entry", func(m *mtlsServer) sdk.Credential {
			return m.credential(t, "cli-key", farFromExpiry)
		}, "admin", config.RefusedStoredUserMismatch,
			`its certificate is for user "pat" but the entry records user "admin" ` +
				"(edited by hand, or an interrupted or concurrent enrolment); no request was sent."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testClientConfig(t)
			m := newMTLSServer(t)
			entry := mtlsEntry(t, m, "srv", tc.stored(m))
			entry.User = tc.entryUser
			putServerEntry(t, "srv", entry)
			mints := stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })

			e := clientConfig.Servers["srv"]
			c := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
			check := func(what string, err error) {
				t.Helper()
				r, ok := config.AsEntryRefused(err)
				if !ok || r.Kind != tc.kind {
					t.Fatalf("%s: err = %v, want the stored-certificate refusal", what, err)
				}
				if err.Error() != r.Error() {
					t.Errorf("%s: the refusal was wrapped: %q", what, err)
				}
				if !strings.Contains(err.Error(), tc.says) {
					t.Errorf("%s: message = %q, want %q", what, err, tc.says)
				}
			}

			_, err := c.GetInfo()
			check("a JSON request", err)
			_, err = c.CreateShedWithProgress(&config.CreateShedRequest{Name: "x"}, false, func(backend.ProgressEvent) {})
			check("a create stream", err)
			if c.Ping() {
				t.Error("Ping reported a refused entry online")
			}
			status, err := c.Status()
			check("Status", err)
			if status != serverStatusRefused {
				t.Errorf("Status = %q, want refused", status)
			}

			target, err := connectTargetFromEntry(&e, "")
			if err != nil {
				t.Fatal(err)
			}
			src := tunnelCredentialSource(c, "srv", &e, target)
			check("the tunnel's credential source", src.Err())
			check("the Connect probe", probeConnectAuth(target, src, "myshed"))
			_, err = tunnels.NewConnectClient(target, src).Dial(context.Background(), "myshed", 8080)
			check("a tunnel dial", err)

			if got := mints.Load(); got != 0 {
				t.Errorf("bootstrap calls = %d, want 0 (a refused entry never runs ssh)", got)
			}
			if got := m.accepted.Load(); got != 0 {
				t.Errorf("the server accepted %d connection(s), want 0", got)
			}
		})
	}
}

// TestRefusedClientNeverReachesSend is the request path's own check, apart
// from the transport backstop below it: sendWithReauth returns the refusal
// without calling send at all.
func TestRefusedClientNeverReachesSend(t *testing.T) {
	refusal := &config.EntryRefusedError{Entry: "srv", Kind: config.RefusedStoredNoUser}
	c := newAPIClientWithSource("https://127.0.0.1:1", "sha256:00", clienttoken.Refused(refusal), DefaultTimeout)
	var sends atomic.Int32
	_, err := c.sendWithReauth(func(clienttoken.Credential) (*http.Response, error) {
		sends.Add(1)
		return nil, context.Canceled
	}, connectFailure)
	if err != refusal {
		t.Errorf("err = %v, want the refusal itself", err)
	}
	if got := sends.Load(); got != 0 {
		t.Errorf("send ran %d time(s) for a refused client, want 0", got)
	}
}

// TestRefusedTransportNeverDials is the backstop: a path that bypassed
// sendWithReauth still cannot reach a refused entry's server.
func TestRefusedTransportNeverDials(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	refusal := &config.EntryRefusedError{Entry: "srv", Kind: config.RefusedStoredNoUser}
	c := newAPIClientWithSource(m.srv.URL, m.pin, clienttoken.Refused(refusal), DefaultTimeout)
	if _, err := c.newHTTPClient(0).Get(m.srv.URL + "/api/info"); err == nil {
		t.Fatal("a request on a refused client's transport succeeded")
	}
	if got := m.accepted.Load(); got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0", got)
	}
}

// TestStoredCertificateWithMalformedUserReEnrolls: a stored certificate whose
// user attribute is malformed is not a refusal but an unusable credential, like
// a pair that does not load: the entry re-enrols.
func TestStoredCertificateWithMalformedUserReEnrolls(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	entry := mtlsEntry(t, m, "srv", selfSignedCredential(t, m.pin, userAttr("Pat")))
	entry.User = testUser
	putServerEntry(t, "srv", entry)
	mints := stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })

	e := clientConfig.Servers["srv"]
	if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1 (the entry re-enrolled)", got)
	}
}

// ---------------------------------------------------------------------------
// the mint-only path (the tunnel daemon)
// ---------------------------------------------------------------------------

// TestMintOnlyRefreshIsHeld: the tunnel daemon's mint-only refresh writes
// nothing. Named (it has the entry's name) it reads the entry's row under the
// config lock at every mint and decides as a persisting refresh does: a
// certificate for a user the row does not record is refused, it never takes up
// a first named user in memory, and a row that now records another user ends
// it. Only a source with no entry name (an ambiguous alias) decides in memory,
// keeping the first named user it adopts.
func TestMintOnlyRefreshIsHeld(t *testing.T) {
	// setup builds the daemon's source the way `shed tunnels start` does: from
	// an API client holding a certificate for entryUser ("" for the implicit
	// user), handed off for the entry named dev-mtls, whose row records
	// entryUser. cached is the recorded expiry of what it holds (the leaf
	// itself is valid for a day): inside the refresh window, a proactive
	// refresh mints.
	setup := func(t *testing.T, entryUser string, cached time.Duration) (*mtlsServer, string, *clienttoken.Source, *sdk.Credential) {
		t.Helper()
		cfgPath := testClientConfig(t)
		m := newMTLSCredentialIssuer(t)
		initial := m.credentialFor(t, "cli-key", entryUser, farFromExpiry)
		cert, err := tlsPair(initial)
		if err != nil {
			t.Fatal(err)
		}
		entry := config.ServerEntry{
			Host: "127.0.0.1", SSHPort: 2222,
			APIURL: "https://localhost:18443", TLSCertFingerprint: m.pin, User: entryUser,
		}
		putServerEntry(t, "dev-mtls", entry)
		client := newAPIClientWithSource(entry.BaseURL(), entry.TLSCertFingerprint,
			clienttoken.New(clienttoken.MTLSCredential(cert, time.Now().Add(cached)), nil), DefaultTimeout)
		next := new(sdk.Credential)
		stubBootstrap(func() sdk.Credential { return *next })
		src := tunnelCredentialSource(client, "dev-mtls", &entry, tunnels.ConnectTarget{TLSPin: m.pin})
		return m, cfgPath, src, next
	}
	refresh := func(src *clienttoken.Source) error {
		_, gen := src.Current()
		_, err := src.Refresh(gen)
		return err
	}
	// sendsNothing: the ended source presents no certificate at all, so no
	// handshake can carry pat's (or anyone's).
	sendsNothing := func(t *testing.T, src *clienttoken.Source) {
		t.Helper()
		if src.Err() == nil {
			t.Error("the refused source is still usable")
		}
		if src.CertificateFor(context.Background()) != nil {
			t.Error("the refused source still presents a certificate")
		}
		if _, err := os.Stat(config.ServerCredsDir("dev-mtls")); !os.IsNotExist(err) {
			t.Errorf("the mint-only path wrote a credential directory (err=%v)", err)
		}
	}

	t.Run("another user than the entry's is refused", func(t *testing.T) {
		m, _, src, next := setup(t, testUser, farFromExpiry)
		*next = m.credentialFor(t, "admin-key", "admin", farFromExpiry)
		asRefusal(t, refresh(src), config.RefusedIssuedUserChange)
		sendsNothing(t, src)
	})

	// The review's second sequence: a daemon holding an implicit certificate
	// for an entry whose row records no user (the server had no user list) is
	// offered pat's certificate (the server gained one). It refuses it, with
	// the remedy, records nothing, and ends: nothing is sent as pat.
	t.Run("a first named user the row does not record is refused", func(t *testing.T) {
		m, cfgPath, src, next := setup(t, "", time.Hour)
		*next = m.credential(t, "cli-key", farFromExpiry)
		err := src.EnsureFreshErr() // the proactive refresh a tunnel's dial runs
		r := asRefusal(t, err, config.RefusedMintOnlyUnrecorded)
		if r.CertUser != testUser {
			t.Errorf("refusal names %q, want pat", r.CertUser)
		}
		for _, want := range []string{`for user "pat"`, `entry "dev-mtls" records no user`,
			"shed server update dev-mtls --refetch", "restart the tunnel"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("message lacks %q:\n%s", want, err)
			}
		}
		if got := heldCertUser(t, src); got == testUser {
			t.Error("the daemon took up pat's certificate")
		}
		sendsNothing(t, src)
		if got := onDisk(t, cfgPath).Servers["dev-mtls"].User; got != "" {
			t.Errorf("the mint-only path recorded user %q", got)
		}
	})

	// The review's first sequence: the daemon is held to pat; another process
	// changes the entry to admin with --allow-user-change. The daemon's next
	// mint (a proactive one, while pat's certificate is still valid) reads the
	// row, finds admin, and ends: pat's certificate is not kept.
	t.Run("a row changed to another user ends it at the next mint", func(t *testing.T) {
		m, cfgPath, src, next := setup(t, testUser, time.Hour)
		asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
			e := c.Servers["dev-mtls"]
			e.User = "admin"
			c.Servers["dev-mtls"] = e
		})
		*next = m.credential(t, "cli-key", farFromExpiry)
		r := asRefusal(t, src.EnsureFreshErr(), config.RefusedEntryUserChanged)
		if r.CertUser != testUser || r.EntryUser != "admin" {
			t.Errorf("refusal names %q and %q, want pat and admin", r.CertUser, r.EntryUser)
		}
		sendsNothing(t, src)
	})

	t.Run("a source with no entry name keeps the first named user it adopts", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSCredentialIssuer(t)
		next := m.credential(t, "cli-key", farFromExpiry)
		stubBootstrap(func() sdk.Credential { return next })
		endpoint := config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222,
			APIURL: "https://localhost:18443", TLSCertFingerprint: m.pin}
		src := clienttoken.New(clienttoken.Credential{Mode: clienttoken.ModeMTLS},
			controlCredentialRefresh(endpoint, "", "127.0.0.1"))
		if err := refresh(src); err != nil {
			t.Fatalf("first named user: %v", err)
		}
		next = m.credentialFor(t, "admin-key", "admin", farFromExpiry)
		r := asRefusal(t, refresh(src), config.RefusedIssuedUserChange)
		if r.EntryUser != testUser {
			t.Errorf("held to %q, want the first adopted pat", r.EntryUser)
		}
	})
}

// ---------------------------------------------------------------------------
// shed server update --refetch and shed server remove
// ---------------------------------------------------------------------------

// refusedEntryAfterAdd adds "srv" for pat over the SSH-first path, then strips
// its user as an older CLI's save would.
func refusedEntryAfterAdd(t *testing.T, m *mtlsServer) string {
	t.Helper()
	cfgPath, _, _ := addedMTLSServer(t, m)
	asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
		e := c.Servers["srv"]
		e.User = ""
		c.Servers["srv"] = e
	})
	reloaded, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig = reloaded
	return cfgPath
}

// TestRefetchRestoresARefusedEntry: `shed server update <name> --refetch`
// builds no client from the entry, so it works on a refused one: it records
// the user and prints it, and the entry works again.
func TestRefetchRestoresARefusedEntry(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true
	m := newMTLSServer(t)
	cfgPath := refusedEntryAfterAdd(t, m)

	e := clientConfig.Servers["srv"]
	if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err == nil {
		t.Fatal("test setup: the stripped entry should be refused")
	}

	serverUpdateRefetch = true
	serverUpdateTrustTOFU = true
	stubBootstrapResult(mtlsCredentialFor(t, m), nil)
	var err error
	out := captureStdout(t, func() { err = runServerUpdate(nil, []string{"srv"}) })
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	if !strings.Contains(out, "  User: pat\n") {
		t.Errorf("the refetch did not print the user:\n%s", out)
	}
	for _, cfg := range loadedAndInMemory(t, cfgPath) {
		if got := cfg.Servers["srv"].User; got != testUser {
			t.Errorf("user = %q, want pat", got)
		}
	}
	e = clientConfig.Servers["srv"]
	if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err != nil {
		t.Fatalf("the refetched entry does not work: %v", err)
	}
}

// TestServerRemoveWorksOnARefusedEntry: removal is the other way back.
func TestServerRemoveWorksOnARefusedEntry(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true
	m := newMTLSServer(t)
	cfgPath := refusedEntryAfterAdd(t, m)

	if err := runServerRemove(nil, []string{"srv"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := onDisk(t, cfgPath).Servers["srv"]; ok {
		t.Error("the refused entry survived its removal")
	}
	if _, err := os.Stat(config.ServerCredsDir("srv")); !os.IsNotExist(err) {
		t.Errorf("its credentials survived (err=%v)", err)
	}
}

// TestRefetchUserChange: a --refetch that is issued another user's
// certificate is refused right after the bootstrap — before the network check,
// with nothing written — unless --allow-user-change asks for the change, which
// is then saved and printed (and in JSON, user and previous_user).
func TestRefetchUserChange(t *testing.T) {
	setup := func(t *testing.T) (*mtlsServer, string) {
		t.Helper()
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true
		m := newMTLSServer(t)
		cfgPath, _, _ := addedMTLSServer(t, m)
		serverUpdateRefetch = true
		serverUpdateTrustTOFU = true
		admin := m.credentialFor(t, "admin-key", "admin", farFromExpiry)
		_, port := hostPort(t, m.srv.URL)
		admin.Bundle.HTTPSPort = port
		stubBootstrapResult(admin, nil)
		return m, cfgPath
	}

	t.Run("without the flag: refused before the network check, nothing written", func(t *testing.T) {
		m, cfgPath := setup(t)
		certPath, keyPath := config.ClientCredentialPaths("srv")
		before := snapshotFiles(t, cfgPath, certPath, keyPath, config.GetKnownHostsPath())
		freezeCreds(t, "srv")
		requests := m.requests.Load()

		err := runServerUpdate(nil, []string{"srv"})
		asRefusal(t, err, config.RefusedIssuedUserChange)
		if got := m.requests.Load(); got != requests {
			t.Errorf("the refused refetch made %d request(s); the network check must not run", got-requests)
		}
		before.assertUnchanged(t)
	})

	t.Run("with the flag: saved and printed", func(t *testing.T) {
		_, cfgPath := setup(t)
		serverUpdateAllowUserChange = true
		var err error
		out := captureStdout(t, func() { err = runServerUpdate(nil, []string{"srv"}) })
		if err != nil {
			t.Fatalf("refetch --allow-user-change: %v", err)
		}
		if !strings.Contains(out, `  User changed from "pat" to "admin"`) {
			t.Errorf("the change was not printed:\n%s", out)
		}
		for _, cfg := range loadedAndInMemory(t, cfgPath) {
			if got := cfg.Servers["srv"].User; got != "admin" {
				t.Errorf("user = %q, want admin", got)
			}
		}
		e := clientConfig.Servers["srv"]
		if _, err := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout).GetInfo(); err != nil {
			t.Fatalf("the changed entry does not work: %v", err)
		}
	})

	t.Run("with the flag, JSON carries user and previous_user", func(t *testing.T) {
		setup(t)
		serverUpdateAllowUserChange = true
		jsonFlag = true
		var err error
		out := captureStdout(t, func() { err = runServerUpdate(nil, []string{"srv"}) })
		if err != nil {
			t.Fatalf("refetch: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		if got["user"] != "admin" || got["previous_user"] != "pat" {
			t.Errorf("JSON = %v, want user admin and previous_user pat", got)
		}
	})

	t.Run("--allow-user-change without --refetch is a usage error", func(t *testing.T) {
		setup(t)
		serverUpdateRefetch = false
		serverUpdateAllowUserChange = true
		err := runServerUpdate(nil, []string{"srv"})
		if err == nil || !strings.Contains(err.Error(), "--allow-user-change only applies with --refetch") {
			t.Fatalf("err = %v, want the usage error", err)
		}
	})
}

// ---------------------------------------------------------------------------
// shed server list
// ---------------------------------------------------------------------------

// TestServerListShowsRefused: STATUS is `refused` for an entry whose stored
// certificate is refused (known without a request) and for one whose re-mint,
// after the server rejected its certificate, was issued another user's (J4);
// each message is printed once to stderr.
func TestServerListShowsRefused(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	m := newMTLSServer(t)

	nouser := mtlsEntry(t, m, "nouser", selfSignedCredential(t, m.pin))
	putServerEntry(t, "nouser", nouser)
	other := mtlsEntry(t, m, "other", m.credential(t, "cli-key", farFromExpiry))
	other.User = ""
	putServerEntry(t, "other", other)
	rejected := m.credential(t, "cli-key", farFromExpiry)
	putServerEntry(t, "changed", mtlsEntry(t, m, "changed", rejected))
	m.revoke(rejected.Bundle.CertSerial)
	putServerEntry(t, "ok", mtlsEntry(t, m, "ok", m.credential(t, "cli-key", farFromExpiry)))

	mints := stubBootstrap(func() sdk.Credential { return m.credentialFor(t, "admin-key", "admin", farFromExpiry) })
	jsonFlag = true
	var out string
	var err error
	stderr := captureStderr(t, func() {
		out = captureStdout(t, func() { err = runServerList(nil, nil) })
	})
	if err != nil {
		t.Fatalf("server list: %v", err)
	}
	var rows []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	want := map[string]string{"nouser": "refused", "other": "refused", "changed": "refused", "ok": "online"}
	for _, r := range rows {
		if r.Status != want[r.Name] {
			t.Errorf("%s: status %q, want %q", r.Name, r.Status, want[r.Name])
		}
	}
	for _, msg := range []string{`entry "nouser" cannot be used`, `entry "other" cannot be used`, `entry "changed" is held to user "pat"`} {
		if n := strings.Count(stderr, msg); n != 1 {
			t.Errorf("stderr has %q %d time(s), want once:\n%s", msg, n, stderr)
		}
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1 (only the rejected entry re-mints)", got)
	}
	// "ok" made one request and "changed" one (refused with a 401); the
	// stored refusals made none.
	if got := m.requests.Load(); got != 2 {
		t.Errorf("the server saw %d request(s), want 2", got)
	}
}

// ---------------------------------------------------------------------------
// interleavings: the one write transaction
// ---------------------------------------------------------------------------

// TestTwoFirstEnrolmentsCannotBothWin: two enrolments start from `user: ""`
// (built from the same row) and are issued different named users, and they
// really overlap. The first is stopped inside its transaction, after it read
// the row and while it holds the server's credential lock, just before it
// writes; the second then runs its whole mint and reaches the transaction. It
// must wait: while the first holds the lock the second neither decides nor
// writes. Once the first records pat and releases, the second finds pat under
// the lock and is refused with nothing written.
func TestTwoFirstEnrolmentsCannotBothWin(t *testing.T) {
	cfgPath := testClientConfig(t)
	m := newMTLSCredentialIssuer(t)
	entry := config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: "https://localhost:18443", TLSCertFingerprint: m.pin}
	putServerEntry(t, "srv", entry)

	patCred := m.credential(t, "cli-key", farFromExpiry)
	adminCred := m.credentialFor(t, "admin-key", "admin", farFromExpiry)
	var mintsDone atomic.Int32
	var issueAdmin atomic.Bool
	bootstrapCredentialFn = func(string, int, string, string, string) (sdk.Credential, error) {
		defer mintsDone.Add(1)
		if issueAdmin.Load() {
			return adminCred, nil
		}
		return patCred, nil
	}

	// The first write blocks at the seam (inside the lock) until released.
	// Any later write is the second enrolment's, which must never happen.
	origWrite := writeCredentialsHeldFn
	t.Cleanup(func() { writeCredentialsHeldFn = origWrite })
	entered, release := make(chan struct{}), make(chan struct{})
	var writes atomic.Int32
	writeCredentialsHeldFn = func(n string, certPEM, keyPEM []byte) (string, string, error) {
		if writes.Add(1) == 1 {
			close(entered)
			<-release
			return origWrite(n, certPEM, keyPEM)
		}
		t.Errorf("the second enrolment wrote credential files")
		return "", "", errors.New("test: the second enrolment must not write")
	}

	first := controlCredentialRefresh(entry, "srv", "srv")
	second := controlCredentialRefresh(entry, "srv", "srv") // built from the same stale row

	firstDone := make(chan error, 1)
	go func() { _, err := first(); firstDone <- err }()
	<-entered // the first holds the lock, has decided, and is about to write

	issueAdmin.Store(true)
	secondDone := make(chan error, 1)
	go func() { _, err := second(); secondDone <- err }()
	for mintsDone.Load() < 2 {
		runtime.Gosched() // wait for the second's mint: it is now at the transaction
	}
	select {
	case err := <-secondDone:
		t.Fatalf("the second enrolment finished (%v) while the first held the lock", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first enrolment: %v", err)
	}
	r := asRefusal(t, <-secondDone, config.RefusedIssuedUserChange)
	if r.EntryUser != testUser || r.CertUser != "admin" {
		t.Errorf("refusal = %v, want admin refused against the first's pat", r)
	}
	if got := writes.Load(); got != 1 {
		t.Errorf("writes = %d, want only the first's", got)
	}
	certPath, _ := config.ClientCredentialPaths("srv")
	if got, _ := os.ReadFile(certPath); string(got) != patCred.Bundle.ClientCert {
		t.Error("the certificate on disk is not the first enrolment's")
	}
	if got := onDisk(t, cfgPath).Servers["srv"].User; got != testUser {
		t.Errorf("user on disk = %q, want pat", got)
	}
}

// TestRefreshRefusedAfterAnotherProcessChangedTheUser: a refresh started as
// pat, after another process changed the entry to admin (--allow-user-change),
// is refused under the lock: the certificate it was issued is pat's, the row
// says admin, and nothing is written — not even rewritten with the same bytes.
func TestRefreshRefusedAfterAnotherProcessChangedTheUser(t *testing.T) {
	cfgPath := testClientConfig(t)
	m := newMTLSCredentialIssuer(t)
	stored := m.credential(t, "cli-key", farFromExpiry)
	certPath, keyPath, err := config.WriteClientCredentials("srv", []byte(stored.Bundle.ClientCert), stored.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	entry := config.ServerEntry{
		Host: "127.0.0.1", SSHPort: 2222, APIURL: "https://localhost:18443", TLSCertFingerprint: m.pin,
		AuthMode: config.AuthModeMTLS, ClientCertFile: certPath, ClientKeyFile: keyPath, User: testUser,
	}
	putServerEntry(t, "srv", entry)
	refresh := controlCredentialRefresh(entry, "srv", "srv")

	asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
		e := c.Servers["srv"]
		e.User = "admin"
		c.Servers["srv"] = e
	})
	before := snapshotFiles(t, cfgPath, certPath, keyPath)
	freezeCreds(t, "srv")

	stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })
	_, err = refresh()
	r := asRefusal(t, err, config.RefusedEntryUserChanged)
	if r.EntryUser != "admin" || r.CertUser != testUser {
		t.Errorf("refusal = %v, want the closure held to pat refused against the row's admin", r)
	}
	if !r.InvalidatesHeld() {
		t.Error("the refusal does not withdraw the certificate this process holds")
	}
	before.assertUnchanged(t)
}

// TestObservedUserChangeWithdrawsTheHeldCertificate is the review's A/B
// interleaving. A holds pat's certificate, near expiry but still valid. While
// A re-enrols, B refetches with --allow-user-change: the row now records
// admin and admin's pair is installed. A is issued pat's certificate; under
// the lock it finds the row records admin. That is not a refused replacement
// that may keep the held certificate: the entry is no longer pat's, so A's
// client ends at once and never sends pat's certificate again — no request,
// no connection, no second ssh — and B's files are untouched.
func TestObservedUserChangeWithdrawsTheHeldCertificate(t *testing.T) {
	cfgPath := testClientConfig(t)
	m := newMTLSServer(t)
	stored := m.credential(t, "cli-key", farFromExpiry)
	entry := mtlsEntry(t, m, "srv", stored)
	entry.ClientCertExpiresAt = time.Now().Add(time.Hour) // inside the window: A re-enrols at build
	putServerEntry(t, "srv", entry)
	certPath, keyPath := config.ClientCredentialPaths("srv")

	adminCred := m.credentialFor(t, "admin-key", "admin", farFromExpiry)
	var afterB fileSnapshot
	mints := stubBootstrap(func() sdk.Credential {
		// B lands while A's bootstrap runs: admin's pair, then admin's row.
		if _, _, err := config.WriteClientCredentials("srv", []byte(adminCred.Bundle.ClientCert), adminCred.KeyPEM); err != nil {
			t.Fatal(err)
		}
		asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
			e := c.Servers["srv"]
			e.User = "admin"
			e.ClientCertExpiresAt = adminCred.Bundle.ExpiresAt
			c.Servers["srv"] = e
		})
		afterB = snapshotFiles(t, cfgPath, certPath, keyPath)
		freezeCreds(t, "srv")
		return m.credential(t, "cli-key", farFromExpiry) // pat again
	})

	e := clientConfig.Servers["srv"]
	c := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
	_, err := c.GetInfo()
	r := asRefusal(t, err, config.RefusedEntryUserChanged)
	if r.EntryUser != "admin" || r.CertUser != testUser {
		t.Errorf("refusal = %v, want pat withdrawn against admin", r)
	}
	if c.tokens.Err() == nil {
		t.Fatal("the client still holds pat's certificate as usable")
	}
	c.tokens.EnsureFresh()
	if _, err := c.GetInfo(); err == nil {
		t.Fatal("a second request was sent")
	}
	if got := m.accepted.Load(); got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0 (pat's certificate was sent again)", got)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1", got)
	}
	afterB.assertUnchanged(t)
	if got := onDisk(t, cfgPath).Servers["srv"].User; got != "admin" {
		t.Errorf("user on disk = %q, want B's admin", got)
	}
}

// TestRefetchRefusedAfterAnotherProcessChangedTheUser: a --refetch reads the
// entry at its start; when another process changes the entry's user while the
// refetch's bootstrap runs, the refetch finds the change under the lock right
// after the bootstrap — before its network check and before it stages any
// file — and saves nothing, rather than overwrite it.
func TestRefetchRefusedAfterAnotherProcessChangedTheUser(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU = true
	m := newMTLSServer(t)
	cfgPath, _, _ := addedMTLSServer(t, m)
	certPath, keyPath := config.ClientCredentialPaths("srv")

	serverUpdateRefetch = true
	serverUpdateTrustTOFU = true
	issued := mtlsCredentialFor(t, m)
	var before fileSnapshot
	var requests int64
	bootstrapCredentialFn = func(string, int, string, string, string) (sdk.Credential, error) {
		// The other process lands while this one's bootstrap runs.
		asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
			e := c.Servers["srv"]
			e.User = "admin"
			c.Servers["srv"] = e
		})
		before = snapshotFiles(t, cfgPath, certPath, keyPath)
		freezeCreds(t, "srv")
		requests = m.requests.Load()
		return issued, nil
	}
	origStage := stageCredentialsFn
	t.Cleanup(func() { stageCredentialsFn = origStage })
	stageCredentialsFn = func(name string, certPEM, keyPEM []byte) (*config.StagedClientCredentials, error) {
		t.Error("a refetch that is already refused staged its certificate")
		return origStage(name, certPEM, keyPEM)
	}

	err := runServerUpdate(nil, []string{"srv"})
	asRefusal(t, err, config.RefusedEntryChanged)
	if !strings.Contains(err.Error(), `entry "srv" changed while this command ran; nothing was saved. Run it again.`) {
		t.Errorf("message = %q", err)
	}
	if got := m.requests.Load() - requests; got != 0 {
		t.Errorf("the refused refetch made %d request(s); the network check must not run", got)
	}
	before.assertUnchanged(t)
	if got := onDisk(t, cfgPath).Servers["srv"].User; got != "admin" {
		t.Errorf("user on disk = %q, want the other process's admin", got)
	}
}
