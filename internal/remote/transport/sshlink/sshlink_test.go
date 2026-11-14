package sshlink

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/remote/transport/conformance"
	"github.com/charliek/shed/internal/remote/transport/sshlink/sshtest"
)

// TestConformanceInProcess is leg 2 of the transport suite: the SSH link
// through an in-process SSH server that forwards to a real agent.
func TestConformanceInProcess(t *testing.T) {
	agent := startLocalAgent(t)
	f := newFixture(t, agent.socketDir(), nil, sshtest.Options{})
	if f.factory.Name() != transport.NameSSH {
		t.Errorf("Name = %q", f.factory.Name())
	}

	conformance.Run(t, conformance.Harness{
		Open:     func(t *testing.T) transport.Link { return f.open(t) },
		HTTPPort: func(transport.Link) uint16 { return agent.httpPort() },
		// The server drops every connection and comes back on another
		// port, like a guest that was stopped and started.
		Restart: func(*testing.T, transport.Link) { f.srv.Restart() },
	})
	if n := f.srv.Refusals(); n != 0 {
		t.Errorf("the server refused %d logins during the suite", n)
	}
}

func TestOpenValidatesTheTarget(t *testing.T) {
	factory, err := New(Config{Keys: []ssh.Signer{sshtest.NewKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	addr := func(context.Context) (string, error) { return "127.0.0.1:1", nil }
	tests := []struct {
		name   string
		target transport.Target
	}{
		{"no Addr", transport.Target{HostKeys: PinnedHostKey("x")}},
		{"no HostKeys", transport.Target{Addr: addr}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := factory.Open(context.Background(), tt.target); err == nil {
				t.Error("Open succeeded")
			}
		})
	}
	if _, err := New(Config{}); err == nil {
		t.Error("New with no key succeeded")
	}
}

// Open must not touch the network: a link to nowhere opens fine and fails
// only at Dial.
func TestOpenDoesNotConnect(t *testing.T) {
	f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
	var asked atomic.Int32
	link := f.openWith(t, PinnedHostKey(f.srv.HostKey()), func(ctx context.Context) (string, error) {
		asked.Add(1)
		return f.addr(ctx)
	})
	time.Sleep(50 * time.Millisecond)
	if asked.Load() != 0 || f.srv.Accepted() != 0 {
		t.Fatalf("Open asked for the address %d times and made %d connections", asked.Load(), f.srv.Accepted())
	}
	if _, err := link.connection(testCtx(t)); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if asked.Load() != 1 {
		t.Errorf("the address was asked for %d times for one connection", asked.Load())
	}
}

func TestHostKeyChecks(t *testing.T) {
	other := sshtest.PublicKeyLine(sshtest.NewKey(t).PublicKey())

	t.Run("the pinned key is accepted", func(t *testing.T) {
		f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
		if _, err := f.open(t).connection(testCtx(t)); err != nil {
			t.Fatalf("connect: %v", err)
		}
	})

	t.Run("another key is a mismatch and the client key is never offered", func(t *testing.T) {
		f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
		link := f.openWith(t, PinnedHostKey(other), f.addr)
		_, err := link.connection(testCtx(t))
		if !errors.Is(err, transport.ErrHostKeyMismatch) {
			t.Fatalf("connect = %v, want ErrHostKeyMismatch", err)
		}
		if f.srv.Logins() != 0 || f.srv.Refusals() != 0 {
			t.Errorf("the server saw %d logins and %d refusals; authentication must not start", f.srv.Logins(), f.srv.Refusals())
		}
	})

	t.Run("a pin that does not parse is a mismatch, never no pin", func(t *testing.T) {
		f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
		store := &learnStore{pin: "ssh-ed25519 not-base64", published: f.srv.HostKey()}
		link := f.openWith(t, store, f.addr)
		if _, err := link.connection(testCtx(t)); !errors.Is(err, transport.ErrHostKeyMismatch) {
			t.Fatalf("connect = %v, want ErrHostKeyMismatch", err)
		}
		if _, learns := store.state(); learns != 0 {
			t.Errorf("Learn was called %d times for a shed that has a pin", learns)
		}
		if f.srv.Accepted() != 0 {
			t.Errorf("the link connected %d times with an unreadable pin", f.srv.Accepted())
		}
	})

	t.Run("no pin and the published key matches: learned", func(t *testing.T) {
		f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
		store := &learnStore{published: f.srv.HostKey()}
		link := f.openWith(t, store, f.addr)
		if _, err := link.connection(testCtx(t)); err != nil {
			t.Fatalf("connect: %v", err)
		}
		if pin, learns := store.state(); pin != f.srv.HostKey() || learns != 1 {
			t.Errorf("pin %q after %d learns, want the server's key after 1", pin, learns)
		}
	})

	t.Run("no pin and the published key differs: unverified", func(t *testing.T) {
		f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
		store := &learnStore{published: other}
		link := f.openWith(t, store, f.addr)
		_, err := link.connection(testCtx(t))
		if !errors.Is(err, transport.ErrNoHostKey) {
			t.Fatalf("connect = %v, want ErrNoHostKey", err)
		}
		if pin, _ := store.state(); pin != "" {
			t.Errorf("a pin %q was stored for a key that was refused", pin)
		}
		if f.srv.Logins() != 0 {
			t.Error("the link logged in to a guest whose key it could not verify")
		}
	})

	t.Run("the pin store fails: no connection", func(t *testing.T) {
		f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
		boom := errors.New("provider down")
		link := f.openWith(t, &learnStore{pinErr: boom}, f.addr)
		if _, err := link.connection(testCtx(t)); !errors.Is(err, boom) {
			t.Fatalf("connect = %v, want the store's error", err)
		}
		if f.srv.Accepted() != 0 {
			t.Error("the link connected without knowing the pin")
		}
	})

	t.Run("a guest with only an RSA host key is refused", func(t *testing.T) {
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		rsaSigner, err := ssh.NewSignerFromKey(rsaKey)
		if err != nil {
			t.Fatal(err)
		}
		f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{HostKey: rsaSigner})
		link := f.openWith(t, PinnedHostKey(other), f.addr)
		if _, err := link.connection(testCtx(t)); !errors.Is(err, transport.ErrHostKeyMismatch) {
			t.Fatalf("connect with a pin = %v, want ErrHostKeyMismatch", err)
		}
		link = f.openWith(t, &learnStore{}, f.addr)
		if _, err := link.connection(testCtx(t)); !errors.Is(err, transport.ErrNoHostKey) {
			t.Fatalf("connect without a pin = %v, want ErrNoHostKey", err)
		}
	})
}

// A host key mismatch usually means the address is stale and belongs to
// another machine now. The link asks for the address once more and tries the
// new one; it never learns the other machine's key.
func TestMismatchResolvesTheAddressOnceMore(t *testing.T) {
	stranger := sshtest.NewServer(t, sshtest.Options{})

	t.Run("a new address is tried", func(t *testing.T) {
		f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
		store := &learnStore{pin: f.srv.HostKey()}
		var asked atomic.Int32
		link := f.openWith(t, store, func(context.Context) (string, error) {
			if asked.Add(1) == 1 {
				return stranger.Addr(), nil // stale
			}
			return f.srv.Addr(), nil
		})
		if _, err := link.connection(testCtx(t)); err != nil {
			t.Fatalf("connect: %v", err)
		}
		if got := link.connects.Load(); got != 2 {
			t.Errorf("%d connections attempted, want 2", got)
		}
		if _, learns := store.state(); learns != 0 {
			t.Errorf("Learn was called %d times", learns)
		}
	})

	t.Run("the same address is not tried twice", func(t *testing.T) {
		f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
		var asked atomic.Int32
		link := f.openWith(t, PinnedHostKey(f.srv.HostKey()), func(context.Context) (string, error) {
			asked.Add(1)
			return stranger.Addr(), nil
		})
		if _, err := link.connection(testCtx(t)); !errors.Is(err, transport.ErrHostKeyMismatch) {
			t.Fatalf("connect = %v, want ErrHostKeyMismatch", err)
		}
		if asked.Load() != 2 || link.connects.Load() != 1 {
			t.Errorf("address asked %d times, %d connections; want 2 and 1", asked.Load(), link.connects.Load())
		}
	})

	t.Run("a second wrong machine ends it", func(t *testing.T) {
		second := sshtest.NewServer(t, sshtest.Options{})
		f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
		var asked atomic.Int32
		link := f.openWith(t, PinnedHostKey(f.srv.HostKey()), func(context.Context) (string, error) {
			if asked.Add(1) == 1 {
				return stranger.Addr(), nil
			}
			return second.Addr(), nil
		})
		if _, err := link.connection(testCtx(t)); !errors.Is(err, transport.ErrHostKeyMismatch) {
			t.Fatalf("connect = %v, want ErrHostKeyMismatch", err)
		}
		if asked.Load() != 2 || link.connects.Load() != 2 {
			t.Errorf("address asked %d times, %d connections; want 2 and 2", asked.Load(), link.connects.Load())
		}
	})
}

func TestAuthentication(t *testing.T) {
	t.Run("an unknown client key is refused", func(t *testing.T) {
		f := newFixture(t, emptySocketDir(t), func(c *Config) {
			c.Keys = []ssh.Signer{sshtest.NewKey(t)}
		}, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{sshtest.NewKey(t).PublicKey()}})
		_, err := f.open(t).connection(testCtx(t))
		if !errors.Is(err, transport.ErrAuth) {
			t.Fatalf("connect = %v, want ErrAuth", err)
		}
	})

	t.Run("another user is refused", func(t *testing.T) {
		f := newFixture(t, emptySocketDir(t), func(c *Config) { c.User = "root" }, sshtest.Options{})
		_, err := f.open(t).connection(testCtx(t))
		if !errors.Is(err, transport.ErrAuth) {
			t.Fatalf("connect = %v, want ErrAuth", err)
		}
		if !strings.Contains(err.Error(), "root") {
			t.Errorf("error %q does not name the user", err)
		}
	})

	t.Run("the second key works while a key is rotated", func(t *testing.T) {
		accepted := sshtest.NewKey(t)
		f := newFixture(t, emptySocketDir(t), func(c *Config) {
			c.Keys = []ssh.Signer{sshtest.NewKey(t), accepted}
		}, sshtest.Options{AuthorizedKeys: []ssh.PublicKey{accepted.PublicKey()}})
		if _, err := f.open(t).connection(testCtx(t)); err != nil {
			t.Fatalf("connect: %v", err)
		}
	})

	t.Run("the default user is the link user", func(t *testing.T) {
		f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{User: "shed-link"})
		c, err := f.open(t).connection(testCtx(t))
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		if got := c.client.User(); got != "shed-link" {
			t.Errorf("logged in as %q", got)
		}
	})
}

