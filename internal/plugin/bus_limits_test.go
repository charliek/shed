package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// What identifies a request on the credential bus, and what is checked at
// each point where a request or its answer has an effect: the server's own
// request IDs, the route check where a message is queued, taken and
// answered, the consumption of a request by its written answer, and the
// bounds on responses. Interleavings are forced with hooks and barriers.

// publish publishes a request with the given guest ID from the shed's
// connection gen and returns it (its ID is then the server's).
func publish(t *testing.T, b *Bridge, shed string, gen uint64, guestID string) *Envelope {
	t.Helper()
	req := request("ssh-agent", shed, guestID, json.RawMessage(`{"op":"sign"}`))
	if err := b.PublishToHost(context.Background(), shed, gen, req); err != nil {
		t.Fatalf("PublishToHost(%s): %v", shed, err)
	}
	return req
}

// counted is a ShedConn.Send that records what it is asked to write.
type counted struct{ written []*Envelope }

func (c *counted) send(env *Envelope) error {
	c.written = append(c.written, env)
	return nil
}

// routeChanges are the ways a shed connection's route changes: every one
// must stop what was published on the old route.
var routeChanges = []struct {
	name string
	do   func(b *Bridge)
}{
	{"user set", func(b *Bridge) { b.SetShedUser("box", t1, "bob") }},
	{"route held", func(b *Bridge) { b.SuspendRoute("box", t1) }},
	{"connection replaced", func(b *Bridge) {
		b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: nopSend})
	}},
	{"connection removed", func(b *Bridge) { b.UnregisterShed("box") }},
}

