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
	messageChannels map[string]*NotifyConn // name -> per-VM message channel
}

// NewCredentialManager creates a new CredentialManager.
func NewCredentialManager(serverCfg *config.ServerConfig, bridge *plugin.Bridge, backendName string, healthTracker *HealthTracker) *CredentialManager {
	return &CredentialManager{
		serverCfg:       serverCfg,
		bridge:          bridge,
		backendName:     backendName,
		healthTracker:   healthTracker,
		messageChannels: make(map[string]*NotifyConn),
	}
}

// SetupCredentials mounts directory credentials via the provided callback
// and starts the message channel for plugin communication and health tracking.
//
// Directory mount failures are logged as warnings (non-fatal).
func (cm *CredentialManager) SetupCredentials(ctx context.Context, agent *AgentClient, shedName string, dirCreds map[string]config.MountConfig, mountDir DirMountFunc) {
	// Mount directory credentials via backend-specific callback
	if len(dirCreds) > 0 && mountDir != nil {
		backend.Progress(ctx, "credentials", "Mounting directory credentials...")
		for name, mount := range dirCreds {
			if err := mountDir(ctx, agent, name, mount); err != nil {
				log.Printf("Warning: directory credential mount failed for %q: %v", name, err)
				backend.ProgressWarning(ctx, "credentials", fmt.Sprintf("Failed to mount credential %q", name))
			}
		}
	}

	// Start the message channel for plugin messages and health tracking.
	cm.startMessageChannel(shedName, agent)
}

// HealthTracker returns the health tracker for querying VM health state.
func (cm *CredentialManager) HealthTracker() *HealthTracker {
	return cm.healthTracker
}

// StopListener stops the message channel for a VM and unregisters it
// from the plugin bridge.
func (cm *CredentialManager) StopListener(name string) {
	cm.mu.Lock()
	ch := cm.messageChannels[name]
	delete(cm.messageChannels, name)
	cm.mu.Unlock()

	if ch != nil {
		ch.Stop()
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
		ch.Stop()
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

// startMessageChannel starts the generalized message channel for a VM.
func (cm *CredentialManager) startMessageChannel(name string, agent *AgentClient) {
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
			cm.healthTracker.Update(name, payload.StartedAt)
		}
	}

	handler := NewMessageHandler(healthFn, func(env *plugin.Envelope) {
		if cm.bridge != nil {
			if err := cm.bridge.PublishToHost(name, env); err != nil {
				log.Printf("[%s] Failed to publish plugin message: %v", name, err)
			}
		}
	})

	conn := NewNotifyConn(agent.Dialer(), agent.NotifyPort(), name)

	// Store the conn in the map before registering/starting so that
	// Close()/StopListener() can find and clean it up if called concurrently.
	cm.mu.Lock()
	cm.messageChannels[name] = conn
	cm.mu.Unlock()

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
		})
	}

	conn.Start(context.Background(), handler)
}
