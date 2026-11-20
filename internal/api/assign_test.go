package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// TestAssignShedRoute drives POST /api/sheds/{name}/assign through the auth
// middleware and the guard, for each refusal and for a success, and then the
// access the new user and the assigning admin get, before and after that
// admin is demoted.
func TestAssignShedRoute(t *testing.T) {
	be := newUserFakeBackend()
	at := be.sheds["box"].CreatedAt
	none := config.SelectedMounts(nil)
	be.sheds["legacy"] = config.Shed{Name: "legacy", Status: config.StatusStopped, CreatedAt: at, Mounts: none}
	be.sheds["old-all"] = config.Shed{Name: "old-all", Status: config.StatusStopped, CreatedAt: at, Mounts: config.AllMounts()}
	be.sheds["hosty"] = config.Shed{Name: "hosty", Status: config.StatusStopped, CreatedAt: at, Mounts: none,
		ProjectMounts: []config.MountConfig{{Source: "/srv/code", Target: "/home/shed/code"}}}
	be.sheds["svc"] = config.Shed{Name: "svc", Status: config.StatusStopped, CreatedAt: at, Owner: "nocturn", Mounts: none}
	be.sheds["odd"] = config.Shed{Name: "odd", Status: config.StatusStopped, CreatedAt: at, User: config.UserUnresolved, Mounts: none}
	s, router := usersTestServer(t, be)
	assign := func(fp, name, body string) apiResp {
		return doAs(router, fp, http.MethodPost, "/api/sheds/"+name+"/assign", body)
	}

	for _, tc := range []struct {
		who, fp, shed, body string
		code                int
		errCode, msg        string
	}{
		// Not an admin: refused before anything is looked up, so a missing
		// shed and an unknown user answer the same.
		{"bob", bobFP, "legacy", `{"user":"bob"}`, http.StatusForbidden, config.ErrForbidden, "admin only"},
		{"bob", bobFP, "missing", `{"user":"nobody"}`, http.StatusForbidden, config.ErrForbidden, "admin only"},
		{"alice", aliceFP, "box", `{"user":"alice"}`, http.StatusForbidden, config.ErrForbidden, "admin only"},
		// The target must be a configured user.
		{"root", rootFP, "legacy", `{"user":"carol"}`, http.StatusBadRequest, config.ErrInvalidRequest, `no user named "carol"`},
		{"root", rootFP, "legacy", `{"user":""}`, http.StatusBadRequest, config.ErrInvalidRequest, "no user named"},
		{"root", rootFP, "legacy", `{"user":"Bob"}`, http.StatusBadRequest, config.ErrInvalidRequest, "no user named"},
		{"root", rootFP, "legacy", `{}`, http.StatusBadRequest, config.ErrInvalidRequest, "no user named"},
		{"root", rootFP, "legacy", `not json`, http.StatusBadRequest, config.ErrInvalidRequest, `{"user":"<name>"}`},
		{"root", rootFP, "legacy", `{"user":"bob","allow_host_resources":true}`, http.StatusBadRequest, config.ErrInvalidRequest, "unknown field"},
		// The shed.
		{"root", rootFP, "missing", `{"user":"bob"}`, http.StatusNotFound, config.ErrShedNotFound, "missing"},
		{"root", rootFP, "box", `{"user":"bob"}`, http.StatusConflict, config.ErrShedAlreadyAssigned, "already belongs to user alice"},
		{"root", rootFP, "svc", `{"user":"bob"}`, http.StatusConflict, config.ErrShedAlreadyAssigned, "service client"},
		{"root", rootFP, "odd", `{"user":"bob"}`, http.StatusConflict, config.ErrShedAlreadyAssigned, "disagree"},
		{"root", rootFP, "hosty", `{"user":"bob"}`, http.StatusConflict, config.ErrShedHostResources, "recreate it without them"},
		{"root", rootFP, "old-all", `{"user":"bob"}`, http.StatusConflict, config.ErrShedHostResources, "server mounts"},
	} {
		be.reset()
		r := assign(tc.fp, tc.shed, tc.body)
		if r.code != tc.code || r.errCode(t) != tc.errCode || !strings.Contains(r.errMessage(t), tc.msg) {
			t.Errorf("%s assign %s %s = %d %s, want %d %s with %q", tc.who, tc.shed, tc.body, r.code, r.body, tc.code, tc.errCode, tc.msg)
		}
		if got := be.actedSnapshot(); len(got) != 0 {
			t.Errorf("%s assign %s %s changed something: %v", tc.who, tc.shed, tc.body, got)
		}
	}

	// Before: nobody may enter the unassigned shed; the admin may view it.
	if r := doAs(router, bobFP, http.MethodPost, "/api/sheds/legacy/start", ""); r.code != http.StatusNotFound {
		t.Errorf("bob start before = %d %s", r.code, r.body)
	}

	r := assign(rootFP, "legacy", `{"user":"bob"}`)
	if r.code != http.StatusOK {
		t.Fatalf("root assign legacy to bob = %d %s", r.code, r.body)
	}
	var got config.Shed
	if err := json.Unmarshal(r.body, &got); err != nil || got.Name != "legacy" || got.User != "bob" {
		t.Errorf("assign answered %s (%v)", r.body, err)
	}
	if r := assign(rootFP, "legacy", `{"user":"alice"}`); r.code != http.StatusConflict || r.errCode(t) != config.ErrShedAlreadyAssigned {
		t.Errorf("assigning it again = %d %s", r.code, r.body)
	}

	// After: bob enters, the admin views but does not enter.
	if r := doAs(router, bobFP, http.MethodPost, "/api/sheds/legacy/start", ""); r.code != http.StatusOK {
		t.Errorf("bob start after = %d %s", r.code, r.body)
	}
	if r := doAs(router, rootFP, http.MethodPost, "/api/sheds/legacy/start", ""); r.code != http.StatusForbidden {
		t.Errorf("admin start of bob's shed = %d %s", r.code, r.body)
	}
	if r := doAs(router, rootFP, http.MethodGet, "/api/sheds/legacy", ""); r.code != http.StatusOK {
		t.Errorf("admin view of bob's shed = %d %s", r.code, r.body)
	}

	// The admin who assigned it is demoted: nothing about the shed changes.
	s.users.(*userstest.Fake).AddUser(users.User{Name: "root"})
	if r := doAs(router, rootFP, http.MethodGet, "/api/sheds/legacy", ""); r.code != http.StatusNotFound {
		t.Errorf("demoted admin view of bob's shed = %d %s", r.code, r.body)
	}
	if r := assign(rootFP, "old-all", `{"user":"root"}`); r.code != http.StatusForbidden {
		t.Errorf("demoted admin assign = %d %s", r.code, r.body)
	}
	if r := doAs(router, bobFP, http.MethodPost, "/api/sheds/legacy/start", ""); r.code != http.StatusOK {
		t.Errorf("bob start after the demotion = %d %s", r.code, r.body)
	}
	if u := be.sheds["legacy"].User; u != "bob" {
		t.Errorf("stored user after the demotion = %q", u)
	}
}

// TestAssignShedNeedsAUserList: on a server with no user list there is nobody
// to assign a shed to; the implicit user is not a target.
func TestAssignShedNeedsAUserList(t *testing.T) {
	be := newUserFakeBackend()
	be.sheds["legacy"] = config.Shed{Name: "legacy", CreatedAt: be.sheds["box"].CreatedAt, Mounts: config.SelectedMounts(nil)}
	s, router := usersTestServer(t, be)
	s.SetUserLookup(userstest.Implicit(rootFP))
	nameTestKey(t, rootFP, servertls.ImplicitUserValue)
	for _, body := range []string{`{"user":""}`, `{"user":"root"}`} {
		r := doAs(router, rootFP, http.MethodPost, "/api/sheds/legacy/assign", body)
		if r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidRequest {
			t.Errorf("assign %s with no user list = %d %s", body, r.code, r.body)
		}
	}
	if got := be.actedSnapshot(); len(got) != 0 {
		t.Errorf("changed: %v", got)
	}
}
