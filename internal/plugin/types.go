// Package plugin provides the namespaced message bus for communication between
// guest VM processes and external host processes, mediated by shed-server.
package plugin

import (
	"encoding/json"
	"time"

	"github.com/charliek/shed/sdk"
)

// MessageType identifies the role of a message in a conversation.
// Aliased from the SDK so existing internal code compiles without changes.
type MessageType = sdk.MessageType

const (
	MessageTypeRequest  = sdk.MessageTypeRequest
	MessageTypeResponse = sdk.MessageTypeResponse
	MessageTypeEvent    = sdk.MessageTypeEvent
)

// Envelope is the universal message format for all plugin communication.
type Envelope = sdk.Envelope

// ShedInfo identifies the shed instance that originated or is targeted by a message.
type ShedInfo = sdk.ShedInfo

// NewEnvelope creates a new envelope with a UUIDv7 ID and current timestamp.
// Final defaults to true; callers can set it to false for multi-response patterns.
func NewEnvelope(namespace string, msgType MessageType, payload json.RawMessage) *Envelope {
	return sdk.NewEnvelope(namespace, msgType, payload)
}

// NewResponse creates a response envelope linked to an original request.
func NewResponse(inReplyTo, namespace string, payload json.RawMessage) *Envelope {
	return sdk.NewResponse(inReplyTo, namespace, payload)
}

// ListenerInfo is the public representation of an active listener.
type ListenerInfo struct {
	Namespace string    `json:"namespace"`
	CreatedAt time.Time `json:"created_at"`
}
