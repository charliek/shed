package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBridgeRegisterAndListSheds(t *testing.T) {
	b := NewBridge(NewRegistry(), false)

	sheds := b.ListSheds("", true)
	if len(sheds) != 0 {
		t.Errorf("expected empty list, got %d", len(sheds))
	}

	b.RegisterShed("dev", &ShedConn{
		Name:    "dev",
		Backend: "vz",
		Server:  "mini",
		Send:    func(env *Envelope) error { return nil },
	})

	sheds = b.ListSheds("", true)
	if len(sheds) != 1 {
		t.Fatalf("expected 1 shed, got %d", len(sheds))
	}
	if sheds[0].Name != "dev" || sheds[0].Backend != "vz" || sheds[0].Server != "mini" {
		t.Errorf("unexpected shed info: %+v", sheds[0])
	}
}

func TestBridgeUnregisterShed(t *testing.T) {
	b := NewBridge(NewRegistry(), false)
	b.RegisterShed("dev", &ShedConn{
		Name: "dev",
		Send: func(env *Envelope) error { return nil },
	})

	b.UnregisterShed("dev")

	if len(b.ListSheds("", true)) != 0 {
		t.Error("expected empty list after unregister")
	}
}

func TestBridgePublishToHost(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg, false)

	l, _ := reg.Register(k("op"))
	gen := b.RegisterShed("dev", &ShedConn{
		Name:    "dev",
		Backend: "vz",
		Server:  "mini",
		Send:    func(env *Envelope) error { return nil },
	})

	env := NewEnvelope("op", MessageTypeRequest, json.RawMessage(`{"cmd":"read"}`))
	// A guest-supplied shed field is overwritten from the bridge's record.
	env.Shed = &ShedInfo{Name: "someone-else", Backend: "x", Server: "y"}
	guestID := env.ID
	if err := b.PublishToHost(context.Background(), "dev", gen, env); err != nil {
		t.Fatalf("PublishToHost: %v", err)
	}

	got := recv(t, l)
	if got.ID == guestID || got.ID != env.ID {
		t.Errorf("the listener was sent ID %q (the guest chose %q), want the server's ID %q", got.ID, guestID, env.ID)
	}
	if got.Shed == nil {
		t.Fatal("expected shed info to be enriched")
	}
	if got.Shed.Name != "dev" || got.Shed.Backend != "vz" || got.Shed.Server != "mini" {
		t.Errorf("unexpected shed info: %+v", got.Shed)
	}
	if string(got.Payload) != `{"cmd":"read"}` {
		t.Errorf("payload = %s", got.Payload)
	}
}

func TestBridgePublishToHostNoListener(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg, false)
	gen := b.RegisterShed("dev", &ShedConn{Name: "dev", Send: func(env *Envelope) error { return nil }})

	env := NewEnvelope("unregistered", MessageTypeRequest, nil)
	if err := b.PublishToHost(context.Background(), "dev", gen, env); err == nil {
		t.Fatal("expected error when no listener for namespace")
	}
}

// TestBridgePublishToHostUnknownShed: a publish from a shed that is not
// registered, or from a connection that is no longer the one registered
// under its name, is dropped with an error, reaches no listener and leaves
// no pending request.
func TestBridgePublishToHostUnknownShed(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg, false)
	l, _ := reg.Register(k("op"))

	unknown := NewEnvelope("op", MessageTypeRequest, nil)
	unknown.Shed = &ShedInfo{Name: "unknown"}
	if err := b.PublishToHost(context.Background(), "unknown", 1, unknown); err == nil {
		t.Fatal("PublishToHost from an unregistered shed was accepted")
	}

	old := b.RegisterShed("dev", &ShedConn{Send: func(*Envelope) error { return nil }})
	b.UnregisterShed("dev")
	cur := b.RegisterShed("dev", &ShedConn{Send: func(*Envelope) error { return nil }})
	if old == cur {
		t.Fatal("two registrations got the same generation")
	}
	stale := NewEnvelope("op", MessageTypeRequest, nil)
	stale.Shed = &ShedInfo{Name: "dev"} // the tuple a wrongly kept request would be tracked under
	if err := b.PublishToHost(context.Background(), "dev", old, stale); err == nil {
		t.Fatal("PublishToHost from a replaced connection was accepted")
	}
	noMessage(t, l)
	// Neither dropped request left anything behind.
	if n := len(reg.pending); n != 0 || reg.totalBytes != 0 {
		t.Fatalf("the dropped publishes left %d pending requests, %d bytes", n, reg.totalBytes)
	}
}

