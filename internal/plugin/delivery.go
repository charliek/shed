package plugin

// Delivery streams on the pin. A person's source holds one delivery stream
// per server (GET /api/delivery/requests), on which the server sends it
// delivery requests (internal/delivery). Its admission is the bus's: the
// same per-user gate, rate limit, store and generation check as Subscribe,
// so a delivery stream and a bus subscription of one machine are admitted
// under one binding and moved together. What is new for delivery:
//
//   - The stream is bound to the certificate serial that opened it and to a
//     stream id made at random, which the source echoes on each answer; the
//     pending-request table compares both (the bus compares neither).
//   - A delivery stream is never re-sent anything. When it ends (Done
//     closes: its connection went, a write stalled, the binding moved, the
//     registry closed) every request it carried fails at once.
//
// A user holds at most one delivery stream: only the bound machine is
// admitted, and its second stream is refused as a duplicate
// (ErrDuplicateStream). Move closes the old machine's stream with its
// listeners, which fails that user's pending delivery requests. Sources
// counts a delivery stream as its machine connected.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/source"
)

// deliveryQueueLen is how many requests a delivery stream's queue holds:
// more than a user may have waiting at once, so a live stream never refuses
// one for want of room.
const deliveryQueueLen = 16

// DeliveryStream is one machine's delivery stream, registered for its user.
// It implements delivery.Stream. Every field is fixed when it is made.
type DeliveryStream struct {
	user      string
	machine   delivery.SourceMachine
	gen       uint64
	serial    string
	id        string
	token     uint64
	createdAt time.Time

	queue     chan delivery.Outgoing
	done      chan struct{}
	closeOnce sync.Once
}

// User is the stream's user ("" for the implicit user).
func (s *DeliveryStream) User() string { return s.user }

// ID is the stream's id: 32 lowercase hex digits, at random.
func (s *DeliveryStream) ID() string { return s.id }

// Machine is the machine that opened the stream.
func (s *DeliveryStream) Machine() delivery.SourceMachine { return s.machine }

// Generation is the binding generation the stream was admitted under.
func (s *DeliveryStream) Generation() uint64 { return s.gen }

// Serial is the serial of the certificate that opened the stream ("" in
// open mode).
func (s *DeliveryStream) Serial() string { return s.serial }

// Done is closed when the stream is unregistered.
func (s *DeliveryStream) Done() <-chan struct{} { return s.done }

// Requests is what the stream's handler writes, in order. Each is written
// only while its Live reports true.
func (s *DeliveryStream) Requests() <-chan delivery.Outgoing { return s.queue }

// Offer queues o without blocking; false when the stream has ended or its
// queue is full.
func (s *DeliveryStream) Offer(o delivery.Outgoing) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.queue <- o:
		return true
	default:
		return false
	}
}

func (s *DeliveryStream) close() { s.closeOnce.Do(func() { close(s.done) }) }

// Test hooks (nil in production). deliveryAdmittedHook runs in
// SubscribeDelivery once the store has admitted the machine, with the user's
// gate still held and before the stream is registered.
var deliveryAdmittedHook func(user string, m source.Machine)

// SubscribeDelivery admits machine m for user and registers its delivery
// stream, opened with the certificate whose serial is serial ("" in open
// mode). m is validated by the caller, as for Subscribe.
//
// It runs under the user's gate, as Subscribe does, with the same checks in
// the same order: the bound machine already holding its delivery stream (at
// the binding's current generation) is refused as a duplicate
// (ErrDuplicateStream) before anything is asked of the store or spent; then
// the shared admission (a remembered machine that is not the bound one is
// *NotBoundError, an admission that would write the store beyond the user's
// rate limit is *ThrottledError, a store failure wraps ErrBindingStore); and
// the admitted machine's stream is registered, under r.mu, only while its
// binding is still current (else *NotBoundError naming the binding now).
// ErrBusClosed after Close, ErrNotPinned with no source bindings, and ctx's
// error when the gate was not had before ctx ended.
func (r *Registry) SubscribeDelivery(ctx context.Context, user string, m source.Machine, serial string) (*DeliveryStream, error) {
	b := r.pin()
	if b == nil {
		return nil, ErrNotPinned
	}
	g := r.gate(user)
	if err := g.lock(ctx); err != nil {
		return nil, err
	}
	defer g.unlock()

	if r.holdsDeliveryCurrent(user, m.ID) {
		return nil, fmt.Errorf("delivery stream: %w", ErrDuplicateStream)
	}
	gen, err := r.admitLocked(g, b, user, m, "a delivery stream")
	if err != nil {
		return nil, err
	}
	if deliveryAdmittedHook != nil {
		deliveryAdmittedHook(user, m)
	}
	return r.registerDelivery(user, m, serial, gen, func() bool { return b.Current(user, m.ID, gen) })
}

