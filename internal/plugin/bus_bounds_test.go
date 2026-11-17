package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The bounds on what the credential bus retains and on how long anything in
// it waits: the limits on held messages, the memory a queue may keep, the
// publishes and re-deliveries that never block without bound, and the sweep.
// Interleavings are forced with the package's test hooks and barriers.

// setVar sets *p to v for the rest of the test.
func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// within runs fn and fails the test if it does not return within a generous
// guard. It is a guard against a hang, never an ordering device.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return", what)
	}
}

// fillerConn is the connection generation fill publishes as: one no bridge
// issues, so its events are never taken for a shed's.
const fillerConn = uint64(1) << 62

// fill publishes events until l's queue is full.
func fill(t *testing.T, r *Registry, user string, l *Listener) {
	t.Helper()
	for len(l.queue) < cap(l.queue) {
		ev := NewEnvelope(l.Namespace, MessageTypeEvent, nil)
		if err := r.Publish(context.Background(), user, fillerConn, ev); err != nil {
			t.Fatalf("filling the queue: %v", err)
		}
	}
}

// request is a request envelope from shed with the given guest ID.
func request(namespace, shed, id string, payload json.RawMessage) *Envelope {
	env := NewEnvelope(namespace, MessageTypeRequest, payload)
	env.Shed = &ShedInfo{Name: shed}
	if id != "" {
		env.ID = id
	}
	return env
}

// checkAccounting fails unless the registry's counts are exactly what its
// held messages add up to.
func checkAccounting(t *testing.T, r *Registry) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	total, tracked := 0, 0
	for conn, u := range r.conns {
		bytes := 0
		for m := range u.msgs {
			if m.env == nil || m.conn != conn {
				t.Fatalf("connection %d holds a freed or foreign message %+v", conn, m)
			}
			bytes += m.size
			if m.tracked {
				tracked++
				if r.pending[m.id] != m {
					t.Fatalf("a held request is not pending under its ID %s", m.id)
				}
			}
		}
		if len(u.msgs) == 0 || bytes != u.bytes {
			t.Fatalf("connection %d counts %d bytes for %d messages that hold %d", conn, u.bytes, len(u.msgs), bytes)
		}
		total += bytes
	}
	if total != r.totalBytes || tracked != len(r.pending) {
		t.Fatalf("the registry counts %d bytes and %d pending; its messages hold %d bytes and %d requests", r.totalBytes, len(r.pending), total, tracked)
	}
}

// redeliveryWatch records, through the registry's hooks, what each Register
// decided to re-deliver and when that re-delivery ended, so a test can wait
// for it to finish and then check a listener's queue for what must not have
// been sent, with no sleep. Make it with watchRedelivery before the test
// starts any goroutine that registers.
type redeliveryWatch struct {
	mu     sync.Mutex
	counts map[*Listener]int
	done   map[*Listener]chan struct{}
}

func watchRedelivery(t *testing.T) *redeliveryWatch {
	t.Helper()
	w := &redeliveryWatch{counts: map[*Listener]int{}, done: map[*Listener]chan struct{}{}}
	setVar(t, &redeliverHook, func(l *Listener, n int) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.counts[l] = n
		w.done[l] = make(chan struct{})
	})
	setVar(t, &redeliverDoneHook, func(l *Listener) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if ch := w.done[l]; ch != nil {
			close(ch)
		}
	})
	return w
}

// register registers key and reports how many retained requests Register
// decided to re-deliver to it (decided synchronously, before it returns).
func (w *redeliveryWatch) register(t *testing.T, r *Registry, key ListenerKey) (*Listener, int) {
	t.Helper()
	l, err := r.Register(key)
	if err != nil {
		t.Fatalf("Register(%+v): %v", key, err)
	}
	w.mu.Lock()
	n, ok := w.counts[l]
	w.mu.Unlock()
	if !ok {
		t.Fatal("Register did not report what it re-delivers")
	}
	return l, n
}

