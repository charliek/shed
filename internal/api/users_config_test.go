package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/users"
)

// These tests drive the API door with users built from a real server.yaml:
// the config loader, the config lookup and the access guard, as shed-server
// wires them, around the fake backend of user_routes_test.go.

// keyPair is a fresh key's authorized_keys line and fingerprint.
type keyPair struct{ line, fp string }

func newKeyPair(t *testing.T) keyPair {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return keyPair{line: strings.TrimSpace(string(gossh.MarshalAuthorizedKey(k))), fp: gossh.FingerprintSHA256(k)}
}

// configServer loads body (after a backend block) as server.yaml through the
// CLI loader, builds the user lookup from it as shed-server does, and returns
// an API server over be.
func configServer(t *testing.T, be *userFakeBackend, body string) (*Server, http.Handler, *config.ServerConfig) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.yaml")
	yaml := "name: test\ndefault_backend: firecracker\nfirecracker:\n  instance_dir: /tmp/ti\n" + body
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadServerConfigForCLI(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var list map[string]config.UserConfig
	if cfg.UsersConfigured() {
		list = cfg.Auth.Users
	}
	lookup, err := users.NewConfigLookup(users.ConfigOptions{
		SSH: cfg.EffectiveSSHAuth(), GitHubCacheDir: t.TempDir(), Users: list,
	})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	reg := plugin.NewRegistry()
	s := NewServer(be, cfg, "", reg, plugin.NewBridge(reg, cfg.UsersConfigured()))
	s.SetUserLookup(lookup)
	pinBus(s)
	return s, s.Router(), cfg
}

// takenNameBackend answers a create for a name in use the way a backend may,
// with more in the message than the name: the guard must say no more than
// that the name is taken.
type takenNameBackend struct{ *userFakeBackend }

func (b takenNameBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	b.mu.Lock()
	s, taken := b.sheds[req.Name]
	b.mu.Unlock()
	if taken {
		return nil, fmt.Errorf("%w: %s (user %s, status %s)", config.ErrShedAlreadyExistsSentinel, req.Name, s.User, s.Status)
	}
	return b.userFakeBackend.CreateShed(ctx, req)
}

// twoUsersAndAnAdmin is the user list of the tests: alice and bob, users who
// are not admins (bob also has a script key, and an expired one), and root,
// an admin.
type twoUsersAndAnAdmin struct {
	alice, bob, bobScript, bobExpired, root, stranger keyPair
}

func newTwoUsersAndAnAdmin(t *testing.T) twoUsersAndAnAdmin {
	return twoUsersAndAnAdmin{
		alice: newKeyPair(t), bob: newKeyPair(t), bobScript: newKeyPair(t), bobExpired: newKeyPair(t),
		root: newKeyPair(t), stranger: newKeyPair(t),
	}
}

func (k twoUsersAndAnAdmin) yaml() string {
	return "auth:\n  mode: mtls\n  users:\n" +
		"    alice:\n      keys:\n        - {key: \"" + k.alice.line + "\", label: laptop}\n" +
		"    bob:\n      keys:\n" +
		"        - {key: \"" + k.bob.line + "\", label: laptop}\n" +
		"        - {key: \"" + k.bobScript.line + "\", label: ci, script: true}\n" +
		"        - {key: \"" + k.bobExpired.line + "\", label: old, expires: \"2020-01-01\"}\n" +
		"    root:\n      admin: true\n      keys:\n        - {key: \"" + k.root.line + "\", label: desk}\n"
}

