package sshd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
	"github.com/charliek/shed/internal/users/userstest"
)

// routeBackend holds two running sheds of alice's, "box" and "git" (a shed
// that predates the reservation), and records the shed name of every call
// that reaches it. Its lookup ignores case, as a shed directory on a
// case-insensitive volume (VZ on a default macOS volume) does: asked for
// "Git" it finds the shed stored as "git".
type routeBackend struct {
	backend.Backend
	mu    sync.Mutex
	calls []string
}

func (b *routeBackend) record(call, name string) {
	b.mu.Lock()
	b.calls = append(b.calls, call+" "+name)
	b.mu.Unlock()
}

func (b *routeBackend) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

func (b *routeBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	b.record("GetShed", name)
	for _, stored := range []string{"box", config.ReservedShedName} {
		if strings.EqualFold(stored, name) {
			return &config.Shed{Name: stored, User: "alice", Status: config.StatusRunning, CreatedAt: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}, nil
		}
	}
	return nil, config.ErrShedNotFoundSentinel
}

func (b *routeBackend) Exec(_ context.Context, name string, _ backend.ExecOptions) error {
	b.record("Exec", name)
	return nil
}

func (b *routeBackend) DialService(_ context.Context, name string, _ uint16) (net.Conn, error) {
	b.record("DialService", name)
	server, client := net.Pipe()
	go func() { _, _ = io.Copy(server, server) }()
	return client, nil
}

// onlyBox fails the test unless every call the backend saw was for box and
// it saw at least one.
func onlyBox(t *testing.T, calls []string) {
	t.Helper()
	if len(calls) == 0 {
		t.Fatal("no call reached the backend")
	}
	for _, c := range calls {
		if !strings.HasSuffix(c, " box") {
			t.Errorf("backend call %q, want only calls for box (all: %v)", c, calls)
		}
	}
}

// routeMode is one way the SSH server decides keys: every row admits the
// key `listed` (as the implicit user, or as alice from a user list).
type routeMode struct {
	name string
	auth func(listedFP string) KeyAuth
}

func routeModes() []routeMode {
	implicit := func(mode string) func(string) KeyAuth {
		return func(fp string) KeyAuth { return KeyAuth{Mode: mode, Users: userstest.Implicit(fp)} }
	}
	userList := func(mode string) func(string) KeyAuth {
		return func(fp string) KeyAuth {
			f := userstest.New()
			f.AddUser(users.User{Name: "alice"})
			f.AddKey("alice", users.Key{Fingerprint: fp, Source: users.SourceInline})
			return KeyAuth{Mode: mode, Users: f}
		}
	}
	return []routeMode{
		{"zero value, no lookup", func(string) KeyAuth { return KeyAuth{} }},
		{"off", implicit(config.SSHAuthOff)},
		{"warn", implicit(config.SSHAuthWarn)},
		{"enforce", implicit(config.SSHAuthEnforce)},
		{"off with a user list", userList(config.SSHAuthOff)},
		{"warn with a user list", userList(config.SSHAuthWarn)},
		{"enforce with a user list", userList(config.SSHAuthEnforce)},
	}
}

func routeServer(t *testing.T, be backend.Backend, auth KeyAuth) (*Server, string) {
	t.Helper()
	s, err := NewServer(be, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, auth)
	if err != nil {
		t.Fatal(err)
	}
	return s, serveForTest(t, s)
}

