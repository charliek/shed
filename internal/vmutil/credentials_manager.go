package vmutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
)

// DirMountFunc is the backend-specific directory mount callback.
// VZ implements this with mountVirtioFSShare, Firecracker with mount9PInGuest.
// Return error is treated as non-fatal for credential mounts (logged as warning),
// but callers may treat workspace mount errors as fatal.
type DirMountFunc func(ctx context.Context, agent *AgentClient, name string, mount config.MountConfig) error

// CredentialManager handles the credential lifecycle shared by VM backends.
// It owns per-VM message channels (for plugin messages and health tracking)
// and the plugin bridge registration.
type CredentialManager struct {
	serverCfg     *config.ServerConfig
	bridge        *plugin.Bridge // plugin message bridge (nil if plugins disabled)
	backendName   string         // "vz" or "firecracker"
	healthTracker *HealthTracker // tracks per-VM heartbeat state

	// mu guards messageChannels. It is held only for bounded steps: nothing
	// the bridge does for it (registering, retiring, setting a user) waits
	// for a publish or a response in progress, so one shed's traffic cannot
	// hold up another shed's start, stop or replacement.
	mu              sync.Mutex
	messageChannels map[string]messageChannel // name -> per-VM message channel
	nextChannel     atomic.Uint64             // channel ids, from 1
}

// messageChannel is a shed's message channel and its place on the bridge.
type messageChannel struct {
	id      uint64 // unique for the manager's life; the channel's claim on its health state
	conn    *NotifyConn
	handler *MessageHandler
	link    *busLink
	cancel  context.CancelFunc // ends the channel's context: a publish in progress returns at once
}

// busLink is a message channel's bridge registration: the bus identity the
// channel was started for and the connection generation its current
// transport is registered as. The channel registers once when it starts and
// again, with a new generation, every time its transport connects, so a
// response is only ever written to the transport its request arrived on.
//
// mu orders a registration against stop and against a change of the user,
// so a stopped channel never registers again, a re-registration always
// carries the current user, and changes of the user reach the link and the
// bridge in the same order. It is held only for steps that do not wait.
type busLink struct {
	mu      sync.Mutex
	id      BusIdentity
	stopped bool
	gen     atomic.Uint64 // written under mu
}

// identity returns the link's bus identity.
func (l *busLink) identity() BusIdentity {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.id
}

// errChannelStopped abandons a transport that connects after its channel
// was stopped.
var errChannelStopped = errors.New("message channel stopped")

// retire ends ch's place on the bridge: its context is cancelled (a publish
// in progress returns), it may not register again, and its registration is
// removed if it is still the one under name. Nothing in it waits (the
// bridge retires a registration without waiting for work in progress on
// it), so it may run under cm.mu. It does not wait for the channel's
// goroutine; the caller calls ch.conn.Stop(), outside cm.mu.
func (cm *CredentialManager) retire(name string, ch messageChannel) {
	ch.cancel()
	ch.link.mu.Lock()
	ch.link.stopped = true
	gen := ch.link.gen.Load()
	ch.link.mu.Unlock()
	if cm.bridge != nil {
		cm.bridge.UnregisterConn(name, gen)
	}
}

// BusIdentity is who a shed is on the credential bus, from the server's own
// record of the shed and never from anything the guest sends.
type BusIdentity struct {
	// Owned is true for a shed a service client owns (any stored owner,
	// including a record that also names a user): its message channel still
	// carries health heartbeats, but the credential bus is off for it in
	// both directions (see plugin.ShedConn.Owned).
	Owned bool
	// User is the shed's user as its record is reported
	// (config.ShedOwnership; config.UserUnresolved for a record that does
	// not agree about whose it is).
	User string
	// CreatedAt is the shed incarnation's creation time.
	CreatedAt time.Time
}

// BusIdentityOf is the bus identity of a Firecracker or VZ shed whose record
// stores owner and user, created at createdAt.
func BusIdentityOf(owner, user string, createdAt time.Time) BusIdentity {
	_, reported := config.ShedOwnership(owner, user)
	return BusIdentity{Owned: owner != "", User: reported, CreatedAt: createdAt}
}

