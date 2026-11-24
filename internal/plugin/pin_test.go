package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/source"
	"github.com/charliek/shed/internal/source/fake"
)

// The pin on the bus: admission by machine per user, stamped requests,
// moves, and the per-user gate. Races are forced with the package's hooks.

// mach is the n-th test machine.
func mach(n int) source.Machine {
	return source.Machine{ID: fmt.Sprintf("%032x", n), Name: fmt.Sprintf("machine-%d", n), KeyFingerprint: fmt.Sprintf("SHA256:key%d", n)}
}

// pinned is a registry with in-memory source bindings, a bridge that routes
// by user, and the bindings.
func pinned(t *testing.T) (*Registry, *Bridge, *fake.Bindings) {
	t.Helper()
	reg := NewRegistry()
	b := fake.New(source.Options{})
	reg.SetBindings(b)
	return reg, NewBridge(reg, true), b
}

// sub subscribes m for key and fails on an error.
func sub(t *testing.T, reg *Registry, key ListenerKey, m source.Machine) *Listener {
	t.Helper()
	l, err := reg.Subscribe(context.Background(), key, m)
	if err != nil {
		t.Fatalf("Subscribe(%+v, %s): %v", key, m.Name, err)
	}
	return l
}

// notBound asserts err is a refusal naming want as the bound machine.
func notBound(t *testing.T, what string, err error, want source.Machine) {
	t.Helper()
	var nb *NotBoundError
	if !errors.As(err, &nb) {
		t.Fatalf("%s = %v, want a NotBoundError naming %s", what, err, want.Name)
	}
	if nb.Bound.ID != want.ID || nb.Bound.Name != want.Name {
		t.Fatalf("%s names %+v, want %s (%s)", what, nb.Bound, want.ID, want.Name)
	}
}

// boxShed registers shed name for user with a recorder of what it is
// written, and returns its connection generation.
func boxShed(b *Bridge, name, user string, written *[]*Envelope, mu *sync.Mutex) uint64 {
	return b.RegisterShed(name, &ShedConn{User: user, CreatedAt: t1, Send: func(env *Envelope) error {
		mu.Lock()
		defer mu.Unlock()
		*written = append(*written, env)
		return nil
	}})
}

// pendingOf counts the registry's pending requests of user.
func pendingOf(reg *Registry, user string) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	n := 0
	for _, m := range reg.pending {
		if m.user == user {
			n++
		}
	}
	return n
}

// count is how many retained requests l's registration decided to
// re-deliver (decided synchronously, before Subscribe or Register returns).
func (w *redeliveryWatch) count(t *testing.T, l *Listener) int {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	n, ok := w.counts[l]
	if !ok {
		t.Fatal("the registration did not report what it re-delivers")
	}
	return n
}

func isClosed(l *Listener) bool {
	select {
	case <-l.Done:
		return true
	default:
		return false
	}
}

func TestPinAdmission(t *testing.T) {
	reg, _, b := pinned(t)
	a, other := mach(1), mach(2)

	// The first machine is bound and may hold every namespace.
	for _, ns := range []string{"ssh-agent", "docker-credential", "aws-credentials"} {
		l := sub(t, reg, uk("alice", ns), a)
		if l.MachineID != a.ID || l.MachineName != a.Name || l.Generation == 0 {
			t.Fatalf("listener stamp = %s %q %d", l.MachineID, l.MachineName, l.Generation)
		}
	}
	if bd, ok := b.Get("alice"); !ok || bd.MachineID() != a.ID {
		t.Fatalf("binding = %+v, %v", bd, ok)
	}
	// Another machine is refused, naming the bound one, and registers nothing.
	_, err := reg.Subscribe(context.Background(), uk("alice", "other-ns"), other)
	notBound(t, "a second machine", err, a)
	if _, ok := reg.Get(uk("alice", "other-ns")); ok {
		t.Fatal("a refused machine was registered")
	}
	// The bound machine already holding a namespace is a duplicate, never a
	// replacement.
	held, _ := reg.Get(uk("alice", "ssh-agent"))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), a); !errors.Is(err, ErrDuplicateStream) {
		t.Fatalf("the same machine twice = %v, want ErrDuplicateStream", err)
	}
	if now, _ := reg.Get(uk("alice", "ssh-agent")); now != held || isClosed(held) {
		t.Fatal("a duplicate subscribe replaced or closed the live stream")
	}
	// Once its stream ends, the same machine is admitted again.
	reg.Unregister(held.Key(), held.Token)
	sub(t, reg, uk("alice", "ssh-agent"), a)
	// A registry with bindings registers nothing without a machine.
	if _, err := reg.Register(uk("alice", "plain")); !errors.Is(err, ErrMachineRequired) {
		t.Fatalf("Register on a pinned registry = %v, want ErrMachineRequired", err)
	}
	if _, err := NewRegistry().Subscribe(context.Background(), uk("alice", "x"), a); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("Subscribe with no bindings = %v", err)
	}
	got := reg.Connected("alice")
	if len(got) != 1 || got[0].ID != a.ID || got[0].Name != a.Name ||
		fmt.Sprint(got[0].Namespaces) != "[aws-credentials docker-credential ssh-agent]" {
		t.Fatalf("Connected = %+v", got)
	}
	if got := reg.Connected("bob"); len(got) != 0 {
		t.Fatalf("Connected(bob) = %+v", got)
	}
}

// TestPinTwoUsersEachBindTheirOwn: two users each bind their own machine and
// both hold the same namespace at once; a user's second machine is refused
// naming that user's machine, never the other user's.
func TestPinTwoUsersEachBindTheirOwn(t *testing.T) {
	reg, _, _ := pinned(t)
	sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	sub(t, reg, uk("bob", "ssh-agent"), mach(2))
	_, err := reg.Subscribe(context.Background(), uk("alice", "docker"), mach(3))
	notBound(t, "alice's second machine", err, mach(1))
	_, err = reg.Subscribe(context.Background(), uk("bob", "docker"), mach(1))
	notBound(t, "alice's machine presented by bob", err, mach(2))
	for _, u := range []string{"alice", "bob"} {
		if _, ok := reg.Get(uk(u, "ssh-agent")); !ok {
			t.Fatalf("%s's listener is gone", u)
		}
	}
}