// ended waits until l's re-delivery, if it had one, has ended.
func (w *redeliveryWatch) ended(t *testing.T, l *Listener) {
	t.Helper()
	w.mu.Lock()
	n, ch := w.counts[l], w.done[l]
	w.mu.Unlock()
	if n == 0 {
		return // no re-delivery goroutine was started
	}
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("re-delivery never ended")
	}
}

// settledRegister registers key and returns once whatever it re-delivers
// has been queued (at most a queue's worth, unread).
func (w *redeliveryWatch) settledRegister(t *testing.T, r *Registry, key ListenerKey) *Listener {
	t.Helper()
	l, _ := w.register(t, r, key)
	w.ended(t, l)
	return l
}

// drain takes everything in l's queue, as its stream would.
func drain(l *Listener) {
	for {
		select {
		case m := <-l.Messages:
			l.Take(m)
		default:
			return
		}
	}
}

// TestMessageLimits: a connection may hold at most maxMessagesPerConn
// messages and maxBytesPerConn of them, and the server maxBytesTotal; a
// message beyond a limit is dropped (not queued, not tracked) and other
// connections are unaffected by one's backlog. A request counts until it is
// answered, any other message until its listener takes it.
func TestMessageLimits(t *testing.T) {
	t.Run("requests per connection", func(t *testing.T) {
		setVar(t, &maxMessagesPerConn, 3)
		r := NewRegistry()
		l, _ := r.Register(k("op"))
		var ids []string
		for range 3 {
			req := request("op", "noisy", "", nil)
			if err := r.Publish(context.Background(), "", 1, req); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, req.ID)
		}
		drain(l) // taken by the listener, and still un-answered
		over := request("op", "noisy", "", nil)
		if err := r.Publish(context.Background(), "", 1, over); !errors.Is(err, ErrPendingLimit) {
			t.Fatalf("a fourth request = %v, want ErrPendingLimit", err)
		}
		noMessage(t, l)
		if len(r.pending) != 3 {
			t.Fatalf("%d requests pending after a dropped one, want 3", len(r.pending))
		}
		if err := r.Publish(context.Background(), "", 2, request("op", "quiet", "", nil)); err != nil {
			t.Fatalf("another connection's request: %v", err)
		}
		// Answering one frees a slot.
		if !consumed(r, "op", "noisy", ids[0], true) {
			t.Fatal("answer refused")
		}
		if err := r.Publish(context.Background(), "", 1, request("op", "noisy", "", nil)); err != nil {
			t.Fatalf("a request after an answer freed a slot: %v", err)
		}
		checkAccounting(t, r)
	})
	t.Run("events per connection, until taken", func(t *testing.T) {
		setVar(t, &maxMessagesPerConn, 3)
		r := NewRegistry()
		l, _ := r.Register(k("op"))
		event := func() error {
			return r.Publish(context.Background(), "", 1, NewEnvelope("op", MessageTypeEvent, nil))
		}
		for range 3 {
			if err := event(); err != nil {
				t.Fatal(err)
			}
		}
		if err := event(); !errors.Is(err, ErrPendingLimit) {
			t.Fatalf("a fourth event with three still queued = %v, want ErrPendingLimit", err)
		}
		recv(t, l) // the listener takes one: it is forgotten
		if err := event(); err != nil {
			t.Fatalf("an event once the listener took one: %v", err)
		}
		checkAccounting(t, r)
	})
	t.Run("bytes per connection", func(t *testing.T) {
		setVar(t, &maxBytesPerConn, 4096)
		r := NewRegistry()
		l, _ := r.Register(k("op"))
		big := json.RawMessage(`"` + strings.Repeat("x", 3000) + `"`)
		if err := r.Publish(context.Background(), "", 1, request("op", "noisy", "", big)); err != nil {
			t.Fatal(err)
		}
		if err := r.Publish(context.Background(), "", 1, request("op", "noisy", "", big)); !errors.Is(err, ErrPendingLimit) {
			t.Fatalf("a request past the connection's bytes = %v, want ErrPendingLimit", err)
		}
		ev := NewEnvelope("op", MessageTypeEvent, big)
		if err := r.Publish(context.Background(), "", 1, ev); !errors.Is(err, ErrPendingLimit) {
			t.Fatalf("an event past the connection's bytes = %v, want ErrPendingLimit", err)
		}
		if err := r.Publish(context.Background(), "", 2, request("op", "quiet", "", big)); err != nil {
			t.Fatalf("another connection's request: %v", err)
		}
		drain(l)
		checkAccounting(t, r)
	})
	t.Run("bytes in total", func(t *testing.T) {
		setVar(t, &maxBytesTotal, 8192)
		r := NewRegistry()
		l, _ := r.Register(k("op"))
		big := json.RawMessage(`"` + strings.Repeat("x", 3000) + `"`)
		for conn, shed := range []string{"a", "b"} {
			if err := r.Publish(context.Background(), "", uint64(conn+1), request("op", shed, "", big)); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Publish(context.Background(), "", 3, request("op", "c", "", big)); !errors.Is(err, ErrPendingLimit) {
			t.Fatalf("a request past the server's bytes = %v, want ErrPendingLimit", err)
		}
		drain(l)
		checkAccounting(t, r)
	})
}