// TestGuestChosenIDsNeverCollide: the server gives every request an ID of
// its own, so the ID a guest chose identifies nothing. Two connections of a
// name (a shed created again), two requests of one connection, and two
// users' sheds all use the guest ID "X" here, and each request stays its
// own: a late answer to the first connection's request matches nothing once
// that connection is gone (it never reaches the newer connection's request
// of the same guest ID), and each answer is written into the connection its
// request came from, with "X" put back.
func TestGuestChosenIDsNeverCollide(t *testing.T) {
	b, reg, alice, bob := usersBridge(t)
	var oldConn, newConn, bobsConn counted
	oldGen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: oldConn.send})
	bobsGen := b.RegisterShed("bobs", &ShedConn{User: "bob", CreatedAt: t1, Send: bobsConn.send})

	stale := publish(t, b, "box", oldGen, "X")
	if got := recv(t, alice); got.ID != stale.ID || got.ID == "X" {
		t.Fatalf("alice was sent ID %q, want the server's ID for the request (%q), never the guest's", got.ID, stale.ID)
	}

	// The shed is deleted and created again; its guest reuses the ID.
	b.UnregisterShed("box")
	newGen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t2, Send: newConn.send})
	first := publish(t, b, "box", newGen, "X")
	second := publish(t, b, "box", newGen, "X") // and again, on the same connection
	theirs := publish(t, b, "bobs", bobsGen, "X")
	ids := map[string]bool{stale.ID: true, first.ID: true, second.ID: true, theirs.ID: true}
	if len(ids) != 4 {
		t.Fatalf("four requests got %d distinct server IDs: %v", len(ids), ids)
	}
	recv(t, alice)
	recv(t, alice)
	recv(t, bob)

	// The late answer to the old connection's request.
	if err := b.Respond("alice", answer("ssh-agent", "box", stale.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("an answer to the retired connection's request = %v, want ErrUnmatched", err)
	}
	// Nobody answers a request by the ID its guest chose.
	if err := b.Respond("alice", answer("ssh-agent", "box", "X")); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("an answer carrying the guest's ID = %v, want ErrUnmatched", err)
	}
	// Nor another user's request, by its server ID.
	if err := b.Respond("alice", answer("ssh-agent", "bobs", theirs.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("alice answering bob's request = %v, want ErrUnmatched", err)
	}
	if err := b.Respond("bob", answer("ssh-agent", "box", first.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("bob answering alice's request = %v, want ErrUnmatched", err)
	}
	if n := len(oldConn.written) + len(newConn.written) + len(bobsConn.written); n != 0 {
		t.Fatalf("%d refused answers were written into a shed", n)
	}
	if len(reg.pending) != 3 {
		t.Fatalf("%d requests pending, want the three of live connections", len(reg.pending))
	}

	for _, req := range []*Envelope{second, first} {
		if err := b.Respond("alice", answer("ssh-agent", "box", req.ID)); err != nil {
			t.Fatalf("answering request %s: %v", req.ID, err)
		}
	}
	if err := b.Respond("bob", answer("ssh-agent", "bobs", theirs.ID)); err != nil {
		t.Fatalf("bob answering his request: %v", err)
	}
	if len(oldConn.written) != 0 {
		t.Fatalf("the retired connection was written %d answers", len(oldConn.written))
	}
	if len(newConn.written) != 2 || len(bobsConn.written) != 1 {
		t.Fatalf("answers written: box %d, bobs %d; want 2 and 1", len(newConn.written), len(bobsConn.written))
	}
	for _, env := range append(newConn.written, bobsConn.written...) {
		if env.InReplyTo != "X" {
			t.Fatalf("an answer was written with in_reply_to %q, want the guest's own ID X", env.InReplyTo)
		}
	}
	if len(reg.pending) != 0 || reg.totalBytes != 0 {
		t.Fatalf("after every answer %d requests are pending, %d bytes counted", len(reg.pending), reg.totalBytes)
	}
}

// TestOnlyTheIDChangesOnTheWire: what a listener is sent is the guest's
// envelope with the server's ID in place of the guest's and the server's own
// shed field, and nothing else changed; what is written into the shed is the
// listener's answer with the guest's ID back in in_reply_to, and nothing
// else changed.
func TestOnlyTheIDChangesOnTheWire(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg, false)
	l, _ := reg.Register(k("ssh-agent"))
	var conn counted
	gen := b.RegisterShed("box", &ShedConn{Backend: "vz", Server: "mini", Send: conn.send})

	fromGuest := []byte(`{"id":"guest-req-1","namespace":"ssh-agent","type":"request","final":true,"timestamp":"2026-11-15T10:00:00Z","payload":{"operation":"list"}}`)
	var req Envelope
	if err := json.Unmarshal(fromGuest, &req); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishToHost(context.Background(), "box", gen, &req); err != nil {
		t.Fatal(err)
	}
	sent := recv(t, l)
	toListener, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent.ID) != requestIDLen || sent.ID == "guest-req-1" {
		t.Fatalf("the listener was sent ID %q, want a server ID of %d bytes", sent.ID, requestIDLen)
	}
	want := `{"id":"` + sent.ID + `","namespace":"ssh-agent","type":"request","final":true,"timestamp":"2026-11-15T10:00:00Z","payload":{"operation":"list"},"shed":{"name":"box","backend":"vz","server":"mini"}}`
	if string(toListener) != want {
		t.Fatalf("the listener was sent\n%s\nwant\n%s", toListener, want)
	}

	fromListener := []byte(`{"id":"host-resp-1","namespace":"ssh-agent","type":"response","in_reply_to":"` + sent.ID + `","final":true,"timestamp":"2026-11-15T10:00:01Z","payload":{"keys":[]},"shed":{"name":"box","backend":"vz","server":"mini"}}`)
	var resp Envelope
	if err := json.Unmarshal(fromListener, &resp); err != nil {
		t.Fatal(err)
	}
	if err := b.Respond("", &resp); err != nil {
		t.Fatal(err)
	}
	if len(conn.written) != 1 {
		t.Fatalf("the shed was written %d answers, want 1", len(conn.written))
	}
	toGuest, err := json.Marshal(conn.written[0])
	if err != nil {
		t.Fatal(err)
	}
	want = `{"id":"host-resp-1","namespace":"ssh-agent","type":"response","in_reply_to":"guest-req-1","final":true,"timestamp":"2026-11-15T10:00:01Z","payload":{"keys":[]},"shed":{"name":"box","backend":"vz","server":"mini"}}`
	if string(toGuest) != want {
		t.Fatalf("the shed was written\n%s\nwant\n%s", toGuest, want)
	}
}

