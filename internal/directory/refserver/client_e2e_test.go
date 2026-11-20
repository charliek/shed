package refserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/directory"
)

// The one place shed's client of the contract (internal/directory) and the
// reference server meet in this package: the client end to end against the
// server, in-process, over real loopback HTTP. Neither imports the other
// outside tests.

// tokenArrival is one request that reached the token endpoint: the client id
// and jti its assertion named, every scope parameter it carried, and the
// status it was answered with.
type tokenArrival struct {
	clientID, jti string
	scope         []string
	status        int
}

// arrivals records every request to the token endpoint before the server
// sees it.
type arrivals struct {
	mu   sync.Mutex
	list []tokenArrival
}

func (a *arrivals) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathToken {
			next.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxTokenBody+1))
		r.Body = io.NopCloser(bytes.NewReader(body))
		var arr tokenArrival
		if form, err := url.ParseQuery(string(body)); err == nil {
			arr.scope = form["scope"]
			if parts := strings.Split(form.Get("client_assertion"), "."); len(parts) == 3 {
				claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
				var c struct {
					Iss string `json:"iss"`
					Jti string `json:"jti"`
				}
				_ = json.Unmarshal(claims, &c)
				arr.clientID, arr.jti = c.Iss, c.Jti
			}
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		arr.status = rec.status
		a.mu.Lock()
		a.list = append(a.list, arr)
		a.mu.Unlock()
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// failure is the *directory.Failure err carries.
func failure(t *testing.T, what string, err error) *directory.Failure {
	t.Helper()
	var f *directory.Failure
	if !errors.As(err, &f) {
		t.Fatalf("%s: %v, want a *directory.Failure", what, err)
	}
	return f
}

// TestClientEndToEnd runs shed's contract client against the reference
// server over loopback HTTP: enrolment with a code, a token, a pull that
// decodes the served users; a user removed and a pull with a changed
// version; an unchanged pull with the same version; the server disabled; the
// outage; a second enrolment with the spent code refused; and, across all of
// it, the server's replay ledger holding every accepted assertion once, no
// jti having arrived twice, and every token request asking for the scope
// directory:read, once.
func TestClientEndToEnd(t *testing.T) {
	var seen arrivals
	c := newControlServerWith(t, seen.wrap)
	ada, bob := newSSHKey(t), newSSHKey(t)
	both := `[{"name":"ada","admin":true,"keys":[{"key":"` + ada + `","label":"laptop"}]},` +
		`{"name":"bob","admin":false,"keys":[{"key":"` + bob + `","label":"desk"}]}]`
	c.write("users.json", both)
	c.write("codes", "build-1 code-one\n")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	issuer := c.srv.Issuer()
	enrol := func() (*directory.Enrolment, error) {
		return directory.Enroll(ctx, directory.EnrolRequest{
			Issuer: issuer, Code: "code-one", Name: "build-1.example.test", AllowPlainHTTP: true,
		})
	}
	newClient := func(e *directory.Enrolment) *directory.Client {
		t.Helper()
		cl, err := directory.NewClient(directory.ClientConfig{Enrolment: e, AllowPlainHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		return cl
	}

	// Enrolment with a code.
	e, err := enrol()
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if !directory.ValidClientID(e.ClientID) || e.Issuer != issuer || e.Organization != "example" || e.TokenEndpoint != issuer+pathToken {
		t.Fatalf("the enrolment: %+v", e)
	}
	c.mu.Lock()
	enrolled := strings.Join(c.enrolled, ",")
	c.mu.Unlock()
	if enrolled != "build-1 "+e.ClientID {
		t.Fatalf("enrolled %q", enrolled)
	}

	// A token and a pull that decodes the served users.
	cl := newClient(e)
	p1, err := cl.Pull(ctx)
	if err != nil {
		t.Fatalf("the first pull: %v", err)
	}
	l := p1.List
	if l.Organization != "example" || len(l.Skipped) != 0 || len(l.Users) != 2 {
		t.Fatalf("the first list: %+v", l)
	}
	for i, want := range []struct {
		name  string
		admin bool
		key   string
		label string
	}{{"ada", true, ada, "laptop"}, {"bob", false, bob, "desk"}} {
		u := l.Users[i]
		if u.Name != want.name || u.Admin != want.admin || len(u.Keys) != 1 ||
			u.Keys[0].Canonical != want.key || u.Keys[0].Label != want.label || u.Keys[0].Expires != nil {
			t.Errorf("user %d: %+v, want %s", i, u, want.name)
		}
	}

	// A user removed: the next pull's version changes.
	c.write("users.json", `[{"name":"ada","admin":true,"keys":[{"key":"`+ada+`","label":"laptop"}]}]`)
	p2, err := cl.Pull(ctx)
	if err != nil {
		t.Fatalf("the pull after the removal: %v", err)
	}
	if p2.List.Version == p1.List.Version || len(p2.List.Users) != 1 || p2.List.Users[0].Name != "ada" {
		t.Fatalf("after the removal: version %s (was %s), users %+v", p2.List.Version, p1.List.Version, p2.List.Users)
	}

	// Unchanged: the same version, from this client and from a fresh one
	// that logs in again.
	p3, err := cl.Pull(ctx)
	if err != nil || p3.List.Version != p2.List.Version {
		t.Fatalf("an unchanged pull: %v, version %v, want %s", err, p3, p2.List.Version)
	}
	if p, err := newClient(e).Pull(ctx); err != nil || p.List.Version != p2.List.Version {
		t.Fatalf("an unchanged pull with a new login: %v %v", p, err)
	}

	// The server disabled: the client holding a token is refused by the
	// directory as a disabled server; a new login is refused.
	c.write("disabled", e.ClientID+"\n")
	_, err = cl.Pull(ctx)
	if f := failure(t, "disabled", err); f.Kind != directory.KindServerDisabled || f.Status != http.StatusForbidden || f.Step != directory.StepDirectory {
		t.Fatalf("disabled: %v, want server-disabled", f)
	}
	_, err = newClient(e).Pull(ctx)
	if f := failure(t, "disabled, a new login", err); f.Kind != directory.KindInvalidClient || f.Step != directory.StepToken {
		t.Fatalf("disabled, a new login: %v, want invalid-client", f)
	}
	if err := os.Remove(filepath.Join(c.dir, "disabled")); err != nil {
		t.Fatal(err)
	}

	// The outage: a failed refresh, unavailable, at the directory and at
	// the token endpoint.
	c.write("outage", "")
	_, err = cl.Pull(ctx)
	if f := failure(t, "outage", err); f.Kind != directory.KindUnavailable || f.Status != http.StatusServiceUnavailable || f.Step != directory.StepDirectory {
		t.Fatalf("outage: %v, want unavailable", f)
	}
	_, err = newClient(e).Pull(ctx)
	if f := failure(t, "outage, a new login", err); f.Kind != directory.KindUnavailable || f.Step != directory.StepToken {
		t.Fatalf("outage, a new login: %v, want unavailable", f)
	}
	if err := os.Remove(filepath.Join(c.dir, "outage")); err != nil {
		t.Fatal(err)
	}
	if p, err := cl.Pull(ctx); err != nil || p.List.Version != p2.List.Version {
		t.Fatalf("after the outage: %v %v", p, err)
	}

	// A second enrolment with the spent code is refused, and nothing is
	// enrolled.
	_, err = enrol()
	f := failure(t, "the spent code", err)
	if f.Kind != directory.KindRefused || f.Status != http.StatusConflict || f.Code != "ENROLMENT_CODE_SPENT" || f.Lost() {
		t.Fatalf("the spent code: %v, want 409 ENROLMENT_CODE_SPENT", f)
	}
	c.mu.Lock()
	if len(c.enrolled) != 1 {
		t.Errorf("enrolled %v after the refused enrolment", c.enrolled)
	}
	c.mu.Unlock()

	// No jti arrived twice, and the ledger holds exactly the accepted ones.
	seen.mu.Lock()
	list := append([]tokenArrival(nil), seen.list...)
	seen.mu.Unlock()
	jtis := map[string]bool{}
	accepted := 0
	for _, a := range list {
		if a.clientID != e.ClientID || a.jti == "" {
			t.Errorf("an assertion naming %q with jti %q", a.clientID, a.jti)
		}
		if jtis[a.jti] {
			t.Errorf("the jti %s arrived twice", a.jti)
		}
		// Every login asks for directory:read, once, and nothing else.
		if len(a.scope) != 1 || a.scope[0] != scopeDirectory {
			t.Errorf("a token request with jti %s carried scope %q, want [%s]", a.jti, a.scope, scopeDirectory)
		}
		jtis[a.jti] = true
		if a.status == http.StatusOK {
			accepted++
		}
	}
	// Logins: the first client, the fresh one, the disabled one, the
	// outage one.
	if len(list) != 4 || accepted != 2 {
		t.Fatalf("%d token requests, %d accepted; want 4 and 2: %+v", len(list), accepted, list)
	}
	l2 := &c.srv.ledger
	l2.mu.Lock()
	defer l2.mu.Unlock()
	if len(l2.spent) != accepted {
		t.Errorf("the ledger holds %d entries, want %d", len(l2.spent), accepted)
	}
	for _, a := range list {
		_, spent := l2.spent[ledgerKey{a.clientID, a.jti}]
		if spent != (a.status == http.StatusOK) {
			t.Errorf("jti %s answered %d is spent=%v in the ledger", a.jti, a.status, spent)
		}
	}
}
