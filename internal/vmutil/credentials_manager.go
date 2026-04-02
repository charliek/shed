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
// It owns the host-side credential watcher, per-VM message channels (for both
// plugin messages and credential sync), and the plugin bridge registration.
type CredentialManager struct {
	serverCfg     *config.ServerConfig
	credWatcher   *CredentialWatcher // nil if watcher failed to start (non-fatal)
	bridge        *plugin.Bridge     // plugin message bridge (nil if plugins disabled)
	backendName   string             // "vz" or "firecracker"
	healthTracker *HealthTracker     // tracks per-VM heartbeat state

	mu              sync.Mutex
	messageChannels map[string]*NotifyConn // name -> per-VM message channel
}

// NewCredentialManager creates a new CredentialManager and starts the host-side
// credential watcher. If the watcher fails to start, it logs a warning and
// continues with a nil watcher (non-fatal, matching existing backend behavior).
func NewCredentialManager(serverCfg *config.ServerConfig, bridge *plugin.Bridge, backendName string, healthTracker *HealthTracker) *CredentialManager {
	cm := &CredentialManager{
		serverCfg:       serverCfg,
		bridge:          bridge,
		backendName:     backendName,
		healthTracker:   healthTracker,
		messageChannels: make(map[string]*NotifyConn),
	}

	if serverCfg != nil && len(serverCfg.Credentials) > 0 {
		cm.credWatcher = NewCredentialWatcher(serverCfg)
		if err := cm.credWatcher.Start(context.Background()); err != nil {
			log.Printf("Warning: failed to start credential watcher: %v", err)
			cm.credWatcher = nil
		}
	}

	return cm
}

// SetupCredentials mounts directory credentials via the provided callback,
// transfers file credentials via tar, and starts the message channel for
// plugin communication and bidirectional credential sync.
//
// Directory mount failures are logged as warnings (non-fatal).
// File transfer failures are logged as warnings (non-fatal).
func (cm *CredentialManager) SetupCredentials(ctx context.Context, agent *AgentClient, shedName string, dirCreds, fileCreds map[string]config.MountConfig, mountDir DirMountFunc) {
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

	// Transfer file credentials via tar
	if len(fileCreds) > 0 {
		backend.Progress(ctx, "credentials", "Transferring file credentials...")
		credTransfer := NewCredentialTransfer(agent, cm.serverCfg)
		for name, mount := range fileCreds {
			if err := credTransfer.TransferCredential(ctx, name, mount); err != nil {
				log.Printf("Warning: failed to transfer credential %q: %v", name, err)
			}
		}
	}

	// Always start the message channel — it handles both plugin messages
	// and credential sync via plugin envelopes.
	cm.startMessageChannel(shedName, agent, fileCreds)
}

// HealthTracker returns the health tracker for querying VM health state.
func (cm *CredentialManager) HealthTracker() *HealthTracker {
	return cm.healthTracker
}

// StopListener stops the message channel for a VM and unregisters it
// from the plugin bridge and host-side credential watcher.
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

	if cm.credWatcher != nil {
		cm.credWatcher.UnregisterVM(name)
	}

	if cm.healthTracker != nil {
		cm.healthTracker.Remove(name)
	}
}

// Close stops all message channels and the credential watcher.
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

	if cm.credWatcher != nil {
		cm.credWatcher.Stop()
	}
}

// startMessageChannel starts the generalized message channel for a VM.
func (cm *CredentialManager) startMessageChannel(name string, agent *AgentClient, fileCreds map[string]config.MountConfig) {
	var credSetup *plugin.CredentialSetupPayload
	var credChangeFn func(string, []string)

	if HasWritableCredentials(fileCreds) {
		creds := make(map[string]string)
		excludes := make(map[string][]string)
		writableCreds := make(map[string]config.MountConfig)
		for credName, mount := range fileCreds {
			if !mount.ReadOnly {
				writableCreds[credName] = mount
				creds[credName] = mount.Target
				if len(mount.Exclude) > 0 {
					excludes[credName] = mount.Exclude
				}
			}
		}
		credSetup = &plugin.CredentialSetupPayload{
			Credentials: creds,
			Excludes:    excludes,
		}

		credNL := NewCredentialNotifyListener(agent, writableCreds, cm.credWatcher)
		credNL.SetName(name)
		credChangeFn = func(credName string, files []string) {
			if err := credNL.PullChangedFiles(credName, files); err != nil {
				log.Printf("[%s] Failed to pull credential changes for %s: %v", name, credName, err)
			}
		}
	}

	// Health heartbeat callback: update the tracker with agent boot time.
	var healthFn func(env *plugin.Envelope)
	if cm.healthTracker != nil {
		healthFn = func(env *plugin.Envelope) {
			var payload plugin.HeartbeatPayload
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				log.Printf("[%s] Invalid heartbeat payload: %v", name, err)
				return
			}
			cm.healthTracker.Update(name, payload.StartedAt)
		}
	}

	handler := NewMessageHandler(credSetup, credChangeFn, healthFn, func(env *plugin.Envelope) {
		if cm.bridge != nil {
			if err := cm.bridge.PublishToHost(name, env); err != nil {
				log.Printf("[%s] Failed to publish plugin message: %v", name, err)
			}
		}
	})

	conn := NewNotifyConn(agent.Dialer(), agent.NotifyPort(), name)

	if cm.credWatcher != nil && HasWritableCredentials(fileCreds) {
		cm.credWatcher.RegisterVM(name, agent)
	}

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