// TestUsernameSwitchToGitIsRefused is the username switch on the wire: on one
// connection, an unsigned query as gitx with a key the server accepts, then a
// request signed as git with the same key. The connection must not
// authenticate as git, in any mode (gliderlabs keeps gitx, the first
// username, as ctx.User(), so a check of ctx.User() alone lets it through).
// The connection then signs as box and every channel addresses box: never
// git, and never gitx, the username it first asked as.
func TestUsernameSwitchToGitIsRefused(t *testing.T) {
	for _, m := range routeModes() {
		t.Run(m.name, func(t *testing.T) {
			listed := testSigner(t)
			be := &routeBackend{}
			_, addr := routeServer(t, be, m.auth(gossh.FingerprintSHA256(listed.PublicKey())))

			c := dialRaw(t, addr)
			if !c.query("gitx", listed.PublicKey()) {
				t.Fatal("the key was not accepted as gitx (the precondition of the switch)")
			}
			if c.login(config.ReservedShedName, listed) {
				t.Fatal("a login signed as git after a query as gitx authenticated the connection")
			}
			if n := len(be.seen()); n != 0 {
				t.Errorf("the refused login reached the backend: %v", be.seen())
			}

			// Control: the connection and the key are otherwise good.
			if !c.login("box", listed) {
				t.Fatal("a login signed as box was refused")
			}
			if stderr, status := c.run("exec", "true"); status != 0 {
				t.Errorf("exec: status %d, stderr %q", status, stderr)
			}
			if stderr, status := c.run("subsystem", "sftp"); status != 0 {
				t.Errorf("sftp: status %d, stderr %q", status, stderr)
			}
			if ok, reason := c.forward(8080); !ok {
				t.Errorf("forward refused: %s", reason)
			}
			onlyBox(t, be.seen())
		})
	}
}

// TestChannelsRouteByTheAuthenticatedUsername: a connection that first asked
// as gitx (an ordinary name) and signed as box addresses box on every channel
// type. The port forward once routed by the first-seen username (gitx) while
// sessions routed by the final one, so one connection could address two
// sheds depending on the channel type.
func TestChannelsRouteByTheAuthenticatedUsername(t *testing.T) {
	listed := testSigner(t)
	be := &routeBackend{}
	_, addr := routeServer(t, be, KeyAuth{Mode: config.SSHAuthEnforce, Users: userstest.Implicit(gossh.FingerprintSHA256(listed.PublicKey()))})

	c := dialRaw(t, addr)
	if !c.query("gitx", listed.PublicKey()) {
		t.Fatal("the key was not accepted as gitx")
	}
	if !c.login("box", listed) {
		t.Fatal("a login signed as box was refused")
	}
	if ok, reason := c.forward(8080); !ok {
		t.Errorf("forward refused: %s", reason)
	}
	if stderr, status := c.run("exec", "true"); status != 0 {
		t.Errorf("exec: status %d, stderr %q", status, stderr)
	}
	if stderr, status := c.run("subsystem", "sftp"); status != 0 {
		t.Errorf("sftp: status %d, stderr %q", status, stderr)
	}
	onlyBox(t, be.seen())
}

// TestUsernameSwitchToGitNeverRefetches: handlePublicKey decides on the
// username of the request in hand, not the connection's first one. An
// unknown key asked about as gitx makes the lookup fetch GitHub keys (an
// ordinary name may); the same key then signed as git on that connection is
// refused before any lookup or fetch.
func TestUsernameSwitchToGitNeverRefetches(t *testing.T) {
	newcomer := testSigner(t)
	lookup := &refetchLookup{Fake: userstest.Implicit()} // a fetch finds nothing
	_, addr := routeServer(t, &routeBackend{}, KeyAuth{Mode: config.SSHAuthEnforce, Users: lookup})

	c := dialRaw(t, addr)
	if c.query("gitx", newcomer.PublicKey()) {
		t.Fatal("an unknown key was accepted in enforce mode")
	}
	if n := len(lookup.seen()); n != 1 {
		t.Fatalf("a query as gitx fetched %d times, want 1", n)
	}
	if c.login(config.ReservedShedName, newcomer) {
		t.Fatal("a login signed as git authenticated")
	}
	if n := len(lookup.seen()); n != 1 {
		t.Errorf("a login signed as git fetched GitHub keys (%d fetches in all, want 1)", n)
	}
}

// fakeConnMeta is the connection metadata x/crypto hands an auth callback.
type fakeConnMeta struct{ user string }

func (m fakeConnMeta) User() string          { return m.user }
func (m fakeConnMeta) SessionID() []byte     { return []byte("session") }
func (m fakeConnMeta) ClientVersion() []byte { return []byte("SSH-2.0-test") }
func (m fakeConnMeta) ServerVersion() []byte { return []byte("SSH-2.0-test") }
func (m fakeConnMeta) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
}
func (m fakeConnMeta) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2222}
}

// fakeSSHContext is a gliderlabs connection context whose User() is the
// connection's first-seen username, as gliderlabs records it.
type fakeSSHContext struct {
	context.Context
	sync.Mutex
	firstUser string
	mu        sync.Mutex
	values    map[any]any
}

