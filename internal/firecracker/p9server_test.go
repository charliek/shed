//go:build linux
// +build linux

package firecracker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hugelgupf/p9/p9"
)

func TestP9Server_StartStop(t *testing.T) {
	hostDir := t.TempDir()

	srv, err := NewP9Server("127.0.0.1", hostDir, "/workspace", false, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}

	// Verify the server has a valid address before starting
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("Addr() returned empty string")
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q) error = %v", addr, err)
	}
	if host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", host)
	}
	if port == "" || port == "0" {
		t.Errorf("port = %q, want a dynamically assigned port", port)
	}

	srv.Start()

	// Verify we can connect to the listening address
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to server at %s: %v", addr, err)
	}
	conn.Close()

	// Close the server
	if err := srv.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Verify the server is no longer accepting connections
	_, err = net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err == nil {
		t.Error("expected connection to fail after Close(), but it succeeded")
	}
}

func TestP9Server_ReadWrite(t *testing.T) {
	hostDir := t.TempDir()

	srv, err := NewP9Server("127.0.0.1", hostDir, "/workspace", false, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}
	srv.Start()
	t.Cleanup(func() { srv.Close() })

	// Connect a 9P client
	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	client, err := p9.NewClient(conn, p9.WithMessageSize(1048576))
	if err != nil {
		conn.Close()
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	// Attach to get the root directory
	root, err := client.Attach("")
	if err != nil {
		t.Fatalf("Attach() error = %v", err)
	}

	// Walk with empty names to clone the root fid before Create mutates it.
	// After root.Create(), root becomes the created file (9P semantics), so
	// we need a separate fid to walk back to the file for reading.
	_, root2, err := root.Walk(nil)
	if err != nil {
		root.Close()
		t.Fatalf("Walk(nil) to clone root fid: %v", err)
	}

	// Create a file (this consumes root, which now represents the new file)
	testContent := []byte("hello 9P world")
	_, _, _, err = root.Create("testfile.txt", p9.ReadWrite, 0644, p9.NoUID, p9.NoGID)
	if err != nil {
		root.Close()
		root2.Close()
		t.Fatalf("Create() error = %v", err)
	}

	// Write content via root (which is now the created file)
	n, err := root.WriteAt(testContent, 0)
	if err != nil {
		root.Close()
		root2.Close()
		t.Fatalf("WriteAt() error = %v", err)
	}
	if n != len(testContent) {
		root.Close()
		root2.Close()
		t.Errorf("WriteAt() wrote %d bytes, want %d", n, len(testContent))
	}
	root.Close()

	// Walk to the file from the cloned root fid and read it
	_, readFile, err := root2.Walk([]string{"testfile.txt"})
	if err != nil {
		root2.Close()
		t.Fatalf("Walk() error = %v", err)
	}
	defer readFile.Close()
	root2.Close()

	_, _, err = readFile.Open(p9.ReadOnly)
	if err != nil {
		t.Fatalf("Open(ReadOnly) error = %v", err)
	}

	buf := make([]byte, 1024)
	n, err = readFile.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt() error = %v", err)
	}

	got := string(buf[:n])
	want := string(testContent)
	if got != want {
		t.Errorf("ReadAt() = %q, want %q", got, want)
	}
}

func TestP9Server_ReadOnly(t *testing.T) {
	// The readOnly field on P9Server is stored but not enforced at the server
	// level (the localfs attacher doesn't respect it). The readOnly flag is
	// used for mount options in the guest (MS_RDONLY). This test verifies the
	// server still starts and operates even when readOnly=true, and that the
	// field is correctly stored.
	hostDir := t.TempDir()

	srv, err := NewP9Server("127.0.0.1", hostDir, "/workspace", true, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}
	srv.Start()
	t.Cleanup(func() { srv.Close() })

	if !srv.readOnly {
		t.Error("readOnly should be true")
	}

	// Connect and verify the server works
	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}

	client, err := p9.NewClient(conn, p9.WithMessageSize(1048576))
	if err != nil {
		conn.Close()
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	root, err := client.Attach("")
	if err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
	defer root.Close()

	// The server still allows writes because readOnly is not enforced at
	// the 9P server level -- it's enforced by the guest mount (MS_RDONLY).
	// Verify we can at least read the (empty) directory.
	_, _, _, err = root.GetAttr(p9.AttrMask{Mode: true})
	if err != nil {
		t.Fatalf("GetAttr() error = %v", err)
	}
}

