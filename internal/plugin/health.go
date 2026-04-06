package plugin

import "time"

// NamespaceHealth is the reserved namespace for health check messages.
const NamespaceHealth = "system:health"

// HeartbeatPayload is the payload for a system:health heartbeat event.
// Sent agent → host periodically on the persistent message channel.
type HeartbeatPayload struct {
	StartedAt  time.Time                  `json:"started_at"`           // agent boot time — detects restarts
	Extensions map[string]ExtensionHealth `json:"extensions,omitempty"` // per-extension health status
}

// HealthRequestPayload is the optional payload of a system:health request.
// The server includes it in the handshake to tell the agent which extensions to enable.
type HealthRequestPayload struct {
	Extensions []string `json:"extensions,omitempty"`
}

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
