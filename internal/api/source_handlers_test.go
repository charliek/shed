package api

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/source"
	"github.com/charliek/shed/internal/source/fake"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// The credential source routes, GET /api/sources, POST /api/sources/use and
// POST /api/sources/unbind,
// driven with the users of userLookup (alice and bob, and root, an admin),
// plus two more keys of alice's: a labelled one and a script key.

var (
	aliceLaptopFP = testFingerprint("alice laptop")
	aliceScriptFP = testFingerprint("alice script")
	rootScriptFP  = testFingerprint("root script")
)

// sourcesServer is busUsersServer with alice's extra keys, a script key of
// root's (the admin), and the service client testClientName configured, and
// its bindings.
func sourcesServer(t *testing.T) (*Server, http.Handler, *fake.Bindings) {
	t.Helper()
	s := busUsersServer(t)
	s.cfg.Auth.Clients = map[string]config.ServiceClientConfig{testClientName: {Policy: defaultTestPolicy()}}
	f := userLookup()
	f.AddKey("alice", users.Key{Fingerprint: aliceLaptopFP, Source: users.SourceAdHoc, Label: "laptop"})
	f.AddKey("alice", users.Key{Fingerprint: aliceScriptFP, Source: users.SourceAdHoc, Label: "ci", Script: true})
	f.AddKey("root", users.Key{Fingerprint: rootScriptFP, Source: users.SourceAdHoc, Label: "cron", Script: true})
	s.SetUserLookup(f)
	b := pinBus(s)
	return s, s.Router(), b
}

// getSources is GET /api/sources as fp (query appended verbatim).
func getSources(h http.Handler, fp, query string) apiResp {
	return doAs(h, fp, http.MethodGet, "/api/sources"+query, "")
}

// useSource is POST /api/sources/use as fp with body.
func useSource(h http.Handler, fp, body string) apiResp {
	return doAs(h, fp, http.MethodPost, "/api/sources/use", body)
}

// unbindSource is POST /api/sources/unbind as fp with body.
func unbindSource(h http.Handler, fp, body string) apiResp {
	return doAs(h, fp, http.MethodPost, "/api/sources/unbind", body)
}

// unbindBody is an unbind body, for user when named.
func unbindBody(user string) string {
	if user == "" {
		return "{}"
	}
	return fmt.Sprintf(`{"user":%q}`, user)
}

// unbound decodes an unbind's answer; it fails the test on anything but 200.
func unbound(t *testing.T, what string, r apiResp) config.SourceUnbindResponse {
	t.Helper()
	if r.code != http.StatusOK {
		t.Fatalf("%s = %d %s, want 200", what, r.code, r.body)
	}
	var resp config.SourceUnbindResponse
	if err := json.Unmarshal(r.body, &resp); err != nil {
		t.Fatalf("%s: %v: %s", what, err, r.body)
	}
	return resp
}

// useBody is a move body to id, for user when named.
func useBody(id, user string) string {
	if user == "" {
		return fmt.Sprintf(`{"machine_id":%q}`, id)
	}
	return fmt.Sprintf(`{"machine_id":%q,"user":%q}`, id, user)
}

// sources decodes an answer of either route; it fails the test on anything
// but 200.
func sources(t *testing.T, what string, r apiResp) config.SourcesResponse {
	t.Helper()
	if r.code != http.StatusOK {
		t.Fatalf("%s = %d %s, want 200", what, r.code, r.body)
	}
	var resp config.SourcesResponse
	if err := json.Unmarshal(r.body, &resp); err != nil {
		t.Fatalf("%s: %v: %s", what, err, r.body)
	}
	return resp
}