// TestConfigUsersAtTheAPIDoor: two users and an admin from a real config. Each
// user reaches their own shed and is told the other's does not exist; the
// admin lists both, stops and deletes the other's, and is refused entry; a
// create is stamped with its caller's user; a create under a taken name says
// only that it is taken; an unknown or expired key is refused.
func TestConfigUsersAtTheAPIDoor(t *testing.T) {
	k := newTwoUsersAndAnAdmin(t)
	inner := newUserFakeBackend()
	inner.sheds["crate"] = config.Shed{Name: "crate", User: "bob", Status: config.StatusRunning, CreatedAt: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)}
	_, router, _ := configServer(t, inner, k.yaml())

	list := func(fp string) []string {
		t.Helper()
		r := doAs(router, fp, http.MethodGet, "/api/sheds", "")
		if r.code != http.StatusOK {
			t.Fatalf("list = %d %s", r.code, r.body)
		}
		var resp config.ShedsResponse
		if err := json.Unmarshal(r.body, &resp); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, sh := range resp.Sheds {
			names = append(names, sh.Name)
		}
		return names
	}
	if got := strings.Join(list(k.alice.fp), ","); got != "box" {
		t.Errorf("alice lists %q, want box", got)
	}
	if got := strings.Join(list(k.bob.fp), ","); got != "crate" {
		t.Errorf("bob lists %q, want crate", got)
	}
	if got := list(k.root.fp); len(got) != 2 {
		t.Errorf("root lists %v, want both", got)
	}

	// Each user, on the other's shed: not found, and nothing reached.
	for _, tc := range []struct{ who, fp, other string }{{"alice", k.alice.fp, "crate"}, {"bob", k.bob.fp, "box"}} {
		for _, rt := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/sheds/" + tc.other, ""},
			{http.MethodPost, "/api/sheds/" + tc.other + "/start", ""},
			{http.MethodPost, "/api/sheds/" + tc.other + "/stop", ""},
			{http.MethodPost, "/api/sheds/" + tc.other + "/exec", `{"argv":["true"]}`},
			{http.MethodGet, "/api/sheds/" + tc.other + "/sessions", ""},
			{http.MethodDelete, "/api/sheds/" + tc.other, ""},
		} {
			inner.reset()
			r := doAs(router, tc.fp, rt.method, rt.path, rt.body)
			if r.code != http.StatusNotFound || r.errCode(t) != config.ErrShedNotFound {
				t.Errorf("%s %s %s = %d %s, want 404 SHED_NOT_FOUND", tc.who, rt.method, rt.path, r.code, r.body)
			}
			if got := inner.actedSnapshot(); len(got) != 0 {
				t.Errorf("%s %s %s reached the backend: %v", tc.who, rt.method, rt.path, got)
			}
		}
		// Their own shed works.
		own := map[string]string{"alice": "box", "bob": "crate"}[tc.who]
		if r := doAs(router, tc.fp, http.MethodPost, "/api/sheds/"+own+"/exec", `{"argv":["true"]}`); r.code != http.StatusOK {
			t.Errorf("%s exec in own shed = %d %s", tc.who, r.code, r.body)
		}
	}

	// The admin may enter neither, and may stop and delete alice's.
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/sheds/box/start", ""},
		{http.MethodPost, "/api/sheds/box/exec", `{"argv":["true"]}`},
		{http.MethodGet, "/api/sheds/box/sessions", ""},
	} {
		inner.reset()
		r := doAs(router, k.root.fp, rt.method, rt.path, rt.body)
		if r.code != http.StatusForbidden || r.errCode(t) != config.ErrForbidden {
			t.Errorf("root %s %s = %d %s, want 403", rt.method, rt.path, r.code, r.body)
		}
		if got := inner.actedSnapshot(); len(got) != 0 {
			t.Errorf("root %s %s entered alice's shed: %v", rt.method, rt.path, got)
		}
	}
	inner.reset()
	if r := doAs(router, k.root.fp, http.MethodPost, "/api/sheds/box/stop", ""); r.code != http.StatusOK {
		t.Errorf("root stop alice's shed = %d %s", r.code, r.body)
	}
	if r := doAs(router, k.root.fp, http.MethodDelete, "/api/sheds/box", ""); r.code != http.StatusNoContent {
		t.Errorf("root delete alice's shed = %d %s", r.code, r.body)
	}
	if got := strings.Join(inner.actedSnapshot(), ","); got != "stop box,delete box" {
		t.Errorf("root's stop and delete reached the backend as %q", got)
	}

	// A create is stamped with its caller's user, never the request's.
	inner.reset()
	if r := doAs(router, k.bob.fp, http.MethodPost, "/api/sheds", `{"name":"fresh","user":"alice","owner":"nocturn"}`); r.code != http.StatusCreated {
		t.Fatalf("bob create = %d %s", r.code, r.body)
	}
	if len(inner.created) != 1 {
		t.Fatalf("creates reached the backend: %d, want 1", len(inner.created))
	}
	if req := inner.created[0]; req.User != "bob" || req.Owner != "" {
		t.Errorf("bob's create reached the backend as user %q owner %q", req.User, req.Owner)
	}

	// Unknown and expired keys are nobody.
	for name, fp := range map[string]string{"stranger": k.stranger.fp, "bob's expired key": k.bobExpired.fp} {
		inner.reset()
		if r := doAs(router, fp, http.MethodGet, "/api/sheds", ""); r.code != http.StatusUnauthorized {
			t.Errorf("%s list = %d %s, want 401", name, r.code, r.body)
		}
	}
}