// Health checks call Dial every 50 ms while a guest boots. That must not
// become a connection every 50 ms.
func TestFailedConnectsAreSpaced(t *testing.T) {
	// A listener that accepts and hangs up: every handshake fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()

	f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
	link := f.openWith(t, PinnedHostKey(f.srv.HostKey()), func(context.Context) (string, error) {
		return ln.Addr().String(), nil
	})

	const window = 1300 * time.Millisecond
	start := time.Now()
	dials := 0
	var first error
	for time.Since(start) < window {
		_, err := link.Dial(context.Background(), transport.PortNotify)
		if !errors.Is(err, transport.ErrUnreachable) {
			t.Fatalf("Dial = %v, want ErrUnreachable", err)
		}
		if first == nil {
			first = err
		}
		dials++
		time.Sleep(10 * time.Millisecond)
	}
	// Attempts at about 0, 500 and 1000 ms.
	got := accepted.Load()
	if got < 2 || got > 3 {
		t.Errorf("%d connections in %v for %d Dials, want 3 (one per %v)", got, window, dials, transport.MinConnectInterval)
	}
	if dials < 50 {
		t.Errorf("only %d Dials returned in %v; a Dial inside the interval must return at once", dials, window)
	}
	if n := link.connects.Load(); int32(n) != got {
		t.Errorf("link counted %d attempts, the listener %d", n, got)
	}
}

