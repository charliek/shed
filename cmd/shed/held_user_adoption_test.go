package main

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// ---------------------------------------------------------------------------
// The held user, third review (plan 037 §3.1): a save-time refusal is never a
// warning, an issuance the SDK cannot use is refused like any unreadable one,
// and a failed write restores only what it changed.
// ---------------------------------------------------------------------------

// renewalInWindow installs "srv" as an entry that records pat and holds pat's
// certificate, valid for a day, with a recorded expiry inside the refresh
// window, so building a client from it renews at once (a renewal: the row
// already records the user the server issues for).
func renewalInWindow(t *testing.T, m *mtlsServer) {
	t.Helper()
	entry := mtlsEntry(t, m, "srv", m.credential(t, "cli-key", farFromExpiry))
	entry.ClientCertExpiresAt = time.Now().Add(time.Hour)
	putServerEntry(t, "srv", entry)
}

// TestSaveTimeRefusalIsNeverAWarning is the review's sequence: A renews pat,
// passes the first check under the lock and writes its new pair; before A's
// save re-checks the row, B removes the entry (`shed server remove` does not
// take the credential lock; B is paused before its file cleanup). The same
// for a row repointed by hand, and for a row that cannot be read at the
// save. A puts its previous pair back and fails with the refusal, unchanged:
// no warning, no adoption, nothing sent.
func TestSaveTimeRefusalIsNeverAWarning(t *testing.T) {
	cases := []struct {
		name    string
		between func(t *testing.T, cfgPath string)
		says    string
	}{
		{"the row is removed", func(t *testing.T, cfgPath string) {
			asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) { delete(c.Servers, "srv") })
		}, `configured server "srv" no longer exists`},
		{"the row is repointed by hand", func(t *testing.T, cfgPath string) {
			asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
				e := c.Servers["srv"]
				e.APIURL = "https://127.0.0.1:1"
				c.Servers["srv"] = e
			})
		}, "no longer names the endpoint"},
		{"the row cannot be read at the save", func(t *testing.T, cfgPath string) {
			if err := os.WriteFile(cfgPath, []byte("servers: [this is not a map\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "could not be read to check the user it is held to"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := testClientConfig(t)
			m := newMTLSServer(t)
			renewalInWindow(t, m)
			certBefore, keyBefore := pairBytes(t, "srv")

			orig := writeCredentialsHeldFn
			t.Cleanup(func() { writeCredentialsHeldFn = orig })
			writeCredentialsHeldFn = func(n string, certPEM, keyPEM []byte) (string, string, error) {
				certPath, keyPath, err := orig(n, certPEM, keyPEM)
				if err == nil {
					tc.between(t, cfgPath) // B, between A's two checks
				}
				return certPath, keyPath, err
			}
			mints := stubBootstrap(func() sdk.Credential { return m.credential(t, "cli-key", farFromExpiry) })

			e := clientConfig.Servers["srv"]
			var c *APIClient
			var err error
			stderr := captureStderr(t, func() {
				c = NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
				_, err = c.GetInfo()
			})
			asRefusal(t, err, config.RefusedEntryUnreadable)
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("message lacks %q:\n%s", tc.says, err)
			}
			if strings.Contains(stderr, "warning: could not persist") {
				t.Errorf("the refusal was turned into a warning:\n%s", stderr)
			}
			if c.tokens.Err() == nil {
				t.Error("the client still holds a usable certificate")
			}
			if got := m.accepted.Load(); got != 0 {
				t.Errorf("the server accepted %d connection(s), want 0", got)
			}
			if got := mints.Load(); got != 1 {
				t.Errorf("mints = %d, want 1", got)
			}
			if cert, key := pairBytes(t, "srv"); string(cert) != string(certBefore) || string(key) != string(keyBefore) {
				t.Error("the previous pair was not put back")
			}
		})
	}
}

