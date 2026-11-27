package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// TestListingCacheApply covers the pure mutation `shed list` and
// `shed ssh-config --all` apply to the shed cache: a server that answered has
// its rows for sheds it no longer lists dropped; a server that did not answer,
// and every row attributed to another server, is left alone.
func TestListingCacheApply(t *testing.T) {
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	cached := func(server, status string) config.ShedCache {
		return config.ShedCache{Server: server, Status: status, UpdatedAt: old}
	}
	fresh := func(server, status string) config.ShedCache {
		return config.ShedCache{Server: server, Status: status, UpdatedAt: now}
	}
	shed := func(name, status string) config.Shed { return config.Shed{Name: name, Status: status} }

	type answer struct {
		server string
		sheds  []config.Shed
	}
	tests := []struct {
		name    string
		cache   map[string]config.ShedCache
		answers []answer
		want    map[string]config.ShedCache
	}{
		{
			name:    "stale row of an answering server dropped",
			cache:   map[string]config.ShedCache{"gone": cached("a", "running"), "here": cached("a", "stopped")},
			answers: []answer{{"a", []config.Shed{shed("here", "running")}}},
			want:    map[string]config.ShedCache{"here": fresh("a", "running")},
		},
		{
			name:    "server answering with no sheds drops all its rows",
			cache:   map[string]config.ShedCache{"one": cached("a", "running"), "two": cached("a", "stopped")},
			answers: []answer{{"a", nil}},
			want:    map[string]config.ShedCache{},
		},
		{
			name:    "row of a server that did not answer is kept",
			cache:   map[string]config.ShedCache{"down": cached("b", "running"), "gone": cached("a", "running")},
			answers: []answer{{"a", nil}},
			want:    map[string]config.ShedCache{"down": cached("b", "running")},
		},
		{
			name:  "another answering server's row with the same name is kept",
			cache: map[string]config.ShedCache{"shared": cached("b", "running")},
			answers: []answer{
				{"a", nil},
				{"b", []config.Shed{shed("shared", "stopped")}},
			},
			want: map[string]config.ShedCache{"shared": fresh("b", "stopped")},
		},
		{
			name:    "a name moved to the answering server is re-attributed, not dropped",
			cache:   map[string]config.ShedCache{"moved": cached("b", "running")},
			answers: []answer{{"a", []config.Shed{shed("moved", "running")}}},
			want:    map[string]config.ShedCache{"moved": fresh("a", "running")},
		},
		{
			name:  "a name its old server no longer lists but another does stays cached",
			cache: map[string]config.ShedCache{"moved": cached("b", "running")},
			answers: []answer{
				{"a", []config.Shed{shed("moved", "running")}},
				{"b", nil},
			},
			want: map[string]config.ShedCache{"moved": fresh("a", "running")},
		},
		{
			name:    "no server answered changes nothing",
			cache:   map[string]config.ShedCache{"x": cached("a", "running"), "y": cached("b", "stopped")},
			answers: nil,
			want:    map[string]config.ShedCache{"x": cached("a", "running"), "y": cached("b", "stopped")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var l listingCache
			for _, a := range tt.answers {
				l.answer(a.server, a.sheds)
			}
			// Apply twice, to two copies, the way Update does: both must agree.
			for _, round := range []string{"disk", "memory"} {
				c := &config.ClientConfig{Servers: map[string]config.ServerEntry{}, Sheds: map[string]config.ShedCache{}}
				for k, v := range tt.cache {
					c.Sheds[k] = v
				}
				l.apply(c, now)
				assertShedCache(t, round, c.Sheds, tt.want)
			}
		})
	}
}

// TestRunListPrunesShedCache drives runList against fake servers and checks the
// cache it leaves on disk and in memory.
func TestRunListPrunesShedCache(t *testing.T) {
	// listServer answers GET /api/sheds with the given names, or 500 when
	// fail is set.
	listServer := func(t *testing.T, fail bool, names ...string) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/sheds" || fail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			resp := config.ShedsResponse{Sheds: []config.Shed{}}
			for _, n := range names {
				resp.Sheds = append(resp.Sheds, config.Shed{Name: n, Status: "running"})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	seed := map[string]config.ShedCache{
		"a-live":  {Server: "a", Status: "stopped"},
		"a-gone":  {Server: "a", Status: "running"},
		"b-kept":  {Server: "b", Status: "running"},
		"c-other": {Server: "c", Status: "running"},
	}

	tests := []struct {
		name  string
		all   bool
		wantS map[string]string // shed name -> server
	}{
		{
			// a answers, b fails, c answers with nothing.
			name:  "all servers",
			all:   true,
			wantS: map[string]string{"a-live": "a", "b-kept": "b"},
		},
		{
			// Only the default server a is asked: b's and c's rows are not
			// its to prune.
			name:  "default server only",
			all:   false,
			wantS: map[string]string{"a-live": "a", "b-kept": "b", "c-other": "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath := testClientConfig(t)
			srvA := listServer(t, false, "a-live")
			srvB := listServer(t, true)
			srvC := listServer(t, false)
			if err := clientConfig.Update(func(c *config.ClientConfig) {
				c.Servers["a"] = config.ServerEntry{APIURL: srvA.URL}
				c.Servers["b"] = config.ServerEntry{APIURL: srvB.URL}
				c.Servers["c"] = config.ServerEntry{APIURL: srvC.URL}
				c.DefaultServer = "a"
				for k, v := range seed {
					c.Sheds[k] = v
				}
			}); err != nil {
				t.Fatal(err)
			}

			origAll, origJSON, origServer, origStdout := listAll, jsonFlag, serverFlag, os.Stdout
			t.Cleanup(func() { listAll, jsonFlag, serverFlag, os.Stdout = origAll, origJSON, origServer, origStdout })
			devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = devNull.Close() })
			listAll, jsonFlag, serverFlag, os.Stdout = tt.all, true, "", devNull

			if err := runList(nil, nil); err != nil {
				t.Fatalf("runList: %v", err)
			}

			loaded, err := config.LoadClientConfigFromPath(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, view := range []struct {
				label string
				sheds map[string]config.ShedCache
			}{{"disk", loaded.Sheds}, {"memory", clientConfig.Sheds}} {
				if len(view.sheds) != len(tt.wantS) {
					t.Fatalf("%s cache = %v, want servers %v", view.label, view.sheds, tt.wantS)
				}
				for name, server := range tt.wantS {
					got, ok := view.sheds[name]
					if !ok || got.Server != server {
						t.Errorf("%s cache[%q] = %+v (present %v), want server %q", view.label, name, got, ok, server)
					}
				}
				// A row of a server that was not asked or did not answer keeps
				// its old status and stamp.
				if got, want := view.sheds["b-kept"], seed["b-kept"]; got.Server != want.Server || got.Status != want.Status || !got.UpdatedAt.Equal(want.UpdatedAt) {
					t.Errorf("%s cache[b-kept] = %+v, want untouched %+v", view.label, got, seed["b-kept"])
				}
			}
		})
	}
}

func assertShedCache(t *testing.T, label string, got, want map[string]config.ShedCache) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: cache = %v, want %v", label, got, want)
	}
	for name, w := range want {
		if g, ok := got[name]; !ok || g != w {
			t.Errorf("%s: cache[%q] = %+v (present %v), want %+v", label, name, g, ok, w)
		}
	}
}
