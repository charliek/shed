package vmutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/plugin"
)

// Bounds on what a shed's message channel does with what its guest sends and
// with responses written back to it. Interleavings are forced with the
// package's test hooks and barriers.

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

// heartbeat feeds h a health heartbeat from its guest reporting startedAt.
func heartbeat(t *testing.T, h *MessageHandler, startedAt time.Time) {
	t.Helper()
	payload, err := json.Marshal(plugin.HeartbeatPayload{StartedAt: startedAt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeEvent, payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.OnMessage(agentproto.MsgTypePluginMessage, data); err != nil {
		t.Fatal(err)
	}
}

// TestReplacedChannelHeartbeatIsIgnored: when a shed's channel is replaced
// (the shed created again under its name), the new incarnation starts with
// no health state, and a heartbeat the old channel delivers afterwards (its
// callback was already running) never overwrites what the new channel
// reported.
func TestReplacedChannelHeartbeatIsIgnored(t *testing.T) {
	ht := NewHealthTracker()
	cm := NewCredentialManager(nil, plugin.NewBridge(plugin.NewRegistry(), true), "test", ht)
	t.Cleanup(cm.Close)
	ctx := context.Background()
	oldStart := time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)
	newStart := oldStart.Add(time.Hour)

	cm.SetupCredentials(ctx, newResumeTestAgent(), "box", nil, nil, BusIdentityOf("", "alice", busT1))
	old := cm.handlerFor("box")
	heartbeat(t, old, oldStart)
	if hs, ok := ht.Get("box"); !ok || !hs.AgentStartedAt.Equal(oldStart) {
		t.Fatalf("test set-up: health = (%+v, %v), want the first channel's", hs, ok)
	}

	cm.SetupCredentials(ctx, newResumeTestAgent(), "box", nil, nil, BusIdentityOf("", "bob", busT2))
	if hs, ok := ht.Get("box"); ok {
		t.Fatalf("the new incarnation shows the replaced channel's health %+v", hs)
	}
	heartbeat(t, cm.handlerFor("box"), newStart)
	heartbeat(t, old, oldStart.Add(time.Minute)) // the old channel's callback, late
	hs, ok := ht.Get("box")
	if !ok || !hs.AgentStartedAt.Equal(newStart) {
		t.Fatalf("health after a late heartbeat of the replaced channel = (%v, %v), want the new channel's %v", hs.AgentStartedAt, ok, newStart)
	}

	// The replaced channel being stopped late does not remove the new one's.
	cm.healthTracker.RemoveFrom("box", 1)
	if _, ok := ht.Get("box"); !ok {
		t.Fatal("removing the replaced channel's claim removed the new channel's health")
	}
}

// captureRejectLines collects the rate-limited lines about what guests sent.
func captureRejectLines(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	setVar(t, &rejectLogf, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

// frame is one frame from a guest.
type frame struct {
	typ  byte
	data []byte
}

func pluginFrame(t *testing.T, env *plugin.Envelope) frame {
	t.Helper()
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return frame{agentproto.MsgTypePluginMessage, data}
}

// TestGuestRejectionsAreLoggedAtABoundedRate: what a guest sends that is
// refused costs at most one log line per rejectLogEvery per channel for each
// kind of refusal: malformed traffic (an envelope that does not parse, an
// unknown frame type, a bad heartbeat), a message past a limit (here an
// over-long identifier), and a message that could not be delivered (nobody
// listens). The next line of a kind says how many of that kind were left
// out, and no line carries a guest-chosen identifier whole.
func TestGuestRejectionsAreLoggedAtABoundedRate(t *testing.T) {
	lines := captureRejectLines(t)
	setVar(t, &rejectLogEvery, time.Hour)
	cm := NewCredentialManager(nil, plugin.NewBridge(plugin.NewRegistry(), false), "test", NewHealthTracker())
	t.Cleanup(cm.Close)
	cm.SetupCredentials(context.Background(), newResumeTestAgent(), "box", nil, nil, BusIdentity{CreatedAt: busT1})
	h := cm.handlerFor("box")

	long := strings.Repeat("z", 4096)
	longID := plugin.NewEnvelope("ssh-agent", plugin.MessageTypeRequest, nil)
	longID.ID = long
	refused := []frame{
		pluginFrame(t, longID),                                 // past a limit
		{agentproto.MsgTypePluginMessage, []byte("{not json")}, // malformed
		{0x7f, nil}, // malformed
		pluginFrame(t, plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeEvent, json.RawMessage(`{}`))), // malformed
		pluginFrame(t, plugin.NewEnvelope("ssh-agent", plugin.MessageTypeRequest, nil)),                            // nobody listens
	}
	send := func() {
		t.Helper()
		for _, f := range refused {
			if err := h.OnMessage(f.typ, f.data); err != nil {
				t.Fatalf("OnMessage(0x%02x) = %v, want the refusal logged and nil", f.typ, err)
			}
		}
	}
	for range 50 {
		send()
	}
	if got := lines(); len(got) != 3 {
		t.Fatalf("250 refused messages of three kinds wrote %d log lines, want 3:\n%s", len(got), strings.Join(got, "\n"))
	}

	setVar(t, &rejectLogEvery, 0)
	send()
	got := lines()
	if len(got) != 3+len(refused) {
		t.Fatalf("with no rate limit five more refusals wrote %d lines, want 5", len(got)-3)
	}
	// The first line of each kind after the quiet hour counts what that kind
	// left out: 49 over-long identifiers, 149 malformed frames, 49 undelivered.
	for i, want := range map[int]string{3: "(49 similar", 4: "(149 similar", 7: "(49 similar"} {
		if !strings.Contains(got[i], want) {
			t.Errorf("line %d = %q, want it to say %q line(s) left out", i, got[i], want+"...")
		}
	}
	for _, l := range got {
		if strings.Contains(l, long[:64]) {
			t.Fatalf("a log line carries the guest's identifier: %.200s", l)
		}
	}
}

// TestMalformedTrafficCannotHideOtherRefusals: each kind of refusal has a
// rate limit of its own. A guest that sends an unparseable frame first in
// every interval does not keep a limit refusal, or an undelivered request,
// out of the log.
func TestMalformedTrafficCannotHideOtherRefusals(t *testing.T) {
	lines := captureRejectLines(t)
	setVar(t, &rejectLogEvery, time.Hour)
	cm := NewCredentialManager(nil, plugin.NewBridge(plugin.NewRegistry(), false), "test", NewHealthTracker())
	t.Cleanup(cm.Close)
	cm.SetupCredentials(context.Background(), newResumeTestAgent(), "box", nil, nil, BusIdentity{CreatedAt: busT1})
	h := cm.handlerFor("box")
	feed := func(f frame) {
		t.Helper()
		if err := h.OnMessage(f.typ, f.data); err != nil {
			t.Fatal(err)
		}
	}

	feed(frame{agentproto.MsgTypePluginMessage, []byte("{not json")}) // first in the interval
	longID := plugin.NewEnvelope("ssh-agent", plugin.MessageTypeRequest, nil)
	longID.ID = strings.Repeat("z", 4096)
	feed(pluginFrame(t, longID))
	feed(pluginFrame(t, plugin.NewEnvelope("ssh-agent", plugin.MessageTypeRequest, nil)))

	got := strings.Join(lines(), "\n")
	for _, want := range []string{"invalid plugin envelope", "identifier is too long", "no listener registered"} {
		if !strings.Contains(got, want) {
			t.Errorf("the log has no line for %q; a malformed frame earlier in the interval hid it:\n%s", want, got)
		}
	}
}

// TestGuestTextInLogLinesIsClipped: a log line or an error never carries
// more than a small fixed length of what a guest chose, including a decode
// error that quotes the field it failed on. A multi-megabyte timestamp that
// does not parse, and a heartbeat with such a started_at, each produce a
// short line.
func TestGuestTextInLogLinesIsClipped(t *testing.T) {
	lines := captureRejectLines(t)
	setVar(t, &rejectLogEvery, 0)
	cm := NewCredentialManager(nil, plugin.NewBridge(plugin.NewRegistry(), false), "test", NewHealthTracker())
	t.Cleanup(cm.Close)
	cm.SetupCredentials(context.Background(), newResumeTestAgent(), "box", nil, nil, BusIdentity{CreatedAt: busT1})
	h := cm.handlerFor("box")

	huge := strings.Repeat("9", 2<<20)
	badTimestamp := []byte(`{"id":"a","namespace":"ssh-agent","type":"request","final":true,"timestamp":"` + huge + `","payload":null}`)
	badHeartbeat := pluginFrame(t, plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeEvent,
		json.RawMessage(`{"started_at":"`+huge+`"}`)))
	for _, f := range []frame{{agentproto.MsgTypePluginMessage, badTimestamp}, badHeartbeat} {
		if err := h.OnMessage(f.typ, f.data); err != nil {
			t.Fatal(err)
		}
	}
	got := lines()
	if len(got) != 2 {
		t.Fatalf("two undecodable messages wrote %d lines, want 2", len(got))
	}
	const most = maxLoggedGuestText + 200 // the clipped text and the line's own words
	for _, l := range got {
		if len(l) > most {
			t.Errorf("a log line about a guest's message is %d bytes (at most %d): %.120s...", len(l), most, l)
		}
	}
	if !strings.Contains(got[0], "invalid plugin envelope") || !strings.Contains(got[1], "Invalid heartbeat payload") {
		t.Errorf("the lines do not say what was refused: %.120q", got)
	}

	// A handler with no rate-limited log returns the error to its read loop,
	// which logs it: that error is clipped too.
	bare := NewMessageHandler(nil, nil, nil)
	err := bare.OnMessage(agentproto.MsgTypePluginMessage, badTimestamp)
	if err == nil || len(err.Error()) > most {
		t.Fatalf("the error for an undecodable envelope is %d bytes (at most %d)", len(fmt.Sprint(err)), most)
	}
}

// readFrames reads the frames written to a guest's end of a transport, in
// order, until it is closed. Started before the write under test, it lets a
// frame that must not be written show up as the wrong first frame, at once,
// instead of as a write nobody takes.
func readFrames(guest net.Conn) <-chan string {
	frames := make(chan string, 8)
	go func() {
		defer close(frames)
		for {
			_, data, err := agentproto.ReadMessage(guest)
			if err != nil {
				return
			}
			frames <- string(data)
		}
	}()
	return frames
}

// expectMarkerNext writes a marker frame with send and fails unless it is the
// next frame the guest reads: nothing else was written before it.
func expectMarkerNext(t *testing.T, frames <-chan string, send func(*plugin.Envelope) error, what string) {
	t.Helper()
	if err := send(plugin.NewEnvelope("marker", plugin.MessageTypeEvent, nil)); err != nil {
		t.Fatalf("writing the marker: %v", err)
	}
	select {
	case data := <-frames:
		if !strings.Contains(data, `"marker"`) {
			t.Fatalf("the guest's next frame is %s, want the marker: %s", data, what)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the guest never read the marker")
	}
}

// pipeHandler is a handler holding the host end of a pipe as its transport.
func pipeHandler(t *testing.T) (h *MessageHandler, host, guest net.Conn) {
	t.Helper()
	h = NewMessageHandler(nil, nil, nil)
	host, guest = net.Pipe()
	t.Cleanup(func() { _ = host.Close(); _ = guest.Close() })
	h.conn = host
	return h, host, guest
}

// TestSendOnGivesUpAtItsDeadline: a response's one deadline covers its wait
// for the transport's write lock and its write. Held behind a write that
// does not end, it gives up at its deadline with nothing written (a
// plugin.ErrResponseTimeout: its request is not consumed); written to a
// guest that does not read, it closes the transport at its deadline, however
// long the handler's own write bound is.
func TestSendOnGivesUpAtItsDeadline(t *testing.T) {
	env := plugin.NewEnvelope("ssh-agent", plugin.MessageTypeResponse, nil)
	t.Run("waiting for the write lock", func(t *testing.T) {
		h, host, _ := pipeHandler(t)
		h.lock() // a write in progress that does not end
		defer h.unlock()
		admitted, wrote := false, false
		var err error
		within(t, "a response behind a held write lock", func() {
			err = h.sendOn(host, env, time.Now().Add(20*time.Millisecond),
				func() error { admitted = true; return nil }, func() { wrote = true })
		})
		if !errors.Is(err, plugin.ErrResponseTimeout) {
			t.Fatalf("a response behind a held write lock = %v, want a plugin.ErrResponseTimeout", err)
		}
		if admitted || wrote {
			t.Fatalf("a response that never got the write lock was admitted (%v) or reported written (%v)", admitted, wrote)
		}
	})
	t.Run("guest not reading", func(t *testing.T) {
		setVar(t, &guestWriteTimeout, time.Hour)
		h, host, guest := pipeHandler(t)
		wrote := false
		var err error
		within(t, "a response to a guest that does not read", func() {
			err = h.sendOn(host, env, time.Now().Add(20*time.Millisecond), nil, func() { wrote = true })
		})
		if err == nil || wrote {
			t.Fatalf("a write the guest never took = %v (reported written: %v)", err, wrote)
		}
		if errors.Is(err, plugin.ErrResponseTimeout) {
			t.Fatalf("a write that may be half done reads as nothing written: %v", err)
		}
		if _, rerr := guest.Read(make([]byte, 1)); !errors.Is(rerr, io.EOF) {
			t.Fatalf("the guest's end after the write gave up reads %v, want the transport closed (EOF)", rerr)
		}
	})
}

// TestSendOnAdmitsUnderTheWriteLock: a response is admitted with the write
// lock held, after any wait for it and immediately before its write; one
// that is not admitted writes nothing; and wrote runs only once the whole
// frame has been written, still under the lock.
func TestSendOnAdmitsUnderTheWriteLock(t *testing.T) {
	env := plugin.NewEnvelope("ssh-agent", plugin.MessageTypeResponse, nil)
	refused := errors.New("no longer pending")
	locked := func(h *MessageHandler) bool { return len(h.writeLock) == 1 }

	t.Run("refused after waiting for the lock", func(t *testing.T) {
		h, host, guest := pipeHandler(t)
		h.lock() // a write in progress
		waiting := make(chan struct{})
		setVar(t, &lockWaitHook, func() { close(waiting) })
		allowed := true // what admit would have said before the wait
		done := make(chan error, 1)
		go func() {
			done <- h.sendOn(host, env, time.Now().Add(time.Hour), func() error {
				if !locked(h) {
					t.Error("admit ran without the write lock held")
				}
				if !allowed {
					return refused
				}
				return nil
			}, func() { t.Error("a refused response reported written") })
		}()
		<-waiting       // queued behind the write in progress
		allowed = false // the route changes while it waits
		frames := readFrames(guest)
		h.unlock()
		if err := <-done; !errors.Is(err, refused) {
			t.Fatalf("a response refused under the write lock = %v, want admit's error", err)
		}
		expectMarkerNext(t, frames, h.sendEnvelope, "the refused response was written")
	})
	t.Run("written, then reported", func(t *testing.T) {
		h, host, guest := pipeHandler(t)
		read := make(chan error, 1)
		go func() {
			_, _, err := agentproto.ReadMessage(guest)
			read <- err
		}()
		var order []string
		err := h.sendOn(host, env, time.Now().Add(time.Hour),
			func() error { order = append(order, "admit"); return nil },
			func() {
				if !locked(h) {
					t.Error("wrote ran without the write lock held")
				}
				order = append(order, "wrote")
			})
		if err != nil {
			t.Fatal(err)
		}
		if err := <-read; err != nil {
			t.Fatalf("the guest reading the frame: %v", err)
		}
		if got := strings.Join(order, ","); got != "admit,wrote" {
			t.Fatalf("order = %s, want admit, then wrote", got)
		}
	})
}

// channelWithTransport starts box's message channel for alice on a bridge
// with a user list, connects its transport, and has the guest make one
// request, which alice's listener receives.
func channelWithTransport(t *testing.T) (cm *CredentialManager, bridge *plugin.Bridge, alice *plugin.Listener, guest net.Conn, req *plugin.Envelope, guestID string) {
	t.Helper()
	reg := plugin.NewRegistry()
	bridge = plugin.NewBridge(reg, true)
	cm = NewCredentialManager(nil, bridge, "test", NewHealthTracker())
	t.Cleanup(cm.Close)
	alice, err := reg.Register(plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"})
	if err != nil {
		t.Fatal(err)
	}
	d := newTransportDialer()
	cm.SetupCredentials(context.Background(), NewAgentClient(d, 1024, 1026), "box", nil, nil, BusIdentityOf("", "alice", busT1))
	guest = d.connect(t)
	guestID = guestSends(t, guest)
	return cm, bridge, alice, guest, expectRequest(t, alice, "box"), guestID
}

// TestQueuedResponseIsNotWrittenAfterTheRouteChanges: an answer that passed
// every check and waits for the transport's write lock (a write to the
// guest is in progress) is not written once it gets the lock if the shed's
// route was held, or its user set, while it waited. This is the real
// transport: the bridge's check runs in sendOn, under the write lock.
func TestQueuedResponseIsNotWrittenAfterTheRouteChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(cm *CredentialManager)
	}{
		{"route held", func(cm *CredentialManager) { cm.SuspendShedRoute("box", busT1) }},
		{"user set", func(cm *CredentialManager) { cm.SetShedUser("box", busT1, "bob") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cm, bridge, _, guest, req, _ := channelWithTransport(t)
			h := cm.handlerFor("box")
			h.lock() // a write to box's guest in progress
			waiting := make(chan struct{})
			setVar(t, &lockWaitHook, func() { close(waiting) })
			done := make(chan error, 1)
			go func() { done <- bridge.Respond("alice", answerTo(req)) }()
			<-waiting // alice's answer is queued behind the write

			within(t, "the route change while a write is in progress", func() { tc.change(cm) })

			frames := readFrames(guest)
			h.unlock()
			if err := <-done; !errors.Is(err, plugin.ErrUnmatched) {
				t.Fatalf("the answer queued across the route change = %v, want plugin.ErrUnmatched", err)
			}
			expectMarkerNext(t, frames, h.sendEnvelope, "alice's answer was written after the route changed")
		})
	}
}

// TestAWriteInProgressHoldsNothingUp: while a write to one shed's guest is
// in progress and an answer is queued behind it, that shed's user is set and
// other sheds stop and start without waiting: no lock of the manager or the
// bridge is held across a write.
func TestAWriteInProgressHoldsNothingUp(t *testing.T) {
	cm, bridge, _, guest, req, guestID := channelWithTransport(t)
	cm.SetupCredentials(context.Background(), newResumeTestAgent(), "other", nil, nil, BusIdentityOf("", "alice", busT1))
	h := cm.handlerFor("box")
	h.lock() // a write to box's guest in progress
	waiting := make(chan struct{})
	setVar(t, &lockWaitHook, func() { close(waiting) })
	done := make(chan error, 1)
	go func() { done <- bridge.Respond("alice", answerTo(req)) }()
	<-waiting

	within(t, "an assignment, and other sheds' stop and start, during a write", func() {
		// The user is set to what it is: the route does not change, and the
		// queued answer is still good.
		if !cm.SetShedUser("box", busT1, "alice") {
			t.Error("SetShedUser refused")
		}
		cm.StopListener("other")
		cm.SetupCredentials(context.Background(), newResumeTestAgent(), "third", nil, nil, BusIdentityOf("", "alice", busT1))
	})

	frames := readFrames(guest)
	h.unlock()
	if err := <-done; err != nil {
		t.Fatalf("alice's answer: %v", err)
	}
	if data := <-frames; !strings.Contains(data, guestID) {
		t.Fatalf("box's guest read %s, want the answer to its request %s", data, guestID)
	}
}

// gatedCloseConn is a transport whose Close, once called, waits for release
// before it closes the underlying transport, so a test can hold the write
// timer's close begun but not yet done. closed reports whether Close was
// called at all.
type gatedCloseConn struct {
	net.Conn
	closing     chan struct{} // closed when Close is first called
	release     chan struct{}
	closingOnce sync.Once
	releaseOnce sync.Once
	closed      atomic.Bool
}

func newGatedCloseConn(t *testing.T, c net.Conn) *gatedCloseConn {
	g := &gatedCloseConn{Conn: c, closing: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

// open lets every Close, begun or to come, close the transport.
func (c *gatedCloseConn) open() { c.releaseOnce.Do(func() { close(c.release) }) }

func (c *gatedCloseConn) Close() error {
	c.closed.Store(true)
	c.closingOnce.Do(func() { close(c.closing) })
	<-c.release
	return c.Conn.Close()
}

// TestAWrittenFinalAnswerIsConsumedAsItsDeadlinePasses: a frame whose write
// returned success is written, even when the write's deadline passed just as
// it finished (its timer has fired and begun to close the transport, and
// stopping it fails). The answer is reported written and consumes its
// request under the write lock, so a second final answer queued behind it
// is refused before the timer's close is done, and the guest reads one
// answer, not two.
func TestAWrittenFinalAnswerIsConsumedAsItsDeadlinePasses(t *testing.T) {
	h, host, guest := pipeHandler(t)
	conn := newGatedCloseConn(t, host)
	h.conn = conn
	frames := readFrames(guest)
	// The frame is written; the hook then waits until the write's timer has
	// fired and its close has begun (held by the gate), so the timer cannot
	// be stopped.
	setVar(t, &wroteFrameHook, func(err error) {
		if err != nil {
			t.Errorf("the frame's write: %v", err)
		}
		<-conn.closing
	})

	consumed, writes := false, 0
	refused := errors.New("no longer pending")
	admit := func() error {
		if consumed {
			return refused
		}
		return nil
	}
	wrote := func() { writes++; consumed = true }
	env := plugin.NewEnvelope("ssh-agent", plugin.MessageTypeResponse, nil)
	env.Final = true

	var first error
	within(t, "a final answer whose deadline passes as its write ends", func() {
		first = h.sendOn(conn, env, time.Now().Add(100*time.Millisecond), admit, wrote)
	})
	if first != nil {
		t.Fatalf("a frame written in full = %v, want it reported written", first)
	}
	// A second final answer, queued behind the first, gets the write lock
	// while the timer's close is still held.
	var second error
	within(t, "a second final answer", func() {
		second = h.sendOn(conn, env, time.Now().Add(time.Hour), admit, wrote)
	})
	if !errors.Is(second, refused) {
		t.Fatalf("a second final answer after the first was written = %v, want it refused", second)
	}
	if writes != 1 {
		t.Fatalf("wrote ran %d times, want once", writes)
	}

	conn.open() // the timer's close completes
	n := 0
	for range frames {
		n++
	}
	if n != 1 {
		t.Fatalf("the guest read %d answers, want 1", n)
	}
}

// failingConn is a transport whose writes fail after the first.
type failingConn struct {
	net.Conn
	writes int
	closed atomic.Bool
}

var errBrokenWrite = errors.New("broken write")

func (c *failingConn) Write(p []byte) (int, error) {
	c.writes++
	if c.writes > 1 {
		return 0, errBrokenWrite
	}
	return c.Conn.Write(p)
}

func (c *failingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// TestAFailedWriteClosesTheTransport: a write that fails on its own (the
// timer did not fire) part-way through a frame closes the transport, as a
// write that times out does, so nothing can be written after the half
// frame; it is not reported written, and it is not a
// plugin.ErrResponseTimeout (something may have been written, so it is not
// safe to retry).
func TestAFailedWriteClosesTheTransport(t *testing.T) {
	h, host, guest := pipeHandler(t)
	conn := &failingConn{Conn: host}
	h.conn = conn
	frames := readFrames(guest)
	env := plugin.NewEnvelope("ssh-agent", plugin.MessageTypeResponse, nil)

	wrote := false
	var err error
	within(t, "a write that fails part-way", func() {
		err = h.sendOn(conn, env, time.Now().Add(time.Hour), nil, func() { wrote = true })
	})
	if !errors.Is(err, errBrokenWrite) || wrote {
		t.Fatalf("a write that failed part-way = %v (reported written: %v), want its error, not written", err, wrote)
	}
	if errors.Is(err, plugin.ErrResponseTimeout) {
		t.Fatalf("a write that may be half done reads as nothing written: %v", err)
	}
	if !conn.closed.Load() {
		t.Fatal("the transport was left open after a half-written frame")
	}
	for data := range frames { // ends once the guest sees the transport closed
		t.Fatalf("the guest read a frame after a failed write: %s", data)
	}
}
