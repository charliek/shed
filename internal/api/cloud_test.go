package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// Cloud sheds at the API (plan 041 criterion 4): "cloud" on a remote record
// on every route that returns one, and on no other record; extend and usage
// only where the backend has leases; a create's lease from a service client
// only.

var (
	cloudAt  = time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	warn7200 = int64(7200)
)

// leaseBackend is the client fake with leases (the remote backend's
// capabilities): it extends and counts, and its "cloudy" shed is a cloud
// shed.
type leaseBackend struct {
	*clientFakeBackend
	extendErr error
	extended  []string
	ids       []string
	asked     []string
	// local names the local admins: not the account service's people.
	local map[string]bool
}

func newLeaseBackend() *leaseBackend {
	cloud := &config.ShedCloud{
		Provider: "gce", Size: config.CloudSize{Name: "standard", CPUs: 2, MemoryMB: 8192}, DiskGB: 50, Image: "shed-full-1",
		CreatedAt: cloudAt, StartedAt: &cloudAt,
		Lease: &config.CloudLease{ExpiresAt: cloudAt.Add(24 * time.Hour), MaxExpiresAt: cloudAt.Add(120 * time.Hour), Extendable: true, PresetsH: []int{6, 12}, WarnS: &warn7200},
	}
	return &leaseBackend{clientFakeBackend: newClientFakeBackend(
		config.Shed{Name: "cloudy", Status: config.StatusRunning, Backend: config.BackendRemote, CreatedAt: cloudAt, Cloud: cloud},
		config.Shed{Name: "nocturn-cloud", Status: config.StatusRunning, Backend: config.BackendRemote, Owner: testClientName, CreatedAt: cloudAt, Cloud: cloud},
		config.Shed{Name: "local", Status: config.StatusRunning, Backend: config.BackendFirecracker, CreatedAt: cloudAt},
	)}
}

func (b *leaseBackend) ExtendShed(ctx context.Context, name string, hours int, rid string) (*config.Shed, error) {
	if err := b.beforeLocked(ctx, name); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.extended = append(b.extended, name)
	b.ids = append(b.ids, rid)
	err := b.extendErr
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return b.GetShed(ctx, name)
}

// ServicePerson: everyone but the names in local is the account service's.
func (b *leaseBackend) ServicePerson(name string) bool { return !b.local[name] }

func (b *leaseBackend) CloudUsage(_ context.Context, user string) (config.CloudUsage, error) {
	b.mu.Lock()
	b.asked = append(b.asked, user)
	b.mu.Unlock()
	u := config.CloudUsage{Org: config.CloudCounts{Machines: 7, Running: 5, Stopped: 2, Max: 10}}
	if user != "" {
		u.Person = &config.CloudCounts{Machines: 3, Running: 2, Stopped: 1, Max: 4}
	}
	return u, nil
}

// cloudOf returns the "cloud" members of every record in a route's answer,
// by shed name (nil where a record has none).
func cloudsOf(t *testing.T, label string, resp apiResp) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	for _, rec := range recordsOf(t, label, resp) {
		name, _ := rec.str("name")
		out[name] = rec["cloud"]
	}
	return out
}

func TestCloudOnRemoteRecordsOnly(t *testing.T) {
	be := newLeaseBackend()
	_, router := clientTestServer(t, be, defaultTestPolicy())
	check := func(label string, clouds map[string]json.RawMessage, names ...string) {
		t.Helper()
		for _, n := range names {
			raw, ok := clouds[n]
			if !ok {
				t.Errorf("%s: no record %s", label, n)
				continue
			}
			var c config.ShedCloud
			if n == "local" {
				if raw != nil {
					t.Errorf("%s: a local shed carries cloud: %s", label, raw)
				}
				continue
			}
			if err := json.Unmarshal(raw, &c); err != nil || c.Size.Name != "standard" || c.Lease == nil || !c.Lease.ExpiresAt.Equal(cloudAt.Add(24*time.Hour)) || c.Lease.WarnS == nil || *c.Lease.WarnS != 7200 {
				t.Errorf("%s: %s's cloud = %s (%v)", label, n, raw, err)
			}
		}
	}
	check("list", cloudsOf(t, "list", do(router, adminLeaf(), http.MethodGet, "/api/sheds", "")), "cloudy", "local")
	for _, n := range []string{"cloudy", "local"} {
		check("get "+n, cloudsOf(t, "get", do(router, adminLeaf(), http.MethodGet, "/api/sheds/"+n, "")), n)
	}
	check("overview", cloudsOf(t, "overview", do(router, adminLeaf(), http.MethodGet, "/api/overview", "")), "cloudy", "local")
	// A service client gets its shed's cloud too: nothing in it names a
	// project, a zone or an address.
	check("client list", cloudsOf(t, "client list", do(router, serviceLeaf(testClientName), http.MethodGet, "/api/sheds", "")), "nocturn-cloud")
	check("client get", cloudsOf(t, "client get", do(router, serviceLeaf(testClientName), http.MethodGet, "/api/sheds/nocturn-cloud", "")), "nocturn-cloud")
}