func newFakeSSHContext(t *testing.T, firstUser string) *fakeSSHContext {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &fakeSSHContext{Context: ctx, firstUser: firstUser, values: map[any]any{}}
}

func (c *fakeSSHContext) Value(key any) any {
	c.mu.Lock()
	v, ok := c.values[key]
	c.mu.Unlock()
	if ok {
		return v
	}
	return c.Context.Value(key)
}

func (c *fakeSSHContext) SetValue(key, value any) {
	c.mu.Lock()
	c.values[key] = value
	c.mu.Unlock()
}

func (c *fakeSSHContext) User() string          { return c.firstUser }
func (c *fakeSSHContext) SessionID() string     { return "session" }
func (c *fakeSSHContext) ClientVersion() string { return "SSH-2.0-test" }
func (c *fakeSSHContext) ServerVersion() string { return "SSH-2.0-test" }
func (c *fakeSSHContext) RemoteAddr() net.Addr  { return fakeConnMeta{}.RemoteAddr() }
func (c *fakeSSHContext) LocalAddr() net.Addr   { return fakeConnMeta{}.LocalAddr() }
func (c *fakeSSHContext) Permissions() *ssh.Permissions {
	return &ssh.Permissions{Permissions: &gossh.Permissions{}}
}

// TestVerifiedKeyRefusesGit is the verified-key callback alone, given the
// metadata of a request signed as git on a connection whose first request
// was as gitx: it refuses, and records no identity. As gitx it records the
// key and that username.
func TestVerifiedKeyRefusesGit(t *testing.T) {
	for _, m := range routeModes() {
		t.Run(m.name, func(t *testing.T) {
			listed := testSigner(t)
			s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, m.auth(gossh.FingerprintSHA256(listed.PublicKey())))
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				user   string
				refuse bool
			}{
				{config.ReservedShedName, true},
				{"gitx", false},
			} {
				ctx := newFakeSSHContext(t, "gitx")
				cfg := s.sshServer.ServerConfigCallback(ctx)
				_, err := cfg.VerifiedPublicKeyCallback(fakeConnMeta{user: tc.user}, listed.PublicKey(), &gossh.Permissions{}, gossh.KeyAlgoED25519)
				if got := err != nil; got != tc.refuse {
					t.Errorf("%s: refused = %v (%v), want %v", tc.user, got, err, tc.refuse)
				}
				id, ok := connIdentityFrom(ctx)
				if tc.refuse && ok {
					t.Errorf("%s: an identity was recorded for a refused login", tc.user)
				}
				if !tc.refuse && (!ok || id.user != tc.user) {
					t.Errorf("%s: identity = %+v (%v), want the key as %s", tc.user, id, ok, tc.user)
				}
			}
		})
	}
}

// fakeNewChannel is a direct-tcpip channel open request.
type fakeNewChannel struct {
	extra    []byte
	rejected string
	accepted bool
}

func (c *fakeNewChannel) Accept() (gossh.Channel, <-chan *gossh.Request, error) {
	c.accepted = true
	return nil, nil, io.ErrClosedPipe
}

func (c *fakeNewChannel) Reject(_ gossh.RejectionReason, msg string) error {
	c.rejected = msg
	return nil
}

func (c *fakeNewChannel) ChannelType() string { return "direct-tcpip" }
func (c *fakeNewChannel) ExtraData() []byte   { return c.extra }

// fakeSession is a gliderlabs session channel: only what the session and
// sftp handlers use on their way to a shed.
type fakeSession struct {
	ssh.Session
	ctx    *fakeSSHContext
	user   string
	shell  bool // an interactive shell: no command, and a PTY
	stderr bytes.Buffer
	exit   int
}

