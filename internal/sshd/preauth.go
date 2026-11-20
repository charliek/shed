package sshd

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/gliderlabs/ssh"
)

// The limits on connections that have not authenticated yet, for a server
// that faces the internet. They apply in every auth mode, to every
// connection, loopback included: a proxy or tunnel that forwards to
// 127.0.0.1 would otherwise get past all of them.
//
//   - The login grace: a connection that has not authenticated
//     defaultLoginGrace after it was accepted is closed (OpenSSH's
//     LoginGraceTime). It is a timer, not a socket deadline: gliderlabs sets
//     the socket's deadline on every read and write (to none, with no idle or
//     max timeout), which would undo one.
//   - The cap: at most defaultMaxUnauthenticated connections are
//     unauthenticated at once (OpenSSH's MaxStartups full-drop point), and at
//     most defaultMaxUnauthenticatedPerSource of them from one source (one
//     IPv4 address, or one IPv6 /64), so one host cannot lock everyone else
//     out. A connection beyond either is closed as soon as it is accepted.
//   - One "SSH auth failed" line for each connection that offered a key and
//     closed without proving one, for fail2ban: the per-key DENIED lines
//     cannot tell a refused peer from a person whose agent offers several
//     keys before the one that logs in.
//
// A connection leaves both counts once, when it authenticates or when it
// closes, whichever comes first, and its grace timer stops then too.

const (
	// defaultLoginGrace is how long a connection may take to authenticate.
	defaultLoginGrace = 120 * time.Second
	// defaultMaxUnauthenticated is how many connections may be
	// unauthenticated at once.
	defaultMaxUnauthenticated = 100
	// defaultMaxUnauthenticatedPerSource is how many of them may come from
	// one source (preauthSource).
	defaultMaxUnauthenticatedPerSource = 10
	// preauthLogEvery is how often a cap refusal is logged at most: per
	// source for the per-source share, and once for the total.
	preauthLogEvery = time.Minute
)

// graceTimer is a connection's login-grace timer (a *time.Timer).
type graceTimer interface{ Stop() bool }

// realAfterFunc is the login-grace timer of a server that no test overrides.
func realAfterFunc(d time.Duration, f func()) graceTimer { return time.AfterFunc(d, f) }

// preauthConnKey is the connection-context key of a connection's preauthConn.
type preauthConnKey struct{}

// preauthConn is a connection from its accept until it authenticates or
// closes: it holds a place in the server's counts and runs the grace timer.
// It stays the connection's net.Conn after it authenticates, doing nothing
// more than pass its calls on.
type preauthConn struct {
	net.Conn
	s      *Server
	source string

	mu sync.Mutex
	// timer is the grace timer, stopped when the connection authenticates
	// or closes.
	timer graceTimer
	// counted: the connection holds a place in the server's counts.
	counted bool
	// authenticated: a key was proven and the connection is bound to it.
	authenticated bool
	// expired: the grace ended first; the connection is being closed and
	// never authenticates.
	expired bool
	// closed: the socket has been closed (shut).
	closed bool
	// keys is how many public-key offers the server examined on the
	// connection (each call of handlePublicKey), and user the username of
	// the last one. A key the client offers twice can count twice; a repeat
	// that x/crypto answers from its cache of the last key is not examined
	// again and does not count.
	keys int
	user string
}

// admitConn is the server's gliderlabs ConnCallback. It refuses a connection
// beyond the cap or its source's share (nil, which gliderlabs closes), and
// otherwise counts it, starts its grace timer and wraps it in a preauthConn,
// kept on the connection's context. On a server that tracks connections the
// preauthConn is then wrapped by wrapConn.
func (s *Server) admitConn(ctx ssh.Context, conn net.Conn) net.Conn {
	source := preauthSource(conn.RemoteAddr())
	if ok, line := s.preauthEnter(source); !ok {
		// Closed here, before the refusal line is written, so a slow or
		// blocked log writer never keeps the socket open. gliderlabs closes
		// it again when this returns nil, which does nothing more.
		_ = conn.Close()
		if line != "" {
			log.Print(line)
		}
		return nil
	}
	c := &preauthConn{Conn: conn, s: s, source: source, counted: true}
	ctx.SetValue(preauthConnKey{}, c)
	// Under c.mu, so the timer cannot fire before it is recorded.
	c.mu.Lock()
	c.timer = s.afterFunc(s.loginGrace, c.graceExpired)
	c.mu.Unlock()
	if s.keyAuth.TrackConnections {
		return s.wrapConn(ctx, c)
	}
	return c
}

