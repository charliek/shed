package vmutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
)

func TestNewCredentialManager_NilConfig(t *testing.T) {
	cm := NewCredentialManager(nil, nil, "test", nil)
	if cm == nil {
		t.Fatal("NewCredentialManager returned nil")
	}

	if cm.messageChannels == nil {
		t.Error("messageChannels should be initialized, got nil")
	}

	cm.Close()
}

func TestNewCredentialManager_EmptyCredentials(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Mounts: map[string]config.MountConfig{},
	}

	cm := NewCredentialManager(serverCfg, nil, "test", nil)
	if cm == nil {
		t.Fatal("NewCredentialManager returned nil")
	}

	cm.Close()
}

func TestCredentialManager_StopListenerNoOp(t *testing.T) {
	cm := NewCredentialManager(nil, nil, "test", nil)

	cm.StopListener("nonexistent-vm")
	cm.StopListener("nonexistent-vm")
	cm.StopListener("")
	cm.StopListener("another-name")
}

func TestCredentialManager_Close(t *testing.T) {
	cm := NewCredentialManager(nil, nil, "test", nil)
	cm.Close()
	cm.Close()
}

func TestCredentialManager_CloseWithEmptyListeners(t *testing.T) {
	serverCfg := &config.ServerConfig{
		Mounts: map[string]config.MountConfig{},
	}

	cm := NewCredentialManager(serverCfg, nil, "test", nil)

	cm.mu.Lock()
	listenerCount := len(cm.messageChannels)
	cm.mu.Unlock()

	if listenerCount != 0 {
		t.Errorf("expected 0 channels, got %d", listenerCount)
	}

	cm.Close()
}

func TestCredentialManager_StopListenerThenClose(t *testing.T) {
	cm := NewCredentialManager(nil, nil, "test", nil)

	cm.StopListener("vm-1")
	cm.StopListener("vm-2")
	cm.Close()
}

// idleDialer is a Dialer whose Dial waits until its context ends: the
// channel's transport never connects, so the only registration is the one
// the channel makes when it starts, and a test that feeds the handler
// directly races no reconnect.
type idleDialer struct{}

func (idleDialer) Dial(ctx context.Context, _ uint32) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// newResumeTestAgent builds an AgentClient whose transport never connects, so
// the message-channel tests below drive the real NotifyConn path without a
// guest on the other end.
func newResumeTestAgent() *AgentClient {
	return NewAgentClient(idleDialer{}, 1024, 1026)
}

// newResumeTestManager returns a manager wired to a real plugin bridge —
// bridge registration is the observable these tests assert on, because it is
// exactly what a restarted server used to lose (#315).
func newResumeTestManager(t *testing.T) (*CredentialManager, *plugin.Bridge) {
	t.Helper()
	bridge := plugin.NewBridge(plugin.NewRegistry(), false)
	cm := NewCredentialManager(nil, bridge, "test", NewHealthTracker())
	t.Cleanup(cm.Close)
	return cm, bridge
}

// bridgeNames is the sorted list of sheds currently registered on the
// bridge — the observable the resume tests assert on, because a lost bridge
// registration is half of what #315 cost a restarted server.
func bridgeNames(b *plugin.Bridge) []string {
	infos := b.ListSheds("", true)
	names := make([]string, 0, len(infos))
	for _, i := range infos {
		names = append(names, i.Name)
	}
	sort.Strings(names)
	return names
}

