package vmutil

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/plugin"
)

// MessageHandler implements NotifyHandler and handles all messages on the
// generalized message channel (port 1026). It routes system namespace messages
// (system:health) and external plugin messages.
//
// For bidirectional writes, the handler captures the connection from OnConnect
// and uses a write mutex. NotifyConn itself is not modified.
type MessageHandler struct {
	// Callback for system:health heartbeat events from the VM.
	// nil if health tracking is not configured.
	healthFn func(env *plugin.Envelope)

	// Callback for incoming plugin messages from the VM.
	pluginFn func(env *plugin.Envelope)

	// Connection captured in OnConnect, used for writes.
	conn    net.Conn
	writeMu sync.Mutex
}

// NewMessageHandler creates a handler for the generalized message channel.
//   - healthFn: called for system:health heartbeat events (nil if no health tracking)
//   - pluginFn: called for incoming plugin messages from the VM
func NewMessageHandler(healthFn func(env *plugin.Envelope), pluginFn func(env *plugin.Envelope)) *MessageHandler {
	return &MessageHandler{
		healthFn: healthFn,
		pluginFn: pluginFn,
	}
}

// OnConnect implements NotifyHandler. It stores the connection and sends
// an initial system:health request (as a handshake to trigger agent-side
// connection promotion and heartbeats).
func (h *MessageHandler) OnConnect(conn net.Conn) error {
	h.writeMu.Lock()
	h.conn = conn
	h.writeMu.Unlock()

	// Always send a health request as the first message. This serves as a
	// handshake that triggers the agent to promote this connection to the
	// persistent message channel and start heartbeats.
	healthEnv := plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeRequest, nil)
	if err := h.sendEnvelope(healthEnv); err != nil {
		return fmt.Errorf("send health handshake: %w", err)
	}

	return nil
}

// OnMessage implements NotifyHandler. It routes messages by type.
func (h *MessageHandler) OnMessage(msgType byte, data []byte) error {
	switch msgType {
	case agentproto.MsgTypePluginMessage:
		var env plugin.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return fmt.Errorf("invalid plugin envelope: %w", err)
		}

		// Consume all system:health messages (events, responses) — never forward to pluginFn.
		if env.Namespace == plugin.NamespaceHealth {
			if env.Type == plugin.MessageTypeEvent && h.healthFn != nil {
				h.healthFn(&env)
			}
			return nil
		}

		// Route all other plugin messages to the callback
		if h.pluginFn != nil {
			h.pluginFn(&env)
		}
		return nil

	default:
		log.Printf("MessageHandler: unexpected message type 0x%02x", msgType)
		return nil
	}
}

// SendPluginMessage writes a plugin envelope to the agent over the vsock
// message connection. Safe for concurrent use.
func (h *MessageHandler) SendPluginMessage(env *plugin.Envelope) error {
	return h.sendEnvelope(env)
}

// sendEnvelope marshals and writes an envelope to the vsock connection.
func (h *MessageHandler) sendEnvelope(env *plugin.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("failed to marshal plugin envelope: %w", err)
	}

	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	if h.conn == nil {
		return fmt.Errorf("message handler: no active connection")
	}

	return agentproto.WriteMessage(h.conn, agentproto.MsgTypePluginMessage, data)
}
