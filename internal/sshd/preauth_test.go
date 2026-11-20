package sshd

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/users/userstest"
)

// The limits on unauthenticated connections (preauth.go), over real SSH,
// with test values for the grace and the cap, in every auth mode.

// preauthMode is an auth mode the limits are tested in. known is accepted
// in every one of them; any other key is refused in enforce mode only.
type preauthMode struct {
	name string
	auth func(known string) KeyAuth
}

func preauthModes() []preauthMode {
	return []preauthMode{
		{"off", func(string) KeyAuth { return KeyAuth{} }},
		{"warn", func(k string) KeyAuth { return KeyAuth{Mode: config.SSHAuthWarn, Users: userstest.Implicit(k)} }},
		{"enforce", func(k string) KeyAuth { return KeyAuth{Mode: config.SSHAuthEnforce, Users: userstest.Implicit(k)} }},
		{"enforce, tracking connections", func(k string) KeyAuth {
			return KeyAuth{Mode: config.SSHAuthEnforce, Users: userstest.Implicit(k), TrackConnections: true}
		}},
	}
}

// recordedTimer is a real grace timer that records whether it was stopped.
type recordedTimer struct {
	t       *time.Timer
	stopped atomic.Bool
}

func (r *recordedTimer) Stop() bool {
	r.stopped.Store(true)
	return r.t.Stop()
}

// timerRecord holds every grace timer a server started, in order.
type timerRecord struct {
	mu     sync.Mutex
	timers []*recordedTimer
}

func (l *timerRecord) afterFunc(d time.Duration, f func()) graceTimer {
	r := &recordedTimer{t: time.AfterFunc(d, f)}
	l.mu.Lock()
	l.timers = append(l.timers, r)
	l.mu.Unlock()
	return r
}

// only is the one timer started, failing the test if there is not exactly
// one.
func (l *timerRecord) only(t *testing.T) *recordedTimer {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.timers) != 1 {
		t.Fatalf("%d grace timers were started, want 1", len(l.timers))
	}
	return l.timers[0]
}

// preauthTest is a server under test with its log, its grace timers and a
// signal for every first close of an admitted connection.
type preauthTest struct {
	s      *Server
	addr   string
	log    *syncLog
	timers *timerRecord
	closes chan struct{}
	known  gossh.Signer
}

// newPreauthTest is a server in mode whose grace is grace and whose cap is
// max with a share of perSource, serving on loopback.
func newPreauthTest(t *testing.T, mode preauthMode, grace time.Duration, max, perSource int) *preauthTest {
	t.Helper()
	known := testSigner(t)
	s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, mode.auth(gossh.FingerprintSHA256(known.PublicKey())))
	if err != nil {
		t.Fatal(err)
	}
	p := &preauthTest{s: s, known: known, timers: &timerRecord{}, closes: make(chan struct{}, 64)}
	s.loginGrace, s.maxUnauthenticated, s.maxUnauthenticatedPerSource = grace, max, perSource
	s.afterFunc = p.timers.afterFunc
	s.afterPreauthClose = func() { p.closes <- struct{}{} }
	p.log = captureLog(t)
	p.addr = serveForTest(t, s)
	return p
}

// closed waits for the next first close of an admitted connection.
func (p *preauthTest) closed(t *testing.T) {
	t.Helper()
	select {
	case <-p.closes:
	case <-time.After(10 * time.Second):
		t.Fatal("no admitted connection closed")
	}
}

