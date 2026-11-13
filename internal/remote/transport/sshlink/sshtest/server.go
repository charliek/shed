// Package sshtest holds the two SSH servers the SSH link is tested against:
// an in-process server built on golang.org/x/crypto/ssh, whose faults a test
// can script (Server), and a real OpenSSH sshd in a container laid out like a
// cloud guest (SSHD).
//
// It is test support: it imports testing and is imported only by tests.
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/remote/guest"
)

// NewKey returns a fresh ed25519 key as an SSH signer.
func NewKey(t testing.TB) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// PublicKeyLine renders a key's public half in authorized_keys form with no
// comment and no newline.
func PublicKeyLine(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// Options configures a Server.
type Options struct {
	// HostKey is the server's host key. Nil generates an ed25519 key.
	HostKey ssh.Signer
	// AuthorizedKeys are the client keys the server accepts.
	AuthorizedKeys []ssh.PublicKey
	// User is the only login name accepted. Default guest.LinkUser.
	User string
	// DenyForwarding makes the server refuse every forwarded unix socket
	// as "administratively prohibited", like an sshd whose config forbids
	// it.
	DenyForwarding bool
}

// Server is an in-process SSH server that does one useful thing, the thing
// the SSH link asks of a guest's sshd: it forwards direct-streamlocal
// channels to unix sockets on this host, with OpenSSH's half-close
// behaviour. Everything else about it is there to be broken on purpose.
type Server struct {
	t    testing.TB
	opts Options

	mu       sync.Mutex
	hostKey  ssh.Signer
	listener net.Listener
	conns    map[*serverConn]struct{}
	thaw     chan struct{} // non-nil while frozen; closed by Thaw
	closed   bool

	accepted int // TCP connections accepted
	logins   int // handshakes that authenticated
	refusals int // logins refused
	forwards int // direct-streamlocal channels accepted
	wg       sync.WaitGroup
}

type serverConn struct {
	raw *gateConn
	ssh *ssh.ServerConn // nil until the handshake is done
}

// NewServer starts a server on a loopback port. It stops with the test.
func NewServer(t testing.TB, opts Options) *Server {
	t.Helper()
	if opts.User == "" {
		opts.User = guest.LinkUser
	}
	if opts.HostKey == nil {
		opts.HostKey = NewKey(t)
	}
	s := &Server{
		t:       t,
		opts:    opts,
		hostKey: opts.HostKey,
		conns:   make(map[*serverConn]struct{}),
	}
	s.listen()
	t.Cleanup(s.Close)
	return s
}

func (s *Server) listen() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatalf("sshtest: listen: %v", err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	s.wg.Add(1)
	go s.acceptLoop(ln)
}

// Addr returns the address the server listens on now. It changes at Restart.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// HostKey returns the current host key's public half in pin form.
func (s *Server) HostKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return PublicKeyLine(s.hostKey.PublicKey())
}

// SetHostKey changes the host key that later connections are offered.
func (s *Server) SetHostKey(key ssh.Signer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostKey = key
}

// Accepted returns how many TCP connections the server has accepted. Logins
// returns how many of them authenticated, Refusals how many were refused, and
// Forwards how many unix-socket channels were opened.
func (s *Server) Accepted() int { return s.count(&s.accepted) }
func (s *Server) Logins() int   { return s.count(&s.logins) }
func (s *Server) Refusals() int { return s.count(&s.refusals) }
func (s *Server) Forwards() int { return s.count(&s.forwards) }

func (s *Server) count(n *int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *n
}

// StopListening closes the listener, so new connections are refused.
// Established connections stay up.
func (s *Server) StopListening() {
	s.mu.Lock()
	ln := s.listener
	s.listener = nil
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
}

// DropConnections closes every established connection, like an sshd that was
// killed.
func (s *Server) DropConnections() {
	s.mu.Lock()
	conns := make([]*serverConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.raw.Close()
	}
}

// Restart drops every connection and listens again on a new port, like a
// guest that was stopped and started and came back at another address.
func (s *Server) Restart() {
	s.StopListening()
	s.DropConnections()
	s.listen()
}

// Freeze turns the server into a black hole: from now on it reads nothing it
// would act on and writes nothing, on established and new connections alike,
// while every TCP connection stays open. Thaw undoes it.
func (s *Server) Freeze() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.thaw == nil {
		s.thaw = make(chan struct{})
	}
}

// Thaw ends a Freeze.
func (s *Server) Thaw() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.thaw != nil {
		close(s.thaw)
		s.thaw = nil
	}
}

