package directory

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/users"
)

// The scope (contract section 2): a token request may name a scope, and an
// absent one means every scope the service has. The client names
// directory:read and nothing else, so a scope the service adds later is never
// asked for. These tests hold what it sends and what it makes of the
// refusals that concern its scope: 400 invalid_scope from the token
// endpoint, a 200 whose scope lacks directory:read (TestTokenAnswerChecks),
// and 403 INSUFFICIENT_SCOPE from the directory. Each is a failed refresh.

// tokenParamsSent are a token request body's parameters, in the order sent,
// each as name=value (still encoded).
func tokenParamsSent(body string) []string { return strings.Split(body, "&") }

// TestTokenRequestAsksForDirectoryRead: every token request carries scope
// exactly once, equal to directory:read (encoded directory%3Aread), beside
// the three parameters it always sent and nothing else: the first login and
// the new login a directory 401 brings alike.
func TestTokenRequestAsksForDirectoryRead(t *testing.T) {
	notValid := served{status: 401, header: http.Header{"Www-Authenticate": {`Bearer error="invalid_token"`}}, body: []byte(`{"error":{"code":"invalid_token","message":"m"}}`)}
	fs := newFakeService(t)
	fs.directory = []served{notValid, {status: 200, body: worldListBody(t)}}
	if _, err := fs.client(t, worldOrg).Pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	bodies := append([]string(nil), fs.tokenForms...)
	fs.mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d token requests, want 2 (a login, and the new one after the 401)", len(bodies))
	}
	for i, body := range bodies {
		var scopes []string
		for _, p := range tokenParamsSent(body) {
			if strings.HasPrefix(p, "scope=") {
				scopes = append(scopes, p)
			}
		}
		if len(scopes) != 1 || scopes[0] != "scope=directory%3Aread" {
			t.Errorf("token request %d: scope parameters %q, want exactly [scope=directory%%3Aread]", i, scopes)
		}
		form, err := url.ParseQuery(body)
		if err != nil {
			t.Fatalf("token request %d is not a form: %v", i, err)
		}
		if got := form["scope"]; !slices.Equal(got, []string{directoryScope}) {
			t.Errorf("token request %d: scope %q, want [%s]", i, got, directoryScope)
		}
		names := make([]string, 0, len(form))
		for name, vals := range form {
			if len(vals) != 1 {
				t.Errorf("token request %d: %s sent %d times", i, name, len(vals))
			}
			names = append(names, name)
		}
		slices.Sort(names)
		if want := []string{"client_assertion", "client_assertion_type", "grant_type", "scope"}; !slices.Equal(names, want) {
			t.Errorf("token request %d: parameters %v, want %v", i, names, want)
		}
	}
}

// invalidScope is a token endpoint's 400 invalid_scope (section 2: scope
// malformed or naming a scope the service lacks).
var invalidScope = served{
	status: http.StatusBadRequest,
	header: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}},
	body:   []byte(`{"error":"invalid_scope","error_description":"the scope is malformed or names a scope this service lacks"}`),
}