// TestQueueKeepsNoPayloadOfAFreedMessage: a listener's queue holds references
// to messages, and a message owns its payload, so what the registry counts is
// what it retains. A listener that never reads has a large request queued for
// it, the shed's connection goes (a reconnect), and again, seventeen times:
// every time the retired connection's request is freed, the count returns to
// nothing, and the queue's references keep no payload alive. (When the queue
// held the envelopes themselves, a retired request was refunded but its
// payload stayed in the queue: 32 slots of uncounted payload per listener.)
func TestQueueKeepsNoPayloadOfAFreedMessage(t *testing.T) {
	reg := NewRegistry()
	b := NewBridge(reg, false)
	l, _ := reg.Register(k("op")) // never reads
	payload := json.RawMessage(`"` + strings.Repeat("x", 256<<10) + `"`)
	const rounds = 17
	for i := range rounds {
		gen := b.RegisterShed("dev", &ShedConn{Send: nopSend}) // a (re)connect: the previous connection is retired
		if reg.totalBytes != 0 || len(reg.pending) != 0 {
			t.Fatalf("round %d: after the reconnect the registry still counts %d bytes, %d pending", i, reg.totalBytes, len(reg.pending))
		}
		if err := b.PublishToHost(context.Background(), "dev", gen, request("op", "dev", "", payload)); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if reg.totalBytes < len(payload) {
			t.Fatalf("round %d: a queued %d-byte request is counted %d bytes", i, len(payload), reg.totalBytes)
		}
		checkAccounting(t, reg)
	}
	b.UnregisterShed("dev")
	if reg.totalBytes != 0 || len(reg.pending) != 0 || len(reg.conns) != 0 {
		t.Fatalf("with every connection gone the registry counts %d bytes, %d pending, %d connections", reg.totalBytes, len(reg.pending), len(reg.conns))
	}
	refs, retained := 0, 0
	for len(l.queue) > 0 {
		m := <-l.queue
		refs++
		if m.env != nil {
			retained += len(m.env.Payload)
		}
	}
	if refs != rounds {
		t.Fatalf("test set-up: the queue held %d references, want %d", refs, rounds)
	}
	if retained != 0 {
		t.Fatalf("the queue keeps %d bytes of payload that the registry counts as freed", retained)
	}
}

