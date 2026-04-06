package vmutil

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/plugin"
)

func TestMessageHandlerOnConnect(t *testing.T) {
	handler := NewMessageHandler(nil, nil, nil)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// OnConnect sends a health handshake
	done := make(chan struct{})
	go func() {
		defer close(done)

		msgType, data, err := agentproto.ReadMessage(server)
		if err != nil {
			t.Errorf("ReadMessage (health): %v", err)
			return
		}
		if msgType != agentproto.MsgTypePluginMessage {
			t.Errorf("health msgType = 0x%02x, want 0x%02x", msgType, agentproto.MsgTypePluginMessage)
		}
		var healthEnv plugin.Envelope
		if err := json.Unmarshal(data, &healthEnv); err != nil {
			t.Errorf("unmarshal health: %v", err)
			return
		}
		if healthEnv.Namespace != plugin.NamespaceHealth {
			t.Errorf("health namespace = %q, want %q", healthEnv.Namespace, plugin.NamespaceHealth)
		}
		if healthEnv.Type != plugin.MessageTypeRequest {
			t.Errorf("health type = %q, want %q", healthEnv.Type, plugin.MessageTypeRequest)
		}
	}()

	if err := handler.OnConnect(client); err != nil {
		t.Fatalf("OnConnect: %v", err)
	}

	<-done
}

func TestMessageHandlerDispatchHealthEvent(t *testing.T) {
	var received *plugin.Envelope
	handler := NewMessageHandler(func(env *plugin.Envelope) {
		received = env
	}, nil, nil)

	payload := plugin.HeartbeatPayload{StartedAt: time.Now()}
	payloadData, _ := json.Marshal(payload)
	env := plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeEvent, payloadData)
	data, _ := json.Marshal(env)

	if err := handler.OnMessage(agentproto.MsgTypePluginMessage, data); err != nil {
		t.Fatalf("OnMessage health event: %v", err)
	}

	if received == nil {
		t.Fatal("expected health callback to be called")
	}
	if received.Namespace != plugin.NamespaceHealth {
		t.Errorf("namespace = %q, want %q", received.Namespace, plugin.NamespaceHealth)
	}
}

func TestMessageHandlerHealthEventNotForwardedToPluginFn(t *testing.T) {
	pluginCalled := false
	handler := NewMessageHandler(nil, func(_ *plugin.Envelope) {
		pluginCalled = true
	}, nil)

	payload := plugin.HeartbeatPayload{StartedAt: time.Now()}
	payloadData, _ := json.Marshal(payload)
	env := plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeEvent, payloadData)
	data, _ := json.Marshal(env)

	if err := handler.OnMessage(agentproto.MsgTypePluginMessage, data); err != nil {
		t.Fatalf("OnMessage health event: %v", err)
	}

	if pluginCalled {
		t.Error("system:health event should not be forwarded to pluginFn")
	}
}

func TestMessageHandlerDispatchPluginMessage(t *testing.T) {
	var received *plugin.Envelope
	handler := NewMessageHandler(nil, func(env *plugin.Envelope) {
		received = env
	}, nil)

	env := plugin.NewEnvelope("op", plugin.MessageTypeRequest, json.RawMessage(`{"cmd":"read"}`))
	data, _ := json.Marshal(env)

	if err := handler.OnMessage(agentproto.MsgTypePluginMessage, data); err != nil {
		t.Fatalf("OnMessage PluginMessage: %v", err)
	}

	if received == nil {
		t.Fatal("expected plugin callback to be called")
	}
	if received.Namespace != "op" {
		t.Errorf("namespace = %q, want %q", received.Namespace, "op")
	}
}

func TestMessageHandlerUnknownTypeLogged(t *testing.T) {
	handler := NewMessageHandler(nil, nil, nil)

	// Should not error, just log
	if err := handler.OnMessage(0xFF, []byte("unknown")); err != nil {
		t.Fatalf("OnMessage unknown type: %v", err)
	}
}

func TestMessageHandlerSendPluginMessage(t *testing.T) {
	handler := NewMessageHandler(nil, nil, nil)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// OnConnect sends a health handshake — drain it in background
	go func() {
		agentproto.ReadMessage(server) // discard health handshake
	}()
	handler.OnConnect(client)

	env := plugin.NewEnvelope("op", plugin.MessageTypeResponse, json.RawMessage(`{"result":"ok"}`))

	// Read the actual plugin message in background
	done := make(chan struct{})
	var readType byte
	var readData []byte
	go func() {
		defer close(done)
		var err error
		readType, readData, err = agentproto.ReadMessage(server)
		if err != nil {
			t.Errorf("ReadMessage: %v", err)
		}
	}()

	if err := handler.SendPluginMessage(env); err != nil {
		t.Fatalf("SendPluginMessage: %v", err)
	}

	<-done

	if readType != agentproto.MsgTypePluginMessage {
		t.Errorf("message type = 0x%02x, want 0x%02x", readType, agentproto.MsgTypePluginMessage)
	}

	var decoded plugin.Envelope
	if err := json.Unmarshal(readData, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.ID != env.ID {
		t.Errorf("ID mismatch: %q != %q", decoded.ID, env.ID)
	}
}

func TestMessageHandlerSendNoConnection(t *testing.T) {
	handler := NewMessageHandler(nil, nil, nil)

	env := plugin.NewEnvelope("op", plugin.MessageTypeResponse, nil)
	err := handler.SendPluginMessage(env)
	if err == nil {
		t.Fatal("expected error when no connection")
	}
}