// TestInvalidScopeIsAFailedRefresh: a token request answered 400
// invalid_scope is a failed refresh, refused with its code: the directory is
// not asked, and the request is never sent again, neither without the scope
// nor in any other form. The next pull logs in afresh, with the scope again.
func TestInvalidScopeIsAFailedRefresh(t *testing.T) {
	fs := newFakeService(t)
	fs.tokens = []served{invalidScope}
	fs.directory = []served{{status: 200, body: worldListBody(t)}}
	c := fs.client(t, worldOrg)
	for pull := 1; pull <= 2; pull++ {
		p, err := c.Pull(context.Background())
		if err == nil {
			t.Fatalf("pull %d: accepted %+v after invalid_scope", pull, p)
		}
		f := asFailure(t, err)
		if f.Step != StepToken || f.Kind != KindRefused || f.Status != http.StatusBadRequest || f.Code != "invalid_scope" {
			t.Errorf("pull %d: %v, want the token step refused, 400 invalid_scope", pull, f)
		}
		if n := fs.count("POST /oauth2/token"); n != pull {
			t.Errorf("pull %d: %d token requests in all, want %d (one per pull, none sent again)", pull, n, pull)
		}
		if n := fs.count("GET /v1/directory"); n != 0 {
			t.Errorf("pull %d: the directory was asked %d times", pull, n)
		}
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for i, body := range fs.tokenForms {
		if !slices.Contains(tokenParamsSent(body), "scope=directory%3Aread") {
			t.Errorf("token request %d did not name the scope: %s", i, body)
		}
	}
}

// TestInsufficientScopeIsAFailedRefresh: a directory answer 403
// INSUFFICIENT_SCOPE (the token lacks directory:read) is a failed refresh,
// refused with its code. It brings no new login (only a 401 does, section
// 3: a new login would ask for the same scope) and no second directory
// request.
func TestInsufficientScopeIsAFailedRefresh(t *testing.T) {
	fs := newFakeService(t)
	fs.directory = []served{{
		status: http.StatusForbidden,
		header: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}},
		body:   []byte(`{"error":{"code":"INSUFFICIENT_SCOPE","message":"the token lacks directory:read"}}`),
	}}
	_, err := fs.client(t, worldOrg).Pull(context.Background())
	if err == nil {
		t.Fatal("a pull answered 403 INSUFFICIENT_SCOPE was accepted")
	}
	f := asFailure(t, err)
	if f.Step != StepDirectory || f.Kind != KindRefused || f.Status != http.StatusForbidden || f.Code != "INSUFFICIENT_SCOPE" {
		t.Errorf("%v, want the directory step refused, 403 INSUFFICIENT_SCOPE", f)
	}
	if tk, d := fs.count("POST /oauth2/token"), fs.count("GET /v1/directory"); tk != 1 || d != 1 {
		t.Errorf("%d logins and %d directory requests, want 1 and 1", tk, d)
	}
}

// TestRuntimeScopeRefusedIsAFailedRefresh: against a service that refuses a
// token request naming a scope (one that breaks the contract, which allows
// it), every refresh fails, as the contract's other refusals do: the refresh
// row says failed with the code, the kept list stays in force with its
// users, the line is logged once for the change of state, and the server
// never falls back to a request with no scope (the reference server behind
// the fault would have answered that one). A good pull follows once the
// service accepts the scope.
func TestRuntimeScopeRefusedIsAFailedRefresh(t *testing.T) {
	_, svc, dir, opsLine, _ := rtSetup(t)
	first := startRT(t, svc, dir, opsLine, nil)
	good := first.refresh(t)
	if good.Status != users.DirectoryRefreshGood || good.List != users.DirectoryListInForce || good.Users != 1 {
		t.Fatalf("the first pull: %+v", good)
	}
	first.stop()

	// A new start loads the kept copy and holds no access token, so its
	// refresh logs in.
	svc.setMode(faultScopeRefused, "")
	srv := startRT(t, svc, dir, opsLine, nil)
	tokens, directory := svc.tokenCount(), svc.directoryCount()
	const line = `directory: pull failed: token: refused (HTTP 400) "invalid_scope"`
	for i := 1; i <= 2; i++ {
		row := srv.refresh(t)
		if row.Status != users.DirectoryRefreshFailed || !strings.Contains(row.Error, `"invalid_scope"`) {
			t.Fatalf("refresh %d: %+v, want failed with invalid_scope", i, row)
		}
		if row.List != users.DirectoryListInForce || row.Users != 1 || row.Version != good.Version {
			t.Errorf("refresh %d: %+v, want the kept list (version %s, 1 user) still in force", i, row, good.Version)
		}
		if n := svc.tokenCount() - tokens; n != i {
			t.Errorf("refresh %d: %d token requests, want %d (one per refresh)", i, n, i)
		}
	}
	if n := svc.directoryCount() - directory; n != 0 {
		t.Errorf("the directory was asked %d times with no token", n)
	}
	if n := srv.log.count(line); n != 1 {
		t.Errorf("%q logged %d times over two refreshes, want 1:\n%s", line, n, srv.log)
	}
	if !strings.Contains(srv.log.String(), "; the list held stays; next attempt at ") {
		t.Errorf("the failed refresh's line does not say the list held stays:\n%s", srv.log)
	}

	svc.setMode(faultNone, "")
	if row := srv.refresh(t); row.Status != users.DirectoryRefreshGood || row.Users != 1 {
		t.Fatalf("once the service accepts the scope: %+v", row)
	}
}