// TestEventsAreCountedUntilTakenOrTheirListenerGoes: a message that is not a
// request is counted while it sits in a queue, and freed when its listener
// takes it or is unregistered with it still queued.
func TestEventsAreCountedUntilTakenOrTheirListenerGoes(t *testing.T) {
	r := NewRegistry()
	l, _ := r.Register(k("op"))
	body := json.RawMessage(`"` + strings.Repeat("e", 1000) + `"`)
	event := func() {
		t.Helper()
		if err := r.Publish(context.Background(), "", 1, NewEnvelope("op", MessageTypeEvent, body)); err != nil {
			t.Fatal(err)
		}
	}
	event()
	if r.totalBytes < len(body) {
		t.Fatalf("a queued %d-byte event is counted %d bytes", len(body), r.totalBytes)
	}
	if got := recv(t, l); string(got.Payload) != string(body) {
		t.Fatal("the event's payload did not reach the listener")
	}
	if r.totalBytes != 0 {
		t.Fatalf("an event the listener took is still counted %d bytes", r.totalBytes)
	}
	event()
	event()
	r.Unregister(l.Key(), l.Token)
	if r.totalBytes != 0 || len(r.conns) != 0 {
		t.Fatalf("events left in an unregistered listener's queue are still counted: %d bytes, %d connections", r.totalBytes, len(r.conns))
	}
}

// TestPublishNeverBlocksOnAFullQueue: a listener that stops reading cannot
// hold a publish: a full queue drops the request after publishWait, and the
// publisher's context ending (its shed's channel stopping) releases it at
// once. A dropped request is not left pending.
func TestPublishNeverBlocksOnAFullQueue(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		setVar(t, &publishWait, 20*time.Millisecond)
		r := NewRegistry()
		l, _ := r.Register(k("op"))
		fill(t, r, "", l)
		req := request("op", "dev", "", nil)
		var err error
		within(t, "a publish to a full queue", func() { err = r.Publish(context.Background(), "", 1, req) })
		if !errors.Is(err, ErrListenerBusy) {
			t.Fatalf("publish to a full queue = %v, want ErrListenerBusy", err)
		}
		if len(r.pending) != 0 {
			t.Fatal("a dropped request was left pending")
		}
		checkAccounting(t, r)
	})
	t.Run("context", func(t *testing.T) {
		setVar(t, &publishWait, time.Hour)
		r := NewRegistry()
		l, _ := r.Register(k("op"))
		fill(t, r, "", l)
		ctx, cancel := context.WithCancel(context.Background())
		waiting := make(chan struct{})
		setVar(t, &publishFullHook, func() { close(waiting) })
		req := request("op", "dev", "", nil)
		done := make(chan error, 1)
		go func() { done <- r.Publish(ctx, "", 1, req) }()
		<-waiting // the publish found the queue full and waits
		cancel()
		var err error
		within(t, "a publish whose context ended", func() { err = <-done })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("publish after its context ended = %v, want context.Canceled", err)
		}
		if len(r.pending) != 0 {
			t.Fatal("a dropped request was left pending")
		}
		checkAccounting(t, r)
	})
}

// TestRegistryPublishBlockedThenListenerDisconnects: a publish waiting on a
// full queue returns when its listener disconnects, and the request is kept
// for the listener's next connection.
func TestRegistryPublishBlockedThenListenerDisconnects(t *testing.T) {
	setVar(t, &publishWait, time.Hour)
	r := NewRegistry()
	l, _ := r.Register(k("op"))
	fill(t, r, "", l)
	waiting := make(chan struct{})
	setVar(t, &publishFullHook, func() { close(waiting) })
	req := request("op", "dev", "", nil)
	done := make(chan error, 1)
	go func() { done <- r.Publish(context.Background(), "", 1, req) }()
	<-waiting
	if !r.Unregister(l.Key(), l.Token) {
		t.Fatal("unregister did nothing")
	}
	var err error
	within(t, "a publish whose listener disconnected", func() { err = <-done })
	if err == nil || !strings.Contains(err.Error(), "disconnected") {
		t.Fatalf("publish after its listener disconnected = %v, want a disconnected error", err)
	}
	l2, _ := r.Register(k("op"))
	if got := recv(t, l2); got.ID != req.ID {
		t.Fatalf("re-delivered %s, want the request kept across the disconnect %s", got.ID, req.ID)
	}
	checkAccounting(t, r)
}

