package plugin

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/source"
)

// Delivery streams on the pin: the admission Subscribe has, extended to
// SubscribeDelivery, and what a move, Sources and Close do with a delivery
// stream. Races are forced with the package's hooks, as in pin_test.go.

// subD opens m's delivery stream for user and fails on an error.
func subD(t *testing.T, reg *Registry, user string, m source.Machine) *DeliveryStream {
	t.Helper()
	s, err := reg.SubscribeDelivery(context.Background(), user, m, "serial-"+m.Name)
	if err != nil {
		t.Fatalf("SubscribeDelivery(%s, %s): %v", user, m.Name, err)
	}
	return s
}

func dClosed(s *DeliveryStream) bool {
	select {
	case <-s.Done():
		return true
	default:
		return false
	}
}

// TestDeliveryAdmission: the first machine to open a delivery stream is
// bound; another is refused naming it; the bound machine's second stream is
// a duplicate, refused before the store is asked; the stream carries its
// machine, generation, serial and a fresh id; a bus subscription of the same
// machine is admitted beside it; and Sources counts the stream as its
// machine connected.
func TestDeliveryAdmission(t *testing.T) {
	reg, _, b := pinned(t)
	s := subD(t, reg, "alice", mach(1))
	bd, _ := b.Get("alice")
	if bd.MachineID() != mach(1).ID {
		t.Fatalf("bound %s, want machine 1", bd.MachineID())
	}
	if s.Machine().MachineID != mach(1).ID || s.Machine().MachineName != mach(1).Name || s.Generation() != bd.Generation ||
		s.Serial() != "serial-machine-1" || !delivery.ValidID(s.ID()) || s.User() != "alice" {
		t.Fatalf("stream = %+v", s)
	}

	_, err := reg.SubscribeDelivery(context.Background(), "alice", mach(2), "x")
	notBound(t, "a second machine's stream", err, mach(1))

	writes := b.Writes()
	if _, err := reg.SubscribeDelivery(context.Background(), "alice", mach(1), "other-serial"); !errors.Is(err, ErrDuplicateStream) {
		t.Fatalf("the bound machine's second stream = %v, want ErrDuplicateStream", err)
	}
	if b.Writes() != writes {
		t.Fatal("a duplicate delivery stream wrote the store")
	}

	got, ok := reg.DeliveryStream("alice")
	if !ok || got.ID() != s.ID() {
		t.Fatalf("DeliveryStream(alice) = %v, %v", got, ok)
	}
	if _, ok := reg.DeliveryStream("bob"); ok {
		t.Fatal("bob has a delivery stream")
	}
	if m, ok := reg.DeliveryBound("alice"); !ok || m.MachineID != mach(1).ID || m.MachineName != mach(1).Name {
		t.Fatalf("DeliveryBound(alice) = %+v, %v", m, ok)
	}
	if _, ok := reg.DeliveryBound("bob"); ok {
		t.Fatal("bob has a bound machine")
	}
	if !reg.DeliveryCurrent("alice", mach(1).ID, bd.Generation) || reg.DeliveryCurrent("alice", mach(2).ID, bd.Generation) {
		t.Fatal("DeliveryCurrent disagrees with the binding")
	}
	if err := reg.DeliveryAnswerFrom("alice", mach(1).ID); err != nil {
		t.Fatalf("DeliveryAnswerFrom(bound) = %v", err)
	}
	notBound(t, "an answer from machine 2", reg.DeliveryAnswerFrom("alice", mach(2).ID), mach(1))
	var nb *NotBoundError
	if err := reg.DeliveryAnswerFrom("bob", mach(1).ID); !errors.As(err, &nb) || nb.Bound.ID != "" {
		t.Fatalf("an answer for bob, who has no binding = %v", err)
	}

	v, err := reg.Sources(context.Background(), "alice")
	if err != nil || !v.Delivery || fmt.Sprint(v.Connected) != fmt.Sprintf("map[%s:[]]", mach(1).ID) {
		t.Fatalf("sources with only a delivery stream = %+v, %v", v, err)
	}
	sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	v, _ = reg.Sources(context.Background(), "alice")
	if !v.Delivery || fmt.Sprint(v.Connected) != fmt.Sprintf("map[%s:[ssh-agent]]", mach(1).ID) {
		t.Fatalf("sources with a delivery stream and a subscription = %+v", v)
	}

	if !reg.UnregisterDelivery(s) || !dClosed(s) {
		t.Fatal("UnregisterDelivery did not remove and close the stream")
	}
	if reg.UnregisterDelivery(s) {
		t.Fatal("a second UnregisterDelivery removed something")
	}
	if _, ok := reg.DeliveryStream("alice"); ok {
		t.Fatal("an unregistered stream is still alice's")
	}
	v, _ = reg.Sources(context.Background(), "alice")
	if v.Delivery {
		t.Fatal("sources count an unregistered delivery stream")
	}
	// The machine opens its stream again.
	s2 := subD(t, reg, "alice", mach(1))
	if s2.ID() == s.ID() {
		t.Fatal("a new stream has the old stream's id")
	}
}

