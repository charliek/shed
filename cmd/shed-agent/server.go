//go:build linux
// +build linux

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/charliek/shed/internal/plugin"
	"github.com/mdlayher/vsock"
)

// userInfo holds resolved user credentials for spawning processes.
type userInfo struct {
	cred    *syscall.Credential
	homeDir string
	name    string
}

const (
	// DefaultConsolePort is the vsock port for console/exec connections.
	DefaultConsolePort = 1024

	// DefaultHealthPort is the vsock port for health checks.
	DefaultHealthPort = 1025

	// DefaultNotifyPort is the vsock port for the message channel (plugins + credentials).
	DefaultNotifyPort = 1026

	// DefaultHTTPPort is the localhost port for the in-VM plugin HTTP API.
	DefaultHTTPPort = 498
)

// Server handles vsock connections from the host.
type Server struct {
	consolePort uint32
	healthPort  uint32
	notifyPort  uint32
	httpPort    uint32
	shedName    string

	consoleListener net.Listener
	healthListener  net.Listener
	notifyListener  net.Listener

	httpServer *http.Server

	// Resolved non-root user for spawning processes (nil = run as root)
	user *userInfo

	// Active message connection (for writing plugin messages to host).
	// msgMu protects both the connection reference and serializes writes.
	msgMu   sync.Mutex
	msgConn net.Conn

	// Pending request/response tracking
	pendingMu sync.Mutex
	pending   map[string]chan *plugin.Envelope // request ID -> response channel

	// For graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewServer creates a new Server.
func NewServer(consolePort, healthPort, notifyPort, httpPort uint32, shedName string) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		consolePort: consolePort,
		healthPort:  healthPort,
		notifyPort:  notifyPort,
		httpPort:    httpPort,
		shedName:    shedName,
		pending:     make(map[string]chan *plugin.Envelope),
		ctx:         ctx,
		cancel:      cancel,
	}

	// Resolve the non-root "shed" user at startup.
	// If lookup fails (degraded rootfs), fall back to running as root.
	if u, err := user.Lookup("shed"); err != nil {
		log.Printf("Warning: shed user not found, running commands as root: %v", err)
	} else if uid, err := strconv.ParseUint(u.Uid, 10, 32); err != nil {
		log.Printf("Warning: failed to parse shed UID %q: %v", u.Uid, err)
	} else if gid, err := strconv.ParseUint(u.Gid, 10, 32); err != nil {
		log.Printf("Warning: failed to parse shed GID %q: %v", u.Gid, err)
	} else {
		groupIDs, err := u.GroupIds()
		if err != nil {
			log.Printf("Warning: failed to get supplementary groups for shed, running commands as root: %v", err)
			return s
		}
		var groups []uint32
		for _, gidStr := range groupIDs {
			g, err := strconv.ParseUint(gidStr, 10, 32)
			if err != nil {
				log.Printf("Warning: failed to parse group ID %q: %v", gidStr, err)
				continue
			}
			groups = append(groups, uint32(g))
		}
		if len(groups) == 0 {
			log.Printf("Warning: shed has no valid supplementary groups, running commands as root")
			return s
		}

		s.user = &userInfo{
			cred: &syscall.Credential{
				Uid:    uint32(uid),
				Gid:    uint32(gid),
				Groups: groups,
			},
			homeDir: u.HomeDir,
			name:    u.Username,
		}
		log.Printf("Resolved shed user: uid=%d gid=%d groups=%v home=%s", uid, gid, groups, u.HomeDir)
	}

	return s
}

