//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/charliek/shed/internal/plugin"
)

// handleMessageConnection handles the persistent message channel connection.
// It dispatches incoming messages by type to the appropriate handler.
func (s *Server) handleMessageConnection(conn net.Conn) {
	defer conn.Close()
	s.setMessageConn(conn)
	defer s.clearMessageConnIfCurrent(conn)

	for {
		msgType, data, err := readMessage(conn)
		if err != nil {
			if err != io.EOF {
				select {
				case <-s.ctx.Done():
				default:
					log.Printf("Message connection read error: %v", err)
				}
			}
			return
		}

		switch msgType {
		case MsgTypePluginMessage:
			s.handlePluginMessage(data)
		default:
			log.Printf("Unknown message type on message channel: 0x%02x", msgType)
		}
	}
}

// handlePluginMessage processes an incoming plugin envelope from the host.
func (s *Server) handlePluginMessage(data []byte) {
	var env plugin.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		log.Printf("Invalid plugin envelope: %v", err)
		return
	}

	switch env.Type {
	case plugin.MessageTypeResponse:
		// Deliver to a waiting request by InReplyTo ID.
		s.deliverResponse(&env)
	case plugin.MessageTypeRequest:
		// System namespace handlers
		if env.Namespace == plugin.NamespaceCredentials {
			s.handleCredentialSetup(&env)
			return
		}
		log.Printf("Plugin request from host: namespace=%s (no local handler)", env.Namespace)
	default:
		log.Printf("Plugin message from host: namespace=%s type=%s (no local handler)", env.Namespace, env.Type)
	}
}

// handleCredentialSetup processes a system:credentials setup request.
func (s *Server) handleCredentialSetup(env *plugin.Envelope) {
	var setup plugin.CredentialSetupPayload
	if err := json.Unmarshal(env.Payload, &setup); err != nil {
		log.Printf("Invalid credential setup payload: %v", err)
		return
	}

	// Cancel any previous credential watcher before starting a new one.
	// This prevents goroutine/fd leaks on reconnect.
	s.stopCredentialWatcher()

	ctx, cancel := context.WithCancel(s.ctx)
	s.msgMu.Lock()
	s.credCancel = cancel
	s.msgMu.Unlock()

	go s.startCredentialWatcher(ctx, &setup)
}

// deliverResponse routes a response envelope to the pending request channel.
func (s *Server) deliverResponse(env *plugin.Envelope) {
	if env.InReplyTo == "" {
		log.Printf("Plugin response missing in_reply_to field")
		return
	}

	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	ch, ok := s.pending[env.InReplyTo]
	if !ok {
		log.Printf("No pending request for response in_reply_to=%s", env.InReplyTo)
		return
	}

	select {
	case ch <- env:
	default:
		log.Printf("Pending channel full for in_reply_to=%s, dropping response", env.InReplyTo)
	}
}

// setMessageConn stores the active message connection for outbound plugin writes.
func (s *Server) setMessageConn(conn net.Conn) {
	s.msgMu.Lock()
	defer s.msgMu.Unlock()
	s.msgConn = conn
}

// clearMessageConnIfCurrent clears the message connection only if conn is still
// the active one. This prevents a closing connection A from blowing away the
// state of a newer connection B that replaced it during a reconnect.
func (s *Server) clearMessageConnIfCurrent(conn net.Conn) {
	s.msgMu.Lock()
	if s.msgConn != conn {
		s.msgMu.Unlock()
		return // a newer connection replaced us; don't touch its state
	}
	s.msgConn = nil
	s.msgMu.Unlock()

	s.stopCredentialWatcher()
	s.clearPending()
}

// stopCredentialWatcher cancels any running credential watcher goroutine.
func (s *Server) stopCredentialWatcher() {
	s.msgMu.Lock()
	if s.credCancel != nil {
		s.credCancel()
		s.credCancel = nil
	}
	s.msgMu.Unlock()
}

// clearPending closes and removes all pending request channels.
func (s *Server) clearPending() {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	for id, ch := range s.pending {
		close(ch)
		delete(s.pending, id)
	}
}

// sendPluginMessage writes a plugin envelope to the active vsock message connection.
func (s *Server) sendPluginMessage(env *plugin.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}

	s.msgMu.Lock()
	defer s.msgMu.Unlock()

	if s.msgConn == nil {
		return errNoConnection
	}

	// Set a write deadline to prevent blocking all senders if the host is unresponsive.
	if err := s.msgConn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	defer func() { _ = s.msgConn.SetWriteDeadline(time.Time{}) }()

	return writeMessage(s.msgConn, MsgTypePluginMessage, data)
}

// startCredentialWatcher handles a system:credentials setup request.
// It creates a credential watcher that sends change events as plugin envelopes.
func (s *Server) startCredentialWatcher(ctx context.Context, setup *plugin.CredentialSetupPayload) {
	if len(setup.Credentials) == 0 {
		log.Printf("Credential setup: no credentials to watch")
		return
	}

	log.Printf("Credential setup: watching %d credentials", len(setup.Credentials))
	for name, path := range setup.Credentials {
		log.Printf("  %s → %s", name, path)
	}

	// Send function wraps changes in a plugin envelope
	sendFn := func(credName string, files []string) error {
		payload := plugin.CredentialChangedPayload{
			Credential: credName,
			Files:      files,
		}
		payloadData, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		env := plugin.NewEnvelope(plugin.NamespaceCredentials, plugin.MessageTypeEvent, payloadData)
		return s.sendPluginMessage(env)
	}

	cw, err := newCredentialWatcher(sendFn, setup.Credentials, setup.Excludes)
	if err != nil {
		log.Printf("Failed to create credential watcher: %v", err)
		return
	}

	// Run until context is cancelled (connection closes or new setup replaces this watcher)
	cw.start(ctx.Done())
	log.Printf("Credential watcher stopped")
}
