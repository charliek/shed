package vmutil

import (
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestNewCredentialManager_NilConfig(t *testing.T) {
	// Verify that a nil serverCfg doesn't cause a panic
	cm := NewCredentialManager(nil)
	if cm == nil {
		t.Fatal("NewCredentialManager(nil) returned nil")
	}

	// credWatcher should be nil when serverCfg is nil
	if cm.credWatcher != nil {
		t.Error("credWatcher should be nil when serverCfg is nil")
	}

	// notifyListeners should be initialized (not nil)
	if cm.notifyListeners == nil {
		t.Error("notifyListeners should be initialized, got nil")
	}

	// Close should not panic with nil config
	cm.Close()
}

func TestNewCredentialManager_EmptyCredentials(t *testing.T) {
	// Verify that an empty credentials map works without issues
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{},
	}

	cm := NewCredentialManager(serverCfg)
	if cm == nil {
		t.Fatal("NewCredentialManager() returned nil")
	}

	// credWatcher should be nil when there are no credentials
	if cm.credWatcher != nil {
		t.Error("credWatcher should be nil when credentials map is empty")
	}

	cm.Close()
}

func TestCredentialManager_StopListenerNoOp(t *testing.T) {
	// Stopping a listener for a non-existent name should not panic
	cm := NewCredentialManager(nil)

	// Should be a no-op, not panic
	cm.StopListener("nonexistent-vm")

	// Call multiple times to ensure idempotency
	cm.StopListener("nonexistent-vm")
	cm.StopListener("")
	cm.StopListener("another-name")
}

func TestCredentialManager_Close(t *testing.T) {
	// Close with no listeners should not panic
	cm := NewCredentialManager(nil)
	cm.Close()

	// Double close should also not panic
	cm.Close()
}

func TestCredentialManager_CloseWithEmptyListeners(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{},
	}

	cm := NewCredentialManager(serverCfg)

	// Verify the listener map starts empty
	cm.mu.Lock()
	listenerCount := len(cm.notifyListeners)
	cm.mu.Unlock()

	if listenerCount != 0 {
		t.Errorf("expected 0 listeners, got %d", listenerCount)
	}

	cm.Close()
}

func TestCredentialManager_StopListenerThenClose(t *testing.T) {
	// Verify StopListener followed by Close doesn't double-free
	cm := NewCredentialManager(nil)

	cm.StopListener("vm-1")
	cm.StopListener("vm-2")
	cm.Close()
}
