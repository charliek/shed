package main

// Tests for the binding of an entry to its identity_file at the points it is
// checked (plan 037 P-a, A7's third review): a stored control token is never
// presented for an entry with an identity file; an enrolment is adopted only
// against the identity it was made with; an identity file that cannot be used
// is an entry refusal everywhere; `ssh-config --install` regenerates the
// managed block whole when it must change, and refuses markers that frame no
// single block; and every new message escapes the path it prints.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/sshconfig"
	"github.com/charliek/shed/internal/tunnels"
	"github.com/charliek/shed/sdk"
)

// tokenRecorder is a plain-HTTP server that records every request and the
// bearer token it carried.
type tokenRecorder struct {
	srv      *httptest.Server
	requests atomic.Int32
	auth     atomic.Value // string
}

func newTokenRecorder(t *testing.T) *tokenRecorder {
	t.Helper()
	rec := &tokenRecorder{}
	rec.auth.Store("")
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.requests.Add(1)
		rec.auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"srv","http_port":0,"ssh_port":2222,"auth_mode":"token"}`)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

// TestStoredTokenNeverPresentedForAnIdentityEntry (P-a): a control token names
// no SSH key, so an entry with an identity_file that holds one (written
// before the identity_file was set) is a refused client, through the named
// and the unnamed constructor, with a static token and with a refreshable
// one, and so is the tunnel's source built from it: the token never reaches
// the server, no ssh is run, and the refusal names the refetch. The negative
// control is the same entry without its identity_file, whose token the server
// does see.
func TestStoredTokenNeverPresentedForAnIdentityEntry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expires time.Time
		named   bool
	}{
		{"a static token, the named client", time.Time{}, true},
		{"a static token, the unnamed client", time.Time{}, false},
		{"a refreshable token, the named client", time.Now().Add(24 * time.Hour), true},
		{"a refreshable token, the unnamed client", time.Now().Add(24 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testClientConfig(t)
			rec := newTokenRecorder(t)
			key := writeKey(t, t.TempDir(), "id_work")
			entry := config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: rec.srv.URL,
				AuthMode: config.AuthModeToken, ControlToken: "stored-token-T", ControlTokenExpiresAt: tc.expires,
				IdentityFile: key}
			putServerEntry(t, "srv", entry)
			calls, _ := stubBootstrapIdentity(sdk.Credential{}, errors.New("test: the bootstrap must not run"))
			build := func(e config.ServerEntry) *APIClient {
				if tc.named {
					return NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
				}
				return NewAPIClientFromEntry(&e, DefaultTimeout)
			}

			c := build(entry)
			_, err := c.GetInfo()
			r := asRefusal(t, err, config.RefusedStoredTokenForKey)
			if r.Entry != "srv" || r.IdentityFile != key || !strings.Contains(err.Error(), "shed server update srv --refetch") {
				t.Errorf("refusal = %v, want srv refused, naming its identity file and the refetch", err)
			}
			if status, serr := c.Status(); status != serverStatusRefused || serr == nil {
				t.Errorf("Status = %q, %v; want refused", status, serr)
			}
			if src := tunnelCredentialSource(c, "srv", &entry, tunnels.ConnectTarget{TLSPin: "sha256:00"}); src == nil || src.Err() == nil {
				t.Error("the tunnel's source is not refused")
			}
			if n := rec.requests.Load(); n != 0 {
				t.Fatalf("the server saw %d request(s) (last with %q), want 0", n, rec.auth.Load())
			}
			if calls.Load() != 0 {
				t.Errorf("the bootstrap ran %d time(s), want 0", calls.Load())
			}

			// The negative control: without an identity file the token is
			// presented, so the zero above would have been seen otherwise.
			entry.IdentityFile = ""
			putServerEntry(t, "srv", entry)
			if _, err := build(entry).GetInfo(); err != nil {
				t.Fatalf("GetInfo without an identity file: %v", err)
			}
			if rec.requests.Load() != 1 || rec.auth.Load() != "Bearer stored-token-T" {
				t.Fatalf("without an identity file the token was not presented (%d request(s), %q)",
					rec.requests.Load(), rec.auth.Load())
			}
		})
	}
}

// setIdentityFileAsAnotherProcess is `shed server update srv --identity-file
// <path>` run by another process: only the row's identity_file changes.
func setIdentityFileAsAnotherProcess(t *testing.T, cfgPath, path string) {
	t.Helper()
	asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
		e := c.Servers["srv"]
		e.IdentityFile = path
		c.Servers["srv"] = e
	})
}