// TestConfigUsersTakenName: a create under a name another user holds is the
// usual 409, worded the same as for one's own name, and says nothing of whose
// shed it is or what it is like.
func TestConfigUsersTakenName(t *testing.T) {
	k := newTwoUsersAndAnAdmin(t)
	inner := newUserFakeBackend()
	s, _, _ := configServer(t, inner, k.yaml())
	router := NewServer(takenNameBackend{inner}, s.cfg, "", s.plugins, s.bridge)
	router.SetUserLookup(s.users)
	h := router.Router()

	theirs := doAs(h, k.bob.fp, http.MethodPost, "/api/sheds", `{"name":"box"}`)
	mine := doAs(h, k.alice.fp, http.MethodPost, "/api/sheds", `{"name":"box"}`)
	for who, r := range map[string]apiResp{"bob (alice's name)": theirs, "alice (her own name)": mine} {
		if r.code != http.StatusConflict || r.errCode(t) != config.ErrShedAlreadyExists {
			t.Errorf("%s create = %d %s, want 409 SHED_ALREADY_EXISTS", who, r.code, r.body)
		}
	}
	if theirs.errMessage(t) != mine.errMessage(t) {
		t.Errorf("the conflict differs by whose the name is: %q vs %q", theirs.errMessage(t), mine.errMessage(t))
	}
	if msg := theirs.errMessage(t); strings.Contains(msg, "alice") || strings.Contains(msg, "running") {
		t.Errorf("the conflict reveals the shed: %q", msg)
	}
}

// TestConfigUsersScriptKey: a script key is its user for everything the
// access rule decides, but may neither subscribe to nor answer on the
// credential bus, even with a credentials certificate (one issued before the
// key was marked, say). The same user's other key may.
func TestConfigUsersScriptKey(t *testing.T) {
	k := newTwoUsersAndAnAdmin(t)
	inner := newUserFakeBackend()
	inner.sheds["crate"] = config.Shed{Name: "crate", User: "bob", Status: config.StatusRunning, CreatedAt: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)}
	s, router, _ := configServer(t, inner, k.yaml())

	if r := doAs(router, k.bobScript.fp, http.MethodPost, "/api/sheds/crate/exec", `{"argv":["true"]}`); r.code != http.StatusOK {
		t.Errorf("bob's script key exec in bob's shed = %d %s", r.code, r.body)
	}
	if r := doAs(router, k.bobScript.fp, http.MethodGet, "/api/sheds/box", ""); r.code != http.StatusNotFound {
		t.Errorf("bob's script key on alice's shed = %d %s", r.code, r.body)
	}

	box := &sendCounter{}
	gen := s.bridge.RegisterShed("crate", &plugin.ShedConn{User: "bob", CreatedAt: inner.sheds["crate"].CreatedAt, Send: box.send})
	l := listen(t, s, "bob", "ssh-agent", k.bob.fp)
	id := publishFromShed(t, s, "crate", gen)

	r := do(router, credsAs(k.bobScript.fp), http.MethodPost, "/api/plugins/listeners/ssh-agent/respond", respondBody("crate", id))
	if r.code != http.StatusForbidden || !strings.Contains(r.errMessage(t), "script key") {
		t.Errorf("script key respond = %d %s, want 403 naming the script key", r.code, r.body)
	}
	if box.count() != 0 {
		t.Error("a script key's response reached the shed")
	}
	s.plugins.Unregister(l.Key(), l.Token)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	sub := func(fp string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/plugins/listeners/ssh-agent/messages", nil).WithContext(ctx)
		withClientCert(req, testClientCert(fp, authtoken.ScopeCredentials, time.Hour))
		withTestMachine(req, k.bob.fp) // bob's machine, whichever of his keys
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := sub(k.bobScript.fp); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "script key") {
		t.Errorf("script key subscribe = %d %s, want 403 naming the script key", rec.Code, rec.Body.String())
	}
	if rec := sub(k.bob.fp); rec.Code != http.StatusOK {
		t.Errorf("bob's own key subscribe = %d %s, want 200", rec.Code, rec.Body.String())
	}

	// The script flag reaches the principal the guard and later checks read.
	caller := userCaller(users.User{Name: "bob"}, users.Key{Script: true}, k.bobScript.fp, "")
	if p, ok := caller.principal(); !ok || !p.Script || p.User != "bob" {
		t.Errorf("script caller principal %+v %v", p, ok)
	}
}

