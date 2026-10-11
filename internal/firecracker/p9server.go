//go:build linux
// +build linux

package firecracker

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/hugelgupf/p9/fsimpl/localfs"
	"github.com/hugelgupf/p9/p9"
)

// defaultP9DrainTimeout is Close's backstop for connection handlers that are
// still running after their connections were closed. Closing a connection ends
// the p9 library's read loop, but a request already blocked in a host syscall
// (an open(2) on a FIFO, a wedged network filesystem) only returns when that
// syscall does, and the library waits for it before tearing the connection
// down (hugelgupf/p9 connState.stop). A healthy handler exits within
// milliseconds of its connection closing, so this only bounds the wedged case.
const defaultP9DrainTimeout = 5 * time.Second

// P9Server wraps a 9P TCP server for sharing a host directory with a
// Firecracker VM. The guest connects to the server's TCP address (on the
// bridge) and mounts the shared directory using the 9P protocol.
//
// Lifetime contract (#284): the owning Client closes a shed's servers only
// once its VMM is verifiably gone — or at daemon exit, which ends every 9P
// socket anyway (#369). Close therefore never waits for the guest to hang up:
// it force-closes every connection it tracks, then waits for their handlers
// for at most drainTimeout.
type P9Server struct {
	listener  net.Listener
	server    *p9.Server
	hostPath  string // path on host being shared
	mountPath string // target path inside guest
	readOnly  bool

	// serveConn serves one connection until it ends: server.Handle in
	// production. A field only so a test can make it panic.
	serveConn func(conn net.Conn)

	// drainTimeout bounds Close's wait for connection handlers after their
	// connections are closed. defaultP9DrainTimeout in production; tests
	// shorten it.
	drainTimeout time.Duration

	// wg counts registered connection handlers. Add happens only in the
	// accept loop, under mu and only while !closed, so once Close has seen
	// the accept loop exit (<-done) no further Add can race its Wait.
	wg sync.WaitGroup
	// done is closed when Start's accept loop exits.
	done chan struct{}

	// mu guards conns, closed and started. It is never held across a wait or
	// a blocking close.
	mu      sync.Mutex
	conns   map[net.Conn]struct{} // live connections, removed by their handler on exit
	closed  bool                  // set once by Close; the accept loop refuses new conns after
	started bool                  // set by Start; Close waits on done only when true

	closeOnce sync.Once
	closeErr  error // shared result of the one real Close, returned to every caller
}

// NewP9Server creates a 9P server that shares hostPath over TCP. The listener
// binds to bridgeIP on a dynamically assigned port. The server is created but
// not yet serving; call Start to begin accepting connections.
//
// When targetUID and targetGID are both 0, the server uses localfs directly
// (passthrough mode). Otherwise, it wraps the attacher with UID/GID remapping
// so that files created by the guest are Lchown'd to the target user.
func NewP9Server(bridgeIP, hostPath, mountPath string, readOnly bool, targetUID, targetGID int) (*P9Server, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(bridgeIP, "0"))
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", bridgeIP, err)
	}

	var attacher p9.Attacher
	inner := localfs.Attacher(hostPath)
	if targetUID == 0 && targetGID == 0 {
		attacher = inner
	} else {
		if !readOnly {
			verifyRemappingCapability(hostPath, targetUID, targetGID)
		}
		attacher = newRemappingAttacher(inner, hostPath, targetUID, targetGID)
	}
	srv := p9.NewServer(attacher)

	return &P9Server{
		listener:     listener,
		server:       srv,
		hostPath:     hostPath,
		mountPath:    mountPath,
		readOnly:     readOnly,
		serveConn:    func(conn net.Conn) { _ = srv.Handle(conn, conn) },
		drainTimeout: defaultP9DrainTimeout,
		done:         make(chan struct{}),
		conns:        make(map[net.Conn]struct{}),
	}, nil
}

// Start begins accepting 9P connections in the background. Each accepted
// connection is registered (so Close can force it shut) and handled in its
// own goroutine. Start is a no-op on a server that is already started or
// already closed.
func (s *P9Server) Start() {
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	go s.acceptLoop()
}