// TestPinRequestsStayWithTheirMachine: a request is stamped with the machine
// it was delivered to. The same machine reconnecting gets it again and can
// answer it; after a move another machine gets none of it, and the old
// machine's answer is refused as not bound.
func TestPinRequestsStayWithTheirMachine(t *testing.T) {
	watch := watchRedelivery(t)
	reg, br, _ := pinned(t)
	var written []*Envelope
	var mu sync.Mutex
	gen := boxShed(br, "box", "alice", &written, &mu)
	a, other := mach(1), mach(2)
	l := sub(t, reg, uk("alice", "ssh-agent"), a)
	// The other machine presents itself (refused, remembered for a move).
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), other); err == nil {
		t.Fatal("test set-up: the other machine was admitted")
	}
	req := publish(t, br, "box", gen, "")
	if got := recv(t, l); got.ID != req.ID {
		t.Fatalf("delivered %s, want %s", got.ID, req.ID)
	}
	reg.Unregister(l.Key(), l.Token)

	// The same machine reconnects: its in-flight request is re-sent.
	l2 := sub(t, reg, uk("alice", "ssh-agent"), a)
	if got := recv(t, l2); got.ID != req.ID {
		t.Fatalf("re-delivered %s, want %s", got.ID, req.ID)
	}
	req2 := publish(t, br, "box", gen, "")
	recv(t, l2)
	reg.Unregister(l2.Key(), l2.Token)

	// Moved to the other machine: it subscribes and is sent nothing of the
	// old machine's.
	if _, err := reg.Move(context.Background(), "alice", other.ID, "SHA256:mover"); err != nil {
		t.Fatal(err)
	}
	l3 := sub(t, reg, uk("alice", "ssh-agent"), other)
	// Conclusive, not a look at the queue alone: its registration decided to
	// re-deliver nothing, and whatever re-delivery it started has ended
	// before the queue is looked at.
	if n := watch.count(t, l3); n != 0 {
		t.Fatalf("the new machine's registration re-delivers %d of the old machine's requests", n)
	}
	watch.ended(t, l3)
	noMessage(t, l3)
	if n := pendingOf(reg, "alice"); n != 0 {
		t.Fatalf("alice has %d pending requests after the move, want the old machine's dropped", n)
	}
	// The old machine's answer is refused as not bound; the new machine's
	// answer to the old request matches nothing.
	_, err := br.BeginResponseFrom("alice", a.ID, answer("ssh-agent", "box", req2.ID))
	notBound(t, "the old machine's answer", err, other)
	if _, err := br.BeginResponseFrom("alice", other.ID, answer("ssh-agent", "box", req2.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("the new machine answering the old machine's request = %v, want ErrUnmatched", err)
	}
	if len(written) != 0 {
		t.Fatalf("the shed was written %d answers", len(written))
	}
	// New requests go to the new machine, which may answer them.
	req3 := publish(t, br, "box", gen, "")
	if got := recv(t, l3); got.ID != req3.ID {
		t.Fatalf("the new machine got %s, want %s", got.ID, req3.ID)
	}
	if err := respondFrom(br, "alice", other.ID, answer("ssh-agent", "box", req3.ID)); err != nil {
		t.Fatalf("the new machine's answer: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("written %d, want 1", len(written))
	}
}

// respondFrom is Respond for an answer from machine.
func respondFrom(b *Bridge, user, machine string, env *Envelope) error {
	resp, err := b.BeginResponseFrom(user, machine, env)
	if err != nil {
		return err
	}
	defer resp.End()
	return resp.Send()
}

// TestPinRefusedAnswerLogsTheMachine: an answer from a machine that is not
// the user's source is logged with the machine's name as the store remembers
// it (the answer itself carries only the id), and a machine the store has no
// record of is logged by its id alone, never with an empty name.
func TestPinRefusedAnswerLogsTheMachine(t *testing.T) {
	reg, br, _ := pinned(t)
	old, cur, stranger := mach(1), mach(2), mach(3)
	sub(t, reg, uk("alice", "ssh-agent"), old)
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), cur); err == nil {
		t.Fatal("test set-up: the second machine was admitted")
	}
	if _, err := reg.Move(context.Background(), "alice", cur.ID, "SHA256:mover"); err != nil {
		t.Fatal(err)
	}
	out := logged(t)

	_, err := br.BeginResponseFrom("alice", old.ID, answer("ssh-agent", "box", "req"))
	notBound(t, "the old machine's answer", err, cur)
	want := fmt.Sprintf(`credential bus: refused an answer from machine %s ("machine-1") for user "alice": the user's credential source is machine %s ("machine-2")`, old.ID, cur.ID)
	if !strings.Contains(out.String(), want) {
		t.Fatalf("log = %q, want a line containing %q", out.String(), want)
	}

	_, err = br.BeginResponseFrom("alice", stranger.ID, answer("ssh-agent", "box", "req"))
	notBound(t, "an unknown machine's answer", err, cur)
	want = fmt.Sprintf(`refused an answer from machine %s for user "alice"`, stranger.ID)
	if !strings.Contains(out.String(), want) {
		t.Fatalf("log = %q, want a line containing %q", out.String(), want)
	}
	if strings.Contains(out.String(), `("")`) {
		t.Fatalf("a refusal was logged with an empty machine name: %q", out.String())
	}
}

// TestPinMoveLeavesOtherUsersAlone: a move of alice's binding closes her old
// machine's listeners and drops its requests; bob's listener and request
// are untouched and he can still answer.
func TestPinMoveLeavesOtherUsersAlone(t *testing.T) {
	reg, br, _ := pinned(t)
	var written []*Envelope
	var mu sync.Mutex
	boxGen := boxShed(br, "box", "alice", &written, &mu)
	bobsGen := boxShed(br, "bobs", "bob", &written, &mu)
	aliceL := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	aliceL2 := sub(t, reg, uk("alice", "docker"), mach(1))
	bobL := sub(t, reg, uk("bob", "ssh-agent"), mach(9))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(2)); err == nil {
		t.Fatal("test set-up")
	}
	publish(t, br, "box", boxGen, "")
	bobReq := publish(t, br, "bobs", bobsGen, "")
	recv(t, aliceL)
	recv(t, bobL)

	mv, err := reg.Move(context.Background(), "alice", mach(2).ID, "SHA256:mover")
	if err != nil || !mv.Moved || mv.From.ID != mach(1).ID || mv.To.ID != mach(2).ID {
		t.Fatalf("Move = %+v, %v", mv, err)
	}
	if !isClosed(aliceL) || !isClosed(aliceL2) {
		t.Fatal("the old machine's listeners were left open")
	}
	if _, ok := reg.Get(uk("alice", "ssh-agent")); ok {
		t.Fatal("the old machine's listener is still registered")
	}
	if pendingOf(reg, "alice") != 0 {
		t.Fatal("the old machine's requests were kept")
	}
	if isClosed(bobL) || pendingOf(reg, "bob") != 1 {
		t.Fatalf("bob's listener closed %v, pending %d; want untouched", isClosed(bobL), pendingOf(reg, "bob"))
	}
	if err := respondFrom(br, "bob", mach(9).ID, answer("ssh-agent", "bobs", bobReq.ID)); err != nil {
		t.Fatalf("bob's answer after alice's move: %v", err)
	}
	// Moving to the machine already bound changes nothing.
	l := sub(t, reg, uk("alice", "ssh-agent"), mach(2))
	if mv, err := reg.Move(context.Background(), "alice", mach(2).ID, "me"); err != nil || mv.Moved {
		t.Fatalf("no-op move = %+v, %v", mv, err)
	}
	if isClosed(l) {
		t.Fatal("a no-op move closed the bound machine's listener")
	}
	if _, err := reg.Move(context.Background(), "alice", mach(7).ID, "me"); !errors.Is(err, source.ErrUnknownMachine) {
		t.Fatalf("move to an unknown machine = %v", err)
	}
}

