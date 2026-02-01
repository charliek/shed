package provision

import (
	"testing"
)

func TestNewState(t *testing.T) {
	// NewState should work with nil client (for unit testing struct creation)
	state := NewState(nil)
	if state == nil {
		t.Fatal("NewState returned nil")
	}
	if state.docker != nil {
		t.Error("Expected nil docker client")
	}
}

func TestStateKeyConstants(t *testing.T) {
	// Verify state key constants are as expected
	if StateKeyInstallRan != "install_ran" {
		t.Errorf("StateKeyInstallRan = %q, want %q", StateKeyInstallRan, "install_ran")
	}
	if StateKeyError != "error" {
		t.Errorf("StateKeyError = %q, want %q", StateKeyError, "error")
	}
}

func TestStateFilePath(t *testing.T) {
	// Verify state file path constant
	expected := "/var/log/shed/.provision_state"
	if stateFilePath != expected {
		t.Errorf("stateFilePath = %q, want %q", stateFilePath, expected)
	}
}
