package vmutil

import (
	"maps"
	"sync"
	"time"

	"github.com/charliek/shed/internal/plugin"
)

// HealthStatus holds the last-known health state for a VM.
type HealthStatus struct {
	LastSeen       time.Time                         // host-side receipt time of last heartbeat
	AgentStartedAt time.Time                         // agent boot time from heartbeat payload
	Extensions     map[string]plugin.ExtensionHealth // per-extension health from latest heartbeat
}

// HealthTracker stores per-VM health state from agent heartbeats.
// Safe for concurrent use.
type HealthTracker struct {
	mu     sync.RWMutex
	status map[string]HealthStatus
}

// NewHealthTracker creates a new HealthTracker.
func NewHealthTracker() *HealthTracker {
	return &HealthTracker{
		status: make(map[string]HealthStatus),
	}
}

// Update records a heartbeat for the named VM using the host clock.
// The extensions map is defensively copied to prevent aliasing.
func (ht *HealthTracker) Update(name string, agentStartedAt time.Time, extensions map[string]plugin.ExtensionHealth) {
	ht.mu.Lock()
	defer ht.mu.Unlock()

	hs := HealthStatus{
		LastSeen:       time.Now(),
		AgentStartedAt: agentStartedAt,
	}

	// Defensive copy of the extensions map
	if len(extensions) > 0 {
		hs.Extensions = make(map[string]plugin.ExtensionHealth, len(extensions))
		maps.Copy(hs.Extensions, extensions)
	}

	ht.status[name] = hs
}

// Get returns the health status for a VM, or false if no heartbeat has been received.
// The returned Extensions map is a defensive copy.
func (ht *HealthTracker) Get(name string) (HealthStatus, bool) {
	ht.mu.RLock()
	defer ht.mu.RUnlock()
	s, ok := ht.status[name]
	if !ok {
		return s, false
	}

	// Defensive copy of the extensions map
	if len(s.Extensions) > 0 {
		ext := make(map[string]plugin.ExtensionHealth, len(s.Extensions))
		maps.Copy(ext, s.Extensions)
		s.Extensions = ext
	}

	return s, true
}

// Remove deletes health state for a VM (called on stop/cleanup).
func (ht *HealthTracker) Remove(name string) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	delete(ht.status, name)
}
