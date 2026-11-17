package sshd

import (
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory"
)

// The connection tracking of a directory server (KeyAuth.TrackConnections),
// over real SSH: the directory lookup's publication hook connected to
// CloseConnectionsExcept, as a directory server connects them.

// trackedCount is how many connections are tracked, by user name.
func (s *Server) trackedCount() map[string]int {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	out := map[string]int{}
	for name, set := range s.conns {
		out[name] = len(set)
	}
	return out
}

// dirUser is a list user with these keys (labels k0, k1, ...).
func dirUser(name string, admin bool, signers ...gossh.Signer) directory.User {
	u := directory.User{Name: name, Admin: admin}
	for i, s := range signers {
		fp := gossh.FingerprintSHA256(s.PublicKey())
		u.Keys = append(u.Keys, directory.Key{PublicKey: s.PublicKey(), Fingerprint: fp, Line: keyLine(s), Label: fmt.Sprintf("k%d", i)})
		u.Fingerprints = append(u.Fingerprints, fp)
	}
	return u
}

// dirPeople is a directory lookup whose local admin is root and whose list,
// pull 1, holds alice and bob (not admins), each with one fresh key.
type dirPeople struct {
	alice, bob, root gossh.Signer
	lookup           *directory.Lookup
}

func newDirPeople(t *testing.T, opts directory.LookupOptions) dirPeople {
	t.Helper()
	p := dirPeople{alice: testSigner(t), bob: testSigner(t), root: testSigner(t)}
	opts.LocalAdmins = map[string]config.UserConfig{"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: keyLine(p.root), Label: "desk"}}}}
	l, err := directory.NewLookup(opts)
	if err != nil {
		t.Fatal(err)
	}
	p.lookup = l
	at := time.Now()
	if opts.Now != nil {
		at = opts.Now()
	}
	installList(t, l, 1, at, dirUser("alice", false, p.alice), dirUser("bob", false, p.bob))
	return p
}

// installList installs a list of us as pull seq, fetched at at, trusted for
// an hour.
func installList(t *testing.T, l *directory.Lookup, seq uint64, at time.Time, us ...directory.User) {
	t.Helper()
	if ok, err := l.Install(&directory.List{Users: us}, at, time.Hour, seq); err != nil || !ok {
		t.Fatalf("Install(seq %d) = %v, %v", seq, ok, err)
	}
}

// trackingServer is a server over be with lookup, tracking connections, whose
// publications close the connections of users the new state does not have,
// as a directory server's do; each publication sends how many it closed.
// prepare runs on the server before it serves.
func trackingServer(t *testing.T, be backend.Backend, lookup *directory.Lookup, prepare func(*Server)) (*Server, string, <-chan int) {
	t.Helper()
	s, err := NewServer(be, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil,
		KeyAuth{Mode: config.SSHAuthEnforce, Users: lookup, TrackConnections: true})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan int, 16)
	lookup.SetPublicationHook(func(p directory.Publication) { closed <- s.CloseConnectionsExcept(p.Has) })
	t.Cleanup(func() { lookup.SetPublicationHook(nil) })
	if prepare != nil {
		prepare(s)
	}
	return s, serveForTest(t, s), closed
}

// closedCount is how many connections the next publication closed.
func closedCount(t *testing.T, closed <-chan int) int {
	t.Helper()
	select {
	case n := <-closed:
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("no publication")
	}
	return -1
}

// waitGone waits for client's connection to end, and fails the test if it
// does not within ten seconds.
func waitGone(t *testing.T, who string, client *gossh.Client) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s's connection was not closed", who)
	}
}

// alive reports whether client's connection still answers a request.
func alive(client *gossh.Client) bool {
	_, _, err := client.SendRequest("keepalive@example.test", true, nil)
	return err == nil
}

// refusedDial reports whether a new connection as user with signer is
// refused.
func refusedDial(addr, user string, signer gossh.Signer) bool {
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err == nil {
		_ = c.Close()
		return false
	}
	return true
}