// OpenChannel asks every connected client to accept a channel of the given
// type, as a compromised guest might, and returns one error per client. A nil
// entry means the client accepted it.
func (s *Server) OpenChannel(chanType string, payload []byte) []error {
	s.mu.Lock()
	var conns []*ssh.ServerConn
	for c := range s.conns {
		if c.ssh != nil {
			conns = append(conns, c.ssh)
		}
	}
	s.mu.Unlock()

	errs := make([]error, 0, len(conns))
	for _, c := range conns {
		ch, reqs, err := c.OpenChannel(chanType, payload)
		if err == nil {
			go ssh.DiscardRequests(reqs)
			ch.Close()
		}
		errs = append(errs, err)
	}
	return errs
}

// Close stops the server.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	s.Thaw()
	s.StopListening()
	s.DropConnections()
	s.wg.Wait()
}

func (s *Server) acceptLoop(ln net.Listener) {
	defer s.wg.Done()
	for {
		raw, err := ln.Accept()
		if err != nil {
			return
		}
		c := &serverConn{raw: &gateConn{Conn: raw, s: s, closed: make(chan struct{})}}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			raw.Close()
			return
		}
		s.accepted++
		s.conns[c] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				c.raw.Close()
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
			}()
			s.serve(c)
		}()
	}
}

func (s *Server) serve(c *serverConn) {
	s.mu.Lock()
	hostKey := s.hostKey
	s.mu.Unlock()

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() != s.opts.User {
				return nil, fmt.Errorf("user %q is not allowed", meta.User())
			}
			for _, allowed := range s.opts.AuthorizedKeys {
				if allowed.Type() == key.Type() && string(allowed.Marshal()) == string(key.Marshal()) {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, errors.New("key not authorized")
		},
	}
	config.AddHostKey(hostKey)

	conn, chans, reqs, err := ssh.NewServerConn(c.raw, config)
	if err != nil {
		var authErr *ssh.ServerAuthError
		if errors.As(err, &authErr) {
			s.mu.Lock()
			s.refusals++
			s.mu.Unlock()
		}
		return
	}
	s.mu.Lock()
	s.logins++
	c.ssh = conn
	s.mu.Unlock()

	// DiscardRequests answers every global request that wants a reply
	// with a failure, which is also how OpenSSH answers a keepalive.
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "direct-streamlocal@openssh.com" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only unix-socket forwarding is served")
			continue
		}
		go s.forward(newCh)
	}
}

// forward serves one direct-streamlocal channel the way sshd does: connect to
// the socket, then copy both ways, turning an end-of-file on either side into
// a half-close on the other.
func (s *Server) forward(newCh ssh.NewChannel) {
	var req struct {
		SocketPath string
		Reserved0  string
		Reserved1  uint32
	}
	if err := ssh.Unmarshal(newCh.ExtraData(), &req); err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, "bad direct-streamlocal request")
		return
	}
	if s.opts.DenyForwarding {
		_ = newCh.Reject(ssh.Prohibited, "administratively prohibited")
		return
	}
	sock, err := net.DialTimeout("unix", req.SocketPath, 5*time.Second)
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, "connect failed")
		return
	}
	unix := sock.(*net.UnixConn)
	ch, reqs, err := newCh.Accept()
	if err != nil {
		unix.Close()
		return
	}
	s.mu.Lock()
	s.forwards++
	s.mu.Unlock()

	// The request channel closes when the SSH channel is closed, by the
	// client or with the connection. That is the moment to drop the
	// socket, whatever the copies are doing.
	go func() {
		for r := range reqs {
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
		unix.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(unix, ch)
		_ = unix.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, unix)
		_ = ch.CloseWrite()
	}()
	wg.Wait()
	ch.Close()
	unix.Close()
}

// gateConn is a server-side TCP connection that Freeze can stall.
type gateConn struct {
	net.Conn
	s      *Server
	closed chan struct{}
	once   sync.Once
}

// wait blocks while the server is frozen and the connection is open.
func (c *gateConn) wait() {
	for {
		c.s.mu.Lock()
		thaw := c.s.thaw
		c.s.mu.Unlock()
		if thaw == nil {
			return
		}
		select {
		case <-thaw:
		case <-c.closed:
			return
		}
	}
}

func (c *gateConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.wait() // what was read while frozen is held back
	return n, err
}

func (c *gateConn) Write(p []byte) (int, error) {
	c.wait()
	return c.Conn.Write(p)
}

func (c *gateConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
