package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// One shed record on every backend: every API route that returns a shed
// record returns it with "isolation" (and "arch" where the server knows it),
// set by the access guard's report step, with "network_profile" as stored.

// recordRoutes are the routes that return shed records, each with what
// drives it below. TestShedRecordRouteWalk fails for a registered route that
// is on neither this list nor notRecordRoutes, so a new route that returns a
// record must be added here, and so driven, before it can pass.
var recordRoutes = []string{
	"GET /api/sheds",
	"POST /api/sheds",
	"GET /api/sheds/{name}",
	"POST /api/sheds/{name}/start",
	"POST /api/sheds/{name}/stop",
	"POST /api/sheds/{name}/assign",
	"POST /api/sheds/{name}/extend",
	"POST /api/sheds/{name}/reset",
	"POST /api/sheds/{name}/provision",
	"POST /api/egress/{name}",
	"DELETE /api/egress/{name}",
	"GET /api/overview",
}

// notRecordRoutes return no shed record.
var notRecordRoutes = []string{
	"GET /api/activity",
	"GET /api/cloud/usage",
	"GET /api/delivery/requests",
	"POST /api/delivery/requests/{id}",
	"GET /api/info",
	"GET /api/ssh-host-key",
	"GET /api/sessions",
	"GET /api/images",
	"DELETE /api/images",
	"GET /api/images/inspect",
	"GET /api/images/inspect/{name}",
	"POST /api/images/prune",
	"POST /api/images/pull",
	"POST /api/images/push",
	"POST /api/images/tag",
	"DELETE /api/images/{name}",
	"GET /api/system/df",
	"POST /api/system/prune",
	"GET /api/snapshots",
	"POST /api/snapshots",
	"GET /api/snapshots/{name}",
	"DELETE /api/snapshots/{name}",
	"GET /api/egress/stream",
	"GET /api/egress/profiles",
	"GET /api/egress/profiles/{name}",
	"PUT /api/egress/profiles/{name}",
	"DELETE /api/egress/profiles/{name}",
	"GET /api/egress/{name}", // an egress status, not a record
	"POST /api/users/refresh",
	"GET /api/users/status",
	"GET /api/sources",
	"POST /api/sources/use",
	"POST /api/sources/unbind",
	"GET /api/plugins/listeners",
	"GET /api/plugins/listeners/{namespace}/messages",
	"POST /api/plugins/listeners/{namespace}/respond",
	"GET /api/plugins/sheds", // shed names for the credential bus
	"DELETE /api/sheds/{name}",
	"POST /api/sheds/{name}/exec",
	"GET /api/sheds/{name}/sessions",
	"DELETE /api/sheds/{name}/sessions/{session}",
	"GET /api/sheds/{name}/connect/{port}",
}

// recordFixture is the users fake (a Firecracker backend) with root's
// isolated shed "rbox" and an unassigned open shed "legacy" beside alice's
// "box", served by an mtls server whose named admin is root.
func recordFixture(t *testing.T) (*userFakeBackend, http.Handler) {
	t.Helper()
	be := newUserFakeBackend()
	at := be.sheds["box"].CreatedAt
	none := config.SelectedMounts(nil)
	box := be.sheds["box"]
	box.NetworkProfile, box.Mounts = config.NetworkProfileOpen, none
	be.sheds["box"] = box
	be.sheds["rbox"] = config.Shed{Name: "rbox", User: "root", Status: config.StatusRunning, CreatedAt: at, Mounts: none, NetworkProfile: config.NetworkProfileIsolated}
	be.sheds["legacy"] = config.Shed{Name: "legacy", Status: config.StatusStopped, CreatedAt: at, Mounts: none}
	_, router := usersTestServer(t, be)
	return be, router
}

// wantIsolation is what each shed of recordFixture (and a created one)
// reports on a Firecracker server.
var wantIsolation = map[string]string{
	"rbox":   backend.IsolationFirewall,
	"box":    backend.IsolationNone,
	"legacy": backend.IsolationNone,
	"fresh":  backend.IsolationNone,
	"a1":     backend.IsolationTopology,
	"ci-1":   backend.IsolationFirewall,
}

type rawRecord map[string]json.RawMessage

func (r rawRecord) str(k string) (string, bool) {
	v, ok := r[k]
	if !ok {
		return "", false
	}
	var s string
	_ = json.Unmarshal(v, &s)
	return s, true
}