// The feature token: lease where the backend has leases, not elsewhere.
func TestLeaseFeatureToken(t *testing.T) {
	s, _ := clientTestServer(t, newLeaseBackend(), defaultTestPolicy())
	if !slices.Contains(s.serverFeatures(), FeatureLease) {
		t.Errorf("a backend with leases: features %v lack %s", s.serverFeatures(), FeatureLease)
	}
	s, _ = clientTestServer(t, newClientFakeBackend(), defaultTestPolicy())
	if slices.Contains(s.serverFeatures(), FeatureLease) {
		t.Errorf("a local backend advertises %s", FeatureLease)
	}
}

func TestExtendRoute(t *testing.T) {
	t.Run("501 on a local backend", func(t *testing.T) {
		_, router := clientTestServer(t, newClientFakeBackend(config.Shed{Name: "local", CreatedAt: cloudAt}), defaultTestPolicy())
		r := do(router, adminLeaf(), http.MethodPost, "/api/sheds/local/extend", `{"hours":6}`)
		if r.code != http.StatusNotImplemented || r.errCode(t) != config.ErrFeatureUnsupported {
			t.Fatalf("extend on a local backend = %d %s, want 501 FEATURE_UNSUPPORTED", r.code, r.body)
		}
	})
	be := newLeaseBackend()
	_, router := clientTestServer(t, be, defaultTestPolicy())
	t.Run("hours out of range", func(t *testing.T) {
		for _, body := range []string{`{"hours":0}`, `{"hours":721}`, `{"hours":-2}`, `{}`, `{"hours":"6"}`, `{"hours":6,"by":1}`, `{"hours":6,"request_id":"x"}`} {
			r := do(router, adminLeaf(), http.MethodPost, "/api/sheds/cloudy/extend", body)
			if r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidRequest {
				t.Errorf("extend %s = %d %s, want 400 INVALID_REQUEST", body, r.code, r.body)
			}
		}
	})
	t.Run("a service client", func(t *testing.T) {
		r := do(router, serviceLeaf(testClientName), http.MethodPost, "/api/sheds/nocturn-cloud/extend", `{"hours":6}`)
		if r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden {
			t.Fatalf("a client's extend = %d %s, want 403 FORBIDDEN", r.code, r.body)
		}
	})
	t.Run("an admin", func(t *testing.T) {
		r := do(router, adminLeaf(), http.MethodPost, "/api/sheds/cloudy/extend", `{"hours":6}`)
		if r.code != http.StatusOK {
			t.Fatalf("extend = %d %s", r.code, r.body)
		}
		// With the caller's request id, the backend gets one derived from
		// it (the same for a retry); without one, none.
		const rid = "6f9619ff-8b86-4d01-b42d-00cf4fc964ff"
		for i := 0; i < 2; i++ {
			if r := do(router, adminLeaf(), http.MethodPost, "/api/sheds/cloudy/extend", `{"hours":6,"request_id":"`+rid+`"}`); r.code != http.StatusOK {
				t.Fatalf("extend with an id = %d %s", r.code, r.body)
			}
		}
		ids := be.ids[len(be.ids)-3:]
		if ids[0] != "" || ids[1] == "" || ids[1] == rid || ids[1] != ids[2] {
			t.Fatalf("request ids reaching the backend: %q", ids)
		}
		if c := cloudsOf(t, "extend", r)["cloudy"]; c == nil {
			t.Fatalf("the extend's answer has no cloud: %s", r.body)
		}
	})
	t.Run("each refusal by its code", func(t *testing.T) {
		for _, tc := range []struct {
			status int
			code   string
		}{
			{http.StatusForbidden, config.ErrExtendNotAllowed},
			{http.StatusConflict, config.ErrLeaseAtCap},
			{http.StatusConflict, config.ErrLeaseEnded},
			{http.StatusForbidden, config.ErrPersonInactive},
			{http.StatusConflict, config.ErrComputeDeleting},
			{http.StatusServiceUnavailable, config.ErrComputeUnavailable},
		} {
			be.extendErr = &config.ComputeRefusal{Status: tc.status, Code: tc.code, Err: errors.New("refused")}
			r := do(router, adminLeaf(), http.MethodPost, "/api/sheds/cloudy/extend", `{"hours":6}`)
			if r.code != tc.status || r.errCode(t) != tc.code {
				t.Errorf("%s: %d %s", tc.code, r.code, r.body)
			}
		}
		be.extendErr = nil
	})
}