// The interval is from the end of a failed attempt, and a success clears it.
func TestRecoveryAfterFailedConnects(t *testing.T) {
	f := newFixture(t, emptySocketDir(t), func(c *Config) {
		c.MinConnectInterval = 100 * time.Millisecond
	}, sshtest.Options{})
	var up atomic.Bool
	link := f.openWith(t, PinnedHostKey(f.srv.HostKey()), func(ctx context.Context) (string, error) {
		if !up.Load() {
			return "", errors.New("the instance has no address yet")
		}
		return f.addr(ctx)
	})

	_, err := link.connection(testCtx(t))
	if !errors.Is(err, transport.ErrUnreachable) || !strings.Contains(err.Error(), "no address yet") {
		t.Fatalf("connect = %v, want ErrUnreachable naming the cause", err)
	}
	up.Store(true)
	if _, again := link.connection(testCtx(t)); again == nil {
		t.Fatal("a Dial inside the interval connected")
	}
	eventually(t, 2*time.Second, "connect once the address exists", func() bool {
		_, err := link.connection(testCtx(t))
		return err == nil
	})
}

func TestConcurrentDialsShareOneConnection(t *testing.T) {
	f := newFixture(t, fakeSockets(t), nil, sshtest.Options{})
	link := f.open(t)

	const n = 32
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := link.Dial(testCtx(t), transport.PortTCPProxy)
			if err == nil {
				defer conn.Close()
				msg := []byte("ping")
				if _, err = conn.Write(msg); err == nil {
					_, err = io.ReadFull(conn, msg)
				}
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("dial %d: %v", i, err)
		}
	}
	if got := f.srv.Accepted(); got != 1 {
		t.Errorf("%d TCP connections for %d concurrent first Dials, want 1", got, n)
	}
	if got := f.srv.Forwards(); got != n {
		t.Errorf("%d channels, want %d", got, n)
	}
}

