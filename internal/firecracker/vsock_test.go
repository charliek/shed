package firecracker

import (
	"testing"
)

func TestNewVsockClient(t *testing.T) {
	client := NewVsockClient("/tmp/test.vsock", 1024, 1025)

	if client.socketPath != "/tmp/test.vsock" {
		t.Errorf("socketPath = %v, want /tmp/test.vsock", client.socketPath)
	}
	if client.consolePort != 1024 {
		t.Errorf("consolePort = %v, want 1024", client.consolePort)
	}
	if client.healthPort != 1025 {
		t.Errorf("healthPort = %v, want 1025", client.healthPort)
	}
}

func TestNewVsockClient_DifferentValues(t *testing.T) {
	tests := []struct {
		name        string
		socketPath  string
		consolePort uint32
		healthPort  uint32
	}{
		{"default", "/var/lib/shed/sockets/test.vsock", 1024, 1025},
		{"custom path", "/tmp/firecracker/vm1.vsock", 1024, 1025},
		{"different ports", "/run/firecracker.vsock", 2000, 2001},
		{"same ports", "/tmp/test.vsock", 1000, 1000}, // technically valid
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewVsockClient(tt.socketPath, tt.consolePort, tt.healthPort)

			if client.socketPath != tt.socketPath {
				t.Errorf("socketPath = %v, want %v", client.socketPath, tt.socketPath)
			}
			if client.consolePort != tt.consolePort {
				t.Errorf("consolePort = %v, want %v", client.consolePort, tt.consolePort)
			}
			if client.healthPort != tt.healthPort {
				t.Errorf("healthPort = %v, want %v", client.healthPort, tt.healthPort)
			}
		})
	}
}