// waitCounts waits until the server counts total unauthenticated
// connections.
func waitCounts(t *testing.T, s *Server, total int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, by := s.preauthCounts()
		if got == total {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d unauthenticated connections (%v), want %d", got, by, total)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// linesAbout is every line of the log that names peer.
func linesAbout(text, peer string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.Contains(line, "remote="+peer+" ") || strings.Contains(line, "from "+peer+" ") {
			out.WriteString(line)
		}
	}
	return out.String()
}

// silent opens a TCP connection to addr that never speaks.
func silent(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// closedByServer reports whether the server closed c within ten seconds,
// reading (and dropping) whatever it sent first: an end of stream or a
// reset, not a timeout.
func closedByServer(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, err := io.Copy(io.Discard, c) // nil at the end of the stream
	var ne net.Error
	return err == nil || !errors.As(err, &ne) || !ne.Timeout()
}

// TestPreauthGraceClosesASilentConnection: a connection that never
// authenticates is closed when its grace ends, with one line naming its
// address, and leaves the counts; it offered no key, so it writes no
// "SSH auth failed" line.
func TestPreauthGraceClosesASilentConnection(t *testing.T) {
	for _, mode := range preauthModes() {
		t.Run(mode.name, func(t *testing.T) {
			p := newPreauthTest(t, mode, 150*time.Millisecond, 100, 10)
			c := silent(t, p.addr)
			peer := c.LocalAddr().String()
			if !closedByServer(c) {
				t.Fatal("the server did not close a connection silent past its grace")
			}
			p.closed(t)
			waitCounts(t, p.s, 0)
			// The grace line follows the close, and may follow the close the
			// connection's own goroutine made when it saw the socket close.
			want := fmt.Sprintf("SSH: closed a connection from %s that did not authenticate within 150ms\n", peer)
			deadline := time.Now().Add(10 * time.Second)
			for got := linesAbout(p.log.String(), peer); got != want; got = linesAbout(p.log.String(), peer) {
				if time.Now().After(deadline) {
					t.Fatalf("log about %s:\n got %q\nwant %q", peer, got, want)
				}
				time.Sleep(5 * time.Millisecond)
			}
			// Nothing more is written about it.
			time.Sleep(100 * time.Millisecond)
			if got := linesAbout(p.log.String(), peer); got != want {
				t.Errorf("log about %s:\n got %q\nwant %q", peer, got, want)
			}
		})
	}
}

// TestPreauthAuthenticatedOutlivesTheGrace: a connection that authenticated
// stays open, idle, well past the grace; its timer was stopped when it
// authenticated, and it left the counts then.
func TestPreauthAuthenticatedOutlivesTheGrace(t *testing.T) {
	for _, mode := range preauthModes() {
		t.Run(mode.name, func(t *testing.T) {
			p := newPreauthTest(t, mode, 100*time.Millisecond, 100, 10)
			client := dialWith(t, p.addr, "box", p.known)
			if !p.timers.only(t).stopped.Load() {
				t.Error("the grace timer of an authenticated connection was not stopped")
			}
			waitCounts(t, p.s, 0)
			time.Sleep(400 * time.Millisecond)
			if !alive(client) {
				t.Fatal("an authenticated connection was closed after the grace")
			}
			if strings.Contains(p.log.String(), "did not authenticate") {
				t.Errorf("a grace line was written for an authenticated connection:\n%s", p.log.String())
			}
		})
	}
}

// TestPreauthClosedConnectionStopsItsTimer: a connection that closes before
// its grace ends has its timer stopped, and no grace line is written for it
// afterwards.
func TestPreauthClosedConnectionStopsItsTimer(t *testing.T) {
	for _, mode := range preauthModes() {
		t.Run(mode.name, func(t *testing.T) {
			p := newPreauthTest(t, mode, 200*time.Millisecond, 100, 10)
			c := silent(t, p.addr)
			peer := c.LocalAddr().String()
			waitCounts(t, p.s, 1)
			_ = c.Close()
			p.closed(t)
			if !p.timers.only(t).stopped.Load() {
				t.Error("the grace timer of a closed connection was not stopped")
			}
			waitCounts(t, p.s, 0)
			time.Sleep(500 * time.Millisecond)
			if got := linesAbout(p.log.String(), peer); got != "" {
				t.Errorf("lines about a connection closed before its grace:\n%s", got)
			}
		})
	}
}

// heldTimer is a grace timer that never fires by itself.
type heldTimer struct{}

func (heldTimer) Stop() bool { return true }

// TestPreauthGraceEndedConnectionDoesNotAuthenticate: a connection whose
// grace ended before its signature was verified is being closed, and does
// not authenticate (preauthAuthenticated refuses it); it leaves the counts
// once.
func TestPreauthGraceEndedConnectionDoesNotAuthenticate(t *testing.T) {
	s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, KeyAuth{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := newFakeSSHContext(t, "box")
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	var fire func()
	s.afterFunc = func(_ time.Duration, f func()) graceTimer { fire = f; return heldTimer{} }
	if s.admitConn(ctx, server) == nil {
		t.Fatal("the first connection was refused")
	}
	captureLog(t)
	fire()
	cfg := s.sshServer.ServerConfigCallback(ctx)
	if _, err := cfg.VerifiedPublicKeyCallback(fakeConnMeta{user: "box"}, testSigner(t).PublicKey(), &gossh.Permissions{}, gossh.KeyAlgoED25519); err == nil {
		t.Error("a connection whose grace ended authenticated")
	}
	if total, _ := s.preauthCounts(); total != 0 {
		t.Errorf("%d unauthenticated connections after the grace closed the only one", total)
	}
}

// stallingConn is a connection whose first Close reports that it began and
// then waits for the test to release it, holding a close half done.
type stallingConn struct {
	net.Conn
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (c *stallingConn) Close() error {
	c.once.Do(func() {
		close(c.entered)
		<-c.release
	})
	return c.Conn.Close()
}

// TestPreauthGraceEndedWhileClosing: the grace has ended (expired) and its
// close has begun but not finished (not yet closed) when the verified-key
// callback runs. The connection does not authenticate, and it leaves the
// counts once, when the close finishes.
func TestPreauthGraceEndedWhileClosing(t *testing.T) {
	s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, KeyAuth{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := newFakeSSHContext(t, "box")
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	conn := &stallingConn{Conn: server, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(conn.release) }) }
	t.Cleanup(release)
	var fire func()
	s.afterFunc = func(_ time.Duration, f func()) graceTimer { fire = f; return heldTimer{} }
	if s.admitConn(ctx, conn) == nil {
		t.Fatal("the first connection was refused")
	}
	captureLog(t)
	fired := make(chan struct{})
	go func() { fire(); close(fired) }()
	select {
	case <-conn.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the grace did not close the connection")
	}

	cfg := s.sshServer.ServerConfigCallback(ctx)
	if _, err := cfg.VerifiedPublicKeyCallback(fakeConnMeta{user: "box"}, testSigner(t).PublicKey(), &gossh.Permissions{}, gossh.KeyAlgoED25519); err == nil {
		t.Error("a connection whose grace ended, still being closed, authenticated")
	}
	if total, _ := s.preauthCounts(); total != 1 {
		t.Errorf("%d unauthenticated connections while the grace's close runs, want 1", total)
	}
	release()
	<-fired
	if total, _ := s.preauthCounts(); total != 0 {
		t.Errorf("%d unauthenticated connections after the grace closed the only one", total)
	}
}

// gateLog sends the standard logger's output to a gatedLog that blocks on
// the first line containing match, for the rest of the test; release lets
// it go (and runs at the end of the test, before earlier clean-ups).
func gateLog(t *testing.T, match string) (gate *gatedLog, release func()) {
	t.Helper()
	gate = &gatedLog{match: match, blocked: make(chan struct{}), release: make(chan struct{})}
	prev := log.Writer()
	log.SetOutput(gate)
	var once sync.Once
	release = func() { once.Do(func() { close(gate.release) }) }
	t.Cleanup(func() {
		release()
		log.SetOutput(prev)
	})
	return gate, release
}

// waitBlocked waits for gate to block on its line.
func waitBlocked(t *testing.T, gate *gatedLog, what string) {
	t.Helper()
	select {
	case <-gate.blocked:
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s was written", what)
	}
}

// TestPreauthGraceClosesBeforeItsLine: while the log writer is blocked on
// the grace line, the connection is already closed and its place given back.
func TestPreauthGraceClosesBeforeItsLine(t *testing.T) {
	p := newPreauthTest(t, preauthModes()[2], 150*time.Millisecond, 100, 10)
	gate, release := gateLog(t, "did not authenticate")
	c := silent(t, p.addr)
	waitBlocked(t, gate, "grace line")
	if !closedByServer(c) {
		t.Error("the connection was not closed while its grace line waited for the log writer")
	}
	waitCounts(t, p.s, 0)
	release()
}

// TestPreauthRefusalClosesBeforeItsLine: while the log writer is blocked on
// the share's refusal line, the refused connection is already closed.
func TestPreauthRefusalClosesBeforeItsLine(t *testing.T) {
	p := newPreauthTest(t, preauthModes()[2], time.Minute, 100, 1)
	silent(t, p.addr)
	waitCounts(t, p.s, 1)
	gate, release := gateLog(t, "SSH: refusing")
	c := silent(t, p.addr)
	waitBlocked(t, gate, "refusal line")
	if !closedByServer(c) {
		t.Error("the refused connection was not closed while its refusal line waited for the log writer")
	}
	waitCounts(t, p.s, 1)
	release()
}

// TestPreauthSweepDoesNotWaitForTheFailedLine: alice's connection is in the
// window between its tracking and its leaving the unauthenticated counts
// (her key was offered, proven and tracked; preauthAuthenticated has not run
// yet) when a publication removes alice and bob. While the log writer is
// blocked on her connection's "SSH auth failed" line, the publication has
// returned, both connections are closed and her place is given back.
func TestPreauthSweepDoesNotWaitForTheFailedLine(t *testing.T) {
	p := newDirPeople(t, directory.LookupOptions{})
	s, addr, closed := trackingServer(t, twoShedBackend{&identityBackend{}}, p.lookup, nil)
	bob := dialWith(t, addr, "crate", p.bob)
	root := dialWith(t, addr, reservedBootstrapUser, p.root)

	ctx := newFakeSSHContext(t, "box")
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	if s.admitConn(ctx, server) == nil {
		t.Fatal("alice's connection was refused")
	}
	noteOffer(ctx, "box")
	id, err := s.newConnIdentity(p.alice.PublicKey(), "box")
	if err != nil {
		t.Fatal(err)
	}
	setConnIdentity(ctx, id)
	if err := s.trackAuthenticated(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := s.trackedCount(); got["alice"] != 1 || got["bob"] != 1 {
		t.Fatalf("tracked %v, want alice and bob", got)
	}
	waitCounts(t, s, 1)

	gate, release := gateLog(t, "SSH auth failed")
	installed := make(chan error, 1)
	go func() {
		ok, err := p.lookup.Install(&directory.List{}, time.Now(), time.Hour, 2)
		if err == nil && !ok {
			err = fmt.Errorf("the list was not published")
		}
		installed <- err
	}()
	waitBlocked(t, gate, "failed line")
	select {
	case err := <-installed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the publication waited for the blocked log writer")
	}
	if n := closedCount(t, closed); n != 2 {
		t.Errorf("the publication closed %d connections, want alice's and bob's", n)
	}
	waitGone(t, "bob", bob)
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Error("alice's connection is open after the sweep")
	}
	waitCounts(t, s, 0)
	if !alive(root) {
		t.Error("the local admin's connection was closed")
	}
	release()
}

// TestPreauthFailedLine: a connection that offered keys and closed without
// proving one writes one "SSH auth failed" line with the number of keys it
// offered and its last username; one that then proves a key, and one that
// offered none, write none.
func TestPreauthFailedLine(t *testing.T) {
	for _, mode := range preauthModes() {
		t.Run(mode.name, func(t *testing.T) {
			p := newPreauthTest(t, mode, time.Minute, 100, 10)
			strangers := []gossh.Signer{testSigner(t), testSigner(t), testSigner(t)}

			// Three keys offered, none proven.
			c := dialRaw(t, p.addr)
			peer := c.conn.LocalAddr().String()
			for i, k := range strangers {
				c.query([]string{"box", "box", "crate"}[i], k.PublicKey())
			}
			_ = c.conn.Close()
			p.closed(t)
			want := fmt.Sprintf("SSH auth failed: remote=%s user=crate keys=3\n", peer)
			if got := failedLines(p.log.String(), peer); got != want {
				t.Errorf("after three keys and no proof:\n got %q\nwant %q", got, want)
			}

			// Three keys offered, then a proven one.
			c = dialRaw(t, p.addr)
			peer = c.conn.LocalAddr().String()
			for _, k := range strangers {
				c.query("box", k.PublicKey())
			}
			if !c.login("box", p.known) {
				t.Fatal("the known key did not log in")
			}
			_ = c.conn.Close()
			p.closed(t)
			if got := failedLines(p.log.String(), peer); got != "" {
				t.Errorf("a connection that proved a key wrote %q", got)
			}

			// No key offered.
			c = dialRaw(t, p.addr)
			peer = c.conn.LocalAddr().String()
			_ = c.conn.Close()
			p.closed(t)
			if got := linesAbout(p.log.String(), peer); got != "" {
				t.Errorf("a connection that offered no key wrote %q", got)
			}
			waitCounts(t, p.s, 0)
		})
	}
}

// failedLines is every "SSH auth failed" line of the log about peer.
func failedLines(text, peer string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(linesAbout(text, peer), "\n") {
		if strings.HasPrefix(line, "SSH auth failed:") {
			out.WriteString(line)
		}
	}
	return out.String()
}

// TestPreauthSlotsFreed: an unauthenticated connection's place is given back
// when it closes, when its handshake fails, and when its keys are refused;
// authenticated connections, however many, hold none, so they never block a
// new one.
func TestPreauthSlotsFreed(t *testing.T) {
	for _, mode := range preauthModes() {
		t.Run(mode.name, func(t *testing.T) {
			p := newPreauthTest(t, mode, time.Minute, 3, 2)
			a, b := silent(t, p.addr), silent(t, p.addr)
			waitCounts(t, p.s, 2)
			_ = a.Close()
			_ = b.Close()
			waitCounts(t, p.s, 0)

			// A failed handshake: a version line, then a packet longer than
			// any the server reads.
			junk := silent(t, p.addr)
			waitCounts(t, p.s, 1)
			if _, err := io.WriteString(junk, "SSH-2.0-junk\r\n\xff\xff\xff\xff\x00\x00\x00\x00"); err != nil {
				t.Fatal(err)
			}
			if !closedByServer(junk) {
				t.Fatal("the server kept a connection that is not SSH")
			}
			waitCounts(t, p.s, 0)

			// An unknown key: refused in enforce mode, and the connection
			// closes; elsewhere it logs in, and the client closes it.
			refusedDial(p.addr, "box", testSigner(t))
			waitCounts(t, p.s, 0)

			// More authenticated connections than the cap and the share.
			for i := range 5 {
				dialWith(t, p.addr, fmt.Sprintf("box%d", i), p.known)
			}
			waitCounts(t, p.s, 0)
			if refusedDial(p.addr, "box", p.known) {
				t.Error("a new connection was refused while only authenticated connections were open")
			}
		})
	}
}

// spoofed serves s on a listener whose connections come from the address
// the test names, so one test can be several sources.
type spoofed struct {
	real net.Listener
	feed *feedListener
}

// spoofConn is a connection that says it comes from remote.
type spoofConn struct {
	net.Conn
	remote net.Addr
}

func (c spoofConn) RemoteAddr() net.Addr { return c.remote }

// feedListener is a listener whose connections the test hands it.
type feedListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	addr  net.Addr
}

func (l *feedListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *feedListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *feedListener) Addr() net.Addr { return l.addr }

func serveSpoofed(t *testing.T, s *Server) *spoofed {
	t.Helper()
	real, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	feed := &feedListener{conns: make(chan net.Conn), done: make(chan struct{}), addr: real.Addr()}
	go func() { _ = s.sshServer.Serve(feed) }()
	t.Cleanup(func() { _ = s.sshServer.Close(); _ = feed.Close(); _ = real.Close() })
	return &spoofed{real: real, feed: feed}
}

// dial opens a connection to the server that comes from host (port 4000).
func (sp *spoofed) dial(t *testing.T, host string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", sp.real.Addr().String(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	server, err := sp.real.Accept()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case sp.feed.conns <- spoofConn{Conn: server, remote: &net.TCPAddr{IP: net.ParseIP(host), Port: 4000}}:
	case <-time.After(10 * time.Second):
		t.Fatal("the server accepted no connection")
	}
	return c
}

// sshLogin authenticates as box with signer over c, a connection to addr.
func sshLogin(c net.Conn, addr string, signer gossh.Signer) error {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	sc, chans, reqs, err := gossh.NewClientConn(c, addr, &gossh.ClientConfig{
		User:            "box",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		return err
	}
	_ = c.SetDeadline(time.Time{})
	go gossh.DiscardRequests(reqs)
	go func() {
		for ch := range chans {
			_ = ch.Reject(gossh.Prohibited, "test")
		}
	}()
	_ = sc
	return nil
}

// fakeClock is a clock the test moves.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// newSpoofedPreauth is a server in mode with a cap of max and a share of
// perSource, whose log lines read a fake clock, served on a spoofing
// listener.
func newSpoofedPreauth(t *testing.T, mode preauthMode, max, perSource int) (*Server, *spoofed, *fakeClock, *syncLog, gossh.Signer) {
	t.Helper()
	known := testSigner(t)
	s, err := NewServer(nil, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil, mode.auth(gossh.FingerprintSHA256(known.PublicKey())))
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)}
	s.maxUnauthenticated, s.maxUnauthenticatedPerSource, s.now = max, perSource, clock.Now
	out := captureLog(t)
	return s, serveSpoofed(t, s), clock, out, known
}