// answer is user's final answer to the request the server sent its listener
// as id, for shed.
func answer(namespace, shed, id string) *Envelope {
	env := NewResponse(id, namespace, json.RawMessage(`{"ok":true}`))
	env.Shed = &ShedInfo{Name: shed}
	return env
}

// TestBridgeRespond: an answer to a request a listener was sent is written
// through the registration's Send with the guest's own ID put back; an answer
// to nothing, or naming another shed, is ErrUnmatched, and a request is
// answered once.
func TestBridgeRespond(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg, false)
	l, _ := reg.Register(k("op"))

	var received []*Envelope
	gen := b.RegisterShed("dev", &ShedConn{
		Name: "dev",
		Send: func(env *Envelope) error {
			received = append(received, env)
			return nil
		},
	})
	req := NewEnvelope("op", MessageTypeRequest, nil)
	guestID := req.ID
	if err := b.PublishToHost(context.Background(), "dev", gen, req); err != nil {
		t.Fatal(err)
	}
	sent := recv(t, l)

	if err := b.Respond("", answer("op", "unknown", sent.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("an answer naming another shed = %v, want ErrUnmatched", err)
	}
	if err := b.Respond("", answer("op", "dev", guestID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("an answer carrying the guest's own ID = %v, want ErrUnmatched", err)
	}
	if len(received) != 0 {
		t.Fatalf("an unmatched answer was written %d times", len(received))
	}
	resp := answer("op", "dev", sent.ID)
	if err := b.Respond("", resp); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if len(received) != 1 || received[0].InReplyTo != guestID || received[0].ID != resp.ID || string(received[0].Payload) != `{"ok":true}` {
		t.Fatalf("the shed was written %+v, want the answer with in_reply_to %q", received, guestID)
	}
	if resp.InReplyTo != sent.ID {
		t.Fatal("Respond changed the caller's envelope")
	}
	if err := b.Respond("", answer("op", "dev", sent.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("a replayed answer = %v, want ErrUnmatched", err)
	}
}

func TestBridgeConcurrentAccess(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg, false)
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
			b.ListSheds("", true)
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
	b := NewBridge(reg, false)
	l, err := reg.Register(k("ssh-agent"))
	if err != nil {
		t.Fatalf("register listener: %v", err)
	}

	ownedSends := 0
	ownedGen := b.RegisterShed("owned", &ShedConn{
		Send:  func(*Envelope) error { ownedSends++; return nil },
		Owned: true,
	})
	mineGen := b.RegisterShed("mine", &ShedConn{Send: func(*Envelope) error { return nil }})

	req := NewEnvelope("ssh-agent", MessageTypeRequest, json.RawMessage(`{"op":"list"}`))
	if err := b.PublishToHost(context.Background(), "owned", ownedGen, req); err != nil {
		t.Fatalf("PublishToHost(owned) = %v, want a silent drop", err)
	}
	noMessage(t, l)

	req2 := NewEnvelope("ssh-agent", MessageTypeRequest, json.RawMessage(`{"op":"list"}`))
	if err := b.PublishToHost(context.Background(), "mine", mineGen, req2); err != nil {
		t.Fatalf("PublishToHost(mine): %v", err)
	}
	got := recv(t, l)
	if got.Shed == nil || got.Shed.Name != "mine" {
		t.Fatalf("unowned shed's envelope arrived without its shed info: %+v", got.Shed)
	}

	// Nothing of an owned shed is ever pending, so nothing can be answered
	// into it: not its own dropped request, and not another shed's request
	// re-addressed to it.
	for _, id := range []string{req.ID, got.ID} {
		if err := b.Respond("", answer("ssh-agent", "owned", id)); !errors.Is(err, ErrUnmatched) {
			t.Fatalf("an answer into an owned shed = %v, want ErrUnmatched", err)
		}
	}
	if ownedSends != 0 {
		t.Fatalf("owned shed's Send was called %d times", ownedSends)
	}

	for _, s := range b.ListSheds("", true) {
		if s.Name == "owned" {
			t.Fatalf("owned shed listed to bus consumers: %+v", b.ListSheds("", true))
		}
	}
	if _, ok := b.ShedUser("owned"); ok {
		t.Fatal("owned shed has a bus user")
	}
}

var (
	t1 = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
)

// usersBridge is a bridge of a server with a user list, with alice's and
// bob's ssh-agent listeners.
func usersBridge(t *testing.T) (*Bridge, *Registry, *Listener, *Listener) {
	t.Helper()
	reg := NewRegistry()
	b := NewBridge(reg, true)
	alice, err := reg.Register(uk("alice", "ssh-agent"))
	if err != nil {
		t.Fatal(err)
	}
	bob, err := reg.Register(uk("bob", "ssh-agent"))
	if err != nil {
		t.Fatal(err)
	}
	return b, reg, alice, bob
}

func nopSend(*Envelope) error { return nil }

// TestBridgeRoutesByTheShedsUser: a request is routed by the user on the
// bridge's own record of the shed, never by anything in the envelope: alice's
// shed reaches only alice's listener, even when the guest names bob's shed.
func TestBridgeRoutesByTheShedsUser(t *testing.T) {
	b, _, alice, bob := usersBridge(t)
	boxGen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: nopSend})
	bobsGen := b.RegisterShed("bobs", &ShedConn{User: "bob", CreatedAt: t1, Send: nopSend})

	// The guest of alice's shed claims to be bob's shed.
	forged := NewEnvelope("ssh-agent", MessageTypeRequest, nil)
	forged.Shed = &ShedInfo{Name: "bobs", Backend: "x", Server: "y"}
	if err := b.PublishToHost(context.Background(), "box", boxGen, forged); err != nil {
		t.Fatal(err)
	}
	got := recv(t, alice)
	if got.ID != forged.ID || got.Shed == nil || got.Shed.Name != "box" {
		t.Fatalf("alice received %+v (shed %+v), want the request stamped as box", got, got.Shed)
	}
	noMessage(t, bob)

	req := NewEnvelope("ssh-agent", MessageTypeRequest, nil)
	if err := b.PublishToHost(context.Background(), "bobs", bobsGen, req); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, bob); got.ID != req.ID {
		t.Fatalf("bob received %s, want %s", got.ID, req.ID)
	}
	noMessage(t, alice)
}