// TestPinSubscribeAdmittedBeforeAMove: a subscribe that has passed
// admission, its gate still held, while a move for the same user starts:
// the move waits for the gate, so the subscribe registers first, and the
// move then closes it. No listener of the old machine is left.
//
// This pins the gate: were the move not to wait for it, the move would run
// to its end inside the hook and the subscribe would then be refused by its
// registration's generation recheck (safe, but not this order).
// TestPinRegistrationRechecksTheGeneration pins the recheck alone.
func TestPinSubscribeAdmittedBeforeAMove(t *testing.T) {
	reg, _, _ := pinned(t)
	if _, err := reg.Subscribe(context.Background(), uk("alice", "docker"), mach(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Subscribe(context.Background(), uk("alice", "docker"), mach(2)); err == nil {
		t.Fatal("test set-up")
	}
	moveDone := make(chan error, 1)
	moveWaiting := make(chan struct{}, 1)
	setVar(t, &gateWaitHook, func() { moveWaiting <- struct{}{} })
	setVar(t, &subscribeAdmittedHook, func(key ListenerKey, _ source.Machine) {
		if key.Namespace != "ssh-agent" {
			return
		}
		go func() {
			_, err := reg.Move(context.Background(), "alice", mach(2).ID, "me")
			moveDone <- err
		}()
		// Until the move is waiting for the gate, or (were it not to take the
		// gate) has finished.
		select {
		case <-moveWaiting:
		case err := <-moveDone:
			moveDone <- err
		}
	})
	l, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(1))
	var serr error
	within(t, "the move", func() { serr = <-moveDone })
	if serr != nil {
		t.Fatal(serr)
	}
	if err != nil {
		t.Fatalf("the subscribe admitted before the move was not registered before it: %v", err)
	}
	if !isClosed(l) {
		t.Fatal("the admitted subscribe's listener outlived the move")
	}
	for _, ns := range []string{"ssh-agent", "docker"} {
		if got, ok := reg.Get(uk("alice", ns)); ok && got.MachineID != mach(2).ID {
			t.Fatalf("a listener of the old machine is registered for %s after the move", ns)
		}
	}
}

// TestPinRegistrationRechecksTheGeneration: the binding changes between a
// subscribe's admission and its registration by a path that does not take
// the user's gate (the store changed directly, as a start-up step changes
// it): the registration's recheck of the generation, under r.mu, refuses it
// naming the machine bound now, and nothing is registered. With the gate
// intact, only the recheck can refuse it.
func TestPinRegistrationRechecksTheGeneration(t *testing.T) {
	reg, _, b := pinned(t)
	sub(t, reg, uk("alice", "docker"), mach(1))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "docker"), mach(2)); err == nil {
		t.Fatal("test set-up")
	}
	hooked := false
	setVar(t, &subscribeAdmittedHook, func(key ListenerKey, _ source.Machine) {
		if key.Namespace != "ssh-agent" {
			return
		}
		hooked = true
		if _, err := b.Use("alice", mach(2).ID, "start-up"); err != nil {
			t.Errorf("test set-up: %v", err)
		}
	})
	_, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(1))
	if !hooked {
		t.Fatal("test set-up: the subscribe never reached its hook")
	}
	notBound(t, "a subscribe whose binding moved before it registered", err, mach(2))
	if _, ok := reg.Get(uk("alice", "ssh-agent")); ok {
		t.Fatal("a subscribe was registered under a binding that is no longer current")
	}
}