// When sshd is up but the agent's socket is not there yet, the Dial fails as
// unreachable and the SSH connection is kept: no reconnect per probe.
func TestAgentSocketMissing(t *testing.T) {
	f := newFixture(t, emptySocketDir(t), nil, sshtest.Options{})
	link := f.open(t)
	for range 5 {
		_, err := link.Dial(testCtx(t), transport.PortNotify)
		if !errors.Is(err, transport.ErrUnreachable) {
			t.Fatalf("Dial = %v, want ErrUnreachable", err)
		}
		if !strings.Contains(err.Error(), "connect failed") {
			t.Errorf("error %q does not carry sshd's reason", err)
		}
	}
	if got := f.srv.Accepted(); got != 1 {
		t.Errorf("%d TCP connections for 5 refused channels, want 1", got)
	}
}

func TestForwardingProhibited(t *testing.T) {
	f := newFixture(t, fakeSockets(t), nil, sshtest.Options{DenyForwarding: true})
	_, err := f.open(t).Dial(testCtx(t), transport.PortTCPProxy)
	if !errors.Is(err, transport.ErrUnreachable) || !strings.Contains(err.Error(), "prohibited") {
		t.Fatalf("Dial = %v, want ErrUnreachable saying prohibited", err)
	}
}

// A guest must not be able to open anything towards the server.
func TestGuestOpenedChannelsAreRefused(t *testing.T) {
	f := newFixture(t, fakeSockets(t), nil, sshtest.Options{})
	link := f.open(t)
	if _, err := link.connection(testCtx(t)); err != nil {
		t.Fatal(err)
	}

	types := []string{
		"forwarded-tcpip",
		"forwarded-streamlocal@openssh.com",
		"auth-agent@openssh.com",
		"x11",
		"session",
		"direct-tcpip",
		"direct-streamlocal@openssh.com",
		"made-up@example.com",
	}
	for _, chanType := range types {
		t.Run(chanType, func(t *testing.T) {
			errs := f.srv.OpenChannel(chanType, nil)
			if len(errs) != 1 {
				t.Fatalf("%d clients connected, want 1", len(errs))
			}
			var refused *ssh.OpenChannelError
			if !errors.As(errs[0], &refused) {
				t.Fatalf("the client's answer was %v, want a refusal", errs[0])
			}
			if refused.Reason != ssh.Prohibited {
				t.Errorf("refused with %v, want %v", refused.Reason, ssh.Prohibited)
			}
		})
	}

	// The connection survives being asked.
	conn, err := link.Dial(testCtx(t), transport.PortTCPProxy)
	if err != nil {
		t.Fatalf("Dial after the refusals: %v", err)
	}
	conn.Close()
	if got := f.srv.Accepted(); got != 1 {
		t.Errorf("the link reconnected (%d connections)", got)
	}
	refusals := 0
	for _, line := range f.logs.all() {
		if strings.Contains(line, "refused a") {
			refusals++
		}
	}
	if refusals != len(types) {
		t.Errorf("%d refusals logged, want %d", refusals, len(types))
	}

	// A guest that keeps asking is still refused every time, and cannot
	// fill the server's log: past the first few, nothing more is logged
	// per channel.
	before := len(f.logs.all())
	for i := 0; i < 200; i++ {
		errs := f.srv.OpenChannel("session", nil)
		var refused *ssh.OpenChannelError
		if len(errs) != 1 || !errors.As(errs[0], &refused) {
			t.Fatalf("channel %d was not refused: %v", i, errs)
		}
	}
	if grew := len(f.logs.all()) - before; grew > maxRefusalLogs+1 {
		t.Errorf("200 more channels wrote %d log lines, want at most %d", grew, maxRefusalLogs+1)
	}
	if logs := strings.Join(f.logs.all(), "\n"); !strings.Contains(logs, "no more of them are logged") {
		t.Errorf("the log does not say that it stopped listing refusals: %s", logs)
	}
}

