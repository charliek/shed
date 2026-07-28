package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/sdk"
)

// ---------------------------------------------------------------------------
// Capture-vs-transmit.
//
// A request records the credential generation it is about to use, so that a
// rejection can be answered with "re-mint the thing that just failed" rather
// than "re-mint whatever is current". That recording is only meaningful if the
// generation recorded is the generation actually SENT — and a request sends its
// credential through two channels that, left to themselves, read the live
// source at two different moments: the Authorization header when the request is
// assembled, and the client certificate when the TLS stack runs the handshake.
//
// A refresh landing between those reads makes a request transmit generation N+1
// while having recorded N. The rejection is then attributed to N, Refresh(N)
// sees N as already superseded and mints nothing, and the single retry re-sends
// the very credential the server just rejected.
//
// These tests force that interleaving deterministically — the refresh runs
// inside the send hook, strictly between the capture and the transmission — and
// assert what the SERVER observed.
// ---------------------------------------------------------------------------

// refreshOnce returns a send wrapper that advances the client's credential
// exactly once, on the first send, after sendWithReauth has already captured.
// That is precisely the window the pinning closes.
func refreshOnce(t *testing.T, c *APIClient) func() {
	t.Helper()
	var once sync.Once
	return func() {
		once.Do(func() {
			_, gen := c.tokens.Current()
			if _, err := c.tokens.Refresh(gen); err != nil {
				t.Errorf("forced concurrent refresh: %v", err)
			}
		})
	}
}

// TestRequestTransmitsTheGenerationItRecorded covers both channels a credential
// travels on, because the race exists independently on each.
func TestRequestTransmitsTheGenerationItRecorded(t *testing.T) {
	// The mtls half: the certificate is chosen by the TLS stack during the
	// handshake, long after the request was built. This is the channel that had
	// no per-request identity at all.
	t.Run("mtls: the handshake presents the captured certificate", func(t *testing.T) {
		testClientConfig(t)
		m := newMTLSServer(t)

		captured := m.credential(t, "cli-key", farFromExpiry)
		putServerEntry(t, "srv", mtlsEntry(t, m, "srv", captured))

		racing := m.credential(t, "cli-key", farFromExpiry)
		if racing.Bundle.CertSerial == captured.Bundle.CertSerial {
			t.Fatal("test setup: the two credentials must be distinguishable by serial")
		}
		mints := stubBootstrap(func() sdk.Credential { return racing })

		e := clientConfig.Servers["srv"]
		c := NewAPIClientFromEntry(&e, DefaultTimeout)
		race := refreshOnce(t, c)

		resp, err := c.sendWithReauth(func(cred clienttoken.Credential) (*http.Response, error) {
			race() // a concurrent refresh lands AFTER the capture, BEFORE the handshake
			return c.sendRequest(http.MethodGet, "/api/info", nil, cred)
		}, connectFailure)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()

		if got := mints.Load(); got != 1 {
			t.Fatalf("mints = %d, want 1 (only the forced race); a second mint means the retry fired", got)
		}
		if got := m.requests.Load(); got != 1 {
			t.Errorf("server saw %d requests, want 1 — the captured certificate should have been accepted first time", got)
		}
		if got := m.lastSerial.Load().(string); got != captured.Bundle.CertSerial {
			t.Errorf("the handshake presented serial %s, want the CAPTURED %s (the racing mint was %s) — "+
				"the request transmitted a generation it had not recorded",
				got, captured.Bundle.CertSerial, racing.Bundle.CertSerial)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
	})

	// The token half: same race, simpler channel. The server here accepts ONLY
	// the captured token, so a request that transmits the racing one is refused
	// — and, because the rejection is attributed to a generation that is already
	// stale, the retry cannot recover it either.
	t.Run("token: the header carries the captured token", func(t *testing.T) {
		testClientConfig(t)

		var seen []string
		var mu sync.Mutex
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.Header.Get("Authorization"))
			mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer captured" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"srv","ssh_port":2222}`)
		}))
		defer srv.Close()

		putServerEntry(t, "srv", config.ServerEntry{
			Host: "127.0.0.1", SSHPort: 2222,
			APIURL:                srv.URL,
			TLSCertFingerprint:    servertls.Fingerprint(srv.Certificate().Raw),
			AuthMode:              config.AuthModeToken,
			ControlToken:          "captured",
			ControlTokenExpiresAt: time.Now().Add(24 * time.Hour), // far from expiry: nothing proactive fires
		})
		mints := stubBootstrap(func() sdk.Credential {
			return sdk.Credential{Bundle: sdk.Bundle{
				AuthMode: sdk.AuthModeToken, HTTPSPort: 443, Scope: "control",
				Token: "racing", ExpiresAt: time.Now().Add(24 * time.Hour),
			}}
		})

		e := clientConfig.Servers["srv"]
		c := NewAPIClientFromEntry(&e, DefaultTimeout)
		race := refreshOnce(t, c)

		resp, err := c.sendWithReauth(func(cred clienttoken.Credential) (*http.Response, error) {
			race()
			return c.sendRequest(http.MethodGet, "/api/info", nil, cred)
		}, connectFailure)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200 — the request should have been sent with the credential it captured", resp.StatusCode)
		}
		if got := mints.Load(); got != 1 {
			t.Errorf("mints = %d, want 1 (only the forced race)", got)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != 1 || seen[0] != "Bearer captured" {
			t.Errorf("server saw %v, want exactly [Bearer captured]", seen)
		}
	})
}