func TestCloudUsageRoute(t *testing.T) {
	t.Run("501 without compute", func(t *testing.T) {
		_, router := clientTestServer(t, newClientFakeBackend(), defaultTestPolicy())
		r := do(router, adminLeaf(), http.MethodGet, "/api/cloud/usage", "")
		if r.code != http.StatusNotImplemented || r.errCode(t) != config.ErrFeatureUnsupported {
			t.Fatalf("usage = %d %s, want 501", r.code, r.body)
		}
	})
	be := newLeaseBackend()
	_, router := clientTestServer(t, be, defaultTestPolicy())
	t.Run("a service client: the organization's", func(t *testing.T) {
		r := do(router, serviceLeaf(testClientName), http.MethodGet, "/api/cloud/usage", "")
		if r.code != http.StatusOK || !strings.Contains(string(r.body), `"person":null`) || !strings.Contains(string(r.body), `"org":{"machines":7,"running":5,"stopped":2,"max":10}`) {
			t.Fatalf("a client's usage = %d %s", r.code, r.body)
		}
		r = do(router, serviceLeaf(testClientName), http.MethodGet, "/api/cloud/usage?user=ada", "")
		if r.code != http.StatusForbidden {
			t.Fatalf("a client naming a user = %d %s, want 403", r.code, r.body)
		}
	})
	t.Run("an admin of the service names a user", func(t *testing.T) {
		_, ur := usersTestServer(t, be)
		r := doAs(ur, rootFP, http.MethodGet, "/api/cloud/usage?user=ada", "")
		var u config.CloudUsage
		if r.code != http.StatusOK || json.Unmarshal(r.body, &u) != nil || u.Person == nil || u.Person.Max != 4 {
			t.Fatalf("usage ?user=ada = %d %s", r.code, r.body)
		}
		if got := be.asked[len(be.asked)-1]; got != "ada" {
			t.Fatalf("the backend was asked about %q", got)
		}
	})
	t.Run("a local admin names nobody", func(t *testing.T) {
		be.local = map[string]bool{"root": true}
		defer func() { be.local = nil }()
		_, ur := usersTestServer(t, be)
		for _, q := range []string{"?user=ada", "?user=root"} {
			if r := doAs(ur, rootFP, http.MethodGet, "/api/cloud/usage"+q, ""); r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden {
				t.Errorf("a local admin's usage%s = %d %s, want 403", q, r.code, r.body)
			}
		}
		if r := doAs(ur, rootFP, http.MethodGet, "/api/cloud/usage", ""); r.code != http.StatusOK {
			t.Errorf("a local admin's own usage = %d %s", r.code, r.body)
		}
	})
	t.Run("the implicit user names nobody", func(t *testing.T) {
		if r := do(router, adminLeaf(), http.MethodGet, "/api/cloud/usage?user=ada", ""); r.code != http.StatusForbidden {
			t.Fatalf("the implicit user's usage ?user= = %d %s, want 403", r.code, r.body)
		}
	})
	t.Run("user twice or not a name", func(t *testing.T) {
		for _, q := range []string{"?user=a&user=b", "?user=A%20B"} {
			if r := do(router, adminLeaf(), http.MethodGet, "/api/cloud/usage"+q, ""); r.code != http.StatusBadRequest {
				t.Errorf("usage%s = %d %s, want 400", q, r.code, r.body)
			}
		}
	})
}

