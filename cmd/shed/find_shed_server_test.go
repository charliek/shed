package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// newFakeShedServer returns an httptest server that answers
// GET /api/sheds/<name> with 200 for every name in hasSheds, and 404
// otherwise — enough to exercise findShedServer's GetShed probe without a
// real shed-server.
func newFakeShedServer(t *testing.T, hasSheds ...string) *httptest.Server {
	t.Helper()
	want := make(map[string]bool, len(hasSheds))
	for _, n := range hasSheds {
		want[n] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/api/sheds/"):]
		if !want[name] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"` + name + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// addFakeServer registers a plain-HTTP server entry (no TLS/token wiring) in
// clientConfig under name, pointed at srv.
func addFakeServer(name string, srv *httptest.Server) {
	clientConfig.Servers[name] = config.ServerEntry{
		APIURL: srv.URL,
	}
}

// withServerFlag sets the package-global serverFlag for the duration of the
// test and restores it on cleanup — serverFlag is read directly by
// findShedServer, so tests that exercise the --server path must stub it like
// this rather than going through cobra flag parsing.
func withServerFlag(t *testing.T, value string) {
	t.Helper()
	orig := serverFlag
	t.Cleanup(func() { serverFlag = orig })
	serverFlag = value
}

// TestFindShedServer covers #298: an explicit --server flag must win
// unconditionally over the shed-name cache, and the cache must still be
// consulted (and self-heal on a stale entry) when no flag is given.
func TestFindShedServer(t *testing.T) {
	t.Run("explicit --server wins over a cache entry pointing elsewhere", func(t *testing.T) {
		testClientConfig(t)
		srvA := newFakeShedServer(t, "myshed")
		srvB := newFakeShedServer(t, "myshed")
		addFakeServer("a", srvA)
		addFakeServer("b", srvB)
		clientConfig.Sheds["myshed"] = config.ShedCache{Server: "a"}
		withServerFlag(t, "b")

		name, entry, err := findShedServer("myshed")
		if err != nil {
			t.Fatalf("findShedServer: unexpected error: %v", err)
		}
		if name != "b" {
			t.Fatalf("server = %q, want %q (the --server flag must win over the cache)", name, "b")
		}
		if entry == nil || entry.APIURL != srvB.URL {
			t.Fatalf("entry = %+v, want the entry for server b (%s)", entry, srvB.URL)
		}
	})

	t.Run("explicit --server naming a server without the shed errors, does not fall back to cache", func(t *testing.T) {
		testClientConfig(t)
		srvA := newFakeShedServer(t, "myshed") // has it
		srvB := newFakeShedServer(t)           // does NOT have it
		addFakeServer("a", srvA)
		addFakeServer("b", srvB)
		clientConfig.Sheds["myshed"] = config.ShedCache{Server: "a"}
		withServerFlag(t, "b")

		name, entry, err := findShedServer("myshed")
		if err == nil {
			t.Fatalf("findShedServer: expected an error naming b, got server=%q entry=%+v", name, entry)
		}
		if !strings.Contains(err.Error(), "b") {
			t.Fatalf("error = %q, want it to name server %q", err.Error(), "b")
		}
		if name != "" || entry != nil {
			t.Fatalf("on error, want zero name/entry, got name=%q entry=%+v", name, entry)
		}
	})

	t.Run("unqualified form still uses the cache", func(t *testing.T) {
		testClientConfig(t)
		srvA := newFakeShedServer(t, "myshed")
		addFakeServer("a", srvA)
		clientConfig.Sheds["myshed"] = config.ShedCache{Server: "a"}
		withServerFlag(t, "")

		name, entry, err := findShedServer("myshed")
		if err != nil {
			t.Fatalf("findShedServer: unexpected error: %v", err)
		}
		if name != "a" {
			t.Fatalf("server = %q, want %q", name, "a")
		}
		if entry == nil || entry.APIURL != srvA.URL {
			t.Fatalf("entry = %+v, want the entry for server a (%s)", entry, srvA.URL)
		}
	})

	t.Run("unqualified form heals a stale cache entry by searching all servers", func(t *testing.T) {
		testClientConfig(t)
		srvA := newFakeShedServer(t) // no longer has it
		srvB := newFakeShedServer(t, "myshed")
		addFakeServer("a", srvA)
		addFakeServer("b", srvB)
		clientConfig.Sheds["myshed"] = config.ShedCache{Server: "a"}
		withServerFlag(t, "")

		name, entry, err := findShedServer("myshed")
		if err != nil {
			t.Fatalf("findShedServer: unexpected error: %v", err)
		}
		if name != "b" {
			t.Fatalf("server = %q, want %q (search-all must find it after the stale cache entry is dropped)", name, "b")
		}
		if entry == nil || entry.APIURL != srvB.URL {
			t.Fatalf("entry = %+v, want the entry for server b (%s)", entry, srvB.URL)
		}
		if _, err := clientConfig.GetShedServer("myshed"); err != nil {
			t.Fatalf("expected the cache to be repopulated pointing at the server that actually has it, got: %v", err)
		}
		if got, _ := clientConfig.GetShedServer("myshed"); got != "b" {
			t.Fatalf("cache after search-all = %q, want %q", got, "b")
		}
	})
}