// bootstrapDuring installs a bootstrap that runs during() (what another
// process does while ssh runs) and then answers with cred.
func bootstrapDuring(cred sdk.Credential, during func()) *atomic.Int32 {
	var calls atomic.Int32
	bootstrapCredentialFn = func(string, int, string, string, string) (sdk.Credential, error) {
		calls.Add(1)
		during()
		return cred, nil
	}
	return &calls
}

// TestEnrolmentAdoptedOnlyAgainstItsIdentity (P-a, the review's sequence): a
// refresh enrols with K1, and while ssh runs another process sets the entry's
// identity_file to K2 (or replaces the key file at K1's path with K2). The
// certificate the server issues for K1 is not adopted: nothing is written, no
// user is recorded, and nothing made for K1 is presented afterwards, not even
// a certificate the process already held. The tunnel daemon's mint-only
// refresh is held the same way, with and without a row to read.
func TestEnrolmentAdoptedOnlyAgainstItsIdentity(t *testing.T) {
	t.Run("a first enrolment while another process sets K2", func(t *testing.T) {
		cfgPath := testClientConfig(t)
		m := newMTLSServer(t)
		dir := t.TempDir()
		k1, k2 := writeKey(t, dir, "id_k1"), writeKey(t, dir, "id_k2")
		entry := config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: m.srv.URL, TLSCertFingerprint: m.pin,
			AuthMode: config.AuthModeMTLS, IdentityFile: k1}
		putServerEntry(t, "srv", entry)
		calls := bootstrapDuring(credentialForKey(t, m, keyFingerprint(t, k1), testUser),
			func() { setIdentityFileAsAnotherProcess(t, cfgPath, k2) })

		c := NewAPIClientFromNamedEntry("srv", &entry, DefaultTimeout)
		_, err := c.GetInfo()
		asRefusal(t, err, config.RefusedEntryUnreadable)
		row := onDisk(t, cfgPath).Servers["srv"]
		if row.User != "" || row.IdentityFile != k2 || row.ClientCertFile != "" {
			t.Errorf("row after = %+v, want no user recorded and only the other process's K2", row)
		}
		if certPath, keyPath := config.ClientCredentialPaths("srv"); fileExists(certPath) || fileExists(keyPath) {
			t.Error("credential files were written")
		}
		if calls.Load() != 1 || m.accepted.Load() != 0 {
			t.Errorf("bootstrap ran %d time(s), the server accepted %d connection(s); want 1 and 0",
				calls.Load(), m.accepted.Load())
		}
	})

	// A process that holds a valid certificate for K1 renews it (it is near
	// expiry, so the client's build renews at once).
	renewal := func(t *testing.T, change func(cfgPath, k1, k2 string)) {
		t.Helper()
		cfgPath := testClientConfig(t)
		m := newMTLSServer(t)
		dir := t.TempDir()
		k1, k2 := writeKey(t, dir, "id_k1"), writeKey(t, dir, "id_k2")
		fp1 := keyFingerprint(t, k1)
		certPEM, keyPEM, serial := m.issueForSubject(t, fp1, testUser, time.Hour)
		entry := mtlsEntry(t, m, "srv", bundleOf(m.pin, certPEM, keyPEM, serial, time.Hour))
		entry.IdentityFile = k1
		putServerEntry(t, "srv", entry)
		certPath, keyPath := config.ClientCredentialPaths("srv")
		before := snapshotFiles(t, certPath, keyPath)
		calls := bootstrapDuring(credentialForKey(t, m, fp1, testUser), func() { change(cfgPath, k1, k2) })

		c := NewAPIClientFromNamedEntry("srv", &entry, DefaultTimeout)
		_, err := c.GetInfo()
		if !clienttoken.IsRefusal(err) {
			t.Fatalf("GetInfo = %v, want a refusal", err)
		}
		before.assertUnchanged(t)
		if got := onDisk(t, cfgPath).Servers["srv"].User; got != testUser {
			t.Errorf("row user = %q, want %q unchanged", got, testUser)
		}
		if calls.Load() != 1 {
			t.Errorf("bootstrap ran %d time(s), want 1", calls.Load())
		}
		if n, shown := m.accepted.Load(), m.shownCNs(); n != 0 || len(shown) != 0 {
			t.Errorf("after the refusal the server accepted %d connection(s) and was shown %q; want none", n, shown)
		}
	}

	t.Run("a renewal while another process sets K2", func(t *testing.T) {
		renewal(t, func(cfgPath, _, k2 string) { setIdentityFileAsAnotherProcess(t, cfgPath, k2) })
	})

	t.Run("a renewal while the key at K1's path is replaced", func(t *testing.T) {
		renewal(t, func(_, k1, _ string) { writeKeyAt(t, k1, nil) })
	})

	// The tunnel daemon: a source holding a valid K1 certificate near expiry,
	// whose entry's row now names K2.
	t.Run("a tunnel daemon's mint after the entry changed to K2", func(t *testing.T) {
		cfgPath := testClientConfig(t)
		m := newMTLSCredentialIssuer(t)
		dir := t.TempDir()
		k1, k2 := writeKey(t, dir, "id_k1"), writeKey(t, dir, "id_k2")
		fp1 := keyFingerprint(t, k1)
		entry := config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: "https://127.0.0.1:18443",
			TLSCertFingerprint: m.pin, AuthMode: config.AuthModeMTLS, IdentityFile: k1, User: testUser}
		putServerEntry(t, "srv", entry)
		src := heldSource(t, m, fp1, controlCredentialMintOnly(entry, "srv"))
		setIdentityFileAsAnotherProcess(t, cfgPath, k2)
		bootstrapDuring(credentialForKey(t, m, fp1, testUser), func() {})

		if err := src.EnsureFreshErr(); !clienttoken.IsRefusal(err) {
			t.Fatalf("EnsureFreshErr = %v, want a refusal", err)
		}
		if src.Err() == nil || src.CertificateFor(context.Background()) != nil {
			t.Error("the source still presents the K1 certificate it held")
		}
	})

	t.Run("a mint with no row while the key at its path is replaced", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSCredentialIssuer(t)
		k1 := writeKey(t, t.TempDir(), "id_k1")
		fp1 := keyFingerprint(t, k1)
		entry := config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: "https://127.0.0.1:18443",
			TLSCertFingerprint: m.pin, AuthMode: config.AuthModeMTLS, IdentityFile: k1, User: testUser}
		refresh := controlCredentialRefreshHeld(entry, "", "", "srv", &heldUser{name: testUser})
		bootstrapDuring(credentialForKey(t, m, fp1, testUser), func() { writeKeyAt(t, k1, nil) })
		if _, err := refresh(); !clienttoken.IsRefusal(err) {
			t.Fatalf("refresh = %v, want a refusal", err)
		}
	})
}