// TestRetiredConnectionsRequestsAreDropped: when a shed's connection goes its
// requests go with it: nothing is counted for them, a reconnecting listener
// is not re-sent them, an answer to one matches nothing, and a shed created
// under the name afterwards (here another user's) starts with all of its
// limit.
func TestRetiredConnectionsRequestsAreDropped(t *testing.T) {
	setVar(t, &maxMessagesPerConn, 3)
	b, reg, alice, bob := usersBridge(t)
	old := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: nopSend})
	var ids []string
	for range 3 {
		ids = append(ids, publish(t, b, "box", old, "").ID)
		recv(t, alice)
	}
	if err := b.PublishToHost(context.Background(), "box", old, request("ssh-agent", "box", "", nil)); !errors.Is(err, ErrPendingLimit) {
		t.Fatalf("test set-up: a fourth request of alice's shed = %v, want ErrPendingLimit", err)
	}

	b.UnregisterShed("box") // deleted
	if len(reg.pending) != 0 || reg.totalBytes != 0 || len(reg.conns) != 0 {
		t.Fatalf("a deleted shed left %d pending requests, %d bytes, %d connections", len(reg.pending), reg.totalBytes, len(reg.conns))
	}
	fresh := b.RegisterShed("box", &ShedConn{User: "bob", CreatedAt: t2, Send: nopSend})
	for range 3 {
		publish(t, b, "box", fresh, "")
		recv(t, bob)
	}
	if err := b.Respond("alice", answer("ssh-agent", "box", ids[0])); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("alice answering her deleted shed's request = %v, want ErrUnmatched", err)
	}
	reg.Unregister(alice.Key(), alice.Token)
	alice2, n := watchRedelivery(t).register(t, reg, uk("alice", "ssh-agent"))
	if n != 0 {
		t.Fatalf("alice's reconnecting machine is re-sent %d requests of her deleted shed", n)
	}
	noMessage(t, alice2)
}

// TestRouteChangeDropsWhatWasPublishedOnTheOldRoute: when a shed's route
// changes, its requests pending on the old route are dropped: the previous
// user's machine is not re-sent them on reconnect and cannot answer them,
// and the new user's machine is not sent them either. Setting the user the
// shed already has drops nothing.
func TestRouteChangeDropsWhatWasPublishedOnTheOldRoute(t *testing.T) {
	b, reg, alice, bob := usersBridge(t)
	gen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: nopSend})
	req := publish(t, b, "box", gen, "")
	recv(t, alice)

	if !b.SetShedUser("box", t1, "bob") {
		t.Fatal("SetShedUser refused")
	}
	reg.Unregister(alice.Key(), alice.Token)
	alice2 := watchRedelivery(t).settledRegister(t, reg, uk("alice", "ssh-agent"))
	noMessage(t, alice2)
	noMessage(t, bob)
	if err := b.Respond("alice", answer("ssh-agent", "box", req.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("alice answering a request of the shed now bob's = %v, want ErrUnmatched", err)
	}
	if len(reg.pending) != 0 {
		t.Fatalf("%d requests still pending after the shed was assigned away", len(reg.pending))
	}

	req2 := publish(t, b, "box", gen, "")
	recv(t, bob)
	if !b.SetShedUser("box", t1, "bob") {
		t.Fatal("SetShedUser refused")
	}
	if err := b.Respond("bob", answer("ssh-agent", "box", req2.ID)); err != nil {
		t.Fatalf("re-setting the same user dropped bob's request: %v", err)
	}
}

// TestRouteChangeStopsAPublishWaitingForRoom: a request whose publish is
// waiting for room in its listener's queue when the shed's route changes is
// not queued when room comes: the route is checked again, under the
// registry's lock, at the moment the request would be queued.
func TestRouteChangeStopsAPublishWaitingForRoom(t *testing.T) {
	for _, rc := range routeChanges {
		t.Run(rc.name, func(t *testing.T) {
			setVar(t, &publishWait, time.Hour)
			b, reg, alice, bob := usersBridge(t)
			gen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: nopSend})
			fill(t, reg, "alice", alice)
			waiting := make(chan struct{})
			setVar(t, &publishFullHook, func() { close(waiting) })

			published := make(chan error, 1)
			go func() {
				published <- b.PublishToHost(context.Background(), "box", gen, request("ssh-agent", "box", "", nil))
			}()
			<-waiting // the request is made, and its publish waits for room

			rc.do(b)

			recv(t, alice) // room: alice's stream takes a message
			var err error
			within(t, "the waiting publish", func() { err = <-published })
			if !errors.Is(err, errConnGone) {
				t.Fatalf("the publish that waited across the route change = %v, want errConnGone", err)
			}
			for len(alice.queue) > 0 {
				if m := <-alice.queue; m.shed == "box" {
					t.Fatal("the request was queued for alice after its shed's route changed")
				}
			}
			noMessage(t, bob)
			if len(reg.pending) != 0 {
				t.Fatalf("%d requests pending after the publish was dropped", len(reg.pending))
			}
		})
	}
}

