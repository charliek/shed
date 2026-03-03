package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

func TestResolveCreateBackend(t *testing.T) {
	info := &config.ServerInfo{
		DefaultBackend:  config.BackendFirecracker,
		EnabledBackends: []string{config.BackendDocker, config.BackendFirecracker, config.BackendVZ},
	}

	t.Run("default backend", func(t *testing.T) {
		backend, warning, err := resolveCreateBackend(info, "", 0, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if backend != config.BackendFirecracker {
			t.Fatalf("backend = %q, want %q", backend, config.BackendFirecracker)
		}
		if warning != "" {
			t.Fatalf("unexpected warning: %s", warning)
		}
	})

	t.Run("requested backend enabled", func(t *testing.T) {
		backend, warning, err := resolveCreateBackend(info, config.BackendDocker, 0, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if backend != config.BackendDocker {
			t.Fatalf("backend = %q, want %q", backend, config.BackendDocker)
		}
		if warning != "" {
			t.Fatalf("unexpected warning: %s", warning)
		}
	})

	t.Run("requested vz backend enabled", func(t *testing.T) {
		backend, warning, err := resolveCreateBackend(info, config.BackendVZ, 0, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if backend != config.BackendVZ {
			t.Fatalf("backend = %q, want %q", backend, config.BackendVZ)
		}
		if warning != "" {
			t.Fatalf("unexpected warning: %s", warning)
		}
	})

	t.Run("requested backend not enabled", func(t *testing.T) {
		info := &config.ServerInfo{
			DefaultBackend:  config.BackendDocker,
			EnabledBackends: []string{config.BackendDocker},
		}
		_, _, err := resolveCreateBackend(info, config.BackendFirecracker, 0, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("warn on docker resources", func(t *testing.T) {
		backend, warning, err := resolveCreateBackend(info, config.BackendDocker, 2, 2048)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if backend != config.BackendDocker {
			t.Fatalf("backend = %q, want %q", backend, config.BackendDocker)
		}
		if !strings.Contains(warning, "ignored") {
			t.Fatalf("expected warning about ignored flags, got %q", warning)
		}
	})

	t.Run("validate firecracker resources", func(t *testing.T) {
		_, _, err := resolveCreateBackend(info, config.BackendFirecracker, 0, 64)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("validate vz resources", func(t *testing.T) {
		_, _, err := resolveCreateBackend(info, config.BackendVZ, config.MaxVZCPUs+1, 0)
		if err == nil {
			t.Fatal("expected cpu upper-bound error")
		}

		_, _, err = resolveCreateBackend(info, config.BackendVZ, 0, 64)
		if err == nil {
			t.Fatal("expected memory lower-bound error")
		}
	})

	t.Run("missing server info", func(t *testing.T) {
		_, _, err := resolveCreateBackend(&config.ServerInfo{}, "", 0, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestFormatUptime(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{"zero minutes", 30 * time.Second, "0m"},
		{"minutes only", 5 * time.Minute, "5m"},
		{"hours and minutes", 3*time.Hour + 12*time.Minute, "3h 12m"},
		{"days and hours", 25*time.Hour + 30*time.Minute, "1d 1h"},
		{"many days", 72 * time.Hour, "3d 0h"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			created := time.Now().Add(-tt.duration)
			got := formatUptime(created)
			if got != tt.want {
				t.Errorf("formatUptime() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShedSSHString(t *testing.T) {
	// Set up clientConfig for tests
	origConfig := clientConfig
	t.Cleanup(func() { clientConfig = origConfig })
	clientConfig = &config.ClientConfig{
		Servers: map[string]config.ServerEntry{
			"devbox": {Host: "devbox.example.com", SSHPort: 2222},
		},
	}

	t.Run("known server", func(t *testing.T) {
		got := shedSSHString("myproj", "devbox")
		want := "myproj@devbox.example.com:2222"
		if got != want {
			t.Errorf("shedSSHString() = %q, want %q", got, want)
		}
	})

	t.Run("unknown server", func(t *testing.T) {
		got := shedSSHString("myproj", "unknown")
		want := "myproj"
		if got != want {
			t.Errorf("shedSSHString() = %q, want %q", got, want)
		}
	})
}

func TestValueOrDash(t *testing.T) {
	if got := valueOrDash(""); got != "-" {
		t.Errorf("valueOrDash(\"\") = %q, want \"-\"", got)
	}
	if got := valueOrDash("hello"); got != "hello" {
		t.Errorf("valueOrDash(\"hello\") = %q, want \"hello\"", got)
	}
}

func TestShedJSONOmitempty(t *testing.T) {
	// Test that empty fields are omitted from JSON
	shed := config.Shed{
		Name:      "test",
		Status:    "running",
		CreatedAt: time.Now(),
	}
	data, err := json.Marshal(shed)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	s := string(data)
	if strings.Contains(s, "ip_address") {
		t.Error("empty ip_address should be omitted from JSON")
	}
	if strings.Contains(s, "cpus") {
		t.Error("zero cpus should be omitted from JSON")
	}
	if strings.Contains(s, "memory_mb") {
		t.Error("zero memory_mb should be omitted from JSON")
	}
	if strings.Contains(s, "pid") {
		t.Error("zero pid should be omitted from JSON")
	}
	if strings.Contains(s, "rootfs_path") {
		t.Error("empty rootfs_path should be omitted from JSON")
	}

	// Test that populated fields appear
	shed.IPAddress = "192.168.1.1"
	shed.CPUs = 4
	shed.MemoryMB = 2048
	shed.PID = 12345
	shed.RootfsPath = "/var/lib/shed/rootfs.ext4"
	data, err = json.Marshal(shed)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	s = string(data)
	if !strings.Contains(s, `"ip_address":"192.168.1.1"`) {
		t.Error("expected ip_address in JSON output")
	}
	if !strings.Contains(s, `"cpus":4`) {
		t.Error("expected cpus in JSON output")
	}
	if !strings.Contains(s, `"memory_mb":2048`) {
		t.Error("expected memory_mb in JSON output")
	}
	if !strings.Contains(s, `"pid":12345`) {
		t.Error("expected pid in JSON output")
	}
	if !strings.Contains(s, `"rootfs_path":"/var/lib/shed/rootfs.ext4"`) {
		t.Error("expected rootfs_path in JSON output")
	}
}
