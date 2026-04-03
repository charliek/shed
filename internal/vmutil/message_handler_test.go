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
	setup := &plugin.CredentialSetupPayload{
		Credentials: map[string]string{"gh": "/home/shed/.config/gh"},
	}
	handler := NewMessageHandler(setup, nil, nil, nil)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// Read both messages sent by OnConnect: health handshake + credential setup
	done := make(chan struct{})
	go func() {
		defer close(done)

		// First message: system:health handshake
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

		// Second message: system:credentials setup
		msgType, data, err = agentproto.ReadMessage(server)
		if err != nil {
			t.Errorf("ReadMessage (creds): %v", err)
			return
		}
		if msgType != agentproto.MsgTypePluginMessage {
			t.Errorf("creds msgType = 0x%02x, want 0x%02x", msgType, agentproto.MsgTypePluginMessage)
		}
		var credEnv plugin.Envelope
		if err := json.Unmarshal(data, &credEnv); err != nil {
			t.Errorf("unmarshal creds: %v", err)
			return
		}
		if credEnv.Namespace != plugin.NamespaceCredentials {
			t.Errorf("creds namespace = %q, want %q", credEnv.Namespace, plugin.NamespaceCredentials)
		}
		if credEnv.Type != plugin.MessageTypeRequest {
			t.Errorf("creds type = %q, want %q", credEnv.Type, plugin.MessageTypeRequest)
		}
	}()

	if err := handler.OnConnect(client); err != nil {
		t.Fatalf("OnConnect: %v", err)
	}

	<-done
}

func TestMessageHandlerOnConnectNoCreds(t *testing.T) {
	handler := NewMessageHandler(nil, nil, nil, nil)

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// OnConnect always sends a health handshake, even with no credentials
	done := make(chan struct{})
	go func() {
		defer close(done)
		msgType, data, err := agentproto.ReadMessage(server)
		if err != nil {
			t.Errorf("ReadMessage: %v", err)
			return
		}
		if msgType != agentproto.MsgTypePluginMessage {
			t.Errorf("msgType = 0x%02x, want 0x%02x", msgType, agentproto.MsgTypePluginMessage)
		}
		var env plugin.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Errorf("unmarshal: %v", err)
			return
		}
		if env.Namespace != plugin.NamespaceHealth {
			t.Errorf("namespace = %q, want %q", env.Namespace, plugin.NamespaceHealth)
		}
	}()

	if err := handler.OnConnect(client); err != nil {
		t.Fatalf("OnConnect with nil cred setup: %v", err)
	}

	<-done
}

func TestMessageHandlerDispatchCredentialChanged(t *testing.T) {
	var gotCred string
	var gotFiles []string
	handler := NewMessageHandler(nil, func(cred string, files []string) {
		gotCred = cred
		gotFiles = files
	}, nil, nil)

	changed := plugin.CredentialChangedPayload{
		Credential: "gh",
		Files:      []string{"hosts.yml"},
	}
	changedData, _ := json.Marshal(changed)
	env := plugin.NewEnvelope(plugin.NamespaceCredentials, plugin.MessageTypeEvent, changedData)
	data, _ := json.Marshal(env)

	if err := handler.OnMessage(agentproto.MsgTypePluginMessage, data); err != nil {
		t.Fatalf("OnMessage credential changed: %v", err)
	}

	if gotCred != "gh" {
		t.Errorf("credential = %q, want %q", gotCred, "gh")
	}
	if len(gotFiles) != 1 || gotFiles[0] != "hosts.yml" {
		t.Errorf("files = %v, want [hosts.yml]", gotFiles)
	}
}

func TestMessageHandlerDispatchHealthEvent(t *testing.T) {
	var received *plugin.Envelope
	handler := NewMessageHandler(nil, nil, func(env *plugin.Envelope) {
		received = env
	}, nil)

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
	handler := NewMessageHandler(nil, nil, nil, func(env *plugin.Envelope) {
		pluginCalled = true
	})

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
	handler := NewMessageHandler(nil, nil, nil, func(env *plugin.Envelope) {
		received = env
	})

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
	handler := NewMessageHandler(nil, nil, nil, nil)

	// Should not error, just log
	if err := handler.OnMessage(0xFF, []byte("unknown")); err != nil {
		t.Fatalf("OnMessage unknown type: %v", err)
	}
}

func TestMessageHandlerSendPluginMessage(t *testing.T) {
	handler := NewMessageHandler(nil, nil, nil, nil)

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
	handler := NewMessageHandler(nil, nil, nil, nil)

	env := plugin.NewEnvelope("op", plugin.MessageTypeResponse, nil)
	err := handler.SendPluginMessage(env)
	if err == nil {
		t.Fatal("expected error when no connection")
	}
}