// holdsDeliveryCurrent reports whether user's delivery stream is held by
// machine at the binding's current generation.
func (r *Registry) holdsDeliveryCurrent(user, machine string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.deliveries[user]
	return s != nil && s.machine.MachineID == machine && r.stampCurrentLocked(user, s.machine.MachineID, s.gen)
}

// registerDelivery makes and registers user's delivery stream, only while
// current reports its stamp is still the user's binding. A stream of the
// user's that is no longer current (left by a change of the binding that did
// not close it) is closed and replaced; a current one is a duplicate.
func (r *Registry) registerDelivery(user string, m source.Machine, serial string, gen uint64, current func() bool) (*DeliveryStream, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrBusClosed
	}
	if !current() {
		return nil, r.notBoundLocked(user)
	}
	if old := r.deliveries[user]; old != nil {
		if r.stampCurrentLocked(user, old.machine.MachineID, old.gen) {
			return nil, fmt.Errorf("delivery stream: %w", ErrDuplicateStream)
		}
		r.removeDeliveryLocked(old)
	}
	r.nextToken++
	s := &DeliveryStream{
		user:      user,
		machine:   delivery.SourceMachine{MachineID: m.ID, MachineName: m.Name},
		gen:       gen,
		serial:    serial,
		id:        newStreamID(),
		token:     r.nextToken,
		createdAt: time.Now().UTC(),
		queue:     make(chan delivery.Outgoing, deliveryQueueLen),
		done:      make(chan struct{}),
	}
	r.deliveries[user] = s
	return s, nil
}

// newStreamID is a delivery stream's id: 32 lowercase hex digits, at random.
func newStreamID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("credential bus: no randomness for a delivery stream id: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// UnregisterDelivery removes s if it is still its user's registered stream,
// and closes it either way; it reports whether it removed it. The stream's
// handler calls it when the stream ends.
func (r *Registry) UnregisterDelivery(s *DeliveryStream) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deliveries[s.user] != s {
		s.close()
		return false
	}
	r.removeDeliveryLocked(s)
	return true
}

// removeDeliveryLocked removes the registered stream s and closes it. The
// caller holds r.mu.
func (r *Registry) removeDeliveryLocked(s *DeliveryStream) {
	if r.deliveries[s.user] == s {
		delete(r.deliveries, s.user)
	}
	s.close()
}

// DeliveryBound returns user's bound machine (delivery.Pin): false when the
// user has no binding or the registry has no source bindings.
func (r *Registry) DeliveryBound(user string) (delivery.SourceMachine, bool) {
	b := r.pin()
	if b == nil {
		return delivery.SourceMachine{}, false
	}
	if _, ok := b.Get(user); !ok {
		return delivery.SourceMachine{}, false
	}
	m := boundMachine(b, user)
	return delivery.SourceMachine{MachineID: m.ID, MachineName: m.Name}, true
}

// DeliveryStream returns user's open delivery stream (delivery.Pin): the
// registered one, while its machine at its generation is still the user's
// binding and it has not ended.
func (r *Registry) DeliveryStream(user string) (delivery.Stream, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.deliveries[user]
	if s == nil || r.bindings == nil || !r.stampCurrentLocked(user, s.machine.MachineID, s.gen) {
		return nil, false
	}
	select {
	case <-s.done:
		return nil, false
	default:
	}
	return s, true
}

// DeliveryCurrent reports whether machineID at generation is still user's
// binding (delivery.Pin). Always false with no source bindings.
func (r *Registry) DeliveryCurrent(user, machineID string, generation uint64) bool {
	b := r.pin()
	return b != nil && b.Current(user, machineID, generation)
}

// DeliveryAnswerFrom checks that machineID is user's bound machine, the one
// whose answers delivery takes: nil when it is, *NotBoundError naming the
// bound machine when it is not (no machine, when the user has none),
// ErrNotPinned with no source bindings.
func (r *Registry) DeliveryAnswerFrom(user, machineID string) error {
	b := r.pin()
	if b == nil {
		return ErrNotPinned
	}
	if bd, ok := b.Get(user); ok && bd.MachineID() == machineID {
		return nil
	}
	return &NotBoundError{User: user, Bound: boundMachine(b, user)}
}
