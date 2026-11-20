package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// statusLookup is a directory server's lookup as the route sees it: the
// users of userLookup (so the front door authenticates as usual) and a
// source state to report.
type statusLookup struct {
	*userstest.Fake
	status config.UsersStatusResponse
}

func (l statusLookup) UsersStatus() config.UsersStatusResponse { return l.status }

// statusRefresher is a directory server's refresher: it reports a source
// state too, so the route knows its pull is a directory's.
type statusRefresher struct {
	*fakeRefresher
}

func (statusRefresher) UsersStatus() config.UsersStatusResponse { return config.UsersStatusResponse{} }

// getStatus is GET /api/users/status as the holder of fp; it decodes a 200.
func getStatus(t *testing.T, h http.Handler, fp string) (apiResp, config.UsersStatusResponse) {
	t.Helper()
	r := doAs(h, fp, http.MethodGet, "/api/users/status", "")
	var st config.UsersStatusResponse
	if r.code == http.StatusOK {
		if err := json.Unmarshal(r.body, &st); err != nil {
			t.Fatalf("status body %s: %v", r.body, err)
		}
	}
	return r, st
}

// noKeysIn fails the test when body holds any of the fingerprints or key
// lines.
func noKeysIn(t *testing.T, body []byte, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if k != "" && strings.Contains(string(body), k) {
			t.Errorf("the status holds a key (%s): %s", k, body)
		}
	}
	if strings.Contains(string(body), "SHA256:") || strings.Contains(string(body), "ssh-") {
		t.Errorf("the status holds a key: %s", body)
	}
}

// TestUsersStatusAdminOnly: the route is an admin's. A user who is not an
// admin is refused; so is a script key, an admin's included; the admin is
// answered with every user (name, admin flag, source) and no key.
func TestUsersStatusAdminOnly(t *testing.T) {
	s, router := usersTestServer(t, newUserFakeBackend())
	l := userLookup()
	rootScript, bobScript := testFingerprint("root-script"), testFingerprint("bob-script")
	l.AddKey("root", users.Key{Fingerprint: rootScript, Source: users.SourceAdHoc, Label: "cron", Script: true})
	l.AddKey("bob", users.Key{Fingerprint: bobScript, Source: users.SourceAdHoc, Label: "ci", Script: true})
	s.SetUserLookup(l)
	nameTestKey(t, rootScript, "root")
	nameTestKey(t, bobScript, "bob")

	for _, tc := range []struct {
		who, fp, msg string
	}{
		{"bob (not an admin)", bobFP, "admin only"},
		{"alice (not an admin)", aliceFP, "admin only"},
		{"bob's script key", bobScript, "admin only"},
		{"root's script key", rootScript, "a script key cannot"},
	} {
		r, _ := getStatus(t, router, tc.fp)
		if r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden || !strings.Contains(r.errMessage(t), tc.msg) {
			t.Errorf("%s: %d %s, want 403 FORBIDDEN %q", tc.who, r.code, r.body, tc.msg)
		}
		if strings.Contains(string(r.body), "alice") || strings.Contains(string(r.body), "root\"") {
			t.Errorf("%s's refusal names a user: %s", tc.who, r.body)
		}
	}

	r, st := getStatus(t, router, rootFP)
	if r.code != http.StatusOK {
		t.Fatalf("admin = %d %s", r.code, r.body)
	}
	want := config.UsersStatusResponse{Source: config.UsersSourceConfig, Users: []config.UserStatus{
		{Name: "alice", Source: config.UserFromConfig},
		{Name: "bob", Source: config.UserFromConfig},
		{Name: "root", Admin: true, Source: config.UserFromConfig},
	}}
	if !reflect.DeepEqual(st, want) {
		t.Errorf("admin's status = %+v, want %+v", st, want)
	}
	noKeysIn(t, r.body, aliceFP, bobFP, rootFP, rootScript, "cron", "ci")
}