// TestAddAndRefetchAdoptOnlyTheKeyAsItIsNow (the third review's P1): `server
// add` and `server update --refetch` with an identity file learn its key
// before ssh runs, and the server issues a certificate for that key; if the
// key at the same path is replaced meanwhile (while the bootstrap runs, or
// after the network check, just before the commit), the issuance is refused:
// nothing more is presented to the server, nothing is staged or committed,
// and config.yaml is unchanged. The negative control of each is the same run
// with the key left alone, which presents the certificate and saves it.
func TestAddAndRefetchAdoptOnlyTheKeyAsItIsNow(t *testing.T) {
	// replaceAt installs a bootstrap answering cred that replaces the key file
	// at its path at w (during the bootstrap, while the server answers the
	// network check, or when the credential is staged after it); untouched,
	// the control, leaves it. staged counts the credentials written to
	// staging.
	type when int
	const (
		untouched when = iota
		duringBootstrap
		duringCheck
		atStage
	)
	var staged atomic.Int32
	replaceAt := func(t *testing.T, w when, m *mtlsServer, key string, cred sdk.Credential) {
		t.Helper()
		staged.Store(0)
		bootstrapDuring(cred, func() {
			if w == duringBootstrap {
				writeKeyAt(t, key, nil)
			}
		})
		if w == duringCheck {
			var once sync.Once
			hook := func() { once.Do(func() { writeKeyAt(t, key, nil) }) }
			m.onRequest.Store(&hook)
			t.Cleanup(func() { m.onRequest.Store(nil) })
		}
		origStage := stageCredentialsFn
		t.Cleanup(func() { stageCredentialsFn = origStage })
		stageCredentialsFn = func(name string, certPEM, keyPEM []byte) (*config.StagedClientCredentials, error) {
			staged.Add(1)
			if w == atStage {
				writeKeyAt(t, key, nil)
			}
			return origStage(name, certPEM, keyPEM)
		}
	}
	// wantStaged is how many credentials may reach staging: none unless the
	// key was still the same when staging began.
	wantStaged := func(w when) int32 {
		if w == atStage || w == untouched {
			return 1
		}
		return 0
	}
	cases := []struct {
		name string
		when when
	}{
		{"the key replaced while the bootstrap runs", duringBootstrap},
		{"the key replaced while the server answers the network check", duringCheck},
		{"the key replaced after the network check, before the commit", atStage},
		{"negative control: the key left alone", untouched},
	}

	t.Run("server add", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cfgPath := testClientConfig(t)
				withCleanAddFlags(t)
				serverAddTrustTOFU = true
				key := writeKey(t, t.TempDir(), "id_work")
				serverAddIdentityFile = key
				m := newMTLSServer(t)
				host, _ := hostPort(t, m.srv.URL)
				hostKey, _ := testHostKey(t)
				stubHostKeyScan(t, hostKey, nil)
				replaceAt(t, tc.when, m, key, credentialForKey(t, m, keyFingerprint(t, key), testUser))

				var err error
				captureStdout(t, func() { err = runServerAdd(nil, []string{host}) })
				if got := staged.Load(); got != wantStaged(tc.when) {
					t.Errorf("%d credential(s) reached staging, want %d", got, wantStaged(tc.when))
				}
				certPath, _ := config.ClientCredentialPaths("srv")
				if tc.when == untouched {
					if err != nil || !fileExists(cfgPath) || !fileExists(certPath) || m.requests.Load() == 0 {
						t.Fatalf("add = %v (config written %v, certificate %v, %d request(s)); want it saved after its check",
							err, fileExists(cfgPath), fileExists(certPath), m.requests.Load())
					}
					return
				}
				r := asRefusal(t, err, config.RefusedEntryUnreadable)
				if !strings.Contains(r.Error(), "the key in its identity_file changed while this command ran") {
					t.Errorf("refusal = %v", r)
				}
				if fileExists(cfgPath) || fileExists(certPath) {
					t.Errorf("something was saved: config %v, certificate %v", fileExists(cfgPath), fileExists(certPath))
				}
				// While the bootstrap: nothing presented at all. Later: only
				// the network check that came before, or with, the change.
				want := int64(0)
				if tc.when == atStage || tc.when == duringCheck {
					want = 1
				}
				if got := m.requests.Load(); got != want {
					t.Errorf("the server got %d request(s), want %d", got, want)
				}
			})
		}
	})

	t.Run("server update --refetch", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				testClientConfig(t)
				withCleanAddFlags(t)
				serverAddTrustTOFU = true
				m := newMTLSServer(t)
				cfgPath, _, _ := addedMTLSServer(t, m)
				serverUpdateRefetch, serverUpdateTrustTOFU = true, true
				key := writeKey(t, t.TempDir(), "id_work")
				serverUpdateIdentityFile = optionalString{value: key, set: true}
				certPath, keyPath := config.ClientCredentialPaths("srv")
				before := snapshotFiles(t, cfgPath, certPath, keyPath)
				requests := m.requests.Load()
				replaceAt(t, tc.when, m, key, credentialForKey(t, m, keyFingerprint(t, key), testUser))

				var err error
				captureStdout(t, func() { err = runServerUpdate(nil, []string{"srv"}) })
				if got := staged.Load(); got != wantStaged(tc.when) {
					t.Errorf("%d credential(s) reached staging, want %d", got, wantStaged(tc.when))
				}
				if tc.when == untouched {
					if err != nil || onDisk(t, cfgPath).Servers["srv"].IdentityFile != key || m.requests.Load() == requests {
						t.Fatalf("refetch = %v (%d new request(s)); want the new key saved after its check",
							err, m.requests.Load()-requests)
					}
					return
				}
				r := asRefusal(t, err, config.RefusedEntryUnreadable)
				if !strings.Contains(r.Error(), "the key in its identity_file changed while this command ran") {
					t.Errorf("refusal = %v", r)
				}
				before.assertUnchanged(t)
				want := requests
				if tc.when == atStage || tc.when == duringCheck {
					want++
				}
				if got := m.requests.Load(); got != want {
					t.Errorf("the server got %d new request(s), want %d", got-requests, want-requests)
				}
			})
		}
	})
}

