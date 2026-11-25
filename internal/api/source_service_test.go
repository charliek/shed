package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/plugin"
)

// The source routes for a directory user (plan 040, section 3.2), judged by
// the target user: on a directory server whose one local admin is root,
// alice and bob are the account service's. Every route that would read,
// bind, move, clear, stream to or answer from a machine for them answers the
// service's document or 409 SOURCE_IS_SERVICE, for themselves and for an
// admin naming them; root is answered as before.

// fakeHosted is an account service that is or is not delivering.
type fakeHosted struct{ notDelivering bool }

func (fakeHosted) Deliver(context.Context, []byte) (delivery.HostedStatus, []byte) {
	return delivery.HostedNotApplicable, nil
}
func (f fakeHosted) NotDelivering() bool { return f.notDelivering }

// directoryServer is sourcesServer as a directory server whose one local
// admin is root, with a delivery service over its registry.
func directoryServer(t *testing.T, notDelivering bool) (*Server, http.Handler) {
	t.Helper()
	s, h, _ := sourcesServer(t)
	s.cfg.Auth.Users = map[string]config.UserConfig{"root": {Admin: true}}
	s.cfg.Auth.Directory = &config.DirectoryConfig{URL: "https://accounts.example.test/"}
	reg := s.plugins
	reg.SetServiceUsers(func(u string) bool { return s.cfg.UserOrigin(u) == delivery.OriginDirectory })
	svc := delivery.NewService(reg, time.Second)
	svc.SetOrigin(s.cfg.UserOrigin)
	svc.SetHosted(fakeHosted{notDelivering: notDelivering})
	s.SetDelivery(svc)
	return s, h
}

func TestDirectoryUserSourceRoutes(t *testing.T) {
	_, h := directoryServer(t, false)

	// The document: the user's own, and an admin's ?user= naming them. It
	// is the shape crates/fixtures/sources/service.json pins, for alice.
	want := canonical(t, []byte(`{"user":"alice","binding":{"delivery":[{"kind":"service","url":"https://accounts.example.test"}],"signing":[]},"machines":[]}`))
	fixture := canonical(t, readShape(t, "service.json"))
	if want != strings.Replace(fixture, `"user":"ada"`, `"user":"alice"`, 1) {
		t.Fatalf("set-up: the expected document %s is not the fixture's %s", want, fixture)
	}
	for _, tc := range []struct{ fp, query string }{{aliceFP, ""}, {rootFP, "?user=alice"}} {
		r := getSources(h, tc.fp, tc.query)
		if r.code != http.StatusOK || canonical(t, []byte(r.body)) != want {
			t.Fatalf("GET /api/sources%s = %d %s", tc.query, r.code, r.body)
		}
	}
	// The local admin's own document is a machine one.
	if r := getSources(h, rootFP, ""); r.code != http.StatusOK || canonical(t, []byte(r.body)) == canonical(t, readShape(t, "service.json")) {
		t.Fatalf("root's own document = %d %s", r.code, r.body)
	}

	isService := func(what string, r apiResp) {
		t.Helper()
		if r.code != http.StatusConflict || canonical(t, []byte(r.body)) != canonical(t, readShape(t, "source-is-service.json")) {
			t.Fatalf("%s = %d %s, want the 409 SOURCE_IS_SERVICE fixture", what, r.code, r.body)
		}
	}
	machine := testMachineFor("alice-laptop").ID
	isService("use (own)", useSource(h, aliceFP, `{"machine_id":"`+machine+`"}`))
	isService("use (admin naming alice)", useSource(h, rootFP, `{"machine_id":"`+machine+`","user":"alice"}`))
	isService("unbind (own)", unbindSource(h, aliceFP, "{}"))
	isService("unbind (admin naming alice)", unbindSource(h, rootFP, unbindBody("alice")))
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/delivery/requests"},
		{http.MethodPost, "/api/delivery/requests/0123456789abcdef0123456789abcdef"},
		{http.MethodGet, "/api/plugins/listeners"},
		{http.MethodGet, "/api/plugins/listeners/ssh-agent/messages"},
		{http.MethodPost, "/api/plugins/listeners/ssh-agent/respond"},
	} {
		isService(route.method+" "+route.path, do(h, credsAs(aliceFP), route.method, route.path, "{}"))
	}
	// root, a local admin, is not refused by any of them for being one.
	if r := unbindSource(h, rootFP, "{}"); r.code != http.StatusOK {
		t.Fatalf("root's own unbind = %d %s", r.code, r.body)
	}
}

// TestDirectoryUserSourceNotDelivering: while the service is not delivering
// to this server, the document's service entry says so.
func TestDirectoryUserSourceNotDelivering(t *testing.T) {
	_, h := directoryServer(t, true)
	r := getSources(h, aliceFP, "")
	if r.code != http.StatusOK || canonical(t, []byte(r.body)) != canonical(t, []byte(`{"user":"alice","binding":{"delivery":[{"kind":"service","url":"https://accounts.example.test","state":"not_delivering"}],"signing":[]},"machines":[]}`)) {
		t.Fatalf("GET /api/sources = %d %s", r.code, r.body)
	}
}

