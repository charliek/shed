package vmutil

import (
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/plugin"
)

func TestHealthTrackerUpdateAndGet(t *testing.T) {
	ht := NewHealthTracker()
	bootTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Get for unknown VM returns false
	_, ok := ht.Get("unknown")
	if ok {
		t.Error("Get() for unknown VM should return false")
	}

	// Update and Get
	before := time.Now()
	ht.Update("vm1", bootTime, nil)
	after := time.Now()

	status, ok := ht.Get("vm1")
	if !ok {
		t.Fatal("Get() after Update should return true")
	}
	if status.AgentStartedAt != bootTime {
		t.Errorf("AgentStartedAt = %v, want %v", status.AgentStartedAt, bootTime)
	}
	// LastSeen should be host time (between before and after)
	if status.LastSeen.Before(before) || status.LastSeen.After(after) {
		t.Errorf("LastSeen = %v, want between %v and %v", status.LastSeen, before, after)
	}
}

func TestHealthTrackerRemove(t *testing.T) {
	ht := NewHealthTracker()
	ht.Update("vm1", time.Now(), nil)

	ht.Remove("vm1")

	_, ok := ht.Get("vm1")
	if ok {
		t.Error("Get() after Remove should return false")
	}

	// Remove of unknown VM is a no-op
	ht.Remove("nonexistent")
}

func TestHealthTrackerUpdateOverwrites(t *testing.T) {
	ht := NewHealthTracker()
	boot1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	boot2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	ht.Update("vm1", boot1, nil)
	ht.Update("vm1", boot2, nil)

	status, _ := ht.Get("vm1")
	if status.AgentStartedAt != boot2 {
		t.Errorf("AgentStartedAt = %v, want %v (latest update)", status.AgentStartedAt, boot2)
	}
}

func TestHealthTrackerConcurrentAccess(t *testing.T) {
	ht := NewHealthTracker()
	var wg sync.WaitGroup

	// Concurrent updates and reads
	for i := range 10 {
		wg.Add(2)
		name := "vm" + string(rune('0'+i))
		go func() {
			defer wg.Done()
			for range 100 {
				ht.Update(name, time.Now(), nil)
			}
		}()
		go func() {
			defer wg.Done()
			for range 100 {
				ht.Get(name)
			}
		}()
	}
	wg.Wait()
}

func TestHealthTrackerExtensions(t *testing.T) {
	ht := NewHealthTracker()
	bootTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	ext := map[string]plugin.ExtensionHealth{
		"ssh-agent": {Guest: plugin.ExtGuestRunning, Host: plugin.ExtHostConnected},
		"aws-creds": {Guest: plugin.ExtGuestRunning, Host: plugin.ExtHostUnreachable},
	}

	ht.Update("vm1", bootTime, ext)

	status, ok := ht.Get("vm1")
	if !ok {
		t.Fatal("Get() after Update should return true")
	}
	if len(status.Extensions) != 2 {
		t.Fatalf("Extensions count = %d, want 2", len(status.Extensions))
	}
	if status.Extensions["ssh-agent"].Guest != plugin.ExtGuestRunning {
		t.Errorf("ssh-agent guest = %q, want %q", status.Extensions["ssh-agent"].Guest, plugin.ExtGuestRunning)
	}
	if status.Extensions["aws-creds"].Host != plugin.ExtHostUnreachable {
		t.Errorf("aws-creds host = %q, want %q", status.Extensions["aws-creds"].Host, plugin.ExtHostUnreachable)
	}
}

func TestHealthTrackerExtensionsDefensiveCopy(t *testing.T) {
	ht := NewHealthTracker()
	bootTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	ext := map[string]plugin.ExtensionHealth{
		"ssh-agent": {Guest: plugin.ExtGuestRunning, Host: plugin.ExtHostConnected},
	}

	ht.Update("vm1", bootTime, ext)

	// Mutate the original map — should not affect stored state
	ext["ssh-agent"] = plugin.ExtensionHealth{Guest: plugin.ExtGuestFailed, Host: plugin.ExtHostUnreachable}

	status, _ := ht.Get("vm1")
	if status.Extensions["ssh-agent"].Guest != plugin.ExtGuestRunning {
		t.Error("stored state was mutated via original map — defensive copy failed on Update")
	}

	// Mutate the returned map — should not affect stored state
	status.Extensions["ssh-agent"] = plugin.ExtensionHealth{Guest: plugin.ExtGuestStopped}

	status2, _ := ht.Get("vm1")
	if status2.Extensions["ssh-agent"].Guest != plugin.ExtGuestRunning {
		t.Error("stored state was mutated via returned map — defensive copy failed on Get")
	}
}

func TestHealthTrackerNilExtensions(t *testing.T) {
	ht := NewHealthTracker()
	ht.Update("vm1", time.Now(), nil)

	status, ok := ht.Get("vm1")
	if !ok {
		t.Fatal("Get() should return true")
	}
	if status.Extensions != nil {
		t.Errorf("Extensions should be nil when none provided, got %v", status.Extensions)
	}
}
