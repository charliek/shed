package main

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/sdk"
)

// ---------------------------------------------------------------------------
// The held user, second review (plan 037 §3.1): what establishes a binding,
// what may be kept, and that every path checks before it sends.
// ---------------------------------------------------------------------------

// implicitEntryInWindow installs "srv" as an entry whose server had no user
// list: it records no user and holds the implicit user's certificate, valid
// for a day, with a recorded expiry inside the refresh window, so building a
// client from it re-enrols at once.
func implicitEntryInWindow(t *testing.T, m *mtlsServer) config.ServerEntry {
	t.Helper()
	entry := mtlsEntry(t, m, "srv", m.credentialFor(t, "cli-key", "", farFromExpiry))
	entry.User = ""
	entry.ClientCertExpiresAt = time.Now().Add(time.Hour)
	putServerEntry(t, "srv", entry)
	return entry
}

// heldCertUser is the user the certificate a source holds names ("" for the
// implicit user or for none held).
func heldCertUser(t *testing.T, src *clienttoken.Source) string {
	t.Helper()
	cred, _ := src.Current()
	cert := cred.ClientCertificate()
	if cert == nil || len(cert.Certificate) == 0 {
		return ""
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	user, _, err := servertls.ClientCertUser(leaf)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

// pairBytes reads the server's credential files.
func pairBytes(t *testing.T, name string) (cert, key []byte) {
	t.Helper()
	certPath, keyPath := config.ClientCredentialPaths(name)
	cert, _ = os.ReadFile(certPath)
	key, _ = os.ReadFile(keyPath)
	return cert, key
}

// ---------------------------------------------------------------------------
// F1: a first named user is used only once the row records it
// ---------------------------------------------------------------------------

// TestUnrecordedFirstUserIsNeverUsed is the review's sequence. An entry that
// records no user (its server had no user list) re-enrols and is issued pat's
// certificate; config.yaml cannot be saved, and the previous files are put
// back. The refresh fails, saying the entry could not be updated; pat's
// certificate is not adopted. A second process then enrols admin as the
// entry's first user, as it may: the row records no one. The first process's
// client still sends nothing, as pat or as anyone.
func TestUnrecordedFirstUserIsNeverUsed(t *testing.T) {
	cfgPath := testClientConfig(t)
	m := newMTLSServer(t)
	implicitEntryInWindow(t, m)
	certBefore, keyBefore := pairBytes(t, "srv")
	working := clientConfig
	breakConfigSave(t)
	patCred := m.credential(t, "cli-key", farFromExpiry)
	stubBootstrap(func() sdk.Credential { return patCred })

	e := clientConfig.Servers["srv"]
	first := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
	_, err := first.GetInfo()
	r := asRefusal(t, err, config.RefusedUserNotRecorded)
	if r.CertUser != testUser {
		t.Errorf("refusal names %q, want pat", r.CertUser)
	}
	for _, want := range []string{`for user "pat"`, "could not be updated to record that user", "the certificate was not used",
		"Make config.yaml and the entry's credential files writable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, err)
		}
	}
	if got := heldCertUser(t, first.tokens); got == testUser {
		t.Fatal("the first process adopted pat's certificate the row does not record")
	}
	if got := m.accepted.Load(); got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0", got)
	}
	if cert, key := pairBytes(t, "srv"); string(cert) != string(certBefore) || string(key) != string(keyBefore) {
		t.Error("the previous pair was not put back")
	}
	if got := onDisk(t, cfgPath).Servers["srv"].User; got != "" {
		t.Fatalf("test setup: the row records %q", got)
	}

	// A second process enrols admin as the entry's first named user.
	clientConfig = working
	nextProcess(t, cfgPath)
	adminCred := m.credentialFor(t, "admin-key", "admin", farFromExpiry)
	stubBootstrap(func() sdk.Credential { return adminCred })
	second := clientConfig.Servers["srv"]
	if _, err := NewAPIClientFromNamedEntry("srv", &second, DefaultTimeout).GetInfo(); err != nil {
		t.Fatalf("the second process's first enrolment: %v", err)
	}
	if got := onDisk(t, cfgPath).Servers["srv"].User; got != "admin" {
		t.Fatalf("user on disk = %q, want the second process's admin", got)
	}

	// The first process's client: nothing is sent, nothing is dialled.
	accepted := m.accepted.Load()
	if _, err := first.GetInfo(); err == nil {
		t.Error("the first process sent a request")
	}
	if first.Ping() {
		t.Error("the first process's Ping reached the server")
	}
	if first.tokens.CertificateFor(context.Background()) != nil {
		t.Error("the first process still presents a certificate")
	}
	if got := m.accepted.Load() - accepted; got != 0 {
		t.Errorf("the server accepted %d connection(s) from the first process, want 0", got)
	}
}