// heldSource is a credential source holding a valid certificate for testUser
// made for the key fingerprint subject, an hour from its end (inside the
// refresh window, so EnsureFreshErr renews it), with refresh as its refresh.
func heldSource(t *testing.T, m *mtlsServer, subject string, refresh func() (clienttoken.Credential, error)) *clienttoken.Source {
	t.Helper()
	certPEM, keyPEM, _ := m.issueForSubject(t, subject, testUser, time.Hour)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return clienttoken.New(clienttoken.MTLSCredential(&cert, time.Now().Add(time.Hour)), refresh)
}

// TestIdentityFileRefusalEndsTheSource (the review's tunnel sequence): a
// tunnel's source holds a valid certificate for the entry's key, and the key
// file is removed. Its next renewal meets the identity file's refusal, and
// that ends the source: the held certificate is not presented again (it can
// no longer be shown to be the key's), and no ssh is run.
func TestIdentityFileRefusalEndsTheSource(t *testing.T) {
	testClientConfig(t)
	m := newMTLSCredentialIssuer(t)
	key := writeKey(t, t.TempDir(), "id_work")
	entry := config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: "https://127.0.0.1:18443",
		TLSCertFingerprint: m.pin, AuthMode: config.AuthModeMTLS, IdentityFile: key, User: testUser}
	putServerEntry(t, "srv", entry)
	src := heldSource(t, m, keyFingerprint(t, key), controlCredentialMintOnly(entry, "srv"))
	calls, _ := stubBootstrapIdentity(sdk.Credential{}, errors.New("test: the bootstrap must not run"))
	if src.CertificateFor(context.Background()) == nil {
		t.Fatal("the source holds no certificate to begin with")
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}

	err := src.EnsureFreshErr()
	asIdentityRefusal(t, err, "srv", "does not exist")
	if src.Err() == nil {
		t.Error("the source was not ended by the identity file's refusal")
	}
	if src.CertificateFor(context.Background()) != nil {
		t.Error("the held certificate is still presented")
	}
	if calls.Load() != 0 {
		t.Errorf("the bootstrap ran %d time(s), want 0", calls.Load())
	}
}