// capLines is every cap refusal line of the log.
func capLines(text string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(line, "SSH: refusing ") {
			out.WriteString(line)
		}
	}
	return out.String()
}

// waitCapLines waits until the log's cap lines are want (or, with contains,
// hold it), and fails the test with what they are if they do not within ten
// seconds. A refusal's line is written after its connection is closed, so a
// test that has seen the close may look before the line is there.
func waitCapLines(t *testing.T, out *syncLog, what, want string, contains bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := capLines(out.String())
		if got == want || contains && strings.Contains(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("cap lines %s:\n got %q\nwant %q", what, got, want)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestPreauthShare: a source that holds its share of unauthenticated
// connections has its next one closed at once, logged with its address at
// most once a minute, while another source still gets in and logs in. An
// IPv6 source is its /64.
func TestPreauthShare(t *testing.T) {
	for _, mode := range preauthModes() {
		t.Run(mode.name, func(t *testing.T) {
			s, sp, clock, out, known := newSpoofedPreauth(t, mode, 10, 2)
			sp.dial(t, "192.0.2.1")
			sp.dial(t, "192.0.2.1")
			waitCounts(t, s, 2)

			if !closedByServer(sp.dial(t, "192.0.2.1")) {
				t.Fatal("the third connection from one address was not closed")
			}
			if !closedByServer(sp.dial(t, "192.0.2.1")) {
				t.Fatal("the fourth connection from one address was not closed")
			}
			// Another address gets in, and logs in, meanwhile.
			other := sp.dial(t, "198.51.100.7")
			if err := sshLogin(other, sp.real.Addr().String(), known); err != nil {
				t.Fatalf("a connection from another address while one was refused: %v", err)
			}
			waitCounts(t, s, 2)
			want := "SSH: refusing connections from 192.0.2.1: 2 are not yet authenticated (at most 2 from one address, 10 in all)\n"
			waitCapLines(t, out, "within the minute", want, false)
			clock.advance(time.Minute)
			if !closedByServer(sp.dial(t, "192.0.2.1")) {
				t.Fatal("the connection a minute later was not closed")
			}
			waitCapLines(t, out, "a minute later", want+want, false)

			// IPv6: the share is by /64.
			sp.dial(t, "2001:db8:1:2::a")
			sp.dial(t, "2001:db8:1:2::b")
			waitCounts(t, s, 4)
			if !closedByServer(sp.dial(t, "2001:db8:1:2:ffff::1")) {
				t.Fatal("a third connection from one IPv6 /64 was not closed")
			}
			waitCapLines(t, out, "for the /64", "SSH: refusing connections from 2001:db8:1:2::/64: 2 are not yet authenticated", true)
		})
	}
}

// TestPreauthCap: when the cap is full, a new connection from any source is
// closed at once, logged at most once a minute with the source holding the
// most; a place given back lets the next one in.
func TestPreauthCap(t *testing.T) {
	for _, mode := range preauthModes() {
		t.Run(mode.name, func(t *testing.T) {
			s, sp, clock, out, known := newSpoofedPreauth(t, mode, 3, 2)
			a := sp.dial(t, "192.0.2.1")
			sp.dial(t, "192.0.2.1")
			sp.dial(t, "203.0.113.5")
			waitCounts(t, s, 3)

			if !closedByServer(sp.dial(t, "198.51.100.7")) {
				t.Fatal("the connection past the cap was not closed")
			}
			if !closedByServer(sp.dial(t, "198.51.100.8")) {
				t.Fatal("the second connection past the cap was not closed")
			}
			want := "SSH: refusing new connections: 3 are not yet authenticated; the most from one address: 192.0.2.1 (2)\n"
			waitCapLines(t, out, "within the minute", want, false)
			clock.advance(time.Minute)
			if !closedByServer(sp.dial(t, "198.51.100.7")) {
				t.Fatal("the connection past the cap a minute later was not closed")
			}
			waitCapLines(t, out, "a minute later", want+want, false)

			_ = a.Close()
			waitCounts(t, s, 2)
			if err := sshLogin(sp.dial(t, "198.51.100.7"), sp.real.Addr().String(), known); err != nil {
				t.Fatalf("a connection after a place was given back: %v", err)
			}
			waitCounts(t, s, 2)
		})
	}
}

// TestPreauthTotalLineNamesTheTopSource: the cap's line names the source
// holding the most, the first in byte order among equals, an IPv6 /64
// included.
func TestPreauthTotalLineNamesTheTopSource(t *testing.T) {
	s := &Server{maxUnauthenticated: 4, maxUnauthenticatedPerSource: 2, now: time.Now}
	for _, src := range []string{"203.0.113.5", "2001:db8:1:2::/64", "203.0.113.5", "2001:db8:1:2::/64"} {
		if ok, line := s.preauthEnter(src); !ok {
			t.Fatalf("%s was refused below the cap: %q", src, line)
		}
	}
	ok, line := s.preauthEnter("198.51.100.7")
	want := "SSH: refusing new connections: 4 are not yet authenticated; the most from one address: 2001:db8:1:2::/64 (2)"
	if ok || line != want {
		t.Errorf("past the cap: %v %q, want refused with %q", ok, line, want)
	}
}

// TestPreauthRefusalRecordForgetsOldSources: the record of per-source
// refusal lines holds only the sources logged in the last minute: an
// unrelated source's old record is dropped when another is logged, while a
// recent one still holds its line back.
func TestPreauthRefusalRecordForgetsOldSources(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)}
	s := &Server{now: clock.Now}
	for _, step := range []struct {
		wait   time.Duration
		source string
		logs   bool
		record []string
	}{
		{0, "192.0.2.1", true, []string{"192.0.2.1"}},
		{30 * time.Second, "192.0.2.2", true, []string{"192.0.2.1", "192.0.2.2"}},
		{0, "192.0.2.1", false, []string{"192.0.2.1", "192.0.2.2"}},
		{30 * time.Second, "192.0.2.3", true, []string{"192.0.2.2", "192.0.2.3"}},
		{0, "192.0.2.2", false, []string{"192.0.2.2", "192.0.2.3"}},
		{0, "192.0.2.1", true, []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}},
		{2 * time.Minute, "192.0.2.4", true, []string{"192.0.2.4"}},
	} {
		clock.advance(step.wait)
		if got := s.preauthMayLog(step.source); got != step.logs {
			t.Errorf("after %s, %s may log = %v, want %v", step.wait, step.source, got, step.logs)
		}
		var record []string
		for src := range s.preLoggedSource {
			record = append(record, src)
		}
		slices.Sort(record)
		if !slices.Equal(record, step.record) || len(s.preLoggedOrder) != len(step.record) {
			t.Errorf("after %s %s: record %v (%d in order), want %v", step.wait, step.source, record, len(s.preLoggedOrder), step.record)
		}
	}
}

