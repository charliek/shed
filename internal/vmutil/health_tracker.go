package vmutil

import (
	"sync"
	"time"
)

// HealthStatus holds the last-known health state for a VM.
type HealthStatus struct {
	LastSeen       time.Time // host-side receipt time of last heartbeat
	AgentStartedAt time.Time // agent boot time from heartbeat payload
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
func (ht *HealthTracker) Update(name string, agentStartedAt time.Time) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	ht.status[name] = HealthStatus{
		LastSeen:       time.Now(),
		AgentStartedAt: agentStartedAt,
	}
}

// Get returns the health status for a VM, or false if no heartbeat has been received.
func (ht *HealthTracker) Get(name string) (HealthStatus, bool) {
	ht.mu.RLock()
	defer ht.mu.RUnlock()
	s, ok := ht.status[name]
	return s, ok
}

// Remove deletes health state for a VM (called on stop/cleanup).
func (ht *HealthTracker) Remove(name string) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	delete(ht.status, name)
}