// TestRouteChangeStopsQueuedWorkAtOnce: from the instant a shed's route
// changes (here: inside the bridge's hook, after the route was changed and
// before the old route's messages have been dropped), a request already in
// its listener's queue is not handed to the listener's stream, and an answer
// to a request the listener already has is not written. The change does not
// depend on the dropping that follows it.
func TestRouteChangeStopsQueuedWorkAtOnce(t *testing.T) {
	for _, rc := range routeChanges {
		t.Run(rc.name, func(t *testing.T) {
			b, reg, alice, _ := usersBridge(t)
			var conn counted
			gen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: conn.send})
			have := publish(t, b, "box", gen, "")
			recv(t, alice)                // alice's machine has this one
			publish(t, b, "box", gen, "") // and this one is still in her queue

			var (
				hooked     bool
				stillThere bool
				taken      *Envelope
				answerErr  error
			)
			setVar(t, &reroutedHook, func() {
				if hooked {
					return
				}
				hooked = true
				stillThere = reg.match("alice", "ssh-agent", "box", have.ID) != nil
				taken = alice.Take(<-alice.Messages)
				answerErr = b.Respond("alice", answer("ssh-agent", "box", have.ID))
			})
			rc.do(b)

			if !hooked {
				t.Fatal("test set-up: the route change never reached its hook")
			}
			if !stillThere {
				t.Fatal("test set-up: the old route's requests were dropped before the hook ran")
			}
			if taken != nil {
				t.Fatalf("a queued request was handed to alice's stream after the route changed: %+v", taken)
			}
			if !errors.Is(answerErr, ErrUnmatched) {
				t.Fatalf("an answer after the route changed = %v, want ErrUnmatched", answerErr)
			}
			if len(conn.written) != 0 {
				t.Fatalf("the shed was written %d answers after its route changed", len(conn.written))
			}
			if len(reg.pending) != 0 || reg.totalBytes != 0 {
				t.Fatalf("after the change %d requests are pending, %d bytes counted", len(reg.pending), reg.totalBytes)
			}
		})
	}
}

// TestRouteChangeStopsAResponseWaitingForTheWriteLock: an answer that passed
// every check and is waiting for the shed transport's write lock when the
// shed's route changes is not written when it gets the lock: the request is
// checked again with the lock held, immediately before the write.
func TestRouteChangeStopsAResponseWaitingForTheWriteLock(t *testing.T) {
	for _, rc := range routeChanges {
		t.Run(rc.name, func(t *testing.T) {
			b, _, alice, _ := usersBridge(t)
			waitingForLock := make(chan struct{})
			lockFree := make(chan struct{})
			var writes atomic.Int32
			gen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1,
				SendWithin: func(_ *Envelope, _ time.Time, admit func() error, wrote func()) error {
					close(waitingForLock) // behind a write in progress
					<-lockFree            // the write lock is had
					if err := admit(); err != nil {
						return err
					}
					writes.Add(1)
					wrote()
					return nil
				}})
			req := publish(t, b, "box", gen, "")
			recv(t, alice)

			done := make(chan error, 1)
			go func() { done <- b.Respond("alice", answer("ssh-agent", "box", req.ID)) }()
			<-waitingForLock

			rc.do(b)

			close(lockFree)
			var err error
			within(t, "the waiting response", func() { err = <-done })
			if !errors.Is(err, ErrUnmatched) {
				t.Fatalf("the answer that waited for the write lock across the route change = %v, want ErrUnmatched", err)
			}
			if n := writes.Load(); n != 0 {
				t.Fatalf("the shed was written %d answers after its route changed", n)
			}
		})
	}
}

