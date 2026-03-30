package plugin

import (
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
)

// TestEndToEndMessageFlow tests the full message flow:
// Guest publish → vsock frame → bridge → registry → listener channel
// Then response: listener → bridge → vsock → agent.
//
// Uses net.Pipe to simulate vsock without requiring a real VM.
func TestEndToEndMessageFlow(t *testing.T) {
	registry := NewRegistry()
	bridge := NewBridge(registry)

	// Use two separate pipes for the two directions:
	// agentToHost: agent writes, host reads (guest→host messages)
	// hostToAgent: host writes, agent reads (host→guest responses)
	agentToHostR, agentToHostW := net.Pipe()
	hostToAgentR, hostToAgentW := net.Pipe()
	defer agentToHostR.Close()
	defer agentToHostW.Close()
	defer hostToAgentR.Close()
	defer hostToAgentW.Close()

	// Register a shed — Send writes to the host→agent pipe
	bridge.RegisterShed("dev", &ShedConn{
		Name:    "dev",
		Backend: "vz",
		Server:  "mini",
		Send: func(env *Envelope) error {
			data, err := json.Marshal(env)
			if err != nil {
				return err
			}
			return agentproto.WriteMessage(hostToAgentW, agentproto.MsgTypePluginMessage, data)
		},
	})
	defer bridge.UnregisterShed("dev")

	// Register a host-side listener
	listener, err := registry.Register("op")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer registry.Unregister("op")

	// === STEP 1: Agent sends a request via agent→host pipe ===
	requestEnv := NewEnvelope("op", MessageTypeRequest, json.RawMessage(`{"cmd":"read","item":"github"}`))
	requestData, _ := json.Marshal(requestEnv)

	go func() {
		agentproto.WriteMessage(agentToHostW, agentproto.MsgTypePluginMessage, requestData)
	}()

	// === STEP 2: Host reads from agent→host pipe, publishes to registry ===
	msgType, frameData, err := agentproto.ReadMessage(agentToHostR)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if msgType != agentproto.MsgTypePluginMessage {
		t.Fatalf("msgType = 0x%02x, want 0x%02x", msgType, agentproto.MsgTypePluginMessage)
	}

	var receivedEnv Envelope
	if err := json.Unmarshal(frameData, &receivedEnv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if err := bridge.PublishToHost("dev", &receivedEnv); err != nil {
		t.Fatalf("PublishToHost: %v", err)
	}

	// Start reading agent-side responses in background (net.Pipe is synchronous,
	// so we must have a reader before the writer can proceed)
	type agentResponse struct {
		msgType byte
		data    []byte
		err     error
	}
	agentRespCh := make(chan agentResponse, 1)
	go func() {
		mt, d, e := agentproto.ReadMessage(hostToAgentR)
		agentRespCh <- agentResponse{mt, d, e}
	}()

	// === STEP 3: Listener receives the message ===
	select {
	case msg := <-listener.Messages:
		if msg.ID != requestEnv.ID {
			t.Errorf("listener got wrong ID: %q != %q", msg.ID, requestEnv.ID)
		}
		if msg.Namespace != "op" {
			t.Errorf("namespace = %q, want %q", msg.Namespace, "op")
		}
		if msg.Shed == nil || msg.Shed.Name != "dev" {
			t.Errorf("shed info not enriched: %+v", msg.Shed)
		}
		if string(msg.Payload) != `{"cmd":"read","item":"github"}` {
			t.Errorf("payload = %s", msg.Payload)
		}

		// === STEP 4: Listener responds via bridge ===
		resp := NewResponse(msg.ID, "op", json.RawMessage(`{"token":"secret123"}`))
		if err := bridge.SendToShed("dev", resp); err != nil {
			t.Fatalf("SendToShed: %v", err)
		}

	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for message on listener")
	}

	// === STEP 5: Agent reads response from host→agent pipe ===
	select {
	case ar := <-agentRespCh:
		if ar.err != nil {
			t.Fatalf("agent ReadMessage: %v", ar.err)
		}
		if ar.msgType != agentproto.MsgTypePluginMessage {
			t.Fatalf("response msgType = 0x%02x", ar.msgType)
		}

		var respEnv Envelope
		if err := json.Unmarshal(ar.data, &respEnv); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if respEnv.InReplyTo != requestEnv.ID {
			t.Errorf("InReplyTo = %q, want %q", respEnv.InReplyTo, requestEnv.ID)
		}
		if respEnv.Type != MessageTypeResponse {
			t.Errorf("type = %q, want %q", respEnv.Type, MessageTypeResponse)
		}
		if string(respEnv.Payload) != `{"token":"secret123"}` {
			t.Errorf("response payload = %s", respEnv.Payload)
		}

	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for agent response")
	}
}

// TestConcurrentPublishFromMultipleSheds verifies that messages from multiple
// VMs are correctly routed to the right namespace listener.
func TestConcurrentPublishFromMultipleSheds(t *testing.T) {
	registry := NewRegistry()
	bridge := NewBridge(registry)

	listener, _ := registry.Register("op")
	defer registry.Unregister("op")

	shedNames := []string{"dev1", "dev2", "dev3", "dev4", "dev5"}
	for _, name := range shedNames {
		bridge.RegisterShed(name, &ShedConn{
			Name:    name,
			Backend: "vz",
			Server:  "mini",
			Send:    func(env *Envelope) error { return nil },
		})
	}

	var wg sync.WaitGroup
	messagesPerShed := 10

	for _, name := range shedNames {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range messagesPerShed {
				env := NewEnvelope("op", MessageTypeEvent, json.RawMessage(`{}`))
				if err := bridge.PublishToHost(name, env); err != nil {
					t.Errorf("PublishToHost from %s: %v", name, err)
				}
			}
		}()
	}

	// Drain messages
	expected := len(shedNames) * messagesPerShed
	received := 0
	shedCounts := make(map[string]int)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range expected {
			select {
			case msg := <-listener.Messages:
				received++
				if msg.Shed != nil {
					shedCounts[msg.Shed.Name]++
				}
			case <-time.After(5 * time.Second):
				return
			}
		}
	}()

	wg.Wait()
	<-done

	if received != expected {
		t.Errorf("received %d messages, expected %d", received, expected)
	}

	for _, name := range shedNames {
		if shedCounts[name] != messagesPerShed {
			t.Errorf("shed %s: got %d messages, expected %d", name, shedCounts[name], messagesPerShed)
		}
	}
}

// TestSSEDisconnectCleansUp verifies that unregistering a listener releases
// the namespace for re-registration.
func TestSSEDisconnectCleansUp(t *testing.T) {
	registry := NewRegistry()

	_, err := registry.Register("op")
	if err != nil {
		t.Fatalf("first register: %v", err)
	}

	// Duplicate should fail
	if _, err := registry.Register("op"); err == nil {
		t.Fatal("expected duplicate registration to fail")
	}

	// Simulate SSE disconnect
	registry.Unregister("op")

	// Should be available again
	if _, err := registry.Register("op"); err != nil {
		t.Fatalf("re-register after unregister: %v", err)
	}
}