func (s *fakeSession) User() string          { return s.user }
func (s *fakeSession) RemoteAddr() net.Addr  { return s.ctx.RemoteAddr() }
func (s *fakeSession) Context() ssh.Context  { return s.ctx }
func (s *fakeSession) Stderr() io.ReadWriter { return &s.stderr }
func (s *fakeSession) Exit(code int) error   { s.exit = code; return nil }
func (s *fakeSession) RawCommand() string {
	if s.shell {
		return ""
	}
	return "true"
}
func (s *fakeSession) Pty() (ssh.Pty, <-chan ssh.Window, bool) {
	if s.shell {
		return ssh.Pty{Term: "xterm", Window: ssh.Window{Width: 80, Height: 24}}, make(chan ssh.Window), true
	}
	return ssh.Pty{}, nil, false
}
func (s *fakeSession) Write(p []byte) (int, error) { return len(p), nil }
func (s *fakeSession) Read([]byte) (int, error)    { return 0, io.EOF }

// TestChannelsRefuseANameThatIsNotAShed is each channel handler alone, given
// a connection whose first-seen username (ctx.User()) is gitx and whose
// authenticated username is another: git, a case variant of git or of box,
// or a special name. Each is refused before any backend call, with the
// answer an unknown shed gets; box, the control, reaches box.
func TestChannelsRefuseANameThatIsNotAShed(t *testing.T) {
	for _, m := range routeModes() {
		t.Run(m.name, func(t *testing.T) {
			listed := testSigner(t)
			be := &routeBackend{}
			s, err := NewServer(be, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, m.auth(gossh.FingerprintSHA256(listed.PublicKey())))
			if err != nil {
				t.Fatal(err)
			}
			conn := func(user string) *fakeSSHContext {
				ctx := newFakeSSHContext(t, "gitx")
				id, err := s.newConnIdentity(listed.PublicKey(), user)
				if err != nil {
					t.Fatal(err)
				}
				setConnIdentity(ctx, id)
				return ctx
			}
			forward := (&wire{}).s("localhost").u32(8080).s("127.0.0.1").u32(40000).b

			for _, user := range []string{config.ReservedShedName, "Git", "GIT", "gIt", "BOX", "Box", "_api", "_bootstrap", ""} {
				ch := &fakeNewChannel{extra: forward}
				s.handleDirectTCPIP(nil, nil, ch, conn(user))
				if ch.rejected != "shed not found" || ch.accepted {
					t.Errorf("forward as %q: rejected %q, accepted %v; want the not-found refusal", user, ch.rejected, ch.accepted)
				}
				want := "Error: shed '" + user + "' not found\n"
				sftp := &fakeSession{ctx: conn(user), user: user}
				s.handleSFTP(sftp)
				if got := sftp.stderr.String(); got != want || sftp.exit != 1 {
					t.Errorf("sftp as %q: stderr %q, exit %d; want %q, 1", user, got, sftp.exit, want)
				}
				if user == "_api" || user == "_bootstrap" {
					continue // the session handles these by name (below)
				}
				sess := &fakeSession{ctx: conn(user), user: user}
				s.handleSession(sess)
				if got := sess.stderr.String(); got != want || sess.exit != 1 {
					t.Errorf("session as %q: stderr %q, exit %d; want %q, 1", user, got, sess.exit, want)
				}
			}
			if calls := be.seen(); len(calls) != 0 {
				t.Errorf("names that are not sheds reached the backend: %v", calls)
			}

			// The session's special names are unchanged.
			api := &fakeSession{ctx: conn("_api"), user: "_api"}
			s.handleSession(api)
			if !strings.Contains(api.stderr.String(), "reserved for API access") || api.exit != 1 {
				t.Errorf("_api session: stderr %q, exit %d", api.stderr.String(), api.exit)
			}
			boot := &fakeSession{ctx: conn("_bootstrap"), user: "_bootstrap"}
			s.handleSession(boot)
			if !strings.HasPrefix(boot.stderr.String(), "bootstrap") || boot.exit != 1 {
				t.Errorf("_bootstrap session: stderr %q, exit %d; want the bootstrap handler's refusal", boot.stderr.String(), boot.exit)
			}
			if calls := be.seen(); len(calls) != 0 {
				t.Errorf("a special name reached the backend: %v", calls)
			}

			// Control: box reaches box on every channel type.
			ch := &fakeNewChannel{extra: forward}
			s.handleDirectTCPIP(nil, nil, ch, conn("box"))
			if ch.rejected != "" {
				t.Errorf("forward as box rejected: %q", ch.rejected)
			}
			for _, h := range []func(ssh.Session){s.handleSession, s.handleSFTP} {
				sess := &fakeSession{ctx: conn("box"), user: "box"}
				h(sess)
				if sess.exit != 0 {
					t.Errorf("as box: exit %d, stderr %q", sess.exit, sess.stderr.String())
				}
			}
			onlyBox(t, be.seen())
		})
	}
}