// TestReauthRetriesWithTheCredentialRefreshReturned: when a caller's generation
// HAS been superseded, Refresh deliberately mints nothing and hands back the
// current credential. The retry must then use that returned credential — which
// is only possible because it is threaded through rather than re-read.
func TestReauthRetriesWithTheCredentialRefreshReturned(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)

	stale := m.credential(t, "cli-key", farFromExpiry)
	putServerEntry(t, "srv", mtlsEntry(t, m, "srv", stale))

	replacement := m.credential(t, "cli-key", farFromExpiry)
	mints := stubBootstrap(func() sdk.Credential { return replacement })

	e := clientConfig.Servers["srv"]
	c := NewAPIClientFromEntry(&e, DefaultTimeout)

	// The captured generation is refused per-request (the server's real mtls
	// posture: the handshake succeeds, the identity is re-checked and denied).
	m.revoke(stale.Bundle.CertSerial)

	if _, err := c.GetInfo(); err != nil {
		t.Fatalf("GetInfo should have recovered: %v", err)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want exactly 1", got)
	}
	if got := m.lastSerial.Load().(string); got != replacement.Bundle.CertSerial {
		t.Errorf("the retry presented %s, want the re-minted %s", got, replacement.Bundle.CertSerial)
	}
}

// ---------------------------------------------------------------------------
// Persisting a mode flip back to token.
// ---------------------------------------------------------------------------