// TestFinalResponseIsConsumedOnlyOnceWritten: a final answer consumes its
// request only when its frame has been written into the shed. An answer
// that could not get the transport's write lock in time wrote nothing and
// leaves the request pending for a retry; so does a write that failed; a
// non-final answer never consumes. Once a final answer is written the
// request is gone: a second final answer that matched before the first was
// written is refused at its own write.
func TestFinalResponseIsConsumedOnlyOnceWritten(t *testing.T) {
	b, reg, alice, _ := usersBridge(t)
	const (
		lockTimesOut = iota
		writeFails
		writes
	)
	mode := lockTimesOut
	errBroken := errors.New("broken pipe")
	var written []*Envelope
	gen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1,
		SendWithin: func(env *Envelope, _ time.Time, admit func() error, wrote func()) error {
			if mode == lockTimesOut {
				return ErrResponseTimeout // the write lock was not had in time
			}
			if err := admit(); err != nil {
				return err
			}
			if mode == writeFails {
				return errBroken
			}
			written = append(written, env)
			wrote()
			return nil
		}})
	req := publish(t, b, "box", gen, "guest-id")
	recv(t, alice)
	final := func() *Envelope { return answer("ssh-agent", "box", req.ID) }

	if err := b.Respond("alice", final()); !errors.Is(err, ErrResponseTimeout) {
		t.Fatalf("an answer that timed out before writing = %v, want ErrResponseTimeout", err)
	}
	mode = writeFails
	if err := b.Respond("alice", final()); !errors.Is(err, errBroken) {
		t.Fatalf("an answer whose write failed = %v, want the write's error", err)
	}
	if reg.match("alice", "ssh-agent", "box", req.ID) == nil {
		t.Fatal("an answer that was never written consumed its request")
	}

	mode = writes
	partial := final()
	partial.Final = false
	if err := b.Respond("alice", partial); err != nil || len(written) != 1 {
		t.Fatalf("a non-final answer = %v (written %d), want it written", err, len(written))
	}
	// Two final answers are matched before either is written.
	first, err := b.BeginResponse("alice", final())
	if err != nil {
		t.Fatalf("the retried final answer: %v", err)
	}
	defer first.End()
	second, err := b.BeginResponse("alice", final())
	if err != nil {
		t.Fatalf("a second final answer, before the first is written: %v", err)
	}
	defer second.End()
	if err := first.Send(); err != nil {
		t.Fatalf("the retried final answer's write: %v", err)
	}
	if err := second.Send(); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("a second final answer's write = %v, want ErrUnmatched", err)
	}
	if len(written) != 2 || written[1].InReplyTo != "guest-id" {
		t.Fatalf("the shed was written %d answers (%+v), want the non-final and one final, to guest-id", len(written), written)
	}
	if err := b.Respond("alice", final()); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("a replayed final answer = %v, want ErrUnmatched", err)
	}
	if len(reg.pending) != 0 || reg.totalBytes != 0 {
		t.Fatalf("after the final answer %d requests are pending, %d bytes counted", len(reg.pending), reg.totalBytes)
	}
}

// TestAStaleRequestIsNotAnswerable: a request older than pendingRetention
// cannot be answered, whether or not a sweep has run since it went stale (no
// publish or subscribe happens here after the first publish, so none does).
// Matching an answer refuses it and frees it; so does admitting an answer
// that was matched while the request was still within its retention. A
// request kept answerable by non-final answers stops being answerable too.
func TestAStaleRequestIsNotAnswerable(t *testing.T) {
	setVar(t, &sweepEvery, time.Hour) // the first publish sweeps; nothing after it does
	b, reg, alice, _ := usersBridge(t)
	sent := &counted{}
	gen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: sent.send})
	nonFinal := func(id string) *Envelope {
		env := answer("ssh-agent", "box", id)
		env.Final = false
		return env
	}

	t.Run("at matching", func(t *testing.T) {
		req := publish(t, b, "box", gen, "guest-1")
		recv(t, alice)
		if err := b.Respond("alice", nonFinal(req.ID)); err != nil {
			t.Fatalf("a non-final answer within the retention: %v", err)
		}
		setVar(t, &pendingRetention, -time.Second) // the request is stale from here
		// Refused at matching: no delivery begins, so no slot is taken and
		// nothing reaches the write.
		if resp, err := b.BeginResponse("alice", nonFinal(req.ID)); !errors.Is(err, ErrUnmatched) {
			if resp != nil {
				resp.End()
			}
			t.Fatalf("matching a non-final answer to a stale request = %v, want ErrUnmatched", err)
		}
		if len(sent.written) != 1 {
			t.Fatalf("the shed was written %d answers, want only the one before the request went stale", len(sent.written))
		}
		if len(reg.pending) != 0 || reg.totalBytes != 0 {
			t.Fatalf("a stale request refused at matching left %d pending, %d bytes counted; want it freed", len(reg.pending), reg.totalBytes)
		}
	})

	t.Run("at admission", func(t *testing.T) {
		before := len(sent.written)
		req := publish(t, b, "box", gen, "guest-2")
		recv(t, alice)
		resp, err := b.BeginResponse("alice", answer("ssh-agent", "box", req.ID))
		if err != nil {
			t.Fatalf("a final answer within the retention: %v", err)
		}
		defer resp.End()
		setVar(t, &pendingRetention, -time.Second) // stale between matching and writing
		if err := resp.Send(); !errors.Is(err, ErrUnmatched) {
			t.Fatalf("an answer admitted after its request went stale = %v, want ErrUnmatched", err)
		}
		if len(sent.written) != before {
			t.Fatal("an answer to a stale request was written")
		}
		if len(reg.pending) != 0 || reg.totalBytes != 0 {
			t.Fatalf("a stale request refused at admission left %d pending, %d bytes counted; want it freed", len(reg.pending), reg.totalBytes)
		}
	})
	checkAccounting(t, reg)
}