// echoThrough sends msg through conn, a forwarded port to an echo, and reads
// it back within echoTimeout.
func echoThrough(conn net.Conn, msg string) error {
	done := make(chan error, 1)
	go func() {
		if _, err := conn.Write([]byte(msg)); err != nil {
			done <- err
			return
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(conn, buf); err != nil {
			done <- err
			return
		}
		if string(buf) != msg {
			done <- fmt.Errorf("read %q, want %q", buf, msg)
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(echoTimeout):
		_ = conn.Close()
		return fmt.Errorf("no echo of %q within %s", msg, echoTimeout)
	}
}

// TestSSHDirectoryRemovalClosesTheUsersConnections: alice, bob and the local
// admin root hold connections; a new list without alice is published. Her
// connection is closed (its open forward with it) and a new one is refused;
// bob's and root's stay open and keep working.
func TestSSHDirectoryRemovalClosesTheUsersConnections(t *testing.T) {
	p := newDirPeople(t, directory.LookupOptions{})
	s, addr, closed := trackingServer(t, twoShedBackend{&identityBackend{}}, p.lookup, nil)

	alice := dialWith(t, addr, "box", p.alice)
	fwd, err := alice.Dial("tcp", "localhost:8080")
	if err != nil {
		t.Fatalf("alice's forward: %v", err)
	}
	if err := echoThrough(fwd, "before"); err != nil {
		t.Fatal(err)
	}
	bob := dialWith(t, addr, "crate", p.bob)
	root := dialWith(t, addr, reservedBootstrapUser, p.root)
	if got, want := s.trackedCount(), map[string]int{"alice": 1, "bob": 1, "root": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tracked %v, want %v", got, want)
	}

	installList(t, p.lookup, 2, time.Now(), dirUser("bob", false, p.bob))
	if n := closedCount(t, closed); n != 1 {
		t.Errorf("the publication closed %d connections, want alice's one", n)
	}
	waitGone(t, "alice", alice)
	if err := echoThrough(fwd, "after"); err == nil {
		t.Error("alice's forward still works after her connection was closed")
	}
	if !refusedDial(addr, "box", p.alice) {
		t.Error("a new connection with alice's key was accepted after she left the list")
	}
	if stderr, status := runOn(t, bob, "true", ""); status != 0 {
		t.Errorf("bob's connection after alice left: status %d stderr %q", status, stderr)
	}
	if !alive(root) {
		t.Error("the local admin's connection was closed")
	}
	if got, want := s.trackedCount(), map[string]int{"bob": 1, "root": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("tracked %v, want %v", got, want)
	}
}

// TestSSHDirectoryKeyMove is TestSSHConnectionKeepsTheUserItAuthenticatedAs
// on a directory server: key K is alice's when three connections
// authenticate with it, as box (alice's shed), as crate (bob's) and as
// _bootstrap; a new list moves K to bob, alice keeping another key. Alice is
// still a user, so no connection is closed. On each of those connections a
// new channel is refused, the enrolment included; the forward box opened
// before the move keeps working; a new connection with K is bob's.
func TestSSHDirectoryKeyMove(t *testing.T) {
	p := newDirPeople(t, directory.LookupOptions{})
	k := p.alice
	be := &identityBackend{}
	s, addr, closed := trackingServer(t, twoShedBackend{be}, p.lookup, func(s *Server) {
		s.SetBootstrap(BootstrapInfo{HTTPSPort: 8443, TokenTTL: time.Hour, CA: testCA(t)})
	})

	if user, bound, stderr, err := bootstrapOn(t, dialWith(t, addr, reservedBootstrapUser, k)); err != nil || !bound || user != "alice" {
		t.Fatalf("enrolment before the move: user %q bound %v stderr %q err %v, want alice", user, bound, stderr, err)
	}
	box := dialWith(t, addr, "box", k)
	crate := dialWith(t, addr, "crate", k)
	boot := dialWith(t, addr, reservedBootstrapUser, k)
	open, err := box.Dial("tcp", "localhost:8080")
	if err != nil {
		t.Fatalf("forward before the move: %v", err)
	}
	defer func() { _ = open.Close() }()
	if err := echoThrough(open, "before"); err != nil {
		t.Fatal(err)
	}
	execsBefore, dialsBefore := be.seen()

	installList(t, p.lookup, 2, time.Now(), dirUser("alice", false, testSigner(t)), dirUser("bob", false, p.bob, k))
	if n := closedCount(t, closed); n != 0 {
		t.Errorf("the move closed %d connections; alice is still a user", n)
	}

	const refused = "no longer belongs to the user this connection authenticated as"
	for name, c := range map[string]*gossh.Client{"box": box, "crate": crate} {
		for _, sub := range []string{"", "sftp"} {
			if stderr, status := runOn(t, c, "true", sub); status != 1 || !strings.Contains(stderr, refused) {
				t.Errorf("new channel (subsystem %q) on the %s connection after the move: status %d stderr %q, want refused", sub, name, status, stderr)
			}
		}
		if conn, err := c.Dial("tcp", "localhost:8080"); err == nil {
			_ = conn.Close()
			t.Errorf("a new forward on the %s connection was opened after the move", name)
		}
	}
	if user, _, stderr, err := bootstrapOn(t, boot); err == nil || !strings.Contains(stderr, "key not authorized") {
		t.Errorf("enrolment on the connection made before the move: user %q err %v stderr %q, want refused", user, err, stderr)
	}
	if err := echoThrough(open, "after"); err != nil {
		t.Errorf("the forward opened before the move stopped: %v", err)
	}
	if execs, dials := be.seen(); len(execs) != len(execsBefore) || len(dials) != len(dialsBefore) {
		t.Errorf("a channel after the move reached the backend: execs %v dials %v", execs, dials)
	}

	if user, bound, stderr, err := bootstrapOn(t, dialWith(t, addr, reservedBootstrapUser, k)); err != nil || !bound || user != "bob" {
		t.Errorf("enrolment with K after the move: user %q bound %v stderr %q err %v, want bob", user, bound, stderr, err)
	}
	if stderr, status := runOn(t, dialWith(t, addr, "crate", k), "true", ""); status != 0 {
		t.Errorf("a new connection with K to bob's crate: status %d stderr %q, want entered as bob", status, stderr)
	}
	if execs, _ := be.seen(); len(execs) == 0 || execs[len(execs)-1] != backend.UserPrincipal("bob", false) {
		t.Errorf("the new connection reached the backend as %v, want bob", execs)
	}
	// Alice's four connections (the first enrolment's, box, crate, boot)
	// are still hers; bob's two are the ones made with K after the move.
	if got, want := s.trackedCount(), map[string]int{"alice": 4, "bob": 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("tracked %v, want %v", got, want)
	}
}

// TestSSHDirectoryTrustEndClosesListedConnections: the end of the list's
// trust time, published by the trust ticker, closes the listed users'
// connections as a removal does; the local admin's stays open, and the
// local admin still logs in.
func TestSSHDirectoryTrustEndClosesListedConnections(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	ticks := make(chan time.Time)
	p := newDirPeople(t, directory.LookupOptions{
		Now:        clock,
		TrustTicks: func() (<-chan time.Time, func()) { return ticks, func() {} },
	})
	_, addr, closed := trackingServer(t, twoShedBackend{&identityBackend{}}, p.lookup, nil)
	ctx := t.Context()
	go p.lookup.WatchTrust(ctx)

	alice := dialWith(t, addr, "box", p.alice)
	bob := dialWith(t, addr, "crate", p.bob)
	root := dialWith(t, addr, reservedBootstrapUser, p.root)

	mu.Lock()
	now = now.Add(time.Hour)
	mu.Unlock()
	ticks <- clock()
	if n := closedCount(t, closed); n != 2 {
		t.Errorf("the end of the trust time closed %d connections, want alice's and bob's", n)
	}
	waitGone(t, "alice", alice)
	waitGone(t, "bob", bob)
	if !alive(root) {
		t.Error("the local admin's connection was closed at the end of the trust time")
	}
	if !refusedDial(addr, "crate", p.bob) {
		t.Error("bob logged in after the trust time ended")
	}
	if refusedDial(addr, reservedBootstrapUser, p.root) {
		t.Error("the local admin was refused after the trust time ended")
	}
}

// TestSSHRemovalWhileAuthenticating: alice leaves the list after her
// connection's identity was resolved and before it was tracked (forced with
// the server's hook), so the publication's closing does not find it. The
// connection's own check after it is tracked then sees the new state, and
// it does not authenticate.
func TestSSHRemovalWhileAuthenticating(t *testing.T) {
	p := newDirPeople(t, directory.LookupOptions{})
	without := &directory.List{Users: []directory.User{dirUser("bob", false, p.bob)}}
	var once sync.Once
	s, addr, closed := trackingServer(t, &identityBackend{}, p.lookup, func(s *Server) {
		s.beforeTrack = func() {
			once.Do(func() {
				if ok, err := p.lookup.Install(without, time.Now(), time.Hour, 2); !ok || err != nil {
					panic(fmt.Sprintf("Install = %v, %v", ok, err))
				}
			})
		}
	})

	if !refusedDial(addr, "box", p.alice) {
		t.Error("alice's connection authenticated although she left the list while it did")
	}
	if n := closedCount(t, closed); n != 0 {
		t.Errorf("the publication closed %d connections; alice's was not tracked yet", n)
	}
	if got := s.trackedCount(); len(got) != 0 {
		t.Errorf("tracked %v after the refused connection, want none", got)
	}
	// The hook ran once: bob, still a user, logs in and is tracked.
	bob := dialWith(t, addr, "crate", p.bob)
	if got := s.trackedCount(); got["bob"] != 1 || !alive(bob) {
		t.Errorf("tracked %v after bob's login", got)
	}
}

// gatedLog is a log writer whose first line containing match blocks until
// the test releases it (and reports that it is blocked); every other line
// passes. It holds the log package's lock while it blocks, so every log line
// anywhere waits for the release.
type gatedLog struct {
	match   string
	once    sync.Once
	blocked chan struct{}
	release chan struct{}
}

func (g *gatedLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), g.match) {
		g.once.Do(func() {
			close(g.blocked)
			<-g.release
		})
	}
	return len(p), nil
}

