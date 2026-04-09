package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmutil"
	"github.com/go-chi/chi/v5"
)

// handleConnect upgrades an HTTP connection to a raw TCP tunnel into a shed VM.
//
// GET /api/sheds/{name}/connect/{port}
//
// On success, responds with 101 Switching Protocols and the connection becomes
// a bidirectional byte stream to the specified port inside the VM.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	shedName := chi.URLParam(r, "name")
	portStr := chi.URLParam(r, "port")

	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_PORT", "port must be 1-65535")
		return
	}

	// Dial into the VM via DialService.
	vmConn, err := s.backend.DialService(r.Context(), shedName, uint16(port))
	if err != nil {
		if errors.Is(err, config.ErrShedNotFoundSentinel) {
			writeError(w, http.StatusNotFound, config.ErrShedNotFound, err.Error())
			return
		}
		if errors.Is(err, config.ErrShedNotRunningSentinel) {
			writeError(w, http.StatusServiceUnavailable, "SHED_NOT_RUNNING", err.Error())
			return
		}
		// Dial failure (connection refused, timeout, etc.)
		writeError(w, http.StatusBadGateway, "CONNECT_FAILED", err.Error())
		return
	}

	// Hijack the HTTP connection.
	hj, ok := w.(http.Hijacker)
	if !ok {
		vmConn.Close()
		writeError(w, http.StatusInternalServerError, "HIJACK_UNSUPPORTED", "server does not support connection hijacking")
		return
	}

	// Send the upgrade response.
	w.Header().Set("Connection", "Upgrade")
	w.Header().Set("Upgrade", "shed-tcp")
	w.WriteHeader(http.StatusSwitchingProtocols)

	clientConn, _, err := hj.Hijack()
	if err != nil {
		vmConn.Close()
		log.Printf("Connect API: hijack failed for %s:%d: %v", shedName, port, err)
		return
	}

	log.Printf("Connect API: tunnel %s:%d established", shedName, port)

	// Bidirectional proxy.
	vmutil.BidirectionalCopy(clientConn, vmConn)
	clientConn.Close()
	vmConn.Close()

	log.Printf("Connect API: tunnel %s:%d closed", shedName, port)
}