// TestBusNeverBindsADirectoryUser: the bus itself refuses to admit, bind or
// move to a machine for a directory user, whatever route asks: no binding
// is ever made for them.
func TestBusNeverBindsADirectoryUser(t *testing.T) {
	s, _ := directoryServer(t, false)
	reg := s.plugins
	m := testMachineFor("alice-laptop")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := reg.Subscribe(ctx, plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"}, m); err != plugin.ErrSourceIsService {
		t.Fatalf("Subscribe = %v, want ErrSourceIsService", err)
	}
	if _, err := reg.SubscribeDelivery(ctx, "alice", m, ""); err != plugin.ErrSourceIsService {
		t.Fatalf("SubscribeDelivery = %v, want ErrSourceIsService", err)
	}
	if _, err := reg.Move(ctx, "alice", m.ID, "admin root"); err != plugin.ErrSourceIsService {
		t.Fatalf("Move = %v, want ErrSourceIsService", err)
	}
	if _, ok := reg.Bindings().Get("alice"); ok {
		t.Fatal("a binding was made for a directory user")
	}
	// root's machine is admitted and bound as before.
	if _, err := reg.Subscribe(ctx, plugin.ListenerKey{User: "root", Namespace: "ssh-agent"}, testMachineFor("root-desk")); err != nil {
		t.Fatalf("root's Subscribe = %v", err)
	}
	if _, ok := reg.Bindings().Get("root"); !ok {
		t.Fatal("root's machine was not bound")
	}
}

// TestShedOriginChangedIs409: a delivery refused because the shed's user's
// name changed origin is 409 SHED_ORIGIN_CHANGED with its one fixed message.
func TestShedOriginChangedIs409(t *testing.T) {
	status, code, msg := mapBackendError(fmt.Errorf("provision box: %w", delivery.ErrShedOriginChanged))
	if status != http.StatusConflict || code != config.ErrShedOriginChanged || msg != config.ShedOriginChangedMessage {
		t.Fatalf("mapBackendError = %d %s %q", status, code, msg)
	}
	if status, code, msg := mapBackendError(fmt.Errorf("provision box: %w", delivery.ErrShedOriginUnknown)); status != http.StatusConflict ||
		code != config.ErrShedOriginUnknown || msg != config.ShedOriginUnknownMessage {
		t.Fatalf("unknown origin = %d %s %q", status, code, msg)
	}
	// A run its shed's stop or delete ended is a 409, not a 500.
	if status, code, _ := mapBackendError(fmt.Errorf("provision box: %w", delivery.ErrShedEnded)); status != http.StatusConflict || code != config.ErrShedAlreadyStopped {
		t.Fatalf("an ended run = %d %s", status, code)
	}
}

// TestDirectoryTargetJudgedStatically: the delivery and listener routes
// judge the user an admin names (?user=), and the source routes classify a
// named user by the static rule before the live lookup, so a directory name
// the list does not hold (after the trust time ends; here carol, whom the
// lookup has never had) still gets the service's document or 409
// SOURCE_IS_SERVICE, never 400.
func TestDirectoryTargetJudgedStatically(t *testing.T) {
	_, h := directoryServer(t, false)
	isService := func(what string, r apiResp) {
		t.Helper()
		if r.code != http.StatusConflict || canonical(t, []byte(r.body)) != canonical(t, readShape(t, "source-is-service.json")) {
			t.Fatalf("%s = %d %s, want 409 SOURCE_IS_SERVICE", what, r.code, r.body)
		}
	}
	for _, named := range []string{"alice", "carol"} {
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/api/delivery/requests?user=" + named},
			{http.MethodPost, "/api/delivery/requests/0123456789abcdef0123456789abcdef?user=" + named},
			{http.MethodGet, "/api/plugins/listeners?user=" + named},
			{http.MethodGet, "/api/plugins/listeners/ssh-agent/messages?user=" + named},
			{http.MethodPost, "/api/plugins/listeners/ssh-agent/respond?user=" + named},
		} {
			isService("root: "+route.method+" "+route.path, do(h, credsAs(rootFP), route.method, route.path, "{}"))
		}
	}
	r := getSources(h, rootFP, "?user=carol")
	if r.code != http.StatusOK || !strings.Contains(string(r.body), `"kind":"service"`) {
		t.Fatalf("GET /api/sources?user=carol = %d %s, want the service's document", r.code, r.body)
	}
	isService("use naming carol", useSource(h, rootFP, `{"machine_id":"`+testMachineFor("x").ID+`","user":"carol"}`))
	isService("unbind naming carol", unbindSource(h, rootFP, unbindBody("carol")))
	// A name that is not a user name at all is still a bad request.
	if r := getSources(h, rootFP, "?user=Not%20A%20Name"); r.code != http.StatusBadRequest {
		t.Fatalf("a malformed name = %d %s", r.code, r.body)
	}
	// root's own listeners are root's, as before.
	if r := do(h, credsAs(rootFP), http.MethodGet, "/api/plugins/listeners", ""); r.code != http.StatusOK {
		t.Fatalf("root's own listeners = %d %s", r.code, r.body)
	}
}