// A connection into a black hole: the peer stops answering but nothing closes
// the TCP connection. Only the keepalive notices.
func TestKeepaliveEndsABlackHoledConnection(t *testing.T) {
	const interval = 100 * time.Millisecond
	f := newFixture(t, fakeSockets(t), func(c *Config) {
		c.KeepaliveInterval = interval
		c.KeepaliveMisses = 2
		c.MinConnectInterval = 50 * time.Millisecond
		c.HandshakeTimeout = 500 * time.Millisecond
	}, sshtest.Options{})
	link := f.open(t)

	conn, err := link.Dial(testCtx(t), transport.PortNotify)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// A healthy connection is left alone across many intervals.
	readDone := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		t.Fatalf("the stream ended on a healthy connection: %v", err)
	case <-time.After(6 * interval):
	}

	f.srv.Freeze()
	frozen := time.Now()
	select {
	case err := <-readDone:
		if !errors.Is(err, ErrConnectionLost) {
			t.Errorf("Read = %v, want ErrConnectionLost", err)
		}
		// Two missed intervals after a request that went out up to one
		// interval after the freeze: 2 to 3 intervals, with slack for a
		// request already in flight and for a slow machine.
		if d := time.Since(frozen); d < interval || d > 8*interval {
			t.Errorf("the connection was closed %v after the freeze, want about %v to %v", d, 2*interval, 3*interval)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a stream on a black-holed connection never ended")
	}

	// Still frozen: connecting fails as unreachable, by the handshake
	// timeout.
	if _, err := link.Dial(testCtx(t), transport.PortNotify); !errors.Is(err, transport.ErrUnreachable) {
		t.Errorf("Dial into the black hole = %v, want ErrUnreachable", err)
	}

	f.srv.Thaw()
	eventually(t, 3*time.Second, "reconnect after the thaw", func() bool {
		c, err := link.Dial(testCtx(t), transport.PortNotify)
		if err != nil {
			return false
		}
		c.Close()
		return true
	})
}

// A dropped connection must not look like a clean end-of-file to a reader.
func TestConnectionLossIsNotEOF(t *testing.T) {
	f := newFixture(t, fakeSockets(t), nil, sshtest.Options{})
	link := f.open(t)
	conn, err := link.Dial(testCtx(t), transport.PortNotify)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(conn)
		readDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	f.srv.DropConnections()

	select {
	case err := <-readDone:
		if !errors.Is(err, ErrConnectionLost) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("ReadAll = %v, want ErrConnectionLost", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not end when its connection was dropped")
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Error("Write on a lost connection succeeded")
	}

	// And the next Dial reconnects.
	again, err := link.Dial(testCtx(t), transport.PortNotify)
	if err != nil {
		t.Fatalf("Dial after the drop: %v", err)
	}
	again.Close()
	if got := f.srv.Accepted(); got != 2 {
		t.Errorf("%d connections, want 2", got)
	}
}

// A clean end-of-file from the guest stays a clean end-of-file.
func TestGuestCloseIsEOF(t *testing.T) {
	f := newFixture(t, fakeSockets(t), nil, sshtest.Options{})
	conn, err := f.open(t).Dial(testCtx(t), transport.PortTCPProxy)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("echo me")); err != nil {
		t.Fatal(err)
	}
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "echo me" {
		t.Fatalf("ReadAll = %q, %v; want the echo and a clean end-of-file", got, err)
	}
}

