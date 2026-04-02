package plugin

import "time"

// NamespaceHealth is the reserved namespace for health check messages.
const NamespaceHealth = "system:health"

// HeartbeatPayload is the payload for a system:health heartbeat event.
// Sent agent → host periodically on the persistent message channel.
type HeartbeatPayload struct {
	StartedAt time.Time `json:"started_at"` // agent boot time — detects restarts
}
