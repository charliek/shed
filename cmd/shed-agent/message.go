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

// probeLinger bounds how long the agent waits, after answering a probe, for
// the host to hang up first. Waiting lets the reply drain on every transport
// before the agent's close; the host normally closes as soon as it has read
// the reply.
const probeLinger = 2 * time.Second

// handleNotifyConnection handles a connection on the notify port. It reads the
// first message to determine the connection type:
//   - Probe (system:health request with probe:true): reply with the agent's
//     version, protocol and layout, then close. Never promoted.
//   - Anything else: the persistent message channel. Promote to msgConn,
//     start heartbeats, enter the read loop.
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

	if env, ok := parseProbe(msgType, data); ok {
		s.handleProbe(conn, env)
		return
	}

	// The persistent message channel starts with a system:health handshake
	// from the host. Respond to it, then promote this connection and start
	// heartbeats. A server older than the probe field sends its transient
	// health checks the same way; those are promoted too, and
	// clearMessageConnIfCurrent cleans up when they close. That is the bug
	// probe:true exists to avoid: a check arriving while the real channel is
	// up steals the msgConn slot, leaving the real channel silent.
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

		// Include per-extension health if extensions are enabled.
		extHealth := s.collectExtensionHealth()
		if len(extHealth) > 0 {
			payload.Extensions = extHealth
		}

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
//
// If the request includes a HealthRequestPayload with extension namespaces,
// the agent activates the corresponding systemd units (idempotent).
func (s *Server) handleHealthRequest(conn net.Conn, env *plugin.Envelope) {
	resp := plugin.NewResponse(env.ID, plugin.NamespaceHealth, nil)
	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("Failed to marshal health response: %v", err)
		return
	}
	// Written under msgMu: on a promoted connection the heartbeat goroutine
	// writes too, and a frame is two writes, so unserialised writes could
	// interleave. The write is bounded like sendPluginMessage's: msgMu also
	// guards msgConn, so a host that stopped reading must not be able to
	// hold it (and with it every sender and the next connection's
	// promotion) for longer than writeTimeout.
	s.msgMu.Lock()
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	err = writeMessage(conn, MsgTypePluginMessage, data)
	_ = conn.SetWriteDeadline(time.Time{})
	s.msgMu.Unlock()
	if err != nil {
		log.Printf("Failed to write health response: %v", err)
	}

	// Parse the optional extensions payload from the health handshake.
	// Transient health polls from servers older than the probe field send a
	// nil payload; only the persistent message channel handshake includes
	// extensions. (Probes never reach here: handleNotifyConnection answers
	// them with handleProbe.)
	if len(env.Payload) > 0 {
		var reqPayload plugin.HealthRequestPayload
		if err := json.Unmarshal(env.Payload, &reqPayload); err != nil {
			log.Printf("Failed to parse health request payload: %v", err)
			return
		}
		if len(reqPayload.Extensions) > 0 {
			s.enableExtensions(reqPayload.Extensions)
		}
	}
}

// parseProbe reports whether the first message on a notify connection is a
// probe: a system:health request whose payload sets probe:true.
func parseProbe(msgType byte, data []byte) (*plugin.Envelope, bool) {
	if msgType != MsgTypePluginMessage {
		return nil, false
	}
	var env plugin.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, false
	}
	if env.Type != plugin.MessageTypeRequest || env.Namespace != plugin.NamespaceHealth || len(env.Payload) == 0 {
		return nil, false
	}
	var req plugin.HealthRequestPayload
	if err := json.Unmarshal(env.Payload, &req); err != nil || !req.Probe {
		return nil, false
	}
	return &env, true
}

// handleProbe answers a probe with the agent's version, protocol and layout.
// It does not promote the connection, start heartbeats or act on extensions.
// After the reply it waits (at most probeLinger) for the host to close, then
// the caller closes the connection.
func (s *Server) handleProbe(conn net.Conn, env *plugin.Envelope) {
	payload, err := json.Marshal(s.info)
	if err != nil {
		log.Printf("Failed to marshal probe reply: %v", err)
		return
	}
	data, err := json.Marshal(plugin.NewResponse(env.ID, plugin.NamespaceHealth, payload))
	if err != nil {
		log.Printf("Failed to marshal probe reply: %v", err)
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := writeMessage(conn, MsgTypePluginMessage, data); err != nil {
		log.Printf("Failed to write probe reply: %v", err)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(probeLinger))
	_, _ = io.Copy(io.Discard, io.LimitReader(conn, MaxMessageSize))
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