// TestIdentityFileRefusedWithNoStoredCertificate: an entry with no stored
// certificate (an mtls entry whose files are gone, and a secure entry with no
// credential at all) whose identity file cannot be used is a refused client:
// nothing is sent, not even a request without a certificate.
func TestIdentityFileRefusedWithNoStoredCertificate(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
	}{
		{"an mtls entry with no certificate", config.AuthModeMTLS},
		{"a secure entry with no credential", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testClientConfig(t)
			m := newMTLSServer(t)
			entry := config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: m.srv.URL, TLSCertFingerprint: m.pin,
				AuthMode: tc.mode, IdentityFile: filepath.Join(t.TempDir(), "removed")}
			putServerEntry(t, "srv", entry)
			calls, _ := stubBootstrapIdentity(sdk.Credential{}, errors.New("test: the bootstrap must not run"))

			c := NewAPIClientFromNamedEntry("srv", &entry, DefaultTimeout)
			_, err := c.GetInfo()
			asIdentityRefusal(t, err, "srv", "does not exist")
			if !clienttoken.IsRefusal(err) {
				t.Error("the identity file's error is not a refusal")
			}
			if status, _ := c.Status(); status != serverStatusRefused {
				t.Errorf("Status = %q, want refused", status)
			}
			if n := m.accepted.Load(); n != 0 || calls.Load() != 0 {
				t.Errorf("the server accepted %d connection(s) and the bootstrap ran %d time(s); want 0 and 0", n, calls.Load())
			}
		})
	}
}

