package sshd

import (
	"errors"
	"fmt"
	"log"
	"net"
	"sort"

	"github.com/gliderlabs/ssh"
)

// Connection tracking (KeyAuth.TrackConnections): on a server whose users
// change while it runs (a directory server), every authenticated connection
// is kept by the name of the user it is bound to, so that when a user leaves
// the list (or the list's trust time ends) CloseConnectionsExcept closes
// their connections, and with them every channel open on them. A connection
// whose user is still a user is never closed by it, even when its key has
// since moved to another user: that connection opens no new channel
// (principalFor), and the channels already open on it stay open.
//
// The order that makes this hold whatever runs in between: the user lookup
// publishes a new state (its users) before it asks for the connections of
// users it no longer has to be closed, and an authenticating connection is
// tracked before it checks, against the lookup as it is then, that its key
// still resolves to the user its identity is bound to. Both the tracking and
// the closing hold connMu. So either the closing sees the connection tracked
// and closes it, or the connection was tracked after the closing and its
// check sees the new state, and it does not authenticate.

// trackedConnKey is the connection-context key of a connection's trackedConn.
type trackedConnKey struct{}

// trackedConn is a connection of a server with KeyAuth.TrackConnections. It
// is in the server's conns, under name, from its authentication until it
// closes or CloseConnectionsExcept closes it.
type trackedConn struct {
	net.Conn
	s *Server
	// name and tracked say where the connection is in s.conns; closed marks
	// a connection that has been closed, which is never tracked again. All
	// guarded by s.connMu.
	name    string
	tracked bool
	closed  bool
}

// Close closes the connection and stops tracking it.
func (c *trackedConn) Close() error {
	c.s.connMu.Lock()
	c.closed = true
	c.s.untrackLocked(c)
	c.s.connMu.Unlock()
	err := c.Conn.Close()
	if c.s.afterClose != nil {
		c.s.afterClose()
	}
	return err
}

// wrapConn is the gliderlabs ConnCallback of a server with
// KeyAuth.TrackConnections: it wraps each new connection in a trackedConn,
// kept on the connection's context until it authenticates.
func (s *Server) wrapConn(ctx ssh.Context, conn net.Conn) net.Conn {
	c := &trackedConn{Conn: conn, s: s}
	ctx.SetValue(trackedConnKey{}, c)
	return c
}

// errUntracked is a connection of a tracking server with no trackedConn on
// its context, which wrapConn never lets happen.
var errUntracked = errors.New("this connection is not tracked")

// trackAuthenticated tracks the connection on ctx, whose identity id has just
// been resolved, under the name of the user id is bound to, and then checks
// that id's key still resolves to that user (principalForIdentity). If it
// does not (the user lookup changed between the resolution and the tracking)
// the connection is not tracked and does not authenticate. See the order
// described at the top of this file.
func (s *Server) trackAuthenticated(ctx ssh.Context, id connIdentity) error {
	c, ok := ctx.Value(trackedConnKey{}).(*trackedConn)
	if !ok {
		return errUntracked
	}
	if s.beforeTrack != nil {
		s.beforeTrack()
	}
	s.connMu.Lock()
	if c.closed {
		s.connMu.Unlock()
		return net.ErrClosed
	}
	s.untrackLocked(c)
	if s.conns == nil {
		s.conns = map[string]map[*trackedConn]struct{}{}
	}
	set := s.conns[id.bound.Name]
	if set == nil {
		set = map[*trackedConn]struct{}{}
		s.conns[id.bound.Name] = set
	}
	set[c] = struct{}{}
	c.name, c.tracked = id.bound.Name, true
	s.connMu.Unlock()

	if _, err := s.principalForIdentity(id); err != nil {
		s.connMu.Lock()
		s.untrackLocked(c)
		s.connMu.Unlock()
		return err
	}
	return nil
}

// untrackLocked removes c from s.conns, if it is there. The caller holds
// s.connMu.
func (s *Server) untrackLocked(c *trackedConn) {
	if !c.tracked {
		return
	}
	if set := s.conns[c.name]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(s.conns, c.name)
		}
	}
	c.name, c.tracked = "", false
}

// CloseConnectionsExcept closes every tracked connection whose user keep
// rejects, which ends every channel open on it, and returns how many
// connections it closed. It closes the socket of every such connection first,
// and only after that sweep are its log lines written (one per user, with
// the count), from a goroutine of their own, outside every lock, so a slow
// or blocked log writer never delays or prevents a close, and never holds
// the publication that called it (nor, through it, the next publication or
// an unknown-key login waiting for the directory runtime's lock). keep is
// called with each tracked user's name, under the server's connection lock:
// it must not block or call back into the server. With
// KeyAuth.TrackConnections unset nothing is tracked, and it closes nothing.
//
// It is what a directory server's publication hook does, inside the
// publication: it does no network or other blocking work beyond closing the
// sockets.
func (s *Server) CloseConnectionsExcept(keep func(name string) bool) int {
	byUser := map[string][]*trackedConn{}
	s.connMu.Lock()
	for name, set := range s.conns {
		if keep(name) {
			continue
		}
		for c := range set {
			byUser[name] = append(byUser[name], c)
			c.closed = true
			c.name, c.tracked = "", false
		}
		delete(s.conns, name)
	}
	s.connMu.Unlock()

	// The sweep: every socket, before any log line.
	closed := 0
	for _, conns := range byUser {
		for _, c := range conns {
			_ = c.Conn.Close()
		}
		closed += len(conns)
	}

	if closed == 0 {
		return 0
	}
	names := make([]string, 0, len(byUser))
	for name := range byUser {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, len(names))
	for i, name := range names {
		lines[i] = fmt.Sprintf("SSH: closed %d connection(s) of user %q: no longer a user of this server", len(byUser[name]), name)
	}
	// Not joined, by choice: a line not yet written when the process exits is lost.
	go func() {
		for _, line := range lines {
			log.Print(line)
		}
	}()
	return closed
}
