package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
)

// refreshingSource builds a Source seeded with tok that mints newTok on refresh
// (recording whether it fired), with far-future expiries so only the reactive
// path is exercised.
func refreshingSource(tok, newTok string, fired *bool) *clienttoken.Source {
	return clienttoken.New(clienttoken.TokenCredential(tok, time.Now().Add(24*time.Hour)), func() (clienttoken.Credential, error) {
		if fired != nil {
			*fired = true
		}
		return clienttoken.TokenCredential(newTok, time.Now().Add(24*time.Hour)), nil
	})
}

func TestDoRequest401RefreshAndRetry(t *testing.T) {
	var hits int
	refreshed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") == "Bearer new" {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newAPIClientWithSource(srv.URL, "", refreshingSource("stale", "new", &refreshed), DefaultTimeout)

	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.doRequest("GET", "/x", nil, &out); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if !refreshed {
		t.Error("refresh should have been called on the 401")
	}
	if !out.OK {
		t.Error("the retry should have succeeded with the refreshed token")
	}
	if hits != 2 {
		t.Errorf("want 2 requests (401 then 200), got %d", hits)
	}
}

func TestDoRequest401RetryAtMostOnce(t *testing.T) {
	var hits, refreshes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	src := clienttoken.New(clienttoken.TokenCredential("stale", time.Now().Add(24*time.Hour)), func() (clienttoken.Credential, error) {
		refreshes++
		return clienttoken.TokenCredential("new", time.Now().Add(24*time.Hour)), nil
	})
	c := newAPIClientWithSource(srv.URL, "", src, DefaultTimeout)

	if err := c.doRequest("GET", "/x", nil, nil); err == nil {
		t.Error("expected an error when the 401 persists after refresh")
	}
	if refreshes != 1 {
		t.Errorf("refresh must run at most once, ran %d", refreshes)
	}
	if hits != 2 {
		t.Errorf("want 2 requests (initial + one retry), got %d", hits)
	}
}

// TestDoRequest401StaticNoRetry: a static token (not Refreshable) must NOT retry
// on a 401 — behavior preserved from the pre-Source client.
func TestDoRequest401StaticNoRetry(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newAPIClient(srv.URL, "static", "", DefaultTimeout) // static Source, no refresh
	if err := c.doRequest("GET", "/x", nil, nil); err == nil {
		t.Error("expected an error on 401")
	}
	if hits != 1 {
		t.Errorf("a static token must not retry; want 1 request, got %d", hits)
	}
}

// TestDeleteShedWithProgress401RefreshAndRetry exercises the streaming delete
// path's own on-401 refresh (it bypasses doRequest): 401 → re-mint → 204 (done).
func TestDeleteShedWithProgress401RefreshAndRetry(t *testing.T) {
	var hits int
	refreshed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") == "Bearer new" {
			w.WriteHeader(http.StatusNoContent) // delete OK, no stream
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newAPIClientWithSource(srv.URL, "", refreshingSource("stale", "new", &refreshed), DefaultTimeout)

	if err := c.DeleteShedWithProgress("myshed", nil); err != nil {
		t.Fatalf("DeleteShedWithProgress: %v", err)
	}
	if !refreshed {
		t.Error("refresh should have been called on the 401")
	}
	if hits != 2 {
		t.Errorf("want 2 requests (401 then 204), got %d", hits)
	}
}

func TestServerNameForEntry(t *testing.T) {
	orig := clientConfig
	defer func() { clientConfig = orig }()
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{
		"alpha": {Host: "a", SSHPort: 2222, APIURL: "https://a:8443", ControlToken: "tok-a"},
		"beta":  {Host: "b", SSHPort: 2222, APIURL: "https://b:8443", ControlToken: "tok-b"},
	}}
	a := clientConfig.Servers["alpha"]
	if got := serverNameForEntry(&a); got != "alpha" {
		t.Errorf("serverNameForEntry = %q, want alpha", got)
	}
	oneOff := config.ServerEntry{Host: "z", SSHPort: 1, ControlToken: "tok-z"}
	if got := serverNameForEntry(&oneOff); got != "" {
		t.Errorf("a one-off entry should map to \"\", got %q", got)
	}
}