// TestPersistTokenCredentialDeletesOnlyAfterASuccessfulSave.
//
// The mtls→token flip clears the entry's certificate paths and then deletes the
// files they named. Those two steps are only safe in that order: the save is
// best-effort and warns rather than fails, so if it does fail config.yaml still
// points at the credential files — and deleting them would leave the entry
// naming material that no longer exists, which nothing in the client can
// recover from on its own.
func TestPersistTokenCredentialDeletesOnlyAfterASuccessfulSave(t *testing.T) {
	// The failure branch: the files must survive, because the config still
	// refers to them.
	t.Run("a failed save keeps the credential files", func(t *testing.T) {
		cfgPath := testClientConfig(t)
		certPath, keyPath := mtlsEntryForFlip(t)

		// Make the config update fail without touching permissions (so the test
		// behaves the same as root): put a DIRECTORY where config.yaml has to
		// be. Update reads the file and then renames a temp file onto it, and
		// neither can be done to a directory.
		if err := os.Remove(cfgPath); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(cfgPath, 0700); err != nil {
			t.Fatal(err)
		}
		if err := clientConfig.Update(func(*config.ClientConfig) error { return nil }); err == nil {
			t.Fatal("test setup: the config update was expected to fail")
		}

		persistTokenCredential("srv", clientConfig.Servers["srv"], "fresh-token", time.Now().Add(24*time.Hour))

		for _, p := range []string{certPath, keyPath} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s was deleted even though the config save failed: %v", p, err)
			}
		}
	})

	// The success branch: the whole point of the deletion is that a server
	// switched back to token mode leaves no private key on disk.
	t.Run("a successful save deletes them", func(t *testing.T) {
		testClientConfig(t)
		certPath, keyPath := mtlsEntryForFlip(t)

		persistTokenCredential("srv", clientConfig.Servers["srv"], "fresh-token", time.Now().Add(24*time.Hour))

		if got := clientConfig.Servers["srv"]; got.AuthMode != config.AuthModeToken || got.ControlToken != "fresh-token" {
			t.Errorf("entry did not migrate to token mode: %+v", got)
		}
		for _, p := range []string{certPath, keyPath} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s survived the flip back to token mode (err=%v)", p, err)
			}
		}
	})
}

// mtlsEntryForFlip installs an "srv" entry in mtls state with real credential
// files on disk, and returns their paths.
func mtlsEntryForFlip(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	m := newMTLSServer(t)
	cred := m.credential(t, "cli-key", farFromExpiry)
	certPath, keyPath, err := config.WriteClientCredentials("srv",
		[]byte(cred.Bundle.ClientCert), cred.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	putServerEntry(t, "srv", config.ServerEntry{
		Host: "127.0.0.1", SSHPort: 2222,
		APIURL:              m.srv.URL,
		TLSCertFingerprint:  m.pin,
		AuthMode:            config.AuthModeMTLS,
		ClientCertFile:      certPath,
		ClientKeyFile:       keyPath,
		ClientCertExpiresAt: cred.Bundle.ExpiresAt,
	})
	return certPath, keyPath
}

// TestMismatchedStoredPairReEnrolls: a credential store left holding a
// certificate and a key that do not go together — a crash between the two
// renames of a rotation — must behave exactly like a missing one. Failing
// instead would leave the user with an entry that cannot be used and cannot be
// repaired without deleting files by hand.
func TestMismatchedStoredPairReEnrolls(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)

	stored := m.credential(t, "cli-key", farFromExpiry)
	entry := mtlsEntry(t, m, "srv", stored)
	// Replace the key with another enrollment's, leaving the certificate alone.
	other := m.credential(t, "cli-key", farFromExpiry)
	if err := os.WriteFile(entry.ClientKeyFile, other.KeyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	putServerEntry(t, "srv", entry)

	issued := m.credential(t, "cli-key", farFromExpiry)
	mints := stubBootstrap(func() sdk.Credential { return issued })

	e := clientConfig.Servers["srv"]
	c := NewAPIClientFromEntry(&e, DefaultTimeout)
	if _, err := c.GetInfo(); err != nil {
		t.Fatalf("a mismatched stored pair must re-enroll, not fail: %v", err)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1", got)
	}
	if got := m.lastSerial.Load().(string); got != issued.Bundle.CertSerial {
		t.Errorf("presented serial %s, want the freshly enrolled %s", got, issued.Bundle.CertSerial)
	}
	// The store is consistent again.
	stampedCert := filepath.Join(config.ServerCredsDir("srv"), "client.pem")
	stampedKey := filepath.Join(config.ServerCredsDir("srv"), "client.key")
	if _, err := config.LoadClientCredentials("srv", stampedCert, stampedKey); err != nil {
		t.Errorf("the re-enrolled pair does not load: %v", err)
	}
}