// TestSetShedUserNeverTouchesAReplacementsRequests: setting a shed's user
// drops requests by the connection generation it changed, never by the
// shed's name. Here the shed is deleted and created again by carol, and
// publishes, while an assignment of the old shed is between changing its
// route and dropping what that route left behind (the bridge's hook): the
// replacement's request survives and carol's machine answers it.
func TestSetShedUserNeverTouchesAReplacementsRequests(t *testing.T) {
	b, reg, alice, _ := usersBridge(t)
	carol, err := reg.Register(uk("carol", "ssh-agent"))
	if err != nil {
		t.Fatal(err)
	}
	old := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: nopSend})
	stale := publish(t, b, "box", old, "")
	recv(t, alice)

	var replacement counted
	var carols *Envelope
	hooked := false
	setVar(t, &reroutedHook, func() {
		if hooked {
			return
		}
		hooked = true
		gen := b.RegisterShed("box", &ShedConn{User: "carol", CreatedAt: t2, Send: replacement.send})
		carols = publish(t, b, "box", gen, "")
	})
	if !b.SetShedUser("box", t1, "bob") {
		t.Fatal("SetShedUser refused the shed's own incarnation")
	}
	if !hooked {
		t.Fatal("test set-up: the assignment never reached its hook")
	}

	if got := recv(t, carol); got.ID != carols.ID {
		t.Fatalf("carol was sent %s, want her shed's request %s", got.ID, carols.ID)
	}
	if err := b.Respond("carol", answer("ssh-agent", "box", carols.ID)); err != nil {
		t.Fatalf("carol answering her shed's request after the old shed's assignment finished: %v", err)
	}
	if len(replacement.written) != 1 {
		t.Fatalf("carol's shed was written %d answers, want 1", len(replacement.written))
	}
	if err := b.Respond("alice", answer("ssh-agent", "box", stale.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("alice answering the old shed's request = %v, want ErrUnmatched", err)
	}
	if u, ok := b.ShedUser("box"); !ok || u != "carol" {
		t.Fatalf("the replacement routes to (%q, %v), want carol: the old shed's assignment reached it", u, ok)
	}
}

// TestNothingWaitsForAWriteInProgress: no lock is held across a write into a
// shed. While an answer's frame is being written (and the guest is not
// taking it), the shed's user is set, its route held and lifted, its
// listings read, another shed's request published, and the shed replaced
// and removed, all without waiting. The write, already past its check,
// finishes.
func TestNothingWaitsForAWriteInProgress(t *testing.T) {
	b, _, alice, _ := usersBridge(t)
	writing := make(chan struct{})
	finish := make(chan struct{})
	gen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: func(*Envelope) error {
		close(writing)
		<-finish
		return nil
	}})
	other := b.RegisterShed("other", &ShedConn{User: "alice", CreatedAt: t1, Send: nopSend})
	req := publish(t, b, "box", gen, "")
	recv(t, alice)

	done := make(chan error, 1)
	go func() { done <- b.Respond("alice", answer("ssh-agent", "box", req.ID)) }()
	<-writing // past its check, its frame being written

	within(t, "the bridge's other work during a write", func() {
		b.SetShedUser("box", t1, "bob")
		b.SetShedUser("box", t1, "alice")
		b.ResumeRoute("box", b.SuspendRoute("box", t1))
		b.ListSheds("alice", false)
		b.ShedUser("box")
		publish(t, b, "other", other, "")
		recv(t, alice)
		b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: nopSend})
		b.UnregisterShed("box")
	})
	close(finish)
	if err := <-done; err != nil {
		t.Fatalf("the write already past its check = %v, want it to finish", err)
	}
}

