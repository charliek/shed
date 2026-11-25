package api

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/charliek/shed/internal/agentproto"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/users"
	"github.com/go-chi/chi/v5"
)

// sseKeepaliveInterval is how often an idle SSE subscription emits a comment
// ping. It keeps an idle NAT mapping / proxy IdleTimeout from evicting a
// long-lived, quiet bus stream (the Phase-1→Phase-5 keepalive). A package var
// so tests can shorten it.
var sseKeepaliveInterval = 25 * time.Second

// sseWriteTimeout bounds every write to a subscription's stream, the
// keepalives included: the response headers, each event and each keepalive
// must be taken by the connection (written and flushed) within it. A write
// that misses it closes the stream and unregisters the listener, so a
// subscriber that stopped reading (a machine gone to sleep, a broker
// wedged) gives its namespace back, and its next subscribe succeeds instead
// of being refused as a duplicate.
//
// It is shorter than the keepalive interval, so a stalled stream is found by
// the next keepalive at the latest: within sseKeepaliveInterval +
// sseWriteTimeout (40 s) of when its connection stopped taking data. A write
// blocks only when the connection's send buffer is full, so what a slow but
// live subscriber sees is this: a frame its link cannot absorb within 15 s
// (a frame larger than the socket buffers, on a link slower than about its
// size per 15 s) ends its stream; its broker reconnects on its usual backoff
// and is sent its machine's pending requests again. Bus frames are small
// (a credential request is a few KB), so in practice only a dead or stopped
// reader reaches it.
//
// A peer that vanished without closing (slept, or lost its network) leaves
// the send buffer room for the small keepalives for a long time, so the
// write deadline alone would not find it. The stream's connection is
// therefore also given a bound on how long written data may stay
// unacknowledged (sseUnackedTimeout, where the platform has one): a
// keepalive the peer never acknowledges fails the connection, which ends the
// stream the same way.
var sseWriteTimeout = 15 * time.Second

// sseUnackedTimeout is how long data written to a subscription's connection
// may stay unacknowledged before the kernel fails the connection (Linux
// TCP_USER_TIMEOUT, macOS TCP_RXT_CONNDROPTIME). A machine that went to
// sleep stops acknowledging; its stream is then reaped within about
// sseKeepaliveInterval + sseUnackedTimeout of its last acknowledged byte.
var sseUnackedTimeout = 40 * time.Second

// maxRespondBody is the most bytes of a /respond body that are read: the
// largest frame a guest can be written (the vsock framing limit).
const maxRespondBody = agentproto.MaxMessageSize

// respondValidatedHook, when set (tests only), runs in handlePluginRespond
// after a response has been matched to its pending request and before it is
// delivered, so a test can replace the shed in between.
var respondValidatedHook func()

// busCaller returns the user a request acts as on the credential bus, and
// whether it sees every user's rows (an admin, or the implicit user of a
// server with no user list). ok is false for a caller with no place on the
// bus: a service client, or a request with no caller.
func busCaller(r *http.Request) (user string, all, ok bool) {
	c := callerFrom(r.Context())
	switch {
	case c.Kind != CallerUser:
		return "", false, false
	case c.Implicit:
		return users.ImplicitName, true, true
	case c.User == "":
		return "", false, false
	}
	return c.User, c.Admin, true
}

// refuseBusCaller answers a caller with no place on the bus.
func refuseBusCaller(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "FORBIDDEN", "the credential bus is for users")
}

// refuseScriptKey answers a request made with a script key's certificate on
// a bus route that subscribes or answers, and reports whether it did. A
// script key is automation: it never holds the user's credential source, so
// it may neither receive the user's sheds' requests nor answer them. Read
// from the live lookup on every request, so a key marked script since its
// certificate was issued is refused at once.
func refuseScriptKey(w http.ResponseWriter, r *http.Request) bool {
	if !callerFrom(r.Context()).Script {
		return false
	}
	writeError(w, http.StatusForbidden, "FORBIDDEN", "a script key cannot subscribe to or answer on the credential bus")
	return true
}

// handleListListeners returns the caller's active plugin listeners, or every
// user's for an admin.
func (s *Server) handleListListeners(w http.ResponseWriter, r *http.Request) {
	user, all, ok := busCaller(r)
	if !ok {
		refuseBusCaller(w)
		return
	}
	if s.refuseServiceTarget(w, r, user) {
		return
	}
	if s.plugins == nil {
		writeJSON(w, http.StatusOK, map[string]any{"listeners": []any{}})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"listeners": s.plugins.List(user, all)})
}

