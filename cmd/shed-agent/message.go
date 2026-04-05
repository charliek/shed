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

// handleNotifyConnection handles a connection on the notify port. It reads the
// first message to determine the connection type:
//   - Transient health check (system:health request): respond inline and close
//   - Persistent message channel: promote to msgConn, start heartbeats, enter read loop
func (s *Server) handleNotifyConnection(conn net.Conn) {
	defer conn.Close()

	// Read the first message to determine connection type.
	msgType, data, err := readMessage(conn)
	if err != nil {
		if err != io.EOF {
			select {
			case <-s.ctx.Done():
			default:
				log.Printf("Notify connection read error: %v", err)
			}
		}
		return
	}

	// The persistent message channel starts with a system:health handshake
	// from the host. Respond to it, then promote this connection and start heartbeats.
	// Transient health checks (from WaitForHealth during startup) use the same
	// protocol but close the connection after one exchange — the agent handles
	// both identically, and clearMessageConnIfCurrent safely cleans up when
	// the transient connection closes.
	s.setMessageConn(conn)
	defer s.clearMessageConnIfCurrent(conn)

	connCtx, connCancel := context.WithCancel(s.ctx)
	defer connCancel()

	go s.runHeartbeat(connCtx)

	// Process the first message we already read, then enter the read loop.
	s.dispatchMessage(conn, msgType, data)
	s.readMessageLoop(conn)
}

// dispatchMessage routes a single message by type.
func (s *Server) dispatchMessage(conn net.Conn, msgType byte, data []byte) {
	switch msgType {
	case MsgTypePluginMessage:
		s.handlePluginMessage(conn, data)
	default:
		log.Printf("Unknown message type on message channel: 0x%02x", msgType)
	}
}

// readMessageLoop reads and dispatches messages until the connection closes.
func (s *Server) readMessageLoop(conn net.Conn) {
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
		s.dispatchMessage(conn, msgType, data)
	}
}

// runHeartbeat sends periodic system:health events on the persistent connection.
// It sends one immediately, then ticks at s.heartbeatInterval.
// Exits on context cancellation or send error.
func (s *Server) runHeartbeat(ctx context.Context) {
	sendHeartbeat := func() error {
		payload := plugin.HeartbeatPayload{StartedAt: s.startedAt}
		payloadData, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal heartbeat: %w", err)
		}
		env := plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeEvent, payloadData)
		return s.sendPluginMessage(env)
	}

	// Send immediately on connect
	if err := sendHeartbeat(); err != nil {
		log.Printf("Heartbeat send failed: %v", err)
		return
	}

	ticker := time.NewTicker(s.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sendHeartbeat(); err != nil {
				log.Printf("Heartbeat send failed: %v", err)
				return
			}
		}
	}
}

// handlePluginMessage processes an incoming plugin envelope from the host.
// conn is the connection that carried the request, used for direct replies
// (e.g. system:health) that should not route through the global sendPluginMessage path.
func (s *Server) handlePluginMessage(conn net.Conn, data []byte) {
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
		switch env.Namespace {
		case plugin.NamespaceHealth:
			s.handleHealthRequest(conn, &env)
		default:
			log.Printf("Plugin request from host: namespace=%s (no local handler)", env.Namespace)
		}
	default:
		log.Printf("Plugin message from host: namespace=%s type=%s (no local handler)", env.Namespace, env.Type)
	}
}

// handleHealthRequest responds to a system:health request by writing directly
// to the request connection. This avoids the global sendPluginMessage/msgConn
// path, which is important during startup when transient health poll connections
// briefly occupy msgConn.
func (s *Server) handleHealthRequest(conn net.Conn, env *plugin.Envelope) {
	resp := plugin.NewResponse(env.ID, plugin.NamespaceHealth, nil)
	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("Failed to marshal health response: %v", err)
		return
	}
	if err := writeMessage(conn, MsgTypePluginMessage, data); err != nil {
		log.Printf("Failed to write health response: %v", err)
	}
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

	s.clearPending()
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

