package sdk

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewEnvelope(t *testing.T) {
	payload := json.RawMessage(`{"key":"value"}`)
	env := NewEnvelope("test-ns", MessageTypeRequest, payload)

	if env.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if env.Namespace != "test-ns" {
		t.Errorf("namespace = %q, want %q", env.Namespace, "test-ns")
	}
	if env.Type != MessageTypeRequest {
		t.Errorf("type = %q, want %q", env.Type, MessageTypeRequest)
	}
	if !env.Final {
		t.Error("expected Final to be true by default")
	}
	if env.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
	if env.InReplyTo != "" {
		t.Error("expected empty InReplyTo for new envelope")
	}
	if env.Shed != nil {
		t.Error("expected nil Shed for new envelope")
	}
}

func TestNewResponse(t *testing.T) {
	original := NewEnvelope("test-ns", MessageTypeRequest, nil)
	resp := NewResponse(original.ID, "test-ns", json.RawMessage(`{"result":"ok"}`))

	if resp.InReplyTo != original.ID {
		t.Errorf("InReplyTo = %q, want %q", resp.InReplyTo, original.ID)
	}
	if resp.Type != MessageTypeResponse {
		t.Errorf("type = %q, want %q", resp.Type, MessageTypeResponse)
	}
	if !resp.Final {
		t.Error("expected Final to be true")
	}
	if resp.Namespace != "test-ns" {
		t.Errorf("namespace = %q, want %q", resp.Namespace, "test-ns")
	}
	if resp.ID == original.ID {
		t.Error("response should have its own unique ID")
	}
}

func TestEnvelopeMarshalRoundTrip(t *testing.T) {
	env := NewEnvelope("op", MessageTypeRequest, json.RawMessage(`{"cmd":"read"}`))
	env.Shed = &ShedInfo{Name: "dev", Backend: "vz", Server: "mini"}

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Envelope
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != env.ID {
		t.Errorf("ID mismatch: %q != %q", decoded.ID, env.ID)
	}
	if decoded.Namespace != env.Namespace {
		t.Errorf("Namespace mismatch: %q != %q", decoded.Namespace, env.Namespace)
	}
	if decoded.Type != env.Type {
		t.Errorf("Type mismatch: %q != %q", decoded.Type, env.Type)
	}
	if decoded.Final != env.Final {
		t.Errorf("Final mismatch: %v != %v", decoded.Final, env.Final)
	}
	if decoded.Shed == nil || decoded.Shed.Name != "dev" {
		t.Errorf("Shed mismatch: %+v", decoded.Shed)
	}
	if decoded.Shed.Backend != "vz" {
		t.Errorf("Shed.Backend mismatch: %q != %q", decoded.Shed.Backend, "vz")
	}
	if decoded.Shed.Server != "mini" {
		t.Errorf("Shed.Server mismatch: %q != %q", decoded.Shed.Server, "mini")
	}
	if string(decoded.Payload) != `{"cmd":"read"}` {
		t.Errorf("Payload mismatch: %s", decoded.Payload)
	}
}

func TestEnvelopeUUIDv7Ordering(t *testing.T) {
	env1 := NewEnvelope("ns", MessageTypeEvent, nil)
	time.Sleep(time.Millisecond)
	env2 := NewEnvelope("ns", MessageTypeEvent, nil)

	if env1.ID >= env2.ID {
		t.Errorf("UUIDv7 should be time-ordered: %s >= %s", env1.ID, env2.ID)
	}
}

func TestEnvelopeOmitsEmptyFields(t *testing.T) {
	env := NewEnvelope("ns", MessageTypeEvent, nil)

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := raw["in_reply_to"]; ok {
		t.Error("expected in_reply_to to be omitted when empty")
	}
	if _, ok := raw["shed"]; ok {
		t.Error("expected shed to be omitted when nil")
	}
}