// handlePluginSubscribe establishes an SSE stream for plugin messages.
//
// The caller's machine is admitted for its user (the pin; see
// internal/plugin/pin.go) and registered as that user's listener for the
// namespace, and receives that user's sheds' messages as SSE events until it
// disconnects. In order, after the caller, scope and script-key checks and
// the namespace's own checks: a request with no valid machine headers is 403
// SOURCE_MACHINE_ID_REQUIRED; an admission that would write the binding store
// beyond the user's rate limit is 429 SOURCE_RATE_LIMITED with Retry-After; a
// machine that is not the user's source is 403 SOURCE_NOT_BOUND naming the
// one that is; the source machine already holding the namespace is 409
// SOURCE_DUPLICATE_STREAM (nothing is replaced); a store failure is 500 and
// registers nothing. Another user's subscription to the namespace is
// independent.
func (s *Server) handlePluginSubscribe(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "namespace")

	user, _, ok := busCaller(r)
	if !ok {
		refuseBusCaller(w)
		return
	}
	if s.refuseServiceTarget(w, r, user) {
		return
	}
	if refuseScriptKey(w, r) {
		return
	}
	if s.plugins == nil {
		writeError(w, http.StatusServiceUnavailable, "PLUGINS_DISABLED", "plugin system not initialized")
		return
	}

	if plugin.IsSystemNamespace(namespace) {
		writeError(w, http.StatusForbidden, "NAMESPACE_RESERVED",
			fmt.Sprintf("namespace prefix %q is reserved for internal use", "system:"))
		return
	}
	if err := plugin.ValidateNamespace(namespace); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_NAMESPACE", err.Error())
		return
	}
	// Fail closed: a bus without its source bindings admits nobody.
	if s.plugins.Bindings() == nil {
		writeError(w, http.StatusServiceUnavailable, "PLUGINS_DISABLED", "credential source bindings not initialized")
		return
	}
	machine, ok := machineFromRequest(r)
	if !ok {
		s.missingID.logOnce(user, callerFrom(r.Context()).Name)
		writeMachineIDRequired(w)
		return
	}

	listener, err := s.plugins.Subscribe(r.Context(), plugin.ListenerKey{User: user, Namespace: namespace}, machine)
	if err != nil {
		var notBound *plugin.NotBoundError
		var throttled *plugin.ThrottledError
		switch {
		case errors.As(err, &notBound):
			writeSourceNotBound(w, notBound.Bound)
		case errors.As(err, &throttled):
			writeAdmissionThrottled(w, throttled.RetryAfter)
		case errors.Is(err, plugin.ErrSourceIsService):
			writeError(w, http.StatusConflict, config.ErrSourceIsService, config.SourceIsServiceMessage)
		case errors.Is(err, plugin.ErrDuplicateStream):
			writeDuplicateStream(w, namespace)
		case errors.Is(err, plugin.ErrBindingStore):
			log.Printf("credential bus: admitting machine %s (%q)%s: %v", machine.ID, machine.Name, forBusUser(user), err)
			writeError(w, http.StatusInternalServerError, config.ErrInternalError, "the credential source bindings could not be saved; nothing was registered, retry")
		case errors.Is(err, plugin.ErrBusClosed):
			writeError(w, http.StatusServiceUnavailable, "PLUGINS_DISABLED", "the server is shutting down")
		case errors.Is(err, r.Context().Err()) && r.Context().Err() != nil:
			// The client went away while waiting for its user's gate.
		default:
			writeError(w, http.StatusInternalServerError, config.ErrInternalError, err.Error())
		}
		return
	}

	// Ensure cleanup on disconnect: this listener only, by its token, so a
	// handler that ends late never removes a newer listener for the key.
	defer s.plugins.Unregister(listener.Key(), listener.Token)

	s.streamListener(w, r, listener)
}