// TestUsersStatusNoList: a server with no user list answers source none and
// no users, whether it is open (no lookup at all) or mtls with every key the
// implicit user's.
func TestUsersStatusNoList(t *testing.T) {
	open := NewServer(newUserFakeBackend(), &config.ServerConfig{Auth: &config.AuthConfig{Mode: config.AuthModeOpen}}, "", nil, nil)
	rec := httptest.NewRecorder()
	open.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/users/status", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"source":"none","users":[]}`+"\n" {
		t.Errorf("open server = %d %q, want 200 source none", rec.Code, rec.Body)
	}

	s, router := usersTestServer(t, newUserFakeBackend())
	s.SetUserLookup(userstest.Implicit(rootFP))
	nameTestKey(t, rootFP, servertls.ImplicitUserValue)
	r := doAs(router, rootFP, http.MethodGet, "/api/users/status", "")
	if r.code != http.StatusOK || string(r.body) != `{"source":"none","users":[]}`+"\n" {
		t.Errorf("implicit user = %d %s, want 200 source none", r.code, r.body)
	}
}

// directoryStates are a directory server's source in each of its states, as
// its lookup reports them.
func directoryStates() map[string]config.UsersStatusResponse {
	at := func(s string) *time.Time {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		return &t
	}
	version := strings.Repeat("ab", 32)
	local := config.UserStatus{Name: "root", Admin: true, Source: config.UserFromLocal}
	listed := []config.UserStatus{
		{Name: "ada", Admin: true, Source: config.UserFromDirectory},
		{Name: "bob", Source: config.UserFromDirectory},
		local,
	}
	skipped := []config.SkippedEntry{{Name: "root", Reason: "local-admin-name", Description: "its name is a local admin's"}}
	base := func(state, reason string) *config.DirectoryStatus {
		return &config.DirectoryStatus{URL: "https://accounts.example.test", Organization: "example", ClientID: "srv_" + strings.Repeat("0a", 16),
			State: state, Reason: reason, Copy: "none", Skipped: []config.SkippedEntry{}}
	}
	trusted := base(config.DirectoryStateTrusted, "")
	trusted.LastGoodPull, trusted.Version, trusted.TrustedUntil = at("2026-11-16T09:00:00Z"), version, at("2026-11-19T09:00:00Z")
	trusted.LastAttempt, trusted.NextAttempt, trusted.Copy, trusted.Skipped = at("2026-11-16T09:00:00Z"), at("2026-11-17T09:00:00Z"), "kept", skipped

	failed := base(config.DirectoryStateTrusted, "")
	*failed = *trusted
	failed.LastAttempt, failed.LastError, failed.NextAttempt = at("2026-11-17T09:00:00Z"), `the account service answered 503 UNAVAILABLE: "try later"`, at("2026-11-17T09:15:00Z")

	expired := base(config.DirectoryStateExpired, "the list's trust time ended at 2026-11-19T09:00:00Z")
	expired.LastGoodPull, expired.Version, expired.TrustedUntil, expired.Skipped = at("2026-11-16T09:00:00Z"), version, at("2026-11-19T09:00:00Z"), skipped
	expired.LastAttempt, expired.LastError, expired.NextAttempt = at("2026-11-19T08:50:00Z"), "the account service did not answer", at("2026-11-19T09:05:00Z")
	expired.Copy, expired.CopyError = "none", "removed: the trust time of its list ended at 2026-11-19T09:00:00Z"

	noCopy := base(config.DirectoryStateNoCopy, "no list yet: no usable kept copy and no good pull")
	noCopy.Copy, noCopy.CopyError = "unused", "/srv/state/directory/users.json: is a symbolic link"

	notEnrolled := base(config.DirectoryStateNotEnrolled, "no enrolment: /srv/state/directory does not exist")
	notEnrolled.ClientID = ""
	notEnrolled.LastAttempt, notEnrolled.LastError = at("2026-11-17T09:00:00Z"), "not enrolled: no enrolment"

	unusable := base(config.DirectoryStateEnrolmentUnusable, "/srv/state/directory/enrolment.json: group or others may use it")
	unusable.ClientID = ""

	out := map[string]config.UsersStatusResponse{}
	for name, d := range map[string]*config.DirectoryStatus{
		"trusted": trusted, "trusted, the last pull failed": failed, "expired": expired,
		"no-copy": noCopy, "not-enrolled": notEnrolled, "enrolment-unusable": unusable,
	} {
		us := []config.UserStatus{local}
		if d.State == config.DirectoryStateTrusted {
			us = listed
		}
		out[name] = config.UsersStatusResponse{Source: config.UsersSourceDirectory, Directory: d, Users: us}
	}
	return out
}