// fakeBootstrapSSH puts an `ssh` on PATH that answers the bootstrap with
// bundle (as a server's _bootstrap channel would) and counts its runs, and
// installs the CLI's real bootstrap, so the answer goes through the SDK's own
// validation (sdk/bootstrap.RunCredential). testClientConfig must run first:
// it puts the bootstrap seam back.
func fakeBootstrapSSH(t *testing.T, bundle sdk.Bundle) (runs func() int) {
	t.Helper()
	dir := t.TempDir()
	body, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundlePath, runsPath := filepath.Join(dir, "bundle.json"), filepath.Join(dir, "runs")
	if err := os.WriteFile(bundlePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho run >> " + shellQuoteArg(runsPath) + "\ncat " + shellQuoteArg(bundlePath) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	bootstrapCredentialFn = bootstrapServerCredential
	return func() int {
		b, _ := os.ReadFile(runsPath)
		return strings.Count(string(b), "run\n")
	}
}

// TestUnusableIssuanceThroughTheSDKIsRefused: an mtls entry holds no usable
// pair, and the server's bundle carries a client_cert the SDK's bootstrap
// rejects (missing, only whitespace, not PEM, DER that does not parse, a
// certificate for another key). The issuance is refused as unreadable through
// the same decision as any other: terminal, since nothing valid is held,
// returned unwrapped, and never followed by a request without a certificate
// or by a second ssh.
func TestUnusableIssuanceThroughTheSDKIsRefused(t *testing.T) {
	cases := []struct {
		name string
		cert func(t *testing.T, m *mtlsServer) string
	}{
		// "" is left out of the bundle (omitempty): a missing client_cert and
		// an empty one decode the same. The SDK's own test sends both.
		{"client_cert is missing or empty", func(*testing.T, *mtlsServer) string { return "" }},
		{"client_cert is only whitespace", func(*testing.T, *mtlsServer) string { return " \n\t " }},
		{"client_cert is not PEM", func(*testing.T, *mtlsServer) string { return "not a certificate" }},
		{"client_cert DER does not parse", func(*testing.T, *mtlsServer) string {
			return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{0x30, 0x03, 0x02, 0x01}}))
		}},
		{"client_cert is for another key", func(t *testing.T, m *mtlsServer) string {
			return m.credential(t, "cli-key", farFromExpiry).Bundle.ClientCert
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := testClientConfig(t)
			m := newMTLSServer(t)
			putServerEntry(t, "srv", config.ServerEntry{
				Host: "127.0.0.1", SSHPort: 2222, APIURL: m.srv.URL, TLSCertFingerprint: m.pin,
				AuthMode: config.AuthModeMTLS, User: testUser,
			})
			before := snapshotFiles(t, cfgPath)
			_, port := hostPort(t, m.srv.URL)
			runs := fakeBootstrapSSH(t, sdk.Bundle{
				AuthMode: sdk.AuthModeMTLS, HTTPSPort: port, TLSCertFingerprint: m.pin,
				ClientCert: tc.cert(t, m), Scope: "control", ExpiresAt: time.Now().Add(farFromExpiry),
			})

			e := clientConfig.Servers["srv"]
			c := NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
			_, err := c.GetInfo()
			r := asRefusal(t, err, config.RefusedIssuedUnreadable)
			var unusable *sdkbootstrap.UnusableCertificateError
			if !errors.As(r, &unusable) {
				t.Errorf("the refusal's cause is %v, want the SDK's rejection", r.Cause)
			}
			if c.tokens.Err() == nil {
				t.Fatal("the client is still usable after an unusable issuance with nothing held")
			}
			if _, err := c.GetInfo(); err == nil {
				t.Fatal("a second request was sent")
			}
			if got := runs(); got != 1 {
				t.Errorf("ssh ran %d time(s), want 1", got)
			}
			if got := m.accepted.Load(); got != 0 {
				t.Errorf("the server accepted %d connection(s), want 0 (a request without a certificate)", got)
			}
			before.assertUnchanged(t)
			if _, err := os.Stat(config.ServerCredsDir("srv")); !os.IsNotExist(err) {
				t.Errorf("credential files were written (err=%v)", err)
			}
		})
	}
}

