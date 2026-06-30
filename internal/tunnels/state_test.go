package tunnels

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLoadTunnelState(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("file not exists returns empty state", func(t *testing.T) {
		state, err := LoadTunnelStateFromPath(filepath.Join(tmpDir, "nonexistent.json"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state.Tunnels == nil {
			t.Error("Tunnels map should be initialized")
		}
		if len(state.Tunnels) != 0 {
			t.Errorf("expected empty tunnels, got %d", len(state.Tunnels))
		}
	})

	t.Run("valid state", func(t *testing.T) {
		statePath := filepath.Join(tmpDir, "state.json")
		content := `{
  "tunnels": {
    "myproj": {
      "shed_name": "myproj",
      "profile": "dev",
      "pid": 12345,
      "ports": [{"local": 4501, "remote": 4096}],
      "started_at": "2024-01-17T10:30:00Z",
      "server_name": "home"
    }
  }
}`
		if err := os.WriteFile(statePath, []byte(content), 0600); err != nil {
			t.Fatalf("failed to write state: %v", err)
		}

		state, err := LoadTunnelStateFromPath(statePath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		entry, ok := state.GetTunnel("myproj")
		if !ok {
			t.Fatal("expected tunnel entry for myproj")
		}
		if entry.PID != 12345 {
			t.Errorf("PID = %d, want 12345", entry.PID)
		}
		if entry.Profile != "dev" {
			t.Errorf("Profile = %s, want dev", entry.Profile)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		statePath := filepath.Join(tmpDir, "invalid.json")
		if err := os.WriteFile(statePath, []byte("not json"), 0600); err != nil {
			t.Fatalf("failed to write state: %v", err)
		}

		_, err := LoadTunnelStateFromPath(statePath)
		if err == nil {
			t.Error("expected error for invalid JSON")
		}
	})
}

func TestTunnelStateSave(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")

	state := &TunnelState{
		Tunnels: map[string]TunnelEntry{
			"myproj": {
				ShedName:   "myproj",
				Profile:    "default",
				PID:        54321,
				Ports:      []PortMapping{{Local: 3000, Remote: 3000}},
				StartedAt:  time.Now(),
				ServerName: "home",
			},
		},
		path: statePath,
	}

	if err := state.Save(); err != nil {
		t.Fatalf("failed to save state: %v", err)
	}

	// Reload and verify
	loaded, err := LoadTunnelStateFromPath(statePath)
	if err != nil {
		t.Fatalf("failed to reload state: %v", err)
	}

	entry, ok := loaded.GetTunnel("myproj")
	if !ok {
		t.Fatal("expected tunnel entry")
	}
	if entry.PID != 54321 {
		t.Errorf("PID = %d, want 54321", entry.PID)
	}
}

func TestTunnelStateOperations(t *testing.T) {
	state := &TunnelState{
		Tunnels: make(map[string]TunnelEntry),
		path:    filepath.Join(t.TempDir(), "state.json"),
	}

	// Set tunnel
	if err := state.Update(func(tunnels map[string]TunnelEntry) {
		tunnels["proj1"] = TunnelEntry{
			ShedName:  "proj1",
			Profile:   "default",
			PID:       1000,
			Ports:     []PortMapping{{Local: 3000, Remote: 3000}},
			StartedAt: time.Now(),
		}
	}); err != nil {
		t.Fatalf("update (set): %v", err)
	}

	// Get tunnel
	entry, ok := state.GetTunnel("proj1")
	if !ok {
		t.Fatal("expected tunnel entry")
	}
	if entry.PID != 1000 {
		t.Errorf("PID = %d, want 1000", entry.PID)
	}

	// Get nonexistent
	_, ok = state.GetTunnel("nonexistent")
	if ok {
		t.Error("expected false for nonexistent tunnel")
	}

	// Remove tunnel
	if err := state.Update(func(tunnels map[string]TunnelEntry) {
		delete(tunnels, "proj1")
	}); err != nil {
		t.Fatalf("update (delete): %v", err)
	}
	_, ok = state.GetTunnel("proj1")
	if ok {
		t.Error("tunnel should be removed")
	}
}

// TestUpdateNoClobber is the regression guard for the state-clobber bug: a
// handle that loaded the file while it was empty must not erase entries written
// by another handle since then. The old separate-lock Save() overwrote the file
// from a stale in-memory map; Update() reloads inside the lock.
func TestUpdateNoClobber(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "state.json")

	// Two independent handles over the same file, both loaded while empty
	// (simulating two background daemons).
	a, err := LoadTunnelStateFromPath(path)
	if err != nil {
		t.Fatalf("load a: %v", err)
	}
	b, err := LoadTunnelStateFromPath(path)
	if err != nil {
		t.Fatalf("load b: %v", err)
	}

	if err := a.Update(func(tunnels map[string]TunnelEntry) {
		tunnels["a"] = TunnelEntry{ShedName: "a", PID: 111}
	}); err != nil {
		t.Fatalf("update a: %v", err)
	}
	if err := b.Update(func(tunnels map[string]TunnelEntry) {
		tunnels["b"] = TunnelEntry{ShedName: "b", PID: 222}
	}); err != nil {
		t.Fatalf("update b: %v", err)
	}

	loaded, err := LoadTunnelStateFromPath(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := loaded.GetTunnel("a"); !ok {
		t.Error("entry a was clobbered by b's update")
	}
	if _, ok := loaded.GetTunnel("b"); !ok {
		t.Error("entry b missing")
	}
}

// TestUpdateConcurrent hammers Update from many goroutines, each with its own
// handle (its own flock fd, like separate processes), on distinct keys. All
// entries must survive.
func TestUpdateConcurrent(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "state.json")

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := LoadTunnelStateFromPath(path)
			if err != nil {
				errs <- err
				return
			}
			name := fmt.Sprintf("shed-%d", i)
			if err := s.Update(func(tunnels map[string]TunnelEntry) {
				tunnels[name] = TunnelEntry{ShedName: name, PID: 1000 + i}
			}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent update failed: %v", err)
	}

	loaded, err := LoadTunnelStateFromPath(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(loaded.Tunnels) != n {
		t.Fatalf("expected %d tunnels, got %d", n, len(loaded.Tunnels))
	}
}

// TestUpdatePIDGuardedRemoval covers the daemon-shutdown contract: a stale old
// daemon must delete its entry only if it still owns it, so a --replace that
// swapped in a new daemon (different PID) is not undone by the old one's SIGTERM.
func TestUpdatePIDGuardedRemoval(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "state.json")
	s, err := LoadTunnelStateFromPath(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Replacement daemon owns "web" with PID 222.
	if err := s.Update(func(tunnels map[string]TunnelEntry) {
		tunnels["web"] = TunnelEntry{ShedName: "web", PID: 222}
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Old daemon (PID 111) shutting down must NOT delete the replacement's entry.
	const oldPID = 111
	if err := s.Update(func(tunnels map[string]TunnelEntry) {
		if e, ok := tunnels["web"]; ok && e.PID == oldPID {
			delete(tunnels, "web")
		}
	}); err != nil {
		t.Fatalf("old-daemon update: %v", err)
	}
	loaded, err := LoadTunnelStateFromPath(path)
	if err != nil {
		t.Fatalf("reload after non-owner update: %v", err)
	}
	if e, ok := loaded.GetTunnel("web"); !ok || e.PID != 222 {
		t.Fatal("replacement entry should survive removal attempt by stale old daemon")
	}

	// The owning daemon (PID 222) removes its own entry.
	if err := s.Update(func(tunnels map[string]TunnelEntry) {
		if e, ok := tunnels["web"]; ok && e.PID == 222 {
			delete(tunnels, "web")
		}
	}); err != nil {
		t.Fatalf("owner update: %v", err)
	}
	loaded, err = LoadTunnelStateFromPath(path)
	if err != nil {
		t.Fatalf("reload after owner update: %v", err)
	}
	if _, ok := loaded.GetTunnel("web"); ok {
		t.Fatal("owning daemon should have removed its own entry")
	}
}

func TestFindTunnelUsingPort(t *testing.T) {
	state := &TunnelState{
		Tunnels: map[string]TunnelEntry{
			"proj1": {
				ShedName: "proj1",
				Profile:  "default",
				PID:      1000,
				Ports:    []PortMapping{{Local: 3000, Remote: 3000}},
			},
			"proj2": {
				ShedName: "proj2",
				Profile:  "dev",
				PID:      2000,
				Ports: []PortMapping{
					{Local: 4000, Remote: 4000},
					{Local: 5000, Remote: 5000},
				},
			},
		},
	}

	t.Run("find port in first project", func(t *testing.T) {
		name, entry := state.FindTunnelUsingPort(3000)
		if name != "proj1" {
			t.Errorf("name = %s, want proj1", name)
		}
		if entry == nil {
			t.Fatal("expected entry")
		}
	})

	t.Run("find port in second project", func(t *testing.T) {
		name, entry := state.FindTunnelUsingPort(5000)
		if name != "proj2" {
			t.Errorf("name = %s, want proj2", name)
		}
		if entry == nil {
			t.Fatal("expected entry")
		}
	})

	t.Run("port not found", func(t *testing.T) {
		name, entry := state.FindTunnelUsingPort(9999)
		if name != "" {
			t.Errorf("expected empty name, got %s", name)
		}
		if entry != nil {
			t.Error("expected nil entry")
		}
	})
}