// Start starts the vsock listeners.
func (s *Server) Start() error {
	// Start console listener
	var err error
	s.consoleListener, err = vsock.Listen(s.consolePort, nil)
	if err != nil {
		return err
	}

	// Start health listener
	s.healthListener, err = vsock.Listen(s.healthPort, nil)
	if err != nil {
		s.consoleListener.Close()
		return err
	}

	// Start notify listener
	s.notifyListener, err = vsock.Listen(s.notifyPort, nil)
	if err != nil {
		s.consoleListener.Close()
		s.healthListener.Close()
		return err
	}

	// Accept console connections
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := s.consoleListener.Accept()
			if err != nil {
				select {
				case <-s.ctx.Done():
					// Graceful shutdown, don't log error
					return
				default:
					log.Printf("Console accept error: %v", err)
					time.Sleep(200 * time.Millisecond)
					continue
				}
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				handleExecConnection(conn, s.user)
			}()
		}
	}()

	// Accept health connections
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := s.healthListener.Accept()
			if err != nil {
				select {
				case <-s.ctx.Done():
					// Graceful shutdown, don't log error
					return
				default:
					log.Printf("Health accept error: %v", err)
					time.Sleep(200 * time.Millisecond)
					continue
				}
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handleHealthConnection(conn)
			}()
		}
	}()

	// Accept message channel connections (generalized notify port)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := s.notifyListener.Accept()
			if err != nil {
				select {
				case <-s.ctx.Done():
					return
				default:
					log.Printf("Message channel accept error: %v", err)
					time.Sleep(200 * time.Millisecond)
					continue
				}
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handleMessageConnection(conn)
			}()
		}
	}()

	// Start localhost HTTP server for in-VM plugin API
	if err := s.startHTTPServer(); err != nil {
		log.Printf("Warning: failed to start HTTP server: %v", err)
	}

	log.Printf("Listening on vsock ports: console=%d, health=%d, message=%d; HTTP on 127.0.0.1:%d",
		s.consolePort, s.healthPort, s.notifyPort, s.httpPort)
	return nil
}

// startHTTPServer starts the localhost HTTP API for plugin message publishing.
func (s *Server) startHTTPServer() error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/publish", s.handlePublish)

	addr := fmt.Sprintf("127.0.0.1:%d", s.httpPort)

	// Bind synchronously so we detect port conflicts immediately.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	s.httpServer = &http.Server{
		Handler:        mux,
		ReadTimeout:    60 * time.Second,
		WriteTimeout:   60 * time.Second,
		MaxHeaderBytes: 1 << 16, // 64 KB
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	return nil
}

// drainTimeout is the maximum time to wait for active connections to finish
// before forcing shutdown. This prevents hung connection handlers from blocking
// VM shutdown indefinitely.
const drainTimeout = 5 * time.Second

// Stop stops the vsock listeners and waits for active connections to finish.
// If connections don't drain within drainTimeout, shutdown proceeds anyway.
func (s *Server) Stop() {
	// Signal shutdown
	s.cancel()

	// Close listeners to stop accepting new connections
	if s.consoleListener != nil {
		s.consoleListener.Close()
	}
	if s.healthListener != nil {
		s.healthListener.Close()
	}
	if s.notifyListener != nil {
		s.notifyListener.Close()
	}

	// Shutdown HTTP server
	if s.httpServer != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), drainTimeout)
		defer shutdownCancel()
		s.httpServer.Shutdown(shutdownCtx)
	}

	// Clean up pending requests
	s.clearPending()

	// Wait for active connections to finish, with timeout
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Printf("Server stopped gracefully")
	case <-time.After(drainTimeout):
		log.Printf("Server drain timeout (%v) reached, forcing exit", drainTimeout)
	}
}

// handleHealthConnection handles a health check connection.
func (s *Server) handleHealthConnection(conn net.Conn) {
	defer conn.Close()

	// Read the health request
	msgType, _, err := readMessage(conn)
	if err != nil {
		log.Printf("Failed to read health request: %v", err)
		return
	}

	if msgType != MsgTypeHealthRequest {
		log.Printf("Unexpected message type on health port: %d", msgType)
		return
	}

	// Send health response
	if err := writeMessage(conn, MsgTypeHealthResponse, nil); err != nil {
		log.Printf("Failed to write health response: %v", err)
		return
	}
}