// TestCaseVariantsReachNoShed is the case-variant route on the wire, with an
// ordinary client: Git, GIT and gIt (git, a shed that predates the
// reservation, is stored as "git" on a case-insensitive volume), and BOX (an
// ordinary shed's name in capitals) are answered as an unknown shed is on
// exec, sftp and a port forward, before any backend call. box itself, and
// the special names, are unchanged.
func TestCaseVariantsReachNoShed(t *testing.T) {
	for _, m := range routeModes() {
		t.Run(m.name, func(t *testing.T) {
			listed := testSigner(t)
			be := &routeBackend{}
			_, addr := routeServer(t, be, m.auth(gossh.FingerprintSHA256(listed.PublicKey())))

			for _, user := range []string{"Git", "GIT", "gIt", "BOX"} {
				client := dialWith(t, addr, user, listed)
				want := "Error: shed '" + user + "' not found\n"
				if stderr, status := runOn(t, client, "true", ""); stderr != want || status != 1 {
					t.Errorf("exec as %s: stderr %q, status %d; want %q, 1", user, stderr, status, want)
				}
				if stderr, status := runOn(t, client, "", "sftp"); stderr != want || status != 1 {
					t.Errorf("sftp as %s: stderr %q, status %d; want %q, 1", user, stderr, status, want)
				}
				if c, err := client.Dial("tcp", "localhost:8080"); err == nil {
					_ = c.Close()
					t.Errorf("forward as %s was accepted", user)
				} else if !strings.Contains(err.Error(), "shed not found") {
					t.Errorf("forward as %s: %v, want the not-found refusal", user, err)
				}
			}
			if calls := be.seen(); len(calls) != 0 {
				t.Errorf("a case variant reached the backend: %v", calls)
			}

			// _api and _bootstrap are as they were.
			api := dialWith(t, addr, "_api", listed)
			if stderr, status := runOn(t, api, "true", ""); !strings.Contains(stderr, "reserved for API access") || status != 1 {
				t.Errorf("_api: stderr %q, status %d", stderr, status)
			}
			boot := dialWith(t, addr, "_bootstrap", listed)
			if stderr, status := runOn(t, boot, "control cli", ""); !strings.HasPrefix(stderr, "bootstrap") || status != 1 {
				t.Errorf("_bootstrap: stderr %q, status %d; want the bootstrap handler's refusal", stderr, status)
			}
			if calls := be.seen(); len(calls) != 0 {
				t.Errorf("a special name reached the backend: %v", calls)
			}

			// Control: box itself.
			client := dialWith(t, addr, "box", listed)
			if stderr, status := runOn(t, client, "true", ""); status != 0 {
				t.Errorf("exec as box: stderr %q, status %d", stderr, status)
			}
			if stderr, status := runOn(t, client, "", "sftp"); status != 0 {
				t.Errorf("sftp as box: stderr %q, status %d", stderr, status)
			}
			c, err := client.Dial("tcp", "localhost:8080")
			if err != nil {
				t.Errorf("forward as box: %v", err)
			} else {
				_ = c.Close()
			}
			onlyBox(t, be.seen())
		})
	}
}