// TestBridgeDropsShedsWithNoRoute: on a server with a user list a shed with
// no user (not yet assigned) or an unresolved one is routed nowhere: its
// requests are dropped without an error, it has no bus user, and only an
// admin's listing shows it.
func TestBridgeDropsShedsWithNoRoute(t *testing.T) {
	b, reg, alice, bob := usersBridge(t)
	implicit, err := reg.Register(k("ssh-agent"))
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"", "!unresolved"} {
		gen := b.RegisterShed("lost", &ShedConn{User: user, CreatedAt: t1, Send: nopSend})
		lost := NewEnvelope("ssh-agent", MessageTypeRequest, nil)
		if err := b.PublishToHost(context.Background(), "lost", gen, lost); err != nil {
			t.Fatalf("user %q: PublishToHost = %v, want a silent drop", user, err)
		}
		for _, who := range []string{"", "alice", user} {
			if err := b.Respond(who, answer("ssh-agent", "lost", lost.ID)); !errors.Is(err, ErrUnmatched) {
				t.Fatalf("user %q: %q answering a shed routed nowhere = %v, want ErrUnmatched", user, who, err)
			}
		}
		noMessage(t, alice)
		noMessage(t, bob)
		noMessage(t, implicit)
		if u, ok := b.ShedUser("lost"); ok {
			t.Fatalf("user %q: shed routed to %q", user, u)
		}
		if got := b.ListSheds("alice", false); len(got) != 0 {
			t.Fatalf("user %q: alice lists %+v", user, got)
		}
		if got := b.ListSheds("", false); len(got) != 0 {
			t.Fatalf("user %q: the implicit user lists %+v", user, got)
		}
		if got := b.ListSheds("root", true); len(got) != 1 || got[0].Name != "lost" || got[0].User != "" {
			t.Fatalf("user %q: an admin lists %+v, want the shed with no user", user, got)
		}
		if n := len(reg.pending); n != 0 {
			t.Fatalf("user %q: the dropped request left %d pending", user, n)
		}
	}
}

