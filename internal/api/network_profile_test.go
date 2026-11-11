package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// vzClientFake is the owned-shed fake reporting the VZ backend.
type vzClientFake struct{ *clientFakeBackend }

func (vzClientFake) Type() backend.Type { return backend.TypeVZ }

// enforcementDownFake fails creates and starts the way the Firecracker backend
// does when its network-policy rules cannot be applied.
type enforcementDownFake struct{ *clientFakeBackend }

func (enforcementDownFake) CreateShed(context.Context, config.CreateShedRequest) (*config.Shed, error) {
	return nil, fmt.Errorf("network profile isolated for x: %w: nft -f: exit status 1", config.ErrNetworkEnforcementUnavailableSentinel)
}

func (enforcementDownFake) StartShed(context.Context, string) (*config.Shed, error) {
	return nil, fmt.Errorf("start x: %w: nft is not installed", config.ErrNetworkEnforcementUnavailableSentinel)
}

func lastCreatedNetwork(t *testing.T, b *clientFakeBackend) string {
	t.Helper()
	c := b.createdSnapshot()
	if len(c) == 0 {
		t.Fatal("backend saw no create")
	}
	return c[len(c)-1].Network
}

func TestNetworkProfileResolution(t *testing.T) {
	t.Run("admin default is the server default", func(t *testing.T) {
		be := newClientFakeBackend()
		s, router := clientTestServer(t, be, defaultTestPolicy())
		if r := do(router, adminLeaf(), "POST", "/api/sheds", `{"name":"a1"}`); r.code != http.StatusCreated {
			t.Fatalf("create = %d %s", r.code, r.body)
		}
		if got := lastCreatedNetwork(t, be); got != config.NetworkProfileOpen {
			t.Errorf("network = %q, want open", got)
		}
		s.cfg.Network = &config.NetworkConfig{DefaultProfile: config.NetworkProfileIsolated}
		if r := do(router, adminLeaf(), "POST", "/api/sheds", `{"name":"a2"}`); r.code != http.StatusCreated {
			t.Fatalf("create = %d %s", r.code, r.body)
		}
		if got := lastCreatedNetwork(t, be); got != config.NetworkProfileIsolated {
			t.Errorf("network = %q, want the isolated default", got)
		}
		// An explicit open overrides an isolated default.
		if r := do(router, adminLeaf(), "POST", "/api/sheds", `{"name":"a3","network":"open"}`); r.code != http.StatusCreated {
			t.Fatalf("create = %d %s", r.code, r.body)
		}
		if got := lastCreatedNetwork(t, be); got != config.NetworkProfileOpen {
			t.Errorf("network = %q, want open", got)
		}
	})
	t.Run("admin explicit", func(t *testing.T) {
		be := newClientFakeBackend()
		_, router := clientTestServer(t, be, defaultTestPolicy())
		if r := do(router, adminLeaf(), "POST", "/api/sheds", `{"name":"a1","network":"isolated"}`); r.code != http.StatusCreated {
			t.Fatalf("create = %d %s", r.code, r.body)
		}
		if got := lastCreatedNetwork(t, be); got != config.NetworkProfileIsolated {
			t.Errorf("network = %q, want isolated", got)
		}
	})
	t.Run("admin invalid is 400 before the backend", func(t *testing.T) {
		be := newClientFakeBackend()
		_, router := clientTestServer(t, be, defaultTestPolicy())
		for _, body := range []string{`{"name":"a1","network":"none"}`, `{"name":"a1","network":"Isolated"}`} {
			r := do(router, adminLeaf(), "POST", "/api/sheds", body)
			if r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidNetworkProfile {
				t.Errorf("%s = %d %s", body, r.code, r.body)
			}
			// SSE is no different: refused before the stream starts.
			r = do(router, adminLeaf(), "POST", "/api/sheds", body, "Accept", "text/event-stream")
			if r.code != http.StatusBadRequest {
				t.Errorf("%s (SSE) = %d %s", body, r.code, r.body)
			}
		}
		if len(be.createdSnapshot()) != 0 {
			t.Error("an invalid profile reached the backend")
		}
	})
	t.Run("client is pinned", func(t *testing.T) {
		be := newClientFakeBackend()
		_, router := clientTestServer(t, be, defaultTestPolicy())
		client := serviceLeaf(testClientName)
		// Omitted: the policy's profile (isolated by default).
		if r := do(router, client, "POST", "/api/sheds", `{"name":"nocturn-a"}`); r.code != http.StatusCreated {
			t.Fatalf("create = %d %s", r.code, r.body)
		}
		if got := lastCreatedNetwork(t, be); got != config.NetworkProfileIsolated {
			t.Errorf("network = %q, want isolated", got)
		}
		// The same value explicitly is fine.
		if r := do(router, client, "POST", "/api/sheds", `{"name":"nocturn-b","network":"isolated"}`); r.code != http.StatusCreated {
			t.Fatalf("create = %d %s", r.code, r.body)
		}
		// Anything else is a policy violation, whatever it is.
		n := len(be.createdSnapshot())
		for _, net := range []string{"open", "none"} {
			r := do(router, client, "POST", "/api/sheds", `{"name":"nocturn-c","network":"`+net+`"}`)
			if r.code != http.StatusForbidden || r.errCode(t) != config.ErrPolicyViolation || !strings.Contains(r.errMessage(t), "network") {
				t.Errorf("network %s = %d %s", net, r.code, r.body)
			}
		}
		if len(be.createdSnapshot()) != n {
			t.Error("a refused profile reached the backend")
		}
	})
	t.Run("client pinned open", func(t *testing.T) {
		be := newClientFakeBackend()
		p := defaultTestPolicy()
		p.Network = config.NetworkProfileOpen
		_, router := clientTestServer(t, be, p)
		client := serviceLeaf(testClientName)
		if r := do(router, client, "POST", "/api/sheds", `{"name":"nocturn-a"}`); r.code != http.StatusCreated {
			t.Fatalf("create = %d %s", r.code, r.body)
		}
		if got := lastCreatedNetwork(t, be); got != config.NetworkProfileOpen {
			t.Errorf("network = %q, want open", got)
		}
		if r := do(router, client, "POST", "/api/sheds", `{"name":"nocturn-b","network":"isolated"}`); r.code != http.StatusForbidden {
			t.Errorf("isolated on an open-pinned client = %d %s", r.code, r.body)
		}
	})
	t.Run("vz refuses non-open with 422", func(t *testing.T) {
		inner := newClientFakeBackend()
		_, router := clientTestServer(t, vzClientFake{inner}, defaultTestPolicy())
		r := do(router, adminLeaf(), "POST", "/api/sheds", `{"name":"a1","network":"isolated"}`)
		if r.code != http.StatusUnprocessableEntity || r.errCode(t) != config.ErrNetworkProfileUnsupported {
			t.Errorf("isolated on vz = %d %s", r.code, r.body)
		}
		// A client pinned isolated (config validation forbids that on VZ,
		// but the API refuses it too).
		r = do(router, serviceLeaf(testClientName), "POST", "/api/sheds", `{"name":"nocturn-a"}`)
		if r.code != http.StatusUnprocessableEntity {
			t.Errorf("isolated client on vz = %d %s", r.code, r.body)
		}
		if len(inner.createdSnapshot()) != 0 {
			t.Error("a refused profile reached the backend")
		}
		// Open is fine on VZ.
		if r := do(router, adminLeaf(), "POST", "/api/sheds", `{"name":"a2","network":"open"}`); r.code != http.StatusCreated {
			t.Errorf("open on vz = %d %s", r.code, r.body)
		}
	})
	t.Run("enforcement unavailable is 503", func(t *testing.T) {
		be := enforcementDownFake{newClientFakeBackend(config.Shed{Name: "x", Status: config.StatusStopped})}
		_, router := clientTestServer(t, be, defaultTestPolicy())
		r := do(router, adminLeaf(), "POST", "/api/sheds", `{"name":"x2","network":"isolated"}`)
		if r.code != http.StatusServiceUnavailable || r.errCode(t) != config.ErrNetworkEnforcementUnavailable {
			t.Errorf("create = %d %s", r.code, r.body)
		}
		r = do(router, adminLeaf(), "POST", "/api/sheds/x/start", "")
		if r.code != http.StatusServiceUnavailable || r.errCode(t) != config.ErrNetworkEnforcementUnavailable {
			t.Errorf("start = %d %s", r.code, r.body)
		}
	})
}