// TestResumeMessageChannel covers the manager-level half of #315: a resume
// registers the shed on the bridge, and the shared dedupe holds in both
// orders so a StartShed either side of the startup walk opens one channel.
func TestResumeMessageChannel(t *testing.T) {
	t.Run("registers_on_bridge", func(t *testing.T) {
		cm, bridge := newResumeTestManager(t)

		cm.ResumeMessageChannel("shed-a", newResumeTestAgent(), BusIdentity{})

		if got := bridgeNames(bridge); len(got) != 1 || got[0] != "shed-a" {
			t.Fatalf("bridge sheds = %v, want [shed-a]", got)
		}
		if n := cm.channelCount(); n != 1 {
			t.Fatalf("messageChannels = %d, want 1", n)
		}
	})

	// NEGATIVE CONTROL for the case above: the identical fixture with the
	// ResumeMessageChannel call removed must leave the bridge empty. Without
	// it, "registers_on_bridge" could be passing on a registration some other
	// part of the fixture performed.
	t.Run("control_no_resume_leaves_bridge_empty", func(t *testing.T) {
		cm, bridge := newResumeTestManager(t)

		// Deliberately no ResumeMessageChannel call.
		_ = newResumeTestAgent()

		if got := bridgeNames(bridge); len(got) != 0 {
			t.Fatalf("bridge sheds = %v, want empty without a resume", got)
		}
		if n := cm.channelCount(); n != 0 {
			t.Fatalf("messageChannels = %d, want 0 without a resume", n)
		}
	})

	// Idempotence: a StartShed landing after the startup resume walk runs
	// SetupCredentials for a name that already has a channel. It must not
	// open a second NotifyConn onto the same guest.
	t.Run("resume_then_setup_opens_one_channel", func(t *testing.T) {
		cm, bridge := newResumeTestManager(t)

		cm.ResumeMessageChannel("shed-a", newResumeTestAgent(), BusIdentity{})
		first := cm.channel("shed-a")
		if first == nil {
			t.Fatal("resume did not open a channel")
		}

		cm.SetupCredentials(context.Background(), newResumeTestAgent(), "shed-a", nil, nil, BusIdentity{})

		if n := cm.channelCount(); n != 1 {
			t.Fatalf("messageChannels = %d after resume+setup, want 1", n)
		}
		if got := cm.channel("shed-a"); got != first {
			t.Fatalf("SetupCredentials replaced the resumed channel (%p -> %p)", first, got)
		}
		if got := bridgeNames(bridge); len(got) != 1 || got[0] != "shed-a" {
			t.Fatalf("bridge sheds = %v, want [shed-a]", got)
		}
	})

	// The mirror case: the resume walk racing in after a normal start.
	t.Run("setup_then_resume_opens_one_channel", func(t *testing.T) {
		cm, _ := newResumeTestManager(t)

		cm.SetupCredentials(context.Background(), newResumeTestAgent(), "shed-a", nil, nil, BusIdentity{})
		first := cm.channel("shed-a")
		if first == nil {
			t.Fatal("SetupCredentials did not open a channel")
		}

		cm.ResumeMessageChannel("shed-a", newResumeTestAgent(), BusIdentity{})

		if n := cm.channelCount(); n != 1 {
			t.Fatalf("messageChannels = %d after setup+resume, want 1", n)
		}
		if got := cm.channel("shed-a"); got != first {
			t.Fatalf("ResumeMessageChannel replaced the live channel (%p -> %p)", first, got)
		}
	})
}

// TestStaleAdminChannelNotReusedForOwnedShed: an admin shed's channel left
// behind under a name (a teardown that missed StopListener) is replaced, not
// reused, when a service client creates an owned shed with that name, so the
// owned shed never inherits the admin shed's credential-bus access.
func TestStaleAdminChannelNotReusedForOwnedShed(t *testing.T) {
	reg := plugin.NewRegistry()
	bridge := plugin.NewBridge(reg, false)
	cm := NewCredentialManager(nil, bridge, "test", NewHealthTracker())
	t.Cleanup(cm.Close)
	implicit, err := reg.Register(plugin.ListenerKey{Namespace: "ssh-agent"})
	if err != nil {
		t.Fatal(err)
	}

	cm.SetupCredentials(context.Background(), newResumeTestAgent(), "nocturn-a", nil, nil, BusIdentity{})
	stale := cm.channel("nocturn-a")
	if got := bridgeNames(bridge); len(got) != 1 {
		t.Fatalf("admin shed not on the bridge: %v", got)
	}
	guestRequest(t, cm.handlerFor("nocturn-a"), "nocturn-a")
	adminReq := expectRequest(t, implicit, "nocturn-a")

	// No StopListener: the owned shed's setup finds the stale channel.
	cm.SetupCredentials(context.Background(), newResumeTestAgent(), "nocturn-a", nil, nil, BusIdentity{Owned: true})

	if got := cm.channel("nocturn-a"); got == nil || got == stale {
		t.Fatalf("owned setup reused the admin shed's channel (%p -> %p)", stale, got)
	}
	if n := cm.channelCount(); n != 1 {
		t.Fatalf("messageChannels = %d, want 1", n)
	}
	if got := bridgeNames(bridge); len(got) != 0 {
		t.Fatalf("owned shed listed on the bridge: %v", got)
	}
	if u, ok := bridge.ShedUser("nocturn-a"); ok {
		t.Fatalf("owned shed routes to %q", u)
	}
	// The admin shed's request, still pending when it was replaced, is gone:
	// its answer is not written into the owned shed.
	if err := bridge.Respond("", answerTo(adminReq)); !errors.Is(err, plugin.ErrUnmatched) {
		t.Fatalf("an answer to the replaced admin shed's request = %v, want ErrUnmatched", err)
	}
	// And the owned shed's own requests reach no listener.
	guestRequest(t, cm.handlerFor("nocturn-a"), "nocturn-a")
	expectNothing(t, implicit)

	// The same flag again still dedupes.
	owned := cm.channel("nocturn-a")
	cm.ResumeMessageChannel("nocturn-a", newResumeTestAgent(), BusIdentity{Owned: true})
	if got := cm.channel("nocturn-a"); got != owned {
		t.Fatalf("same-flag resume replaced the channel (%p -> %p)", owned, got)
	}
}