// streamListener writes listener's messages to the subscriber as SSE events,
// with a keepalive comment when idle, until the request ends, the listener
// is unregistered (a move of the binding, shutdown) or a write fails. Every
// write has the deadline sseWriteTimeout: a write that misses it (the
// subscriber stopped reading) ends the stream, and the caller's deferred
// Unregister gives the namespace back.
func (s *Server) streamListener(w http.ResponseWriter, r *http.Request, listener *plugin.Listener) {
	rc := http.NewResponseController(w)
	// The deadline is armed only while a write is in progress (see write
	// below). Also cleared when the stream ends, whatever ended it: the
	// server sets no write deadline of its own (no WriteTimeout), so one left
	// here would fail the next request's response on a keep-alive connection.
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	// Data the peer never acknowledges fails the connection (where the
	// platform supports it): a machine that slept is found by its next
	// keepalive, not after the kernel's own, much longer, retransmissions.
	if err := limitUnackedData(r.Context(), sseUnackedTimeout); err != nil {
		log.Printf("credential bus: cannot bound unacknowledged data on the stream of machine %s (%q): %v", listener.MachineID, listener.MachineName, err)
	}
	// write runs fn (one write) and the flush that sends it, both within one
	// deadline, and reports whether both completed. The deadline is cleared
	// once they have, before the stream waits for its next event: it bounds a
	// write, never the idle time between two. That matters under HTTP/2
	// (which the HTTPS listener negotiates), where a stream's write deadline
	// is a timer that resets the stream when it fires, write in progress or
	// not: one left armed after the last write would reset a healthy idle
	// subscription sseWriteTimeout later, before its next keepalive, on
	// every reconnect. A writer with no deadline support (a test recorder) is
	// written without one.
	write := func(fn func() error) bool {
		if err := rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		if err := fn(); err != nil {
			return s.streamWriteFailed(listener, err)
		}
		if err := rc.Flush(); err != nil {
			return s.streamWriteFailed(listener, err)
		}
		if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return false
		}
		return true
	}

	// Set SSE headers (overrides ContentTypeJSON middleware)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if !write(func() error { w.WriteHeader(http.StatusOK); return nil }) {
		return
	}

	ctx := r.Context()
	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	stillTheCaller := s.busCallerCheck(r, listener.User)

	for {
		select {
		case <-ctx.Done():
			return
		case m := <-listener.Messages:
			// The queue carries references. Take is the check at the point
			// of sending: a request answered or dropped since it was queued,
			// whose shed's route changed, or whose machine is no longer the
			// user's source, is not sent.
			env := listener.Take(m)
			if env == nil {
				continue
			}
			// On a directory server the caller is checked again, at the
			// point of sending: a user who left the list, whose key moved to
			// another user, or whose certificate is past its validity dates,
			// is sent nothing more. The stream ends; a
			// request stays pending as it does for any stream that ends (Take
			// keeps it), and an event is dropped with it.
			if stillTheCaller != nil && !stillTheCaller() {
				log.Printf("credential bus: closing the %q stream of machine %s (%q)%s: its certificate no longer authenticates its user; nothing more is sent on it",
					listener.Namespace, listener.MachineID, listener.MachineName, forBusUser(listener.User))
				return
			}
			data, err := json.Marshal(env)
			if err != nil {
				log.Printf("credential bus: cannot encode a message for the stream: %v", err)
				continue
			}
			if !write(func() error {
				_, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
				return err
			}) {
				return
			}
		case <-listener.Done:
			return
		case <-keepalive.C:
			// Idle keepalive: an SSE comment line. Clients ignore ":"-prefixed
			// lines (sdk HostClient and the CLI both skip them), so it's
			// invisible to the protocol but resets NAT/proxy idle timers.
			if !write(func() error { _, err := fmt.Fprint(w, ": keepalive\n\n"); return err }) {
				return
			}
		}
	}
}

// busCallerCheck is the check a credential-bus or delivery stream makes again
// before each message it sends, on a server whose user lookup is
// users.BoundCredentialsOnly (a directory server, whose users change while it
// runs): the request's client certificate must still be inside its validity
// dates (certValidAt, as the mtls middleware checks every request's) and
// still authenticate the stream's user (CertificateUser, against the lookup
// as it is then). nil on any other server, whose streams are authorized once,
// when they open, as they always were. A request with no client certificate
// fails the check.
func (s *Server) busCallerCheck(r *http.Request, user string) func() bool {
	if _, ok := s.users.(users.BoundCredentialsOnly); !ok {
		return nil
	}
	var leaf *x509.Certificate
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		leaf = r.TLS.PeerCertificates[0]
	}
	clock := s.busNow
	if clock == nil {
		clock = time.Now
	}
	return func() bool {
		if leaf == nil {
			return false
		}
		now := clock()
		if !certValidAt(leaf, now) {
			return false
		}
		u, _, ok := CertificateUser(s.users, leaf, now)
		return ok && !u.Implicit && u.Name == user
	}
}

// streamWriteFailed logs a stream write that failed or missed its deadline
// and reports false (the stream ends).
func (s *Server) streamWriteFailed(l *plugin.Listener, err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		log.Printf("credential bus: closing the %q stream of machine %s (%q)%s: a write was not taken within %s (the subscriber stopped reading); its next subscribe is admitted",
			l.Namespace, l.MachineID, l.MachineName, forBusUser(l.User), sseWriteTimeout)
	}
	return false
}

