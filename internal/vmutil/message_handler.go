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
// (system:credentials, system:health) and external plugin messages.
//
// For bidirectional writes, the handler captures the connection from OnConnect
// and uses a write mutex. NotifyConn itself is not modified.
type MessageHandler struct {
	// Credential configuration for sending setup on connect.
	// nil if no credentials need watching.
	credSetup *plugin.CredentialSetupPayload

	// Callback for credential file change events from the VM.
	credChangeFn func(credName string, files []string)

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
//   - credSetup: credential configuration to send on connect (nil if no credentials)
//   - credChangeFn: called when credential files change in the VM (nil if no credentials)
//   - healthFn: called for system:health heartbeat events (nil if no health tracking)
//   - pluginFn: called for incoming plugin messages from the VM
func NewMessageHandler(credSetup *plugin.CredentialSetupPayload, credChangeFn func(string, []string), healthFn func(env *plugin.Envelope), pluginFn func(env *plugin.Envelope)) *MessageHandler {
	return &MessageHandler{
		credSetup:    credSetup,
		credChangeFn: credChangeFn,
		healthFn:     healthFn,
		pluginFn:     pluginFn,
	}
}

// OnConnect implements NotifyHandler. It stores the connection and sends
// credential setup via a system:credentials envelope if configured.
func (h *MessageHandler) OnConnect(conn net.Conn) error {
	h.writeMu.Lock()
	h.conn = conn
	h.writeMu.Unlock()

	if h.credSetup != nil && len(h.credSetup.Credentials) > 0 {
		payloadData, err := json.Marshal(h.credSetup)
		if err != nil {
			return fmt.Errorf("marshal credential setup: %w", err)
		}
		env := plugin.NewEnvelope(plugin.NamespaceCredentials, plugin.MessageTypeRequest, payloadData)
		if err := h.sendEnvelope(env); err != nil {
			return fmt.Errorf("send credential setup: %w", err)
		}
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

		// Route system namespace events to their handlers.
		// These are consumed here and not forwarded to pluginFn.
		if env.Type == plugin.MessageTypeEvent {
			switch env.Namespace {
			case plugin.NamespaceCredentials:
				return h.handleCredentialChanged(&env)
			case plugin.NamespaceHealth:
				if h.healthFn != nil {
					h.healthFn(&env)
				}
				return nil
			}
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

// handleCredentialChanged processes a system:credentials change event.
func (h *MessageHandler) handleCredentialChanged(env *plugin.Envelope) error {
	var changed plugin.CredentialChangedPayload
	if err := json.Unmarshal(env.Payload, &changed); err != nil {
		return fmt.Errorf("invalid credential changed payload: %w", err)
	}

	if h.credChangeFn != nil {
		h.credChangeFn(changed.Credential, changed.Files)
	}
	return nil
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
