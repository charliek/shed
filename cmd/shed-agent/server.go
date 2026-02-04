package main

import (
	"context"
	"log"
	"net"
	"sync"

	"github.com/mdlayher/vsock"
)

const (
	// DefaultConsolePort is the vsock port for console/exec connections.
	DefaultConsolePort = 1024

	// DefaultHealthPort is the vsock port for health checks.
	DefaultHealthPort = 1025
)

// Server handles vsock connections from the host.
type Server struct {
	consolePort uint32
	healthPort  uint32

	consoleListener net.Listener
	healthListener  net.Listener

	// For graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewServer creates a new Server.
func NewServer(consolePort, healthPort uint32) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		consolePort: consolePort,
		healthPort:  healthPort,
		ctx:         ctx,
		cancel:      cancel,
	}
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
					return
				}
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				handleExecConnection(conn)
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
					return
				}
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handleHealthConnection(conn)
			}()
		}
	}()

	log.Printf("Listening on vsock ports: console=%d, health=%d", s.consolePort, s.healthPort)
	return nil
}

// Stop stops the vsock listeners and waits for active connections to finish.
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

	// Wait for active connections to finish
	s.wg.Wait()
	log.Printf("Server stopped gracefully")
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
