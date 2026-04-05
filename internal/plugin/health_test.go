package plugin

import (
	"encoding/json"
	"testing"
	"time"
)

func TestHealthRequestPayloadWithExtensions(t *testing.T) {
	payload := HealthRequestPayload{
		Extensions: []string{"ssh-agent", "aws-credentials"},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded HealthRequestPayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(decoded.Extensions) != 2 {
		t.Fatalf("expected 2 extensions, got %d", len(decoded.Extensions))
	}
	if decoded.Extensions[0] != "ssh-agent" {
		t.Errorf("extensions[0] = %q, want %q", decoded.Extensions[0], "ssh-agent")
	}
	if decoded.Extensions[1] != "aws-credentials" {
		t.Errorf("extensions[1] = %q, want %q", decoded.Extensions[1], "aws-credentials")
	}
}

func TestHealthRequestPayloadNilExtensions(t *testing.T) {
	payload := HealthRequestPayload{
		Extensions: nil,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// With omitempty, the extensions field should not appear in the JSON
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["extensions"]; ok {
		t.Error("expected extensions to be omitted when nil")
	}

	// Round-trip should produce nil extensions
	var decoded HealthRequestPayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Extensions != nil {
		t.Errorf("expected nil extensions, got %v", decoded.Extensions)
	}
}

func TestExtensionHealthMarshalRoundTrip(t *testing.T) {
	health := ExtensionHealth{
		Guest: ExtGuestRunning,
		Host:  ExtHostConnected,
	}

	data, err := json.Marshal(health)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ExtensionHealth
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Guest != ExtGuestRunning {
		t.Errorf("Guest = %q, want %q", decoded.Guest, ExtGuestRunning)
	}
	if decoded.Host != ExtHostConnected {
		t.Errorf("Host = %q, want %q", decoded.Host, ExtHostConnected)
	}
}

func TestHealthConstants(t *testing.T) {
	// Verify guest status constants have expected values
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"ExtGuestRunning", ExtGuestRunning, "running"},
		{"ExtGuestStopped", ExtGuestStopped, "stopped"},
		{"ExtGuestFailed", ExtGuestFailed, "failed"},
		{"ExtHostConnected", ExtHostConnected, "connected"},
		{"ExtHostUnreachable", ExtHostUnreachable, "unreachable"},
		{"ExtHostUnknown", ExtHostUnknown, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestNamespaceHealthConstant(t *testing.T) {
	if NamespaceHealth != "system:health" {
		t.Errorf("NamespaceHealth = %q, want %q", NamespaceHealth, "system:health")
	}
}

func TestHeartbeatPayloadMarshalRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	payload := HeartbeatPayload{
		StartedAt: now,
		Extensions: map[string]ExtensionHealth{
			"ssh-agent": {Guest: ExtGuestRunning, Host: ExtHostConnected},
			"aws-creds": {Guest: ExtGuestFailed, Host: ExtHostUnreachable},
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded HeartbeatPayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !decoded.StartedAt.Equal(now) {
		t.Errorf("StartedAt = %v, want %v", decoded.StartedAt, now)
	}
	if len(decoded.Extensions) != 2 {
		t.Fatalf("expected 2 extensions, got %d", len(decoded.Extensions))
	}
	if decoded.Extensions["ssh-agent"].Guest != ExtGuestRunning {
		t.Errorf("ssh-agent guest = %q, want %q", decoded.Extensions["ssh-agent"].Guest, ExtGuestRunning)
	}
	if decoded.Extensions["aws-creds"].Host != ExtHostUnreachable {
		t.Errorf("aws-creds host = %q, want %q", decoded.Extensions["aws-creds"].Host, ExtHostUnreachable)
	}
}