// TestPinAnswerInFlightAcrossAMove: an answer matched under the gate, a move
// that then completes before the answer is written: the write is refused as
// not bound, nothing reaches the shed, and the request is gone.
func TestPinAnswerInFlightAcrossAMove(t *testing.T) {
	reg, br, _ := pinned(t)
	var written []*Envelope
	var mu sync.Mutex
	gen := boxShed(br, "box", "alice", &written, &mu)
	l := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(2)); err == nil {
		t.Fatal("test set-up")
	}
	req := publish(t, br, "box", gen, "")
	recv(t, l)

	moveDone := make(chan error, 1)
	moveWaiting := make(chan struct{}, 1)
	setVar(t, &gateWaitHook, func() { moveWaiting <- struct{}{} })
	setVar(t, &respondGateHook, func(string) {
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
	resp, err := br.BeginResponseFrom("alice", mach(1).ID, answer("ssh-agent", "box", req.ID))
	if err != nil {
		t.Fatalf("the answer was not matched before the move: %v", err)
	}
	within(t, "the move", func() {
		if err := <-moveDone; err != nil {
			t.Error(err)
		}
	})
	err = resp.Send()
	resp.End()
	notBound(t, "the answer written after the move", err, mach(2))
	if len(written) != 0 {
		t.Fatalf("an answer from the old machine was written after the move: %+v", written)
	}
	if pendingOf(reg, "alice") != 0 {
		t.Fatal("the request outlived the move")
	}
}

// TestPinAnswerWrittenWhileAMoveCleansUp: an answer matched before a move
// whose write begins after the store has moved the binding but before the
// move has dropped the old machine's requests (forced with the move's hook):
// the write is refused as not bound, by the stamp check alone.
func TestPinAnswerWrittenWhileAMoveCleansUp(t *testing.T) {
	reg, br, _ := pinned(t)
	var written []*Envelope
	var mu sync.Mutex
	gen := boxShed(br, "box", "alice", &written, &mu)
	l := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(2)); err == nil {
		t.Fatal("test set-up")
	}
	req := publish(t, br, "box", gen, "")
	recv(t, l)
	resp, err := br.BeginResponseFrom("alice", mach(1).ID, answer("ssh-agent", "box", req.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.End()
	var sendErr error
	hooked := false
	setVar(t, &moveStoredHook, func(string) {
		hooked = true
		if pendingOf(reg, "alice") != 1 {
			t.Error("test set-up: the request was dropped before the hook ran")
		}
		sendErr = resp.Send()
	})
	if _, err := reg.Move(context.Background(), "alice", mach(2).ID, "me"); err != nil {
		t.Fatal(err)
	}
	if !hooked {
		t.Fatal("test set-up: the move never reached its hook")
	}
	notBound(t, "an answer written between the move's store write and its clean-up", sendErr, mach(2))
	if len(written) != 0 {
		t.Fatalf("an answer from the old machine was written after the store moved the binding: %+v", written)
	}
}

// TestPinTwoFirstSubscribes: two machines' first subscribes for one user,
// the second arriving while the first holds the gate after its admission:
// exactly one is bound and registered, and the other is refused naming it.
func TestPinTwoFirstSubscribes(t *testing.T) {
	reg, _, b := pinned(t)
	second := make(chan error, 1)
	waiting := make(chan struct{}, 1)
	setVar(t, &gateWaitHook, func() { waiting <- struct{}{} })
	setVar(t, &subscribeAdmittedHook, func(key ListenerKey, m source.Machine) {
		if m.ID != mach(1).ID {
			return
		}
		go func() {
			_, err := reg.Subscribe(context.Background(), uk("alice", "docker"), mach(2))
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
	within(t, "the second subscribe", func() { err = <-second })
	notBound(t, "the second machine", err, mach(1))
	if bd, _ := b.Get("alice"); bd.MachineID() != mach(1).ID {
		t.Fatalf("bound %s", bd.MachineID())
	}
	if _, ok := reg.Get(uk("alice", "docker")); ok {
		t.Fatal("the second machine was registered")
	}
}

// TestPinStoreFailureRegistersNothing: an admission whose store write fails
// is an error wrapping ErrBindingStore, binds nothing and registers nothing;
// once the store works the machine is admitted.
func TestPinStoreFailureRegistersNothing(t *testing.T) {
	reg, _, b := pinned(t)
	b.FailWrites(errors.New("disk full"))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(1)); !errors.Is(err, ErrBindingStore) {
		t.Fatalf("Subscribe with a failing store = %v, want ErrBindingStore", err)
	}
	if _, ok := reg.Get(uk("alice", "ssh-agent")); ok {
		t.Fatal("a failed admission registered a listener")
	}
	if _, ok := b.Get("alice"); ok {
		t.Fatal("a failed admission bound a machine")
	}
	b.FailWrites(nil)
	sub(t, reg, uk("alice", "ssh-agent"), mach(1))
}

// TestPinAdmissionRateLimit: per user, admissions that write the store are
// limited; one that writes nothing (the bound machine again) is not; another
// user has a bucket of their own; the limit refills with time.
func TestPinAdmissionRateLimit(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	setVar(t, &pinNow, func() time.Time { return now })
	setVar(t, &admitBurst, 2)
	setVar(t, &admitRefill, time.Minute)
	reg, _, b := pinned(t)

	sub(t, reg, uk("alice", "ssh-agent"), mach(1)) // writes: 1
	if _, err := reg.Subscribe(context.Background(), uk("alice", "x"), mach(2)); err == nil {
		t.Fatal("test set-up") // writes (a new machine, refused): 2
	}
	writes := b.Writes()
	_, err := reg.Subscribe(context.Background(), uk("alice", "x"), mach(3))
	var th *ThrottledError
	if !errors.As(err, &th) || th.RetryAfter <= 0 || th.RetryAfter > time.Minute {
		t.Fatalf("a third writing admission = %v, want ThrottledError within a minute", err)
	}
	if b.Writes() != writes {
		t.Fatal("a throttled admission wrote the store")
	}
	// A renamed machine writes too.
	renamed := mach(1)
	renamed.Name = "renamed"
	if _, err := reg.Subscribe(context.Background(), uk("alice", "y"), renamed); !errors.As(err, &th) {
		t.Fatalf("a renamed machine with no budget = %v, want ThrottledError", err)
	}
	// The bound machine, unchanged, writes nothing and is admitted.
	sub(t, reg, uk("alice", "docker"), mach(1))
	// Bob is not held back by alice.
	sub(t, reg, uk("bob", "ssh-agent"), mach(5))
	// After the refill, one more.
	now = now.Add(time.Minute)
	if _, err := reg.Subscribe(context.Background(), uk("alice", "x"), mach(3)); errors.As(err, &th) {
		t.Fatalf("after the refill = %v", err)
	}
	if _, err := reg.Subscribe(context.Background(), uk("alice", "x"), mach(4)); !errors.As(err, &th) {
		t.Fatalf("after spending the refill = %v, want ThrottledError", err)
	}
}

// TestPinCloseEndsEveryStream: Close removes every listener (their Done
// closes) and refuses every later subscribe.
func TestPinCloseEndsEveryStream(t *testing.T) {
	reg, _, _ := pinned(t)
	a := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	b := sub(t, reg, uk("bob", "ssh-agent"), mach(2))
	reg.Close()
	if !isClosed(a) || !isClosed(b) {
		t.Fatal("Close left a listener open")
	}
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(1)); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("Subscribe after Close = %v", err)
	}
}

// TestPinUnpinnedAnswerNeedsNoMachine: with no source bindings (the bus on
// its own) answers carry no machine, as before the pin; one that names a
// machine matches nothing.
func TestPinUnpinnedAnswerNeedsNoMachine(t *testing.T) {
	reg := NewRegistry()
	br := NewBridge(reg, false)
	l, _ := reg.Register(k("op"))
	gen := br.RegisterShed("dev", &ShedConn{Send: func(*Envelope) error { return nil }})
	req := NewEnvelope("op", MessageTypeRequest, json.RawMessage(`{}`))
	if err := br.PublishToHost(context.Background(), "dev", gen, req); err != nil {
		t.Fatal(err)
	}
	recv(t, l)
	if _, err := br.BeginResponseFrom("", mach(1).ID, answer("op", "dev", req.ID)); !errors.Is(err, ErrUnmatched) {
		t.Fatalf("an answer naming a machine on an unpinned bus = %v", err)
	}
	if err := br.Respond("", answer("op", "dev", req.ID)); err != nil {
		t.Fatal(err)
	}
}

// logged captures the standard logger's output for the rest of the test.
func logged(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// recordedKey is the key fingerprint the store remembers for machine id of
// user.
func recordedKey(t *testing.T, b *fake.Bindings, user, id string) string {
	t.Helper()
	for _, m := range b.Machines(user) {
		if m.ID == id {
			return m.KeyFingerprint
		}
	}
	t.Fatalf("machine %s of %s is not remembered", id, user)
	return ""
}

// TestPinDuplicateRetriesWriteNothing: one machine whose daemon and desktop
// app enrolled with different keys, each holding one namespace, each
// retrying the other's: every retry is a duplicate refused before the store
// is asked, so the store is never written, the recorded key never flips and
// the user's admission budget is never spent; a useful subscribe from either
// process afterwards is admitted, and its change of key is recorded and
// logged.
func TestPinDuplicateRetriesWriteNothing(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	setVar(t, &pinNow, func() time.Time { return now })
	setVar(t, &admitBurst, 4)
	setVar(t, &admitRefill, time.Hour)
	reg, _, b := pinned(t)
	daemon, desktop := mach(1), mach(1)
	daemon.KeyFingerprint, desktop.KeyFingerprint = "SHA256:daemon", "SHA256:desktop"

	held := map[string]*Listener{
		"y": sub(t, reg, uk("alice", "y"), daemon),  // binds: a write
		"x": sub(t, reg, uk("alice", "x"), desktop), // a changed key: a write
	}
	writes := b.Writes()
	for i := 0; i < 50; i++ {
		for _, retry := range []struct {
			ns string
			m  source.Machine
		}{{"x", daemon}, {"y", desktop}} {
			_, err := reg.Subscribe(context.Background(), uk("alice", retry.ns), retry.m)
			if got := b.Writes(); got != writes {
				t.Fatalf("retry %d of %q by key %s wrote the store (%d writes since the set-up)", i, retry.ns, retry.m.KeyFingerprint, got-writes)
			}
			if !errors.Is(err, ErrDuplicateStream) {
				t.Fatalf("retry %d of %q by key %s = %v, want ErrDuplicateStream", i, retry.ns, retry.m.KeyFingerprint, err)
			}
		}
	}
	if got := recordedKey(t, b, "alice", daemon.ID); got != desktop.KeyFingerprint {
		t.Fatalf("recorded key = %s, want the last accepted subscription's (%s)", got, desktop.KeyFingerprint)
	}
	for ns, l := range held {
		if now, ok := reg.Get(uk("alice", ns)); !ok || now != l || isClosed(l) {
			t.Fatalf("a duplicate retry replaced or closed the %q stream", ns)
		}
	}

	// Useful subscribes, a namespace neither holds, from each process: both
	// admitted (the budget is intact), each change of key recorded and logged.
	out := logged(t)
	sub(t, reg, uk("alice", "z"), daemon)
	if got := recordedKey(t, b, "alice", daemon.ID); got != daemon.KeyFingerprint {
		t.Fatalf("recorded key after the daemon's useful subscribe = %s", got)
	}
	sub(t, reg, uk("alice", "w"), desktop)
	if got := recordedKey(t, b, "alice", daemon.ID); got != desktop.KeyFingerprint {
		t.Fatalf("recorded key after the desktop's useful subscribe = %s", got)
	}
	if got := b.Writes(); got != writes+2 {
		t.Fatalf("two changes of key wrote the store %d times, want 2", got-writes)
	}
	for _, line := range []string{
		`now connects with key "SHA256:daemon" (was "SHA256:desktop")`,
		`now connects with key "SHA256:desktop" (was "SHA256:daemon")`,
	} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("the change of key was not logged (%s): %s", line, out.String())
		}
	}
}

// recordedMachine is machine id of user as the store remembers it.
func recordedMachine(t *testing.T, b source.Bindings, user, id string) source.Machine {
	t.Helper()
	for _, m := range b.Machines(user) {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("machine %s of %s is not remembered", id, user)
	return source.Machine{}
}

// TestPinRefusedMachineRetriesWriteNothing: a machine that is not the
// source, whose two processes (one machine id) present two names and two
// keys, retrying several namespaces many times: after its first refusal (the
// one write that remembers it) no retry writes the store, spends the user's
// budget or is logged again, every one is refused naming the bound machine,
// and the record keeps what it presented first. A brand-new machine is still
// remembered once, and that still costs budget.
func TestPinRefusedMachineRetriesWriteNothing(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	setVar(t, &pinNow, func() time.Time { return now })
	setVar(t, &admitBurst, 2)
	setVar(t, &admitRefill, time.Hour)
	reg, _, b := pinned(t)
	bound := mach(1)
	sub(t, reg, uk("alice", "ssh-agent"), bound) // binds: a write and a token

	daemon, desktop := mach(2), mach(2)
	daemon.Name, daemon.KeyFingerprint = "laptop", "SHA256:daemon"
	desktop.Name, desktop.KeyFingerprint = "laptop (app)", "SHA256:desktop"
	writes := b.Writes()
	_, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), daemon)
	notBound(t, "the first sight of a refused machine", err, bound) // a write and the last token
	if got := b.Writes(); got != writes+1 {
		t.Fatalf("the first sight of a refused machine wrote the store %d times, want 1", got-writes)
	}

	out := logged(t)
	writes = b.Writes()
	for i := 0; i < 50; i++ {
		for _, ns := range []string{"ssh-agent", "docker-credential", "aws-credentials"} {
			for _, m := range []source.Machine{desktop, daemon} {
				_, err := reg.Subscribe(context.Background(), uk("alice", ns), m)
				if got := b.Writes(); got != writes {
					t.Fatalf("retry %d of %q by key %s wrote the store (%d writes since its first sight)", i, ns, m.KeyFingerprint, got-writes)
				}
				notBound(t, fmt.Sprintf("retry %d of %q by key %s", i, ns, m.KeyFingerprint), err, bound)
				if _, ok := reg.Get(uk("alice", ns)); ok && ns != "ssh-agent" {
					t.Fatalf("a refused retry registered %q", ns)
				}
			}
		}
	}
	if got := recordedMachine(t, b, "alice", daemon.ID); got.Name != daemon.Name || got.KeyFingerprint != daemon.KeyFingerprint {
		t.Fatalf("recorded = %q %s, want what it presented first (%q %s)", got.Name, got.KeyFingerprint, daemon.Name, daemon.KeyFingerprint)
	}
	if s := out.String(); strings.Contains(s, "refused") || strings.Contains(s, "throttled") {
		t.Fatalf("a retry was logged again: %s", s)
	}

	// A brand-new machine is remembered once (after a refill: it costs a
	// token), and the next new one, with none left, is throttled.
	now = now.Add(time.Hour)
	newcomer := mach(3)
	writes = b.Writes()
	for i := 0; i < 3; i++ {
		_, err := reg.Subscribe(context.Background(), uk("alice", "docker-credential"), newcomer)
		notBound(t, fmt.Sprintf("subscribe %d of a brand-new machine", i), err, bound)
	}
	if got := b.Writes(); got != writes+1 {
		t.Fatalf("a brand-new machine wrote the store %d times, want 1", got-writes)
	}
	if got := recordedMachine(t, b, "alice", newcomer.ID); got.Name != newcomer.Name || got.KeyFingerprint != newcomer.KeyFingerprint {
		t.Fatalf("the brand-new machine is recorded as %+v", got)
	}
	var th *ThrottledError
	if _, err := reg.Subscribe(context.Background(), uk("alice", "x"), mach(4)); !errors.As(err, &th) {
		t.Fatalf("a second brand-new machine with no budget = %v, want ThrottledError", err)
	}
}