// TestDeliveryOffer: a stream takes requests while its queue has room and
// it is open, and none once it is closed.
func TestDeliveryOffer(t *testing.T) {
	reg, _, _ := pinned(t)
	s := subD(t, reg, "alice", mach(1))
	for i := range deliveryQueueLen {
		if !s.Offer(delivery.Outgoing{ID: fmt.Sprint(i)}) {
			t.Fatalf("offer %d refused with room", i)
		}
	}
	if s.Offer(delivery.Outgoing{ID: "full"}) {
		t.Fatal("a full queue took one more")
	}
	if o := <-s.Requests(); o.ID != "0" {
		t.Fatalf("first queued = %s", o.ID)
	}
	reg.UnregisterDelivery(s)
	if s.Offer(delivery.Outgoing{ID: "closed"}) {
		t.Fatal("a closed stream took a request")
	}
}

// TestDeliveryMoveClosesTheOldStream: a move of alice's binding closes her
// old machine's delivery stream (so its requests fail) with her listeners,
// and leaves bob's alone; the new machine is then admitted.
func TestDeliveryMoveClosesTheOldStream(t *testing.T) {
	reg, _, _ := pinned(t)
	a := subD(t, reg, "alice", mach(1))
	l := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	bob := subD(t, reg, "bob", mach(5))
	if _, err := reg.SubscribeDelivery(context.Background(), "alice", mach(2), ""); err == nil {
		t.Fatal("test set-up")
	}
	mv, err := reg.Move(context.Background(), "alice", mach(2).ID, "me")
	if err != nil {
		t.Fatal(err)
	}
	if !dClosed(a) || !isClosed(l) {
		t.Fatal("the move left the old machine's streams open")
	}
	if _, ok := reg.DeliveryStream("alice"); ok {
		t.Fatal("alice still has a delivery stream after the move")
	}
	if mv.View.Delivery || len(mv.View.Connected) != 0 {
		t.Fatalf("the move's view = %+v", mv.View)
	}
	if dClosed(bob) {
		t.Fatal("alice's move closed bob's stream")
	}
	if err := reg.DeliveryAnswerFrom("alice", mach(1).ID); err == nil {
		t.Fatal("the old machine's answers are still taken")
	}
	s := subD(t, reg, "alice", mach(2))
	if s.Generation() != mv.Binding.Generation {
		t.Fatalf("new stream at generation %d, want %d", s.Generation(), mv.Binding.Generation)
	}
}

