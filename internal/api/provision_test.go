package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/lockmap"
	"github.com/charliek/shed/internal/users"
)

// POST /api/sheds/{name}/provision: the shed's own user on their running
// shed is let through; a script key of that user, an admin on a service
// client's shed (which an admin may enter), a stopped shed and a provision
// already in flight are refused before any stream is opened, and none of
// them reaches the backend.

// provisionFakeBackend is the user fake with a real provisioning slot per
// shed: the shed named busy always has its slot taken, and a run of the
// shed named slow holds its slot until slow is closed.
type provisionFakeBackend struct {
	*userFakeBackend
	slots   *lockmap.NamedMutexMap
	slow    chan struct{}
	running chan struct{}
}

func (b provisionFakeBackend) BeginProvision(ctx context.Context, name string) (backend.ProvisionRun, error) {
	release, ok := b.slots.TryAcquire(name)
	if !ok || name == "busy" {
		if ok {
			release()
		}
		return nil, fmt.Errorf("%w: %s", config.ErrProvisionBusySentinel, name)
	}
	run, err := b.userFakeBackend.BeginProvision(ctx, name)
	if err != nil {
		release()
		return nil, err
	}
	return slowRun{ProvisionRun: run, release: release, b: b, name: name}, nil
}

type slowRun struct {
	backend.ProvisionRun
	release func()
	b       provisionFakeBackend
	name    string
}

func (r slowRun) Run(ctx context.Context) (*config.Shed, error) {
	if r.name == "slow" {
		r.b.running <- struct{}{}
		<-r.b.slow
	}
	return r.ProvisionRun.Run(ctx)
}

func (r slowRun) Release() { r.release() }

func newProvisionFake(ufb *userFakeBackend) provisionFakeBackend {
	return provisionFakeBackend{userFakeBackend: ufb, slots: &lockmap.NamedMutexMap{}, slow: make(chan struct{}), running: make(chan struct{}, 1)}
}

func TestProvisionRoute(t *testing.T) {
	ufb := newUserFakeBackend()
	at := ufb.sheds["box"].CreatedAt
	ufb.sheds["stopped"] = config.Shed{Name: "stopped", User: "alice", Status: config.StatusStopped, CreatedAt: at}
	ufb.sheds["busy"] = config.Shed{Name: "busy", User: "alice", Status: config.StatusRunning, CreatedAt: at}
	ufb.sheds["ci"] = config.Shed{Name: "ci", Owner: "nocturn", Status: config.StatusRunning, CreatedAt: at}
	s, router := usersTestServer(t, newProvisionFake(ufb))
	scriptFP := testFingerprint("alice-ci")
	lookup := userLookup()
	lookup.AddKey("alice", users.Key{Fingerprint: scriptFP, Source: users.SourceInline, Script: true})
	s.SetUserLookup(lookup)

	post := func(fp, user, name string, stream bool) apiResp {
		var hdr []string
		if stream {
			hdr = []string{"Accept", "text/event-stream"}
		}
		return do(router, clientCertNaming(fp, user, authtoken.ScopeControl, time.Hour), http.MethodPost, "/api/sheds/"+name+"/provision", "", hdr...)
	}
	cases := []struct {
		name, fp, user, shed string
		stream               bool
		code                 int
		errCode              string
	}{
		{"the shed's user", aliceFP, "alice", "box", false, http.StatusOK, ""},
		{"the shed's user, streamed", aliceFP, "alice", "box", true, http.StatusOK, ""},
		{"a script key of the shed's user", scriptFP, "alice", "box", true, http.StatusForbidden, config.ErrForbidden},
		{"an admin on another user's shed", rootFP, "root", "box", true, http.StatusForbidden, config.ErrForbidden},
		{"an admin on a service client's shed", rootFP, "root", "ci", true, http.StatusForbidden, config.ErrForbidden},
		{"another user", bobFP, "bob", "box", true, http.StatusNotFound, config.ErrShedNotFound},
		{"a stopped shed", aliceFP, "alice", "stopped", true, http.StatusConflict, config.ErrShedAlreadyStopped},
		{"a provision in flight", aliceFP, "alice", "busy", false, http.StatusConflict, config.ErrProvisionInProgress},
		{"a provision in flight, streamed", aliceFP, "alice", "busy", true, http.StatusConflict, config.ErrProvisionInProgress},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ufb.reset()
			r := post(tc.fp, tc.user, tc.shed, tc.stream)
			if r.code != tc.code {
				t.Fatalf("= %d %s, want %d", r.code, r.body, tc.code)
			}
			if tc.errCode != "" {
				if got := r.errCode(t); got != tc.errCode {
					t.Fatalf("code %s, want %s", got, tc.errCode)
				}
				if got := ufb.actedSnapshot(); len(got) != 0 && tc.shed != "busy" {
					t.Fatalf("a refused provision reached the backend: %v", got)
				}
				return
			}
			if got := ufb.actedSnapshot(); !slices.Contains(got, "provision "+tc.shed) {
				t.Fatalf("acted %v", got)
			}
			if tc.stream && !strings.Contains(string(r.body), "event: complete") {
				t.Fatalf("stream = %s", r.body)
			}
		})
	}

	// The server advertises delivery.
	r := do(router, clientCertNaming(aliceFP, "alice", authtoken.ScopeControl, time.Hour), http.MethodGet, "/api/info", "")
	var info config.ServerInfo
	if err := json.Unmarshal(r.body, &info); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(info.Features, FeatureDelivery) {
		t.Fatalf("features = %v", info.Features)
	}
}

// Two streamed provisions of one shed at once: the second is refused with a
// real 409 before any stream opens, while the first holds the shed's slot
// through its run; once it is done, a third is let in.
func TestProvisionRouteAdmitsOneAtATime(t *testing.T) {
	ufb := newUserFakeBackend()
	ufb.sheds["slow"] = config.Shed{Name: "slow", User: "alice", Status: config.StatusRunning, CreatedAt: ufb.sheds["box"].CreatedAt}
	pf := newProvisionFake(ufb)
	_, router := usersTestServer(t, pf)
	post := func() apiResp {
		return do(router, clientCertNaming(aliceFP, "alice", authtoken.ScopeControl, time.Hour), http.MethodPost, "/api/sheds/slow/provision", "", "Accept", "text/event-stream")
	}
	first := make(chan apiResp, 1)
	go func() { first <- post() }()
	select {
	case <-pf.running:
	case <-time.After(10 * time.Second):
		t.Fatal("the first provision never ran")
	}
	second := post()
	if second.code != http.StatusConflict || second.errCode(t) != config.ErrProvisionInProgress {
		t.Fatalf("second streamed provision = %d %s, want 409 PROVISION_IN_PROGRESS", second.code, second.body)
	}
	if ct := second.header.Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Fatalf("the refusal opened a stream: %s", ct)
	}
	close(pf.slow)
	if r := <-first; r.code != http.StatusOK || !strings.Contains(string(r.body), "event: complete") {
		t.Fatalf("first = %d %s", r.code, r.body)
	}
	if r := post(); r.code != http.StatusOK {
		t.Fatalf("a provision after the first finished = %d %s", r.code, r.body)
	}
}