// preauthEnter counts a new connection from source and reports true, unless
// the source's share or the cap is full; then it reports false and the
// refusal line to write, which is empty when one was written for the same
// refusal in the last minute (per source, and once for the cap). It writes
// nothing itself: the caller closes the connection first.
func (s *Server) preauthEnter(source string) (bool, string) {
	var line string
	s.preMu.Lock()
	switch {
	case s.preBySource[source] >= s.maxUnauthenticatedPerSource:
		if s.preauthMayLog(source) {
			line = fmt.Sprintf("SSH: refusing connections from %s: %d are not yet authenticated (at most %d from one address, %d in all)",
				source, s.preBySource[source], s.maxUnauthenticatedPerSource, s.maxUnauthenticated)
		}
	case s.preTotal >= s.maxUnauthenticated:
		now := s.now()
		if s.preLoggedTotal.IsZero() || now.Sub(s.preLoggedTotal) >= preauthLogEvery {
			s.preLoggedTotal = now
			top, n := s.preauthTopLocked()
			line = fmt.Sprintf("SSH: refusing new connections: %d are not yet authenticated; the most from one address: %s (%d)", s.preTotal, top, n)
		}
	default:
		s.preTotal++
		if s.preBySource == nil {
			s.preBySource = map[string]int{}
		}
		s.preBySource[source]++
		s.preMu.Unlock()
		return true, ""
	}
	s.preMu.Unlock()
	return false, line
}

// preauthLogged is one per-source refusal line, in the order they were
// written (Server.preLoggedOrder).
type preauthLogged struct {
	source string
	at     time.Time
}

// preauthMayLog reports whether a per-source refusal of source may be logged
// now (none in the last minute), and records it if so. The record holds only
// sources logged in the last minute: each call first drops the records more
// than a minute old from the front of preLoggedOrder, which is in the order
// they were written, and stops at the first that is not. So each record is
// dropped once, and a call examines only the records it drops and one more,
// however many sources a flood of refusals has named. The caller holds
// s.preMu.
func (s *Server) preauthMayLog(source string) bool {
	now := s.now()
	for len(s.preLoggedOrder) > 0 && now.Sub(s.preLoggedOrder[0].at) >= preauthLogEvery {
		old := s.preLoggedOrder[0]
		s.preLoggedOrder[0] = preauthLogged{}
		s.preLoggedOrder = s.preLoggedOrder[1:]
		// A source logged again since has a newer record, kept.
		if at, ok := s.preLoggedSource[old.source]; ok && at.Equal(old.at) {
			delete(s.preLoggedSource, old.source)
		}
	}
	if at, ok := s.preLoggedSource[source]; ok && now.Sub(at) < preauthLogEvery {
		return false
	}
	if s.preLoggedSource == nil {
		s.preLoggedSource = map[string]time.Time{}
	}
	s.preLoggedSource[source] = now
	s.preLoggedOrder = append(s.preLoggedOrder, preauthLogged{source: source, at: now})
	return true
}

// preauthTopLocked is the source holding the most unauthenticated
// connections, and how many (the first in byte order among equals). The
// caller holds s.preMu.
func (s *Server) preauthTopLocked() (string, int) {
	top, most := "-", 0
	for src, n := range s.preBySource {
		if n > most || n == most && src < top {
			top, most = src, n
		}
	}
	return top, most
}

// preauthLeave gives back a connection's place in the counts.
func (s *Server) preauthLeave(source string) {
	s.preMu.Lock()
	s.preTotal--
	if s.preBySource[source]--; s.preBySource[source] <= 0 {
		delete(s.preBySource, source)
	}
	s.preMu.Unlock()
}

// preauthCounts is how many connections are unauthenticated, in all and by
// source.
func (s *Server) preauthCounts() (int, map[string]int) {
	s.preMu.Lock()
	defer s.preMu.Unlock()
	by := make(map[string]int, len(s.preBySource))
	for src, n := range s.preBySource {
		by[src] = n
	}
	return s.preTotal, by
}

// preauthConnFrom is the preauthConn of the connection ctx belongs to, if it
// has one (every connection the server accepts does; a test that calls an
// auth callback directly may give none).
func preauthConnFrom(ctx ssh.Context) (*preauthConn, bool) {
	c, ok := ctx.Value(preauthConnKey{}).(*preauthConn)
	return c, ok
}

// noteOffer records that the server examined a public-key offer, as user, on
// the connection ctx belongs to (the keys= of its "SSH auth failed" line).
func noteOffer(ctx ssh.Context, user string) {
	c, ok := preauthConnFrom(ctx)
	if !ok {
		return
	}
	c.mu.Lock()
	c.keys++
	c.user = user
	c.mu.Unlock()
}