// TestPinMovedToARefusedMachineRecordsItsCurrentKey: a refused machine's
// retries with a new name and key leave its record as first seen; after a
// move to it, its next subscribe is accepted and records (and logs) what it
// presents now; the machine moved away from is then refused naming it by its
// new name, without a write.
func TestPinMovedToARefusedMachineRecordsItsCurrentKey(t *testing.T) {
	reg, _, b := pinned(t)
	first := mach(1)
	sub(t, reg, uk("alice", "ssh-agent"), first)
	before, after := mach(2), mach(2)
	before.Name, before.KeyFingerprint = "old-name", "SHA256:old"
	after.Name, after.KeyFingerprint = "new-name", "SHA256:new"
	_, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), before)
	notBound(t, "the first sight", err, first)
	for i := 0; i < 3; i++ {
		_, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), after)
		notBound(t, fmt.Sprintf("retry %d with a new name and key", i), err, first)
	}
	if got := recordedMachine(t, b, "alice", after.ID); got.Name != before.Name || got.KeyFingerprint != before.KeyFingerprint {
		t.Fatalf("while refused, recorded = %q %s, want what it presented first", got.Name, got.KeyFingerprint)
	}

	if _, err := reg.Move(context.Background(), "alice", after.ID, "SHA256:mover"); err != nil {
		t.Fatal(err)
	}
	out := logged(t)
	l := sub(t, reg, uk("alice", "ssh-agent"), after)
	if l.MachineID != after.ID || l.MachineName != after.Name {
		t.Fatalf("listener stamp = %s %q", l.MachineID, l.MachineName)
	}
	if got := recordedMachine(t, b, "alice", after.ID); got.Name != after.Name || got.KeyFingerprint != after.KeyFingerprint {
		t.Fatalf("after its accepted subscribe, recorded = %q %s, want %q %s", got.Name, got.KeyFingerprint, after.Name, after.KeyFingerprint)
	}
	for _, line := range []string{
		`changed its name from "old-name" to "new-name"`,
		`now connects with key "SHA256:new" (was "SHA256:old")`,
	} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("the change was not logged (%s): %s", line, out.String())
		}
	}
	writes := b.Writes()
	_, err = reg.Subscribe(context.Background(), uk("alice", "docker-credential"), first)
	notBound(t, "the machine moved away from", err, after)
	if b.Writes() != writes {
		t.Fatal("the refusal of the machine moved away from wrote the store")
	}
}

// TestPinRetryingRefusedMachineIsNotEvicted: at the machine limit the least
// recently seen machine that is not the source is forgotten; a refused known
// machine that keeps retrying counts as seen at each retry (though nothing
// is written), so it is not the one forgotten.
func TestPinRetryingRefusedMachineIsNotEvicted(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	setVar(t, &pinNow, clock)
	setVar(t, &admitBurst, 4*source.MaxMachinesPerUser)
	reg := NewRegistry()
	b := fake.New(source.Options{Now: clock})
	reg.SetBindings(b)
	bound := mach(1)
	sub(t, reg, uk("alice", "ssh-agent"), bound)
	for n := 2; n <= source.MaxMachinesPerUser; n++ {
		now = now.Add(time.Second)
		_, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(n))
		notBound(t, fmt.Sprintf("machine %d", n), err, bound)
	}
	// Machine 2, first seen before every other, keeps retrying (with another
	// process's key); machine 3 is then the least recently seen.
	retry := mach(2)
	retry.KeyFingerprint = "SHA256:other-process"
	for i := 0; i < 3; i++ {
		now = now.Add(time.Second)
		writes := b.Writes()
		_, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), retry)
		notBound(t, fmt.Sprintf("retry %d", i), err, bound)
		if b.Writes() != writes {
			t.Fatalf("retry %d wrote the store", i)
		}
	}
	now = now.Add(time.Second)
	_, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(500))
	notBound(t, "a new machine at the limit", err, bound)
	has := map[string]bool{}
	for _, m := range b.Machines("alice") {
		has[m.ID] = true
	}
	if len(has) != source.MaxMachinesPerUser || !has[mach(2).ID] || has[mach(3).ID] || !has[mach(500).ID] || !has[bound.ID] {
		t.Fatalf("after the limit: machine 2 kept %v, machine 3 kept %v, machine 500 kept %v, bound kept %v, %d machines",
			has[mach(2).ID], has[mach(3).ID], has[mach(500).ID], has[bound.ID], len(has))
	}
}

// usage is what the registry counts: bytes and pending requests in all, and
// the bytes and messages of each connection generation.
type usage struct {
	total, pending int
	conns          map[uint64][2]int
}

