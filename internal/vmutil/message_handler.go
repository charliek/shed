package vmutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/plugin"
)

// MessageHandler implements NotifyHandler and handles all messages on the
// generalized message channel (port 1026). It routes system namespace messages
// (system:health) and external plugin messages.
//
// For bidirectional writes, the handler captures the connection from OnConnect
// and serializes writes with a write lock that a response can wait on with a
// deadline. NotifyConn itself is not modified.
type MessageHandler struct {
	// Callback for system:health heartbeat events from the VM.
	// nil if health tracking is not configured.
	healthFn func(env *plugin.Envelope)

	// Callback for incoming plugin messages from the VM.
	pluginFn func(env *plugin.Envelope)

	// Extension namespaces the server wants the agent to enable.
	enabledExtensions []string

	// onConnect, when set, runs in OnConnect for each new transport, after
	// the handler has taken it and before the handshake; an error abandons
	// the transport. The credential manager registers the transport with
	// the bridge here, as a new connection generation.
	onConnect func(conn net.Conn) error

	// rejectf, when set, logs what the guest sent that is refused (an
	// envelope that does not parse, an unknown message type) at a bounded
	// rate; OnMessage then returns nil for it. The credential manager sets
	// it. When nil, such a message is logged by the read loop, as before.
	rejectf func(format string, args ...any)

	// Connection captured in OnConnect, used for writes; read and written
	// only while holding the write lock.
	conn net.Conn
	// writeLock is the write lock: a one-slot semaphore, so a response can
	// stop waiting for it at its deadline.
	writeLock chan struct{}
}

// guestWriteTimeout bounds one write to the guest. A guest that does not take
// a frame within it has its transport closed (the frame may be half written,
// so the stream cannot go on), and the message channel reconnects. A var so
// tests can shrink it.
var guestWriteTimeout = 5 * time.Second

// errTransportGone is returned for a write to a transport the handler no
// longer holds: a response is written only to the transport its request
// arrived on.
var errTransportGone = errors.New("message handler: the transport this message was for is gone")

// errWriteDeadline is returned when a response's deadline passes before its
// write could begin: nothing was written. It is a
// plugin.ErrResponseTimeout.
var errWriteDeadline = fmt.Errorf("message handler: nothing was written: %w", plugin.ErrResponseTimeout)

// maxLoggedGuestText is the most bytes of anything a guest chose (a field of
// what it sent, or an error that quotes one) that a log line or an error
// carries.
const maxLoggedGuestText = 160

// clipGuestText shortens text a guest influenced for a log line or an error.
func clipGuestText(s string) string {
	if len(s) <= maxLoggedGuestText {
		return s
	}
	return s[:maxLoggedGuestText] + fmt.Sprintf("... (%d bytes)", len(s))
}

// lock takes the write lock, waiting as long as it takes (each holder is
// bounded).
func (h *MessageHandler) lock() { h.writeLock <- struct{}{} }

