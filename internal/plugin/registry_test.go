package plugin

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// k is the implicit user's listener key for namespace: the only kind a
// server with no user list has.
func k(namespace string) ListenerKey { return ListenerKey{Namespace: namespace} }

// uk is user's listener key for namespace.
func uk(user, namespace string) ListenerKey { return ListenerKey{User: user, Namespace: namespace} }

// unregister removes the listener currently under key, whatever its token
// (a test standing in for its handler's deferred Unregister).
func unregister(t *testing.T, r *Registry, key ListenerKey) {
	t.Helper()
	if l, ok := r.Get(key); ok {
		r.Unregister(key, l.Token)
	}
}

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()

	l, err := r.Register(k("op"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if l.Namespace != "op" {
		t.Errorf("namespace = %q, want %q", l.Namespace, "op")
	}
	if l.Messages == nil {
		t.Fatal("Messages channel should not be nil")
	}

	got, ok := r.Get(k("op"))
	if !ok {
		t.Fatal("Get should find registered listener")
	}
	if got != l {
		t.Error("Get returned different listener")
	}
}

func TestRegistryDuplicateRejected(t *testing.T) {
	r := NewRegistry()

	if _, err := r.Register(k("op")); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	_, err := r.Register(k("op"))
	if err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestRegistrySystemNamespaceRejected(t *testing.T) {
	r := NewRegistry()

	_, err := r.Register(k("system:credentials"))
	if err == nil {
		t.Fatal("expected error for system:* namespace")
	}
}

func TestRegistryUnregister(t *testing.T) {
	r := NewRegistry()

	l, _ := r.Register(k("op"))
	unregister(t, r, k("op"))

	// Done channel should be closed
	select {
	case <-l.Done:
	default:
		t.Error("Done channel should be closed after Unregister")
	}

	// Namespace should be available again
	if _, ok := r.Get(k("op")); ok {
		t.Error("listener should be removed after Unregister")
	}

	// Re-register should succeed
	if _, err := r.Register(k("op")); err != nil {
		t.Fatalf("re-register after Unregister: %v", err)
	}
}

func TestRegistryUnregisterNonexistent(t *testing.T) {
	r := NewRegistry()
	// Should not panic
	if r.Unregister(k("nonexistent"), 1) {
		t.Error("Unregister of nothing reported a removal")
	}
}

func TestRegistryList(t *testing.T) {
	r := NewRegistry()

	list := r.List("", true)
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d items", len(list))
	}

	r.Register(k("op"))
	r.Register(k("proxy"))

	list = r.List("", true)
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
	l, _ := r.Register(k("op"))

	env := NewEnvelope("op", MessageTypeRequest, nil)
	if err := r.Publish(context.Background(), "", 0, env); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if got := recv(t, l); got.ID != env.ID {
		t.Errorf("received wrong message: %q != %q", got.ID, env.ID)
	}
}

// newTrackingRegistry returns a registry. Pending requests are tracked in
// every mode now, so it is a plain one.
func newTrackingRegistry() *Registry { return NewRegistry() }

// answered reports whether an answer from user, carrying id (the server's ID
// for a request) for namespace and shed, matches a pending request, and
// consumes the request when final: what a delivered response does, without a
// bridge or a shed to write into.
func answered(r *Registry, user, namespace, shed, id string, final bool) bool {
	m := r.match(user, namespace, shed, id)
	if m == nil {
		return false
	}
	if final {
		r.free(m)
	}
	return true
}

// consumed is answered for the implicit user.
func consumed(r *Registry, namespace, shed, id string, final bool) bool {
	return answered(r, "", namespace, shed, id, final)
}

// dispatchRequest publishes a request to record a pending entry, returning
// the ID the server gave it (the one its listener is sent).
func dispatchRequest(t *testing.T, r *Registry, namespace, shed string) string {
	t.Helper()
	req := NewEnvelope(namespace, MessageTypeRequest, nil)
	req.Shed = &ShedInfo{Name: shed}
	guestID := req.ID
	if err := r.Publish(context.Background(), "", 0, req); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if req.ID == guestID || req.ID == "" {
		t.Fatalf("the request kept the guest's ID %q", guestID)
	}
	return req.ID
}

func TestAnswerMatchesPending(t *testing.T) {
	r := newTrackingRegistry()
	r.Register(k("op"))
	id := dispatchRequest(t, r, "op", "dev")

	if !consumed(r, "op", "dev", id, true) {
		t.Error("a response to a dispatched request must be accepted")
	}
	// Consumed on the final response → a replay is rejected.
	if consumed(r, "op", "dev", id, true) {
		t.Error("a replayed response must be rejected")
	}
}

func TestAnswerRejectsForged(t *testing.T) {
	r := newTrackingRegistry()
	r.Register(k("op"))
	id := dispatchRequest(t, r, "op", "dev")

	if consumed(r, "op", "dev", "made-up-id", true) {
		t.Error("a forged requestID must be rejected")
	}
	if consumed(r, "op", "other-shed", id, true) {
		t.Error("a response naming the wrong shed must be rejected")
	}
	if consumed(r, "other-ns", "dev", id, true) {
		t.Error("a response for the wrong namespace must be rejected")
	}
}

func TestAnswerNonFinalKeepsPending(t *testing.T) {
	r := newTrackingRegistry()
	r.Register(k("op"))
	id := dispatchRequest(t, r, "op", "dev")

	if !consumed(r, "op", "dev", id, false) {
		t.Error("a non-final response must match")
	}
	if !consumed(r, "op", "dev", id, true) {
		t.Error("the final response must still match")
	}
	if consumed(r, "op", "dev", id, true) {
		t.Error("after the final response the request must be consumed")
	}
}

func TestReDeliversPendingOnReconnect(t *testing.T) {
	r := newTrackingRegistry()
	r.Register(k("op"))
	id := dispatchRequest(t, r, "op", "dev")

	// The listener's connection drops. Its un-answered request is RETAINED (not
	// discarded) so a reconnecting listener can still complete the approval.
	unregister(t, r, k("op"))

	// A new listener reclaims the namespace (reconnect) and must be re-delivered
	// the still-pending request.
	l2, err := r.Register(k("op"))
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if got := recv(t, l2); got.ID != id {
		t.Errorf("re-delivered request ID = %q, want %q", got.ID, id)
	}

	// The response is now honored, idempotently: the final response is accepted
	// once, then consumed so a replay is rejected (no duplicate approval).
	if !consumed(r, "op", "dev", id, true) {
		t.Error("a response to the re-delivered request must be accepted")
	}
	if consumed(r, "op", "dev", id, true) {
		t.Error("the replayed response must be rejected after consumption")
	}
}

func TestSweepsStaleAbandonedPending(t *testing.T) {
	r := newTrackingRegistry()
	r.Register(k("op"))
	id := dispatchRequest(t, r, "op", "dev")
	unregister(t, r, k("op")) // retained, but now eligible to be swept once stale

	// Every request is past its retention from here (no wait needed).
	setVar(t, &pendingRetention, -time.Second)

	// Re-subscribing triggers the opportunistic sweep: the abandoned request is
	// gone, so it is neither re-delivered nor answerable.
	setVar(t, &sweepEvery, 0)
	l2, n := watchRedelivery(t).register(t, r, k("op"))
	if n != 0 {
		t.Fatalf("a stale (swept) request is re-delivered (%d)", n)
	}
	noMessage(t, l2)
	if consumed(r, "op", "dev", id, true) {
		t.Error("a swept (abandoned) request must not be answerable")
	}
}

// TestReDeliversLargeBacklogWithoutWedging guards the wedge fix: a retained
// backlog larger than the listener's channel buffer (32) must not block Register
// (which would hang the HTTP handler and wedge the namespace at 409 on every
// reconnect). Register must return promptly and every request must still be
// re-delivered.
func TestReDeliversLargeBacklogWithoutWedging(t *testing.T) {
	r := newTrackingRegistry()
	l1, _ := r.Register(k("op"))

	const n = 50 // > the 32-element listener buffer
	ids := make(map[string]bool, n)
	for range n {
		ids[dispatchRequest(t, r, "op", "dev")] = true
		recv(t, l1) // drain so Publish's own send doesn't block on the buffer
	}
	unregister(t, r, k("op"))

	// Reconnect in a goroutine so a (buggy) blocking Register surfaces as a clean
	// timeout failure rather than hanging the whole test.
	type regResult struct {
		l   *Listener
		err error
	}
	resCh := make(chan regResult, 1)
	go func() {
		l2, err := r.Register(k("op"))
		resCh <- regResult{l2, err}
	}()
	var res regResult
	select {
	case res = <-resCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Register blocked on a backlog larger than the channel buffer (wedge)")
	}
	if res.err != nil {
		t.Fatalf("re-register: %v", res.err)
	}

	// Every retained request is eventually re-delivered, none lost.
	got := make(map[string]bool, n)
	for len(got) < n {
		got[recv(t, res.l).ID] = true
	}
	for id := range ids {
		if !got[id] {
			t.Errorf("request %s was not re-delivered", id)
		}
	}
}

func TestEventsCreateNoPending(t *testing.T) {
	r := newTrackingRegistry()
	r.Register(k("op"))
	ev := NewEnvelope("op", MessageTypeEvent, nil)
	ev.Shed = &ShedInfo{Name: "dev"}
	if err := r.Publish(context.Background(), "", 0, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if consumed(r, "op", "dev", ev.ID, true) {
		t.Error("an event must not create a pending entry")
	}
}

func TestRegistryPublishNoListener(t *testing.T) {
	r := NewRegistry()
	env := NewEnvelope("unregistered", MessageTypeRequest, nil)

	err := r.Publish(context.Background(), "", 0, env)
	if err == nil {
		t.Fatal("expected error when no listener registered")
	}
}

// TestRegistryPublishAfterListenerUnregistered: once a listener has been
// unregistered (its stream dropped), a publish for its key finds no listener.
// A publish already waiting when the listener disconnects is
// TestRegistryPublishBlockedThenListenerDisconnects.
func TestRegistryPublishAfterListenerUnregistered(t *testing.T) {
	r := NewRegistry()
	r.Register(k("op"))
	unregister(t, r, k("op"))
	env := NewEnvelope("op", MessageTypeRequest, nil)
	err := r.Publish(context.Background(), "", 0, env)
	if err == nil || !strings.Contains(err.Error(), "no listener registered") {
		t.Fatalf("Publish = %v, want no listener registered", err)
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
			if l, err := r.Register(k(ns)); err == nil {
				r.List("", true)
				r.Get(k(ns))
				r.Unregister(k(ns), l.Token)
			}
		}()
	}

	wg.Wait()
}

// publishFrom publishes a request from shed for user, as the bridge would
// for a shed of that user's on connection gen, and returns the server's ID
// for it.
func publishFrom(t *testing.T, r *Registry, user string, gen uint64, namespace, shed string) string {
	t.Helper()
	req := NewEnvelope(namespace, MessageTypeRequest, nil)
	req.Shed = &ShedInfo{Name: shed}
	if err := r.Publish(context.Background(), user, gen, req); err != nil {
		t.Fatalf("Publish(%q): %v", user, err)
	}
	return req.ID
}

// recv returns the next envelope l's stream would send: the next queued
// message that Take lets through. It fails if there is none (the guard is
// against a hang; every caller's message is queued before it looks).
func recv(t *testing.T, l *Listener) *Envelope {
	t.Helper()
	for {
		select {
		case m := <-l.Messages:
			if env := l.Take(m); env != nil {
				return env
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("listener %q/%q received nothing", l.User, l.Namespace)
			return nil
		}
	}
}

// noMessage fails if l's queue holds a message its stream would send.
// References to messages dropped since they were queued do not count.
func noMessage(t *testing.T, l *Listener) {
	t.Helper()
	for {
		select {
		case m := <-l.Messages:
			if env := l.Take(m); env != nil {
				t.Fatalf("listener %q/%q received %s from %+v", l.User, l.Namespace, env.ID, env.Shed)
			}
		default:
			return
		}
	}
}

// TestTwoUsersHoldTheSameNamespace: two users' machines hold the same
// namespace at once; each is sent only its own user's requests, neither
// inherits the other's on reconnect, and neither can answer the other's.
func TestTwoUsersHoldTheSameNamespace(t *testing.T) {
	watch := watchRedelivery(t)
	r := NewRegistry()
	alice, err := r.Register(uk("alice", "ssh-agent"))
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	bob, err := r.Register(uk("bob", "ssh-agent"))
	if err != nil {
		t.Fatalf("bob registering the namespace alice holds: %v", err)
	}
	// One user still holds a namespace once.
	if _, err := r.Register(uk("alice", "ssh-agent")); err == nil {
		t.Fatal("a second listener for alice's namespace was accepted")
	} else if err.Error() != `namespace "ssh-agent" is already registered` {
		t.Fatalf("duplicate error = %q, want the existing text", err)
	}

	aliceReq := publishFrom(t, r, "alice", 1, "ssh-agent", "box")
	bobReq := publishFrom(t, r, "bob", 2, "ssh-agent", "bobs")
	if got := recv(t, alice); got.ID != aliceReq {
		t.Fatalf("alice received %s, want her own %s", got.ID, aliceReq)
	}
	noMessage(t, alice)
	if got := recv(t, bob); got.ID != bobReq {
		t.Fatalf("bob received %s, want his own %s", got.ID, bobReq)
	}
	noMessage(t, bob)

	// Neither can answer the other's request: bob's attempt on alice's
	// request matches nothing and leaves it pending for alice.
	if answered(r, "bob", "ssh-agent", "box", aliceReq, true) {
		t.Fatal("bob answered alice's request")
	}
	if answered(r, "", "ssh-agent", "box", aliceReq, true) {
		t.Fatal("the implicit user answered alice's request")
	}

	// Neither inherits the other's pending requests on reconnect: bob
	// reconnects and gets only his own still-pending request.
	if !r.Unregister(bob.Key(), bob.Token) {
		t.Fatal("bob's listener was not removed")
	}
	bob2, n := watch.register(t, r, uk("bob", "ssh-agent"))
	if n != 1 {
		t.Fatalf("bob's reconnect re-delivers %d requests, want only his own one", n)
	}
	if got := recv(t, bob2); got.ID != bobReq {
		t.Fatalf("bob was re-delivered %s, want only his own %s", got.ID, bobReq)
	}
	watch.ended(t, bob2)
	noMessage(t, bob2)

	if m := r.match("alice", "ssh-agent", "box", aliceReq); m == nil || m.conn != 1 {
		t.Fatalf("alice's answer to her request matches %+v, want her request on connection 1", m)
	}
	if m := r.match("bob", "ssh-agent", "bobs", bobReq); m == nil || m.conn != 2 {
		t.Fatalf("bob's answer to his request matches %+v, want his request on connection 2", m)
	}
}

// TestPublishForUserWithNoListener: a request for a user who has no machine
// subscribed is refused (the caller drops and logs it), and nothing is
// recorded: that user's machine connecting later is not sent it, and it
// cannot be answered.
func TestPublishForUserWithNoListener(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Register(uk("alice", "ssh-agent")); err != nil {
		t.Fatal(err)
	}
	req := NewEnvelope("ssh-agent", MessageTypeRequest, nil)
	req.Shed = &ShedInfo{Name: "bobs"}
	err := r.Publish(context.Background(), "bob", 1, req)
	if err == nil || !strings.Contains(err.Error(), `user "bob"`) {
		t.Fatalf("Publish for bob with no listener = %v, want an error naming him", err)
	}
	bob, n := watchRedelivery(t).register(t, r, uk("bob", "ssh-agent"))
	if n != 0 {
		t.Fatalf("bob's listener is re-delivered %d requests, want none", n)
	}
	noMessage(t, bob)
	if answered(r, "bob", "ssh-agent", "bobs", req.ID, true) {
		t.Fatal("a request that reached no listener was answerable")
	}
	if n := len(r.pending); n != 0 || r.totalBytes != 0 {
		t.Fatalf("a request that reached no listener left %d pending, %d bytes", n, r.totalBytes)
	}
}

// TestLateUnregisterKeepsNewerListener: a handler that ends late (its
// connection dropped, a new one already subscribed) unregisters by its own
// token and leaves the newer listener in place, open; its own Done is closed
// exactly once however often it is unregistered.
func TestLateUnregisterKeepsNewerListener(t *testing.T) {
	r := NewRegistry()
	old, err := r.Register(uk("alice", "ssh-agent"))
	if err != nil {
		t.Fatal(err)
	}
	// The old stream is reaped (its Done closes) and a reconnect registers
	// before the old handler's deferred Unregister has run.
	if !r.Unregister(old.Key(), old.Token) {
		t.Fatal("first unregister of the old listener did nothing")
	}
	newer, err := r.Register(uk("alice", "ssh-agent"))
	if err != nil {
		t.Fatal(err)
	}
	if newer.Token == old.Token {
		t.Fatal("a new listener reused a token")
	}

	// The old handler's deferred Unregister now runs, late.
	if r.Unregister(old.Key(), old.Token) {
		t.Fatal("the old handler's late unregister removed the newer listener")
	}
	if got, ok := r.Get(uk("alice", "ssh-agent")); !ok || got != newer {
		t.Fatalf("after the late unregister the key holds %v (%v), want the newer listener", got, ok)
	}
	select {
	case <-newer.Done:
		t.Fatal("the newer listener was closed by the old handler's unregister")
	default:
	}
	select {
	case <-old.Done:
	default:
		t.Fatal("the old listener's Done is not closed")
	}
	// Requests still reach the newer listener.
	id := publishFrom(t, r, "alice", 1, "ssh-agent", "box")
	if got := recv(t, newer); got.ID != id {
		t.Fatalf("newer listener received %s, want %s", got.ID, id)
	}
}

// TestListFiltersByUser: List returns the user's listeners, or all of them.
func TestListFiltersByUser(t *testing.T) {
	r := NewRegistry()
	for _, key := range []ListenerKey{uk("alice", "ssh-agent"), uk("bob", "ssh-agent"), uk("alice", "docker")} {
		if _, err := r.Register(key); err != nil {
			t.Fatal(err)
		}
	}
	names := func(infos []ListenerInfo) []string {
		var out []string
		for _, i := range infos {
			out = append(out, i.User+"/"+i.Namespace)
		}
		return out
	}
	if got := names(r.List("alice", false)); strings.Join(got, ",") != "alice/docker,alice/ssh-agent" {
		t.Errorf("alice's listeners = %v", got)
	}
	if got := names(r.List("bob", false)); strings.Join(got, ",") != "bob/ssh-agent" {
		t.Errorf("bob's listeners = %v", got)
	}
	if got := names(r.List("carol", false)); len(got) != 0 {
		t.Errorf("carol's listeners = %v, want none", got)
	}
	if got := names(r.List("alice", true)); len(got) != 3 {
		t.Errorf("all listeners = %v, want 3", got)
	}
}