// preauthAuthenticated marks the connection ctx belongs to as authenticated:
// its grace timer stops and it leaves the counts. It fails, and the
// connection does not authenticate, when the connection is already closed or
// its grace has ended.
func preauthAuthenticated(ctx ssh.Context) error {
	c, ok := preauthConnFrom(ctx)
	if !ok {
		return nil
	}
	c.mu.Lock()
	if c.closed || c.expired {
		c.mu.Unlock()
		return net.ErrClosed
	}
	c.authenticated = true
	timer, counted := c.timer, c.counted
	c.counted = false
	c.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if counted {
		c.s.preauthLeave(c.source)
	}
	return nil
}

// graceExpired runs when the login grace ends: it closes the connection
// unless it has authenticated or closed already. The socket is closed and
// the place given back before any line is written, so a slow or blocked log
// writer never keeps the connection open or its place taken.
func (c *preauthConn) graceExpired() {
	c.mu.Lock()
	if c.authenticated || c.closed {
		c.mu.Unlock()
		return
	}
	c.expired = true
	c.mu.Unlock()
	// The connection's own goroutine, which sees its socket close, may close
	// it too and finish first; the grace line is written either way, once,
	// since only this call set expired.
	first, failed, _ := c.shut()
	log.Printf("SSH: closed a connection from %s that did not authenticate within %s", logRemote(c.RemoteAddr()), durationText(c.s.loginGrace))
	if first {
		c.closeLogged(failed)
	}
}

// Close closes the connection. The first close of a connection that has not
// authenticated stops its grace timer and gives back its place in the counts
// (shut), and then, when it offered a key, writes its "SSH auth failed" line.
func (c *preauthConn) Close() error {
	first, failed, err := c.shut()
	if first {
		c.closeLogged(failed)
	}
	return err
}

// shut closes the socket and, the first time, stops the grace timer and
// gives back the connection's place in the counts. It writes nothing, and
// holds no lock when it returns: it reports whether this was the first close
// and, for a connection that offered a key and never proved one, its
// "SSH auth failed" line, which the caller writes (closeLogged) when nothing
// waits on it. CloseConnectionsExcept shuts a connection in its sweep and
// writes the line afterwards, outside the sweep.
func (c *preauthConn) shut() (first bool, failed string, err error) {
	err = c.Conn.Close()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false, "", err
	}
	c.closed = true
	timer, counted := c.timer, c.counted
	c.counted = false
	if !c.authenticated && c.keys > 0 {
		failed = fmt.Sprintf("SSH auth failed: remote=%s user=%s keys=%d", logRemote(c.RemoteAddr()), logUser(c.user), c.keys)
	}
	c.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if counted {
		c.s.preauthLeave(c.source)
	}
	return true, failed, err
}

// closeLogged writes what a first close of the connection has to say (its
// "SSH auth failed" line, if any), after the close.
func (c *preauthConn) closeLogged(failed string) {
	if failed != "" {
		log.Print(failed)
	}
	if c.s.afterPreauthClose != nil {
		c.s.afterPreauthClose()
	}
}

// shutQuietly closes conn, a tracked connection's inner connection, for the
// sweep of CloseConnectionsExcept: through its preauthConn when it has one,
// which gives back its place in the counts without writing anything. It
// returns what is to be written about the close afterwards (nil when
// nothing is).
func shutQuietly(conn net.Conn) func() {
	c, ok := conn.(*preauthConn)
	if !ok {
		_ = conn.Close()
		return nil
	}
	first, failed, _ := c.shut()
	if !first {
		return nil
	}
	return func() { c.closeLogged(failed) }
}

// preauthSource is the source a connection's share of the cap is counted
// by: its IPv4 address, or the /64 of its IPv6 address (one host usually
// holds a whole /64). An address that is not an IP address counts as itself.
func preauthSource(addr net.Addr) string {
	if addr == nil {
		return "-"
	}
	var ip net.IP
	if tcp, ok := addr.(*net.TCPAddr); ok {
		ip = tcp.IP
	} else if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		ip = net.ParseIP(host)
	}
	if ip == nil {
		return addr.String()
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	mask := net.CIDRMask(64, 128)
	return (&net.IPNet{IP: ip.Mask(mask), Mask: mask}).String()
}

// durationText is a limit's duration as a log line says it: "120 seconds"
// for a whole number of seconds, Go's form otherwise (a test's short grace).
func durationText(d time.Duration) string {
	if d == time.Second {
		return "1 second"
	}
	if d > time.Second && d%time.Second == 0 {
		return fmt.Sprintf("%d seconds", d/time.Second)
	}
	return d.String()
}
