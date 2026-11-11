package vmutil

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
)

// DirMountFunc is the backend-specific directory mount callback.
// VZ implements this with mountVirtioFSShare, Firecracker with mount9PInGuest.
// Return error is treated as non-fatal for credential mounts (logged as warning),
// but callers may treat workspace mount errors as fatal.
type DirMountFunc func(ctx context.Context, agent *AgentClient, name string, mount config.MountConfig) error

// CredentialManager handles the credential lifecycle shared by VM backends.
// It owns per-VM message channels (for plugin messages and health tracking)
// and the plugin bridge registration.
type CredentialManager struct {
	serverCfg     *config.ServerConfig
	bridge        *plugin.Bridge // plugin message bridge (nil if plugins disabled)
	backendName   string         // "vz" or "firecracker"
	healthTracker *HealthTracker // tracks per-VM heartbeat state

	mu              sync.Mutex
	messageChannels map[string]messageChannel // name -> per-VM message channel
}

// messageChannel is a shed's message channel and whether it was started for
// an owned shed. The owned flag is captured by the channel's handler and its
// bridge registration, so a channel is reused only for the same flag.
type messageChannel struct {
	conn  *NotifyConn
	owned bool
}

// NewCredentialManager creates a new CredentialManager.
func NewCredentialManager(serverCfg *config.ServerConfig, bridge *plugin.Bridge, backendName string, healthTracker *HealthTracker) *CredentialManager {
	return &CredentialManager{
		serverCfg:       serverCfg,
		bridge:          bridge,
		backendName:     backendName,
		healthTracker:   healthTracker,
		messageChannels: make(map[string]messageChannel),
	}
}

// SetupCredentials mounts directory credentials via the provided callback
// and starts the message channel for plugin communication and health tracking.
// owned is true for a shed a service client owns: its message channel still
// carries health heartbeats, but the credential bus is off for it in both
// directions (see plugin.ShedConn.Owned).
//
// Directory mount failures are logged as warnings (non-fatal).
func (cm *CredentialManager) SetupCredentials(ctx context.Context, agent *AgentClient, shedName string, dirCreds map[string]config.MountConfig, mountDir DirMountFunc, owned bool) {
	// Mount directory credentials via backend-specific callback
	if len(dirCreds) > 0 && mountDir != nil {
		backend.Phase(ctx, "credentials")
		backend.Status(ctx, "Mounting directory credentials...")
		for name, mount := range dirCreds {
			if err := mountDir(ctx, agent, name, mount); err != nil {
				log.Printf("Warning: directory credential mount failed for %q: %v", name, err)
				backend.Phase(ctx, "credentials")
				backend.StatusWarning(ctx, fmt.Sprintf("Failed to mount credential %q", name))
			}
		}
	}

	// Start the message channel for plugin messages and health tracking.
	cm.startMessageChannel(shedName, agent, owned)
}

// HealthTracker returns the health tracker for querying VM health state.
func (cm *CredentialManager) HealthTracker() *HealthTracker {
	return cm.healthTracker
}

// StopListener stops the message channel for a VM and unregisters it
// from the plugin bridge.
func (cm *CredentialManager) StopListener(name string) {
	cm.mu.Lock()
	ch, ok := cm.messageChannels[name]
	delete(cm.messageChannels, name)
	cm.mu.Unlock()

	if ok {
		ch.conn.Stop()
	}

	if cm.bridge != nil {
		cm.bridge.UnregisterShed(name)
	}

	if cm.healthTracker != nil {
		cm.healthTracker.Remove(name)
	}
}

// Close stops all message channels.
func (cm *CredentialManager) Close() {
	cm.mu.Lock()
	for name, ch := range cm.messageChannels {
		ch.conn.Stop()
		if cm.bridge != nil {
			cm.bridge.UnregisterShed(name)
		}
		if cm.healthTracker != nil {
			cm.healthTracker.Remove(name)
		}
		delete(cm.messageChannels, name)
	}
	cm.mu.Unlock()
}