// "nft: command not found" must never read as a missing shed: the sentinel
// is matched before the "not found" string fallback.
func TestMapBackendErrorNetworkSentinels(t *testing.T) {
	code, errCode, _ := mapBackendError(fmt.Errorf("%w: nft: command not found", config.ErrNetworkEnforcementUnavailableSentinel))
	if code != http.StatusServiceUnavailable || errCode != config.ErrNetworkEnforcementUnavailable {
		t.Errorf("enforcement = %d %s", code, errCode)
	}
	code, errCode, _ = mapBackendError(fmt.Errorf("%w: isolated", config.ErrNetworkProfileUnsupportedSentinel))
	if code != http.StatusUnprocessableEntity || errCode != config.ErrNetworkProfileUnsupported {
		t.Errorf("unsupported = %d %s", code, errCode)
	}
}

func TestNetworkProfilesFeature(t *testing.T) {
	fc := NewServer(newClientFakeBackend(), &config.ServerConfig{Name: "t"}, "", nil, nil)
	if !sliceHas(fc.serverFeatures(), FeatureNetworkProfiles) {
		t.Errorf("firecracker features %v lack %s", fc.serverFeatures(), FeatureNetworkProfiles)
	}
	vz := NewServer(vzClientFake{newClientFakeBackend()}, &config.ServerConfig{Name: "t"}, "", nil, nil)
	if sliceHas(vz.serverFeatures(), FeatureNetworkProfiles) {
		t.Errorf("vz advertises %s", FeatureNetworkProfiles)
	}
	if FeatureNetworkProfiles != config.FeatureNetworkProfiles {
		t.Error("the CLI's token and the server's differ")
	}
}
