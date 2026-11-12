package plugin

import (
	"errors"
	"fmt"
	"log"
	"sync"
)

// ShedConn represents an active message channel to a running shed.
type ShedConn struct {
	Name    string
	Backend string
	Server  string
	Send    func(env *Envelope) error // writes MsgTypePluginMessage to vsock
	// Owned marks a shed a service client owns. The credential bus is off
	// for it, always: the bridge drops its envelopes to the host, refuses to
	// send to it, and leaves it out of ListSheds.
	Owned bool
}

// ErrCredentialBusDisabled is returned by SendToShed for a client-owned shed.
var ErrCredentialBusDisabled = errors.New("credential bus is disabled for client-owned sheds")

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

	ownedDropLogged sync.Map // shed name -> true, once a drop was logged
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
		if conn.Owned {
			continue
		}
		result = append(result, ShedConnInfo{
			Name:    conn.Name,
			Backend: conn.Backend,
			Server:  conn.Server,
		})
	}
	return result
}

// PublishToHost enriches the envelope with shed metadata and routes it to the
// registered listener for the envelope's namespace. An envelope from a
// client-owned shed is dropped (nil error): the credential bus is off for it.
func (b *Bridge) PublishToHost(shedName string, env *Envelope) error {
	b.mu.RLock()
	conn, ok := b.sheds[shedName]
	b.mu.RUnlock()

	if ok && conn.Owned {
		b.logOwnedDrop(shedName)
		return nil
	}
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
	if conn.Owned {
		return fmt.Errorf("shed %q: %w", shedName, ErrCredentialBusDisabled)
	}

	return conn.Send(env)
}

// logOwnedDrop logs, once per shed name, that an owned shed's envelope was
// dropped.
func (b *Bridge) logOwnedDrop(shedName string) {
	if _, loaded := b.ownedDropLogged.LoadOrStore(shedName, true); !loaded {
		log.Printf("[%s] credential bus is disabled for client-owned sheds; dropping its plugin messages", shedName)
	}
}