// TestChannelsDeclineAConnectionWithNoIdentity is each channel handler alone
// on a connection with no authenticated key on record (no
// VerifiedPublicKeyCallback ran for it): a shell, a command, sftp and a port
// forward are each refused with errNoIdentity before any backend call, even
// though the connection's first-seen username (ctx.User(), and so
// sess.User()) is box, a shed the key could enter, or _bootstrap.
func TestChannelsDeclineAConnectionWithNoIdentity(t *testing.T) {
	for _, m := range routeModes() {
		t.Run(m.name, func(t *testing.T) {
			listed := testSigner(t)
			be := &routeBackend{}
			s, err := NewServer(be, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, m.auth(gossh.FingerprintSHA256(listed.PublicKey())))
			if err != nil {
				t.Fatal(err)
			}
			want := "Error: " + errNoIdentity.Error() + "\n"
			forward := (&wire{}).s("localhost").u32(8080).s("127.0.0.1").u32(40000).b
			for _, first := range []string{"box", reservedBootstrapUser} {
				for _, tc := range []struct {
					name string
					run  func(ssh.Session)
					sess *fakeSession
				}{
					{"shell", s.handleSession, &fakeSession{shell: true}},
					{"exec", s.handleSession, &fakeSession{}},
					{"sftp", s.handleSFTP, &fakeSession{}},
				} {
					tc.sess.ctx, tc.sess.user = newFakeSSHContext(t, first), first
					tc.run(tc.sess)
					if got := tc.sess.stderr.String(); got != want || tc.sess.exit != 1 {
						t.Errorf("%s, first seen as %s: stderr %q, exit %d; want %q, 1", tc.name, first, got, tc.sess.exit, want)
					}
				}
				ch := &fakeNewChannel{extra: forward}
				s.handleDirectTCPIP(nil, nil, ch, newFakeSSHContext(t, first))
				if ch.rejected != errNoIdentity.Error() || ch.accepted {
					t.Errorf("forward, first seen as %s: rejected %q, accepted %v; want %q", first, ch.rejected, ch.accepted, errNoIdentity.Error())
				}
			}
			if calls := be.seen(); len(calls) != 0 {
				t.Errorf("a connection with no identity reached the backend: %v", calls)
			}
		})
	}
}

// TestUsernameSwitchIntoAndOutOfSpecialNames is the username switch on the
// wire with the special names: a query as one name, then a login signed as
// another, on one connection. Every channel follows the name that was
// signed for, in both directions: into _bootstrap or _api from box (the
// bootstrap handler's or the API refusal; never box), and out of them into
// box (box, as an ordinary login reaches it).
func TestUsernameSwitchIntoAndOutOfSpecialNames(t *testing.T) {
	for _, m := range routeModes() {
		t.Run(m.name, func(t *testing.T) {
			listed := testSigner(t)
			be := &routeBackend{}
			_, addr := routeServer(t, be, m.auth(gossh.FingerprintSHA256(listed.PublicKey())))

			// Into a special name: asked as box, signed as _bootstrap or _api.
			for _, special := range []string{reservedBootstrapUser, reservedAPIUser} {
				c := dialRaw(t, addr)
				if !c.query("box", listed.PublicKey()) {
					t.Fatal("the key was not accepted as box")
				}
				if !c.login(special, listed) {
					t.Fatalf("a login signed as %s was refused", special)
				}
				stderr, status := c.run("exec", "control cli")
				switch special {
				case reservedBootstrapUser:
					if !strings.HasPrefix(stderr, "bootstrap") || status != 1 {
						t.Errorf("exec as %s after a query as box: stderr %q, status %d; want the bootstrap handler's refusal", special, stderr, status)
					}
				case reservedAPIUser:
					if !strings.Contains(stderr, "reserved for API access") || status != 1 {
						t.Errorf("exec as %s after a query as box: stderr %q, status %d; want the API refusal", special, stderr, status)
					}
				}
				want := "Error: shed '" + special + "' not found\n"
				if stderr, status := c.run("subsystem", "sftp"); stderr != want || status != 1 {
					t.Errorf("sftp as %s after a query as box: stderr %q, status %d; want %q, 1", special, stderr, status, want)
				}
				if ok, reason := c.forward(8080); ok || reason != "shed not found" {
					t.Errorf("forward as %s after a query as box: accepted %v, reason %q; want the not-found refusal", special, ok, reason)
				}
			}
			if calls := be.seen(); len(calls) != 0 {
				t.Errorf("a login signed as a special name reached the backend: %v", calls)
			}

			// Out of a special name: asked as _bootstrap or _api, signed as box.
			for _, special := range []string{reservedBootstrapUser, reservedAPIUser} {
				c := dialRaw(t, addr)
				if !c.query(special, listed.PublicKey()) {
					t.Fatalf("the key was not accepted as %s", special)
				}
				if !c.login("box", listed) {
					t.Fatal("a login signed as box was refused")
				}
				if stderr, status := c.run("exec", "true"); stderr != "" || status != 0 {
					t.Errorf("exec as box after a query as %s: stderr %q, status %d", special, stderr, status)
				}
				if stderr, status := c.run("subsystem", "sftp"); stderr != "" || status != 0 {
					t.Errorf("sftp as box after a query as %s: stderr %q, status %d", special, stderr, status)
				}
				if ok, reason := c.forward(8080); !ok {
					t.Errorf("forward as box after a query as %s refused: %s", special, reason)
				}
			}
			onlyBox(t, be.seen())
		})
	}
}