// TestReDeliveryEndsWhenTheListenerDisconnects: re-delivery to a listener that
// never reads ends when it disconnects, and the requests stay for the next
// connection. The re-delivery's own give-up time is an hour here, so only the
// disconnect can end it.
func TestReDeliveryEndsWhenTheListenerDisconnects(t *testing.T) {
	setVar(t, &redeliverWait, time.Hour)
	r := NewRegistry()
	l, _ := r.Register(k("op"))
	const n = 40
	for range n {
		dispatchRequest(t, r, "op", "dev")
		recv(t, l)
	}
	r.Unregister(l.Key(), l.Token)

	full := make(chan *Listener, 1)
	setVar(t, &redeliverFullHook, func(l *Listener) { full <- l })
	ended := make(chan *Listener, 2)
	setVar(t, &redeliverDoneHook, func(l *Listener) { ended <- l })
	l2, _ := r.Register(k("op")) // never read: the queue fills at 32
	select {
	case got := <-full:
		if got != l2 || len(l2.queue) != cap(l2.queue) {
			t.Fatalf("test set-up: re-delivery found a full queue on %p with %d/%d queued, want %p full", got, len(l2.queue), cap(l2.queue), l2)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("test set-up: re-delivery never filled the queue")
	}
	r.Unregister(l2.Key(), l2.Token)
	select {
	case got := <-ended:
		if got != l2 {
			t.Fatalf("the re-delivery that ended was for %p, want %p", got, l2)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("re-delivery outlived its listener")
	}

	l3, _ := r.Register(k("op"))
	for range n {
		recv(t, l3)
	}
}

// TestReDeliveryEndsWhenTheListenerStopsReading: re-delivery to a listener
// that stays connected but never reads gives up after redeliverWait.
func TestReDeliveryEndsWhenTheListenerStopsReading(t *testing.T) {
	setVar(t, &redeliverWait, 20*time.Millisecond)
	r := NewRegistry()
	l, _ := r.Register(k("op"))
	for range 40 {
		dispatchRequest(t, r, "op", "dev")
		recv(t, l)
	}
	r.Unregister(l.Key(), l.Token)
	ended := make(chan struct{}, 2)
	setVar(t, &redeliverDoneHook, func(*Listener) { ended <- struct{}{} })
	r.Register(k("op")) // never read, never unregistered
	within(t, "re-delivery to a listener that never reads", func() { <-ended })
}

// TestEnvelopeIdentifiersAreCapped: an ID, in_reply_to, namespace or type
// longer than maxIdentifierLen is refused before anything is retained or
// delivered, and the error never carries the value.
func TestEnvelopeIdentifiersAreCapped(t *testing.T) {
	long := strings.Repeat("q", maxIdentifierLen+1)
	for _, tc := range []struct {
		field string
		set   func(*Envelope)
	}{
		{"id", func(e *Envelope) { e.ID = long }},
		{"in_reply_to", func(e *Envelope) { e.InReplyTo = long }},
		{"namespace", func(e *Envelope) { e.Namespace = long }},
		{"type", func(e *Envelope) { e.Type = MessageType(long) }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			r := NewRegistry()
			l, _ := r.Register(k("op"))
			env := request("op", "dev", "", nil)
			tc.set(env)
			err := r.Publish(context.Background(), "", 1, env)
			if !errors.Is(err, ErrIdentifierTooLong) {
				t.Fatalf("an envelope with a %d-byte %s = %v, want ErrIdentifierTooLong", len(long), tc.field, err)
			}
			if strings.Contains(err.Error(), long[:maxQuoted+1]) {
				t.Fatalf("the error carries the guest's %s: %v", tc.field, err)
			}
			noMessage(t, l)
			if len(r.pending) != 0 || r.totalBytes != 0 {
				t.Fatalf("a refused envelope retained %d entries, %d bytes", len(r.pending), r.totalBytes)
			}
		})
	}
	t.Run("at the cap", func(t *testing.T) {
		r := NewRegistry()
		l, _ := r.Register(k("op"))
		env := request("op", "dev", strings.Repeat("q", maxIdentifierLen), nil)
		env.InReplyTo = strings.Repeat("r", maxIdentifierLen)
		if err := r.Publish(context.Background(), "", 1, env); err != nil {
			t.Fatalf("identifiers of exactly %d bytes: %v", maxIdentifierLen, err)
		}
		recv(t, l)
	})
}

// TestErrorsNeverQuoteAGuestStringWhole: an error of the bus that names what
// a guest chose (a namespace nobody listens for) carries at most maxQuoted
// bytes of it.
func TestErrorsNeverQuoteAGuestStringWhole(t *testing.T) {
	r := NewRegistry()
	namespace := strings.Repeat("n", maxIdentifierLen)
	err := r.Publish(context.Background(), "", 1, request(namespace, "dev", "", nil))
	if err == nil {
		t.Fatal("a publish with no listener was accepted")
	}
	if strings.Contains(err.Error(), namespace[:maxQuoted+1]) {
		t.Fatalf("the error quotes more than %d bytes of the guest's namespace: %v", maxQuoted, err)
	}
	if !strings.Contains(err.Error(), namespace[:maxQuoted]) {
		t.Fatalf("the error does not name the namespace at all: %v", err)
	}
}

// TestMessageSizeCountsEveryField: a held message is counted for every field
// of its envelope, for the guest's ID it keeps and for the server's ID.
func TestMessageSizeCountsEveryField(t *testing.T) {
	sizeOf := func(change func(*Envelope)) int {
		t.Helper()
		r := NewRegistry()
		if _, err := r.Register(k("op")); err != nil {
			t.Fatal(err)
		}
		env := request("op", "dev", "fixed-id", []byte(`{"a":1}`))
		if change != nil {
			change(env)
		}
		if err := r.Publish(context.Background(), "", 1, env); err != nil {
			t.Fatal(err)
		}
		checkAccounting(t, r)
		return r.totalBytes // while it is queued, whatever kind it is
	}
	base := sizeOf(nil)
	env := request("op", "dev", "fixed-id", []byte(`{"a":1}`))
	if want := envelopeSize(env) + len("fixed-id") + requestIDLen + messageOverhead; base != want {
		t.Fatalf("a request is counted %d bytes, want envelope + guest ID + server ID + overhead = %d", base, want)
	}
	extra := strings.Repeat("r", 100)
	for _, tc := range []struct {
		field  string
		change func(*Envelope)
		want   int
	}{
		{"in_reply_to", func(e *Envelope) { e.InReplyTo = extra }, 100},
		{"payload", func(e *Envelope) { e.Payload = append(e.Payload, extra...) }, 100},
		{"type", func(e *Envelope) { e.Type += MessageType(extra) }, 100}, // an unknown type: not a request, still counted
		{"id", func(e *Envelope) { e.ID += extra }, 200},                  // in the envelope as it arrived, and kept
	} {
		if got := sizeOf(tc.change) - base; got != tc.want {
			t.Errorf("100 more bytes of %s add %d bytes to the count, want %d", tc.field, got, tc.want)
		}
	}
}

// TestRegisterSweepIsRateLimited: a subscribe sweeps stale requests
// at most once per sweepEvery, as a publish does.
func TestRegisterSweepIsRateLimited(t *testing.T) {
	setVar(t, &sweepEvery, time.Hour)
	watch := watchRedelivery(t)
	r := NewRegistry()
	l, _ := r.Register(k("op")) // sweeps: the first in an hour
	id := dispatchRequest(t, r, "op", "dev")
	recv(t, l)
	r.Unregister(l.Key(), l.Token)

	setVar(t, &pendingRetention, -time.Second) // every request is stale from here
	l2, n := watch.register(t, r, k("op"))
	if n != 1 {
		t.Fatalf("a subscribe within sweepEvery of the last sweep re-delivers %d requests, want the stale one kept (1)", n)
	}
	if got := recv(t, l2); got.ID != id {
		t.Fatalf("re-delivered %s, want %s", got.ID, id)
	}
	watch.ended(t, l2)
	r.Unregister(l2.Key(), l2.Token)

	setVar(t, &sweepEvery, 0)
	l3, n := watch.register(t, r, k("op"))
	if n != 0 {
		t.Fatalf("a subscribe once sweepEvery has passed re-delivers %d stale requests, want 0", n)
	}
	noMessage(t, l3)
	checkAccounting(t, r)
}

// TestRegistryConcurrentPublishAndRedelivery: registers, publishes, answers,
// retirements and re-deliveries interleave freely under the race detector,
// and traffic really flows: requests are delivered and answered. At the end
// the registry's counts are exactly what it holds, and a fresh listener is
// re-delivered exactly the requests still pending, which are all on live
// connections and include a known set published last.
func TestRegistryConcurrentPublishAndRedelivery(t *testing.T) {
	setVar(t, &publishWait, 10*time.Millisecond)
	watch := watchRedelivery(t)
	r := NewRegistry()
	var (
		wg, pubs                     sync.WaitGroup
		published, received, answers atomic.Int64
		retired                      sync.Map // connection generation -> true
		finalGens                    [8]uint64
		stop                         = make(chan struct{})
		listening                    = make(chan struct{})
		listeningOnce                sync.Once
		liveOn                       = func(gen uint64) func() bool { return func() bool { _, gone := retired.Load(gen); return !gone } }
		publishOn                    = func(gen uint64, shed string) (*Envelope, error) {
			req := request("op", shed, "", nil)
			return req, r.publish(context.Background(), "u", gen, req, liveOn(gen))
		}
	)
	for range 4 {
		wg.Add(1)
		go func() { // a listener that reconnects, and answers every other request
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				l, err := r.Register(uk("u", "op"))
				if err != nil {
					continue
				}
				listeningOnce.Do(func() { close(listening) })
				for i := range 3 {
					select {
					case m := <-l.Messages:
						env := l.Take(m)
						if env == nil {
							continue
						}
						received.Add(1)
						if env.Type == MessageTypeRequest && i%2 == 0 {
							if answered(r, "u", "op", env.Shed.Name, env.ID, true) {
								answers.Add(1)
							}
						}
					case <-time.After(time.Millisecond):
					}
				}
				r.Unregister(l.Key(), l.Token)
			}
		}()
	}
	<-listening // publishers start once a listener holds the key
	for i := range 8 {
		pubs.Add(1)
		go func() {
			defer pubs.Done()
			shed := "s" + string(rune('a'+i))
			gen := uint64(i+1) * 1000
			for j := range 50 {
				if _, err := publishOn(gen, shed); err == nil {
					published.Add(1)
				}
				if j%10 == 9 && j != 49 { // a reconnect: the old connection is retired
					retired.Store(gen, true)
					r.prune(gen)
					gen++
				}
			}
			finalGens[i] = gen // never retired
		}()
	}
	pubs.Wait()
	close(stop)
	wg.Wait()

	if published.Load() == 0 || received.Load() == 0 || answers.Load() == 0 {
		t.Fatalf("no traffic flowed: %d published, %d received, %d answered", published.Load(), received.Load(), answers.Load())
	}

	// A known tail: one request more on every connection still live, taken by
	// a listener that answers nothing, so the final set cannot be empty.
	setVar(t, &publishWait, time.Hour)
	tail, _ := watch.register(t, r, uk("u", "op"))
	stopTaking, taken := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(taken)
		for {
			select {
			case m := <-tail.Messages:
				tail.Take(m)
			case <-stopTaking:
				return
			}
		}
	}()
	known := map[string]bool{}
	for i, gen := range finalGens {
		req, err := publishOn(gen, "s"+string(rune('a'+i)))
		if err != nil {
			t.Fatalf("publishing on live connection %d: %v", gen, err)
		}
		known[req.ID] = true
	}
	watch.ended(t, tail)
	close(stopTaking)
	<-taken
	r.Unregister(tail.Key(), tail.Token)

	checkAccounting(t, r)
	r.mu.Lock()
	want := map[string]bool{}
	for id, m := range r.pending {
		if _, gone := retired.Load(m.conn); gone {
			r.mu.Unlock()
			t.Fatalf("request %s of retired connection %d is still pending", id, m.conn)
		}
		want[id] = true
	}
	r.mu.Unlock()
	for id := range known {
		if !want[id] {
			t.Fatalf("request %s, published last on a live connection, is not pending", id)
		}
	}
	l, n := watch.register(t, r, uk("u", "op"))
	if n != len(want) {
		t.Fatalf("Register re-delivers %d requests, want the %d still pending", n, len(want))
	}
	got := map[string]bool{}
	for len(got) < len(want) {
		env := recv(t, l)
		if !want[env.ID] || got[env.ID] {
			t.Fatalf("re-delivered %s, which is not pending (or was sent twice)", env.ID)
		}
		got[env.ID] = true
	}
	watch.ended(t, l)
	noMessage(t, l)
}