func TestProactiveRefreshOnNearExpiry(t *testing.T) {
	origCfg, origBF := clientConfig, bootstrapCredentialFn
	defer func() { clientConfig, bootstrapCredentialFn = origCfg, origBF }()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.LoadClientConfigFromPath(cfgPath) // empty, but with its path set so Save works
	if err != nil {
		t.Fatal(err)
	}
	clientConfig = cfg
	nearExpiry := time.Now().Add(30 * time.Minute) // inside the 2h window
	clientConfig.Servers["s"] = config.ServerEntry{
		Host: "h", SSHPort: 2222, APIURL: "https://h:8443",
		ControlToken: "old", ControlTokenExpiresAt: nearExpiry,
	}

	newExpiry := time.Now().Add(24 * time.Hour)
	bootstrapCredentialFn = func(string, int, string, string) (sdk.Credential, error) {
		return sdk.Credential{Bundle: sdk.Bundle{Token: "fresh", ExpiresAt: newExpiry, Scope: "control"}}, nil
	}

	entry := clientConfig.Servers["s"]
	c := NewAPIClientFromEntry(&entry, DefaultTimeout)
	if c.tokens.Token() != "fresh" {
		t.Errorf("client token = %q, want fresh (proactively refreshed)", c.tokens.Token())
	}
	if got := clientConfig.Servers["s"].ControlToken; got != "fresh" {
		t.Errorf("in-memory token = %q, want fresh", got)
	}
	// And it persisted to disk.
	reloaded, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Servers["s"].ControlToken != "fresh" {
		t.Error("the refreshed token was not persisted to disk")
	}
}

func TestNoRefreshForStaticToken(t *testing.T) {
	origCfg, origBF := clientConfig, bootstrapCredentialFn
	defer func() { clientConfig, bootstrapCredentialFn = origCfg, origBF }()
	called := false
	bootstrapCredentialFn = func(string, int, string, string) (sdk.Credential, error) {
		called = true
		return sdk.Credential{Bundle: sdk.Bundle{Token: "x"}}, nil
	}
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{}}

	// A static/legacy token has a zero ControlTokenExpiresAt → no refresh wiring.
	entry := config.ServerEntry{Host: "h", ControlToken: "static"}
	c := NewAPIClientFromEntry(&entry, DefaultTimeout)
	if called {
		t.Error("a static token (zero expiry) must not bootstrap")
	}
	if c.tokens.Refreshable() {
		t.Error("a static token must not be refreshable")
	}
	if c.tokens.Token() != "static" {
		t.Errorf("token = %q, want static", c.tokens.Token())
	}
}

func TestServerNameForEntryAmbiguous(t *testing.T) {
	orig := clientConfig
	defer func() { clientConfig = orig }()
	// Two names aliasing the same endpoint: the lookup must refuse to guess
	// rather than return an arbitrary one, so a refresh never persists to the
	// wrong alias.
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{
		"primary": {Host: "h", SSHPort: 2222, APIURL: "https://h:8443", ControlToken: "tok1"},
		"alias":   {Host: "h", SSHPort: 2222, APIURL: "https://h:8443", ControlToken: "tok2"},
	}}
	e := config.ServerEntry{Host: "h", SSHPort: 2222, APIURL: "https://h:8443"}
	if got := serverNameForEntry(&e); got != "" {
		t.Errorf("ambiguous match must return \"\" (refuse to persist), got %q", got)
	}
}

// TestConcurrentRefreshNoRace drives the REAL `shed system df --all` fan-out:
// forEachServer reads clientConfig.Servers while the goroutines it spawns
// refresh + persist (write that same map). It must run clean under
// `go test -race` — exercising both configMu (the refresh-path writes) and
// forEachServer's snapshot-before-spawn (the launcher's reads). Calling
// forEachServer directly (rather than snapshotting in the test) is deliberate:
// it covers the launcher's own map reads, which a hand-rolled snapshot misses.
func TestConcurrentRefreshNoRace(t *testing.T) {
	origCfg, origBF := clientConfig, bootstrapCredentialFn
	defer func() { clientConfig, bootstrapCredentialFn = origCfg, origBF }()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig = cfg
	nearExpiry := time.Now().Add(30 * time.Minute) // inside the 2h window → all refresh
	for _, n := range []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8"} {
		clientConfig.Servers[n] = config.ServerEntry{
			Host: n + ".example", SSHPort: 2222, APIURL: "https://" + n + ".example:8443",
			ControlToken: "old", ControlTokenExpiresAt: nearExpiry,
		}
	}
	newExpiry := time.Now().Add(24 * time.Hour)
	bootstrapCredentialFn = func(host string, _ int, _, _ string) (sdk.Credential, error) {
		return sdk.Credential{Bundle: sdk.Bundle{Token: "fresh-" + host, ExpiresAt: newExpiry, Scope: "control"}}, nil
	}

	_ = forEachServer(clientConfig.Servers, func(_ string, entry config.ServerEntry) (struct{}, error) {
		_ = NewAPIClientFromEntry(&entry, DefaultTimeout)
		return struct{}{}, nil
	})
}