// TestSSHCloseSweepBeforeLogging: a publication removes alice and bob, each
// holding a connection. While the sweep's first log line is held by a log
// writer that does not return, both connections are already closed: every
// socket is closed before any log line, so logging never delays or prevents
// a close. The local admin's connection stays open.
func TestSSHCloseSweepBeforeLogging(t *testing.T) {
	p := newDirPeople(t, directory.LookupOptions{})
	_, addr, closed := trackingServer(t, twoShedBackend{&identityBackend{}}, p.lookup, nil)
	alice := dialWith(t, addr, "box", p.alice)
	bob := dialWith(t, addr, "crate", p.bob)
	root := dialWith(t, addr, reservedBootstrapUser, p.root)

	gate := &gatedLog{match: "SSH: closed", blocked: make(chan struct{}), release: make(chan struct{})}
	prev := log.Writer()
	log.SetOutput(gate)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	// Runs before the server's own clean-up (registered earlier), which may
	// log too.
	t.Cleanup(func() {
		release()
		log.SetOutput(prev)
	})

	installed := make(chan error, 1)
	go func() {
		ok, err := p.lookup.Install(&directory.List{}, time.Now(), time.Hour, 2)
		if err == nil && !ok {
			err = fmt.Errorf("the list was not published")
		}
		installed <- err
	}()
	select {
	case <-gate.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the publication wrote no close line")
	}
	waitGone(t, "alice", alice)
	waitGone(t, "bob", bob)
	release()
	if err := <-installed; err != nil {
		t.Fatal(err)
	}
	if n := closedCount(t, closed); n != 2 {
		t.Errorf("the publication closed %d connections, want alice's and bob's", n)
	}
	if !alive(root) {
		t.Error("the local admin's connection was closed")
	}
}