func TestCreateLease(t *testing.T) {
	be := newLeaseBackend()
	_, router := clientTestServer(t, be, defaultTestPolicy())
	t.Run("a client's lease reaches the backend", func(t *testing.T) {
		r := do(router, serviceLeaf(testClientName), http.MethodPost, "/api/sheds", `{"name":"nocturn-a","lease":"24h"}`)
		if r.code != http.StatusCreated {
			t.Fatalf("create = %d %s", r.code, r.body)
		}
		created := be.createdSnapshot()
		if got := created[len(created)-1]; got.ClientLease != 24*time.Hour || got.Owner != testClientName {
			t.Fatalf("the backend got lease %v owner %q", got.ClientLease, got.Owner)
		}
	})
	t.Run("absent: none sent (the backend takes max_age)", func(t *testing.T) {
		r := do(router, serviceLeaf(testClientName), http.MethodPost, "/api/sheds", `{"name":"nocturn-b"}`)
		if r.code != http.StatusCreated {
			t.Fatalf("create = %d %s", r.code, r.body)
		}
		created := be.createdSnapshot()
		if got := created[len(created)-1]; got.ClientLease != 0 {
			t.Fatalf("the backend got lease %v", got.ClientLease)
		}
	})
	t.Run("above max_age", func(t *testing.T) {
		r := do(router, serviceLeaf(testClientName), http.MethodPost, "/api/sheds", `{"name":"nocturn-c","lease":"31h"}`)
		if r.code != http.StatusForbidden || r.errCode(t) != config.ErrPolicyViolation || !strings.Contains(r.errMessage(t), "max_age") {
			t.Fatalf("a lease above max_age = %d %s, want 403 POLICY_VIOLATION", r.code, r.body)
		}
	})
	t.Run("not a duration", func(t *testing.T) {
		for _, l := range []string{"soon", "30s", "90.5s", "-1h"} {
			r := do(router, serviceLeaf(testClientName), http.MethodPost, "/api/sheds", `{"name":"nocturn-d","lease":"`+l+`"}`)
			if r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidRequest {
				t.Errorf("lease %q = %d %s, want 400", l, r.code, r.body)
			}
		}
	})
	t.Run("a person", func(t *testing.T) {
		n := len(be.createdSnapshot())
		r := do(router, adminLeaf(), http.MethodPost, "/api/sheds", `{"name":"mine","lease":"24h"}`)
		if r.code != http.StatusUnprocessableEntity || r.errCode(t) != config.ErrFeatureUnsupported {
			t.Fatalf("a person's lease = %d %s, want 422 FEATURE_UNSUPPORTED", r.code, r.body)
		}
		if len(be.createdSnapshot()) != n {
			t.Fatal("a refused create reached the backend")
		}
	})
	t.Run("a server without leases", func(t *testing.T) {
		_, router := clientTestServer(t, newClientFakeBackend(), defaultTestPolicy())
		r := do(router, serviceLeaf(testClientName), http.MethodPost, "/api/sheds", `{"name":"nocturn-e","lease":"24h"}`)
		if r.code != http.StatusUnprocessableEntity || r.errCode(t) != config.ErrFeatureUnsupported {
			t.Fatalf("a lease on a local server = %d %s, want 422 FEATURE_UNSUPPORTED", r.code, r.body)
		}
	})
}

// A compute refusal's reason reaches the caller inside error, on a JSON
// answer and on a create's stream.
func TestComputeRefusalReasonOnTheWire(t *testing.T) {
	be := newLeaseBackend()
	_, router := clientTestServer(t, be, defaultTestPolicy())
	be.extendErr = &config.ComputeRefusal{Status: http.StatusServiceUnavailable, Code: config.ErrComputeLimitReached, Reason: "organization", Err: errors.New("x")}
	r := do(router, adminLeaf(), http.MethodPost, "/api/sheds/cloudy/extend", `{"hours":6}`)
	var e config.APIError
	if err := json.Unmarshal(r.body, &e); err != nil || e.Error.Code != config.ErrComputeLimitReached || e.Error.Reason != "organization" {
		t.Fatalf("the refusal = %d %s", r.code, r.body)
	}
}