// TestUsersStatusDirectoryStates: on a directory server the route answers
// what the lookup reports, in every state, to an admin only; it adds the
// local-admin key warning, from the config, only when every local admin key
// has expired.
func TestUsersStatusDirectoryStates(t *testing.T) {
	for name, want := range directoryStates() {
		t.Run(name, func(t *testing.T) {
			s, router := usersTestServer(t, newUserFakeBackend())
			s.SetUserLookup(statusLookup{Fake: userLookup(), status: want})

			if r, _ := getStatus(t, router, bobFP); r.code != http.StatusForbidden {
				t.Errorf("bob = %d %s, want 403", r.code, r.body)
			}
			r, got := getStatus(t, router, rootFP)
			if r.code != http.StatusOK {
				t.Fatalf("admin = %d %s", r.code, r.body)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("status = %+v\nwant %+v", got.Directory, want.Directory)
			}
			noKeysIn(t, r.body)
		})
	}
}

// TestUsersStatusExpiredLocalAdmins: the warning that every local admin key
// has expired is the config's, added by the route, naming the local admins by
// name only: no label, key or fingerprint reaches the answer. One key still
// valid and it is absent.
func TestUsersStatusExpiredLocalAdmins(t *testing.T) {
	past, future := "2020-01-01T00:00:00Z", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name  string
		users map[string]config.UserConfig
		want  []string
	}{
		{"every key expired", map[string]config.UserConfig{
			"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: "k1", Label: "desk", Expires: past}, {Key: "k2", Label: "laptop", Expires: past}}},
			"ops":  {Admin: true, Keys: []config.UserKeyConfig{{Key: "k3", Label: "tablet", Expires: past}}},
		}, []string{"ops", "root"}},
		{"one key still valid", map[string]config.UserConfig{
			"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: "k1", Label: "desk", Expires: past}, {Key: "k2", Label: "laptop", Expires: future}}},
		}, nil},
		{"no expiry", map[string]config.UserConfig{"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: "k1", Label: "desk"}}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, router := usersTestServer(t, newUserFakeBackend())
			s.cfg.Auth.Users = tc.users
			s.SetUserLookup(statusLookup{Fake: userLookup(), status: directoryStates()["no-copy"]})
			r, got := getStatus(t, router, rootFP)
			if r.code != http.StatusOK {
				t.Fatalf("admin = %d %s", r.code, r.body)
			}
			if !reflect.DeepEqual(got.Directory.ExpiredLocalAdmins, tc.want) {
				t.Errorf("expired local admins = %q, want %q", got.Directory.ExpiredLocalAdmins, tc.want)
			}
			for _, secret := range []string{"desk", "laptop", "tablet", "k1", "k2", "k3"} {
				if strings.Contains(string(r.body), secret) {
					t.Errorf("the status holds a local admin's label or key (%q): %s", secret, r.body)
				}
			}
		})
	}
}

// TestUsersStatusFillsTheLists: a reporter that leaves its lists nil is
// still answered with empty lists, never null: users, and the directory's
// skipped entries.
func TestUsersStatusFillsTheLists(t *testing.T) {
	s, router := usersTestServer(t, newUserFakeBackend())
	d := *directoryStates()["no-copy"].Directory
	d.Skipped = nil
	s.SetUserLookup(statusLookup{Fake: userLookup(), status: config.UsersStatusResponse{Source: config.UsersSourceDirectory, Directory: &d}})
	r, got := getStatus(t, router, rootFP)
	if r.code != http.StatusOK {
		t.Fatalf("admin = %d %s", r.code, r.body)
	}
	if !strings.Contains(string(r.body), `"skipped":[]`) || !strings.Contains(string(r.body), `"users":[]`) || strings.Contains(string(r.body), "null") {
		t.Errorf("status = %s, want empty lists, not null", r.body)
	}
	if got.Users == nil || got.Directory == nil || got.Directory.Skipped == nil {
		t.Errorf("decoded %+v", got)
	}
}

