// Package plugin provides the namespaced message bus for communication between
// guest VM processes and external host processes, mediated by shed-server.
package plugin

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// MessageType identifies the role of a message in a conversation.
type MessageType string

const (
	MessageTypeRequest  MessageType = "request"
	MessageTypeResponse MessageType = "response"
	MessageTypeEvent    MessageType = "event"
)

// Envelope is the universal message format for all plugin communication.
type Envelope struct {
	ID        string          `json:"id"`
	Namespace string          `json:"namespace"`
	Type      MessageType     `json:"type"`
	InReplyTo string          `json:"in_reply_to,omitempty"`
	Final     bool            `json:"final"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
	Shed      *ShedInfo       `json:"shed,omitempty"`
}

// ShedInfo identifies the shed instance that originated or is targeted by a message.
type ShedInfo struct {
	Name    string `json:"name"`    // shed instance name
	Backend string `json:"backend"` // "vz", "firecracker", "docker"
	Server  string `json:"server"`  // server name from config
}

// ListenerInfo is the public representation of an active listener.
type ListenerInfo struct {
	Namespace string    `json:"namespace"`
	CreatedAt time.Time `json:"created_at"`
}

// NewEnvelope creates a new envelope with a UUIDv7 ID and current timestamp.
// Final defaults to true; callers can set it to false for multi-response patterns.
func NewEnvelope(namespace string, msgType MessageType, payload json.RawMessage) *Envelope {
	return &Envelope{
		ID:        uuid.Must(uuid.NewV7()).String(),
		Namespace: namespace,
		Type:      msgType,
		Final:     true,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
}

// NewResponse creates a response envelope linked to an original request.
func NewResponse(inReplyTo, namespace string, payload json.RawMessage) *Envelope {
	return &Envelope{
		ID:        uuid.Must(uuid.NewV7()).String(),
		Namespace: namespace,
		Type:      MessageTypeResponse,
		InReplyTo: inReplyTo,
		Final:     true,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
}