// TestOnlyTheSigningRequestCounts: on one connection a client asks about
// several keys as several usernames (some accepted, one not), is refused a
// signed login as git, and then signs. The connection is the key and the
// username of the request that authenticated it, nothing it asked about
// before: alice's key signed as box reaches alice's box, after bob's key was
// accepted as box; bob's key signed as box is bob, who may not see alice's
// box, after alice's key was accepted as box and as gitx.
func TestOnlyTheSigningRequestCounts(t *testing.T) {
	for _, mode := range []string{config.SSHAuthOff, config.SSHAuthWarn, config.SSHAuthEnforce} {
		t.Run(mode+" with a user list", func(t *testing.T) {
			alice, bob, stranger := testSigner(t), testSigner(t), testSigner(t)
			f := userstest.New()
			f.AddUser(users.User{Name: "alice"})
			f.AddKey("alice", users.Key{Fingerprint: gossh.FingerprintSHA256(alice.PublicKey()), Source: users.SourceInline})
			f.AddUser(users.User{Name: "bob"})
			f.AddKey("bob", users.Key{Fingerprint: gossh.FingerprintSHA256(bob.PublicKey()), Source: users.SourceInline})
			be := &routeBackend{}
			_, addr := routeServer(t, be, KeyAuth{Mode: mode, Users: f})

			// alice signs, after bob's key was accepted as box.
			c := dialRaw(t, addr)
			if !c.query("box", bob.PublicKey()) {
				t.Fatal("bob's key was not accepted as box")
			}
			if c.query("gitx", stranger.PublicKey()) {
				t.Fatal("a key on no user's list was accepted")
			}
			if !c.query("gitx", alice.PublicKey()) {
				t.Fatal("alice's key was not accepted as gitx")
			}
			if c.login(config.ReservedShedName, alice) {
				t.Fatal("a login signed as git authenticated")
			}
			if !c.login("box", alice) {
				t.Fatal("alice's key signed as box was refused")
			}
			if stderr, status := c.run("exec", "true"); stderr != "" || status != 0 {
				t.Errorf("exec as alice: stderr %q, status %d", stderr, status)
			}
			if stderr, status := c.run("subsystem", "sftp"); stderr != "" || status != 0 {
				t.Errorf("sftp as alice: stderr %q, status %d", stderr, status)
			}
			if ok, reason := c.forward(8080); !ok {
				t.Errorf("forward as alice refused: %s", reason)
			}
			onlyBox(t, be.seen())
			if !slicesContain(be.seen(), "Exec box") || !slicesContain(be.seen(), "DialService box") {
				t.Errorf("alice's channels did not all reach box: %v", be.seen())
			}

			// bob signs, after alice's key was accepted as box and as gitx.
			before := len(be.seen())
			c = dialRaw(t, addr)
			if !c.query("box", alice.PublicKey()) || !c.query("gitx", alice.PublicKey()) {
				t.Fatal("alice's key was not accepted")
			}
			if !c.login("box", bob) {
				t.Fatal("bob's key signed as box was refused")
			}
			want := "Error: shed 'box' not found\n"
			if stderr, status := c.run("exec", "true"); stderr != want || status != 1 {
				t.Errorf("exec as bob: stderr %q, status %d; want %q, 1", stderr, status, want)
			}
			if stderr, status := c.run("subsystem", "sftp"); stderr != want || status != 1 {
				t.Errorf("sftp as bob: stderr %q, status %d; want %q, 1", stderr, status, want)
			}
			if ok, reason := c.forward(8080); ok || reason != "shed not found" {
				t.Errorf("forward as bob: accepted %v, reason %q; want the not-found refusal", ok, reason)
			}
			for _, call := range be.seen()[before:] {
				if call != "GetShed box" {
					t.Errorf("bob's channels went past the lookup: %v", be.seen()[before:])
					break
				}
			}
		})
	}
}