func usageOf(r *Registry) usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := usage{total: r.totalBytes, pending: len(r.pending), conns: map[uint64][2]int{}}
	for gen, c := range r.conns {
		u.conns[gen] = [2]int{c.bytes, len(c.msgs)}
	}
	return u
}

// TestPinFailedMoveChangesNothing: a move whose store write fails changes
// nothing in the registry: the old machine's listener stays registered and
// open, its requests stay pending, the byte and message counts are exactly
// as they were, and the machine still is sent its queued request and may
// answer.
func TestPinFailedMoveChangesNothing(t *testing.T) {
	reg, br, b := pinned(t)
	var written []*Envelope
	var mu sync.Mutex
	gen := boxShed(br, "box", "alice", &written, &mu)
	l := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(2)); err == nil {
		t.Fatal("test set-up")
	}
	req := publish(t, br, "box", gen, "")
	recv(t, l)
	queued := publish(t, br, "box", gen, "") // left in the queue
	before := usageOf(reg)
	if before.pending != 2 || before.conns[gen][1] != 2 {
		t.Fatalf("test set-up: %+v", before)
	}

	b.FailWrites(errors.New("disk full"))
	if _, err := reg.Move(context.Background(), "alice", mach(2).ID, "me"); err == nil {
		t.Fatal("a move whose store write failed succeeded")
	}
	b.FailWrites(nil)
	if after := usageOf(reg); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("a failed move changed the counts: %+v, was %+v", after, before)
	}
	checkAccounting(t, reg)
	if now, ok := reg.Get(uk("alice", "ssh-agent")); !ok || now != l || isClosed(l) {
		t.Fatal("a failed move closed or removed the bound machine's listener")
	}
	if got := recv(t, l); got.ID != queued.ID {
		t.Fatalf("after a failed move the queued request = %s, want %s", got.ID, queued.ID)
	}
	if err := respondFrom(br, "alice", mach(1).ID, answer("ssh-agent", "box", req.ID)); err != nil || len(written) != 1 {
		t.Fatalf("the bound machine's answer after a failed move = %v (written %d)", err, len(written))
	}
}

// TestPinMoveReleasesTheOldMachinesAccounting: a move frees the old
// machine's requests, the one still queued included, and with them every
// byte and message they counted, server-wide and on their connection;
// another user's are counted as before.
func TestPinMoveReleasesTheOldMachinesAccounting(t *testing.T) {
	reg, br, _ := pinned(t)
	var written []*Envelope
	var mu sync.Mutex
	boxGen := boxShed(br, "box", "alice", &written, &mu)
	bobsGen := boxShed(br, "bobs", "bob", &written, &mu)
	aliceL := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	bobL := sub(t, reg, uk("bob", "ssh-agent"), mach(9))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(2)); err == nil {
		t.Fatal("test set-up")
	}
	publish(t, br, "box", boxGen, "")
	recv(t, aliceL)
	publish(t, br, "box", boxGen, "") // left in the queue
	publish(t, br, "bobs", bobsGen, "")
	recv(t, bobL)
	before := usageOf(reg)
	bobs := before.conns[bobsGen]
	if before.conns[boxGen][1] != 2 || bobs[1] != 1 || before.total != before.conns[boxGen][0]+bobs[0] {
		t.Fatalf("test set-up: %+v", before)
	}

	if _, err := reg.Move(context.Background(), "alice", mach(2).ID, "me"); err != nil {
		t.Fatal(err)
	}
	after := usageOf(reg)
	if _, ok := after.conns[boxGen]; ok {
		t.Fatalf("the old machine's connection still counts %v after the move", after.conns[boxGen])
	}
	if after.total != bobs[0] || after.pending != 1 || after.conns[bobsGen] != bobs {
		t.Fatalf("after the move the registry counts %+v, want only bob's %v", after, bobs)
	}
	checkAccounting(t, reg)
}

// queuedFor is what l's queue holds right now, taken out of it.
func queuedFor(l *Listener) []*Message {
	var out []*Message
	for {
		select {
		case m := <-l.queue:
			out = append(out, m)
		default:
			return out
		}
	}
}

// TestPinStampStopsQueuedWorkBeforeTheMoveCleansUp: between a move's store
// write and its clean-up (forced with the move's hook) the old machine's
// stamp is no longer current, and that alone stops what was already queued
// or under way: Take sends nothing, a publish to the old machine's listener
// keeps nothing, and a re-delivery waiting for room queues nothing.
func TestPinStampStopsQueuedWorkBeforeTheMoveCleansUp(t *testing.T) {
	setup := func(t *testing.T) (*Registry, *Bridge, uint64) {
		reg, br, _ := pinned(t)
		var written []*Envelope
		var mu sync.Mutex
		gen := boxShed(br, "box", "alice", &written, &mu)
		if _, err := reg.Subscribe(context.Background(), uk("alice", "docker"), mach(1)); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.Subscribe(context.Background(), uk("alice", "docker"), mach(2)); err == nil {
			t.Fatal("test set-up")
		}
		return reg, br, gen
	}
	move := func(t *testing.T, reg *Registry, during func()) {
		t.Helper()
		hooked := false
		setVar(t, &moveStoredHook, func(string) { hooked = true; during() })
		if _, err := reg.Move(context.Background(), "alice", mach(2).ID, "me"); err != nil {
			t.Fatal(err)
		}
		if !hooked {
			t.Fatal("test set-up: the move never reached its hook")
		}
	}

	t.Run("take", func(t *testing.T) {
		reg, br, gen := setup(t)
		l := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
		publish(t, br, "box", gen, "") // queued, not taken
		var queued []*Message
		var sent []*Envelope
		move(t, reg, func() {
			queued = queuedFor(l)
			for _, m := range queued {
				if env := l.Take(m); env != nil {
					sent = append(sent, env)
				}
			}
		})
		if len(queued) != 1 || len(sent) != 0 {
			t.Fatalf("queued %d, sent %d after the store moved the binding; want 1 queued, none sent", len(queued), len(sent))
		}
	})

	t.Run("publish", func(t *testing.T) {
		reg, br, gen := setup(t)
		l := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
		var err error
		var pending, queued int
		move(t, reg, func() {
			err = br.PublishToHost(context.Background(), "box", gen, request("ssh-agent", "box", "", json.RawMessage(`{}`)))
			pending, queued = pendingOf(reg, "alice"), len(l.queue)
		})
		if err == nil || pending != 0 || queued != 0 {
			t.Fatalf("a publish to the old machine after the store moved the binding = %v (pending %d, queued %d); want refused, nothing kept", err, pending, queued)
		}
	})

	t.Run("re-delivery", func(t *testing.T) {
		reg, br, gen := setup(t)
		l := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
		req := publish(t, br, "box", gen, "")
		recv(t, l)
		reg.Unregister(l.Key(), l.Token) // the request stays pending for mach(1)

		// mach(1) reconnects; its queue is full before its re-delivery
		// starts, so the re-delivery waits for room.
		full, done := make(chan struct{}), make(chan struct{})
		setVar(t, &redeliverHook, func(l *Listener, n int) {
			if n == 1 {
				fill(t, reg, "alice", l)
			}
		})
		setVar(t, &redeliverFullHook, func(*Listener) { close(full) })
		setVar(t, &redeliverDoneHook, func(*Listener) { close(done) })
		l2 := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
		within(t, "the re-delivery finding the queue full", func() { <-full })

		var redelivered bool
		move(t, reg, func() {
			// Room, and with it the re-delivery's next look, comes only now.
			for _, m := range queuedFor(l2) {
				l2.Take(m)
			}
			within(t, "the re-delivery", func() { <-done })
			for _, m := range queuedFor(l2) {
				redelivered = redelivered || m.id == req.ID
			}
		})
		if redelivered {
			t.Fatal("a request of the old machine was re-delivered after the store moved the binding")
		}
	})
}

