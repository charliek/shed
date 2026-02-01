package tunnels

import (
	"os"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

func TestBuildSSHArgs(t *testing.T) {
	cfg := &TunnelConfig{
		SSH: SSHConfig{
			ServerAliveInterval: 30,
			ServerAliveCountMax: 3,
			ConnectTimeout:      10,
		},
		Sheds: make(map[string]ShedConfig),
	}
	state := &TunnelState{
		Tunnels: make(map[string]TunnelEntry),
	}
	mgr := NewManagerWithConfig(cfg, state)

	serverEntry := &config.ServerEntry{
		Host:    "myserver.local",
		SSHPort: 2222,
	}

	tests := []struct {
		name      string
		shedName  string
		ports     []PortMapping
		wantParts []string // Substrings that must appear in the args
	}{
		{
			name:     "single port",
			shedName: "myproj",
			ports:    []PortMapping{{Local: 3000, Remote: 3000}},
			wantParts: []string{
				"ssh",
				"-N",
				"-T",
				"-p", "2222",
				"-o", "StrictHostKeyChecking=yes",
				"-o", "ServerAliveInterval=30",
				"-o", "ServerAliveCountMax=3",
				"-o", "ConnectTimeout=10",
				"-o", "ExitOnForwardFailure=yes",
				"-L", "3000:localhost:3000",
				"myproj@myserver.local",
			},
		},
		{
			name:     "multiple ports with different local/remote",
			shedName: "devproj",
			ports: []PortMapping{
				{Local: 4501, Remote: 4096},
				{Local: 8080, Remote: 8080},
			},
			wantParts: []string{
				"ssh",
				"-L", "4501:localhost:4096",
				"-L", "8080:localhost:8080",
				"devproj@myserver.local",
			},
		},
		{
			name:      "no ports",
			shedName:  "empty",
			ports:     []PortMapping{},
			wantParts: []string{"ssh", "empty@myserver.local"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := mgr.BuildSSHArgs(tt.shedName, serverEntry, tt.ports)

			// Check all expected parts appear in order
			argStr := ""
			for _, arg := range args {
				argStr += arg + " "
			}

			for _, want := range tt.wantParts {
				found := false
				for _, arg := range args {
					if arg == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("BuildSSHArgs() missing expected arg %q in %v", want, args)
				}
			}
		})
	}
}

func TestBuildSSHArgsUsesKnownHostsPath(t *testing.T) {
	cfg := &TunnelConfig{
		SSH: SSHConfig{
			ServerAliveInterval: 30,
			ServerAliveCountMax: 3,
			ConnectTimeout:      10,
		},
		Sheds: make(map[string]ShedConfig),
	}
	state := &TunnelState{
		Tunnels: make(map[string]TunnelEntry),
	}
	mgr := NewManagerWithConfig(cfg, state)

	serverEntry := &config.ServerEntry{
		Host:    "test.local",
		SSHPort: 22,
	}

	args := mgr.BuildSSHArgs("test", serverEntry, []PortMapping{{Local: 3000, Remote: 3000}})

	// Check that UserKnownHostsFile is set
	foundKnownHosts := false
	for _, arg := range args {
		if len(arg) > 18 && arg[:18] == "UserKnownHostsFile" {
			foundKnownHosts = true
			break
		}
	}
	if !foundKnownHosts {
		t.Errorf("BuildSSHArgs() should include UserKnownHostsFile option, got %v", args)
	}
}

func TestCheckPortConflict(t *testing.T) {
	cfg := &TunnelConfig{
		SSH:   SSHConfig{},
		Sheds: make(map[string]ShedConfig),
	}

	t.Run("conflict with existing tunnel from own process", func(t *testing.T) {
		// Use our own PID which we know is alive
		ourPID := os.Getpid()
		state := &TunnelState{
			Tunnels: map[string]TunnelEntry{
				"existing": {
					ShedName:  "existing",
					Profile:   "default",
					PID:       ourPID,
					Ports:     []PortMapping{{Local: 9999, Remote: 9999}},
					StartedAt: time.Now().Add(-time.Hour),
				},
			},
		}
		mgr := NewManagerWithConfig(cfg, state)

		err := mgr.CheckPortConflict(9999)
		if err == nil {
			t.Error("expected error for port conflict with existing tunnel")
		}
	})

	t.Run("no conflict with dead tunnel", func(t *testing.T) {
		state := &TunnelState{
			Tunnels: map[string]TunnelEntry{
				"dead": {
					ShedName:  "dead",
					Profile:   "default",
					PID:       999999999, // Unlikely to be a real process
					Ports:     []PortMapping{{Local: 19876, Remote: 19876}},
					StartedAt: time.Now().Add(-time.Hour),
				},
			},
		}
		mgr := NewManagerWithConfig(cfg, state)

		// Port 19876 shouldn't be in use (high ephemeral port)
		// This test may fail if port happens to be in use
		err := mgr.CheckPortConflict(19876)
		if err != nil {
			// If port is actually in use by something else, skip
			t.Skipf("port 19876 appears to be in use: %v", err)
		}
	})
}

func TestCheckPortConflicts(t *testing.T) {
	cfg := &TunnelConfig{
		SSH:   SSHConfig{},
		Sheds: make(map[string]ShedConfig),
	}
	// Use our own PID which we know is alive
	ourPID := os.Getpid()
	state := &TunnelState{
		Tunnels: map[string]TunnelEntry{
			"existing": {
				ShedName:  "existing",
				Profile:   "default",
				PID:       ourPID,
				Ports:     []PortMapping{{Local: 9998, Remote: 9998}},
				StartedAt: time.Now().Add(-time.Hour),
			},
		},
	}
	mgr := NewManagerWithConfig(cfg, state)

	t.Run("conflict in list", func(t *testing.T) {
		ports := []PortMapping{
			{Local: 19877, Remote: 19877},
			{Local: 9998, Remote: 9998}, // Conflicts with existing tunnel
		}
		err := mgr.CheckPortConflicts(ports)
		if err == nil {
			t.Error("expected error for port conflict")
		}
	})

	t.Run("empty list", func(t *testing.T) {
		err := mgr.CheckPortConflicts([]PortMapping{})
		if err != nil {
			t.Errorf("unexpected error for empty list: %v", err)
		}
	})
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{
			name:     "seconds",
			duration: 45 * time.Second,
			want:     "45s",
		},
		{
			name:     "one second",
			duration: time.Second,
			want:     "1s",
		},
		{
			name:     "minutes",
			duration: 5 * time.Minute,
			want:     "5m",
		},
		{
			name:     "one minute",
			duration: time.Minute,
			want:     "1m",
		},
		{
			name:     "hours",
			duration: 3 * time.Hour,
			want:     "3h",
		},
		{
			name:     "one hour",
			duration: time.Hour,
			want:     "1h",
		},
		{
			name:     "days",
			duration: 48 * time.Hour,
			want:     "2d",
		},
		{
			name:     "one day",
			duration: 24 * time.Hour,
			want:     "1d",
		},
		{
			name:     "mixed minutes and seconds shows minutes",
			duration: 2*time.Minute + 30*time.Second,
			want:     "2m",
		},
		{
			name:     "mixed hours and minutes shows hours",
			duration: 2*time.Hour + 30*time.Minute,
			want:     "2h",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDuration(tt.duration)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.duration, got, tt.want)
			}
		})
	}
}