// same reports whether a channel started for id can serve other: the same
// incarnation, owned flag and user.
func (id BusIdentity) same(other BusIdentity) bool {
	return id.Owned == other.Owned && id.User == other.User && id.CreatedAt.Equal(other.CreatedAt)
}

// NewCredentialManager creates a new CredentialManager.
func NewCredentialManager(serverCfg *config.ServerConfig, bridge *plugin.Bridge, backendName string, healthTracker *HealthTracker) *CredentialManager {
	return &CredentialManager{
		serverCfg:       serverCfg,
		bridge:          bridge,
		backendName:     backendName,
		healthTracker:   healthTracker,
		messageChannels: make(map[string]messageChannel),
	}
}

// SetupCredentials mounts directory credentials via the provided callback
// and starts the message channel for plugin communication and health tracking.
// id is the shed's bus identity, from the server's own record of it.
//
// Directory mount failures are logged as warnings (non-fatal).
func (cm *CredentialManager) SetupCredentials(ctx context.Context, agent *AgentClient, shedName string, dirCreds map[string]config.MountConfig, mountDir DirMountFunc, id BusIdentity) {
	// Mount directory credentials via backend-specific callback
	if len(dirCreds) > 0 && mountDir != nil {
		backend.Phase(ctx, "credentials")
		backend.Status(ctx, "Mounting directory credentials...")
		for name, mount := range dirCreds {
			if err := mountDir(ctx, agent, name, mount); err != nil {
				log.Printf("Warning: directory credential mount failed for %q: %v", name, err)
				backend.Phase(ctx, "credentials")
				backend.StatusWarning(ctx, fmt.Sprintf("Failed to mount credential %q", name))
			}
		}
	}

	// Start the message channel for plugin messages and health tracking.
	cm.startMessageChannel(shedName, agent, id)
}

// HealthTracker returns the health tracker for querying VM health state.
func (cm *CredentialManager) HealthTracker() *HealthTracker {
	return cm.healthTracker
}

// StopListener stops the message channel for a VM and unregisters it
// from the plugin bridge.
//
// The bridge registration is removed under cm.mu, and only if it is still
// this channel's (by generation), so a StopListener racing a new channel's
// start for the same name can never unregister the new one. The wait for
// the channel's goroutine happens after cm.mu is released.
func (cm *CredentialManager) StopListener(name string) {
	cm.mu.Lock()
	ch, ok := cm.messageChannels[name]
	delete(cm.messageChannels, name)
	if ok {
		cm.retire(name, ch)
	} else if cm.bridge != nil {
		cm.bridge.UnregisterShed(name)
	}
	if cm.healthTracker != nil {
		if ok {
			cm.healthTracker.RemoveFrom(name, ch.id)
		} else {
			cm.healthTracker.Remove(name)
		}
	}
	cm.mu.Unlock()

	if ok {
		ch.conn.Stop()
	}
}

// Close stops all message channels.
func (cm *CredentialManager) Close() {
	cm.mu.Lock()
	stopped := make([]messageChannel, 0, len(cm.messageChannels))
	for name, ch := range cm.messageChannels {
		cm.retire(name, ch)
		stopped = append(stopped, ch)
		if cm.healthTracker != nil {
			cm.healthTracker.RemoveFrom(name, ch.id)
		}
		delete(cm.messageChannels, name)
	}
	cm.mu.Unlock()
	for _, ch := range stopped {
		ch.conn.Stop()
	}
}