// x/crypto runs the host key callback again at every re-key. The pin must be
// learned once, and a long-lived connection must survive its re-keys.
func TestRekeyKeepsTheLearnedKey(t *testing.T) {
	f := newFixture(t, fakeSockets(t), func(c *Config) {
		c.rekeyThreshold = 64 * 1024
		c.HandshakeTimeout = time.Second
	}, sshtest.Options{})
	store := &learnStore{published: f.srv.HostKey()}
	link := f.openWith(t, store, f.addr)

	conn, err := link.Dial(testCtx(t), transport.PortTCPProxy)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 2 MiB each way through the echo socket: dozens of re-keys.
	payload := bytes.Repeat([]byte("0123456789abcdef"), 2<<20/16)
	go func() {
		_, _ = conn.Write(payload)
		_ = conn.(interface{ CloseWrite() error }).CloseWrite()
	}()
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read after %d bytes: %v", len(got), err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("%d bytes echoed, want %d identical bytes", len(got), len(payload))
	}
	if pin, learns := store.state(); learns != 1 || pin != f.srv.HostKey() {
		t.Errorf("Learn was called %d times (pin %q), want once", learns, pin)
	}

	// The handshake deadline must not have been re-armed by a re-key: the
	// connection is still usable after HandshakeTimeout has passed.
	time.Sleep(1200 * time.Millisecond)
	again, err := link.Dial(testCtx(t), transport.PortTCPProxy)
	if err != nil {
		t.Fatalf("Dial after the re-keys: %v", err)
	}
	again.Close()
	if got := f.srv.Accepted(); got != 1 {
		t.Errorf("the link reconnected (%d connections); a re-key broke the connection", got)
	}
}

func TestDialHonoursItsContext(t *testing.T) {
	f := newFixture(t, fakeSockets(t), func(c *Config) {
		c.HandshakeTimeout = 5 * time.Second
	}, sshtest.Options{})
	f.srv.Freeze() // accepts TCP, never speaks
	link := f.open(t)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := link.Dial(ctx, transport.PortNotify)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial = %v, want the context's deadline", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Dial returned after %v with a 150 ms context", d)
	}

	// The attempt is still running. Closing the link ends it at once, and
	// a Dial waiting on it gets ErrClosed.
	waiting := make(chan error, 1)
	go func() {
		_, err := link.Dial(context.Background(), transport.PortNotify)
		waiting <- err
	}()
	time.Sleep(50 * time.Millisecond)
	closed := time.Now()
	link.Close()
	select {
	case err := <-waiting:
		if !errors.Is(err, transport.ErrClosed) {
			t.Errorf("Dial during Close = %v, want ErrClosed", err)
		}
		if d := time.Since(closed); d > time.Second {
			t.Errorf("Close took %v to end a connection attempt", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not end the connection attempt")
	}
	if _, err := link.Dial(context.Background(), transport.PortNotify); !errors.Is(err, transport.ErrClosed) {
		t.Errorf("Dial after Close = %v, want ErrClosed", err)
	}
}

func TestAddrForms(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{"host and port", "203.0.113.7:2222", "203.0.113.7:2222"},
		{"host only gets the guest's SSH port", "203.0.113.7", "203.0.113.7:22"},
		{"name only", "guest.example", "guest.example:22"},
		{"IPv6 literal", "2001:db8::1", "[2001:db8::1]:22"},
		{"bracketed IPv6 literal", "[2001:db8::1]", "[2001:db8::1]:22"},
		{"IPv6 with a port", "[2001:db8::1]:2222", "[2001:db8::1]:2222"},
		{"surrounding space", " 203.0.113.7 \n", "203.0.113.7:22"},
	}
	factory, err := New(Config{Keys: []ssh.Signer{sshtest.NewKey(t)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link, err := factory.Open(context.Background(), transport.Target{
				Addr:     func(context.Context) (string, error) { return tt.addr, nil },
				HostKeys: PinnedHostKey(""),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer link.Close()
			got, err := link.(*Link).resolve(context.Background())
			if err != nil || got != tt.want {
				t.Errorf("resolve(%q) = %q, %v; want %q", tt.addr, got, err, tt.want)
			}
		})
	}

	t.Run("an empty address is unreachable", func(t *testing.T) {
		link, err := factory.Open(context.Background(), transport.Target{
			Addr:     func(context.Context) (string, error) { return "", nil },
			HostKeys: PinnedHostKey(""),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer link.Close()
		if _, err := link.(*Link).resolve(context.Background()); !errors.Is(err, transport.ErrUnreachable) {
			t.Errorf("resolve = %v, want ErrUnreachable", err)
		}
	})
}