// TestUnrecordedFirstUserOnEveryWriteFailure: the read of the previous pair
// before the write, and the file write itself, are the other two steps whose
// failure would leave a first named user unrecorded. Each fails the refresh,
// with nothing written, nothing adopted and nothing sent. (A renewal for the
// user the row already records stays best effort:
// TestUnreadableSnapshotWritesNothing.)
func TestUnrecordedFirstUserOnEveryWriteFailure(t *testing.T) {
	cases := []struct {
		name   string
		inject func(t *testing.T)
		cause  string
	}{
		{"the read of the previous pair fails", failSnapshotReads, "permission denied"},
		{"the file write fails", func(t *testing.T) {
			orig := writeCredentialsHeldFn
			t.Cleanup(func() { writeCredentialsHeldFn = orig })
			var calls atomic.Int32
			writeCredentialsHeldFn = func(n string, certPEM, keyPEM []byte) (string, string, error) {
				if calls.Add(1) == 1 {
					// Fails before either destination changes: nothing to put back.
					return "", "", errors.New("injected: no space left on device")
				}
				return orig(n, certPEM, keyPEM)
			}
		}, "no space left on device"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := testClientConfig(t)
			m := newMTLSServer(t)
			implicitEntryInWindow(t, m)
			certBefore, keyBefore := pairBytes(t, "srv")
			tc.inject(t)
			stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })

			e := clientConfig.Servers["srv"]
			c := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
			_, err := c.GetInfo()
			asRefusal(t, err, config.RefusedUserNotRecorded)
			if !strings.Contains(err.Error(), tc.cause) {
				t.Errorf("message lacks the cause %q:\n%s", tc.cause, err)
			}
			if got := heldCertUser(t, c.tokens); got == testUser {
				t.Error("pat's certificate was adopted")
			}
			if got := m.accepted.Load(); got != 0 {
				t.Errorf("the server accepted %d connection(s), want 0", got)
			}
			if cert, key := pairBytes(t, "srv"); string(cert) != string(certBefore) || string(key) != string(keyBefore) {
				t.Error("the pair on disk changed")
			}
			if got := onDisk(t, cfgPath).Servers["srv"].User; got != "" {
				t.Errorf("user on disk = %q, want none", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// F2: a refusal keeps the held certificate only after a positive check
// ---------------------------------------------------------------------------

// TestRefusalKeepsOnlyWhenTheRowStillRecordsTheHeldUser is the review's A/B
// interleaving for the refusals that would otherwise keep the held
// certificate. A holds pat's certificate, near expiry but still valid, and
// re-enrols. While its bootstrap runs, B changes the entry (to admin, with
// admin's pair), or the row becomes unreadable. A is issued a certificate that
// names no user, or one with a malformed user attribute, or pat's again. In
// every case A first checks the row under the lock: a row that names admin ends
// A's client with the user-changed refusal, and a row it cannot read ends it
// with the unreadable refusal — so pat's certificate is never sent again, and
// B's files are untouched.
func TestRefusalKeepsOnlyWhenTheRowStillRecordsTheHeldUser(t *testing.T) {
	issuances := []struct {
		name  string
		issue func(t *testing.T, m *mtlsServer) sdk.Credential
	}{
		{"an issuance that names no user", func(t *testing.T, m *mtlsServer) sdk.Credential {
			return selfSignedCredential(t, m.pin)
		}},
		{"a malformed issuance", func(t *testing.T, m *mtlsServer) sdk.Credential {
			return selfSignedCredential(t, m.pin, userAttr("Pat"))
		}},
		{"pat's certificate again", func(t *testing.T, m *mtlsServer) sdk.Credential {
			return m.credential(t, "cli-key", farFromExpiry)
		}},
	}
	changes := []struct {
		name string
		// change runs as B, inside A's bootstrap.
		change func(t *testing.T, m *mtlsServer, cfgPath string)
		kind   config.RefusalKind
		says   string
	}{
		{"another process changed the entry to admin", func(t *testing.T, m *mtlsServer, cfgPath string) {
			admin := m.credentialFor(t, "admin-key", "admin", farFromExpiry)
			if _, _, err := config.WriteClientCredentials("srv", []byte(admin.Bundle.ClientCert), admin.KeyPEM); err != nil {
				t.Fatal(err)
			}
			asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
				e := c.Servers["srv"]
				e.User = "admin"
				c.Servers["srv"] = e
			})
		}, config.RefusedEntryUserChanged, `now records user "admin"`},
		{"the row cannot be read", func(t *testing.T, _ *mtlsServer, cfgPath string) {
			if err := os.WriteFile(cfgPath, []byte("servers: [this is not a map\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, config.RefusedEntryUnreadable, "could not be read to check the user it is held to"},
	}
	for _, ch := range changes {
		for _, is := range issuances {
			t.Run(ch.name+"/"+is.name, func(t *testing.T) {
				cfgPath := testClientConfig(t)
				m := newMTLSServer(t)
				stored := m.credential(t, "cli-key", farFromExpiry)
				entry := mtlsEntry(t, m, "srv", stored)
				entry.ClientCertExpiresAt = time.Now().Add(time.Hour) // inside the window: A re-enrols at build
				putServerEntry(t, "srv", entry)
				certPath, keyPath := config.ClientCredentialPaths("srv")

				var afterB fileSnapshot
				mints := stubBootstrap(func() sdk.Credential {
					ch.change(t, m, cfgPath)
					afterB = snapshotFiles(t, cfgPath, certPath, keyPath)
					freezeCreds(t, "srv")
					return is.issue(t, m)
				})

				e := clientConfig.Servers["srv"]
				c := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
				_, err := c.GetInfo()
				asRefusal(t, err, ch.kind)
				if !strings.Contains(err.Error(), ch.says) {
					t.Errorf("message lacks %q:\n%s", ch.says, err)
				}
				if c.tokens.Err() == nil {
					t.Fatal("the client still holds pat's certificate as usable")
				}
				c.tokens.EnsureFresh()
				if _, err := c.GetInfo(); err == nil {
					t.Fatal("a second request was sent")
				}
				if c.Ping() {
					t.Error("Ping reached the server")
				}
				if got := m.accepted.Load(); got != 0 {
					t.Errorf("the server accepted %d connection(s), want 0 (pat's certificate was sent again)", got)
				}
				if got := mints.Load(); got != 1 {
					t.Errorf("mints = %d, want 1", got)
				}
				afterB.assertUnchanged(t)
			})
		}
	}
}

// ---------------------------------------------------------------------------
// F3: a client is built from the row as it is on disk
// ---------------------------------------------------------------------------

// TestClientAfterFirstEnrolmentInTheLookupWorks: the lookup builds a client
// that enrols the entry's first user (pat) and records it, then hands back the
// copy of the entry it started from, which records no user. The command's next
// client — console, exec, attach, sync and tunnels all build one from that
// copy — must be held to the row as it is now, not refuse the certificate the
// first one just recorded.
func TestClientAfterFirstEnrolmentInTheLookupWorks(t *testing.T) {
	cfgPath := testClientConfig(t)
	m := newMTLSServer(t)
	implicitEntryInWindow(t, m)
	stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })
	withServerFlag(t, "srv")

	serverName, entry, err := findShedServer("box")
	if err != nil {
		t.Fatalf("findShedServer: %v", err)
	}
	if got := onDisk(t, cfgPath).Servers["srv"].User; got != testUser {
		t.Fatalf("test setup: the lookup's client did not record pat (user %q)", got)
	}
	if entry.User != "" {
		t.Fatal("test setup: the lookup's copy of the entry is supposed to be stale")
	}

	c := NewAPIClientFromNamedEntry(serverName, entry, DefaultTimeout)
	if _, err := c.GetInfo(); err != nil {
		t.Fatalf("the command's next client failed after a successful enrolment: %v", err)
	}
	target, err := connectTargetFromEntry(entry, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := tunnelCredentialSource(c, serverName, entry, target).Err(); err != nil {
		t.Errorf("the tunnel's source is refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// F5: a kept certificate's expiry is noticed by the pre-send check
// ---------------------------------------------------------------------------

// TestKeptCertificateExpiresAtThePreSendCheck: a proactive refresh is refused
// (admin's certificate came back) while pat's certificate is still valid, so
// the client keeps it. Once its NotAfter has passed, no path sends or dials
// with it — a JSON request, an event stream, Ping and Status — even though
// none of them runs another proactive refresh: each one's pre-send check sees
// the expiry and ends the client with the remembered refusal, unwrapped.
func TestKeptCertificateExpiresAtThePreSendCheck(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	stored := m.credential(t, "cli-key", farFromExpiry)
	entry := mtlsEntry(t, m, "srv", stored)
	entry.ClientCertExpiresAt = time.Now().Add(time.Hour) // inside the window: re-enrols at build
	putServerEntry(t, "srv", entry)
	mints := stubBootstrap(func() sdk.Credential { return m.credentialFor(t, "admin-key", "admin", farFromExpiry) })

	e := clientConfig.Servers["srv"]
	c := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
	if c.tokens.Err() != nil {
		t.Fatal("test setup: the still-valid certificate was not kept")
	}

	// The kept certificate's own NotAfter has passed (the server, on the real
	// clock, would still take it: a send would show as an accepted connection).
	t.Cleanup(clienttoken.SetClockForTest(func() time.Time { return time.Now().Add(48 * time.Hour) }))

	_, err := c.GetInfo()
	asRefusal(t, err, config.RefusedIssuedUserChange)
	_, err = c.CreateShedWithProgress(&config.CreateShedRequest{Name: "x"}, false, func(backend.ProgressEvent) {})
	asRefusal(t, err, config.RefusedIssuedUserChange)
	if c.Ping() {
		t.Error("Ping reported the server online with an expired kept certificate")
	}
	status, err := c.Status()
	asRefusal(t, err, config.RefusedIssuedUserChange)
	if status != serverStatusRefused {
		t.Errorf("Status = %q, want refused", status)
	}
	if got := m.accepted.Load(); got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0", got)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// F6: the refusal reaches the person from ssh-config --all and attach --new
// ---------------------------------------------------------------------------

// shedListServer answers GET /api/sheds with one shed, box.
func shedListServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sheds" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sheds":[{"name":"box","status":"running"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSSHConfigAllReportsRefusals: `shed ssh-config --all` prints each refused
// server's refusal once on stderr, whatever the verbosity. When every
// configured server is refused it fails (it does not print "No sheds found"
// and exit 0); when another server answers it succeeds with that server's
// sheds.
func TestSSHConfigAllReportsRefusals(t *testing.T) {
	setup := func(t *testing.T) *mtlsServer {
		t.Helper()
		testClientConfig(t)
		orig := []bool{sshConfigAll, sshConfigInstall, sshConfigDryRun, sshConfigUninstall, jsonFlag}
		t.Cleanup(func() {
			sshConfigAll, sshConfigInstall, sshConfigDryRun, sshConfigUninstall, jsonFlag = orig[0], orig[1], orig[2], orig[3], orig[4]
		})
		sshConfigAll, sshConfigInstall, sshConfigDryRun, sshConfigUninstall, jsonFlag = true, false, false, false, false
		m := newMTLSServer(t)
		refusedServerEntry(t, m, "a")
		refusedServerEntry(t, m, "b")
		return m
	}
	run := func(t *testing.T) (stdout, stderr string, err error) {
		t.Helper()
		stderr = captureStderr(t, func() {
			stdout = captureStdout(t, func() { err = runSSHConfig(nil, nil) })
		})
		return stdout, stderr, err
	}

	t.Run("every server refused", func(t *testing.T) {
		m := setup(t)
		stdout, stderr, err := run(t)
		if err == nil {
			t.Fatalf("ssh-config --all succeeded with every server refused; stdout:\n%s", stdout)
		}
		if strings.Contains(stdout, "No sheds found") {
			t.Errorf("the refusals were reported as no sheds:\n%s", stdout)
		}
		for _, name := range []string{"a", "b"} {
			msg := `entry "` + name + `" cannot be used`
			if n := strings.Count(stderr+err.Error(), msg); n != 1 {
				t.Errorf("%q appears %d time(s), want once:\nstderr: %s\nerr: %v", msg, n, stderr, err)
			}
		}
		if got := m.accepted.Load(); got != 0 {
			t.Errorf("the server accepted %d connection(s), want 0", got)
		}
	})

	t.Run("another server answers", func(t *testing.T) {
		setup(t)
		delete(clientConfig.Servers, "b")
		addFakeServer("ok", shedListServer(t))
		stdout, stderr, err := run(t)
		if err != nil {
			t.Fatalf("ssh-config --all failed although a server answered: %v", err)
		}
		if n := strings.Count(stderr, `entry "a" cannot be used`); n != 1 {
			t.Errorf("the refusal appears %d time(s) on stderr, want once:\n%s", n, stderr)
		}
		if !strings.Contains(stdout, "box") {
			t.Errorf("the answering server's shed is missing:\n%s", stdout)
		}
	})
}

// TestAttachNewReturnsTheRefusal: `shed attach --new` lists the shed's
// sessions with a client of its own before it runs ssh. When that client is
// refused, the command fails with the refusal itself — not wrapped as "failed
// to check existing sessions" — and ssh is never run.
func TestAttachNewReturnsTheRefusal(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	refusedServerEntry(t, m, "srv")
	origSession, origNew, origExec := attachSessionFlag, attachNewFlag, execSSH
	t.Cleanup(func() { attachSessionFlag, attachNewFlag, execSSH = origSession, origNew, origExec })
	attachSessionFlag, attachNewFlag = config.DefaultSessionName, true
	execSSH = func(string, []string, []string) error {
		t.Error("ssh was run for a refused entry")
		return syscall.EINVAL
	}

	e := clientConfig.Servers["srv"]
	err := attachPlain("box", "srv", &e, &config.Shed{Name: "box"})
	asRefusal(t, err, config.RefusedStoredUserMismatch)
	if got := m.accepted.Load(); got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0", got)
	}
}