// ResumeMessageChannel re-opens the message channel for a VM that is already
// running — the shed-server-restart path (#315). The host dials the guest, so
// nothing re-establishes the channel on its own after a restart: the plugin
// bridge would hold no registration for the shed and `shed list -vv` would
// show no extension health until the shed was manually stopped and started.
//
// It is deliberately message-channel-only: no mounts, no provisioning, no
// hooks. Those all ran when the shed was created/started and the guest still
// has them.
func (cm *CredentialManager) ResumeMessageChannel(name string, agent *AgentClient, id BusIdentity) {
	if cm.startMessageChannel(name, agent, id) {
		log.Printf("[%s] resumed message channel after server restart", name)
	}
}

// startMessageChannel starts the generalized message channel for a VM.
//
// It returns true when it started a channel, false when one was already
// present for this name. The dedupe lives here so the two entry points
// (SetupCredentials on create/start, ResumeMessageChannel on server startup)
// can't race a second NotifyConn onto the same guest — e.g. a StartShed that
// lands just after the startup resume walk has already resumed the shed.
//
// A channel left behind for the same name with another bus identity (a shed
// deleted and recreated under its name, when a teardown path missed
// StopListener: another incarnation, owned flag or user) is stopped and
// replaced, never reused: its handler and bridge registration would carry
// the old shed's credential-bus access.
//
// The channel is registered on the bridge when it starts and again, as a new
// connection generation, each time its transport connects; the publish
// callback publishes as the generation of the transport the message arrived
// on, and each registration's Send writes only to its own transport. A
// channel that outlives its registration can never publish as whatever is
// registered under the name later, and a response is never written to a
// transport other than the one its request came from.
func (cm *CredentialManager) startMessageChannel(name string, agent *AgentClient, id BusIdentity) bool {
	chID := cm.nextChannel.Add(1)
	// What the guest sends that is refused is logged at a bounded rate.
	rejects := &rejectLog{}

	// Health heartbeat callback: update the tracker with agent boot time,
	// only while this channel holds the claim on the shed's health state (a
	// channel replaced by another, still running, is ignored).
	var healthFn func(env *plugin.Envelope)
	if cm.healthTracker != nil {
		healthFn = func(env *plugin.Envelope) {
			var payload plugin.HeartbeatPayload
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				// The error can quote the guest's started_at whole.
				rejects.malformed.logf("[%s] Invalid heartbeat payload: %s", name, clipGuestText(err.Error()))
				return
			}
			if payload.StartedAt.IsZero() {
				rejects.malformed.logf("[%s] Ignoring heartbeat with zero started_at", name)
				return
			}
			cm.healthTracker.UpdateFrom(name, chID, payload.StartedAt, payload.Extensions)
		}
	}

	// Extract enabled extensions from server config (nil-safe).
	var enabledExtensions []string
	if cm.serverCfg != nil && cm.serverCfg.Extensions != nil {
		enabledExtensions = cm.serverCfg.Extensions.Enabled
	}

	ctx, cancel := context.WithCancel(context.Background())
	link := &busLink{id: id}
	handler := NewMessageHandler(healthFn, func(env *plugin.Envelope) {
		// An owned shed's plugin messages are dropped here as well as in the
		// bridge: the credential bus is off for it, whatever the bridge's
		// registration state.
		if id.Owned {
			return
		}
		if cm.bridge != nil {
			// The read loop calls this, after OnConnect registered the
			// transport the message arrived on, so the link's generation is
			// that transport's.
			if err := publishToHost(ctx, cm.bridge, name, link.gen.Load(), env); err != nil {
				rejects.refused(name, err)
			}
		}
	}, enabledExtensions)
	handler.rejectf = func(format string, args ...any) {
		rejects.malformed.logf("[%s] %s", name, clipGuestText(fmt.Sprintf(format, args...)))
	}
	handler.onConnect = func(conn net.Conn) error {
		return cm.registerTransport(name, link, func(env *plugin.Envelope, deadline time.Time, admit func() error, wrote func()) error {
			return handler.sendOn(conn, env, deadline, admit, wrote)
		})
	}

	// Reserve, register and start under ONE hold of the mutex.
	//
	// Two holds would let two callers both see an empty slot and open two
	// connections to the same guest. Publishing the conn and then starting it
	// outside the lock is worse still: StopListener/Close could take the slot
	// in between and call Stop() on a conn whose Start() has not run yet.
	// NotifyConn.Stop reads nc.cancel (still nil) and then waits on nc.wg, so
	// the interleaving Stop-reads-nil → Start-adds-to-wg → Stop-waits hangs
	// shutdown forever on a connection nobody can cancel; the reverse order
	// leaves a started conn that is in nobody's map and reconnects for the
	// life of the process. #315's startup resume walk runs concurrently with
	// normal serving and with shutdown, which is what made that pre-existing
	// window reachable in practice.
	//
	// The calls made inside the lock do not wait (a registration, and the
	// retirement of the one it replaces, never wait for work in progress on
	// the bridge; a goroutine spawn), and the cm.mu → bridge lock order is
	// the one Close() already uses. A replaced channel's goroutine is waited
	// for after the lock is released.
	cm.mu.Lock()
	var replaced *messageChannel
	if old, exists := cm.messageChannels[name]; exists {
		oldID := old.link.identity()
		if oldID.same(id) {
			cm.mu.Unlock()
			cancel()
			return false
		}
		log.Printf("[%s] replacing a stale message channel (owned=%v user=%q created %s) for owned=%v user=%q created %s",
			name, oldID.Owned, oldID.User, oldID.CreatedAt.Format(time.RFC3339Nano), id.Owned, id.User, id.CreatedAt.Format(time.RFC3339Nano))
		cm.retire(name, old)
		delete(cm.messageChannels, name)
		replaced = &old
	}
	conn := NewNotifyConn(agent.Dialer(), agent.NotifyPort(), name)

	// Registered before any transport connects, so the shed is on the bridge
	// (its user known to the respond check and the egress filter) from the
	// start; nothing can be written to it until a transport connects.
	_ = cm.registerTransport(name, link, func(*plugin.Envelope, time.Time, func() error, func()) error { return errNoTransport })
	cm.messageChannels[name] = messageChannel{id: chID, conn: conn, handler: handler, link: link, cancel: cancel}
	if cm.healthTracker != nil {
		cm.healthTracker.Claim(name, chID)
	}

	conn.Start(ctx, handler)
	cm.mu.Unlock()

	if replaced != nil {
		replaced.conn.Stop()
	}
	return true
}