// TestSSHConfigShowsAnIdentityFileRefusal: `shed ssh-config` lists sheds from
// every server; one whose entry's identity file cannot be used is a refused
// server, its message printed, not a server that could not be reached.
func TestSSHConfigShowsAnIdentityFileRefusal(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	key := writeKey(t, t.TempDir(), "id_work")
	entry := storeCertForKey(t, m, key, key)
	putServerEntry(t, "srv", entry)
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	stubBootstrapIdentity(sdk.Credential{}, errors.New("test: the bootstrap must not run"))

	var err error
	stderr := captureStderr(t, func() { _, err = getAllShedsInfo() })
	if err == nil || !strings.Contains(err.Error(), "1 refused") {
		t.Errorf("getAllShedsInfo err = %v, want the command to fail with one refused server", err)
	}
	if !strings.Contains(stderr, `entry "srv": identity_file`) || !strings.Contains(stderr, "does not exist") {
		t.Errorf("stderr = %q, want the identity file's refusal", stderr)
	}
	if n := m.accepted.Load(); n != 0 {
		t.Errorf("the server accepted %d connection(s), want 0", n)
	}
}

// installFixture runs `shed ssh-config --install` (JSON) for sheds against a
// throwaway ~/.ssh/config holding content, returning the file after, the
// result and the error.
type installFixture struct {
	path       string
	knownHosts string
}

func newInstallFixture(t *testing.T) installFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return installFixture{path: filepath.Join(home, ".ssh", "config"), knownHosts: config.GetKnownHostsPath()}
}

func (f installFixture) stanza(name, user string) sshconfig.Entry {
	return sshconfig.Entry{Name: name, Host: "shed.example.test", Port: 2222, User: user, KnownHostsFile: f.knownHosts}
}

func (f installFixture) install(t *testing.T, content string, sheds []shedInfo) (string, sshConfigInstallResult, error) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	origJSON := jsonFlag
	t.Cleanup(func() { jsonFlag = origJSON })
	jsonFlag = true
	entries, err := generateEntries(sheds)
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	out := captureStdout(t, func() { runErr = runSSHConfigInstall(entries, sheds) })
	var result struct {
		Details sshConfigInstallResult `json:"details"`
	}
	if runErr == nil {
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("JSON %s: %v", out, err)
		}
	}
	return string(must(os.ReadFile(f.path))), result.Details, runErr
}

// lastUpdated matches the managed block's timestamp line, the one line a
// regenerated block differs in from run to run.
var lastUpdated = regexp.MustCompile(`(?m)^# Last updated: .*$`)

func sameButTimestamp(a, b string) bool {
	return lastUpdated.ReplaceAllString(a, "# Last updated: T") == lastUpdated.ReplaceAllString(b, "# Last updated: T")
}

