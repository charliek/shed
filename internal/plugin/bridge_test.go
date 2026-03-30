package plugin

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestBridgeRegisterAndListSheds(t *testing.T) {
	b := NewBridge(NewRegistry())

	sheds := b.ListSheds()
	if len(sheds) != 0 {
		t.Errorf("expected empty list, got %d", len(sheds))
	}

	b.RegisterShed("dev", &ShedConn{
		Name:    "dev",
		Backend: "vz",
		Server:  "mini",
		Send:    func(env *Envelope) error { return nil },
	})

	sheds = b.ListSheds()
	if len(sheds) != 1 {
		t.Fatalf("expected 1 shed, got %d", len(sheds))
	}
	if sheds[0].Name != "dev" || sheds[0].Backend != "vz" || sheds[0].Server != "mini" {
		t.Errorf("unexpected shed info: %+v", sheds[0])
	}
}

func TestBridgeUnregisterShed(t *testing.T) {
	b := NewBridge(NewRegistry())
	b.RegisterShed("dev", &ShedConn{
		Name: "dev",
		Send: func(env *Envelope) error { return nil },
	})

	b.UnregisterShed("dev")

	if len(b.ListSheds()) != 0 {
		t.Error("expected empty list after unregister")
	}
}

func TestBridgePublishToHost(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg)

	l, _ := reg.Register("op")
	b.RegisterShed("dev", &ShedConn{
		Name:    "dev",
		Backend: "vz",
		Server:  "mini",
		Send:    func(env *Envelope) error { return nil },
	})

	env := NewEnvelope("op", MessageTypeRequest, json.RawMessage(`{"cmd":"read"}`))
	if err := b.PublishToHost("dev", env); err != nil {
		t.Fatalf("PublishToHost: %v", err)
	}

	select {
	case got := <-l.Messages:
		if got.ID != env.ID {
			t.Errorf("wrong message ID")
		}
		if got.Shed == nil {
			t.Fatal("expected shed info to be enriched")
		}
		if got.Shed.Name != "dev" || got.Shed.Backend != "vz" || got.Shed.Server != "mini" {
			t.Errorf("unexpected shed info: %+v", got.Shed)
		}
	default:
		t.Fatal("expected message on listener channel")
	}
}

func TestBridgePublishToHostNoListener(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg)
	b.RegisterShed("dev", &ShedConn{Name: "dev", Send: func(env *Envelope) error { return nil }})

	env := NewEnvelope("unregistered", MessageTypeRequest, nil)
	if err := b.PublishToHost("dev", env); err == nil {
		t.Fatal("expected error when no listener for namespace")
	}
}

func TestBridgePublishToHostUnknownShed(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg)
	reg.Register("op")

	// Even with unknown shed, it should still publish (just without shed info enrichment)
	env := NewEnvelope("op", MessageTypeRequest, nil)
	if err := b.PublishToHost("unknown", env); err != nil {
		t.Fatalf("PublishToHost with unknown shed should still deliver: %v", err)
	}
}

func TestBridgeSendToShed(t *testing.T) {
	b := NewBridge(NewRegistry())

	var received *Envelope
	b.RegisterShed("dev", &ShedConn{
		Name: "dev",
		Send: func(env *Envelope) error {
			received = env
			return nil
		},
	})

	env := NewEnvelope("op", MessageTypeResponse, nil)
	if err := b.SendToShed("dev", env); err != nil {
		t.Fatalf("SendToShed: %v", err)
	}
	if received == nil || received.ID != env.ID {
		t.Error("expected envelope to be sent to shed")
	}
}

func TestBridgeSendToShedNotConnected(t *testing.T) {
	b := NewBridge(NewRegistry())

	env := NewEnvelope("op", MessageTypeResponse, nil)
	if err := b.SendToShed("unknown", env); err == nil {
		t.Fatal("expected error for unknown shed")
	}
}

func TestBridgeConcurrentAccess(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg)
	var wg sync.WaitGroup

	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "shed-" + string(rune('a'+i%26))
			b.RegisterShed(name, &ShedConn{
				Name: name,
				Send: func(env *Envelope) error { return nil },
			})
			b.ListSheds()
			b.UnregisterShed(name)
		}()
	}

	wg.Wait()
}