// errNoTransport is the Send of a channel's registration before its first
// transport connects.
var errNoTransport = errors.New("message handler: no active connection")

// registerTransport registers link's channel on the bridge as a new
// connection generation whose responses go through send, unless the channel
// was stopped. It runs when the channel starts and from OnConnect for every
// transport. The registration does not wait (see plugin.Bridge.RegisterShed).
func (cm *CredentialManager) registerTransport(name string, link *busLink, send plugin.SendFunc) error {
	if cm.bridge == nil {
		return nil
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	if link.stopped {
		return errChannelStopped
	}
	serverName := ""
	if cm.serverCfg != nil {
		serverName = cm.serverCfg.Name
	}
	link.gen.Store(cm.bridge.RegisterShed(name, &plugin.ShedConn{
		Name:       name,
		Backend:    cm.backendName,
		Server:     serverName,
		SendWithin: send,
		Owned:      link.id.Owned,
		User:       link.id.User,
		CreatedAt:  link.id.CreatedAt,
	}))
	return nil
}

// SetShedUser changes the user the running shed name's message channel, and
// its bridge registration, route as, when the channel is for the incarnation
// created at createdAt and is not a service client's; it reports whether it
// did. The admin assign command calls it after it changes the shed's record,
// and the remote backend whenever a shed's reported user changes, so a
// running shed's requests follow its new user without a restart. user is
// the shed's reported user. The bridge drops what the shed published on its
// previous route, and nothing of that is queued, sent or answered from that
// instant (see plugin.Bridge.SetShedUser). Nothing here waits for a publish
// or a response in progress.
func (cm *CredentialManager) SetShedUser(name string, createdAt time.Time, user string) bool {
	cm.mu.Lock()
	ch, ok := cm.messageChannels[name]
	cm.mu.Unlock()
	if !ok {
		return false
	}
	link := ch.link
	link.mu.Lock()
	defer link.mu.Unlock()
	if link.stopped || link.id.Owned || !link.id.CreatedAt.Equal(createdAt) {
		return false
	}
	// From here every new registration of the channel carries the new user;
	// the current one is changed under the same hold, so the two cannot be
	// applied out of order.
	link.id.User = user
	if cm.bridge != nil {
		cm.bridge.SetShedUser(name, createdAt, user)
	}
	return true
}

// SuspendShedRoute holds the bus route of the shed incarnation (name,
// createdAt) at once, without waiting for anything, and returns the hold's
// epoch for ResumeShedRoute (see plugin.Bridge.SuspendRoute). The remote
// backend calls it, with its own lock held, the moment a shed's reported
// user changes, and applies the new user later with SetShedUser.
func (cm *CredentialManager) SuspendShedRoute(name string, createdAt time.Time) uint64 {
	if cm.bridge == nil {
		return 0
	}
	return cm.bridge.SuspendRoute(name, createdAt)
}

// ResumeShedRoute lifts the route hold made with epoch, unless a later one
// replaced it (see plugin.Bridge.ResumeRoute). It does not wait.
func (cm *CredentialManager) ResumeShedRoute(name string, epoch uint64) bool {
	if cm.bridge == nil {
		return false
	}
	return cm.bridge.ResumeRoute(name, epoch)
}

// publishToHost publishes a guest's message on the bridge. A var so a test
// can stand in for the bridge's publish.
var publishToHost = func(ctx context.Context, b *plugin.Bridge, name string, gen uint64, env *plugin.Envelope) error {
	return b.PublishToHost(ctx, name, gen, env)
}

// rejectLogEvery is the least time between two log lines of one kind about
// what one channel's guest sent that was refused. A var so tests can change
// it.
var rejectLogEvery = 10 * time.Second

// rejectLogf writes a rate-limited line (a var so tests can count them).
var rejectLogf = log.Printf

// rejectLog is one channel's log lines about what its guest sent that was
// refused. Each kind has a rate limit of its own, so a guest cannot hide one
// kind behind another (a stream of unparseable frames first in every
// interval would otherwise leave a limit or a routing refusal unlogged):
// malformed traffic (an envelope or a heartbeat that does not parse, an
// unknown frame type), a message past a limit, and a message that could not
// be delivered (no listener, a listener not reading, a route gone).
type rejectLog struct {
	malformed, limit, undelivered logLimiter
}

// refused logs a publish the bus refused, under its kind. The error's text
// is clipped: it can quote what the guest chose (a namespace).
func (rl *rejectLog) refused(name string, err error) {
	ll := &rl.undelivered
	if errors.Is(err, plugin.ErrPendingLimit) || errors.Is(err, plugin.ErrIdentifierTooLong) {
		ll = &rl.limit
	}
	ll.logf("[%s] Failed to publish plugin message: %s", name, clipGuestText(err.Error()))
}

// logLimiter rate-limits one kind of a channel's log lines about what its
// guest sent: at most one every rejectLogEvery; the next line written says
// how many were left out. A guest can send refused messages as fast as it
// likes, so they must not each cost a log line.
type logLimiter struct {
	mu      sync.Mutex
	last    time.Time
	skipped int
}

func (ll *logLimiter) logf(format string, args ...any) {
	ll.mu.Lock()
	now := time.Now()
	if !ll.last.IsZero() && now.Sub(ll.last) < rejectLogEvery {
		ll.skipped++
		ll.mu.Unlock()
		return
	}
	skipped := ll.skipped
	ll.skipped = 0
	ll.last = now
	ll.mu.Unlock()
	if skipped > 0 {
		format += fmt.Sprintf(" (%d similar line(s) left out)", skipped)
	}
	rejectLogf(format, args...)
}
