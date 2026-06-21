package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// newEgressProfileServer builds a server with egress enabled, one config profile
// ("base"), a user store in a temp dir, and the given sheds (for the fan-out).
func newEgressProfileServer(t *testing.T, sheds []config.Shed) (*Server, *egressFakeBackend, *config.UserProfileStore) {
	t.Helper()
	store, err := config.OpenUserProfileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.ServerConfig{
		Name: "test",
		Egress: &config.EgressConfig{
			Enabled:  true,
			Profiles: map[string]config.EgressProfile{"base": {Allow: []string{"*.ubuntu.com"}}},
		},
	}
	be := &egressFakeBackend{sheds: sheds}
	srv := NewServer(be, cfg, "", nil, nil)
	srv.SetEgressUserStore(store)
	return srv, be, store
}

func doReq(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	return w
}

func TestHandlePutAndGetProfile(t *testing.T) {
	srv, _, _ := newEgressProfileServer(t, nil)

	w := doReq(t, srv, http.MethodPut, "/api/egress/profiles/mine", `{"allow":["example.com"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, body %s", w.Code, w.Body.String())
	}
	var info config.EgressProfileInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Source != "user" || len(info.Profile.Allow) != 1 || info.Profile.Allow[0] != "example.com" {
		t.Fatalf("PUT resp = %+v", info)
	}

	// GET it back
	if w := doReq(t, srv, http.MethodGet, "/api/egress/profiles/mine", ""); w.Code != http.StatusOK {
		t.Fatalf("GET = %d", w.Code)
	}

	// a config profile reports source "config"
	w = doReq(t, srv, http.MethodGet, "/api/egress/profiles/base", "")
	_ = json.Unmarshal(w.Body.Bytes(), &info)
	if info.Source != "config" {
		t.Errorf("base source = %q, want config", info.Source)
	}

	// unknown → 404
	if w := doReq(t, srv, http.MethodGet, "/api/egress/profiles/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET unknown = %d, want 404", w.Code)
	}
}

func TestHandlePutProfileRejections(t *testing.T) {
	srv, _, _ := newEgressProfileServer(t, nil)
	cases := []struct {
		name, body string
		want       int
	}{
		{"base", `{"allow":["x.com"]}`, http.StatusConflict},    // config-name collision
		{"off", `{"mode":"audit"}`, http.StatusConflict},        // reserved name
		{"bad", `{"rule":"garbage ++"}`, http.StatusBadRequest}, // bad CEL
		{"Up", `{"allow":["x.com"]}`, http.StatusBadRequest},    // uppercase name
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doReq(t, srv, http.MethodPut, "/api/egress/profiles/"+c.name, c.body)
			if w.Code != c.want {
				t.Errorf("PUT %s = %d, want %d (%s)", c.name, w.Code, c.want, w.Body.String())
			}
		})
	}
}

func TestHandleListProfiles(t *testing.T) {
	srv, _, store := newEgressProfileServer(t, nil)
	if err := store.Put("mine", config.EgressProfile{Allow: []string{"a.com"}}); err != nil {
		t.Fatal(err)
	}
	w := doReq(t, srv, http.MethodGet, "/api/egress/profiles", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d", w.Code)
	}
	var infos []config.EgressProfileInfo
	if err := json.Unmarshal(w.Body.Bytes(), &infos); err != nil {
		t.Fatal(err)
	}
	// base (config) + mine (user), sorted by name
	if len(infos) != 2 || infos[0].Name != "base" || infos[0].Source != "config" || infos[1].Name != "mine" || infos[1].Source != "user" {
		t.Fatalf("list = %+v", infos)
	}
}

func TestHandlePutProfileRePushesRunningSheds(t *testing.T) {
	sheds := []config.Shed{
		{Name: "running-ref", Status: config.StatusRunning, EgressProfiles: []string{"mine"}},
		{Name: "stopped-ref", Status: config.StatusStopped, EgressProfiles: []string{"mine"}},
		{Name: "running-other", Status: config.StatusRunning, EgressProfiles: []string{"base"}},
	}
	srv, be, _ := newEgressProfileServer(t, sheds)
	if w := doReq(t, srv, http.MethodPut, "/api/egress/profiles/mine", `{"allow":["example.com"]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d", w.Code)
	}
	// only the RUNNING shed that references "mine" is re-pushed
	if len(be.rePushed) != 1 || be.rePushed[0] != "running-ref" {
		t.Fatalf("rePushed = %v, want [running-ref]", be.rePushed)
	}
}

func TestHandleDeleteProfile(t *testing.T) {
	sheds := []config.Shed{{Name: "web", Status: config.StatusRunning, EgressProfiles: []string{"mine"}}}
	srv, _, store := newEgressProfileServer(t, sheds)
	if err := store.Put("mine", config.EgressProfile{Allow: []string{"a.com"}}); err != nil {
		t.Fatal(err)
	}

	// referenced → 409 naming the shed
	w := doReq(t, srv, http.MethodDelete, "/api/egress/profiles/mine", "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "web") {
		t.Fatalf("delete referenced = %d %s", w.Code, w.Body.String())
	}

	// config profile → 409 (read-only)
	if w := doReq(t, srv, http.MethodDelete, "/api/egress/profiles/base", ""); w.Code != http.StatusConflict {
		t.Errorf("delete config = %d, want 409", w.Code)
	}

	// unreferenced user profile → 200
	if err := store.Put("free", config.EgressProfile{Allow: []string{"b.com"}}); err != nil {
		t.Fatal(err)
	}
	if w := doReq(t, srv, http.MethodDelete, "/api/egress/profiles/free", ""); w.Code != http.StatusOK {
		t.Errorf("delete free = %d, want 200", w.Code)
	}

	// missing → 404
	if w := doReq(t, srv, http.MethodDelete, "/api/egress/profiles/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("delete ghost = %d, want 404", w.Code)
	}
}

// TestEgressProfilesRouteNotShadowed guards that the literal /profiles routes win
// over /egress/{name} at the chi trie (no 405, not dispatched to handleEgressShow).
func TestEgressProfilesRouteNotShadowed(t *testing.T) {
	srv, _, _ := newEgressProfileServer(t, nil)
	// GET /profiles must reach the list handler (200 + JSON array), not show.
	w := doReq(t, srv, http.MethodGet, "/api/egress/profiles", "")
	if w.Code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(w.Body.String()), "[") {
		t.Fatalf("GET /profiles = %d, body %s (shadowed by /{name}?)", w.Code, w.Body.String())
	}
	// PUT /profiles/{name} must not 405
	if w := doReq(t, srv, http.MethodPut, "/api/egress/profiles/x", `{"allow":["a.com"]}`); w.Code == http.StatusMethodNotAllowed {
		t.Fatal("PUT /profiles/x returned 405 (route shadowed)")
	}
}

// TestHandleEgressShowMergesUserProfile: `shed egress show` must include a user
// profile's rules, not only config profiles.
func TestHandleEgressShowMergesUserProfile(t *testing.T) {
	srv, be, store := newEgressProfileServer(t, nil)
	if err := store.Put("mine", config.EgressProfile{Allow: []string{"example.com"}}); err != nil {
		t.Fatal(err)
	}
	be.shed = &config.Shed{Name: "web", EgressProfiles: []string{"mine"}, EgressPort: 20001}
	w := doReq(t, srv, http.MethodGet, "/api/egress/web", "")
	if w.Code != http.StatusOK {
		t.Fatalf("show = %d", w.Code)
	}
	var st config.EgressStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	def, ok := st.Rules["mine"]
	if !ok || len(def.Allow) != 1 || def.Allow[0] != "example.com" {
		t.Fatalf("show.Rules missing user profile: %+v", st.Rules)
	}
}
