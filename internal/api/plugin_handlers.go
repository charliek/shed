package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	if s.plugins == nil {
		writeJSON(w, http.StatusOK, map[string]any{"listeners": []any{}})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"listeners": s.plugins.List(user, all)})
}

// handlePluginSubscribe establishes an SSE stream for plugin messages.
// The caller is registered as its user's listener for the given namespace and
// receives that user's sheds' messages as SSE events until they disconnect.
// A second subscription of the same user to the same namespace is refused
// with 409; another user's subscription to it is independent.
func (s *Server) handlePluginSubscribe(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "namespace")

	user, _, ok := busCaller(r)
	if !ok {
		refuseBusCaller(w)
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

	listener, err := s.plugins.Register(plugin.ListenerKey{User: user, Namespace: namespace})
	if err != nil {
		if plugin.ValidateNamespace(namespace) != nil {
			writeError(w, http.StatusBadRequest, "INVALID_NAMESPACE", err.Error())
		} else {
			writeError(w, http.StatusConflict, "NAMESPACE_ALREADY_REGISTERED", err.Error())
		}
		return
	}

	// Ensure cleanup on disconnect: this listener only, by its token, so a
	// handler that ends late never removes a newer listener for the key.
	defer s.plugins.Unregister(listener.Key(), listener.Token)

	// Check for SSE support
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "SSE_UNSUPPORTED", "streaming not supported")
		return
	}

	// Set SSE headers (overrides ContentTypeJSON middleware)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case m := <-listener.Messages:
			// The queue carries references. Take is the check at the point
			// of sending: a request answered or dropped since it was queued,
			// or whose shed's route changed, is not sent.
			env := listener.Take(m)
			if env == nil {
				continue
			}
			writeSSEEvent(w, "message", env)
			flusher.Flush()
		case <-listener.Done:
			return
		case <-keepalive.C:
			// Idle keepalive: an SSE comment line. Clients ignore ":"-prefixed
			// lines (sdk HostClient and the CLI both skip them), so it's
			// invisible to the protocol but resets NAT/proxy idle timers.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
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
	resp, err := s.bridge.BeginResponse(user, &env)
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
	switch {
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
