package plugin

import (
	"encoding/json"
	"errors"
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

// TestBridgeOwnedShedCredentialBusOff: the credential bus is off for a shed a
// service client owns. With a listener registered for the ssh-agent
// namespace, the owned shed's request never reaches it while an unowned
// shed's does; sending to the owned shed fails without calling its Send; and
// the owned shed is not listed to bus consumers.
func TestBridgeOwnedShedCredentialBusOff(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg)
	l, err := reg.Register("ssh-agent")
	if err != nil {
		t.Fatalf("register listener: %v", err)
	}

	ownedSends := 0
	b.RegisterShed("owned", &ShedConn{
		Send:  func(*Envelope) error { ownedSends++; return nil },
		Owned: true,
	})
	b.RegisterShed("mine", &ShedConn{Send: func(*Envelope) error { return nil }})

	req := NewEnvelope("ssh-agent", MessageTypeRequest, json.RawMessage(`{"op":"list"}`))
	if err := b.PublishToHost("owned", req); err != nil {
		t.Fatalf("PublishToHost(owned) = %v, want a silent drop", err)
	}
	select {
	case got := <-l.Messages:
		t.Fatalf("owned shed's ssh-agent request reached the listener: %+v", got)
	default:
	}

	req2 := NewEnvelope("ssh-agent", MessageTypeRequest, json.RawMessage(`{"op":"list"}`))
	if err := b.PublishToHost("mine", req2); err != nil {
		t.Fatalf("PublishToHost(mine): %v", err)
	}
	select {
	case got := <-l.Messages:
		if got.Shed == nil || got.Shed.Name != "mine" {
			t.Fatalf("unowned shed's envelope arrived without its shed info: %+v", got.Shed)
		}
	default:
		t.Fatal("unowned shed's envelope did not reach the listener")
	}

	resp := NewEnvelope("ssh-agent", MessageTypeResponse, nil)
	if err := b.SendToShed("owned", resp); !errors.Is(err, ErrCredentialBusDisabled) {
		t.Fatalf("SendToShed(owned) = %v, want ErrCredentialBusDisabled", err)
	}
	if ownedSends != 0 {
		t.Fatalf("owned shed's Send was called %d times", ownedSends)
	}

	for _, s := range b.ListSheds() {
		if s.Name == "owned" {
			t.Fatalf("owned shed listed to bus consumers: %+v", b.ListSheds())
		}
	}
}