// TestSSHConfigInstallRegeneratesTheBlock (the third review's install cases):
// whenever the block must change, install writes it again whole from the
// desired entries, so a hand edit inside it is not kept, a name held twice
// ends as one stanza with the desired key, and marker text inside a quoted
// value before the block is left whole. A file whose marker lines frame no
// single block is refused, byte-identical.
func TestSSHConfigInstallRegeneratesTheBlock(t *testing.T) {
	f := newInstallFixture(t)
	key := writeKey(t, t.TempDir(), "id_work")
	old := writeKey(t, t.TempDir(), "id_old")
	plain := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222}
	keyed := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, IdentityFile: key}
	withKey := func(e sshconfig.Entry, k string) sshconfig.Entry { e.IdentityFile = k; return e }
	sheds := []shedInfo{{name: "a", serverName: "one", server: keyed}, {name: "b", serverName: "two", server: keyed}}
	desired := sshconfig.GenerateManagedBlock([]sshconfig.Entry{withKey(f.stanza("shed-a", "a"), key), withKey(f.stanza("shed-b", "b"), key)})

	t.Run("a name held twice, the second copy right: one stanza with the desired key", func(t *testing.T) {
		block := sshconfig.GenerateManagedBlock([]sshconfig.Entry{withKey(f.stanza("shed-a", "a"), old), f.stanza("shed-b", "b")})
		block = strings.Replace(block, sshconfig.EndMarker, "\n"+sshconfig.GenerateEntry(withKey(f.stanza("shed-a", "a"), key))+sshconfig.EndMarker, 1)
		got, result, err := f.install(t, block, sheds)
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if !slices.Equal(result.Changed, []string{"shed-a", "shed-b"}) {
			t.Errorf("changed = %v, want both", result.Changed)
		}
		if !sameButTimestamp(got, desired) || strings.Contains(got, old) {
			t.Errorf("after =\n%s\nwant\n%s", got, desired)
		}
	})

	t.Run("a hand-added ProxyJump is not kept when the block is regenerated", func(t *testing.T) {
		block := sshconfig.GenerateManagedBlock([]sshconfig.Entry{withKey(f.stanza("shed-a", "a"), key), f.stanza("shed-b", "b")})
		block = strings.Replace(block, "    User a\n", "    User a\n    ProxyJump bastion\n", 1)
		got, result, err := f.install(t, block, sheds)
		if err != nil || !slices.Equal(result.Changed, []string{"shed-b"}) {
			t.Fatalf("install = %+v, %v; want shed-b changed", result, err)
		}
		if !sameButTimestamp(got, desired) {
			t.Errorf("after =\n%s\nwant\n%s", got, desired)
		}
	})

	t.Run("a quoted marker before the block is left whole", func(t *testing.T) {
		head := "Host local\n    RemoteCommand \"echo " + sshconfig.BeginMarker + "\"\n\n"
		got, _, err := f.install(t, head+sshconfig.GenerateManagedBlock([]sshconfig.Entry{f.stanza("shed-a", "a"), f.stanza("shed-b", "b")}), sheds)
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if !sameButTimestamp(got, head+desired) {
			t.Errorf("after =\n%s\nwant\n%s", got, head+desired)
		}
	})

	t.Run("marker lines framing no single block are refused", func(t *testing.T) {
		block := sshconfig.GenerateManagedBlock([]sshconfig.Entry{f.stanza("shed-a", "a"), f.stanza("shed-b", "b")})
		for _, content := range []string{block + "\n" + block, block + sshconfig.EndMarker + "\n"} {
			for _, sh := range [][]shedInfo{sheds, {{name: "a", serverName: "one", server: plain}}} {
				got, _, err := f.install(t, content, sh)
				var me *sshconfig.MarkerError
				if !errors.As(err, &me) || !strings.Contains(err.Error(), f.path) {
					t.Fatalf("install = %v, want the marker refusal naming the file", err)
				}
				if got != content {
					t.Errorf("the file changed after a refusal:\n%s", got)
				}
			}
		}
	})
}