// lockBy takes the write lock unless deadline passes first.
func (h *MessageHandler) lockBy(deadline time.Time) bool {
	select {
	case h.writeLock <- struct{}{}:
		return true
	default:
	}
	if lockWaitHook != nil {
		lockWaitHook()
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case h.writeLock <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

// lockWaitHook, when set (tests only), runs when lockBy finds the write lock
// taken, before it waits for it.
var lockWaitHook func()

// unlock releases the write lock.
func (h *MessageHandler) unlock() { <-h.writeLock }

// NewMessageHandler creates a handler for the generalized message channel.
//   - healthFn: called for system:health heartbeat events (nil if no health tracking)
//   - pluginFn: called for incoming plugin messages from the VM
//   - enabledExtensions: extension namespaces to include in the health handshake (nil if none)
func NewMessageHandler(healthFn func(env *plugin.Envelope), pluginFn func(env *plugin.Envelope), enabledExtensions []string) *MessageHandler {
	return &MessageHandler{
		healthFn:          healthFn,
		pluginFn:          pluginFn,
		enabledExtensions: enabledExtensions,
		writeLock:         make(chan struct{}, 1),
	}
}

// OnConnect implements NotifyHandler. It stores the connection and sends
// an initial system:health request (as a handshake to trigger agent-side
// connection promotion and heartbeats). The handshake includes the list of
// enabled extensions for the agent to activate.
func (h *MessageHandler) OnConnect(conn net.Conn) error {
	h.lock()
	h.conn = conn
	h.unlock()

	if h.onConnect != nil {
		if err := h.onConnect(conn); err != nil {
			return err
		}
	}

	// Build health request payload with enabled extensions.
	var payload json.RawMessage
	if len(h.enabledExtensions) > 0 {
		reqPayload := plugin.HealthRequestPayload{
			Extensions: h.enabledExtensions,
		}
		var err error
		payload, err = json.Marshal(reqPayload)
		if err != nil {
			return fmt.Errorf("marshal health request payload: %w", err)
		}
	}

	healthEnv := plugin.NewEnvelope(plugin.NamespaceHealth, plugin.MessageTypeRequest, payload)
	if err := h.sendEnvelope(healthEnv); err != nil {
		return fmt.Errorf("send health handshake: %w", err)
	}

	return nil
}

// OnMessage implements NotifyHandler. It routes messages by type.
func (h *MessageHandler) OnMessage(msgType byte, data []byte) error {
	switch msgType {
	case agentproto.MsgTypePluginMessage:
		var env plugin.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			// A decode error can quote the field it failed on (a timestamp
			// that does not parse is quoted whole), so its text is clipped
			// like anything else the guest chose.
			reason := clipGuestText(err.Error())
			if h.rejectf != nil {
				h.rejectf("invalid plugin envelope: %s", reason)
				return nil
			}
			return fmt.Errorf("invalid plugin envelope: %s", reason)
		}

		// Consume all system:health messages (events, responses) — never forward to pluginFn.
		if env.Namespace == plugin.NamespaceHealth {
			if env.Type == plugin.MessageTypeEvent && h.healthFn != nil {
				h.healthFn(&env)
			}
			return nil
		}

		// Route all other plugin messages to the callback
		if h.pluginFn != nil {
			h.pluginFn(&env)
		}
		return nil

	default:
		if h.rejectf != nil {
			h.rejectf("unexpected message type 0x%02x", msgType)
			return nil
		}
		log.Printf("MessageHandler: unexpected message type 0x%02x", msgType)
		return nil
	}
}

// SendPluginMessage writes a plugin envelope to the agent over the vsock
// message connection. Safe for concurrent use.
func (h *MessageHandler) SendPluginMessage(env *plugin.Envelope) error {
	return h.sendEnvelope(env)
}

// sendOn writes a plugin envelope to the guest over conn, only while conn is
// still the handler's transport, and gives up at deadline: one deadline
// covers the wait for the write lock and the write. It is the SendWithin of
// the bridge registration made for conn (a plugin.SendFunc), so a response is
// never written to a later transport than the one its request arrived on.
//
// admit runs with the write lock held, after any wait for it and immediately
// before the write: an error from it ends the delivery with nothing written.
// wrote runs, the lock still held, once the whole frame has been written
// (writeBounded returned nil), so a final answer is consumed before anything
// else can be written. When the lock is not had by the deadline nothing is
// written (errWriteDeadline, safe to retry); a write that fails closes the
// transport and is not safe to retry (see writeBounded).
func (h *MessageHandler) sendOn(conn net.Conn, env *plugin.Envelope, deadline time.Time, admit func() error, wrote func()) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("failed to marshal plugin envelope: %w", err)
	}

	if !h.lockBy(deadline) {
		return errWriteDeadline
	}
	defer h.unlock()

	if h.conn != conn {
		return errTransportGone
	}
	if admit != nil {
		if err := admit(); err != nil {
			return err
		}
	}
	if err := writeBounded(conn, data, time.Until(deadline)); err != nil {
		return err
	}
	if wrote != nil {
		wrote()
	}
	return nil
}

// sendEnvelope marshals and writes an envelope to the vsock connection.
func (h *MessageHandler) sendEnvelope(env *plugin.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("failed to marshal plugin envelope: %w", err)
	}

	h.lock()
	defer h.unlock()

	if h.conn == nil {
		return fmt.Errorf("message handler: no active connection")
	}

	return writeBounded(h.conn, data, guestWriteTimeout)
}

// writeBounded writes one frame to conn, closing conn if the write takes
// longer than limit. A limit already spent, or a frame larger than the
// framing allows, writes nothing and leaves conn as it was (errWriteDeadline
// for the first: it is safe to retry). Otherwise the outcome is the write's:
//   - a write that returned success is a written frame, and nil is returned,
//     even when the timer fired just as it finished: the timer's close then
//     only ends a transport that has nothing half written on it, and any
//     later write sees it closed;
//   - a write that failed, whether the timer closed conn under it or it
//     failed on its own, may have left a frame half written, so conn is
//     closed (the stream cannot go on, and nothing more is written to it)
//     and the error is not a plugin.ErrResponseTimeout: something may have
//     been written, so it is not safe to retry.
func writeBounded(conn net.Conn, data []byte, limit time.Duration) error {
	if limit <= 0 {
		return errWriteDeadline
	}
	if len(data) > agentproto.MaxMessageSize {
		return fmt.Errorf("message handler: a frame of %d bytes is larger than the most a guest takes (%d); nothing was written", len(data), agentproto.MaxMessageSize)
	}
	timer := time.AfterFunc(limit, func() { _ = conn.Close() })
	err := agentproto.WriteMessage(conn, agentproto.MsgTypePluginMessage, data)
	if wroteFrameHook != nil {
		wroteFrameHook(err)
	}
	stopped := timer.Stop()
	if err == nil {
		return nil
	}
	_ = conn.Close()
	if !stopped {
		return fmt.Errorf("message handler: the guest did not take a write within %s; transport closed", limit.Round(time.Millisecond))
	}
	return fmt.Errorf("message handler: writing to the guest: %w; transport closed", err)
}

// wroteFrameHook, when set (tests only), runs in writeBounded once the frame's
// write has returned (with its error) and before the write's timer is
// stopped.
var wroteFrameHook func(err error)