// machineRow is the row of id in resp (fails without one).
func machineRow(t *testing.T, resp config.SourcesResponse, id string) config.SourceMachine {
	t.Helper()
	for _, m := range resp.Machines {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("no row for machine %s in %+v", id, resp.Machines)
	return config.SourceMachine{}
}

// present makes machine m present itself for user (a subscribe to a namespace
// it is refused on), as a machine that is not the source does.
func present(t *testing.T, s *Server, user string, m source.Machine) {
	t.Helper()
	_, err := s.plugins.Subscribe(context.Background(), plugin.ListenerKey{User: user, Namespace: "ssh-agent"}, m)
	var nb *plugin.NotBoundError
	if !errors.As(err, &nb) {
		t.Fatalf("presenting %s for %s: %v, want a refusal (not the source)", m.ID, user, err)
	}
}

// subscribeAs registers machine m as user's listener for namespace.
func subscribeAs(t *testing.T, s *Server, user, namespace string, m source.Machine) *plugin.Listener {
	t.Helper()
	l, err := s.plugins.Subscribe(context.Background(), plugin.ListenerKey{User: user, Namespace: namespace}, m)
	if err != nil {
		t.Fatalf("subscribing %s for %s to %s: %v", m.ID, user, namespace, err)
	}
	return l
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

var (
	aliceA = testMachineFor(aliceFP)
	aliceB = source.Machine{ID: strings.Repeat("b1", 16), Name: "alice\u2019s laptop"}
)

// TestSourcesEmpty: a server where nobody has connected answers the caller's
// user with a null binding and no machines.
func TestSourcesEmpty(t *testing.T) {
	_, router, _ := sourcesServer(t)
	r := getSources(router, aliceFP, "")
	if !strings.Contains(string(r.body), `"binding":null`) || !strings.Contains(string(r.body), `"machines":[]`) {
		t.Fatalf("empty answer = %d %s, want a null binding and an empty list", r.code, r.body)
	}
	if resp := sources(t, "empty", r); resp.User != "alice" || resp.Binding != nil || len(resp.Machines) != 0 {
		t.Fatalf("empty answer = %+v", resp)
	}
}

// TestSourcesListAfterSubscribe: the bound machine is listed bound and
// connected with the request types it holds; a refused machine is listed,
// not bound and not connected.
func TestSourcesListAfterSubscribe(t *testing.T) {
	s, router, _ := sourcesServer(t)
	subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	subscribeAs(t, s, "alice", "docker-credential", aliceA)
	present(t, s, "alice", aliceB)

	resp := sources(t, "list", getSources(router, aliceFP, ""))
	if resp.User != "alice" || resp.Binding.MachineID() != aliceA.ID || resp.Binding.BoundBy != source.BoundByFirstConnect {
		t.Fatalf("binding = %+v, want alice's first machine, bound at first connect", resp.Binding)
	}
	if d := resp.Binding.Delivery; len(d) != 1 || d[0] != (config.SourceRef{Kind: source.KindMachine, ID: aliceA.ID}) {
		t.Fatalf("delivery = %+v", d)
	}
	if len(resp.Machines) != 2 {
		t.Fatalf("machines = %+v, want 2", resp.Machines)
	}
	a := machineRow(t, resp, aliceA.ID)
	if !a.Connected || strings.Join(a.Namespaces, ",") != "docker-credential,ssh-agent" || a.Name != aliceA.Name || a.KeyFingerprint != aliceA.KeyFingerprint || a.FirstSeen.IsZero() {
		t.Fatalf("bound machine row = %+v", a)
	}
	b := machineRow(t, resp, aliceB.ID)
	if b.Connected || len(b.Namespaces) != 0 || b.Name != aliceB.Name || b.ID == resp.Binding.MachineID() {
		t.Fatalf("refused machine row = %+v", b)
	}
	if b.Namespaces == nil {
		t.Fatal("a machine with no subscription has a null namespace list, want []")
	}
}

// TestSourcesUseMovesTheBinding: a move to another machine of the user's
// answers the new binding, ends the old machine's live stream at once and
// drops the request sent to it: the old machine's answer is refused naming
// the new one, and the new machine is never sent it nor may answer it. A
// move to the machine already bound changes nothing.
func TestSourcesUseMovesTheBinding(t *testing.T) {
	s, router, b := sourcesServer(t)
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: (&sendCounter{}).send})
	ts := sseServer(t, s, map[string]*x509.Certificate{"alice": credsAs(aliceFP)})
	stream := openStream(t, ts, "alice", subPath)
	waitListener(t, s, plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"})
	id := publishFromShed(t, s, "box", gen)
	if got := nextData(t, "alice", stream); !strings.Contains(got, id) {
		t.Fatalf("alice's machine got %s, want %s", got, id)
	}
	present(t, s, "alice", aliceB)
	before := mustBinding(t, s, "alice")

	// The move, from alice's labelled key.
	resp := sources(t, "move", useSource(router, aliceLaptopFP, useBody(aliceB.ID, "")))
	if resp.Binding.MachineID() != aliceB.ID || resp.Binding.Generation <= before.Generation {
		t.Fatalf("binding after the move = %+v, want machine B at a later generation", resp.Binding)
	}
	if want := fmt.Sprintf(`user alice with key "laptop" (%s)`, aliceLaptopFP); resp.Binding.BoundBy != want {
		t.Fatalf("bound_by = %q, want %q", resp.Binding.BoundBy, want)
	}
	if a := machineRow(t, resp, aliceA.ID); a.Connected {
		t.Fatalf("the old machine is still listed connected: %+v", a)
	}

	// The old machine's stream ends.
	deadline := time.After(3 * time.Second)
	for ended := false; !ended; {
		select {
		case _, ok := <-stream:
			ended = !ok
		case <-deadline:
			t.Fatal("the old machine's stream did not end after the move")
		}
	}
	// Its answer to the request it was sent is refused, naming B.
	r := do(router, credsAs(aliceFP), http.MethodPost, respondPath, respondBody("box", id))
	assertRefusal(t, "the old machine's answer", r, http.StatusForbidden, config.ErrSourceNotBound)
	if got, _ := refusalSource(t, r); got != aliceB.ID {
		t.Fatalf("refusal names %s, want B", got)
	}
	// The request was dropped: B is not sent it and may not answer it.
	lb := subscribeAs(t, s, "alice", "ssh-agent", aliceB)
	select {
	case m := <-lb.Messages:
		if env := lb.Take(m); env != nil {
			t.Fatalf("the new machine was sent the old machine's request: %+v", env)
		}
	case <-time.After(150 * time.Millisecond):
	}
	r = do(router, credsAs(aliceFP), http.MethodPost, respondPath, respondBody("box", id), HeaderMachineID, aliceB.ID, HeaderMachineName, url.PathEscape(aliceB.Name))
	if r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden {
		t.Fatalf("the new machine answering the dropped request = %d %s, want 403 FORBIDDEN (no such request)", r.code, r.body)
	}

	// Moving to the machine already bound changes nothing.
	writes := b.Writes()
	again := sources(t, "move to the bound machine", useSource(router, aliceFP, useBody(aliceB.ID, "")))
	if again.Binding.Generation != resp.Binding.Generation || again.Binding.BoundBy != resp.Binding.BoundBy || b.Writes() != writes {
		t.Fatalf("a move to the bound machine changed something: %+v (writes %d -> %d)", again.Binding, writes, b.Writes())
	}
	if closed(lb.Done) {
		t.Fatal("a move to the bound machine closed its stream")
	}
	if m := machineRow(t, again, aliceB.ID); !m.Connected || strings.Join(m.Namespaces, ",") != "ssh-agent" {
		t.Fatalf("bound machine after the no-op = %+v", m)
	}
}

// TestSourcesUseRefusals: an unknown machine is 404 SOURCE_MACHINE_NOT_FOUND
// and a malformed request 400; neither changes anything.
func TestSourcesUseRefusals(t *testing.T) {
	s, router, b := sourcesServer(t)
	l := subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	before := mustBinding(t, s, "alice")
	writes := b.Writes()

	r := useSource(router, aliceFP, useBody(strings.Repeat("0", 32), ""))
	if r.code != http.StatusNotFound || r.errCode(t) != config.ErrSourceMachineNotFound {
		t.Fatalf("unknown machine = %d %s, want 404 %s", r.code, r.body, config.ErrSourceMachineNotFound)
	}
	for name, body := range map[string]string{
		"uppercase id":      useBody(strings.ToUpper(aliceB.ID), ""),
		"short id":          useBody(aliceA.ID[:31], ""),
		"hyphenated id":     useBody("00000000-0000-0000-0000-000000000001", ""),
		"empty id":          useBody("", ""),
		"no id":             `{}`,
		"not JSON":          `machine`,
		"empty body":        ``,
		"unknown field":     `{"machine_id":"` + aliceA.ID + `","force":true}`,
		"id not a string":   `{"machine_id":7}`,
		"name not an id":    useBody(aliceA.Name, ""),
		"not an object":     `["` + aliceB.ID + `"]`,
		"trailing garbage":  useBody(aliceB.ID, "") + `x`,
		"a second object":   useBody(aliceB.ID, "") + `{}`,
		"a second value":    useBody(aliceB.ID, "") + ` "` + aliceB.ID + `"`,
		"id given twice":    `{"machine_id":"` + aliceA.ID + `","machine_id":"` + aliceB.ID + `"}`,
		"user given twice":  `{"machine_id":"` + aliceB.ID + `","user":"alice","user":"alice"}`,
		"id in capitals":    `{"MACHINE_ID":"` + aliceB.ID + `"}`,
		"id in mixed case":  `{"Machine_Id":"` + aliceB.ID + `"}`,
		"user in capitals":  `{"machine_id":"` + aliceB.ID + `","User":"alice"}`,
		"user not a string": `{"machine_id":"` + aliceB.ID + `","user":["alice"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := useSource(router, aliceFP, body)
			if r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidRequest {
				t.Fatalf("%s = %d %s, want 400 INVALID_REQUEST", name, r.code, r.body)
			}
		})
	}
	if bd := mustBinding(t, s, "alice"); bd.Generation != before.Generation || b.Writes() != writes || closed(l.Done) {
		t.Fatalf("a refused move changed something: %+v (writes %d -> %d)", bd, writes, b.Writes())
	}
}

// TestSourcesTwoUsers: each user sees and moves only their own binding; a
// move of alice's leaves bob's listener alone.
func TestSourcesTwoUsers(t *testing.T) {
	s, router, _ := sourcesServer(t)
	bobM := testMachineFor(bobFP)
	subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	present(t, s, "alice", aliceB)
	lb := subscribeAs(t, s, "bob", "ssh-agent", bobM)

	bobView := sources(t, "bob's list", getSources(router, bobFP, ""))
	if bobView.User != "bob" || bobView.Binding.MachineID() != bobM.ID || len(bobView.Machines) != 1 || bobView.Machines[0].ID != bobM.ID {
		t.Fatalf("bob's list = %+v, want only his machine", bobView)
	}
	aliceView := sources(t, "alice's list", getSources(router, aliceFP, ""))
	for _, m := range aliceView.Machines {
		if m.ID == bobM.ID {
			t.Fatalf("alice's list shows bob's machine: %+v", aliceView)
		}
	}

	// Bob cannot move to alice's machine (it is not his) nor move hers.
	if r := useSource(router, bobFP, useBody(aliceB.ID, "")); r.code != http.StatusNotFound || r.errCode(t) != config.ErrSourceMachineNotFound {
		t.Fatalf("bob moving to alice's machine = %d %s, want 404", r.code, r.body)
	}
	if r := useSource(router, bobFP, useBody(aliceB.ID, "alice")); r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden {
		t.Fatalf("bob moving alice's = %d %s, want 403", r.code, r.body)
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceA.ID {
		t.Fatalf("bob changed alice's binding: %+v", bd)
	}
	if bd := mustBinding(t, s, "bob"); bd.MachineID() != bobM.ID {
		t.Fatalf("bob's binding changed: %+v", bd)
	}

	// Alice moves hers; bob's stream and binding are untouched.
	sources(t, "alice's move", useSource(router, aliceFP, useBody(aliceB.ID, "")))
	if closed(lb.Done) {
		t.Fatal("a move of alice's binding closed bob's stream")
	}
	if bd := mustBinding(t, s, "bob"); bd.MachineID() != bobM.ID {
		t.Fatalf("alice's move changed bob's binding: %+v", bd)
	}
}

// TestSourcesAdmin: an admin lists and moves another user's binding, but only
// to a machine that user presented; an admin's own list is their own; naming
// a user who is not configured is 400.
func TestSourcesAdmin(t *testing.T) {
	s, router, _ := sourcesServer(t)
	bobM := testMachineFor(bobFP)
	subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	present(t, s, "alice", aliceB)
	subscribeAs(t, s, "bob", "ssh-agent", bobM)

	view := sources(t, "admin listing alice's", getSources(router, rootFP, "?user=alice"))
	if view.User != "alice" || view.Binding.MachineID() != aliceA.ID || len(view.Machines) != 2 {
		t.Fatalf("admin's view of alice = %+v", view)
	}
	own := sources(t, "admin's own", getSources(router, rootFP, ""))
	if own.User != "root" || own.Binding != nil || len(own.Machines) != 0 {
		t.Fatalf("admin's own list = %+v, want root's (empty)", own)
	}

	// Only to a machine alice presented: bob's is unknown for her.
	if r := useSource(router, rootFP, useBody(bobM.ID, "alice")); r.code != http.StatusNotFound || r.errCode(t) != config.ErrSourceMachineNotFound {
		t.Fatalf("admin moving alice's to bob's machine = %d %s, want 404", r.code, r.body)
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceA.ID {
		t.Fatalf("a refused admin move changed alice's binding: %+v", bd)
	}
	moved := sources(t, "admin moving alice's", useSource(router, rootFP, useBody(aliceB.ID, "alice")))
	if moved.User != "alice" || moved.Binding.MachineID() != aliceB.ID {
		t.Fatalf("after the admin's move = %+v", moved)
	}
	if want := "admin root with key " + rootFP; moved.Binding.BoundBy != want {
		t.Fatalf("bound_by = %q, want %q", moved.Binding.BoundBy, want)
	}
	if bd := mustBinding(t, s, "bob"); bd.MachineID() != bobM.ID {
		t.Fatalf("the admin's move of alice's changed bob's: %+v", bd)
	}

	for _, q := range []string{"?user=ghost", "?user=" + url.QueryEscape("Not A Name"), "?user=alice&user=bob", "?user=Alice", "?user=" + testClientName} {
		if r := getSources(router, rootFP, q); r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidRequest {
			t.Errorf("admin GET %s = %d %s, want 400", q, r.code, r.body)
		}
	}
	for _, named := range []string{"ghost", "Alice", testClientName} {
		if r := useSource(router, rootFP, useBody(aliceA.ID, named)); r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidRequest {
			t.Errorf("admin moving %q's = %d %s, want 400", named, r.code, r.body)
		}
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceB.ID {
		t.Fatalf("a refused admin move changed alice's binding: %+v", bd)
	}

	// The admin moves their own, naming nobody or themselves: recorded as a
	// user's own move.
	rootM, root2 := testMachineFor(rootFP), source.Machine{ID: strings.Repeat("c3", 16), Name: "root's other"}
	subscribeAs(t, s, "root", "ssh-agent", rootM)
	present(t, s, "root", root2)
	self := sources(t, "admin moving their own", useSource(router, rootFP, useBody(root2.ID, "")))
	if self.User != "root" || self.Binding.MachineID() != root2.ID || self.Binding.BoundBy != "user root with key "+rootFP {
		t.Fatalf("admin's own move = %+v", self.Binding)
	}
	back := sources(t, "admin moving their own, naming themselves", useSource(router, rootFP, useBody(rootM.ID, "root")))
	if back.User != "root" || back.Binding.MachineID() != rootM.ID || back.Binding.BoundBy != "user root with key "+rootFP {
		t.Fatalf("admin's own move naming themselves = %+v", back.Binding)
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceB.ID {
		t.Fatalf("the admin's own move changed alice's: %+v", bd)
	}
}

// TestSourcesNonAdminNamingAnotherUser: a user who is not an admin naming
// another user gets the same 403, status, headers and body alike, whether
// that user exists or not: another user, an admin, nobody, a malformed name,
// their own name or another's in other capitals, and a service client's
// name. Naming themselves (exactly, or with an empty name) is their own.
func TestSourcesNonAdminNamingAnotherUser(t *testing.T) {
	s, router, _ := sourcesServer(t)
	subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	bobM, bob2 := testMachineFor(bobFP), source.Machine{ID: strings.Repeat("b2", 16), Name: "bob's other"}
	subscribeAs(t, s, "bob", "ssh-agent", bobM)
	present(t, s, "bob", bob2)

	var want *apiResp
	for _, named := range []string{"alice", "root", "ghost", "Not A Name", "Bob", "ALICE", testClientName} {
		for _, r := range []apiResp{
			getSources(router, bobFP, "?user="+url.QueryEscape(named)),
			useSource(router, bobFP, useBody(aliceA.ID, named)),
		} {
			if r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden {
				t.Fatalf("bob naming %q = %d %s, want 403", named, r.code, r.body)
			}
			if want == nil {
				first := r
				want = &first
			}
			if r.code != want.code || string(r.body) != string(want.body) || !reflect.DeepEqual(r.header, want.header) {
				t.Fatalf("bob naming %q got %d %v %s, but naming another got %d %v %s: the answer must not depend on the user",
					named, r.code, r.header, r.body, want.code, want.header, want.body)
			}
		}
	}
	for _, q := range []string{"?user=bob", "?user="} {
		if v := sources(t, "bob naming himself as "+q, getSources(router, bobFP, q)); v.User != "bob" || v.Binding.MachineID() != bobM.ID {
			t.Fatalf("bob naming himself as %q = %+v", q, v)
		}
	}
	// An empty name in the body is his own too (no move: his machine is bound).
	if v := sources(t, "bob's move naming nobody", useSource(router, bobFP, `{"machine_id":"`+bobM.ID+`","user":""}`)); v.User != "bob" || v.Binding.MachineID() != bobM.ID {
		t.Fatalf("bob's move with an empty user = %+v", v)
	}
	// Naming himself, he moves his own.
	if v := sources(t, "bob's move naming himself", useSource(router, bobFP, useBody(bob2.ID, "bob"))); v.User != "bob" || v.Binding.MachineID() != bob2.ID ||
		v.Binding.BoundBy != "user bob with key "+bobFP {
		t.Fatalf("bob's move naming himself = %+v", v.Binding)
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceA.ID {
		t.Fatalf("alice's binding changed: %+v", bd)
	}
}

// TestSourcesScriptKeyRefused: a script key can neither read nor move its own
// user's source, however it names the user; an admin's script key may not
// read or move the admin's own nor another user's (being an admin does not
// get past the script check).
func TestSourcesScriptKeyRefused(t *testing.T) {
	s, router, _ := sourcesServer(t)
	subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	present(t, s, "alice", aliceB)
	rootM := testMachineFor(rootFP)
	subscribeAs(t, s, "root", "ssh-agent", rootM)
	present(t, s, "root", aliceB)
	for _, r := range []apiResp{
		getSources(router, aliceScriptFP, ""),
		getSources(router, aliceScriptFP, "?user=alice"),
		useSource(router, aliceScriptFP, useBody(aliceB.ID, "")),
		useSource(router, aliceScriptFP, useBody(aliceB.ID, "alice")),
		getSources(router, rootScriptFP, ""),
		getSources(router, rootScriptFP, "?user=root"),
		getSources(router, rootScriptFP, "?user=alice"),
		useSource(router, rootScriptFP, useBody(aliceB.ID, "")),
		useSource(router, rootScriptFP, useBody(aliceB.ID, "root")),
		useSource(router, rootScriptFP, useBody(aliceB.ID, "alice")),
		unbindSource(router, aliceScriptFP, unbindBody("")),
		unbindSource(router, aliceScriptFP, unbindBody("alice")),
		unbindSource(router, rootScriptFP, unbindBody("")),
		unbindSource(router, rootScriptFP, unbindBody("alice")),
	} {
		if r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden || !strings.Contains(r.errMessage(t), "script key") {
			t.Fatalf("script key = %d %s, want 403 naming the script key", r.code, r.body)
		}
		if strings.Contains(string(r.body), aliceA.ID) || strings.Contains(string(r.body), rootM.ID) {
			t.Fatalf("a script key was told the bound machine: %s", r.body)
		}
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceA.ID {
		t.Fatalf("a script key moved the binding: %+v", bd)
	}
	if bd := mustBinding(t, s, "root"); bd.MachineID() != rootM.ID {
		t.Fatalf("the admin's script key moved the admin's binding: %+v", bd)
	}
	// The same user's other key may.
	sources(t, "alice's own key", useSource(router, aliceFP, useBody(aliceB.ID, "")))
}

// TestSourcesNeedTheControlScope: a credentials certificate (a machine's
// broker's) is refused both routes and moves nothing.
func TestSourcesNeedTheControlScope(t *testing.T) {
	s, router, _ := sourcesServer(t)
	subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	present(t, s, "alice", aliceB)
	var rs []apiResp
	for _, fp := range []string{aliceFP, rootFP} {
		for _, named := range sourceTargets {
			rs = append(rs,
				do(router, credsAs(fp), http.MethodGet, "/api/sources?user="+url.QueryEscape(named), ""),
				do(router, credsAs(fp), http.MethodPost, "/api/sources/use", useBody(aliceB.ID, named)),
				do(router, credsAs(fp), http.MethodPost, "/api/sources/unbind", unbindBody(named)))
		}
	}
	for _, r := range rs {
		if r.code != http.StatusForbidden {
			t.Fatalf("credentials certificate = %d %s, want 403", r.code, r.body)
		}
		if strings.Contains(string(r.body), aliceA.ID) {
			t.Fatalf("a credentials certificate was told the bound machine: %s", r.body)
		}
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceA.ID {
		t.Fatalf("a credentials certificate moved the binding: %+v", bd)
	}
}

// sourceTargets are the users a refused caller names: none, an empty name,
// a user, an admin, nobody, a user in other capitals, and a service client.
var sourceTargets = []string{"", "alice", "root", "ghost", "Alice", testClientName}

// TestSourcesRefuseCallersThatAreNotUsers: the handlers themselves refuse a
// service client and a request with no caller, whoever they name (the client
// allowlist refuses a client first: through the router too, below, and
// TestClientRouteWalk covers that door).
func TestSourcesRefuseCallersThatAreNotUsers(t *testing.T) {
	s, router, _ := sourcesServer(t)
	subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	present(t, s, "alice", aliceB)
	for name, ctx := range map[string]context.Context{
		"service client":    withCaller(context.Background(), Caller{Kind: CallerClient, Name: testClientName, Client: &config.ServiceClientConfig{}}),
		"no caller":         context.Background(),
		"user with no name": withCaller(context.Background(), Caller{Kind: CallerUser, Name: aliceFP}),
	} {
		for _, named := range sourceTargets {
			for _, h := range []struct {
				method, target, body string
				fn                   http.HandlerFunc
			}{
				{http.MethodGet, "/api/sources?user=" + url.QueryEscape(named), "", s.handleListSources},
				{http.MethodPost, "/api/sources/use", useBody(aliceB.ID, named), s.handleUseSource},
				{http.MethodPost, "/api/sources/unbind", unbindBody(named), s.handleUnbindSource},
			} {
				req := httptest.NewRequest(h.method, h.target, strings.NewReader(h.body)).WithContext(ctx)
				rec := httptest.NewRecorder()
				h.fn(rec, req)
				if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), aliceA.ID) {
					t.Errorf("%s %s naming %q = %d %s, want 403 and nothing of alice's", name, h.method, named, rec.Code, rec.Body.String())
				}
			}
		}
	}
	for _, named := range sourceTargets {
		for _, r := range []apiResp{
			do(router, serviceLeaf(testClientName), http.MethodGet, "/api/sources?user="+url.QueryEscape(named), ""),
			do(router, serviceLeaf(testClientName), http.MethodPost, "/api/sources/use", useBody(aliceB.ID, named)),
			do(router, serviceLeaf(testClientName), http.MethodPost, "/api/sources/unbind", unbindBody(named)),
		} {
			if r.code != http.StatusForbidden || strings.Contains(string(r.body), aliceA.ID) {
				t.Errorf("service client through the router naming %q = %d %s, want 403 and nothing of alice's", named, r.code, r.body)
			}
		}
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceA.ID {
		t.Fatalf("a caller that is not a user moved alice's binding: %+v", bd)
	}
}

// TestSourcesStoreFailure: a move whose save fails is 500, moves nothing and
// closes nothing.
func TestSourcesStoreFailure(t *testing.T) {
	s, router, b := sourcesServer(t)
	l := subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	present(t, s, "alice", aliceB)
	b.FailWrites(errors.New("disk full"))
	r := useSource(router, aliceFP, useBody(aliceB.ID, ""))
	if r.code != http.StatusInternalServerError || r.errCode(t) != config.ErrInternalError {
		t.Fatalf("move with a failing store = %d %s, want 500", r.code, r.body)
	}
	// A save that fails late (its directory sync) may still be read at the
	// next start, so the answer claims nothing changed only for now, and
	// says how to check.
	msg := r.errMessage(t)
	if strings.Contains(msg, "nothing changed") || !strings.Contains(msg, "not confirmed") ||
		!strings.Contains(msg, "current credential source stays in use for now") || !strings.Contains(msg, "restarts") || !strings.Contains(msg, "shed source ls") {
		t.Fatalf("store failure message = %q", msg)
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceA.ID {
		t.Fatalf("a failed move changed the binding: %+v", bd)
	}
	if closed(l.Done) {
		t.Fatal("a failed move closed the bound machine's stream")
	}
	b.FailWrites(nil)
	sources(t, "the move once saves work", useSource(router, aliceFP, useBody(aliceB.ID, "")))
}

// TestSourcesImplicitUser: on a server with no user list (open mode here) the
// caller is the implicit user, who reads and moves its binding; there is no
// other user to name.
func TestSourcesImplicitUser(t *testing.T) {
	s := newTestServerWithPlugins()
	router := s.Router()
	open := source.Machine{ID: strings.Repeat("0a", 16), Name: "desk"}
	subscribeAs(t, s, "", "ssh-agent", testMachineFor(""))
	present(t, s, "", open)

	view := sources(t, "implicit list", do(router, nil, http.MethodGet, "/api/sources", ""))
	if view.User != "" || view.Binding.MachineID() != testMachineFor("").ID || len(view.Machines) != 2 {
		t.Fatalf("implicit list = %+v", view)
	}
	moved := sources(t, "implicit move", do(router, nil, http.MethodPost, "/api/sources/use", useBody(open.ID, "")))
	if moved.Binding.MachineID() != open.ID || moved.Binding.BoundBy != "the implicit user (no key: open mode)" {
		t.Fatalf("implicit move = %+v", moved.Binding)
	}
	if r := do(router, nil, http.MethodGet, "/api/sources?user=alice", ""); r.code != http.StatusBadRequest {
		t.Fatalf("implicit user naming alice = %d %s, want 400 (no user list)", r.code, r.body)
	}
}

// TestSourcesNoBindings: a server whose bus has no source bindings answers
// 503 on both routes.
func TestSourcesNoBindings(t *testing.T) {
	reg := plugin.NewRegistry()
	s := NewServer(nil, &config.ServerConfig{Name: "t", HTTPPort: 8080}, "", reg, plugin.NewBridge(reg, false))
	for _, r := range []apiResp{
		do(s.Router(), nil, http.MethodGet, "/api/sources", ""),
		do(s.Router(), nil, http.MethodPost, "/api/sources/use", useBody(aliceA.ID, "")),
		do(s.Router(), nil, http.MethodPost, "/api/sources/unbind", ""),
	} {
		if r.code != http.StatusServiceUnavailable {
			t.Fatalf("no bindings = %d %s, want 503", r.code, r.body)
		}
	}
}

// TestSourcesUnbind: a user clears their own credential source. The store
// forgets the binding and the remembered machines, the user's streams are
// closed, and the next machine to connect is bound (first-connect). A second
// unbind is answered that nothing changed. Another user's binding is
// untouched.
func TestSourcesUnbind(t *testing.T) {
	s, router, _ := sourcesServer(t)
	l := subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	present(t, s, "alice", aliceB)
	bobM := testMachineFor(bobFP)
	bobL := subscribeAs(t, s, "bob", "ssh-agent", bobM)

	resp := unbound(t, "alice's unbind", unbindSource(router, aliceFP, unbindBody("")))
	if resp.User != "alice" || !resp.Unbound || resp.Previous == nil || resp.Previous.ID != aliceA.ID || resp.Previous.Name != aliceA.Name || resp.ForgottenMachines != 2 {
		t.Fatalf("unbind = %+v", resp)
	}
	if _, ok := s.plugins.Bindings().Get("alice"); ok {
		t.Fatal("the unbind left alice's binding")
	}
	if !closed(l.Done) {
		t.Fatal("the unbind left the old source's stream open")
	}
	if view := sources(t, "after the unbind", getSources(router, aliceFP, "")); view.Binding != nil || len(view.Machines) != 0 {
		t.Fatalf("sources after the unbind = %+v", view)
	}
	if _, ok := s.plugins.DeliveryBound("alice"); ok {
		t.Fatal("delivery still finds a bound machine: a create would ask an offline source")
	}
	if closed(bobL.Done) || mustBinding(t, s, "bob").MachineID() != bobM.ID {
		t.Fatal("alice's unbind touched bob's source")
	}

	// Idempotent: nothing to clear is a success that says so. No body at
	// all is the caller's own.
	again := unbound(t, "a second unbind", unbindSource(router, aliceFP, ""))
	if again.Unbound || again.Previous != nil || again.ForgottenMachines != 0 {
		t.Fatalf("second unbind = %+v", again)
	}
	if !strings.Contains(string(unbindSource(router, aliceFP, "{}").body), `"previous":null`) {
		t.Fatal("an unbind with nothing to clear does not answer previous: null")
	}

	// The next machine to connect is bound, by first connect, whichever it is.
	subscribeAs(t, s, "alice", "ssh-agent", aliceB)
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceB.ID || bd.BoundBy != source.BoundByFirstConnect {
		t.Fatalf("binding after the next connect = %+v", bd)
	}
}

// TestSourcesUnbindRefusals: a malformed body is 400 and changes nothing; a
// user naming another is 403; an admin naming a user who is not configured
// is 400; an admin naming alice clears hers, recorded as the admin's.
func TestSourcesUnbindRefusals(t *testing.T) {
	s, router, _ := sourcesServer(t)
	subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	for _, body := range []string{
		`[]`, `"alice"`, `{"user":"alice","user":"alice"}`, `{"User":"alice"}`, `{"machine_id":"x"}`,
		`{"user":1}`, `{} {}`, `{`,
	} {
		r := unbindSource(router, aliceFP, body)
		if r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidRequest {
			t.Errorf("body %s = %d %s, want 400", body, r.code, r.body)
		}
	}
	for _, named := range []string{"alice", "ghost"} {
		if r := unbindSource(router, bobFP, unbindBody(named)); r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden {
			t.Errorf("bob naming %s = %d %s, want 403", named, r.code, r.body)
		}
	}
	if r := unbindSource(router, rootFP, unbindBody("ghost")); r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidRequest {
		t.Errorf("admin naming ghost = %d %s, want 400", r.code, r.body)
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceA.ID {
		t.Fatalf("a refused unbind changed alice's binding: %+v", bd)
	}
	resp := unbound(t, "the admin's unbind of alice", unbindSource(router, rootFP, unbindBody("alice")))
	if resp.User != "alice" || !resp.Unbound || resp.Previous == nil || resp.Previous.ID != aliceA.ID {
		t.Fatalf("admin unbind = %+v", resp)
	}
	if _, ok := s.plugins.Bindings().Get("alice"); ok {
		t.Fatal("the admin's unbind left alice's binding")
	}
}

// TestSourcesUnbindStoreFailure: an unbind whose save fails is 500, clears
// nothing and closes nothing.
func TestSourcesUnbindStoreFailure(t *testing.T) {
	s, router, b := sourcesServer(t)
	l := subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	b.FailWrites(errors.New("disk full"))
	r := unbindSource(router, aliceFP, unbindBody(""))
	if r.code != http.StatusInternalServerError || r.errCode(t) != config.ErrInternalError || !strings.Contains(r.errMessage(t), "not confirmed") {
		t.Fatalf("unbind with a failing store = %d %s, want 500", r.code, r.body)
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceA.ID {
		t.Fatalf("a failed unbind changed the binding: %+v", bd)
	}
	if closed(l.Done) {
		t.Fatal("a failed unbind closed the bound machine's stream")
	}
	b.FailWrites(nil)
	if !unbound(t, "the unbind once saves work", unbindSource(router, aliceFP, unbindBody(""))).Unbound {
		t.Fatal("the unbind did not clear the binding once saves worked")
	}
}

// TestSourcesUnbindImplicitUser: on a server with no user list the implicit
// user clears its own binding; there is no other user to name.
func TestSourcesUnbindImplicitUser(t *testing.T) {
	s := newTestServerWithPlugins()
	router := s.Router()
	subscribeAs(t, s, "", "ssh-agent", testMachineFor(""))
	if r := do(router, nil, http.MethodPost, "/api/sources/unbind", unbindBody("alice")); r.code != http.StatusBadRequest {
		t.Fatalf("implicit user naming alice = %d %s, want 400 (no user list)", r.code, r.body)
	}
	resp := unbound(t, "implicit unbind", do(router, nil, http.MethodPost, "/api/sources/unbind", ""))
	if resp.User != "" || !resp.Unbound || resp.Previous == nil || resp.Previous.ID != testMachineFor("").ID {
		t.Fatalf("implicit unbind = %+v", resp)
	}
}

// TestSourcesNamesAreJSONEncoded: a machine name holding HTML, quotes, a
// bidirectional override and a zero-width character comes back exactly, as
// JSON, with no raw HTML character in the body.
func TestSourcesNamesAreJSONEncoded(t *testing.T) {
	s, router, _ := sourcesServer(t)
	name := "<b>\"x\" \u202egnp.exe\u200b"
	if !source.ValidMachineName(name) {
		t.Fatal("test set-up: the name is not valid")
	}
	subscribeAs(t, s, "alice", "ssh-agent", source.Machine{ID: strings.Repeat("ee", 16), Name: name})
	r := getSources(router, aliceFP, "")
	if strings.ContainsAny(string(r.body), "<>") {
		t.Fatalf("raw HTML characters in %s", r.body)
	}
	if got := sources(t, "list", r).Machines[0].Name; got != name {
		t.Fatalf("name = %q, want %q", got, name)
	}
}

// TestMoverFor: what a move records as its mover.
func TestMoverFor(t *testing.T) {
	long := strings.Repeat("x", source.MaxTextBytes)
	for _, tc := range []struct {
		name  string
		c     Caller
		other bool
		want  string
	}{
		{"own, labelled key", Caller{User: "alice", Name: aliceFP, KeyLabel: "laptop"}, false, `user alice with key "laptop" (` + aliceFP + ")"},
		{"own, no label", Caller{User: "alice", Name: aliceFP}, false, "user alice with key " + aliceFP},
		{"admin for another", Caller{User: "root", Name: rootFP, KeyLabel: "desk"}, true, `admin root with key "desk" (` + rootFP + ")"},
		{"control characters in a label are escaped", Caller{User: "alice", Name: aliceFP, KeyLabel: "a\nb\u202e"}, false, `user alice with key "a\nb\u202e" (` + aliceFP + ")"},
		{"a label too long is left out", Caller{User: "alice", Name: aliceFP, KeyLabel: long}, false, "user alice with key " + aliceFP},
		{"implicit, mtls", Caller{Implicit: true, Name: aliceFP}, false, "the implicit user with key " + aliceFP},
		{"implicit, open mode", Caller{Implicit: true}, false, "the implicit user (no key: open mode)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := moverFor(tc.c, tc.other)
			if got != tc.want {
				t.Fatalf("mover = %q, want %q", got, tc.want)
			}
			if len(got) > source.MaxTextBytes || strings.ContainsAny(got, "\n\u202e") {
				t.Fatalf("mover %q cannot be stored", got)
			}
		})
	}
}

// TestUserCallerKeyLabel: only a label from auth.users or from a directory's
// list becomes the caller's key label; an authorized_keys comment (often an
// account and host) does not.
func TestUserCallerKeyLabel(t *testing.T) {
	u := users.User{Name: "alice"}
	if c := userCaller(u, users.Key{Source: users.SourceAdHoc, Label: "laptop"}, aliceFP, ""); c.KeyLabel != "laptop" {
		t.Fatalf("auth.users label = %q, want laptop", c.KeyLabel)
	}
	c := userCaller(u, users.Key{Source: users.SourceDirectory, Label: "work \"laptop\""}, aliceFP, "")
	if c.KeyLabel != `work "laptop"` {
		t.Fatalf("directory label = %q, want the list's label", c.KeyLabel)
	}
	// It is shown quoted, like a config user's.
	if got, want := moverFor(c, false), `user alice with key "work \"laptop\"" (`+aliceFP+")"; got != want {
		t.Fatalf("a directory user's mover = %q, want %q", got, want)
	}
	for _, src := range []users.Source{users.SourceInline, users.SourceFile, users.SourceGitHub} {
		if c := userCaller(u, users.Key{Source: src, Label: "comment naming a person and host"}, aliceFP, ""); c.KeyLabel != "" {
			t.Fatalf("a %v key's comment became the label %q", src, c.KeyLabel)
		}
	}
}

// TestSourcesImplicitUserOverMTLS: on an mtls server with no user list the
// certificate's key is the implicit user's, through the real certificate
// middleware: an empty name (in the query or the body) is the implicit
// user's own, and any named user is 400 (there is no user list to name one
// from). The mover is the implicit user with that key.
func TestSourcesImplicitUserOverMTLS(t *testing.T) {
	reg := plugin.NewRegistry()
	s := NewServer(nil, &config.ServerConfig{Name: "t", Auth: &config.AuthConfig{Mode: config.AuthModeMTLS}}, "", reg, plugin.NewBridge(reg, true))
	s.SetUserLookup(userstest.Implicit(testAdminFP))
	pinBus(s)
	router := s.Router()
	mine, other := testMachineFor(testAdminFP), source.Machine{ID: strings.Repeat("0b", 16), Name: "desk"}
	subscribeAs(t, s, "", "ssh-agent", mine)
	present(t, s, "", other)

	for _, q := range []string{"", "?user="} {
		v := sources(t, "implicit GET "+q, doAs(router, testAdminFP, http.MethodGet, "/api/sources"+q, ""))
		if v.User != "" || v.Binding.MachineID() != mine.ID || len(v.Machines) != 2 {
			t.Fatalf("implicit GET %q = %+v", q, v)
		}
	}
	for _, named := range []string{"alice", "root", testClientName} {
		if r := doAs(router, testAdminFP, http.MethodGet, "/api/sources?user="+named, ""); r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidRequest {
			t.Errorf("implicit GET naming %q = %d %s, want 400", named, r.code, r.body)
		}
		if r := doAs(router, testAdminFP, http.MethodPost, "/api/sources/use", useBody(other.ID, named)); r.code != http.StatusBadRequest || r.errCode(t) != config.ErrInvalidRequest {
			t.Errorf("implicit move naming %q = %d %s, want 400", named, r.code, r.body)
		}
	}
	if bd := mustBinding(t, s, ""); bd.MachineID() != mine.ID {
		t.Fatalf("a refused implicit move changed the binding: %+v", bd)
	}
	moved := sources(t, "implicit move", doAs(router, testAdminFP, http.MethodPost, "/api/sources/use", `{"machine_id":"`+other.ID+`","user":""}`))
	if moved.User != "" || moved.Binding.MachineID() != other.ID || moved.Binding.BoundBy != "the implicit user with key "+testAdminFP {
		t.Fatalf("implicit move = %+v", moved.Binding)
	}
}

// TestSourcesTargetIsReadFromOnePlaceOnly: GET takes the user from its query
// only (a body is ignored), POST from its body only (a query is ignored), so
// a request cannot name one user to the check and another to the action.
func TestSourcesTargetIsReadFromOnePlaceOnly(t *testing.T) {
	s, router, _ := sourcesServer(t)
	subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	present(t, s, "alice", aliceB)
	bobM := testMachineFor(bobFP)
	subscribeAs(t, s, "bob", "ssh-agent", bobM)

	if v := sources(t, "bob's GET with a body naming alice", doAs(router, bobFP, http.MethodGet, "/api/sources", `{"user":"alice"}`)); v.User != "bob" {
		t.Fatalf("a GET body named the user: %+v", v)
	}
	r := doAs(router, bobFP, http.MethodPost, "/api/sources/use?user=alice", useBody(aliceB.ID, ""))
	if r.code != http.StatusNotFound || r.errCode(t) != config.ErrSourceMachineNotFound {
		t.Fatalf("bob's POST with ?user=alice = %d %s, want 404 (his own records, which lack alice's machine)", r.code, r.body)
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceA.ID {
		t.Fatalf("a query on POST moved alice's binding: %+v", bd)
	}
}

// TestSourcesUseAnswersItsOwnMove: the answer to a move is the state that
// move left, even when another move of the same user lands before the answer
// is written (forced with the route's hook).
func TestSourcesUseAnswersItsOwnMove(t *testing.T) {
	s, router, _ := sourcesServer(t)
	subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	present(t, s, "alice", aliceB)
	hooked := false
	sourceMovedHook = func() {
		hooked = true
		if _, err := s.plugins.Move(context.Background(), "alice", aliceA.ID, "user alice with key other"); err != nil {
			t.Error(err)
		}
	}
	defer func() { sourceMovedHook = nil }()
	resp := sources(t, "move", useSource(router, aliceFP, useBody(aliceB.ID, "")))
	if !hooked {
		t.Fatal("test set-up: the hook did not run")
	}
	if bd := mustBinding(t, s, "alice"); bd.MachineID() != aliceA.ID {
		t.Fatalf("test set-up: the second move did not land: %+v", bd)
	}
	if resp.Binding.MachineID() != aliceB.ID || resp.Binding.BoundBy != "user alice with key "+aliceFP {
		t.Fatalf("the answer = %+v, want this request's move (machine B, by alice's key)", resp.Binding)
	}
	if a := machineRow(t, resp, aliceA.ID); a.Connected || len(a.Namespaces) != 0 {
		t.Fatalf("the answer shows the old machine connected: %+v", a)
	}
}

// TestSourcesReadDuringASave: a GET that arrives while a move holds the
// user's gate (its store write, held open here) waits for it and then shows
// the state the move left; one that waits past the bounded time is 503
// SOURCE_BUSY with Retry-After and nothing of the user's records; one whose
// client goes away is answered nothing.
func TestSourcesReadDuringASave(t *testing.T) {
	s, router, b := sourcesServer(t)
	subscribeAs(t, s, "alice", "ssh-agent", aliceA)
	present(t, s, "alice", aliceB)
	old := sourcesReadWait
	defer func() { sourcesReadWait = old }()

	var busy apiResp
	var gone *httptest.ResponseRecorder
	waited := make(chan apiResp, 1)
	saves := 0
	b.BeforeWrite(func() {
		saves++
		if saves != 1 {
			return
		}
		// The gate is held: the move is saving.
		sourcesReadWait = 30 * time.Millisecond
		bounded := make(chan apiResp, 1)
		go func() { bounded <- getSources(router, aliceFP, "") }()
		select {
		case busy = <-bounded:
		case <-time.After(3 * time.Second):
			t.Error("a GET waited for the move's save past its bound")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodGet, "/api/sources", nil).WithContext(ctx)
		withClientCert(req, testClientCert(aliceFP, "control", time.Hour))
		gone = httptest.NewRecorder()
		router.ServeHTTP(gone, req)
		sourcesReadWait = 10 * time.Second
		go func() { waited <- getSources(router, aliceFP, "") }()
	})
	defer b.BeforeWrite(nil)
	moved := sources(t, "move", useSource(router, aliceFP, useBody(aliceB.ID, "")))

	if busy.code != http.StatusServiceUnavailable || busy.errCode(t) != config.ErrSourceBusy || busy.header.Get("Retry-After") != "1" {
		t.Fatalf("a GET past the bounded wait = %d %v %s, want 503 SOURCE_BUSY with Retry-After", busy.code, busy.header, busy.body)
	}
	if strings.Contains(string(busy.body), aliceA.ID) || strings.Contains(string(busy.body), aliceB.ID) {
		t.Fatalf("the busy answer names a machine: %s", busy.body)
	}
	if gone.Body.Len() != 0 {
		t.Fatalf("a GET whose client went away was answered %d %s", gone.Code, gone.Body)
	}
	var r apiResp
	select {
	case r = <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the GET that waited for the move did not finish")
	}
	v := sources(t, "the GET that waited", r)
	if v.Binding.MachineID() != aliceB.ID || v.Binding.Generation != moved.Binding.Generation {
		t.Fatalf("the GET that waited shows %+v, want the move's binding", v.Binding)
	}
	if a := machineRow(t, v, aliceA.ID); a.Connected || len(a.Namespaces) != 0 {
		t.Fatalf("the GET that waited shows the old machine connected: %+v", a)
	}
}