// TestNoUserListUnchanged: with no auth.users, every accepted key is the
// implicit user, who reaches every shed whatever user its record carries
// (alice's "box" here), as before users existed.
func TestNoUserListUnchanged(t *testing.T) {
	key := newKeyPair(t)
	inner := newUserFakeBackend()
	_, router, cfg := configServer(t, inner, "auth:\n  mode: mtls\n  ssh:\n    authorized_keys: [\""+key.line+"\"]\n")
	if cfg.UsersConfigured() {
		t.Fatal("a config with no auth.users has a user list")
	}
	for _, rt := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/sheds/box", "", http.StatusOK},
		{http.MethodPost, "/api/sheds/box/exec", `{"argv":["true"]}`, http.StatusOK},
		{http.MethodPost, "/api/sheds/box/start", "", http.StatusOK},
		{http.MethodPost, "/api/images/prune", "", http.StatusOK},
		{http.MethodPost, "/api/sheds", `{"name":"fresh","local_dir":"` + t.TempDir() + `"}`, http.StatusCreated},
	} {
		if r := doAs(router, key.fp, rt.method, rt.path, rt.body); r.code != rt.want {
			t.Errorf("implicit user %s %s = %d %s, want %d", rt.method, rt.path, r.code, r.body, rt.want)
		}
	}
	if len(inner.created) != 1 {
		t.Fatalf("creates reached the backend: %d, want 1", len(inner.created))
	}
	if req := inner.created[0]; req.User != "" || req.Owner != "" {
		t.Errorf("the implicit user's create was stamped user %q owner %q", req.User, req.Owner)
	}
}

// TestConfigUsersKeyExpiresWhileConnected: a hand-listed key from a real
// config expires while its user's API connections stay open, over real TLS
// with the listener's key check wired to the lookup as shed-server wires it.
// After expiry a NEW request on the open (pooled) connection is refused with
// 401 and never dispatched, and so is a new connection; the event stream
// opened before expiry keeps streaming.
func TestConfigUsersKeyExpiresWhileConnected(t *testing.T) {
	alice, root := newKeyPair(t), newKeyPair(t)
	// RFC 3339 holds whole seconds: between one and two seconds from now.
	expires := time.Now().Add(2 * time.Second).UTC().Truncate(time.Second)
	s, _, _ := configServer(t, newUserFakeBackend(), "auth:\n  mode: mtls\n  users:\n"+
		"    alice:\n      keys:\n        - {key: \""+alice.line+"\", label: laptop, expires: \""+expires.Format(time.RFC3339)+"\"}\n"+
		"    root:\n      admin: true\n      keys:\n        - {key: \""+root.line+"\", label: desk}\n")

	tick, done := make(chan struct{}), make(chan struct{})
	var hits atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/api/egress/stream" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		for {
			select {
			case <-done:
				return
			case <-r.Context().Done():
				return
			case <-tick:
				_, _ = io.WriteString(w, "data: still-here\n\n")
				flusher.Flush()
			}
		}
	})
	ca := testCA(t)
	srv := httptest.NewUnstartedServer(s.authMiddleware(handler))
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		ClientCAs:  ca.Pool(),
		ClientAuth: tls.RequireAndVerifyClientCert,
		VerifyConnection: servertls.AllowlistConnectionVerifier(func(fp string) bool {
			return users.HasKey(s.users, fp)
		}),
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	cert := issueClientCert(t, ca, alice.fp, authtoken.ScopeControl, time.Hour)
	newClient := func() *http.Client {
		pool := x509.NewCertPool()
		pool.AddCert(srv.Certificate())
		return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: pool, MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert},
		}}}
	}

	stream, err := newClient().Get(srv.URL + "/api/egress/stream")
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { close(done); _ = stream.Body.Close() }()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("open stream = %d, want 200", stream.StatusCode)
	}
	pooled := newClient()
	if status, _, err := reusedGet(t, pooled, srv.URL+"/api/sheds"); err != nil || status != http.StatusOK {
		t.Fatalf("request before expiry: %d %v, want 200", status, err)
	}

	time.Sleep(time.Until(expires) + 100*time.Millisecond)
	before := hits.Load()

	status, reused, err := reusedGet(t, pooled, srv.URL+"/api/sheds")
	if err != nil {
		t.Fatalf("request after expiry on the open connection errored instead of a 401: %v", err)
	}
	if !reused {
		t.Fatal("the request after expiry opened a new connection; the test proves nothing about an open one")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("request after expiry on the open connection = %d, want 401", status)
	}
	if status, _, err := reusedGet(t, newClient(), srv.URL+"/api/sheds"); err == nil && status == http.StatusOK {
		t.Error("a new connection after expiry was served")
	}
	if got := hits.Load(); got != before {
		t.Errorf("a request after expiry was dispatched (hits %d -> %d)", before, got)
	}

	tick <- struct{}{}
	buf := make([]byte, 64)
	n, err := stream.Body.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "still-here") {
		t.Errorf("the stream opened before expiry stopped: %q %v", buf[:n], err)
	}
}