// TestBridgeWithNoUserListRoutesEveryShedAsTheImplicitUser: on a server with
// no user list, every shed routes as the implicit user whatever user its
// record carries (a user list removed later leaves sheds reachable).
func TestBridgeWithNoUserListRoutesEveryShedAsTheImplicitUser(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg, false)
	l, err := reg.Register(k("ssh-agent"))
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"", "alice", "!unresolved"} {
		gen := b.RegisterShed("box", &ShedConn{User: user, CreatedAt: t1, Send: nopSend})
		req := NewEnvelope("ssh-agent", MessageTypeRequest, nil)
		if err := b.PublishToHost(context.Background(), "box", gen, req); err != nil {
			t.Fatalf("user %q: %v", user, err)
		}
		if got := recv(t, l); got.ID != req.ID {
			t.Fatalf("user %q: implicit listener received %s, want %s", user, got.ID, req.ID)
		}
		if u, ok := b.ShedUser("box"); !ok || u != "" {
			t.Fatalf("user %q: ShedUser = (%q, %v), want the implicit user", user, u, ok)
		}
		if got := b.ListSheds("", false); len(got) != 1 || got[0].User != "" {
			t.Fatalf("user %q: implicit listing = %+v", user, got)
		}
	}
}

// TestBridgeResponseIsBoundToTheConnection: an answer is written into the
// connection its request arrived on, by the shed's user only, and never into
// a shed registered under the same name since, whoever that one belongs to.
func TestBridgeResponseIsBoundToTheConnection(t *testing.T) {
	for _, newUser := range []string{"alice", "bob"} {
		t.Run("recreated_by_"+newUser, func(t *testing.T) {
			b, _, alice, _ := usersBridge(t)
			var oldSent, newSent int
			oldGen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: func(*Envelope) error { oldSent++; return nil }})
			ctx := context.Background()
			first, second := request("ssh-agent", "box", "", nil), request("ssh-agent", "box", "", nil)
			for _, req := range []*Envelope{first, second} {
				if err := b.PublishToHost(ctx, "box", oldGen, req); err != nil {
					t.Fatal(err)
				}
				recv(t, alice)
			}

			if err := b.Respond("bob", answer("ssh-agent", "box", first.ID)); !errors.Is(err, ErrUnmatched) {
				t.Fatalf("bob answering alice's shed = %v, want ErrUnmatched", err)
			}
			if err := b.Respond("alice", answer("ssh-agent", "box", first.ID)); err != nil || oldSent != 1 {
				t.Fatalf("alice answering her shed = %v (sent %d), want delivered once", err, oldSent)
			}

			// The shed is deleted and created again under its name.
			b.UnregisterShed("box")
			b.RegisterShed("box", &ShedConn{User: newUser, CreatedAt: t2, Send: func(*Envelope) error { newSent++; return nil }})
			if err := b.Respond("alice", answer("ssh-agent", "box", second.ID)); !errors.Is(err, ErrUnmatched) {
				t.Fatalf("an answer for the old connection = %v, want ErrUnmatched", err)
			}
			if newSent != 0 || oldSent != 1 {
				t.Fatalf("the old connection's answer was delivered (old %d, new %d)", oldSent, newSent)
			}
		})
	}
}

// TestBridgeUnregisterConnKeepsNewerRegistration: unregistering an old
// connection by its generation leaves a newer one under the name in place.
func TestBridgeUnregisterConnKeepsNewerRegistration(t *testing.T) {
	b := NewBridge(NewRegistry(), false)
	old := b.RegisterShed("box", &ShedConn{Send: nopSend})
	cur := b.RegisterShed("box", &ShedConn{Send: nopSend})
	if b.UnregisterConn("box", old) {
		t.Fatal("unregistering the old connection removed the newer one")
	}
	if _, ok := b.ShedUser("box"); !ok {
		t.Fatal("the newer registration is gone")
	}
	if !b.UnregisterConn("box", cur) {
		t.Fatal("unregistering the current connection did nothing")
	}
	if _, ok := b.ShedUser("box"); ok {
		t.Fatal("the current registration is still there")
	}
}