// TestDeliveryAdmittedBeforeAMove: a delivery stream passes admission, its
// gate still held, while a move for the same user starts: the move waits for
// the gate, so the stream registers first, and the move then closes it.
func TestDeliveryAdmittedBeforeAMove(t *testing.T) {
	reg, _, _ := pinned(t)
	first := subD(t, reg, "alice", mach(1))
	if _, err := reg.SubscribeDelivery(context.Background(), "alice", mach(2), ""); err == nil {
		t.Fatal("test set-up")
	}
	reg.UnregisterDelivery(first)

	moveDone := make(chan error, 1)
	moveWaiting := make(chan struct{}, 1)
	setVar(t, &gateWaitHook, func() { moveWaiting <- struct{}{} })
	setVar(t, &deliveryAdmittedHook, func(string, source.Machine) {
		go func() {
			_, err := reg.Move(context.Background(), "alice", mach(2).ID, "me")
			moveDone <- err
		}()
		select {
		case <-moveWaiting:
		case err := <-moveDone:
			moveDone <- err
		}
	})
	s, err := reg.SubscribeDelivery(context.Background(), "alice", mach(1), "")
	var merr error
	within(t, "the move", func() { merr = <-moveDone })
	if merr != nil {
		t.Fatal(merr)
	}
	if err != nil {
		t.Fatalf("the stream admitted before the move was not registered before it: %v", err)
	}
	if !dClosed(s) {
		t.Fatal("the admitted stream outlived the move")
	}
	if _, ok := reg.DeliveryStream("alice"); ok {
		t.Fatal("a stream of the old machine is registered after the move")
	}
}

// TestDeliveryRegistrationRechecksTheGeneration: the binding changes between
// a stream's admission and its registration by a path that does not take the
// user's gate: the registration's recheck refuses it naming the machine bound
// now, and nothing is registered.
func TestDeliveryRegistrationRechecksTheGeneration(t *testing.T) {
	reg, _, b := pinned(t)
	sub(t, reg, uk("alice", "docker"), mach(1))
	if _, err := reg.SubscribeDelivery(context.Background(), "alice", mach(2), ""); err == nil {
		t.Fatal("test set-up")
	}
	hooked := false
	setVar(t, &deliveryAdmittedHook, func(string, source.Machine) {
		hooked = true
		if _, err := b.Use("alice", mach(2).ID, "start-up"); err != nil {
			t.Errorf("test set-up: %v", err)
		}
	})
	_, err := reg.SubscribeDelivery(context.Background(), "alice", mach(1), "")
	if !hooked {
		t.Fatal("test set-up: the stream never reached its hook")
	}
	notBound(t, "a stream whose binding moved before it registered", err, mach(2))
	if _, ok := reg.DeliveryStream("alice"); ok {
		t.Fatal("a stream was registered under a binding that is no longer current")
	}
}

// TestDeliveryTwoFirstStreams: two machines of a user with no binding open
// their delivery streams at once; the gate serializes them: the first is
// bound and registered, the second refused naming it.
func TestDeliveryTwoFirstStreams(t *testing.T) {
	reg, _, b := pinned(t)
	second := make(chan error, 1)
	waiting := make(chan struct{}, 1)
	setVar(t, &gateWaitHook, func() { waiting <- struct{}{} })
	setVar(t, &deliveryAdmittedHook, func(_ string, m source.Machine) {
		if m.ID != mach(1).ID {
			return
		}
		go func() {
			_, err := reg.SubscribeDelivery(context.Background(), "alice", mach(2), "")
			second <- err
		}()
		select {
		case <-waiting:
		case err := <-second:
			second <- err
		}
	})
	subD(t, reg, "alice", mach(1))
	var err error
	within(t, "the second stream", func() { err = <-second })
	notBound(t, "the second machine", err, mach(1))
	if bd, _ := b.Get("alice"); bd.MachineID() != mach(1).ID {
		t.Fatalf("bound %s", bd.MachineID())
	}
	if s, ok := reg.DeliveryStream("alice"); !ok || s.Machine().MachineID != mach(1).ID {
		t.Fatal("the first machine's stream is not the one registered")
	}
}

// TestDeliveryAndSubscribeShareTheGate: a bus subscription of machine 1 and
// the delivery stream of machine 2, both first for the user, race: one binds,
// the other is refused naming it, whichever kind it is.
func TestDeliveryAndSubscribeShareTheGate(t *testing.T) {
	reg, _, b := pinned(t)
	second := make(chan error, 1)
	waiting := make(chan struct{}, 1)
	setVar(t, &gateWaitHook, func() { waiting <- struct{}{} })
	setVar(t, &subscribeAdmittedHook, func(_ ListenerKey, m source.Machine) {
		go func() {
			_, err := reg.SubscribeDelivery(context.Background(), "alice", mach(2), "")
			second <- err
		}()
		select {
		case <-waiting:
		case err := <-second:
			second <- err
		}
	})
	sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	var err error
	within(t, "the delivery stream", func() { err = <-second })
	notBound(t, "machine 2's delivery stream", err, mach(1))
	if bd, _ := b.Get("alice"); bd.MachineID() != mach(1).ID {
		t.Fatalf("bound %s", bd.MachineID())
	}
}

