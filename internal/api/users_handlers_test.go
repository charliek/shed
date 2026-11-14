package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
)

// fakeRefresher answers Refresh with a fixed report and counts the calls.
type fakeRefresher struct {
	report users.RefreshReport
	err    error
	calls  atomic.Int64
}

func (f *fakeRefresher) Refresh(context.Context) (users.RefreshReport, error) {
	f.calls.Add(1)
	return f.report, f.err
}

func sampleRefreshReport() users.RefreshReport {
	return users.RefreshReport{Logins: []users.LoginRefresh{
		{User: "", Login: "octocat", Status: users.RefreshOK, PinnedID: 583231, Keys: 2, Added: []string{"SHA256:new"}},
		{User: "", Login: "hubot", Status: users.RefreshMismatch, PinnedID: 1, SeenID: 2, Error: ""},
	}}
}

// TestUsersRefreshRoute: an admin forces a fetch and gets one row per login;
// a user who is not an admin is refused before anything is fetched.
func TestUsersRefreshRoute(t *testing.T) {
	s, router := usersTestServer(t, newUserFakeBackend())
	ref := &fakeRefresher{report: sampleRefreshReport()}
	s.SetKeyRefresher(ref)

	bob := doAs(router, bobFP, http.MethodPost, "/api/users/refresh", "")
	if bob.code != http.StatusForbidden || bob.errCode(t) != config.ErrForbidden {
		t.Errorf("non-admin = %d %s, want 403 FORBIDDEN", bob.code, bob.body)
	}
	if ref.calls.Load() != 0 {
		t.Error("a non-admin's request reached the fetch")
	}

	root := doAs(router, rootFP, http.MethodPost, "/api/users/refresh", "")
	if root.code != http.StatusOK {
		t.Fatalf("admin = %d %s, want 200", root.code, root.body)
	}
	if ref.calls.Load() != 1 {
		t.Errorf("fetches = %d, want 1", ref.calls.Load())
	}
	var resp config.UsersRefreshResponse
	if err := json.Unmarshal(root.body, &resp); err != nil {
		t.Fatal(err)
	}
	want := []config.GitHubLoginRefresh{
		{User: "", Login: "octocat", Status: "ok", GitHubID: 583231, Keys: 2, Added: []string{"SHA256:new"}},
		{User: "", Login: "hubot", Status: "mismatch", GitHubID: 1, SeenID: 2},
	}
	if len(resp.Logins) != len(want) {
		t.Fatalf("rows = %+v, want %+v", resp.Logins, want)
	}
	for i := range want {
		g, w := resp.Logins[i], want[i]
		if g.User != w.User || g.Login != w.Login || g.Status != w.Status || g.GitHubID != w.GitHubID ||
			g.SeenID != w.SeenID || g.Keys != w.Keys || len(g.Added) != len(w.Added) {
			t.Errorf("row %d = %+v, want %+v", i, g, w)
		}
	}
}

// TestUsersRefreshOpenMode: on an open server (loopback only) the caller is
// the implicit user, an admin, and may refresh.
func TestUsersRefreshOpenMode(t *testing.T) {
	s := NewServer(newUserFakeBackend(), &config.ServerConfig{Auth: &config.AuthConfig{Mode: config.AuthModeOpen}}, "", nil, nil)
	ref := &fakeRefresher{report: sampleRefreshReport()}
	s.SetKeyRefresher(ref)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/users/refresh", nil))
	if rec.Code != http.StatusOK || ref.calls.Load() != 1 {
		t.Errorf("open mode = %d %s (fetches %d), want 200 and one fetch", rec.Code, rec.Body, ref.calls.Load())
	}
}

// TestUsersRefreshEdges: no refresher answers with no rows; a fetch that
// outlives the request is a 504, any other failure a 500.
func TestUsersRefreshEdges(t *testing.T) {
	s, router := usersTestServer(t, newUserFakeBackend())
	r := doAs(router, rootFP, http.MethodPost, "/api/users/refresh", "")
	if r.code != http.StatusOK || string(r.body) != "{\"logins\":[]}\n" {
		t.Errorf("no refresher = %d %q, want 200 with no rows", r.code, r.body)
	}

	s.SetKeyRefresher(&fakeRefresher{err: context.DeadlineExceeded})
	if r := doAs(router, rootFP, http.MethodPost, "/api/users/refresh", ""); r.code != http.StatusGatewayTimeout {
		t.Errorf("timed out fetch = %d %s, want 504", r.code, r.body)
	}
	s.SetKeyRefresher(&fakeRefresher{err: errors.New("boom")})
	if r := doAs(router, rootFP, http.MethodPost, "/api/users/refresh", ""); r.code != http.StatusInternalServerError {
		t.Errorf("failed fetch = %d %s, want 500", r.code, r.body)
	}
}
