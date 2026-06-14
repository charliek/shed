package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
)

func TestNeedsRefresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"zero expiry (static token / open server) never refreshes", time.Time{}, false},
		{"far-future expiry does not refresh", now.Add(24 * time.Hour), false},
		{"within the refresh window refreshes", now.Add(time.Hour), true},
		{"exactly at the window edge refreshes", now.Add(tokenRefreshWindow), true},
		{"expired refreshes", now.Add(-time.Minute), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsRefresh(tt.expiresAt, now); got != tt.want {
				t.Errorf("needsRefresh = %v, want %v", got, tt.want)
			}
		})
	}
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

	c := newAPIClient(srv.URL, "stale", "", DefaultTimeout)
	c.refreshFn = func() (string, error) { refreshed = true; return "new", nil }

	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.doRequest("GET", "/x", nil, &out); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if !refreshed {
		t.Error("refreshFn should have been called on the 401")
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

	c := newAPIClient(srv.URL, "stale", "", DefaultTimeout)
	c.refreshFn = func() (string, error) { refreshes++; return "new", nil }

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
	origCfg, origBF := clientConfig, bootstrapFn
	defer func() { clientConfig, bootstrapFn = origCfg, origBF }()

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
	bootstrapFn = func(string, int, string, string) (sdk.Bundle, error) {
		return sdk.Bundle{Token: "fresh", ExpiresAt: newExpiry, Scope: "control"}, nil
	}

	entry := clientConfig.Servers["s"]
	c := NewAPIClientFromEntry(&entry, DefaultTimeout)
	if c.token != "fresh" {
		t.Errorf("client token = %q, want fresh (proactively refreshed)", c.token)
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
	origCfg, origBF := clientConfig, bootstrapFn
	defer func() { clientConfig, bootstrapFn = origCfg, origBF }()
	called := false
	bootstrapFn = func(string, int, string, string) (sdk.Bundle, error) {
		called = true
		return sdk.Bundle{Token: "x"}, nil
	}
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{}}

	// A static/legacy token has a zero ControlTokenExpiresAt → no refresh wiring.
	entry := config.ServerEntry{Host: "h", ControlToken: "static"}
	c := NewAPIClientFromEntry(&entry, DefaultTimeout)
	if called {
		t.Error("a static token (zero expiry) must not bootstrap")
	}
	if c.refreshFn != nil {
		t.Error("a static token must not get a refreshFn")
	}
	if c.token != "static" {
		t.Errorf("token = %q, want static", c.token)
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

// TestConcurrentRefreshNoRace mirrors the `shed system df --all` fan-out
// (forEachServer spawns a goroutine per server, each constructing a client). It
// must run clean under `go test -race`: the proactive refresh writes
// clientConfig.Servers + Save()s, so without configMu serialization this races
// the map (a fatal "concurrent map writes") and the shared config save.
func TestConcurrentRefreshNoRace(t *testing.T) {
	origCfg, origBF := clientConfig, bootstrapFn
	defer func() { clientConfig, bootstrapFn = origCfg, origBF }()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	clientConfig = cfg
	nearExpiry := time.Now().Add(30 * time.Minute) // inside the 2h window → all refresh
	for _, n := range []string{"s1", "s2", "s3", "s4", "s5"} {
		clientConfig.Servers[n] = config.ServerEntry{
			Host: n + ".example", SSHPort: 2222, APIURL: "https://" + n + ".example:8443",
			ControlToken: "old", ControlTokenExpiresAt: nearExpiry,
		}
	}
	newExpiry := time.Now().Add(24 * time.Hour)
	bootstrapFn = func(host string, _ int, _, _ string) (sdk.Bundle, error) {
		return sdk.Bundle{Token: "fresh-" + host, ExpiresAt: newExpiry, Scope: "control"}, nil
	}

	// Snapshot entries first so the spawning goroutine isn't itself racing the
	// map against the refresh writes.
	var entries []config.ServerEntry
	for _, se := range clientConfig.Servers {
		entries = append(entries, se)
	}
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func(e config.ServerEntry) {
			defer wg.Done()
			_ = NewAPIClientFromEntry(&e, DefaultTimeout)
		}(e)
	}
	wg.Wait()
}