// TestDeliveryStoreFailureRegistersNothing: an admission whose store write
// fails wraps ErrBindingStore, binds nothing and registers nothing.
func TestDeliveryStoreFailureRegistersNothing(t *testing.T) {
	reg, _, b := pinned(t)
	b.FailWrites(errors.New("disk full"))
	if _, err := reg.SubscribeDelivery(context.Background(), "alice", mach(1), ""); !errors.Is(err, ErrBindingStore) {
		t.Fatalf("SubscribeDelivery with a failing store = %v, want ErrBindingStore", err)
	}
	if _, ok := reg.DeliveryStream("alice"); ok {
		t.Fatal("a failed admission registered a stream")
	}
	if _, ok := b.Get("alice"); ok {
		t.Fatal("a failed admission bound a machine")
	}
	b.FailWrites(nil)
	subD(t, reg, "alice", mach(1))
}

// TestDeliveryAdmissionRateLimit: delivery admissions that write the store
// spend the same per-user budget as bus subscriptions.
func TestDeliveryAdmissionRateLimit(t *testing.T) {
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	setVar(t, &pinNow, func() time.Time { return now })
	setVar(t, &admitBurst, 2)
	setVar(t, &admitRefill, time.Minute)
	reg, _, b := pinned(t)

	sub(t, reg, uk("alice", "ssh-agent"), mach(1)) // writes: 1
	if _, err := reg.SubscribeDelivery(context.Background(), "alice", mach(2), ""); err == nil {
		t.Fatal("test set-up") // writes (a new machine, refused): 2
	}
	writes := b.Writes()
	var th *ThrottledError
	if _, err := reg.SubscribeDelivery(context.Background(), "alice", mach(3), ""); !errors.As(err, &th) {
		t.Fatalf("a third writing admission = %v, want ThrottledError", err)
	}
	if b.Writes() != writes {
		t.Fatal("a throttled admission wrote the store")
	}
	// The bound machine, unchanged, writes nothing and is admitted.
	subD(t, reg, "alice", mach(1))
	now = now.Add(time.Minute)
	if _, err := reg.SubscribeDelivery(context.Background(), "alice", mach(3), ""); errors.As(err, &th) {
		t.Fatalf("after the refill = %v", err)
	}
}

// TestDeliveryCloseEndsStreams: Close closes every delivery stream and
// refuses every later one.
func TestDeliveryCloseEndsStreams(t *testing.T) {
	reg, _, _ := pinned(t)
	a := subD(t, reg, "alice", mach(1))
	bb := subD(t, reg, "bob", mach(2))
	reg.Close()
	if !dClosed(a) || !dClosed(bb) {
		t.Fatal("Close left a delivery stream open")
	}
	if _, err := reg.SubscribeDelivery(context.Background(), "alice", mach(1), ""); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("SubscribeDelivery after Close = %v", err)
	}
}

// TestDeliveryUnpinned: a registry with no source bindings admits no stream
// and binds nobody.
func TestDeliveryUnpinned(t *testing.T) {
	reg := NewRegistry()
	if _, err := reg.SubscribeDelivery(context.Background(), "alice", mach(1), ""); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("SubscribeDelivery unpinned = %v", err)
	}
	if _, ok := reg.DeliveryBound("alice"); ok {
		t.Fatal("an unpinned registry has a bound machine")
	}
	if _, ok := reg.DeliveryStream("alice"); ok {
		t.Fatal("an unpinned registry has a stream")
	}
	if reg.DeliveryCurrent("alice", mach(1).ID, 1) {
		t.Fatal("an unpinned registry has a current binding")
	}
	if err := reg.DeliveryAnswerFrom("alice", mach(1).ID); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("DeliveryAnswerFrom unpinned = %v", err)
	}
}

// The registry is the delivery Pin.
var _ delivery.Pin = (*Registry)(nil)