func TestP9Server_MultipleConnections(t *testing.T) {
	hostDir := t.TempDir()

	srv, err := NewP9Server("127.0.0.1", hostDir, "/workspace", false, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}
	srv.Start()
	t.Cleanup(func() { srv.Close() })

	const numClients = 5
	var wg sync.WaitGroup
	errs := make(chan error, numClients)

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
			if err != nil {
				errs <- err
				return
			}

			client, err := p9.NewClient(conn, p9.WithMessageSize(1048576))
			if err != nil {
				conn.Close()
				errs <- err
				return
			}
			defer client.Close()

			root, err := client.Attach("")
			if err != nil {
				errs <- err
				return
			}
			defer root.Close()

			// Each client reads the root directory attributes
			_, _, _, err = root.GetAttr(p9.AttrMask{Mode: true, Size: true})
			if err != nil {
				errs <- err
				return
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent client error: %v", err)
	}
}

// --- Close contract (#284) ---------------------------------------------------
//
// Close must not wait for the guest to hang up: it force-closes every
// connection it tracks and waits for their handlers only up to drainTimeout.
// These pin each clause of that contract.

// closeBound is the wall-clock ceiling for a Close (or a set of them) that
// must not wait on its drain backstop. It sits far below what it guards
// against — the 5 s default backstop, or forever — and far above a healthy
// close (milliseconds), so -race and a starved CPU don't trip it.
const closeBound = 2 * time.Second