// TestBridgeConcurrentRouteChanges: publishes, answers, changes of the
// shed's user, route holds and reconnects interleave freely under the race
// detector without a deadlock, and when the shed is gone the registry holds
// and counts nothing of it.
func TestBridgeConcurrentRouteChanges(t *testing.T) {
	setVar(t, &publishWait, 10*time.Millisecond)
	b, reg, alice, bob := usersBridge(t)
	var gen atomic.Uint64
	register := func() {
		gen.Store(b.RegisterShed("box", &ShedConn{User: "alice", CreatedAt: t1, Send: nopSend}))
	}
	register()
	var work, readers sync.WaitGroup
	var delivered, published atomic.Int64
	stop := make(chan struct{})
	for _, l := range []*Listener{alice, bob} {
		readers.Add(1)
		go func() { // the user's machine: takes each request and answers it
			defer readers.Done()
			for {
				select {
				case m := <-l.Messages:
					if env := l.Take(m); env != nil && b.Respond(l.User, answer("ssh-agent", "box", env.ID)) == nil {
						delivered.Add(1)
					}
				case <-stop:
					return
				}
			}
		}()
	}
	for range 4 {
		work.Add(1)
		go func() { // the guest
			defer work.Done()
			for range 300 {
				if b.PublishToHost(context.Background(), "box", gen.Load(), request("ssh-agent", "box", "", nil)) == nil {
					published.Add(1)
				}
			}
		}()
	}
	work.Add(1)
	go func() { // whoever changes the shed's route
		defer work.Done()
		for i := range 300 {
			switch i % 4 {
			case 0:
				b.SetShedUser("box", t1, "bob")
			case 1:
				b.SetShedUser("box", t1, "alice")
			case 2:
				b.ResumeRoute("box", b.SuspendRoute("box", t1))
			case 3:
				register()
			}
		}
	}()
	within(t, "the publishers and the route changes", work.Wait)
	if published.Load() == 0 {
		t.Fatal("no request was published")
	}
	b.UnregisterShed("box")
	close(stop)
	readers.Wait()
	checkAccounting(t, reg)
	if len(reg.pending) != 0 || reg.totalBytes != 0 {
		t.Fatalf("with the shed gone the registry holds %d pending requests, %d bytes", len(reg.pending), reg.totalBytes)
	}
}
