package tunnels

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFileLock_LockUnlock(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "test.state")

	fl := NewFileLock(lockPath)

	// Lock should succeed
	if err := fl.Lock(); err != nil {
		t.Fatalf("Lock() failed: %v", err)
	}

	// Lock file should exist
	if _, err := os.Stat(lockPath + ".lock"); os.IsNotExist(err) {
		t.Error("Lock file was not created")
	}

	// Unlock should succeed
	if err := fl.Unlock(); err != nil {
		t.Fatalf("Unlock() failed: %v", err)
	}
}

func TestFileLock_TryLock(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "test.state")

	fl1 := NewFileLock(lockPath)
	fl2 := NewFileLock(lockPath)

	// First lock should succeed
	acquired, err := fl1.TryLock()
	if err != nil {
		t.Fatalf("TryLock() failed: %v", err)
	}
	if !acquired {
		t.Error("TryLock() should have acquired the lock")
	}

	// Second lock should fail (non-blocking)
	acquired, err = fl2.TryLock()
	if err != nil {
		t.Fatalf("TryLock() returned error: %v", err)
	}
	if acquired {
		t.Error("TryLock() should not have acquired the lock while held")
	}

	// Unlock first lock
	if err := fl1.Unlock(); err != nil {
		t.Fatalf("Unlock() failed: %v", err)
	}

	// Now second lock should succeed
	acquired, err = fl2.TryLock()
	if err != nil {
		t.Fatalf("TryLock() failed: %v", err)
	}
	if !acquired {
		t.Error("TryLock() should have acquired the lock after release")
	}

	if err := fl2.Unlock(); err != nil {
		t.Fatalf("Unlock() failed: %v", err)
	}
}

func TestFileLock_ConcurrentAccess(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "test.state")

	var wg sync.WaitGroup
	var mu sync.Mutex
	accessOrder := make([]int, 0)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			fl := NewFileLock(lockPath)
			if err := fl.Lock(); err != nil {
				t.Errorf("Lock() failed for goroutine %d: %v", id, err)
				return
			}

			// Record access order
			mu.Lock()
			accessOrder = append(accessOrder, id)
			mu.Unlock()

			// Simulate some work
			time.Sleep(10 * time.Millisecond)

			if err := fl.Unlock(); err != nil {
				t.Errorf("Unlock() failed for goroutine %d: %v", id, err)
			}
		}(i)
	}

	wg.Wait()

	// All goroutines should have accessed
	if len(accessOrder) != 5 {
		t.Errorf("Expected 5 accesses, got %d", len(accessOrder))
	}
}

func TestFileLock_UnlockWithoutLock(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "test.state")

	fl := NewFileLock(lockPath)

	// Unlock without lock should not error
	if err := fl.Unlock(); err != nil {
		t.Errorf("Unlock() without lock should not error: %v", err)
	}
}

func TestFileLock_CreateDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, "nested", "dir", "test.state")

	fl := NewFileLock(lockPath)

	if err := fl.Lock(); err != nil {
		t.Fatalf("Lock() failed: %v", err)
	}

	// Nested directory should exist
	if _, err := os.Stat(filepath.Dir(lockPath + ".lock")); os.IsNotExist(err) {
		t.Error("Nested directory was not created")
	}

	if err := fl.Unlock(); err != nil {
		t.Fatalf("Unlock() failed: %v", err)
	}
}
