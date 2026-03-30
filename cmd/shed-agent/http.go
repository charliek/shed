//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
)

const (
	// publishTimeout is the maximum time to wait for a response to a request.
	publishTimeout = 30 * time.Second

	// writeTimeout is the maximum time to wait for a vsock write to complete.
	writeTimeout = 10 * time.Second

	// maxPublishBodySize limits the request body for POST /v1/publish.
	maxPublishBodySize = 1 << 20 // 1 MB
)

var errNoConnection = errors.New("no active message connection to host")

// publishRequest is the expected JSON body for POST /v1/publish.
type publishRequest struct {
	Namespace string             `json:"namespace"`
	Type      plugin.MessageType `json:"type"`
	Payload   json.RawMessage    `json:"payload"`
}

// handlePublish handles POST /v1/publish — sends a plugin message to the host.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPublishBodySize)

	var req publishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, "INVALID_BODY", "invalid JSON: "+err.Error())
		return
	}

	if req.Namespace == "" {
		writeHTTPError(w, http.StatusBadRequest, "MISSING_NAMESPACE", "namespace is required")
		return
	}

	// Default type to request
	if req.Type == "" {
		req.Type = plugin.MessageTypeRequest
	}

	env := plugin.NewEnvelope(req.Namespace, req.Type, req.Payload)

	switch req.Type {
	case plugin.MessageTypeRequest:
		s.handlePublishRequest(r.Context(), w, env)
	case plugin.MessageTypeEvent:
		s.handlePublishEvent(w, env)
	default:
		writeHTTPError(w, http.StatusBadRequest, "INVALID_TYPE",
			fmt.Sprintf("type must be %q or %q", plugin.MessageTypeRequest, plugin.MessageTypeEvent))
	}
}

// handlePublishRequest sends a request and waits for a response.
func (s *Server) handlePublishRequest(ctx context.Context, w http.ResponseWriter, env *plugin.Envelope) {
	// Register pending response channel
	ch := make(chan *plugin.Envelope, 1)
	s.pendingMu.Lock()
	s.pending[env.ID] = ch
	s.pendingMu.Unlock()

	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, env.ID)
		s.pendingMu.Unlock()
	}()

	// Send the message
	if err := s.sendPluginMessage(env); err != nil {
		if errors.Is(err, errNoConnection) {
			writeHTTPError(w, http.StatusServiceUnavailable, "NO_CONNECTION", "no active connection to host")
			return
		}
		writeHTTPError(w, http.StatusInternalServerError, "SEND_FAILED", err.Error())
		return
	}

	// Wait for response
	timer := time.NewTimer(publishTimeout)
	defer timer.Stop()

	select {
	case resp, ok := <-ch:
		if !ok {
			writeHTTPError(w, http.StatusServiceUnavailable, "CONNECTION_LOST", "host connection lost while waiting for response")
			return
		}
		data, err := json.Marshal(resp)
		if err != nil {
			writeHTTPError(w, http.StatusBadGateway, "INVALID_RESPONSE", "host returned an invalid response")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	case <-timer.C:
		writeHTTPError(w, http.StatusGatewayTimeout, "TIMEOUT", "no response from host within timeout")
	case <-ctx.Done():
		return // HTTP caller disconnected
	case <-s.ctx.Done():
		writeHTTPError(w, http.StatusServiceUnavailable, "SHUTTING_DOWN", "agent is shutting down")
	}
}

// handlePublishEvent sends an event (fire-and-forget).
func (s *Server) handlePublishEvent(w http.ResponseWriter, env *plugin.Envelope) {
	if err := s.sendPluginMessage(env); err != nil {
		if errors.Is(err, errNoConnection) {
			writeHTTPError(w, http.StatusServiceUnavailable, "NO_CONNECTION", "no active connection to host")
			return
		}
		writeHTTPError(w, http.StatusInternalServerError, "SEND_FAILED", err.Error())
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

func writeHTTPError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(config.NewAPIError(code, message)); err != nil {
		log.Printf("Failed to write HTTP error response: %v", err)
	}
}