// TestPinGateWaitersGiveUp: while a durable write holds alice's gate (a new
// machine's admission, its save blocked), a subscribe waiting for the gate
// gives up when its context ends, and an answer waiting for it gives up at
// the response deadline with ErrResponseTimeout (retryable), having matched
// and consumed nothing; once the write ends the answer is accepted.
func TestPinGateWaitersGiveUp(t *testing.T) {
	setVar(t, &responseTimeout, 200*time.Millisecond)
	reg, br, b := pinned(t)
	var written []*Envelope
	var mu sync.Mutex
	gen := boxShed(br, "box", "alice", &written, &mu)
	l := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	req := publish(t, br, "box", gen, "")
	recv(t, l)

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b.BeforeWrite(func() { once.Do(func() { close(entered); <-release }) })
	slow := make(chan error, 1)
	go func() {
		_, err := reg.Subscribe(context.Background(), uk("alice", "docker"), mach(2))
		slow <- err
	}()
	within(t, "the blocked write", func() { <-entered })
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	var subErr, ansErr error
	var waited time.Duration
	within(t, "a subscribe waiting for the gate", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, subErr = reg.Subscribe(ctx, uk("alice", "aws"), mach(1))
	})
	if !errors.Is(subErr, context.DeadlineExceeded) {
		t.Fatalf("a subscribe waiting for the gate past its context = %v, want its context's error", subErr)
	}
	within(t, "an answer waiting for the gate", func() {
		start := time.Now()
		_, ansErr = br.BeginResponseFrom("alice", mach(1).ID, answer("ssh-agent", "box", req.ID))
		waited = time.Since(start)
	})
	if !errors.Is(ansErr, ErrResponseTimeout) || waited < responseTimeout {
		t.Fatalf("an answer waiting for the gate = %v after %s, want ErrResponseTimeout at the %s deadline", ansErr, waited, responseTimeout)
	}
	if pendingOf(reg, "alice") != 1 || len(written) != 0 {
		t.Fatal("an answer that gave up at the gate consumed or wrote something")
	}

	released = true
	close(release)
	var err error
	within(t, "the blocked admission", func() { err = <-slow })
	notBound(t, "the new machine", err, mach(1))
	if err := respondFrom(br, "alice", mach(1).ID, answer("ssh-agent", "box", req.ID)); err != nil || len(written) != 1 {
		t.Fatalf("the answer once the gate is free = %v (written %d)", err, len(written))
	}
}

// TestPinSourcesReadDuringAMove: a read of the user's sources that arrives
// between a move's store write and its clean-up (forced with the move's hook,
// the moment the store already names the new machine while the old one still
// holds its listeners) waits for the user's gate and shows the state the move
// left: the new machine bound, and the old one neither connected nor holding
// request types. A machine that is not the bound one never shows connected.
func TestPinSourcesReadDuringAMove(t *testing.T) {
	reg, _, _ := pinned(t)
	sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	sub(t, reg, uk("alice", "docker"), mach(1))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(2)); err == nil {
		t.Fatal("test set-up")
	}
	before, err := reg.Sources(context.Background(), "alice")
	if err != nil || before.Binding == nil || before.Binding.MachineID() != mach(1).ID ||
		fmt.Sprint(before.Connected) != fmt.Sprintf("map[%s:[docker ssh-agent]]", mach(1).ID) || len(before.Machines) != 2 {
		t.Fatalf("sources before the move = %+v, %v", before, err)
	}

	read := make(chan SourcesView, 1)
	waiting := make(chan struct{}, 1)
	setVar(t, &gateWaitHook, func() { waiting <- struct{}{} })
	hooked := false
	setVar(t, &moveStoredHook, func(string) {
		hooked = true
		go func() {
			v, err := reg.Sources(context.Background(), "alice")
			if err != nil {
				t.Error(err)
			}
			read <- v
		}()
		// Wait until the read is waiting for the gate (or, were it not to
		// take the gate, until it has read).
		select {
		case <-waiting:
		case v := <-read:
			read <- v
		}
	})
	mv, err := reg.Move(context.Background(), "alice", mach(2).ID, "me")
	if err != nil || !hooked {
		t.Fatalf("move = %v (hooked %v)", err, hooked)
	}
	var v SourcesView
	within(t, "the read", func() { v = <-read })
	if v.Binding == nil || v.Binding.MachineID() != mach(2).ID || v.Binding.Generation != mv.Binding.Generation {
		t.Fatalf("the read during the move shows binding %+v, want machine 2 at generation %d", v.Binding, mv.Binding.Generation)
	}
	for id, ns := range v.Connected {
		if id != v.Binding.MachineID() {
			t.Fatalf("the read during the move shows machine %s connected with %v, but machine %s is bound", id, ns, v.Binding.MachineID())
		}
	}
}

// TestPinMoveAnswersItsOwnMove: Move's view is the state that move left,
// read before it releases the gate, even when another move of the same user
// is waiting for the gate and lands the moment it is released.
func TestPinMoveAnswersItsOwnMove(t *testing.T) {
	reg, _, b := pinned(t)
	sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(2)); err == nil {
		t.Fatal("test set-up")
	}
	second := make(chan error, 1)
	waiting := make(chan struct{}, 1)
	setVar(t, &gateWaitHook, func() { waiting <- struct{}{} })
	first := true
	setVar(t, &moveStoredHook, func(string) {
		if !first {
			return
		}
		first = false
		go func() {
			_, err := reg.Move(context.Background(), "alice", mach(1).ID, "other")
			second <- err
		}()
		select {
		case <-waiting:
		case err := <-second:
			second <- err
		}
	})
	res, err := reg.Move(context.Background(), "alice", mach(2).ID, "me")
	if err != nil {
		t.Fatal(err)
	}
	within(t, "the second move", func() { err = <-second })
	if err != nil {
		t.Fatal(err)
	}
	if bd, _ := b.Get("alice"); bd.MachineID() != mach(1).ID {
		t.Fatalf("test set-up: the second move did not land (bound %s)", bd.MachineID())
	}
	v := res.View
	if !res.Moved || v.Binding == nil || v.Binding.MachineID() != mach(2).ID || v.Binding.Generation != res.Binding.Generation || v.Binding.BoundBy != "me" {
		t.Fatalf("the first move's view = %+v, want its own move (machine 2, generation %d, by me)", v.Binding, res.Binding.Generation)
	}
	if len(v.Connected) != 0 {
		t.Fatalf("the first move's view shows %v connected; machine 1's stream was closed by it and machine 2 has not connected", v.Connected)
	}
}

// TestPinSourcesWaitEndsWithItsContext: a read waiting for a gate held by a
// move (here, its store write) gives up when its context ends.
func TestPinSourcesWaitEndsWithItsContext(t *testing.T) {
	reg, _, _ := pinned(t)
	sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(2)); err == nil {
		t.Fatal("test set-up")
	}
	var readErr error
	setVar(t, &moveStoredHook, func(string) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, readErr = reg.Sources(ctx, "alice")
	})
	if _, err := reg.Move(context.Background(), "alice", mach(2).ID, "me"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(readErr, context.DeadlineExceeded) {
		t.Fatalf("a read while the gate was held = %v, want its context's deadline", readErr)
	}
	if _, err := NewRegistry().Sources(context.Background(), "alice"); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("Sources with no bindings = %v", err)
	}
}