// TestUsersStatusAsksTheReporterFirst: a lookup that reports a source state
// is answered from that report, before any check of its users: even one
// with an implicit user (no user list) answers its directory state, not
// source none.
func TestUsersStatusAsksTheReporterFirst(t *testing.T) {
	s, router := usersTestServer(t, newUserFakeBackend())
	want := directoryStates()["not-enrolled"]
	s.SetUserLookup(statusLookup{Fake: userstest.Implicit(rootFP), status: want})
	nameTestKey(t, rootFP, servertls.ImplicitUserValue)
	r, got := getStatus(t, router, rootFP)
	if r.code != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Errorf("status = %d %s, want the reporter's directory state", r.code, r.body)
	}
}

// TestUsersStatusWithTheDirectoryRuntime: the route over the real directory
// lookup and runtime, unenrolled (no state directory), holding a list: the
// local admin is answered with source directory, the state and its reason,
// the users with where each comes from, and the entry skipped for a local
// admin's name, quoted nowhere and with no key; the listed user who is not
// an admin is refused.
func TestUsersStatusWithTheDirectoryRuntime(t *testing.T) {
	rootLine, rootFP := directoryTestKey(t)
	adaLine, adaFP := directoryTestKey(t)
	shadowLine, _ := directoryTestKey(t)
	entry := func(name string, admin bool, line string) string {
		return `{"name":` + strconv.Quote(name) + `,"admin":` + strconv.FormatBool(admin) +
			`,"keys":[{"key":` + strconv.Quote(line) + `,"label":"laptop"}]}`
	}
	body := `{"contract":1,"organization":"example","version":"` + strings.Repeat("ab", 32) + `","users":[` +
		entry("ada", false, adaLine) + "," + entry("root", true, shadowLine) + `]}`
	list, err := directory.DecodeList([]byte(body), "example")
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := directory.NewLookup(directory.LookupOptions{
		LocalAdmins: map[string]config.UserConfig{"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: rootLine, Label: "desk"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := lookup.Install(list, time.Now(), 24*time.Hour, 1); err != nil || !ok {
		t.Fatalf("Install = %v, %v", ok, err)
	}
	stateDir := filepath.Join(t.TempDir(), "directory")
	rt, err := directory.NewRuntime(directory.RuntimeOptions{
		Lookup: lookup, StateDir: stateDir, Issuer: "https://accounts.example.test", Refresh: time.Hour, Trust: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.Load()

	s, router := usersTestServer(t, newUserFakeBackend())
	s.cfg.Auth.Users = map[string]config.UserConfig{"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: rootLine, Label: "desk"}}}}
	s.SetUserLookup(rt)

	if r := do(router, boundLeaf(adaFP, "ada"), http.MethodGet, "/api/users/status", ""); r.code != http.StatusForbidden {
		t.Errorf("ada (listed, not an admin) = %d %s, want 403", r.code, r.body)
	}
	r := do(router, boundLeaf(rootFP, "root"), http.MethodGet, "/api/users/status", "")
	if r.code != http.StatusOK {
		t.Fatalf("the local admin = %d %s", r.code, r.body)
	}
	var st config.UsersStatusResponse
	if err := json.Unmarshal(r.body, &st); err != nil {
		t.Fatal(err)
	}
	d := st.Directory
	if st.Source != config.UsersSourceDirectory || d == nil || d.State != config.DirectoryStateNotEnrolled ||
		!strings.Contains(d.Reason, stateDir+" does not exist") || d.URL != "https://accounts.example.test" || d.ClientID != "" || d.Copy != "none" {
		t.Fatalf("status = %s", r.body)
	}
	wantUsers := []config.UserStatus{{Name: "ada", Source: config.UserFromDirectory}, {Name: "root", Admin: true, Source: config.UserFromLocal}}
	if !reflect.DeepEqual(st.Users, wantUsers) {
		t.Errorf("users = %+v, want %+v", st.Users, wantUsers)
	}
	if len(d.Skipped) != 1 || d.Skipped[0].Name != "root" || d.Skipped[0].Reason != string(directory.SkipLocalAdminName) || d.Skipped[0].Description == "" {
		t.Errorf("skipped = %+v, want root for a local admin's name", d.Skipped)
	}
	noKeysIn(t, r.body, rootLine, adaLine, shadowLine, rootFP, adaFP, "laptop", "desk")
}

// TestUsersRefreshDirectoryRow: a directory server's refresh answers its
// pull's row, good or failed, with 200 above any login rows; a pull that
// outlives the request is a 504 that names the directory pull.
func TestUsersRefreshDirectoryRow(t *testing.T) {
	s, router := usersTestServer(t, newUserFakeBackend())
	version := strings.Repeat("cd", 32)
	for _, tc := range []struct {
		name string
		row  users.DirectoryRefresh
		want config.DirectoryRefreshRow
	}{
		{"good", users.DirectoryRefresh{Status: users.DirectoryRefreshGood, List: users.DirectoryListInForce, Version: version, Users: 3, Skipped: 1},
			config.DirectoryRefreshRow{Status: config.DirectoryPullGood, List: config.DirectoryListInForce, Version: version, Users: 3, Skipped: 1}},
		{"failed, a list held", users.DirectoryRefresh{Status: users.DirectoryRefreshFailed, List: users.DirectoryListInForce, Version: version, Users: 3, Error: `the account service answered 403 SERVER_DISABLED: "disabled"`},
			config.DirectoryRefreshRow{Status: config.DirectoryPullFailed, List: config.DirectoryListInForce, Version: version, Users: 3, Error: `the account service answered 403 SERVER_DISABLED: "disabled"`}},
		{"failed, no list", users.DirectoryRefresh{Status: users.DirectoryRefreshFailed, List: users.DirectoryListNone, Error: "not enrolled: no enrolment"},
			config.DirectoryRefreshRow{Status: config.DirectoryPullFailed, List: config.DirectoryListNone, Error: "not enrolled: no enrolment"}},
		{"failed, the trust time ended", users.DirectoryRefresh{Status: users.DirectoryRefreshFailed, List: users.DirectoryListTrustEnded, Error: "the account service did not answer"},
			config.DirectoryRefreshRow{Status: config.DirectoryPullFailed, List: config.DirectoryListTrustEnded, Error: "the account service did not answer"}},
		{"failed, withdrawn", users.DirectoryRefresh{Status: users.DirectoryRefreshFailed, List: users.DirectoryListWithdrawn, Error: "not enrolled: no enrolment"},
			config.DirectoryRefreshRow{Status: config.DirectoryPullFailed, List: config.DirectoryListWithdrawn, Error: "not enrolled: no enrolment"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.row
			s.SetKeyRefresher(statusRefresher{&fakeRefresher{report: users.RefreshReport{Logins: []users.LoginRefresh{}, Directory: &row}}})
			r := doAs(router, rootFP, http.MethodPost, "/api/users/refresh", "")
			if r.code != http.StatusOK {
				t.Fatalf("refresh = %d %s, want 200", r.code, r.body)
			}
			var resp config.UsersRefreshResponse
			if err := json.Unmarshal(r.body, &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Directory == nil || *resp.Directory != tc.want || resp.Logins == nil || len(resp.Logins) != 0 {
				t.Errorf("refresh = %s, want the row %+v and no logins", r.body, tc.want)
			}
		})
	}

	s.SetKeyRefresher(statusRefresher{&fakeRefresher{err: context.DeadlineExceeded}})
	r := doAs(router, rootFP, http.MethodPost, "/api/users/refresh", "")
	if r.code != http.StatusGatewayTimeout || !strings.Contains(r.errMessage(t), "the directory pull is still running") || strings.Contains(r.errMessage(t), "GitHub") {
		t.Errorf("a directory pull past the deadline = %d %s, want 504 naming the directory pull", r.code, r.body)
	}
	s.SetKeyRefresher(&fakeRefresher{err: context.DeadlineExceeded})
	r = doAs(router, rootFP, http.MethodPost, "/api/users/refresh", "")
	if r.code != http.StatusGatewayTimeout || !strings.Contains(r.errMessage(t), "the GitHub fetch is still running") {
		t.Errorf("a GitHub fetch past the deadline = %d %s, want 504 naming the GitHub fetch", r.code, r.body)
	}
	if r := doAs(router, bobFP, http.MethodPost, "/api/users/refresh", ""); r.code != http.StatusForbidden {
		t.Errorf("bob's refresh = %d, want 403", r.code)
	}
}