// channelCount and channel are test-only accessors over the manager's
// internal map (same package).
func (cm *CredentialManager) channelCount() int {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return len(cm.messageChannels)
}

// channel returns the NotifyConn registered for name, or nil. Identity of
// the returned pointer is what the idempotence tests compare, so they can
// tell "kept the existing channel" from "opened a second, equivalent one".
func (cm *CredentialManager) channel(name string) *NotifyConn {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.messageChannels[name].conn
}

// TestStartMessageChannelLifecycleRace hammers the window that #315's startup
// resume walk made reachable: the walk runs concurrently with normal serving
// and with shutdown, so ResumeMessageChannel / SetupCredentials can interleave
// with StopListener and Close on the same name.
//
// Before the fix the conn was published to the map and only THEN registered
// and started, outside the lock. Two bad interleavings lived in that window:
// Stop() reading a nil cancel and then waiting forever on a wait group Start()
// was about to add to (a shutdown hang), or Stop() finishing first and leaving
// a started conn in nobody's map, reconnecting for the life of the process.
//
// Run this with -race. The assertion is that the manager terminates and ends
// consistent with the bridge — a hang here IS the failure.
func TestStartMessageChannelLifecycleRace(t *testing.T) {
	const names = 8
	const rounds = 40

	for r := 0; r < rounds; r++ {
		bridge := plugin.NewBridge(plugin.NewRegistry(), false)
		cm := NewCredentialManager(nil, bridge, "test", NewHealthTracker())

		var wg sync.WaitGroup
		for i := 0; i < names; i++ {
			name := fmt.Sprintf("shed-%d", i)

			wg.Add(3)
			go func() { defer wg.Done(); cm.ResumeMessageChannel(name, newResumeTestAgent(), BusIdentity{}) }()
			go func() {
				defer wg.Done()
				cm.SetupCredentials(context.Background(), newResumeTestAgent(), name, nil, nil, BusIdentity{})
			}()
			go func() { defer wg.Done(); cm.StopListener(name) }()
		}
		wg.Wait()

		// Close must return. If the pre-fix interleaving is reintroduced this
		// blocks in NotifyConn.Stop's wg.Wait() and the test times out.
		cm.Close()

		// After Close the manager owns nothing, and the bridge agrees: every
		// registration Close saw was unregistered with it.
		if n := cm.channelCount(); n != 0 {
			t.Fatalf("round %d: messageChannels = %d after Close, want 0", r, n)
		}
		for _, got := range bridgeNames(bridge) {
			if ch := cm.channel(got); ch != nil {
				t.Fatalf("round %d: bridge still lists %q with a live channel", r, got)
			}
		}
	}
}

var (
	busT1 = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	busT2 = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
)

