package plugin

import (
	"time"

	"github.com/charliek/shed/internal/remote/guest"
)

// NamespaceHealth is the reserved namespace for health check messages.
const NamespaceHealth = "system:health"

// HeartbeatPayload is the payload for a system:health heartbeat event.
// Sent agent → host periodically on the persistent message channel.
type HeartbeatPayload struct {
	StartedAt  time.Time                  `json:"started_at"`           // agent boot time — detects restarts
	Extensions map[string]ExtensionHealth `json:"extensions,omitempty"` // per-extension health status
}

// HealthRequestPayload is the optional payload of a system:health request.
// The server includes it in the message-channel handshake to tell the agent
// which extensions to enable, and sets Probe on a health check.
type HealthRequestPayload struct {
	Extensions []string `json:"extensions,omitempty"`
	// Probe marks a short health check rather than the message channel's
	// handshake. The agent answers it with a HealthResponsePayload and
	// closes the connection: it does not promote it to the message channel,
	// start heartbeats or act on Extensions. An agent that predates the
	// field ignores it, answers with no payload and promotes the
	// connection as before.
	Probe bool `json:"probe,omitempty"`
}

// HealthResponsePayload is the payload of the agent's reply to a probe: its
// version, wire protocol and guest layout, the same object "shed-agent
// version" prints. A reply with no payload comes from an agent older than
// the probe field and parses (guest.ParseAgentInfo) as protocol 0.
type HealthResponsePayload = guest.AgentInfo

// Extension guest (systemd unit) status constants.
const (
	ExtGuestRunning = "running"
	ExtGuestStopped = "stopped"
	ExtGuestFailed  = "failed"
)

// Extension host (end-to-end reachability) status constants.
const (
	ExtHostConnected   = "connected"
	ExtHostUnreachable = "unreachable"
	ExtHostUnknown     = "unknown"
)

// ExtensionHealth reports the health of a single extension.
type ExtensionHealth struct {
	Guest string `json:"guest"` // ExtGuest* constants
	Host  string `json:"host"`  // ExtHost* constants
}
