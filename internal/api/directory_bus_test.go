package api

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// The credential bus and client certificates on a directory server, whose
// users change while it runs.

// boundCreds is a credentials certificate for the key with this
// fingerprint, bound to user name, as the bootstrap issues one.
func boundCreds(fp, name string) *x509.Certificate {
	leaf := credsAs(fp)
	leaf.Subject.Names = []pkix.AttributeTypeAndValue{{Type: servertls.ClientUserOID, Value: name}}
	return leaf
}

// directoryLookupFor is a directory lookup whose local admin root has the key
// rootLine, holding a list (pull 1, trusted for an hour from now) of the
// users named in listed, each with the key of that fingerprint.
func directoryLookupFor(t *testing.T, rootLine string, listed map[string]string) *directory.Lookup {
	t.Helper()
	l, err := directory.NewLookup(directory.LookupOptions{
		LocalAdmins: map[string]config.UserConfig{"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: rootLine, Label: "laptop"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	installUsers(t, l, 1, listed)
	return l
}

// installUsers installs, as pull seq, a list of the users named in listed,
// each with the key of that fingerprint.
func installUsers(t *testing.T, l *directory.Lookup, seq uint64, listed map[string]string) {
	t.Helper()
	list := &directory.List{}
	for name, fp := range listed {
		list.Users = append(list.Users, directory.User{Name: name, Keys: []directory.Key{{Fingerprint: fp, Label: "l"}}})
	}
	if ok, err := l.Install(list, time.Now(), time.Hour, seq); err != nil || !ok {
		t.Fatalf("Install(seq %d) = %v, %v", seq, ok, err)
	}
}

// TestUnboundCertificateRefusedOnADirectoryServer: a client certificate
// that names no user (issued before certificates named one) is refused on a
// directory server, for a local admin's key and a listed user's alike, while
// the same keys' bound certificates are accepted; it is refused with the
// lookup of auth.users (ConfigLookup) and with no user list as well, where
// the same key's bound certificate is accepted (cert_binding_test.go holds
// the handshake).
func TestUnboundCertificateRefusedOnADirectoryServer(t *testing.T) {
	rootLine, rootFP := directoryTestKey(t)
	_, adaFP := directoryTestKey(t)
	lookup := directoryLookupFor(t, rootLine, map[string]string{"ada": adaFP})
	s, router := clientTestServer(t, newClientFakeBackend(), defaultTestPolicy())
	s.SetUserLookup(lookup)

	for _, tc := range []struct{ name, fp string }{{"root", rootFP}, {"ada", adaFP}} {
		unbound := unboundClientCert(tc.fp, "control", time.Hour)
		if _, bound, err := servertls.ClientCertUser(unbound); err != nil || bound {
			t.Fatalf("the test's unbound certificate is bound (%v, %v)", bound, err)
		}
		if u, _, ok := CertificateUser(lookup, unbound, time.Now()); ok {
			t.Errorf("CertificateUser accepted %s's unbound certificate as %+v", tc.name, u)
		}
		if r := do(router, unbound, "GET", "/api/sheds", ""); r.code != http.StatusUnauthorized {
			t.Errorf("GET /api/sheds with %s's unbound certificate = %d %s, want 401", tc.name, r.code, r.body)
		}
		if u, _, ok := CertificateUser(lookup, boundLeaf(tc.fp, tc.name), time.Now()); !ok || u.Name != tc.name {
			t.Errorf("CertificateUser(%s's bound certificate) = %+v %v", tc.name, u, ok)
		}
		if r := do(router, boundLeaf(tc.fp, tc.name), "GET", "/api/sheds", ""); r.code != http.StatusOK {
			t.Errorf("GET /api/sheds with %s's bound certificate = %d %s, want 200", tc.name, r.code, r.body)
		}
	}

	cl, err := users.NewConfigLookup(users.ConfigOptions{
		SSH:   &config.SSHAuthConfig{Mode: config.SSHAuthEnforce},
		Users: map[string]config.UserConfig{"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: rootLine, Label: "laptop"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, only := any(cl).(users.BoundCredentialsOnly); only {
		t.Fatal("ConfigLookup is BoundCredentialsOnly")
	}
	if u, _, ok := CertificateUser(cl, unboundClientCert(rootFP, "control", time.Hour), time.Now()); ok {
		t.Errorf("CertificateUser(ConfigLookup, root's unbound certificate) = %+v; want refused", u)
	}
	if u, _, ok := CertificateUser(cl, boundLeaf(rootFP, "root"), time.Now()); !ok || u.Name != "root" {
		t.Errorf("CertificateUser(ConfigLookup, root's bound certificate) = %+v %v; want root", u, ok)
	}
	implicit := userstest.Implicit(rootFP)
	if u, _, ok := CertificateUser(implicit, unboundClientCert(rootFP, "control", time.Hour), time.Now()); ok {
		t.Errorf("CertificateUser(no user list, an unbound certificate) = %+v; want refused", u)
	}
	if u, _, ok := CertificateUser(implicit, implicitClientCert(rootFP, "control", time.Hour), time.Now()); !ok || !u.Implicit {
		t.Errorf("CertificateUser(no user list, the implicit user's certificate) = %+v %v; want the implicit user", u, ok)
	}
}

// streamEnds waits for a stream's lines to end and returns the data lines
// it sent first.
func streamEnds(t *testing.T, who string, lines <-chan string) []string {
	t.Helper()
	var got []string
	timeout := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return got
			}
			got = append(got, l)
		case <-timeout:
			t.Fatalf("%s's stream did not end (it sent %v)", who, got)
		}
	}
}

// TestBusStreamEndsWhenTheCallerLeavesTheList: on a directory server alice's
// open credential-bus stream is sent box's first request; a new list without
// alice is published; box's next request is not sent, the stream ends and
// its listener is unregistered. The request stays pending: once alice is
// listed again, her next subscription is sent it.
func TestBusStreamEndsWhenTheCallerLeavesTheList(t *testing.T) {
	rootLine, _ := directoryTestKey(t)
	_, aliceFP := directoryTestKey(t)
	_, bobFP := directoryTestKey(t)
	lookup := directoryLookupFor(t, rootLine, map[string]string{"alice": aliceFP, "bob": bobFP})
	s := busUsersServer(t)
	s.SetUserLookup(lookup)
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: (&sendCounter{}).send})
	ts := sseServer(t, s, map[string]*x509.Certificate{"alice": boundCreds(aliceFP, "alice")})
	const path = "/api/plugins/listeners/ssh-agent/messages"
	key := plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"}

	lines := openStream(t, ts, "alice", path)
	waitListener(t, s, key)
	first := publishFromShed(t, s, "box", gen)
	if got := nextData(t, "alice", lines); !strings.Contains(got, first) {
		t.Fatalf("alice's first message = %s, want box's request %s", got, first)
	}

	installUsers(t, lookup, 2, map[string]string{"bob": bobFP})
	second := publishFromShed(t, s, "box", gen)
	if sent := streamEnds(t, "alice", lines); len(sent) != 0 {
		t.Errorf("alice's stream sent %v after she left the list, want nothing", sent)
	}
	if _, ok := s.plugins.Get(key); ok {
		t.Error("alice's listener is still registered after her stream ended")
	}
	if r := do(s.Router(), boundCreds(aliceFP, "alice"), http.MethodGet, path, ""); r.code != http.StatusUnauthorized {
		t.Errorf("alice's new subscription after she left the list = %d %s, want 401", r.code, r.body)
	}

	// Both requests are pending (neither was answered): the next
	// subscription is sent both, the one never sent included.
	installUsers(t, lookup, 3, map[string]string{"alice": aliceFP, "bob": bobFP})
	again := openStream(t, ts, "alice", path)
	got := nextData(t, "alice", again) + "\n" + nextData(t, "alice", again)
	if !strings.Contains(got, first) || !strings.Contains(got, second) {
		t.Errorf("alice's next subscription was sent %s, want the pending requests %s and %s", got, first, second)
	}
}