func slicesContain(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

// TestUnsupportedForwardsAndChannelsDeclined: the server grants only what it
// has handlers for (a session and a direct-tcpip forward). A remote forward
// (tcpip-forward, and its cancel), a Unix socket forward in either direction
// (streamlocal-forward@openssh.com, direct-streamlocal@openssh.com), a
// forwarded-tcpip or x11 channel opened by the client, and a channel type
// nobody defines are all declined, on an authenticated connection that can
// otherwise reach its shed, and none reaches the backend.
func TestUnsupportedForwardsAndChannelsDeclined(t *testing.T) {
	for _, m := range routeModes() {
		t.Run(m.name, func(t *testing.T) {
			listed := testSigner(t)
			be := &routeBackend{}
			_, addr := routeServer(t, be, m.auth(gossh.FingerprintSHA256(listed.PublicKey())))

			c := dialRaw(t, addr)
			if !c.login("box", listed) {
				t.Fatal("a login signed as box was refused")
			}
			for _, req := range []struct {
				name    string
				payload []byte
			}{
				{"tcpip-forward", (&wire{}).s("127.0.0.1").u32(0).b},
				{"tcpip-forward", (&wire{}).s("localhost").u32(8080).b},
				{"cancel-tcpip-forward", (&wire{}).s("127.0.0.1").u32(8080).b},
				{"streamlocal-forward@openssh.com", (&wire{}).s("/tmp/shedtest.sock").b},
				{"cancel-streamlocal-forward@openssh.com", (&wire{}).s("/tmp/shedtest.sock").b},
			} {
				if c.globalRequest(req.name, req.payload) {
					t.Errorf("global request %s was granted", req.name)
				}
			}
			for _, open := range []struct {
				kind  string
				extra []byte
			}{
				{"direct-streamlocal@openssh.com", (&wire{}).s("/run/shed.sock").s("").u32(0).b},
				{"forwarded-tcpip", (&wire{}).s("127.0.0.1").u32(8080).s("127.0.0.1").u32(40000).b},
				{"forwarded-streamlocal@openssh.com", (&wire{}).s("/run/shed.sock").s("").b},
				{"x11", (&wire{}).s("127.0.0.1").u32(6010).b},
				{"shedtest-unknown@example.invalid", nil},
			} {
				if _, ok, reason := c.open(open.kind, open.extra); ok || reason != "unsupported channel type" {
					t.Errorf("channel %s: accepted %v, reason %q; want it declined as an unsupported type", open.kind, ok, reason)
				}
			}
			if calls := be.seen(); len(calls) != 0 {
				t.Errorf("a declined request reached the backend: %v", calls)
			}

			// Control: the same connection reaches box.
			if stderr, status := c.run("exec", "true"); stderr != "" || status != 0 {
				t.Errorf("exec as box: stderr %q, status %d", stderr, status)
			}
			onlyBox(t, be.seen())
		})
	}
}

// TestStartRefusesALoginBanner: a server NewServer builds has no login
// banner, and Start refuses to serve one that has been given a Banner or a
// BannerHandler (gliderlabs would replace the BannerCallback requestUser
// depends on: checkNoBanner), before it listens. That no banner reaches a
// client is checked on every raw-client test (rawSSH.read).
func TestStartRefusesALoginBanner(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*ssh.Server)
	}{
		{"Banner", func(srv *ssh.Server) { srv.Banner = "welcome\n" }},
		{"BannerHandler", func(srv *ssh.Server) { srv.BannerHandler = func(ssh.Context) string { return "" } }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, KeyAuth{})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.checkNoBanner(); err != nil {
				t.Fatalf("a server NewServer built: %v", err)
			}
			tc.set(s.sshServer)
			started := make(chan error, 1)
			go func() { started <- s.Start() }()
			select {
			case err := <-started:
				if !errors.Is(err, errBannerConfigured) {
					t.Errorf("Start = %v, want %v", err, errBannerConfigured)
				}
				if s.listener != nil {
					t.Error("Start listened before refusing")
				}
			case <-time.After(5 * time.Second):
				_ = s.sshServer.Close()
				t.Fatal("Start served with a login banner configured")
			}
		})
	}
}
