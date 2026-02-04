package firecracker

import (
	"testing"
)

func TestNewVsockClient(t *testing.T) {
	client := NewVsockClient(100, 1024, 1025)

	if client.cid != 100 {
		t.Errorf("cid = %v, want 100", client.cid)
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
		cid         uint32
		consolePort uint32
		healthPort  uint32
	}{
		{"default", 100, 1024, 1025},
		{"high CID", 65535, 1024, 1025},
		{"different ports", 200, 2000, 2001},
		{"same ports", 100, 1000, 1000}, // technically valid
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewVsockClient(tt.cid, tt.consolePort, tt.healthPort)

			if client.cid != tt.cid {
				t.Errorf("cid = %v, want %v", client.cid, tt.cid)
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
