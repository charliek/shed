//go:build linux
// +build linux

package firecracker

import (
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

func TestNewCredentialWatcher(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"gh": {
				Source:   "/home/user/.config/gh",
				Target:   "/home/shed/.config/gh",
				ReadOnly: false,
			},
		},
	}

	cw := NewCredentialWatcher(serverCfg)
	if cw.serverCfg != serverCfg {
		t.Error("serverCfg not set correctly")
	}
	if cw.vms == nil {
		t.Error("vms map should be initialized")
	}
	if cw.echoCooldowns == nil {
		t.Error("echoCooldowns map should be initialized")
	}
}

func TestCredentialWatcherRegisterUnregisterVM(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{},
	}
	cw := NewCredentialWatcher(serverCfg)

	vsock := NewVsockClient("/tmp/test.vsock", 1024, 1025, 1026)
	cw.RegisterVM("vm1", vsock)

	cw.mu.RLock()
	if _, ok := cw.vms["vm1"]; !ok {
		t.Error("VM vm1 not registered")
	}
	cw.mu.RUnlock()

	cw.UnregisterVM("vm1")

	cw.mu.RLock()
	if _, ok := cw.vms["vm1"]; ok {
		t.Error("VM vm1 should be unregistered")
	}
	cw.mu.RUnlock()
}

func TestEchoSuppression(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{},
	}
	cw := NewCredentialWatcher(serverCfg)

	// Initially not suppressed
	if cw.isEchoSuppressed("vm1", "gh") {
		t.Error("should not be suppressed before SuppressEcho")
	}

	// Suppress echo
	cw.SuppressEcho("vm1", "gh")

	if !cw.isEchoSuppressed("vm1", "gh") {
		t.Error("should be suppressed after SuppressEcho")
	}

	// Different VM should not be suppressed
	if cw.isEchoSuppressed("vm2", "gh") {
		t.Error("vm2 should not be suppressed")
	}

	// Different credential on same VM should not be suppressed
	if cw.isEchoSuppressed("vm1", "claude") {
		t.Error("claude on vm1 should not be suppressed")
	}
}

func TestEchoSuppressionExpiry(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{},
	}
	cw := NewCredentialWatcher(serverCfg)

	// Set a cooldown that's already expired
	cw.echoMu.Lock()
	cw.echoCooldowns["vm1:gh"] = time.Now().Add(-1 * time.Second)
	cw.echoMu.Unlock()

	// Should not be suppressed since cooldown expired
	if cw.isEchoSuppressed("vm1", "gh") {
		t.Error("should not be suppressed after expiry")
	}

	// Expired entry should be cleaned up
	cw.echoMu.Lock()
	if _, ok := cw.echoCooldowns["vm1:gh"]; ok {
		t.Error("expired entry should have been cleaned up")
	}
	cw.echoMu.Unlock()
}

func TestResolveCredential(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Credentials: map[string]config.MountConfig{
			"gh": {
				Source:   "/home/user/.config/gh",
				Target:   "/home/shed/.config/gh",
				ReadOnly: false,
			},
			"ssh": {
				Source:   "/home/user/.ssh",
				Target:   "/mnt/ssh-host",
				ReadOnly: true,
			},
			"claude": {
				Source:   "/home/user/.claude",
				Target:   "/home/shed/.claude",
				ReadOnly: false,
			},
		},
	}
	cw := NewCredentialWatcher(serverCfg)

	tests := []struct {
		path string
		want string
	}{
		{"/home/user/.config/gh/hosts.yml", "gh"},
		{"/home/user/.claude/.credentials.json", "claude"},
		{"/home/user/.ssh/id_rsa", ""},       // readonly, should not resolve
		{"/home/user/.config/other/file", ""}, // no match
		{"/home/user/.config/gh", "gh"},       // exact match
	}

	for _, tt := range tests {
		got := cw.resolveCredential(tt.path)
		if got != tt.want {
			t.Errorf("resolveCredential(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