// TestSSHDisconnectUntracks: a connection the client closes is no longer
// tracked once the server has closed its end.
func TestSSHDisconnectUntracks(t *testing.T) {
	p := newDirPeople(t, directory.LookupOptions{})
	closes := make(chan struct{}, 4)
	s, addr, _ := trackingServer(t, twoShedBackend{&identityBackend{}}, p.lookup, func(s *Server) {
		s.afterClose = func() { closes <- struct{}{} }
	})
	alice := dialWith(t, addr, "box", p.alice)
	if got, want := s.trackedCount(), map[string]int{"alice": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tracked %v, want %v", got, want)
	}
	if err := alice.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closes:
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not close alice's connection after she disconnected")
	}
	if got := s.trackedCount(); len(got) != 0 {
		t.Errorf("tracked %v after alice disconnected, want none", got)
	}
}

// TestSSHRefusedKeyThenAcceptedKeyTracked: one connection offers alice's key
// and then bob's. Alice leaves the list while her key's signature is being
// accepted (forced with the server's hook), so her key does not authenticate
// the connection; bob's then does, and the connection is tracked under bob
// alone, so bob's removal closes it.
func TestSSHRefusedKeyThenAcceptedKeyTracked(t *testing.T) {
	p := newDirPeople(t, directory.LookupOptions{})
	var once sync.Once
	var tracks atomic.Int32
	s, addr, closed := trackingServer(t, twoShedBackend{&identityBackend{}}, p.lookup, func(s *Server) {
		s.beforeTrack = func() {
			tracks.Add(1)
			once.Do(func() {
				without := &directory.List{Users: []directory.User{dirUser("bob", false, p.bob)}}
				if ok, err := p.lookup.Install(without, time.Now(), time.Hour, 2); !ok || err != nil {
					panic(fmt.Sprintf("Install = %v, %v", ok, err))
				}
			})
		}
	})

	both := dialWith(t, addr, "crate", p.alice, p.bob)
	// Two signatures were accepted and reached the tracking: alice's, which
	// was refused there, then bob's.
	if n := tracks.Load(); n != 2 {
		t.Fatalf("%d keys reached the tracking, want alice's and then bob's", n)
	}
	if n := closedCount(t, closed); n != 0 {
		t.Errorf("alice's removal closed %d connections; hers was not tracked yet", n)
	}
	if stderr, status := runOn(t, both, "true", ""); status != 0 {
		t.Fatalf("the connection in bob's crate: status %d stderr %q, want entered as bob", status, stderr)
	}
	if got, want := s.trackedCount(), map[string]int{"bob": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tracked %v, want %v", got, want)
	}

	installList(t, p.lookup, 3, time.Now())
	if n := closedCount(t, closed); n != 1 {
		t.Errorf("bob's removal closed %d connections, want the one", n)
	}
	waitGone(t, "bob", both)
}

// TestSSHConnectionsNotTrackedByDefault: without TrackConnections no
// connection is wrapped or tracked, and CloseConnectionsExcept closes
// nothing, whatever keep says.
func TestSSHConnectionsNotTrackedByDefault(t *testing.T) {
	who := newPeople(t)
	s, addr := identityServer(t, &identityBackend{}, who.lookup)
	if s.sshServer.ConnCallback != nil {
		t.Error("a connection callback is installed without TrackConnections")
	}
	alice := dialWith(t, addr, "box", who.alice)
	if stderr, status := runOn(t, alice, "true", ""); status != 0 {
		t.Fatalf("alice in box: status %d stderr %q", status, stderr)
	}
	if got := s.trackedCount(); len(got) != 0 {
		t.Errorf("tracked %v without TrackConnections", got)
	}
	if n := s.CloseConnectionsExcept(func(string) bool { return false }); n != 0 {
		t.Errorf("CloseConnectionsExcept closed %d connections without TrackConnections", n)
	}
	if stderr, status := runOn(t, alice, "true", ""); status != 0 {
		t.Errorf("alice's connection after CloseConnectionsExcept: status %d stderr %q", status, stderr)
	}
}