// newStartedP9Server returns a started passthrough server on 127.0.0.1 that
// is closed when the test ends.
func newStartedP9Server(t *testing.T, hostDir string) *P9Server {
	t.Helper()
	srv, err := NewP9Server("127.0.0.1", hostDir, "/home/shed/project", false, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}
	srv.Start()
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// trackedConns is how many connections srv currently tracks.
func trackedConns(srv *P9Server) int {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return len(srv.conns)
}

// waitForTrackedConns blocks until srv tracks at least n connections, so a
// test's Close is guaranteed to find them registered rather than still in the
// kernel's accept backlog.
func waitForTrackedConns(t *testing.T, srv *P9Server, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for trackedConns(srv) < n {
		if time.Now().After(deadline) {
			t.Fatalf("server tracks %d connection(s), want at least %d", trackedConns(srv), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// dialMidTversion connects to srv and sends only the first two bytes of a
// Tversion's 4-byte size field, parking the server's handler in its read of
// the rest: a guest that has gone quiet but never hung up. It waits until the
// server has registered the connection.
func dialMidTversion(t *testing.T, srv *P9Server) net.Conn {
	t.Helper()
	before := trackedConns(srv)
	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", srv.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte{0x13, 0x00}); err != nil {
		t.Fatalf("write partial Tversion header: %v", err)
	}
	waitForTrackedConns(t, srv, before+1)
	return conn
}

// readUntilHangup starts a read on conn that blocks until the server closes
// it (or a 10 s safety deadline, comfortably past the 5 s default backstop),
// and returns a channel with the read's error.
func readUntilHangup(conn net.Conn) <-chan error {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err == nil {
			err = fmt.Errorf("server sent %d unexpected byte(s)", n)
		}
		got <- err
	}()
	return got
}

// assertHungUp asserts the read result means "the server closed the
// connection" — EOF or a reset — and not the safety deadline.
func assertHungUp(t *testing.T, readErr error) {
	t.Helper()
	var ne net.Error
	switch {
	case errors.As(readErr, &ne) && ne.Timeout():
		t.Fatalf("client read timed out: the server never closed the connection (%v)", readErr)
	case errors.Is(readErr, io.EOF), errors.Is(readErr, syscall.ECONNRESET):
	default:
		t.Fatalf("client read error = %v, want EOF or a connection reset", readErr)
	}
}

// goroutinesIn counts the goroutines whose stack contains frame.
func goroutinesIn(frame string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Count(buf[:n], []byte(frame))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// localfsOpenFrame is the host-side open a wedged 9P request is parked in.
const localfsOpenFrame = "localfs.(*Local).Open("

// wedgeHandler parks one of srv's connection handlers inside a host open(2)
// that cannot return on its own: a 9P client opens a FIFO in the shared
// directory for reading, and nothing opens it for writing. Closing the
// connection cannot interrupt that syscall, and the p9 library waits for the
// in-flight request before its Handle returns, so the handler outlives
// Close's force-close — exactly the case the drain backstop bounds.
//
// The wedge is deliberately never released. Releasing it after Close makes
// the p9 library Add to a connection's WaitGroup while that connection's
// teardown is still in Wait (hugelgupf/p9 v0.3.0 server.go:473 vs :591): a
// library data race that -race reports, and that can panic — sometimes in our
// handler goroutine (recoverable, see P9Server.handle), sometimes in the
// library's own request goroutines (not recoverable). The cost: each wedge
// parks one goroutine and its OS thread in open(2), plus its connection's
// goroutines and fids, for the rest of the test binary — six per run of this
// file's tests, accumulating linearly with -count (120 at -count=20).
func wedgeHandler(t *testing.T, srv *P9Server, hostDir string) {
	t.Helper()
	fifo := filepath.Join(hostDir, "wedge.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", srv.Addr(), err)
	}
	client, err := p9.NewClient(conn, p9.WithMessageSize(1<<20))
	if err != nil {
		_ = conn.Close()
		t.Fatalf("p9.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	root, err := client.Attach("")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_, f, err := root.Walk([]string{"wedge.fifo"})
	if err != nil {
		t.Fatalf("Walk to the FIFO: %v", err)
	}

	// Earlier wedges (other tests, other -count runs) are still parked in
	// the same frame, so wait for one MORE goroutine there, not just one.
	before := goroutinesIn(localfsOpenFrame)
	go func() {
		_, _, _ = f.Open(p9.ReadOnly) // blocks: the server's open(2) has no writer
	}()
	deadline := time.Now().Add(5 * time.Second)
	for goroutinesIn(localfsOpenFrame) <= before {
		if time.Now().After(deadline) {
			t.Fatalf("the 9P Tlopen never reached the host open of %s", fifo)
		}
		time.Sleep(5 * time.Millisecond)
	}

}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog redirects the standard logger for the rest of the test.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// A guest that went quiet mid-message used to hold Close for the full 5 s
// drain, and the connection was abandoned rather than closed.
func TestP9Server_CloseForceClosesBlockedClient(t *testing.T) {
	srv := newStartedP9Server(t, t.TempDir())
	conn := dialMidTversion(t, srv)
	read := readUntilHangup(conn)

	start := time.Now()
	if err := srv.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed >= closeBound {
		t.Errorf("Close() took %v with a client blocked mid-Tversion, want < %v (the 5 s backstop means it waited for the guest)", elapsed, closeBound)
	}
	assertHungUp(t, <-read)
	if n := trackedConns(srv); n != 0 {
		t.Errorf("server still tracks %d connection(s) after Close", n)
	}
}

// Registration in the accept loop races Close: every connection must either be
// refused or tracked-and-force-closed, never escape, and every concurrent
// Close caller must get the one shared result.
func TestP9Server_ConcurrentAcceptAndClose(t *testing.T) {
	srv := newStartedP9Server(t, t.TempDir())

	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	stop := make(chan struct{})
	var dialers sync.WaitGroup
	for i := 0; i < 8; i++ {
		dialers.Add(1)
		go func() {
			defer dialers.Done()
			for j := 0; j < 40; j++ {
				select {
				case <-stop:
					return
				default:
				}
				c, err := net.DialTimeout("tcp", srv.Addr(), time.Second)
				if err != nil {
					return // the listener is closed
				}
				_, _ = c.Write([]byte{0x13})
				mu.Lock()
				conns = append(conns, c)
				mu.Unlock()
			}
		}()
	}
	waitForTrackedConns(t, srv, 4)

	const closers = 4
	results := make(chan error, closers)
	start := time.Now()
	for i := 0; i < closers; i++ {
		go func() { results <- srv.Close() }()
	}
	first := <-results
	for i := 1; i < closers; i++ {
		if err := <-results; err != first {
			t.Errorf("concurrent Close() returned %v and %v; every caller must get the same result", first, err)
		}
	}
	if elapsed := time.Since(start); elapsed >= closeBound {
		t.Errorf("Close() took %v racing new connections, want < %v", elapsed, closeBound)
	}
	close(stop)
	dialers.Wait()

	if n := trackedConns(srv); n != 0 {
		t.Errorf("server still tracks %d connection(s) after Close", n)
	}
	// Every connection that reached the server — tracked, refused after
	// close, or still in the backlog when the listener closed — is hung up.
	mu.Lock()
	defer mu.Unlock()
	for _, c := range conns {
		assertHungUp(t, <-readUntilHangup(c))
		_ = c.Close()
	}
}

func TestP9Server_RepeatedCloseReturnsSameError(t *testing.T) {
	srv, err := NewP9Server("127.0.0.1", t.TempDir(), "/home/shed/project", false, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}
	srv.Start()
	// Pull the listener out from under the server so Close's own
	// listener.Close fails: a non-nil result to compare.
	_ = srv.listener.Close()

	first := srv.Close()
	if first == nil {
		t.Fatal("Close() = nil after the listener was already closed; want the listener's close error")
	}
	for i := 0; i < 3; i++ {
		if err := srv.Close(); err != first {
			t.Errorf("Close() call %d = %v, want the first call's %v", i+2, err, first)
		}
	}
}

// Close before Start must not wait on an accept loop that never ran.
func TestP9Server_CloseBeforeStart(t *testing.T) {
	srv, err := NewP9Server("127.0.0.1", t.TempDir(), "/home/shed/project", false, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}
	addr := srv.Addr()

	closed := make(chan error, 1)
	go func() { closed <- srv.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close() before Start = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close() before Start blocked waiting for an accept loop that never started")
	}

	srv.Start() // a no-op on a closed server
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		_ = c.Close()
		t.Error("listener still accepts connections after Close")
	}
}

// A handler wedged in a host syscall cannot be force-closed; Close returns at
// the backstop and says so, naming the shared host path.
func TestP9Server_CloseWedgedHandlerHitsBackstop(t *testing.T) {
	logs := captureLog(t)
	hostDir := t.TempDir()
	srv := newStartedP9Server(t, hostDir)
	srv.drainTimeout = 200 * time.Millisecond
	wedgeHandler(t, srv, hostDir)

	start := time.Now()
	if err := srv.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < srv.drainTimeout {
		t.Errorf("Close() returned after %v, before the %v backstop, with a handler still wedged", elapsed, srv.drainTimeout)
	}
	if elapsed >= closeBound {
		t.Errorf("Close() took %v, want ≈ the %v backstop (< %v; the default backstop is 5 s)", elapsed, srv.drainTimeout, closeBound)
	}
	got := logs.String()
	if !strings.Contains(got, "still running") || !strings.Contains(got, hostDir) {
		t.Errorf("Close() did not log the wedged handler for %s; log:\n%s", hostDir, got)
	}
}

// stopP9Servers closes a shed's servers concurrently: five mounts close in
// about the time of one, both when each has a client blocked mid-message
// (force-close) and when each has a handler wedged at a 500 ms backstop. The
// 1.5 s bound sits between one backstop (parallel, ~0.5 s) and five (serial,
// >= 2.5 s), and far below the 5 s default backstop a blocked client would
// cost without force-close.
func TestStopP9ServersClosesInParallel(t *testing.T) {
	const (
		mounts   = 5
		backstop = 500 * time.Millisecond
		bound    = 1500 * time.Millisecond
	)
	tests := []struct {
		name  string
		wedge bool
	}{
		{name: "client blocked mid-Tversion on each"},
		{name: "handler wedged in a host syscall on each", wedge: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wedge {
				captureLog(t)
			}
			c := &Client{p9Servers: make(map[string][]*P9Server)}
			var reads []<-chan error
			for i := 0; i < mounts; i++ {
				hostDir := t.TempDir()
				srv := newStartedP9Server(t, hostDir)
				if tt.wedge {
					srv.drainTimeout = backstop
					wedgeHandler(t, srv, hostDir)
				} else {
					reads = append(reads, readUntilHangup(dialMidTversion(t, srv)))
				}
				c.p9Servers["alpha"] = append(c.p9Servers["alpha"], srv)
			}

			start := time.Now()
			c.stopP9Servers("alpha")
			if elapsed := time.Since(start); elapsed >= bound {
				t.Errorf("stopP9Servers closed %d servers in %v, want < %v", mounts, elapsed, bound)
			}
			if _, ok := c.p9Servers["alpha"]; ok {
				t.Error("stopP9Servers left the shed's entry in the map")
			}
			for _, read := range reads {
				assertHungUp(t, <-read)
			}
		})
	}
}

// A panic raised in the connection-handler goroutine itself — the Wait-side
// "WaitGroup is reused before previous Wait has returned" that hugelgupf/p9
// v0.3.0's connState.stop() can raise there — is contained to that
// connection: it is logged, the connection is untracked and closed, and the
// server carries on. This only exercises recovery in our own goroutine; the
// library's Add/Done-side panics in its request goroutines are out of reach
// of any recover here (see P9Server.handle).
func TestP9Server_HandlerPanicIsContained(t *testing.T) {
	logs := captureLog(t)
	srv, err := NewP9Server("127.0.0.1", t.TempDir(), "/home/shed/project", false, 0, 0)
	if err != nil {
		t.Fatalf("NewP9Server() error = %v", err)
	}
	srv.serveConn = func(net.Conn) { // set before Start: the accept loop reads it
		panic("sync: WaitGroup is reused before previous Wait has returned")
	}
	srv.Start()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	assertHungUp(t, <-readUntilHangup(conn))

	deadline := time.Now().Add(5 * time.Second)
	for trackedConns(srv) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the panicking handler never untracked its connection")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !strings.Contains(logs.String(), "recovered a panic") {
		t.Errorf("the contained panic was not logged; log:\n%s", logs.String())
	}

	// The handler's wg count was released too: Close drains at once instead
	// of waiting out the backstop.
	start := time.Now()
	if err := srv.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed >= closeBound {
		t.Errorf("Close() took %v after a contained panic; the handler's wg count leaked", elapsed)
	}
}