// TestPreauthSource: the source a connection's share is counted by.
func TestPreauthSource(t *testing.T) {
	for _, tc := range []struct {
		addr net.Addr
		want string
	}{
		{&net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 22}, "192.0.2.1"},
		{&net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.1"), Port: 22}, "192.0.2.1"},
		{&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}, "127.0.0.1"},
		{&net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:3:4:5:6"), Port: 22}, "2001:db8:1:2::/64"},
		{&net.TCPAddr{IP: net.ParseIP("::1"), Port: 22}, "::/64"},
		{&net.UnixAddr{Name: "/run/x.sock", Net: "unix"}, "/run/x.sock"},
		{nil, "-"},
	} {
		if got := preauthSource(tc.addr); got != tc.want {
			t.Errorf("preauthSource(%v) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// TestDurationText: a whole number of seconds reads as seconds.
func TestDurationText(t *testing.T) {
	for d, want := range map[time.Duration]string{
		defaultLoginGrace:       "120 seconds",
		time.Second:             "1 second",
		2 * time.Second:         "2 seconds",
		150 * time.Millisecond:  "150ms",
		1500 * time.Millisecond: "1.5s",
	} {
		if got := durationText(d); got != want {
			t.Errorf("durationText(%v) = %q, want %q", d, got, want)
		}
	}
}
