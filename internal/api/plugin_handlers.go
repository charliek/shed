package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/charliek/shed/internal/plugin"
	"github.com/go-chi/chi/v5"
)

// sseKeepaliveInterval is how often an idle SSE subscription emits a comment
// ping. It keeps an idle NAT mapping / proxy IdleTimeout from evicting a
// long-lived, quiet bus stream (the Phase-1→Phase-5 keepalive). A package var
// so tests can shorten it.
var sseKeepaliveInterval = 25 * time.Second

// handleListListeners returns all active plugin listeners.
func (s *Server) handleListListeners(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		writeJSON(w, http.StatusOK, map[string]any{"listeners": []any{}})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"listeners": s.plugins.List()})
}

// handlePluginSubscribe establishes an SSE stream for plugin messages.
// The caller is registered as the listener for the given namespace and receives
// messages as SSE events until they disconnect.
func (s *Server) handlePluginSubscribe(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "namespace")

	if s.plugins == nil {
		writeError(w, http.StatusServiceUnavailable, "PLUGINS_DISABLED", "plugin system not initialized")
		return
	}

	if plugin.IsSystemNamespace(namespace) {
		writeError(w, http.StatusForbidden, "NAMESPACE_RESERVED",
			fmt.Sprintf("namespace prefix %q is reserved for internal use", "system:"))
		return
	}

	listener, err := s.plugins.Register(namespace)
	if err != nil {
		if plugin.ValidateNamespace(namespace) != nil {
			writeError(w, http.StatusBadRequest, "INVALID_NAMESPACE", err.Error())
		} else {
			writeError(w, http.StatusConflict, "NAMESPACE_ALREADY_REGISTERED", err.Error())
		}
		return
	}

	// Ensure cleanup on disconnect
	defer s.plugins.Unregister(namespace)

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
		case env, ok := <-listener.Messages:
			if !ok {
				return
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
func (s *Server) handlePluginRespond(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "namespace")

	if s.bridge == nil {
		writeError(w, http.StatusServiceUnavailable, "PLUGINS_DISABLED", "plugin system not initialized")
		return
	}

	var env plugin.Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
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

	if err := s.bridge.SendToShed(env.Shed.Name, &env); err != nil {
		writeError(w, http.StatusNotFound, "SHED_NOT_CONNECTED", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleListPluginSheds returns all sheds with active message channels.
func (s *Server) handleListPluginSheds(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil {
		writeJSON(w, http.StatusOK, map[string]any{"sheds": []any{}})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"sheds": s.bridge.ListSheds()})
}