// recordsOf decodes the shed records in a route's answer: the "sheds" list
// of a list or the overview, the "complete" event of a stream, or the one
// record otherwise.
func recordsOf(t *testing.T, label string, resp apiResp) []rawRecord {
	t.Helper()
	if resp.code < 200 || resp.code > 299 {
		t.Fatalf("%s: status %d: %s", label, resp.code, resp.body)
	}
	body := resp.body
	if strings.HasPrefix(resp.header.Get("Content-Type"), "text/event-stream") {
		names, data := sseEvents(t, body)
		if len(names) == 0 || names[len(names)-1] != "complete" {
			t.Fatalf("%s: events %v, want complete last: %s", label, names, body)
		}
		body = []byte(data)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("%s: %v: %s", label, err, body)
	}
	if list, ok := top["sheds"]; ok {
		var rows []rawRecord
		if err := json.Unmarshal(list, &rows); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if len(rows) == 0 {
			t.Fatalf("%s: no sheds listed", label)
		}
		return rows
	}
	return []rawRecord{rawRecord(top)}
}

// checkRecords requires every record to carry isolation as wantIsolation
// says, arch as wantArch says ("" for none), and network_profile as stored
// (stored, by name; a name it does not hold is not compared).
func checkRecords(t *testing.T, label string, recs []rawRecord, wantArch string, stored map[string]config.Shed) {
	t.Helper()
	for _, rec := range recs {
		name, _ := rec.str("name")
		iso, ok := rec.str("isolation")
		want, known := wantIsolation[name]
		if !known {
			t.Errorf("%s: unexpected shed %q", label, name)
			continue
		}
		if !ok || iso != want {
			t.Errorf("%s: %s isolation %q (present %v), want %q", label, name, iso, ok, want)
		}
		arch, hasArch := rec.str("arch")
		if wantArch == "" && hasArch {
			t.Errorf("%s: %s arch %q, want none", label, name, arch)
		}
		if wantArch != "" && arch != wantArch {
			t.Errorf("%s: %s arch %q, want %q", label, name, arch, wantArch)
		}
		if s, ok := stored[name]; ok {
			np, has := rec.str("network_profile")
			if np != s.NetworkProfile || has != (s.NetworkProfile != "") {
				t.Errorf("%s: %s network_profile %q (present %v), want it as stored (%q)", label, name, np, has, s.NetworkProfile)
			}
		}
	}
}

// TestShedRecordRouteWalk: every registered route is on exactly one of the
// two lists above.
func TestShedRecordRouteWalk(t *testing.T) {
	_, router := recordFixture(t)
	listed := map[string]int{}
	for _, k := range recordRoutes {
		listed[k]++
	}
	for _, k := range notRecordRoutes {
		listed[k]++
	}
	walked := walkedRoutes(t, router.(chi.Router))
	for _, k := range walked {
		switch listed[k] {
		case 0:
			t.Errorf("route %s is on neither recordRoutes nor notRecordRoutes: say whether it returns a shed record (and drive it in TestShedRecordRoutesCarryIsolation if so)", k)
		case 2:
			t.Errorf("route %s is on both lists", k)
		}
	}
	for k := range listed {
		if !slices.Contains(walked, k) {
			t.Errorf("listed route %s is not registered", k)
		}
	}
}

