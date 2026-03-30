package plugin

import (
	"sync"
	"testing"
)

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()

	l, err := r.Register("op")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if l.Namespace != "op" {
		t.Errorf("namespace = %q, want %q", l.Namespace, "op")
	}
	if l.Messages == nil {
		t.Fatal("Messages channel should not be nil")
	}

	got, ok := r.Get("op")
	if !ok {
		t.Fatal("Get should find registered listener")
	}
	if got != l {
		t.Error("Get returned different listener")
	}
}

func TestRegistryDuplicateRejected(t *testing.T) {
	r := NewRegistry()

	if _, err := r.Register("op"); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	_, err := r.Register("op")
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestRegistrySystemNamespaceRejected(t *testing.T) {
	r := NewRegistry()

	_, err := r.Register("system:credentials")
	if err == nil {
		t.Fatal("expected error for system:* namespace")
	}
}

func TestRegistryUnregister(t *testing.T) {
	r := NewRegistry()

	l, _ := r.Register("op")
	r.Unregister("op")

	// Done channel should be closed
	select {
	case <-l.Done:
	default:
		t.Error("Done channel should be closed after Unregister")
	}

	// Namespace should be available again
	if _, ok := r.Get("op"); ok {
		t.Error("listener should be removed after Unregister")
	}

	// Re-register should succeed
	if _, err := r.Register("op"); err != nil {
		t.Fatalf("re-register after Unregister: %v", err)
	}
}

func TestRegistryUnregisterNonexistent(t *testing.T) {
	r := NewRegistry()
	// Should not panic
	r.Unregister("nonexistent")
}

func TestRegistryList(t *testing.T) {
	r := NewRegistry()

	list := r.List()
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d items", len(list))
	}

	r.Register("op")
	r.Register("proxy")

	list = r.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 items, got %d", len(list))
	}

	namespaces := map[string]bool{}
	for _, info := range list {
		namespaces[info.Namespace] = true
	}
	if !namespaces["op"] || !namespaces["proxy"] {
		t.Errorf("unexpected namespaces: %v", list)
	}
}

func TestRegistryPublish(t *testing.T) {
	r := NewRegistry()
	l, _ := r.Register("op")

	env := NewEnvelope("op", MessageTypeRequest, nil)
	if err := r.Publish(env); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-l.Messages:
		if got.ID != env.ID {
			t.Errorf("received wrong message: %q != %q", got.ID, env.ID)
		}
	default:
		t.Fatal("expected message on channel")
	}
}

func TestRegistryPublishNoListener(t *testing.T) {
	r := NewRegistry()
	env := NewEnvelope("unregistered", MessageTypeRequest, nil)

	err := r.Publish(env)
	if err == nil {
		t.Fatal("expected error when no listener registered")
	}
}

func TestRegistryPublishToDisconnectedListener(t *testing.T) {
	r := NewRegistry()
	r.Register("op")
	r.Unregister("op") // simulate disconnect
	// Re-register so there's still a listener but test the Done path
	r.Register("op")

	// Now unregister and try to publish
	r.Unregister("op")
	env := NewEnvelope("op", MessageTypeRequest, nil)
	err := r.Publish(env)
	if err == nil {
		t.Fatal("expected error when no listener registered")
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup

	// Concurrent registers and unregisters
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ns := "ns-" + string(rune('a'+i%26))
			r.Register(ns)
			r.List()
			r.Get(ns)
			r.Unregister(ns)
		}()
	}

	wg.Wait()
}