// TestSuspendRouteHoldsTheIncarnation: holding a shed's route takes effect
// at once: the shed routes nowhere, a new request is dropped quietly, and
// what was pending is gone and stays gone when the hold is lifted. A
// reconnect of the same incarnation stays held; only the newest hold's epoch
// lifts it; another incarnation of the name is not held; and a server with
// no user list holds nothing.
func TestSuspendRouteHoldsTheIncarnation(t *testing.T) {
	b, reg, alice, _ := usersBridge(t)
	var conn counted
	gen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: conn.send})
	ctx := context.Background()
	before := publish(t, b, "box", gen, "")
	recv(t, alice)

	epoch := b.SuspendRoute("box", t1)
	if u, ok := b.ShedUser("box"); ok {
		t.Fatalf("a held route still routes to %q", u)
	}
	if err := b.PublishToHost(ctx, "box", gen, request("ssh-agent", "box", "", nil)); err != nil {
		t.Fatalf("a request of a held route = %v, want it dropped quietly", err)
	}
	noMessage(t, alice)
	if err := b.Respond("alice", answer("ssh-agent", "box", before.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("an answer into a held route = %v, want ErrUnmatched", err)
	}
	if len(reg.pending) != 0 || len(conn.written) != 0 {
		t.Fatalf("a held route left %d requests pending and took %d answers", len(reg.pending), len(conn.written))
	}

	// The transport reconnects: the same incarnation stays held.
	gen2 := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: conn.send})
	if u, ok := b.ShedUser("box"); ok {
		t.Fatalf("a reconnect lifted the hold (routes to %q)", u)
	}
	newer := b.SuspendRoute("box", t1)
	if b.ResumeRoute("box", epoch) {
		t.Fatal("a replaced hold's epoch lifted the newer hold")
	}
	if _, ok := b.ShedUser("box"); ok {
		t.Fatal("the route is no longer held after a stale ResumeRoute")
	}
	if !b.ResumeRoute("box", newer) {
		t.Fatal("the newest hold's epoch did not lift it")
	}
	if u, ok := b.ShedUser("box"); !ok || u != "alice" {
		t.Fatalf("after the hold is lifted the shed routes to (%q, %v), want alice", u, ok)
	}
	// What was pending before the hold did not come back with the route.
	if err := b.Respond("alice", answer("ssh-agent", "box", before.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("an answer to a request from before the hold = %v, want ErrUnmatched", err)
	}
	after := publish(t, b, "box", gen2, "")
	if got := recv(t, alice); got.ID != after.ID {
		t.Fatalf("alice received %s, want %s", got.ID, after.ID)
	}
	if err := b.Respond("alice", answer("ssh-agent", "box", after.ID)); err != nil || len(conn.written) != 1 {
		t.Fatalf("an answer once the hold is lifted = %v (written %d), want it written", err, len(conn.written))
	}

	t.Run("another incarnation is not held", func(t *testing.T) {
		b, _, _, _ := usersBridge(t)
		b.SuspendRoute("box", t1)
		b.RegisterShed("box", &ShedConn{User: "bob", CreatedAt: t2, Send: nopSend})
		if u, ok := b.ShedUser("box"); !ok || u != "bob" {
			t.Fatalf("a shed created again under a held name routes to (%q, %v), want bob", u, ok)
		}
	})
	t.Run("no user list", func(t *testing.T) {
		b := NewBridge(NewRegistry(), false)
		b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: nopSend})
		if epoch := b.SuspendRoute("box", t1); epoch != 0 {
			t.Fatalf("SuspendRoute on a server with no user list = %d, want 0", epoch)
		}
		if _, ok := b.ShedUser("box"); !ok {
			t.Fatal("a server with no user list held a route")
		}
	})
}

// TestAResumedRouteIsANewRoute: what was published before a route hold does
// not come back when the hold is lifted, even when it is lifted before the
// held route's messages have been dropped (here: inside the bridge's hook,
// between the hold and the dropping). The route the shed gets back says the
// same as the one it had and is still another route: a request is live only
// on the very route it was published on.
func TestAResumedRouteIsANewRoute(t *testing.T) {
	b, reg, alice, _ := usersBridge(t)
	var conn counted
	gen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: conn.send})
	have := publish(t, b, "box", gen, "")
	recv(t, alice)
	publish(t, b, "box", gen, "") // still in alice's queue

	var (
		hooked    bool
		resumed   bool
		taken     *Envelope
		answerErr error
	)
	setVar(t, &reroutedHook, func() {
		if hooked {
			return
		}
		hooked = true
		b.mu.RLock()
		epoch := b.holdSeq // the hold being made
		b.mu.RUnlock()
		resumed = b.ResumeRoute("box", epoch)
		taken = alice.Take(<-alice.Messages)
		answerErr = b.Respond("alice", answer("ssh-agent", "box", have.ID))
	})
	b.SuspendRoute("box", t1)

	if !hooked || !resumed {
		t.Fatalf("test set-up: hook reached %v, hold lifted inside it %v", hooked, resumed)
	}
	if u, ok := b.ShedUser("box"); !ok || u != "alice" {
		t.Fatalf("test set-up: the shed routes to (%q, %v) after the hold was lifted, want alice", u, ok)
	}
	if taken != nil {
		t.Fatalf("a request queued before the hold was handed to alice's stream after it: %+v", taken)
	}
	if !errors.Is(answerErr, ErrUnmatched) || len(conn.written) != 0 {
		t.Fatalf("an answer to a request from before the hold = %v (written %d), want ErrUnmatched", answerErr, len(conn.written))
	}
	if len(reg.pending) != 0 {
		t.Fatalf("%d requests from before the hold are still pending", len(reg.pending))
	}
	// The shed's new requests flow on its new route.
	after := publish(t, b, "box", gen, "")
	if got := recv(t, alice); got.ID != after.ID {
		t.Fatalf("alice received %s, want the request published after the hold %s", got.ID, after.ID)
	}
	if err := b.Respond("alice", answer("ssh-agent", "box", after.ID)); err != nil {
		t.Fatalf("an answer on the new route: %v", err)
	}
}