// TestShedRecordRoutesCarryIsolation drives every route on recordRoutes and
// requires every record in its answer to carry isolation and arch, with
// network_profile as stored. The list is driven as each kind of caller: a
// named admin, the implicit user of an open server and a service client
// (the system principal's list is the guard's, TestGuardListStampsEveryPrincipal).
func TestShedRecordRoutesCarryIsolation(t *testing.T) {
	be, router := recordFixture(t)
	stored := map[string]config.Shed{}
	for name, s := range be.sheds {
		stored[name] = s
	}
	asRoot := func(method, path, body string, hdr ...string) apiResp {
		return do(router, testClientCert(rootFP, "control", time.Hour), method, path, body, hdr...)
	}
	drive := map[string]func(t *testing.T){
		"GET /api/sheds": func(t *testing.T) {
			recs := recordsOf(t, "list as a named admin", asRoot(http.MethodGet, "/api/sheds", ""))
			if len(recs) != 3 {
				t.Errorf("a named admin listed %d sheds, want 3", len(recs))
			}
			checkRecords(t, "list as a named admin", recs, runtime.GOARCH, stored)
		},
		"POST /api/sheds": func(t *testing.T) {
			checkRecords(t, "create (JSON)", recordsOf(t, "create (JSON)", asRoot(http.MethodPost, "/api/sheds", `{"name":"fresh"}`)), runtime.GOARCH, stored)
			delete(be.sheds, "fresh")
			resp := asRoot(http.MethodPost, "/api/sheds", `{"name":"fresh"}`, "Accept", "text/event-stream")
			checkRecords(t, "create (stream)", recordsOf(t, "create (stream)", resp), runtime.GOARCH, stored)
		},
		"GET /api/sheds/{name}": func(t *testing.T) {
			for _, name := range []string{"rbox", "box", "legacy"} {
				checkRecords(t, "get "+name, recordsOf(t, "get "+name, asRoot(http.MethodGet, "/api/sheds/"+name, "")), runtime.GOARCH, stored)
			}
		},
		"POST /api/sheds/{name}/start": func(t *testing.T) {
			checkRecords(t, "start", recordsOf(t, "start", asRoot(http.MethodPost, "/api/sheds/rbox/start", "")), runtime.GOARCH, stored)
		},
		"POST /api/sheds/{name}/stop": func(t *testing.T) {
			checkRecords(t, "stop", recordsOf(t, "stop", asRoot(http.MethodPost, "/api/sheds/rbox/stop", "")), runtime.GOARCH, stored)
		},
		"POST /api/sheds/{name}/assign": func(t *testing.T) {
			checkRecords(t, "assign", recordsOf(t, "assign", asRoot(http.MethodPost, "/api/sheds/legacy/assign", `{"user":"bob"}`)), runtime.GOARCH, stored)
		},
		"POST /api/sheds/{name}/extend": func(t *testing.T) {
			checkRecords(t, "extend", recordsOf(t, "extend", asRoot(http.MethodPost, "/api/sheds/rbox/extend", `{"hours":6}`)), runtime.GOARCH, stored)
		},
		"POST /api/sheds/{name}/reset": func(t *testing.T) {
			checkRecords(t, "reset", recordsOf(t, "reset", asRoot(http.MethodPost, "/api/sheds/rbox/reset", "")), runtime.GOARCH, stored)
		},
		"POST /api/sheds/{name}/provision": func(t *testing.T) {
			// rbox is root's own: the admin provisions it as its user.
			checkRecords(t, "provision (JSON)", recordsOf(t, "provision (JSON)", asRoot(http.MethodPost, "/api/sheds/rbox/provision", "")), runtime.GOARCH, stored)
			resp := asRoot(http.MethodPost, "/api/sheds/rbox/provision", "", "Accept", "text/event-stream")
			checkRecords(t, "provision (stream)", recordsOf(t, "provision (stream)", resp), runtime.GOARCH, stored)
		},
		"POST /api/egress/{name}": func(t *testing.T) {
			checkRecords(t, "egress set", recordsOf(t, "egress set", asRoot(http.MethodPost, "/api/egress/rbox", `{"profiles":["off"]}`)), runtime.GOARCH, stored)
		},
		"DELETE /api/egress/{name}": func(t *testing.T) {
			checkRecords(t, "egress off", recordsOf(t, "egress off", asRoot(http.MethodDelete, "/api/egress/rbox", "")), runtime.GOARCH, stored)
		},
		"GET /api/overview": func(t *testing.T) {
			checkRecords(t, "overview", recordsOf(t, "overview", asRoot(http.MethodGet, "/api/overview", "")), runtime.GOARCH, stored)
		},
	}
	for _, route := range recordRoutes {
		fn, ok := drive[route]
		if !ok {
			t.Errorf("route %s returns shed records but is not driven here", route)
			continue
		}
		t.Run(route, fn)
	}

	t.Run("list as the implicit user of an open server", func(t *testing.T) {
		open := NewServer(be, &config.ServerConfig{Name: "test-server", DefaultBackend: config.BackendFirecracker}, "", nil, nil).Router()
		rec := httptest.NewRecorder()
		open.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sheds", nil))
		recs := recordsOf(t, "list (open)", apiResp{rec.Code, rec.Body.Bytes(), rec.Header()})
		if len(recs) != len(be.sheds) {
			t.Errorf("the implicit user listed %d sheds, want all %d", len(recs), len(be.sheds))
		}
		checkRecords(t, "list (open)", recs, runtime.GOARCH, stored)
	})

	t.Run("list and get as a service client", func(t *testing.T) {
		cb := newClientFakeBackend(config.Shed{Name: "ci-1", Owner: testClientName, Status: config.StatusRunning, NetworkProfile: config.NetworkProfileIsolated, Mounts: config.SelectedMounts(nil)})
		_, h := clientTestServer(t, cb, defaultTestPolicy())
		want := map[string]config.Shed{"ci-1": cb.sheds["ci-1"]}
		checkRecords(t, "list (client)", recordsOf(t, "list (client)", do(h, serviceLeaf(testClientName), http.MethodGet, "/api/sheds", "")), runtime.GOARCH, want)
		checkRecords(t, "get (client)", recordsOf(t, "get (client)", do(h, serviceLeaf(testClientName), http.MethodGet, "/api/sheds/ci-1", "")), runtime.GOARCH, want)
	})

	// Remote: an asynchronous create answers JSON at once, and its stream's
	// complete event is the shed WaitSettled returns. The fake's record
	// carries no arch, so none is reported.
	t.Run("remote create (JSON) and create stream (wait settled)", func(t *testing.T) {
		b := newAsyncFakeBackend()
		h := remoteTestServer(t, b, remotePolicy())
		resp := doCtx(context.Background(), h, http.MethodPost, "/api/sheds", `{"name":"a1","network":"isolated"}`)
		checkRecords(t, "remote create (JSON)", recordsOf(t, "remote create (JSON)", resp), "", nil)
		close(b.release)
		<-b.settled

		b2 := newAsyncFakeBackend()
		h2 := remoteTestServer(t, b2, remotePolicy())
		close(b2.release)
		resp = doCtx(context.Background(), h2, http.MethodPost, "/api/sheds", `{"name":"a1","network":"isolated"}`, "Accept", "text/event-stream")
		checkRecords(t, "remote create (stream)", recordsOf(t, "remote create (stream)", resp), "", nil)
		checkRecords(t, "remote list", recordsOf(t, "remote list", doCtx(context.Background(), h2, http.MethodGet, "/api/sheds", "")), "", nil)
	})
}

