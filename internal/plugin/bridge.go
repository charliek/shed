package plugin

import (
	"fmt"
	"sync"
)

// ShedConn represents an active message channel to a running shed.
type ShedConn struct {
	Name    string
	Backend string
	Server  string
	Send    func(env *Envelope) error // writes MsgTypePluginMessage to vsock
}

// ShedConnInfo is the public representation of a connected shed.
type ShedConnInfo struct {
	Name    string `json:"name"`
	Backend string `json:"backend"`
	Server  string `json:"server"`
}

// Bridge connects the API layer to per-shed vsock message connections and
// routes messages between host listeners and VM agents.
type Bridge struct {
	registry *Registry
	mu       sync.RWMutex
	sheds    map[string]*ShedConn
}

// NewBridge creates a new bridge backed by the given registry.
func NewBridge(registry *Registry) *Bridge {
	return &Bridge{
		registry: registry,
		sheds:    make(map[string]*ShedConn),
	}
}

// RegisterShed registers a shed's active message connection with the bridge.
// The connection's Name is set to match the registration key.
func (b *Bridge) RegisterShed(name string, conn *ShedConn) {
	conn.Name = name
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sheds[name] = conn
}

// UnregisterShed removes a shed's message connection from the bridge.
func (b *Bridge) UnregisterShed(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sheds, name)
}

// ListSheds returns info about all sheds with active message channels.
func (b *Bridge) ListSheds() []ShedConnInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]ShedConnInfo, 0, len(b.sheds))
	for _, conn := range b.sheds {
		result = append(result, ShedConnInfo{
			Name:    conn.Name,
			Backend: conn.Backend,
			Server:  conn.Server,
		})
	}
	return result
}

// PublishToHost enriches the envelope with shed metadata and routes it to the
// registered listener for the envelope's namespace.
func (b *Bridge) PublishToHost(shedName string, env *Envelope) error {
	b.mu.RLock()
	conn, ok := b.sheds[shedName]
	b.mu.RUnlock()

	if ok {
		env.Shed = &ShedInfo{
			Name:    conn.Name,
			Backend: conn.Backend,
			Server:  conn.Server,
		}
	}

	return b.registry.Publish(env)
}

// SendToShed routes an envelope to the named shed's vsock connection.
func (b *Bridge) SendToShed(shedName string, env *Envelope) error {
	b.mu.RLock()
	conn, ok := b.sheds[shedName]
	b.mu.RUnlock()

	if !ok {
		return fmt.Errorf("shed %q is not connected", shedName)
	}

	return conn.Send(env)
}
