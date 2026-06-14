package plugin

import (
	"sync"
	"testing"
	"time"
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

// newTrackingRegistry returns a registry with ownership tracking on, as a
// server with HTTP auth enforced would configure it.
func newTrackingRegistry() *Registry {
	r := NewRegistry()
	r.EnableOwnershipTracking()
	return r
}

// dispatchRequest registers (if needed) and publishes a request to record a
// pending entry, returning the request ID.
func dispatchRequest(t *testing.T, r *Registry, namespace, shed string) string {
	t.Helper()
	req := NewEnvelope(namespace, MessageTypeRequest, nil)
	req.Shed = &ShedInfo{Name: shed}
	if err := r.Publish(req); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return req.ID
}

func TestConsumeResponseMatchesPending(t *testing.T) {
	r := newTrackingRegistry()
	r.Register("op")
	id := dispatchRequest(t, r, "op", "dev")

	if !r.ConsumeResponse("op", "dev", id, true) {
		t.Error("a response to a dispatched request must be accepted")
	}
	// Consumed on the final response → a replay is rejected.
	if r.ConsumeResponse("op", "dev", id, true) {
		t.Error("a replayed response must be rejected")
	}
}

func TestConsumeResponseRejectsForged(t *testing.T) {
	r := newTrackingRegistry()
	r.Register("op")
	id := dispatchRequest(t, r, "op", "dev")

	if r.ConsumeResponse("op", "dev", "made-up-id", true) {
		t.Error("a forged requestID must be rejected")
	}
	if r.ConsumeResponse("op", "other-shed", id, true) {
		t.Error("a response naming the wrong shed must be rejected")
	}
	if r.ConsumeResponse("other-ns", "dev", id, true) {
		t.Error("a response for the wrong namespace must be rejected")
	}
}

func TestConsumeResponseNonFinalKeepsPending(t *testing.T) {
	r := newTrackingRegistry()
	r.Register("op")
	id := dispatchRequest(t, r, "op", "dev")

	if !r.ConsumeResponse("op", "dev", id, false) {
		t.Error("a non-final response must match")
	}
	if !r.ConsumeResponse("op", "dev", id, true) {
		t.Error("the final response must still match")
	}
	if r.ConsumeResponse("op", "dev", id, true) {
		t.Error("after the final response the request must be consumed")
	}
}

func TestReDeliversPendingOnReconnect(t *testing.T) {
	r := newTrackingRegistry()
	r.Register("op")
	id := dispatchRequest(t, r, "op", "dev")

	// The listener's connection drops. Its un-answered request is RETAINED (not
	// discarded) so a reconnecting listener can still complete the approval.
	r.Unregister("op")

	// A new listener reclaims the namespace (reconnect) and must be re-delivered
	// the still-pending request.
	l2, err := r.Register("op")
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	select {
	case got := <-l2.Messages:
		if got.ID != id {
			t.Errorf("re-delivered request ID = %q, want %q", got.ID, id)
		}
	case <-time.After(time.Second):
		t.Fatal("the pending request was not re-delivered on reconnect")
	}

	// The response is now honored, idempotently: the final response is accepted
	// once, then consumed so a replay is rejected (no duplicate approval).
	if !r.ConsumeResponse("op", "dev", id, true) {
		t.Error("a response to the re-delivered request must be accepted")
	}
	if r.ConsumeResponse("op", "dev", id, true) {
		t.Error("the replayed response must be rejected after consumption")
	}
}

func TestSweepsStaleAbandonedPending(t *testing.T) {
	defer func(old time.Duration) { pendingRetention = old }(pendingRetention)
	pendingRetention = time.Millisecond

	r := newTrackingRegistry()
	r.Register("op")
	id := dispatchRequest(t, r, "op", "dev")
	r.Unregister("op") // retained, but now eligible to be swept once stale

	time.Sleep(20 * time.Millisecond) // exceed the shrunk retention

	// Re-subscribing triggers the opportunistic sweep: the abandoned request is
	// gone, so it is neither re-delivered nor answerable.
	l2, err := r.Register("op")
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	select {
	case <-l2.Messages:
		t.Fatal("a stale (swept) request must not be re-delivered")
	default:
	}
	if r.ConsumeResponse("op", "dev", id, true) {
		t.Error("a swept (abandoned) request must not be answerable")
	}
}

func TestEventsCreateNoPending(t *testing.T) {
	r := newTrackingRegistry()
	r.Register("op")
	ev := NewEnvelope("op", MessageTypeEvent, nil)
	ev.Shed = &ShedInfo{Name: "dev"}
	if err := r.Publish(ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if r.ConsumeResponse("op", "dev", ev.ID, true) {
		t.Error("an event must not create a pending entry")
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
