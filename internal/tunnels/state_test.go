package tunnels

import (
	"os"
	"path/filepath"
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
		Tunnels: make(map[string]TunnelEntry),
		path:    statePath,
	}

	state.SetTunnel("myproj", TunnelEntry{
		ShedName:   "myproj",
		Profile:    "default",
		PID:        54321,
		Ports:      []PortMapping{{Local: 3000, Remote: 3000}},
		StartedAt:  time.Now(),
		ServerName: "home",
	})

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
	}

	// Set tunnel
	state.SetTunnel("proj1", TunnelEntry{
		ShedName:  "proj1",
		Profile:   "default",
		PID:       1000,
		Ports:     []PortMapping{{Local: 3000, Remote: 3000}},
		StartedAt: time.Now(),
	})

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
	state.RemoveTunnel("proj1")
	_, ok = state.GetTunnel("proj1")
	if ok {
		t.Error("tunnel should be removed")
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
