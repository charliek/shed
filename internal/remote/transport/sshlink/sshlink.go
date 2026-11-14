// Package sshlink is the SSH transport of the remote backend: one SSH
// connection per shed, with every agent connection carried as a forwarded
// unix socket (a direct-streamlocal channel) to one of the agent's sockets in
// the guest.
//
// The server is always the client. A link logs in as the guest's link user
// with the environment's key, accepts exactly one host key (the shed's pin),
// opens channels and nothing else: it never asks the guest to listen, never
// forwards an agent, and refuses every channel the guest tries to open.
//
// # Connection lifecycle
//
// A link connects on its first Dial and again on the first Dial after the
// connection was lost. Before each connection attempt it asks Target.Addr
// where the guest is, because a cloud guest's address changes at each start.
// Failed attempts are spaced transport.MinConnectInterval apart; a Dial inside
// that interval returns the last error at once. Concurrent Dials share one
// attempt.
//
// A keepalive request goes out every KeepaliveInterval. When KeepaliveMisses
// intervals pass with no reply the connection is closed, which ends every
// stream on it; a connection into a black hole is therefore noticed within
// (KeepaliveMisses+1) intervals.
//
// # Host keys
//
// With a pin, any other key is transport.ErrHostKeyMismatch. That usually
// means the address now belongs to another machine, so the link asks
// Target.Addr once more and tries the new address if it differs. It never
// learns a second key. Without a pin the presented key goes to
// HostKeyStore.Learn, which the backend answers from what the guest published
// through the provider.
package sshlink

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/transport"
)

// maxRefusalLogs is how many channels opened by the guest are logged one by
// one on a connection before the rest are only counted.
const maxRefusalLogs = 8

// Defaults for Config.
const (
	DefaultConnectTimeout    = 5 * time.Second
	DefaultHandshakeTimeout  = 10 * time.Second
	DefaultKeepaliveInterval = 10 * time.Second
	DefaultKeepaliveMisses   = 2

	// resolveTimeout bounds one Target.Addr or HostKeyStore.Pin call.
	resolveTimeout = 15 * time.Second
	// learnTimeout bounds HostKeyStore.Learn, which writes the pin through
	// the provider and reads it back.
	learnTimeout = 60 * time.Second

	keepaliveRequest = "keepalive@openssh.com"
	clientVersion    = "SSH-2.0-shed-link"
)

// Config configures a Factory. The zero value of every field but Keys is
// usable.
type Config struct {
	// Keys are the server's client keys, tried in order. One is normal;
	// two while a key is being rotated. Required.
	Keys []ssh.Signer
	// User is the login name. Default guest.LinkUser.
	User string
	// SocketDir is the directory of the agent's sockets in the guest.
	// Default transport.GuestSocketDir.
	SocketDir string

	// ConnectTimeout bounds the TCP connect, HandshakeTimeout the SSH
	// handshake that follows (not counting HostKeyStore.Learn).
	ConnectTimeout   time.Duration
	HandshakeTimeout time.Duration
	// KeepaliveInterval is the time between keepalive requests;
	// KeepaliveMisses is how many intervals may pass without a reply
	// before the connection is closed.
	KeepaliveInterval time.Duration
	KeepaliveMisses   int
	// MinConnectInterval is the spacing of failed connection attempts.
	// Default transport.MinConnectInterval; tests shorten it.
	MinConnectInterval time.Duration

	// Logf receives connection events. Default log.Printf.
	Logf func(format string, args ...any)

	// DialContext opens the TCP connection. Default net.Dialer. Tests use
	// it to put a fault-injecting conn under the SSH connection.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	// rekeyThreshold overrides how many bytes pass between key exchanges.
	// Zero is x/crypto's default. Tests lower it.
	rekeyThreshold uint64
}

