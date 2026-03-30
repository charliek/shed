//go:build linux
// +build linux

package firecracker

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/hugelgupf/p9/fsimpl/localfs"
	"github.com/hugelgupf/p9/p9"
)

// P9Server wraps a 9P TCP server for sharing a host directory with a
// Firecracker VM. The guest connects to the server's TCP address and mounts
// the shared directory using the 9P protocol.
type P9Server struct {
	listener  net.Listener
	server    *p9.Server
	hostPath  string // path on host being shared
	mountPath string // target path inside guest
	readOnly  bool
	wg        sync.WaitGroup // tracks active connections for graceful drain
	done      chan struct{}
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
		listener:  listener,
		server:    srv,
		hostPath:  hostPath,
		mountPath: mountPath,
		readOnly:  readOnly,
		done:      make(chan struct{}),
	}, nil
}

// Start begins accepting 9P connections in the background. Each accepted
// connection is handled in its own goroutine, tracked by wg for graceful
// shutdown.
func (s *P9Server) Start() {
	go func() {
		defer close(s.done)
		for {
			conn, err := s.listener.Accept()
			if err != nil {
				// Listener was closed; stop accepting.
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer conn.Close()
				// Handle blocks until the connection is done.
				_ = s.server.Handle(conn, conn)
			}()
		}
	}()
}

// Addr returns the host:port address the server is listening on. The guest
// uses this address to connect via 9P over TCP.
func (s *P9Server) Addr() string {
	return s.listener.Addr().String()
}

// Close stops the server by closing the listener and waits up to 5 seconds
// for active connections to drain before returning.
func (s *P9Server) Close() error {
	err := s.listener.Close()

	// Wait for the accept loop to exit.
	<-s.done

	// Wait for in-flight connections with a timeout.
	finished := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
	}

	return err
}