// acceptLoop accepts connections until the listener is closed. Registration
// and wg.Add happen together under mu and only while the server is open, so a
// connection accepted after Close has begun is refused here instead of
// escaping Close's snapshot.
func (s *P9Server) acceptLoop() {
	defer close(s.done)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			// Listener was closed; stop accepting.
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			continue
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()

		go s.handle(conn)
	}
}

// handle serves one connection until it ends, then untracks it. Deferred in
// reverse: recover, close the conn, untrack it under mu, and only then signal
// wg — so a Close that has drained wg sees an empty conns map.
func (s *P9Server) handle(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()
	defer conn.Close()
	defer func() {
		// hugelgupf/p9 v0.3.0 races its own per-connection WaitGroup on
		// teardown: once the connection's read fails, Handle's deferred
		// connState.stop() Waits on pendingWg (p9/server.go:591) while
		// request goroutines still in flight finish, loop, and Add to it
		// again (server.go:473-474).
		//
		// This recover contains exactly ONE of the resulting panics: the
		// Wait-side "WaitGroup is reused before previous Wait has returned",
		// which stop() raises in THIS goroutine. It cannot contain the
		// Add/Done-side panics ("Add called concurrently with Wait") the
		// library raises in its own request goroutines (`go
		// cs.handleRequests()`, server.go:508) when two or more of them on
		// one connection finish and re-enter while stop() waits: those
		// crash the process. That race is pre-existing — a guest EOF or
		// reset reaches the same stop(), and before #284 the host sockets
		// also errored out via TCP keepalive after the VM died — it is
		// still present in p9 v0.4.1, and it is tracked as a follow-up.
		// Force-closing a connection with a request wedged in a host
		// syscall is one more way to reach it.
		//
		// Recovering here also skips the rest of cs.stop() for this
		// connection — its explicit fid cleanup — so those fids' host files
		// are left to the GC finalizers.
		if r := recover(); r != nil {
			log.Printf("Warning: 9P server for %s: recovered a panic tearing down a connection: %v", s.hostPath, r)
		}
	}()
	// Blocks until the connection is done (the guest hangs up, or Close
	// closes the conn under it).
	s.serveConn(conn)
}

// Addr returns the host:port address the server is listening on. The guest
// uses this address to connect via 9P over TCP.
func (s *P9Server) Addr() string {
	return s.listener.Addr().String()
}

// Close stops the server: it refuses new connections, closes the listener,
// force-closes every live connection, and waits for their handlers for at most
// drainTimeout. A handler still running at the backstop is wedged in a host
// syscall; Close logs it and returns, and that goroutine (with its fids)
// leaks until the syscall returns.
//
// Close is idempotent and safe to call concurrently: the work runs once and
// every caller gets the same result. Close before Start only closes the
// listener.
func (s *P9Server) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.close() })
	return s.closeErr
}

func (s *P9Server) close() error {
	// 1. Refuse further registrations and take the first snapshot.
	s.mu.Lock()
	s.closed = true
	started := s.started
	conns := s.snapshotConnsLocked()
	s.mu.Unlock()

	// 2. Stop accepting.
	err := s.listener.Close()
	if !started {
		// Nothing was ever accepted and no accept loop will close done.
		return err
	}

	// 3. Wait for the accept loop to exit. Every wg.Add happens inside it,
	// so after this the wg.Wait below cannot race an Add.
	<-s.done

	// 4. Force every connection shut, outside mu. The second snapshot is
	// belt-and-braces for anything registered between the first one and the
	// accept loop's exit; closing a conn twice is harmless.
	closeConns(conns)
	s.mu.Lock()
	late := s.snapshotConnsLocked()
	s.mu.Unlock()
	closeConns(late)

	// 5. Wait for the handlers, bounded.
	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()
	timer := time.NewTimer(s.drainTimeout)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		s.mu.Lock()
		stuck := len(s.conns)
		s.mu.Unlock()
		log.Printf("Warning: 9P server for %s: %d connection handler(s) still running %v after close; a request is wedged in a host syscall, and its goroutine and fids leak until that syscall returns",
			s.hostPath, stuck, s.drainTimeout)
	}

	return err
}

// snapshotConnsLocked copies the live connection set. Caller holds mu.
func (s *P9Server) snapshotConnsLocked() []net.Conn {
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	return conns
}

func closeConns(conns []net.Conn) {
	for _, c := range conns {
		_ = c.Close()
	}
}
