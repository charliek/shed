package vmutil

import (
	"sync"
	"testing"
	"time"
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
	ht.Update("vm1", bootTime)
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
	ht.Update("vm1", time.Now())

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

	ht.Update("vm1", boot1)
	ht.Update("vm1", boot2)

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
				ht.Update(name, time.Now())
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