// TestSSHConfigInstallWithNoIdentityFileIsAsBefore: when no desired entry has
// an identity file, install compares by name only and writes exactly what it
// wrote before identity files existed: an addition or a removal regenerates
// the block between the bytes around it, and nothing to do leaves the file
// byte-identical, a hand edit included.
func TestSSHConfigInstallWithNoIdentityFileIsAsBefore(t *testing.T) {
	f := newInstallFixture(t)
	plain := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222}
	head := "Host *\n    ServerAliveInterval 30\n\n"
	tail := "\nHost tail\n    HostName tail.example.test\n"
	a, b := f.stanza("shed-a", "a"), f.stanza("shed-b", "b")
	edited := strings.Replace(sshconfig.GenerateManagedBlock([]sshconfig.Entry{a}), "    User a\n", "    User a\n    ProxyJump bastion\n", 1)
	onlyA := []shedInfo{{name: "a", serverName: "one", server: plain}}
	// What the writer has always written around a regenerated block: the
	// bytes before it, then the block, then a blank line and the bytes after
	// it as they were (so a blank line already there is doubled, as before).
	regenerated := func(entries ...sshconfig.Entry) string {
		return head + sshconfig.GenerateManagedBlock(entries) + "\n" + tail
	}

	t.Run("additions only", func(t *testing.T) {
		got, result, err := f.install(t, head+edited+tail, []shedInfo{onlyA[0], {name: "b", serverName: "one", server: plain}})
		if err != nil || !slices.Equal(result.Added, []string{"shed-b"}) || len(result.Changed) != 0 {
			t.Fatalf("install = %+v, %v", result, err)
		}
		if want := regenerated(a, b); !sameButTimestamp(got, want) {
			t.Errorf("after =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("removals only", func(t *testing.T) {
		got, result, err := f.install(t, head+sshconfig.GenerateManagedBlock([]sshconfig.Entry{a, b})+tail, onlyA)
		if err != nil || !slices.Equal(result.Removed, []string{"shed-b"}) || len(result.Changed) != 0 {
			t.Fatalf("install = %+v, %v", result, err)
		}
		if want := regenerated(a); !sameButTimestamp(got, want) {
			t.Errorf("after =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("no change", func(t *testing.T) {
		got, result, err := f.install(t, head+edited+tail, onlyA)
		if err != nil || len(result.Added)+len(result.Removed)+len(result.Changed) != 0 {
			t.Fatalf("install = %+v, %v", result, err)
		}
		if got != head+edited+tail {
			t.Errorf("the file changed:\n%s", got)
		}
	})
}

// TestSSHConfigInstallEscapesUpdatedNames: an alias listed under "Entries to
// update" is printed with its control characters escaped. A server entry name
// edited by hand into config.yaml with an escape sequence reaches the output
// through the qualified roost alias `shed attach` wrote for it.
func TestSSHConfigInstallEscapesUpdatedNames(t *testing.T) {
	f := newInstallFixture(t)
	key := writeKey(t, t.TempDir(), "id_work")
	server := "srv\x1b[31m"
	keyed := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, IdentityFile: key}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := sshconfig.GenerateManagedBlock([]sshconfig.Entry{f.stanza("shed-box", "box"), f.stanza("shed-"+server+"-box", "box")})
	if err := os.WriteFile(f.path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sheds := []shedInfo{{name: "box", serverName: server, server: keyed}}
	entries, err := generateEntries(sheds)
	if err != nil {
		t.Fatal(err)
	}
	origJSON, origDry := jsonFlag, sshConfigDryRun
	t.Cleanup(func() { jsonFlag, sshConfigDryRun = origJSON, origDry })
	jsonFlag, sshConfigDryRun = false, true
	var runErr error
	out := captureStdout(t, func() { runErr = runSSHConfigInstall(entries, sheds) })
	if runErr != nil {
		t.Fatalf("install: %v", runErr)
	}
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("the output carries a raw ESC: %q", out)
	}
	if !strings.Contains(out, `~ shed-srv\x1b[31m-box`) {
		t.Errorf("the updated alias is not listed escaped: %q", out)
	}
}

// TestIdentityPathEscapedInReports: a key whose path holds an escape sequence
// (the rule allows one) is reported by `server update --identity-file`, and by
// the identity line `server add` and a refetch print, with the control bytes
// escaped: no raw control byte reaches the output.
func TestIdentityPathEscapedInReports(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	putServerEntry(t, "plain", config.ServerEntry{Host: "shed.example.test", HTTPPort: 8080, SSHPort: 2222})
	key := writeKey(t, t.TempDir(), "id_\x1b[31mwork\x1b]0;title\x07")

	serverUpdateIdentityFile = optionalString{value: key, set: true}
	var err error
	out := captureStdout(t, func() { err = runServerUpdate(nil, []string{"plain"}) })
	if err != nil {
		t.Fatalf("update --identity-file: %v", err)
	}
	out += captureStdout(t, func() { printIdentityFileLine(key) })
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("the output carries a raw control byte: %q", out)
	}
	if !strings.Contains(out, `id_\x1b[31mwork`) {
		t.Errorf("the output does not show the path escaped: %q", out)
	}
}