// TestPinUnbind: Unbind drops the user's records, closes every stream of the
// user (listeners and the delivery stream) and drops their requests, and
// leaves another user's alone. The next machine to subscribe is bound, at a
// generation that never was the user's, so nothing stamped before the
// unbind can be answered. A user with no binding changes nothing.
func TestPinUnbind(t *testing.T) {
	reg, br, b := pinned(t)
	var written []*Envelope
	var mu sync.Mutex
	boxGen := boxShed(br, "box", "alice", &written, &mu)
	bobsGen := boxShed(br, "bobs", "bob", &written, &mu)
	aliceL := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	aliceD := subD(t, reg, "alice", mach(1))
	bobL := sub(t, reg, uk("bob", "ssh-agent"), mach(9))
	if _, err := reg.Subscribe(context.Background(), uk("alice", "ssh-agent"), mach(2)); err == nil {
		t.Fatal("test set-up")
	}
	oldGen := mustGet(t, b, "alice").Generation
	req := publish(t, br, "box", boxGen, "")
	recv(t, aliceL)
	publish(t, br, "bobs", bobsGen, "")
	recv(t, bobL)

	res, err := reg.Unbind(context.Background(), "alice", "SHA256:mover")
	if err != nil || !res.Unbound || res.Previous.ID != mach(1).ID || res.Previous.Name != mach(1).Name || res.Forgotten != 2 || res.Closed != 2 || res.Dropped != 1 {
		t.Fatalf("Unbind = %+v, %v", res, err)
	}
	if _, ok := b.Get("alice"); ok || len(b.Machines("alice")) != 0 {
		t.Fatal("the unbind left alice's records")
	}
	if !isClosed(aliceL) || !dClosed(aliceD) {
		t.Fatal("the unbind left alice's streams open")
	}
	if _, ok := reg.Get(uk("alice", "ssh-agent")); ok {
		t.Fatal("the old listener is still registered")
	}
	if pendingOf(reg, "alice") != 0 {
		t.Fatal("alice's requests were kept")
	}
	if _, ok := reg.DeliveryBound("alice"); ok {
		t.Fatal("delivery still finds a bound machine")
	}
	if isClosed(bobL) || pendingOf(reg, "bob") != 1 {
		t.Fatal("alice's unbind touched bob's listener or requests")
	}

	// Nothing to clear: no change, no error.
	if res, err := reg.Unbind(context.Background(), "alice", "me"); err != nil || res.Unbound || res.Previous.ID != "" {
		t.Fatalf("second Unbind = %+v, %v", res, err)
	}

	// The next machine to connect is bound, the old one's request is not
	// answerable from it.
	l := sub(t, reg, uk("alice", "ssh-agent"), mach(2))
	bd := mustGet(t, b, "alice")
	if bd.MachineID() != mach(2).ID || bd.BoundBy != source.BoundByFirstConnect || bd.Generation <= oldGen {
		t.Fatalf("binding after the next connect = %+v (was generation %d)", bd, oldGen)
	}
	if err := respondFrom(br, "alice", mach(2).ID, answer("ssh-agent", "box", req.ID)); err == nil {
		t.Fatal("a request from before the unbind was answered")
	}
	if isClosed(l) {
		t.Fatal("test: the new listener closed")
	}
}

// TestPinUnbindStoreFailureChangesNothing: an unbind whose save fails keeps
// the binding and every stream.
func TestPinUnbindStoreFailureChangesNothing(t *testing.T) {
	reg, _, b := pinned(t)
	l := sub(t, reg, uk("alice", "ssh-agent"), mach(1))
	b.FailWrites(errors.New("disk full"))
	if _, err := reg.Unbind(context.Background(), "alice", "me"); err == nil {
		t.Fatal("an unbind whose store write failed succeeded")
	}
	b.FailWrites(nil)
	if bd := mustGet(t, b, "alice"); bd.MachineID() != mach(1).ID || isClosed(l) {
		t.Fatalf("a failed unbind changed the binding or closed the stream: %+v", bd)
	}
	if _, err := NewRegistry().Unbind(context.Background(), "alice", "me"); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("Unbind with no bindings = %v, want ErrNotPinned", err)
	}
}

// mustGet is user's binding in b (fails without one).
func mustGet(t *testing.T, b source.Bindings, user string) source.Binding {
	t.Helper()
	bd, ok := b.Get(user)
	if !ok {
		t.Fatalf("%s has no binding", user)
	}
	return bd
}

// dropAfterRead is a store whose user's records are dropped (as an unbind
// does) right after the first read once armed: an unbind landing between
// two reads of a reader that does not hold the user's gate.
type dropAfterRead struct {
	*fake.Bindings
	user  string
	armed atomic.Bool
}

func (d *dropAfterRead) after() {
	if d.armed.CompareAndSwap(true, false) {
		if _, err := d.Drop(d.user); err != nil {
			panic(err)
		}
	}
}

func (d *dropAfterRead) Get(user string) (source.Binding, bool) {
	bd, ok := d.Bindings.Get(user)
	d.after()
	return bd, ok
}

func (d *dropAfterRead) Machines(user string) []source.Machine {
	ms := d.Bindings.Machines(user)
	d.after()
	return ms
}

func (d *dropAfterRead) Bound(user string) (source.Binding, source.Machine, bool) {
	bd, m, ok := d.Bindings.Bound(user)
	d.after()
	return bd, m, ok
}

// TestPinDeliveryBoundIsOneMomentsState: delivery's lookup of the bound
// machine (DeliveryBound, which takes no gate) reads the binding and the
// machine at one moment, so an unbind landing between reads leaves it either
// bound to the whole machine (its id and name) or not bound, never bound to
// a machine with no id; and a refusal names the machine the same way.
func TestPinDeliveryBoundIsOneMomentsState(t *testing.T) {
	reg := NewRegistry()
	b := &dropAfterRead{Bindings: fake.New(source.Options{}), user: "alice"}
	reg.SetBindings(b)
	sub(t, reg, uk("alice", "ssh-agent"), mach(1))

	b.armed.Store(true)
	m, ok := reg.DeliveryBound("alice")
	if ok && (m.MachineID != mach(1).ID || m.MachineName != mach(1).Name) {
		t.Fatalf("DeliveryBound with an unbind between its reads = %+v, bound; want the whole machine or not bound", m)
	}
	if _, ok := reg.DeliveryBound("alice"); ok {
		t.Fatal("DeliveryBound after the unbind still finds a machine")
	}

	// Through a create's delivery: an unbind at its first read gives a
	// whole machine (offline) or no request, never a machine with no id.
	// (The test drops the records without closing streams, as Unbind would,
	// so each bind here takes a namespace not held yet.)
	sub(t, reg, uk("alice", "git"), mach(1))
	b.armed.Store(true)
	svc := delivery.NewService(reg, time.Second)
	res, err := svc.Request(context.Background(), delivery.Params{Reason: delivery.ReasonCreate, User: "alice",
		Shed: delivery.RequestShed{Name: "box", ID: "idbox", Backend: delivery.BackendFirecracker}})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case res.Reason == delivery.ReasonNotApplicable:
	case res.Reason == delivery.ReasonSourceOffline && res.Source != nil && res.Source.MachineID == mach(1).ID && res.Source.MachineName == mach(1).Name:
	default:
		t.Fatalf("a create's delivery with an unbind between its reads = %+v (source %+v)", res, res.Source)
	}

	sub(t, reg, uk("alice", "docker"), mach(1))
	b.armed.Store(true)
	var nb *NotBoundError
	if err := reg.DeliveryAnswerFrom("alice", mach(2).ID); !errors.As(err, &nb) {
		t.Fatalf("DeliveryAnswerFrom = %v", err)
	} else if nb.Bound.ID != "" && (nb.Bound.ID != mach(1).ID || nb.Bound.Name != mach(1).Name) {
		t.Fatalf("a refusal with an unbind between its reads names %+v", nb.Bound)
	}
}