// TestFailedWriteRestoresOnlyWhatItChanged: a renewal for the user the row
// records (pat) whose credential write fails. When it fails before either
// destination changed (no space for the temporary file), the previous pair is
// intact: nothing is rewritten, the usable credential is not withdrawn, and
// the renewal proceeds as before (the new certificate is used in memory, a
// warning says it could not be saved). When only the key had changed, only the
// key is put back.
func TestFailedWriteRestoresOnlyWhatItChanged(t *testing.T) {
	cases := []struct {
		name string
		// write is the failing credential write.
		write func(t *testing.T, name string, keyPEM []byte) error
		// wantPutBack is what the restoration may write: "key" or nothing.
		wantPutBack []string
	}{
		{"it fails before any rename", func(*testing.T, string, []byte) error {
			return errors.New("injected: create temp file: no space left on device")
		}, nil},
		{"it fails after the key was renamed in", func(t *testing.T, name string, keyPEM []byte) error {
			_, keyPath := config.ClientCredentialPaths(name)
			if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
				t.Fatal(err)
			}
			return errors.New("injected: write client certificate: no space left on device")
		}, []string{"key"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testClientConfig(t)
			m := newMTLSServer(t)
			renewalInWindow(t, m)
			certBefore, keyBefore := pairBytes(t, "srv")
			certPath, keyPath := config.ClientCredentialPaths("srv")

			origWrite, origPutBack, origRemove := writeCredentialsHeldFn, putBackCredentialFileFn, removeCredentialsHeldFn
			t.Cleanup(func() {
				writeCredentialsHeldFn, putBackCredentialFileFn, removeCredentialsHeldFn = origWrite, origPutBack, origRemove
			})
			writeCredentialsHeldFn = func(n string, _, keyPEM []byte) (string, string, error) {
				return "", "", tc.write(t, n, keyPEM)
			}
			var putBack []string
			putBackCredentialFileFn = func(path string, data []byte) error {
				switch path {
				case keyPath:
					putBack = append(putBack, "key")
				case certPath:
					putBack = append(putBack, "certificate")
				}
				if tc.wantPutBack == nil {
					// A disk with no space fails the restoration too.
					return errors.New("injected: no space left on device")
				}
				return origPutBack(path, data)
			}
			removeCredentialsHeldFn = func(string) error {
				t.Error("the credential directory was removed")
				return nil
			}
			issued := m.credential(t, "cli-key", farFromExpiry)
			stubBootstrap(func() sdk.Credential { return issued })

			e := clientConfig.Servers["srv"]
			var c *APIClient
			var err error
			stderr := captureStderr(t, func() {
				c = NewAPIClientFromNamedEntry("srv", &e, DefaultTimeout)
				_, err = c.GetInfo()
			})
			if err != nil {
				t.Fatalf("GetInfo: %v (a usable renewal was withdrawn)", err)
			}
			if strings.Join(putBack, ",") != strings.Join(tc.wantPutBack, ",") {
				t.Errorf("put back %v, want %v", putBack, tc.wantPutBack)
			}
			if got := m.lastSerial.Load().(string); got != issued.Bundle.CertSerial {
				t.Errorf("presented %s, want the renewed certificate (in memory) %s", got, issued.Bundle.CertSerial)
			}
			if !strings.Contains(stderr, "could not store client certificate") {
				t.Errorf("no warning that the certificate could not be saved:\n%s", stderr)
			}
			if cert, key := pairBytes(t, "srv"); string(cert) != string(certBefore) || string(key) != string(keyBefore) {
				t.Error("the previous pair is not on disk")
			}
		})
	}
}