// ResumeMessageChannel re-opens the message channel for a VM that is already
// running — the shed-server-restart path (#315). The host dials the guest, so
// nothing re-establishes the channel on its own after a restart: the plugin
// bridge would hold no registration for the shed and `shed list -vv` would
// show no extension health until the shed was manually stopped and started.
//
// It is deliberately message-channel-only: no mounts, no provisioning, no
// hooks. Those all ran when the shed was created/started and the guest still
// has them.
func (cm *CredentialManager) ResumeMessageChannel(name string, agent *AgentClient, owned bool) {
	if cm.startMessageChannel(name, agent, owned) {
		log.Printf("[%s] resumed message channel after server restart", name)
	}
}

// startMessageChannel starts the generalized message channel for a VM.
//
// It returns true when it started a channel, false when one was already
// present for this name. The dedupe lives here so the two entry points
// (SetupCredentials on create/start, ResumeMessageChannel on server startup)
// can't race a second NotifyConn onto the same guest — e.g. a StartShed that
// lands just after the startup resume walk has already resumed the shed.
//
// A channel left behind for the same name with the other owned flag (a shed
// deleted and recreated under its name by another caller, when a teardown
// path missed StopListener) is stopped and replaced, never reused: its
// handler and bridge registration would carry the old shed's credential-bus
// access.
func (cm *CredentialManager) startMessageChannel(name string, agent *AgentClient, owned bool) bool {
	// Health heartbeat callback: update the tracker with agent boot time.
	var healthFn func(env *plugin.Envelope)
	if cm.healthTracker != nil {
		healthFn = func(env *plugin.Envelope) {
			var payload plugin.HeartbeatPayload
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				log.Printf("[%s] Invalid heartbeat payload: %v", name, err)
				return
			}
			if payload.StartedAt.IsZero() {
				log.Printf("[%s] Ignoring heartbeat with zero started_at", name)
				return
			}
			cm.healthTracker.Update(name, payload.StartedAt, payload.Extensions)
		}
	}

	// Extract enabled extensions from server config (nil-safe).
	var enabledExtensions []string
	if cm.serverCfg != nil && cm.serverCfg.Extensions != nil {
		enabledExtensions = cm.serverCfg.Extensions.Enabled
	}

	handler := NewMessageHandler(healthFn, func(env *plugin.Envelope) {
		// An owned shed's plugin messages are dropped here as well as in the
		// bridge: the credential bus is off for it, whatever the bridge's
		// registration state.
		if owned {
			return
		}
		if cm.bridge != nil {
			if err := cm.bridge.PublishToHost(name, env); err != nil {
				log.Printf("[%s] Failed to publish plugin message: %v", name, err)
			}
		}
	}, enabledExtensions)

	// Reserve, register and start under ONE hold of the mutex.
	//
	// Two holds would let two callers both see an empty slot and open two
	// connections to the same guest. Publishing the conn and then starting it
	// outside the lock is worse still: StopListener/Close could take the slot
	// in between and call Stop() on a conn whose Start() has not run yet.
	// NotifyConn.Stop reads nc.cancel (still nil) and then waits on nc.wg, so
	// the interleaving Stop-reads-nil → Start-adds-to-wg → Stop-waits hangs
	// shutdown forever on a connection nobody can cancel; the reverse order
	// leaves a started conn that is in nobody's map and reconnects for the
	// life of the process. #315's startup resume walk runs concurrently with
	// normal serving and with shutdown, which is what made that pre-existing
	// window reachable in practice.
	//
	// Both calls added inside the lock are non-blocking (a map insert under
	// the bridge's own mutex; a goroutine spawn), and the cm.mu → bridge lock
	// order is the one Close() already uses.
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if old, exists := cm.messageChannels[name]; exists {
		if old.owned == owned {
			return false
		}
		log.Printf("[%s] replacing a stale message channel (owned=%v) for owned=%v", name, old.owned, owned)
		old.conn.Stop()
		if cm.bridge != nil {
			cm.bridge.UnregisterShed(name)
		}
		delete(cm.messageChannels, name)
	}
	conn := NewNotifyConn(agent.Dialer(), agent.NotifyPort(), name)
	cm.messageChannels[name] = messageChannel{conn: conn, owned: owned}

	if cm.bridge != nil {
		serverName := ""
		if cm.serverCfg != nil {
			serverName = cm.serverCfg.Name
		}
		cm.bridge.RegisterShed(name, &plugin.ShedConn{
			Name:    name,
			Backend: cm.backendName,
			Server:  serverName,
			Send:    handler.SendPluginMessage,
			Owned:   owned,
		})
	}

	conn.Start(context.Background(), handler)
	return true
}
