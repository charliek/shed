package vmutil

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// DirMountFunc is the backend-specific directory mount callback.
// VZ implements this with mountVirtioFSShare, Firecracker with mount9PInGuest.
// Return error is treated as non-fatal for credential mounts (logged as warning),
// but callers may treat workspace mount errors as fatal.
type DirMountFunc func(ctx context.Context, agent *AgentClient, name string, mount config.MountConfig) error

// CredentialManager handles the credential lifecycle shared by VM backends.
// It owns the host-side credential watcher and per-VM notification listeners.
type CredentialManager struct {
	serverCfg   *config.ServerConfig
	credWatcher *CredentialWatcher // nil if watcher failed to start (non-fatal)

	mu              sync.Mutex
	notifyListeners map[string]*CredentialNotifyListener
}

// NewCredentialManager creates a new CredentialManager and starts the host-side
// credential watcher. If the watcher fails to start, it logs a warning and
// continues with a nil watcher (non-fatal, matching existing backend behavior).
func NewCredentialManager(serverCfg *config.ServerConfig) *CredentialManager {
	cm := &CredentialManager{
		serverCfg:       serverCfg,
		notifyListeners: make(map[string]*CredentialNotifyListener),
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
// transfers file credentials via tar, and starts the notification listener
// for bidirectional sync of writable file credentials.
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

	// Start notification listener for writable file credentials
	if HasWritableCredentials(fileCreds) {
		cm.startNotifyListener(shedName, agent, fileCreds)
	}
}

// StopListener stops the credential notification listener for a VM
// and unregisters it from the host-side watcher.
func (cm *CredentialManager) StopListener(name string) {
	cm.mu.Lock()
	nl := cm.notifyListeners[name]
	delete(cm.notifyListeners, name)
	cm.mu.Unlock()

	if nl != nil {
		nl.Stop()
	}

	if cm.credWatcher != nil {
		cm.credWatcher.UnregisterVM(name)
	}
}

// Close stops all notification listeners and the credential watcher.
func (cm *CredentialManager) Close() {
	cm.mu.Lock()
	for name, nl := range cm.notifyListeners {
		nl.Stop()
		delete(cm.notifyListeners, name)
	}
	cm.mu.Unlock()

	if cm.credWatcher != nil {
		cm.credWatcher.Stop()
	}
}

// startNotifyListener starts a credential notification listener for a VM.
func (cm *CredentialManager) startNotifyListener(name string, agent *AgentClient, fileCreds map[string]config.MountConfig) {
	if len(fileCreds) == 0 {
		return
	}

	listener := NewCredentialNotifyListener(agent, fileCreds, cm.credWatcher)
	listener.Start(context.Background(), name)

	// Register VM with the credential watcher for host->VM pushes
	if cm.credWatcher != nil {
		cm.credWatcher.RegisterVM(name, agent)
	}

	cm.mu.Lock()
	cm.notifyListeners[name] = listener
	cm.mu.Unlock()
}
