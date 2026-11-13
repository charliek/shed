package sshlink

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/remote/transport/sshlink/sshtest"
)

// goroutinesSettle waits for the goroutine count to stop falling and returns
// it. Goroutines of earlier tests and of closed connections take a moment to
// unwind.
func goroutinesSettle() int {
	n := runtime.NumGoroutine()
	for stable := 0; stable < 10; {
		time.Sleep(10 * time.Millisecond)
		if now := runtime.NumGoroutine(); now < n {
			n, stable = now, 0
		} else {
			stable++
		}
	}
	return n
}

// A closed link leaves nothing running: no connection goroutines (the watch,
// the keepalive, the refuser of guest-opened channels, x/crypto's loops) and
// no stream goroutines, whatever happened to the link on the way. A server
// holds one link per shed for as long as the shed lives and replaces its
// connection at every stop and start, so a goroutine left behind per
// connection or per stream adds up without bound.
func TestClosedLinkLeavesNoGoroutines(t *testing.T) {
	f := newFixture(t, fakeSockets(t), func(c *Config) {
		c.MinConnectInterval = 20 * time.Millisecond
		c.KeepaliveInterval = 30 * time.Millisecond
		c.ConnectTimeout = time.Second
	}, sshtest.Options{})
	ctx := testCtx(t)

	// One full use first, so anything started once per process exists
	// before the count is taken.
	warm := f.open(t)
	if c, err := warm.Dial(ctx, transport.PortTCPProxy); err != nil {
		t.Fatalf("warm-up Dial: %v", err)
	} else {
		c.Close()
	}
	warm.Close()
	before := goroutinesSettle()

	link := f.open(t)
	dial := func(port uint32) net.Conn {
		t.Helper()
		c, err := link.Dial(ctx, port)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		return c
	}
	blockedRead := func(c net.Conn) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := c.Read(make([]byte, 1))
			done <- err
		}()
		return done
	}
	wait := func(what string, done <-chan error) error {
		t.Helper()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: a blocked Read never returned", what)
			return nil
		}
	}

	// Streams that end in each of the ways the server ends them.
	for i := range 12 {
		c := dial(transport.PortTCPProxy) // echoes
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, err := io.ReadFull(c, make([]byte, 4)); err != nil {
			t.Fatalf("Read: %v", err)
		}
		switch i % 4 {
		case 0: // closed while idle
		case 1: // half-closed, read to end-of-file
			if err := c.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
				t.Fatalf("CloseWrite: %v", err)
			}
			if _, err := io.ReadAll(c); err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
		case 2: // closed under a blocked Read
			done := blockedRead(c)
			time.Sleep(5 * time.Millisecond)
			c.Close()
			if err := wait("Close", done); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("Read after Close = %v, want net.ErrClosed", err)
			}
		case 3: // a Read that timed out
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
			if _, err := c.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("Read past its deadline = %v, want os.ErrDeadlineExceeded", err)
			}
		}
		c.Close()
	}

	// Streams open when the connection is dropped, closed afterwards; then
	// the link reconnects.
	var open []net.Conn
	var reads []<-chan error
	for range 4 {
		c := dial(transport.PortNotify) // silent
		open = append(open, c)
		reads = append(reads, blockedRead(c))
	}
	time.Sleep(60 * time.Millisecond) // a keepalive or two goes by
	f.srv.DropConnections()
	for i, done := range reads {
		if err := wait("drop", done); !errors.Is(err, ErrConnectionLost) {
			t.Fatalf("Read on a dropped connection = %v, want ErrConnectionLost", err)
		}
		open[i].Close()
	}
	eventually(t, 3*time.Second, "reconnect after the drop", func() bool {
		c, err := link.Dial(ctx, transport.PortTCPProxy)
		if err != nil {
			return false
		}
		c.Close()
		return true
	})

	// A stream blocked in Read when the link itself is closed, and failed
	// connection attempts before that.
	last := dial(transport.PortNotify)
	lastRead := blockedRead(last)
	other := f.open(t)
	f.srv.StopListening()
	for range 4 {
		if _, err := other.Dial(ctx, transport.PortNotify); !errors.Is(err, transport.ErrUnreachable) {
			t.Fatalf("Dial with the server gone = %v, want ErrUnreachable", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	other.Close()
	link.Close()
	if err := wait("link Close", lastRead); err == nil {
		t.Fatal("Read returned no error after the link was closed")
	}
	last.Close()

	// The server's listener goroutine is gone too, so the count may end
	// below where it started; it must not end above.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			var stacks bytes.Buffer
			_ = pprof.Lookup("goroutine").WriteTo(&stacks, 1)
			t.Fatalf("%d goroutines before, %d after the links were closed:\n%s", before, runtime.NumGoroutine(), stacks.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