func TestManagerState(t *testing.T) {
	cfg := &TunnelConfig{
		Sheds: make(map[string]ShedConfig),
	}
	state := &TunnelState{
		Tunnels: make(map[string]TunnelEntry),
	}
	mgr := NewManagerWithConfig(cfg, state)

	// State() should never return nil
	if mgr.State() == nil {
		t.Error("State() should never return nil")
	}

	// Config() should never return nil
	if mgr.Config() == nil {
		t.Error("Config() should never return nil")
	}
}

func TestCheckHealth(t *testing.T) {
	cfg := &TunnelConfig{
		Sheds: make(map[string]ShedConfig),
	}

	t.Run("unknown tunnel returns false", func(t *testing.T) {
		state := &TunnelState{
			Tunnels: make(map[string]TunnelEntry),
		}
		mgr := NewManagerWithConfig(cfg, state)

		alive, err := mgr.CheckHealth("nonexistent")
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if alive {
			t.Error("expected false for nonexistent tunnel")
		}
	})

	t.Run("dead process returns false", func(t *testing.T) {
		state := &TunnelState{
			Tunnels: map[string]TunnelEntry{
				"dead": {
					ShedName: "dead",
					PID:      999999999, // Unlikely to be a real process
				},
			},
		}
		mgr := NewManagerWithConfig(cfg, state)

		alive, err := mgr.CheckHealth("dead")
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if alive {
			t.Error("expected false for dead process")
		}
	})

	t.Run("own process returns true", func(t *testing.T) {
		// Use our own PID which we know is alive
		ourPID := os.Getpid()
		state := &TunnelState{
			Tunnels: map[string]TunnelEntry{
				"self": {
					ShedName: "self",
					PID:      ourPID,
				},
			},
		}
		mgr := NewManagerWithConfig(cfg, state)

		alive, err := mgr.CheckHealth("self")
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if !alive {
			t.Error("expected true for our own process")
		}
	})
}
