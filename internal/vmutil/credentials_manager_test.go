package vmutil

import (
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestNewCredentialManager_NilConfig(t *testing.T) {
	cm := NewCredentialManager(nil, nil, "test", nil)
	if cm == nil {
		t.Fatal("NewCredentialManager returned nil")
	}

	if cm.messageChannels == nil {
		t.Error("messageChannels should be initialized, got nil")
	}

	cm.Close()
}

func TestNewCredentialManager_EmptyCredentials(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Mounts: map[string]config.MountConfig{},
	}

	cm := NewCredentialManager(serverCfg, nil, "test", nil)
	if cm == nil {
		t.Fatal("NewCredentialManager returned nil")
	}

	cm.Close()
}

func TestCredentialManager_StopListenerNoOp(t *testing.T) {
	cm := NewCredentialManager(nil, nil, "test", nil)

	cm.StopListener("nonexistent-vm")
	cm.StopListener("nonexistent-vm")
	cm.StopListener("")
	cm.StopListener("another-name")
}

func TestCredentialManager_Close(t *testing.T) {
	cm := NewCredentialManager(nil, nil, "test", nil)
	cm.Close()
	cm.Close()
}

func TestCredentialManager_CloseWithEmptyListeners(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Mounts: map[string]config.MountConfig{},
	}

	cm := NewCredentialManager(serverCfg, nil, "test", nil)

	cm.mu.Lock()
	listenerCount := len(cm.messageChannels)
	cm.mu.Unlock()

	if listenerCount != 0 {
		t.Errorf("expected 0 channels, got %d", listenerCount)
	}

	cm.Close()
}

func TestCredentialManager_StopListenerThenClose(t *testing.T) {
	cm := NewCredentialManager(nil, nil, "test", nil)

	cm.StopListener("vm-1")
	cm.StopListener("vm-2")
	cm.Close()
}