// TestResponsesInFlightAreBounded: a shed connection has at most
// maxResponsesInFlight responses being delivered; more are refused with
// ErrTooManyResponses, consume nothing, and a slot frees when its delivery
// ends.
func TestResponsesInFlightAreBounded(t *testing.T) {
	setVar(t, &maxResponsesInFlight, 2)
	b, reg, alice, _ := usersBridge(t)
	var conn counted
	gen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: conn.send})
	req := publish(t, b, "box", gen, "")
	recv(t, alice)
	resp := func() *Envelope { return answer("ssh-agent", "box", req.ID) }

	var inFlight []*Response
	for range 2 {
		r, err := b.BeginResponse("alice", resp())
		if err != nil {
			t.Fatal(err)
		}
		inFlight = append(inFlight, r)
	}
	if _, err := b.BeginResponse("alice", resp()); !errors.Is(err, ErrTooManyResponses) {
		t.Fatalf("a third response = %v, want ErrTooManyResponses", err)
	}
	if err := b.Respond("alice", resp()); !errors.Is(err, ErrTooManyResponses) {
		t.Fatalf("a third response = %v, want ErrTooManyResponses", err)
	}
	if len(conn.written) != 0 || reg.match("alice", "ssh-agent", "box", req.ID) == nil {
		t.Fatalf("a refused response wrote %d answers or consumed its request", len(conn.written))
	}
	// A response that matches nothing takes no slot.
	if _, err := b.BeginResponse("bob", resp()); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("bob's response = %v, want ErrUnmatched whatever slots are free", err)
	}

	inFlight[0].End()
	if err := b.Respond("alice", resp()); err != nil || len(conn.written) != 1 {
		t.Fatalf("a response once a slot is free = %v (written %d), want it written", err, len(conn.written))
	}
	inFlight[1].End()
}

// TestResponseDeadlineCoversTheWholeDelivery: a response's deadline starts
// when its delivery begins and is the one its transport is given, so the
// wait for the transport's write lock and the write both count against it;
// a response whose deadline passed before its write wrote nothing and left
// its request pending.
func TestResponseDeadlineCoversTheWholeDelivery(t *testing.T) {
	b, _, alice, _ := usersBridge(t)
	var got time.Time
	gen := b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1,
		SendWithin: func(_ *Envelope, deadline time.Time, admit func() error, wrote func()) error {
			got = deadline
			return nil
		}})
	req := publish(t, b, "box", gen, "")
	recv(t, alice)
	partial := answer("ssh-agent", "box", req.ID)
	partial.Final = false

	setVar(t, &responseTimeout, time.Hour)
	before := time.Now()
	resp, err := b.BeginResponse("alice", partial)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if err := resp.Send(); err != nil {
		t.Fatal(err)
	}
	resp.End()
	if got.Before(before.Add(time.Hour)) || got.After(after.Add(time.Hour)) {
		t.Fatalf("the write was given deadline %s, want responseTimeout after the delivery began (%s..%s)", got, before.Add(time.Hour), after.Add(time.Hour))
	}

	// With a plain write, serialized by the bridge: a deadline already spent
	// writes nothing.
	var conn counted
	gen = b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: conn.send})
	req = publish(t, b, "box", gen, "")
	recv(t, alice)
	setVar(t, &responseTimeout, 0) // spent the moment the delivery begins
	if err := b.Respond("alice", answer("ssh-agent", "box", req.ID)); !errors.Is(err, ErrResponseTimeout) {
		t.Fatalf("a response past its deadline = %v, want ErrResponseTimeout", err)
	}
	if len(conn.written) != 0 {
		t.Fatalf("a response past its deadline was written %d times", len(conn.written))
	}
	setVar(t, &responseTimeout, time.Hour)
	if err := b.Respond("alice", answer("ssh-agent", "box", req.ID)); err != nil || len(conn.written) != 1 {
		t.Fatalf("the same answer retried in time = %v (written %d), want it written", err, len(conn.written))
	}
}