// TestBusStreamEndsWhenTheCertificateExpires: on a directory server alice's
// open credential-bus stream is checked against her certificate's validity
// dates before each message, as the middleware checks every request's: at
// the certificate's NotAfter it is still sent a request; one nanosecond
// later, with her key and her user still listed, the next request is not
// sent, the stream ends and its listener is unregistered.
func TestBusStreamEndsWhenTheCertificateExpires(t *testing.T) {
	rootLine, _ := directoryTestKey(t)
	_, aliceFP := directoryTestKey(t)
	lookup := directoryLookupFor(t, rootLine, map[string]string{"alice": aliceFP})
	s := busUsersServer(t)
	s.SetUserLookup(lookup)
	leaf := boundCreds(aliceFP, "alice")
	var mu sync.Mutex
	now := time.Now()
	s.busNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	setNow := func(at time.Time) {
		mu.Lock()
		now = at
		mu.Unlock()
	}
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: (&sendCounter{}).send})
	ts := sseServer(t, s, map[string]*x509.Certificate{"alice": leaf})
	const path = "/api/plugins/listeners/ssh-agent/messages"
	key := plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"}

	lines := openStream(t, ts, "alice", path)
	waitListener(t, s, key)
	first := publishFromShed(t, s, "box", gen)
	if got := nextData(t, "alice", lines); !strings.Contains(got, first) {
		t.Fatalf("alice's first message = %s, want box's request %s", got, first)
	}

	setNow(leaf.NotAfter)
	second := publishFromShed(t, s, "box", gen)
	if got := nextData(t, "alice", lines); !strings.Contains(got, second) {
		t.Fatalf("alice's message at her certificate's NotAfter = %s, want %s", got, second)
	}

	setNow(leaf.NotAfter.Add(time.Nanosecond))
	publishFromShed(t, s, "box", gen)
	if sent := streamEnds(t, "alice", lines); len(sent) != 0 {
		t.Errorf("alice's stream sent %v after her certificate expired, want nothing", sent)
	}
	if _, ok := s.plugins.Get(key); ok {
		t.Error("alice's listener is still registered after her stream ended")
	}
	if u, _, ok := lookup.UserForKey(aliceFP); !ok || u.Name != "alice" {
		t.Errorf("alice's key resolves to %+v %v; the stream must have ended on the date alone", u, ok)
	}
}

// TestBusStreamNotRecheckedWithoutADirectory: on a server with auth.users
// (here the test double) a stream is authorized when it opens, as before: a
// key removed from the lookup leaves the open stream sending.
func TestBusStreamNotRecheckedWithoutADirectory(t *testing.T) {
	s := busUsersServer(t)
	gen := s.bridge.RegisterShed("box", &plugin.ShedConn{User: "alice", CreatedAt: busT1, Send: (&sendCounter{}).send})
	ts := sseServer(t, s, map[string]*x509.Certificate{"alice": credsAs(aliceFP)})
	const path = "/api/plugins/listeners/ssh-agent/messages"

	lines := openStream(t, ts, "alice", path)
	waitListener(t, s, plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"})
	first := publishFromShed(t, s, "box", gen)
	if got := nextData(t, "alice", lines); !strings.Contains(got, first) {
		t.Fatalf("alice's first message = %s, want %s", got, first)
	}
	s.users.(*userstest.Fake).RemoveKey(aliceFP)
	second := publishFromShed(t, s, "box", gen)
	if got := nextData(t, "alice", lines); !strings.Contains(got, second) {
		t.Errorf("alice's open stream after her key left = %s, want %s sent as before", got, second)
	}
}
