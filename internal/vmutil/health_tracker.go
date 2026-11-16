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
//
// A VM's state may be claimed by one message channel at a time (Claim): from
// then on only that channel's heartbeats (UpdateFrom) change it, so a
// heartbeat from a channel that was replaced, still running, can never
// overwrite what the current channel reported.
type HealthTracker struct {
	mu     sync.RWMutex
	status map[string]HealthStatus
	owners map[string]uint64 // VM name -> the channel that claimed it
}

// NewHealthTracker creates a new HealthTracker.
func NewHealthTracker() *HealthTracker {
	return &HealthTracker{
		status: make(map[string]HealthStatus),
		owners: make(map[string]uint64),
	}
}

// Claim makes channel owner (a nonzero id) the only one whose heartbeats
// UpdateFrom accepts for name. What another channel reported is dropped: it
// describes the agent that channel reached, which a replacing channel (a shed
// created again under the name) has not reached.
func (ht *HealthTracker) Claim(name string, owner uint64) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	if prev, ok := ht.owners[name]; ok && prev != owner {
		delete(ht.status, name)
	}
	ht.owners[name] = owner
}

// UpdateFrom is Update for a heartbeat that arrived on channel owner, and is
// ignored unless owner holds the claim on name.
func (ht *HealthTracker) UpdateFrom(name string, owner uint64, agentStartedAt time.Time, extensions map[string]plugin.ExtensionHealth) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	if ht.owners[name] != owner {
		return
	}
	ht.updateLocked(name, agentStartedAt, extensions)
}

// RemoveFrom is Remove for channel owner: it removes name's state and claim
// only while owner holds the claim.
func (ht *HealthTracker) RemoveFrom(name string, owner uint64) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	if ht.owners[name] != owner {
		return
	}
	delete(ht.owners, name)
	delete(ht.status, name)
}

// Update records a heartbeat for the named VM using the host clock.
// The extensions map is defensively copied to prevent aliasing.
func (ht *HealthTracker) Update(name string, agentStartedAt time.Time, extensions map[string]plugin.ExtensionHealth) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	ht.updateLocked(name, agentStartedAt, extensions)
}

func (ht *HealthTracker) updateLocked(name string, agentStartedAt time.Time, extensions map[string]plugin.ExtensionHealth) {
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

// Remove deletes health state for a VM (called on stop/cleanup), and its
// claim.
func (ht *HealthTracker) Remove(name string) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	delete(ht.status, name)
	delete(ht.owners, name)
}