// TestInfoGuestArchAndUniformRecordToken: guest_arch is the host's on a
// local backend and the sizes' one architecture on remote, omitted when the
// sizes mix; the uniform_shed_record token is always advertised.
func TestInfoGuestArchAndUniformRecordToken(t *testing.T) {
	remote := func(cat *backend.ComputeCatalog) http.Handler {
		cfg := &config.ServerConfig{
			Name:           "test-server",
			DefaultBackend: config.BackendRemote,
			Remote:         &config.RemoteConfig{},
		}
		b := newAsyncFakeBackend()
		b.catalog = cat
		return NewServer(b, cfg, "", nil, nil).Router()
	}
	mixed := testCatalog("amd64")
	mixed.Sizes[1].Arch, mixed.GuestArch = "arm64", ""
	for _, tc := range []struct {
		name string
		h    http.Handler
		want string
	}{
		{"firecracker", NewServer(newUserFakeBackend(), &config.ServerConfig{Name: "test-server", DefaultBackend: config.BackendFirecracker}, "", nil, nil).Router(), runtime.GOARCH},
		{"vz", NewServer(routesFakeBackend{}, &config.ServerConfig{Name: "test-server", DefaultBackend: config.BackendVZ}, "", nil, nil).Router(), runtime.GOARCH},
		{"remote, one arch", remote(testCatalog("arm64")), "arm64"},
		{"remote, mixed sizes", remote(mixed), ""},
		{"remote, no catalog read yet", remote(nil), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, info := getInfo(t, tc.h)
			_, present := raw["guest_arch"]
			if info.GuestArch != tc.want || present != (tc.want != "") {
				t.Errorf("guest_arch %q (present %v), want %q", info.GuestArch, present, tc.want)
			}
			if !info.HasFeature(FeatureUniformShedRecord) {
				t.Errorf("features %v lack %s", info.Features, FeatureUniformShedRecord)
			}
		})
	}
}