// newUsersBusManager is a manager on the bridge of a server with a user
// list, with alice's and bob's ssh-agent listeners.
func newUsersBusManager(t *testing.T) (*CredentialManager, *plugin.Bridge, *plugin.Listener, *plugin.Listener) {
	t.Helper()
	reg := plugin.NewRegistry()
	bridge := plugin.NewBridge(reg, true)
	cm := NewCredentialManager(nil, bridge, "test", NewHealthTracker())
	t.Cleanup(cm.Close)
	alice, err := reg.Register(plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := reg.Register(plugin.ListenerKey{User: "bob", Namespace: "ssh-agent"})
	if err != nil {
		t.Fatal(err)
	}
	return cm, bridge, alice, bob
}

// guestRequest feeds h a plugin request from its guest, as the message
// channel would on receiving one, and returns the ID the guest chose for it.
// The envelope names shed, which the bridge must overwrite.
func guestRequest(t *testing.T, h *MessageHandler, shed string) string {
	t.Helper()
	env := plugin.NewEnvelope("ssh-agent", plugin.MessageTypeRequest, nil)
	env.Shed = &plugin.ShedInfo{Name: shed}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.OnMessage(agentproto.MsgTypePluginMessage, data); err != nil {
		t.Fatal(err)
	}
	return env.ID
}

// expectRequest returns the next request l's stream would send, which must
// be from shed. Its ID is the server's for the request: the one an answer
// carries in in_reply_to. (The guard is against a hang.)
func expectRequest(t *testing.T, l *plugin.Listener, shed string) *plugin.Envelope {
	t.Helper()
	for {
		select {
		case m := <-l.Messages:
			env := l.Take(m)
			if env == nil {
				continue
			}
			if env.Shed == nil || env.Shed.Name != shed {
				t.Fatalf("%s's listener received %s from %+v, want a request from %s", l.User, env.ID, env.Shed, shed)
			}
			return env
		case <-time.After(10 * time.Second):
			t.Fatalf("%s's listener received nothing, want a request from %s", l.User, shed)
			return nil
		}
	}
}

// expectNothing fails if l's queue holds anything its stream would send.
func expectNothing(t *testing.T, l *plugin.Listener) {
	t.Helper()
	for {
		select {
		case m := <-l.Messages:
			if env := l.Take(m); env != nil {
				t.Fatalf("%s's listener received %s from %+v", l.User, env.ID, env.Shed)
			}
		default:
			return
		}
	}
}

// answerTo is the final answer a listener's machine posts for the request
// it was sent as req.
func answerTo(req *plugin.Envelope) *plugin.Envelope {
	resp := plugin.NewResponse(req.ID, req.Namespace, json.RawMessage(`{"ok":true}`))
	resp.Shed = req.Shed
	return resp
}

// TestMessageChannelRoutesAsTheRecordedUser: a shed's guest requests reach
// the listener of the user the server's record names, and the guest cannot
// choose another (the shed field it sends is overwritten).
func TestMessageChannelRoutesAsTheRecordedUser(t *testing.T) {
	cm, _, alice, bob := newUsersBusManager(t)
	cm.SetupCredentials(context.Background(), newResumeTestAgent(), "box", nil, nil, BusIdentityOf("", "alice", busT1))

	id := guestRequest(t, cm.handlerFor("box"), "bobs")
	if got := expectRequest(t, alice, "box"); got.ID == id {
		t.Fatalf("alice's listener was sent the guest's own ID %s", id)
	}
	expectNothing(t, bob)
}

// TestMessageChannelReplacedForAnotherIdentity: a channel is reused only for
// the same incarnation, owned flag and user; a channel left behind under the
// name for another one is replaced, and its handler can no longer publish:
// it is bound to the connection generation it was registered as.
func TestMessageChannelReplacedForAnotherIdentity(t *testing.T) {
	cm, _, alice, bob := newUsersBusManager(t)
	ctx := context.Background()

	cm.SetupCredentials(ctx, newResumeTestAgent(), "box", nil, nil, BusIdentityOf("", "alice", busT1))
	first := cm.channel("box")
	oldHandler := cm.handlerFor("box")

	// The same identity again (a resume racing a start) keeps the channel.
	cm.ResumeMessageChannel("box", newResumeTestAgent(), BusIdentityOf("", "alice", busT1))
	if got := cm.channel("box"); got != first {
		t.Fatalf("the same identity replaced the channel (%p -> %p)", first, got)
	}

	// A teardown missed StopListener, and bob creates a shed under the name.
	cm.SetupCredentials(ctx, newResumeTestAgent(), "box", nil, nil, BusIdentityOf("", "bob", busT2))
	if got := cm.channel("box"); got == first {
		t.Fatal("a shed of another user and incarnation reused the old channel")
	}

	// The old channel's handler, still alive, receives a request from the
	// guest it reaches: it must not publish as the new shed.
	guestRequest(t, oldHandler, "box")
	expectNothing(t, alice)
	expectNothing(t, bob)

	guestRequest(t, cm.handlerFor("box"), "box")
	expectRequest(t, bob, "box")
	expectNothing(t, alice)

	// Another incarnation for the same user is replaced too.
	second := cm.channel("box")
	cm.SetupCredentials(ctx, newResumeTestAgent(), "box", nil, nil, BusIdentityOf("", "bob", busT1))
	if got := cm.channel("box"); got == second {
		t.Fatal("another incarnation of bob's shed reused the channel")
	}
}

// TestBusIdentityOf: a record naming both a service client and a user stays
// off the bus (owned) and is unresolved; a service client's shed is owned; a
// user's shed carries its user.
func TestBusIdentityOf(t *testing.T) {
	tests := []struct {
		owner, user string
		want        BusIdentity
	}{
		{"", "", BusIdentity{CreatedAt: busT1}},
		{"", "alice", BusIdentity{User: "alice", CreatedAt: busT1}},
		{"nocturn", "", BusIdentity{Owned: true, CreatedAt: busT1}},
		{"nocturn", "alice", BusIdentity{Owned: true, User: config.UserUnresolved, CreatedAt: busT1}},
	}
	for _, tc := range tests {
		t.Run(tc.owner+"/"+tc.user, func(t *testing.T) {
			if got := BusIdentityOf(tc.owner, tc.user, busT1); !got.same(tc.want) {
				t.Fatalf("BusIdentityOf(%q, %q) = %+v, want %+v", tc.owner, tc.user, got, tc.want)
			}
		})
	}
}

// TestCredentialManagerSetShedUser: the assign command's hook moves a
// running shed's route to its new user without a new channel, for the named
// incarnation only, and never for a service client's shed.
func TestCredentialManagerSetShedUser(t *testing.T) {
	cm, _, alice, bob := newUsersBusManager(t)
	cm.SetupCredentials(context.Background(), newResumeTestAgent(), "box", nil, nil, BusIdentityOf("", "", busT1))
	first := cm.channel("box")

	guestRequest(t, cm.handlerFor("box"), "box")
	expectNothing(t, alice) // no user yet: routed nowhere
	expectNothing(t, bob)

	if cm.SetShedUser("box", busT2, "alice") {
		t.Fatal("SetShedUser acted on another incarnation")
	}
	if !cm.SetShedUser("box", busT1, "alice") {
		t.Fatal("SetShedUser refused the running incarnation")
	}
	if got := cm.channel("box"); got != first {
		t.Fatal("SetShedUser replaced the channel")
	}
	guestRequest(t, cm.handlerFor("box"), "box")
	expectRequest(t, alice, "box")
	expectNothing(t, bob)

	// A start racing in with the new record keeps the channel.
	cm.SetupCredentials(context.Background(), newResumeTestAgent(), "box", nil, nil, BusIdentityOf("", "alice", busT1))
	if got := cm.channel("box"); got != first {
		t.Fatal("a start with the assigned user replaced the channel")
	}

	cm.SetupCredentials(context.Background(), newResumeTestAgent(), "svc", nil, nil, BusIdentityOf("nocturn", "", busT1))
	if cm.SetShedUser("svc", busT1, "alice") {
		t.Fatal("SetShedUser moved a service client's shed onto the bus")
	}
}

// handlerFor returns the message handler of name's channel (test only).
func (cm *CredentialManager) handlerFor(name string) *MessageHandler {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.messageChannels[name].handler
}

// transportDialer hands the message channel one end of a fresh net.Pipe per Dial
// and the test the other, as the guest's side of each transport.
type transportDialer struct{ guests chan net.Conn }

func newTransportDialer() *transportDialer { return &transportDialer{guests: make(chan net.Conn, 4)} }

func (d *transportDialer) Dial(ctx context.Context, _ uint32) (net.Conn, error) {
	host, guest := net.Pipe()
	select {
	case d.guests <- guest:
		return host, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// connect waits for the channel's next transport, reads its health handshake
// as the guest would, and returns the guest's end.
func (d *transportDialer) connect(t *testing.T) net.Conn {
	t.Helper()
	select {
	case g := <-d.guests:
		t.Cleanup(func() { _ = g.Close() })
		_ = g.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, _, err := agentproto.ReadMessage(g); err != nil {
			t.Fatalf("reading the handshake: %v", err)
		}
		_ = g.SetReadDeadline(time.Time{})
		return g
	case <-time.After(10 * time.Second):
		t.Fatal("the message channel never connected")
		return nil
	}
}

// guestSends writes a plugin request from the guest on g and returns its ID.
func guestSends(t *testing.T, g net.Conn) string {
	t.Helper()
	env := plugin.NewEnvelope("ssh-agent", plugin.MessageTypeRequest, nil)
	data, _ := json.Marshal(env)
	if err := agentproto.WriteMessage(g, agentproto.MsgTypePluginMessage, data); err != nil {
		t.Fatal(err)
	}
	return env.ID
}

// TestResponseGoesOnlyToTheTransportItsRequestCameFrom: the message channel
// registers a new connection generation every time its transport connects.
// A request made on transport 1, answered after the transport reconnected,
// is not written into transport 2 (it is gone with its connection); a
// request made on transport 2 is answered there, with the ID its guest
// chose, and that answer is the first frame transport 2 is written.
func TestResponseGoesOnlyToTheTransportItsRequestCameFrom(t *testing.T) {
	reg := plugin.NewRegistry()
	bridge := plugin.NewBridge(reg, true)
	cm := NewCredentialManager(nil, bridge, "test", NewHealthTracker())
	t.Cleanup(cm.Close)
	alice, err := reg.Register(plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"})
	if err != nil {
		t.Fatal(err)
	}
	d := newTransportDialer()
	cm.SetupCredentials(context.Background(), NewAgentClient(d, 1024, 1026), "box", nil, nil, BusIdentityOf("", "alice", busT1))

	g1 := d.connect(t)
	guestSends(t, g1)
	req1 := expectRequest(t, alice, "box")

	_ = g1.Close() // the transport drops; the channel reconnects
	g2 := d.connect(t)

	if err := bridge.Respond("alice", answerTo(req1)); !errors.Is(err, plugin.ErrUnmatched) {
		t.Fatalf("answering transport 1's request after a reconnect = %v, want ErrUnmatched", err)
	}

	guestID2 := guestSends(t, g2)
	req2 := expectRequest(t, alice, "box")
	got := make(chan []byte, 1)
	go func() {
		_, data, _ := agentproto.ReadMessage(g2)
		got <- data
	}()
	if err := bridge.Respond("alice", answerTo(req2)); err != nil {
		t.Fatalf("answering transport 2's request: %v", err)
	}
	select {
	case data := <-got:
		// The first frame after the handshake: had transport 1's answer been
		// written here, it would have come first.
		var env plugin.Envelope
		if err := json.Unmarshal(data, &env); err != nil || env.InReplyTo != guestID2 {
			t.Fatalf("transport 2's first frame is %s (%v), want the answer to its own request %s", data, err, guestID2)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("transport 2 never received its answer")
	}
}

// TestStoppingAChannelEndsItsPublishes: the context a channel publishes
// under is the channel's own, ended by StopListener before it waits for the
// channel: a guest message whose publish is in progress (here held until
// its context ends, as a publish waiting for room in a full listener queue
// is; the registry's side of that is its own test) is released by the stop,
// which does not hang on it.
func TestStoppingAChannelEndsItsPublishes(t *testing.T) {
	cm, _ := newResumeTestManager(t)
	publishing := make(chan struct{})
	ended := make(chan error, 1)
	setVar(t, &publishToHost, func(ctx context.Context, _ *plugin.Bridge, _ string, _ uint64, _ *plugin.Envelope) error {
		close(publishing)
		<-ctx.Done() // only the channel's stop ends this publish
		ended <- ctx.Err()
		return ctx.Err()
	})
	d := newTransportDialer()
	cm.SetupCredentials(context.Background(), NewAgentClient(d, 1024, 1026), "box", nil, nil, BusIdentity{CreatedAt: busT1})
	g := d.connect(t)
	guestSends(t, g)
	<-publishing // the channel's read loop is inside the publish

	within(t, "StopListener during a publish in progress", func() { cm.StopListener("box") })
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the publish in progress ended with %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the publish in progress never ended")
	}
}

// TestCredentialManagerSetShedUserDiscardsPending: assigning a running shed
// to another user discards its requests pending for the previous user.
func TestCredentialManagerSetShedUserDiscardsPending(t *testing.T) {
	reg := plugin.NewRegistry()
	bridge := plugin.NewBridge(reg, true)
	cm := NewCredentialManager(nil, bridge, "test", NewHealthTracker())
	t.Cleanup(cm.Close)
	alice, err := reg.Register(plugin.ListenerKey{User: "alice", Namespace: "ssh-agent"})
	if err != nil {
		t.Fatal(err)
	}
	cm.SetupCredentials(context.Background(), newResumeTestAgent(), "box", nil, nil, BusIdentityOf("", "alice", busT1))
	guestRequest(t, cm.handlerFor("box"), "box")
	req := expectRequest(t, alice, "box")
	if !cm.SetShedUser("box", busT1, "bob") {
		t.Fatal("SetShedUser refused")
	}
	if err := bridge.Respond("alice", answerTo(req)); !errors.Is(err, plugin.ErrUnmatched) {
		t.Fatalf("alice answering a request of the shed now bob's = %v, want ErrUnmatched", err)
	}
}