// TestBridgeSetShedUserMovesTheRoute: assigning a running shed to a user
// moves its route without a new connection; it acts on the named incarnation
// only, never on a service client's shed, and a response from the previous
// user's machine is refused afterwards.
func TestBridgeSetShedUserMovesTheRoute(t *testing.T) {
	b, _, alice, bob := usersBridge(t)
	gen := b.RegisterShed("box", &ShedConn{User: "", CreatedAt: t1, Send: nopSend})

	if b.SetShedUser("box", t2, "alice") {
		t.Fatal("SetShedUser acted on another incarnation")
	}
	if !b.SetShedUser("box", t1, "alice") {
		t.Fatal("SetShedUser refused the shed's own incarnation")
	}
	req := NewEnvelope("ssh-agent", MessageTypeRequest, nil)
	if err := b.PublishToHost(context.Background(), "box", gen, req); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, alice); got.ID != req.ID {
		t.Fatalf("alice received %s, want %s", got.ID, req.ID)
	}

	if !b.SetShedUser("box", t1, "bob") {
		t.Fatal("SetShedUser to bob refused")
	}
	if err := b.Respond("alice", answer("ssh-agent", "box", req.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("alice answering after the move = %v, want ErrUnmatched", err)
	}
	req2 := NewEnvelope("ssh-agent", MessageTypeRequest, nil)
	if err := b.PublishToHost(context.Background(), "box", gen, req2); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, bob); got.ID != req2.ID {
		t.Fatalf("bob received %s, want %s", got.ID, req2.ID)
	}
	noMessage(t, alice)

	b.RegisterShed("owned", &ShedConn{Owned: true, CreatedAt: t1, Send: nopSend})
	if b.SetShedUser("owned", t1, "alice") {
		t.Fatal("SetShedUser moved a service client's shed onto the bus")
	}
}

// TestBridgeIncarnationUser: the user of a shed incarnation is answered only
// for the incarnation connected now, read afresh on every call.
func TestBridgeIncarnationUser(t *testing.T) {
	b := NewBridge(NewRegistry(), true)
	b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: nopSend})
	if u, ok := b.IncarnationUser("box", t1); !ok || u != "alice" {
		t.Fatalf("IncarnationUser(box, t1) = (%q, %v), want alice", u, ok)
	}
	if _, ok := b.IncarnationUser("box", time.Time{}); ok {
		t.Fatal("a record with no incarnation was attributed")
	}
	b.UnregisterShed("box")
	b.RegisterShed("box", &ShedConn{User: "bob", CreatedAt: t2, Send: nopSend})
	if u, ok := b.IncarnationUser("box", t1); ok {
		t.Fatalf("the old incarnation is attributed to %q after the name was reused", u)
	}
	if u, ok := b.IncarnationUser("box", t2); !ok || u != "bob" {
		t.Fatalf("IncarnationUser(box, t2) = (%q, %v), want bob", u, ok)
	}
}

// TestBridgeListShedsFiltersByUser: a user lists their own sheds; an admin
// (all) lists every shed but service clients'.
func TestBridgeListShedsFiltersByUser(t *testing.T) {
	b := NewBridge(NewRegistry(), true)
	b.RegisterShed("box", &ShedConn{User: "alice", Send: nopSend})
	b.RegisterShed("bobs", &ShedConn{User: "bob", Send: nopSend})
	b.RegisterShed("svc", &ShedConn{Owned: true, Send: nopSend})
	names := func(infos []ShedConnInfo) string {
		var out []string
		for _, i := range infos {
			out = append(out, i.Name+"="+i.User)
		}
		return strings.Join(out, ",")
	}
	if got := names(b.ListSheds("alice", false)); got != "box=alice" {
		t.Errorf("alice lists %s", got)
	}
	if got := names(b.ListSheds("bob", false)); got != "bobs=bob" {
		t.Errorf("bob lists %s", got)
	}
	if got := names(b.ListSheds("root", true)); got != "bobs=bob,box=alice" {
		t.Errorf("admin lists %s", got)
	}
}