func (c Config) withDefaults() Config {
	if c.User == "" {
		c.User = guest.LinkUser
	}
	if c.SocketDir == "" {
		c.SocketDir = transport.GuestSocketDir
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if c.KeepaliveInterval <= 0 {
		c.KeepaliveInterval = DefaultKeepaliveInterval
	}
	if c.KeepaliveMisses <= 0 {
		c.KeepaliveMisses = DefaultKeepaliveMisses
	}
	if c.MinConnectInterval <= 0 {
		c.MinConnectInterval = transport.MinConnectInterval
	}
	if c.Logf == nil {
		c.Logf = log.Printf
	}
	if c.DialContext == nil {
		c.DialContext = (&net.Dialer{}).DialContext
	}
	return c
}

// Factory makes SSH links. It implements transport.Factory.
type Factory struct {
	cfg Config
}

var _ transport.Factory = (*Factory)(nil)

// New returns a Factory.
func New(cfg Config) (*Factory, error) {
	if len(cfg.Keys) == 0 {
		return nil, errors.New("sshlink: no client key")
	}
	for _, k := range cfg.Keys {
		if k == nil {
			return nil, errors.New("sshlink: nil client key")
		}
	}
	return &Factory{cfg: cfg.withDefaults()}, nil
}

// Name returns transport.NameSSH.
func (f *Factory) Name() string { return transport.NameSSH }

// Open returns a link for the target. It does not connect.
func (f *Factory) Open(_ context.Context, target transport.Target) (transport.Link, error) {
	if target.Addr == nil {
		return nil, errors.New("sshlink: target has no Addr")
	}
	if target.HostKeys == nil {
		return nil, errors.New("sshlink: target has no HostKeys")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Link{
		cfg:    f.cfg,
		target: target,
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// Link is one shed's SSH link. It implements transport.Link.
type Link struct {
	cfg    Config
	target transport.Target

	// ctx ends when the link is closed. Connection attempts run on it, not
	// on a caller's context, so an attempt outlives the Dial that started
	// it and serves the next one.
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	cur      *conn     // the live connection, or nil
	flight   *flight   // the connection attempt in progress, or nil
	lastFail time.Time // when the last attempt failed; zero after a success
	lastErr  error

	connects atomic.Int64 // TCP connections attempted, for tests and logs
}

var _ transport.Link = (*Link)(nil)

// flight is one connection attempt, shared by every Dial that waits for it.
type flight struct {
	done chan struct{}
	conn *conn
	err  error
}

// conn is one established SSH connection.
type conn struct {
	client *ssh.Client
	tcp    *trackedConn
	addr   string
	dead   chan struct{} // closed when the connection has ended
}

// lost reports whether the connection has ended or is ending. A stream uses
// it to tell a guest's end-of-file from a dropped connection.
func (c *conn) lost() bool {
	if c.tcp.broken.Load() {
		return true
	}
	select {
	case <-c.dead:
		return true
	default:
		return false
	}
}

// trackedConn is the TCP connection under an SSH connection. It records the
// first failed Read or the Close before the SSH layer can react, so that by
// the time the SSH layer ends its channels, lost() is already true.
type trackedConn struct {
	net.Conn
	broken atomic.Bool
	// ack acknowledges received data at once; see quickAck. Nil does
	// nothing.
	ack func()
}

func (t *trackedConn) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if err != nil {
		t.broken.Store(true)
	} else if t.ack != nil {
		t.ack()
	}
	return n, err
}

func (t *trackedConn) Close() error {
	t.broken.Store(true)
	return t.Conn.Close()
}

// name describes the link's shed for errors and logs.
func (l *Link) name() string {
	switch {
	case l.target.ShedName != "" && l.target.ShedID != "":
		return l.target.ShedName + " (" + l.target.ShedID + ")"
	case l.target.ShedName != "":
		return l.target.ShedName
	case l.target.ShedID != "":
		return l.target.ShedID
	default:
		return "guest"
	}
}

// Dial opens a stream to the agent socket for port. See transport.Link.
func (l *Link) Dial(ctx context.Context, port uint32) (net.Conn, error) {
	path := transport.SocketPath(l.cfg.SocketDir, port)

	// Two rounds: when the connection turns out to be dead as the channel
	// is opened, drop it and connect once more.
	var lastErr error
	for range 2 {
		c, err := l.connection(ctx)
		if err != nil {
			return nil, err
		}
		nc, err := c.client.DialContext(ctx, "unix", path)
		if err == nil {
			ch, ok := nc.(channelConn)
			if !ok {
				nc.Close()
				return nil, fmt.Errorf("sshlink: %T has no CloseWrite", nc)
			}
			return newStream(ch, c), nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var refused *ssh.OpenChannelError
		if errors.As(err, &refused) {
			// The SSH connection is fine; sshd could not reach the
			// agent's socket (the agent is not up yet) or refuses
			// to forward at all.
			return nil, fmt.Errorf("sshlink: %s: open %s: %s (%s): %w",
				l.name(), path, refused.Message, refused.Reason, transport.ErrUnreachable)
		}
		lastErr = err
		l.drop(c, err)
	}
	if l.isClosed() {
		return nil, transport.ErrClosed
	}
	return nil, fmt.Errorf("sshlink: %s: open %s: %v: %w", l.name(), path, lastErr, transport.ErrUnreachable)
}

// Close ends the link and every stream opened from it.
func (l *Link) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	cur := l.cur
	l.cur = nil
	l.mu.Unlock()

	l.cancel() // ends a connection attempt in progress
	if cur != nil {
		_ = cur.client.Close()
	}
	return nil
}

func (l *Link) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// connection returns the live connection, establishing it if needed. Dials
// that arrive during an attempt wait for it; a Dial whose ctx ends first
// leaves the attempt running.
func (l *Link) connection(ctx context.Context) (*conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, transport.ErrClosed
	}
	if l.cur != nil {
		c := l.cur
		l.mu.Unlock()
		return c, nil
	}
	if l.flight == nil {
		if !l.lastFail.IsZero() && time.Since(l.lastFail) < l.cfg.MinConnectInterval {
			err := l.lastErr
			l.mu.Unlock()
			return nil, err
		}
		f := &flight{done: make(chan struct{})}
		l.flight = f
		go l.fly(f)
	}
	f := l.flight
	l.mu.Unlock()

	select {
	case <-f.done:
		return f.conn, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fly runs one connection attempt and publishes its result.
func (l *Link) fly(f *flight) {
	c, err := l.connect(l.ctx)

	l.mu.Lock()
	l.flight = nil
	switch {
	case l.closed:
		if c != nil {
			_ = c.client.Close()
			c = nil
		}
		err = transport.ErrClosed
	case err != nil:
		l.lastFail, l.lastErr = time.Now(), err
	default:
		l.lastFail, l.lastErr = time.Time{}, nil
		l.cur = c
	}
	f.conn, f.err = c, err
	l.mu.Unlock()
	close(f.done)

	if c != nil {
		l.cfg.Logf("sshlink: %s: connected to %s", l.name(), c.addr)
		go l.watch(c)
		go l.keepalive(c)
	}
}

// watch forgets a connection when it ends.
func (l *Link) watch(c *conn) {
	err := c.client.Wait()
	close(c.dead)
	l.drop(c, err)
}

// drop forgets c, if it is still the link's connection, and closes it.
func (l *Link) drop(c *conn, cause error) {
	l.mu.Lock()
	was := l.cur == c
	if was {
		l.cur = nil
	}
	closed := l.closed
	l.mu.Unlock()

	_ = c.client.Close()
	if was && !closed {
		l.cfg.Logf("sshlink: %s: connection to %s ended: %v", l.name(), c.addr, cause)
	}
}

// keepalive sends a keepalive request every interval and closes the
// connection when KeepaliveMisses intervals pass with a request unanswered.
// Any reply counts, including a refusal: OpenSSH answers this request with a
// failure.
func (l *Link) keepalive(c *conn) {
	ticker := time.NewTicker(l.cfg.KeepaliveInterval)
	defer ticker.Stop()

	replies := make(chan error, 1)
	waiting := false
	misses := 0
	for {
		select {
		case <-c.dead:
			return
		case err := <-replies:
			if err != nil {
				return // the connection is ending; watch reports it
			}
			waiting, misses = false, 0
		case <-ticker.C:
			if waiting {
				misses++
				if misses >= l.cfg.KeepaliveMisses {
					l.drop(c, fmt.Errorf("no keepalive reply in %v", time.Duration(misses)*l.cfg.KeepaliveInterval))
					return
				}
				continue
			}
			waiting = true
			go func() {
				_, _, err := c.client.SendRequest(keepaliveRequest, true, nil)
				replies <- err
			}()
		}
	}
}

// connect establishes the SSH connection: resolve the address, read the pin,
// connect, and on a host key mismatch resolve once more.
func (l *Link) connect(ctx context.Context) (*conn, error) {
	pin, err := l.pin(ctx)
	if err != nil {
		return nil, err
	}
	addr, err := l.resolve(ctx)
	if err != nil {
		return nil, err
	}
	c, err := l.connectTo(ctx, addr, pin)
	if err != nil && errors.Is(err, transport.ErrHostKeyMismatch) {
		// Another machine may hold a stale address. Ask again, once.
		// The pin does not change: a second key is never learned.
		if again, rerr := l.resolve(ctx); rerr == nil && again != addr {
			l.cfg.Logf("sshlink: %s: host key mismatch at %s; the address is now %s", l.name(), addr, again)
			return l.connectTo(ctx, again, pin)
		}
	}
	return c, err
}

// pin reads and parses the shed's pinned host key. A nil key means no pin.
func (l *Link) pin(ctx context.Context) (ssh.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	line, err := l.target.HostKeys.Pin(ctx)
	if err != nil {
		if l.ctx.Err() != nil {
			return nil, transport.ErrClosed
		}
		return nil, fmt.Errorf("sshlink: %s: read the pinned host key: %w", l.name(), err)
	}
	if strings.TrimSpace(line) == "" {
		return nil, nil
	}
	key, err := ParseHostKey(line)
	if err != nil {
		// A pin the server cannot read must not turn into "no pin".
		return nil, fmt.Errorf("sshlink: %s: pinned host key: %v: %w", l.name(), err, transport.ErrHostKeyMismatch)
	}
	return key, nil
}

// resolve asks the target where the guest is and returns host:port.
func (l *Link) resolve(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	addr, err := l.target.Addr(ctx)
	if err != nil {
		if l.ctx.Err() != nil {
			return "", transport.ErrClosed
		}
		return "", fmt.Errorf("sshlink: %s: resolve address: %w: %w", l.name(), err, transport.ErrUnreachable)
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("sshlink: %s: no address yet: %w", l.name(), transport.ErrUnreachable)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(strings.Trim(addr, "[]"), strconv.Itoa(guest.SSHPort))
	}
	return addr, nil
}

// connectTo makes one TCP connection and runs the SSH handshake on it.
func (l *Link) connectTo(ctx context.Context, addr string, pin ssh.PublicKey) (*conn, error) {
	l.connects.Add(1)

	dialCtx, cancel := context.WithTimeout(ctx, l.cfg.ConnectTimeout)
	raw, err := l.cfg.DialContext(dialCtx, "tcp", addr)
	cancel()
	if err != nil {
		if l.ctx.Err() != nil {
			return nil, transport.ErrClosed
		}
		return nil, fmt.Errorf("sshlink: %s: connect to %s: %w: %w", l.name(), addr, err, transport.ErrUnreachable)
	}
	tcp := &trackedConn{Conn: raw, ack: quickAck(raw)}

	// Closing the link aborts the handshake. A deadline on the connection
	// itself bounds it too: a peer that accepts and then stalls must not
	// hold the attempt.
	stop := context.AfterFunc(ctx, func() { _ = tcp.Close() })
	defer stop()
	_ = tcp.SetDeadline(time.Now().Add(l.cfg.HandshakeTimeout))

	// The callback runs again at every re-key of an established connection
	// (x/crypto re-keys after about 1 GiB or an hour). Then the only
	// question is whether the key is still the one accepted at connect:
	// Learn must not run twice, and the connection's deadline is no longer
	// the handshake's to set.
	var (
		hostKeyErr error
		accepted   ssh.PublicKey
	)
	config := &ssh.ClientConfig{
		Config:            ssh.Config{RekeyThreshold: l.cfg.rekeyThreshold},
		User:              l.cfg.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(l.cfg.Keys...)},
		HostKeyAlgorithms: []string{guest.HostKeyType},
		ClientVersion:     clientVersion,
		HostKeyCallback: func(_ string, _ net.Addr, presented ssh.PublicKey) error {
			if accepted != nil {
				if !sameKey(accepted, presented) {
					return fmt.Errorf("the guest changed its host key at a re-key: %w", transport.ErrHostKeyMismatch)
				}
				return nil
			}
			hostKeyErr = l.checkHostKey(ctx, pin, presented)
			if hostKeyErr != nil {
				return hostKeyErr
			}
			accepted = presented
			// Learn may have taken a while; give the rest of the
			// handshake its own time.
			_ = tcp.SetDeadline(time.Now().Add(l.cfg.HandshakeTimeout))
			return nil
		},
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(tcp, addr, config)
	if err != nil {
		_ = tcp.Close()
		return nil, l.handshakeError(addr, pin, err, hostKeyErr)
	}
	_ = tcp.SetDeadline(time.Time{})

	// Every channel the guest opens is refused here, before x/crypto's
	// client sees it. The client is handed a channel that only ever
	// closes, so nothing registered on it later (a Listen, an agent
	// forward) could accept one.
	//
	// The guest decides how many it opens, so only the first few are
	// logged one by one: a guest must not be able to fill the server's
	// log.
	none := make(chan ssh.NewChannel)
	go func() {
		defer close(none)
		refused := 0
		for ch := range chans {
			refused++
			switch {
			case refused <= maxRefusalLogs:
				l.cfg.Logf("sshlink: %s: refused a %q channel opened by the guest", l.name(), ch.ChannelType())
			case refused == maxRefusalLogs+1:
				l.cfg.Logf("sshlink: %s: the guest keeps opening channels; each is turned away, and no more of them are logged for this connection", l.name())
			}
			_ = ch.Reject(ssh.Prohibited, "the server accepts no channels")
		}
		if refused > maxRefusalLogs {
			l.cfg.Logf("sshlink: %s: the guest opened %d channels on this connection; all were turned away", l.name(), refused)
		}
	}()

	return &conn{
		client: ssh.NewClient(sshConn, none, reqs),
		tcp:    tcp,
		addr:   addr,
		dead:   make(chan struct{}),
	}, nil
}

// checkHostKey decides whether to go on with a guest that presented a key.
func (l *Link) checkHostKey(ctx context.Context, pin, presented ssh.PublicKey) error {
	if presented.Type() != guest.HostKeyType {
		return fmt.Errorf("the guest presented a %s host key; only %s is accepted: %w",
			presented.Type(), guest.HostKeyType, mismatchOrUnverified(pin))
	}
	if pin != nil {
		if !sameKey(pin, presented) {
			return fmt.Errorf("the guest presented %s, the pin is %s: %w",
				ssh.FingerprintSHA256(presented), ssh.FingerprintSHA256(pin), transport.ErrHostKeyMismatch)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, learnTimeout)
	defer cancel()
	if err := l.target.HostKeys.Learn(ctx, FormatHostKey(presented)); err != nil {
		return fmt.Errorf("no pin, and the presented key %s was not accepted: %w: %w",
			ssh.FingerprintSHA256(presented), err, transport.ErrNoHostKey)
	}
	return nil
}

// mismatchOrUnverified picks the sentinel for a key that cannot be the right
// one: a mismatch when there is a pin to differ from, unverified otherwise.
func mismatchOrUnverified(pin ssh.PublicKey) error {
	if pin != nil {
		return transport.ErrHostKeyMismatch
	}
	return transport.ErrNoHostKey
}

// handshakeError classifies a failed handshake.
func (l *Link) handshakeError(addr string, pin ssh.PublicKey, err, hostKeyErr error) error {
	if l.ctx.Err() != nil {
		return transport.ErrClosed
	}
	prefix := fmt.Sprintf("sshlink: %s: handshake with %s", l.name(), addr)

	// The host key decision is ours, so it is taken from the callback, not
	// parsed back out of x/crypto's error.
	if hostKeyErr != nil {
		return fmt.Errorf("%s: %w", prefix, hostKeyErr)
	}
	var negotiation *ssh.AlgorithmNegotiationError
	if errors.As(err, &negotiation) {
		// The guest offers no ed25519 host key (or shares no cipher
		// with us): it is not a guest this server built.
		return fmt.Errorf("%s: %v: %w", prefix, err, mismatchOrUnverified(pin))
	}
	if strings.Contains(err.Error(), "unable to authenticate") {
		return fmt.Errorf("%s as %s: %v: %w", prefix, l.cfg.User, err, transport.ErrAuth)
	}
	// A timeout, a reset or an end-of-file part-way: sshd is not ready, or
	// the address is not the guest's.
	return fmt.Errorf("%s: %w: %w", prefix, err, transport.ErrUnreachable)
}