// handlePluginRespond handles a response from a host listener back to a shed.
//
// A response answers a pending request by the ID the server sent the
// listener for it (in_reply_to). It is accepted only from the user that
// request was routed to, for its namespace and shed, and is written into the
// shed connection the request arrived on, only while that connection still
// has the route the request was published on: a shed deleted and created
// again under the name, a transport that reconnected, or a shed whose user
// changed is never sent it. A response that answers no such request is
// refused, in every mode: pending requests are tracked in every mode, so an
// unmatched response is forged, replayed, already answered, another user's,
// swept, for a connection that is gone, or from before a server restart.
//
// The answer must come from the user's credential source: the machine named
// by the request's machine headers (403 SOURCE_MACHINE_ID_REQUIRED without
// valid ones) must be the user's bound machine (403 SOURCE_NOT_BOUND naming
// the one that is), and the request must have been sent to that machine at
// the binding's current generation. After a move of the binding the old
// machine's requests are gone and its answers are refused SOURCE_NOT_BOUND,
// an answer in flight across the move included.
//
// A final response consumes its request only once it has been written into
// the shed. A shed takes at most a few responses at once; more are refused
// with 429 and Retry-After. A response that could not begin its write in
// time is 503 with Retry-After. Neither consumes anything, so both can be
// retried. A write that fails part-way is 404, and is not retryable (the
// transport is closed). A body over maxRespondBody is 413, before it is
// decoded or matched.
func (s *Server) handlePluginRespond(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "namespace")

	user, _, ok := busCaller(r)
	if !ok {
		refuseBusCaller(w)
		return
	}
	if s.refuseServiceTarget(w, r, user) {
		return
	}
	if refuseScriptKey(w, r) {
		return
	}
	// The bridge and registry are wired together (NewBridge(registry)); require
	// both so the ownership gate below can't be skipped by a half-initialized
	// server.
	if s.bridge == nil || s.plugins == nil {
		writeError(w, http.StatusServiceUnavailable, "PLUGINS_DISABLED", "plugin system not initialized")
		return
	}
	// Fail closed: a bus without its source bindings accepts no answer.
	if s.plugins.Bindings() == nil {
		writeError(w, http.StatusServiceUnavailable, "PLUGINS_DISABLED", "credential source bindings not initialized")
		return
	}
	// The answering machine: only the user's bound machine is accepted, and
	// only for requests it was sent at the binding's current generation.
	machine, ok := machineFromRequest(r)
	if !ok {
		s.missingID.logOnce(user, callerFrom(r.Context()).Name)
		writeMachineIDRequired(w)
		return
	}

	// The body is read only up to the largest frame a guest takes: a
	// response larger than that could never be written into the shed, so
	// nothing larger is ever held, matched or not.
	r.Body = http.MaxBytesReader(w, r.Body, maxRespondBody)
	var env plugin.Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, config.ErrRequestTooLarge,
				fmt.Sprintf("response body is over %d bytes", maxRespondBody))
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "invalid JSON: "+err.Error())
		return
	}

	// Ensure the envelope matches the URL namespace
	if env.Namespace == "" {
		env.Namespace = namespace
	}

	// Require shed target for routing
	if env.Shed == nil || env.Shed.Name == "" {
		writeError(w, http.StatusBadRequest, "MISSING_SHED", "envelope must include shed.name for routing")
		return
	}

	// Match the response to a pending request of the caller's user (another
	// user's request, a forged one and a shed that does not exist all look
	// the same: no match) and take one of the shed's response slots. Nothing
	// is consumed yet.
	resp, err := s.bridge.BeginResponseFrom(user, machine.ID, &env)
	if err != nil {
		writeRespondError(w, err)
		return
	}
	defer resp.End()
	if respondValidatedHook != nil {
		respondValidatedHook()
	}
	if err := resp.Send(); err != nil {
		writeRespondError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// writeRespondError answers a response that was not delivered.
func writeRespondError(w http.ResponseWriter, err error) {
	var notBound *plugin.NotBoundError
	switch {
	case errors.As(err, &notBound):
		// Not the user's source (or no longer: the binding moved since the
		// request was sent). Nothing was matched or consumed.
		writeSourceNotBound(w, notBound.Bound)
	case errors.Is(err, plugin.ErrUnmatched):
		writeError(w, http.StatusForbidden, "FORBIDDEN", "response does not match a pending request for this namespace")
	case errors.Is(err, plugin.ErrTooManyResponses):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "TOO_MANY_RESPONSES", err.Error())
	case errors.Is(err, plugin.ErrResponseTimeout):
		// Nothing was written and the request is still pending.
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "RESPONSE_NOT_DELIVERED", "the shed did not take the response in time; nothing was written, retry")
	default:
		// The write failed: the shed's transport is gone or broke part-way.
		writeError(w, http.StatusNotFound, "SHED_NOT_CONNECTED", err.Error())
	}
}

// handleListPluginSheds returns the caller's sheds with active message
// channels, or every user's for an admin.
func (s *Server) handleListPluginSheds(w http.ResponseWriter, r *http.Request) {
	user, all, ok := busCaller(r)
	if !ok {
		refuseBusCaller(w)
		return
	}
	if s.bridge == nil {
		writeJSON(w, http.StatusOK, map[string]any{"sheds": []any{}})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"sheds": s.bridge.ListSheds(user, all)})
}
