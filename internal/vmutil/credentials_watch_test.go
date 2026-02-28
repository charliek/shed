package vmutil

import (
	"fmt"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

func TestCredentialWatcherRegisterUnregister(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"gh": {Source: "/tmp/gh", Target: "/home/shed/.config/gh", ReadOnly: false},
		},
	}

	cw := NewCredentialWatcher(serverCfg)

	agent := NewAgentClient(nil, 1024, 1025, 1026)
	cw.RegisterVM("vm1", agent)

	cw.mu.RLock()
	_, ok := cw.vms["vm1"]
	cw.mu.RUnlock()
	if !ok {
		t.Error("VM should be registered")
	}

	cw.UnregisterVM("vm1")

	cw.mu.RLock()
	_, ok = cw.vms["vm1"]
	cw.mu.RUnlock()
	if ok {
		t.Error("VM should be unregistered")
	}
}

func TestEchoSuppression(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"gh": {Source: "/tmp/gh", Target: "/home/shed/.config/gh", ReadOnly: false},
		},
	}

	cw := NewCredentialWatcher(serverCfg)

	// Initially not suppressed
	if cw.isEchoSuppressed("vm1", "gh") {
		t.Error("should not be suppressed initially")
	}

	// Suppress echo
	cw.SuppressEcho("vm1", "gh")

	// Should be suppressed now
	if !cw.isEchoSuppressed("vm1", "gh") {
		t.Error("should be suppressed after SuppressEcho")
	}

	// Different VM should not be suppressed
	if cw.isEchoSuppressed("vm2", "gh") {
		t.Error("different VM should not be suppressed")
	}

	// Different credential should not be suppressed
	if cw.isEchoSuppressed("vm1", "ssh") {
		t.Error("different credential should not be suppressed")
	}
}

func TestEchoSuppressionExpiry(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"gh": {Source: "/tmp/gh", Target: "/home/shed/.config/gh", ReadOnly: false},
		},
	}

	cw := NewCredentialWatcher(serverCfg)

	// Set a suppression with an already-expired time
	cw.echoMu.Lock()
	cw.echoCooldowns["vm1:gh"] = time.Now().Add(-1 * time.Second)
	cw.echoMu.Unlock()

	// Should not be suppressed (expired)
	if cw.isEchoSuppressed("vm1", "gh") {
		t.Error("should not be suppressed after expiry")
	}

	// Entry should have been cleaned up
	cw.echoMu.Lock()
	_, exists := cw.echoCooldowns["vm1:gh"]
	cw.echoMu.Unlock()
	if exists {
		t.Error("expired entry should have been deleted")
	}
}

func TestEchoSuppressionPruning(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"gh": {Source: "/tmp/gh", Target: "/home/shed/.config/gh", ReadOnly: false},
		},
	}

	cw := NewCredentialWatcher(serverCfg)

	// Fill with expired entries past the threshold using unique keys
	cw.echoMu.Lock()
	expiredTime := time.Now().Add(-10 * time.Second)
	for i := 0; i < echoPruneThreshold+10; i++ {
		key := fmt.Sprintf("vm%d:cred", i)
		cw.echoCooldowns[key] = expiredTime
	}
	initialCount := len(cw.echoCooldowns)
	cw.echoMu.Unlock()

	if initialCount <= echoPruneThreshold {
		t.Fatalf("expected more than %d entries, got %d", echoPruneThreshold, initialCount)
	}

	// isEchoSuppressed should trigger pruning
	cw.isEchoSuppressed("unused", "unused")

	cw.echoMu.Lock()
	remaining := len(cw.echoCooldowns)
	cw.echoMu.Unlock()

	// All entries were expired, so they should all be pruned
	if remaining > 0 {
		t.Errorf("expected 0 entries after pruning, got %d", remaining)
	}
}

func TestResolveCredential(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"gh":  {Source: "/home/user/.config/gh", Target: "/home/shed/.config/gh", ReadOnly: false},
			"ssh": {Source: "/home/user/.ssh", Target: "/home/shed/.ssh", ReadOnly: true},
		},
	}

	cw := NewCredentialWatcher(serverCfg)

	// Should match gh credential
	name := cw.resolveCredential("/home/user/.config/gh/hosts.yml")
	if name != "gh" {
		t.Errorf("resolveCredential() = %q, want %q", name, "gh")
	}

	// ssh is read-only, should not match
	name = cw.resolveCredential("/home/user/.ssh/id_rsa")
	if name != "" {
		t.Errorf("resolveCredential() for read-only = %q, want empty", name)
	}

	// Unrelated path should not match
	name = cw.resolveCredential("/tmp/random/file")
	if name != "" {
		t.Errorf("resolveCredential() for unrelated path = %q, want empty", name)
	}
}

func TestDebounceSync(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"gh": {Source: "/tmp/gh", Target: "/home/shed/.config/gh", ReadOnly: false},
		},
	}

	cw := NewCredentialWatcher(serverCfg)

	// Just test that debouncing sets pending and creates a timer
	cw.debounceSync("gh")

	cw.debounceMu.Lock()
	pending := cw.pending["gh"]
	hasTimer := cw.timers["gh"] != nil
	// Stop the timer to prevent it from firing during cleanup
	if hasTimer {
		cw.timers["gh"].Stop()
	}
	cw.debounceMu.Unlock()

	if !pending {
		t.Error("expected credential to be pending")
	}
	if !hasTimer {
		t.Error("expected timer to be created")
	}
}
