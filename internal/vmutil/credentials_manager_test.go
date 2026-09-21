package vmutil

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
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

// newResumeTestAgent builds an AgentClient over the in-memory dialer from
// service_test.go, so the message-channel tests below drive the real
// NotifyConn path without a guest on the other end.
func newResumeTestAgent() *AgentClient {
	d, _ := vzDialer("", false)
	return NewAgentClient(d, 1024, 1026)
}

// newResumeTestManager returns a manager wired to a real plugin bridge —
// bridge registration is the observable these tests assert on, because it is
// exactly what a restarted server used to lose (#315).
func newResumeTestManager(t *testing.T) (*CredentialManager, *plugin.Bridge) {
	t.Helper()
	bridge := plugin.NewBridge(plugin.NewRegistry())
	cm := NewCredentialManager(nil, bridge, "test", NewHealthTracker())
	t.Cleanup(cm.Close)
	return cm, bridge
}

func bridgeNames(b *plugin.Bridge) []string {
	infos := b.ListSheds()
	names := make([]string, 0, len(infos))
	for _, i := range infos {
		names = append(names, i.Name)
	}
	sort.Strings(names)
	return names
}

func TestResumeMessageChannel(t *testing.T) {
	t.Run("registers_on_bridge", func(t *testing.T) {
		cm, bridge := newResumeTestManager(t)

		cm.ResumeMessageChannel("shed-a", newResumeTestAgent())

		if got := bridgeNames(bridge); len(got) != 1 || got[0] != "shed-a" {
			t.Fatalf("bridge sheds = %v, want [shed-a]", got)
		}
		if n := cm.channelCount(); n != 1 {
			t.Fatalf("messageChannels = %d, want 1", n)
		}
	})

	// NEGATIVE CONTROL for the case above: the identical fixture with the
	// ResumeMessageChannel call removed must leave the bridge empty. Without
	// it, "registers_on_bridge" could be passing on a registration some other
	// part of the fixture performed.
	t.Run("control_no_resume_leaves_bridge_empty", func(t *testing.T) {
		cm, bridge := newResumeTestManager(t)

		// Deliberately no ResumeMessageChannel call.
		_ = newResumeTestAgent()

		if got := bridgeNames(bridge); len(got) != 0 {
			t.Fatalf("bridge sheds = %v, want empty without a resume", got)
		}
		if n := cm.channelCount(); n != 0 {
			t.Fatalf("messageChannels = %d, want 0 without a resume", n)
		}
	})

	// Idempotence: a StartShed landing after the startup resume walk runs
	// SetupCredentials for a name that already has a channel. It must not
	// open a second NotifyConn onto the same guest.
	t.Run("resume_then_setup_opens_one_channel", func(t *testing.T) {
		cm, bridge := newResumeTestManager(t)

		cm.ResumeMessageChannel("shed-a", newResumeTestAgent())
		first := cm.channel("shed-a")
		if first == nil {
			t.Fatal("resume did not open a channel")
		}

		cm.SetupCredentials(context.Background(), newResumeTestAgent(), "shed-a", nil, nil)

		if n := cm.channelCount(); n != 1 {
			t.Fatalf("messageChannels = %d after resume+setup, want 1", n)
		}
		if got := cm.channel("shed-a"); got != first {
			t.Fatalf("SetupCredentials replaced the resumed channel (%p -> %p)", first, got)
		}
		if got := bridgeNames(bridge); len(got) != 1 || got[0] != "shed-a" {
			t.Fatalf("bridge sheds = %v, want [shed-a]", got)
		}
	})

	// The mirror case: the resume walk racing in after a normal start.
	t.Run("setup_then_resume_opens_one_channel", func(t *testing.T) {
		cm, _ := newResumeTestManager(t)

		cm.SetupCredentials(context.Background(), newResumeTestAgent(), "shed-a", nil, nil)
		first := cm.channel("shed-a")
		if first == nil {
			t.Fatal("SetupCredentials did not open a channel")
		}

		cm.ResumeMessageChannel("shed-a", newResumeTestAgent())

		if n := cm.channelCount(); n != 1 {
			t.Fatalf("messageChannels = %d after setup+resume, want 1", n)
		}
		if got := cm.channel("shed-a"); got != first {
			t.Fatalf("ResumeMessageChannel replaced the live channel (%p -> %p)", first, got)
		}
	})
}

// channelCount and channel are test-only accessors over the manager's
// internal map (same package).
func (cm *CredentialManager) channelCount() int {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return len(cm.messageChannels)
}

func (cm *CredentialManager) channel(name string) *NotifyConn {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.messageChannels[name]
}

// TestStartMessageChannelLifecycleRace hammers the window that #315's startup
// resume walk made reachable: the walk runs concurrently with normal serving
// and with shutdown, so ResumeMessageChannel / SetupCredentials can interleave
// with StopListener and Close on the same name.
//
// Before the fix the conn was published to the map and only THEN registered
// and started, outside the lock. Two bad interleavings lived in that window:
// Stop() reading a nil cancel and then waiting forever on a wait group Start()
// was about to add to (a shutdown hang), or Stop() finishing first and leaving
// a started conn in nobody's map, reconnecting for the life of the process.
//
// Run this with -race. The assertion is that the manager terminates and ends
// consistent with the bridge — a hang here IS the failure.
func TestStartMessageChannelLifecycleRace(t *testing.T) {
	const names = 8
	const rounds = 40

	for r := 0; r < rounds; r++ {
		bridge := plugin.NewBridge(plugin.NewRegistry())
		cm := NewCredentialManager(nil, bridge, "test", NewHealthTracker())

		var wg sync.WaitGroup
		for i := 0; i < names; i++ {
			name := fmt.Sprintf("shed-%d", i)

			wg.Add(3)
			go func() { defer wg.Done(); cm.ResumeMessageChannel(name, newResumeTestAgent()) }()
			go func() {
				defer wg.Done()
				cm.SetupCredentials(context.Background(), newResumeTestAgent(), name, nil, nil)
			}()
			go func() { defer wg.Done(); cm.StopListener(name) }()
		}
		wg.Wait()

		// Close must return. If the pre-fix interleaving is reintroduced this
		// blocks in NotifyConn.Stop's wg.Wait() and the test times out.
		cm.Close()

		// After Close the manager owns nothing, and the bridge agrees: every
		// registration Close saw was unregistered with it.
		if n := cm.channelCount(); n != 0 {
			t.Fatalf("round %d: messageChannels = %d after Close, want 0", r, n)
		}
		for _, got := range bridgeNames(bridge) {
			if ch := cm.channel(got); ch != nil {
				t.Fatalf("round %d: bridge still lists %q with a live channel", r, got)
			}
		}
	}
}
