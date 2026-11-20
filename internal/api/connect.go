package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
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

	// A person's tunnel into a service client's shed writes a connect line
	// once it is established (below). For that the shed is checked first,
	// and the dial is pinned to the shed that check found: its incarnation
	// and its owner and user. The guarded dial then refuses (as not found)
	// a shed that changed in between, so the line names exactly the shed and
	// owner the tunnel went into. A service client's own Connect is
	// recorded by its api line and takes none of this, nor does anything
	// when the audit log is off.
	ctx := r.Context()
	caller := callerFrom(ctx)
	var owned *config.Shed
	endEntry := func() {}
	if s.audit != nil && !caller.IsClient() {
		shed, err := backend.Guarded(s.backend).Check(ctx, shedName, backend.ActionEnter)
		if err != nil {
			s.writeConnectError(w, err)
			return
		}
		ctx = backend.PinOwnership(backend.PinCreatedAt(ctx, shed.CreatedAt), shed.Owner, shed.User)
		if shed.Owner != "" {
			// Held from here until its line is written, so the server's
			// shutdown waits for that line before the audit log is closed
			// (DrainEntries); once the shutdown has begun, none starts.
			end, ok := s.connects.Begin()
			if !ok {
				writeError(w, http.StatusServiceUnavailable, config.ErrInternalError, "the server is shutting down; try again once it is back")
				return
			}
			defer end()
			owned, endEntry = shed, end
		}
	}

	// Dial into the VM via DialService.
	vmConn, err := s.backend.DialService(ctx, shedName, uint16(port))
	if err != nil {
		s.writeConnectError(w, err)
		return
	}

	// Hijack the HTTP connection.
	//
	// Auth note: this is the last point at which any middleware runs on this
	// connection. Past the hijack the bytes flow through BidirectionalCopy with
	// no request boundary left, so an established tunnel outlives the expiry or
	// revocation of the credential that opened it (token or client certificate
	// alike) until one side closes it. That parity limitation is documented in
	// full on authMiddleware in auth.go — see the "Known parity limitation"
	// paragraph there before changing anything here.
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

	clientConn, bufrw, err := hj.Hijack()
	if err != nil {
		vmConn.Close()
		log.Printf("Connect API: hijack failed for %s:%d: %v", shedName, port, err)
		return
	}

	log.Printf("Connect API: tunnel %s:%d established", shedName, port)

	// The tunnel is established: a person's into a service client's shed
	// is recorded now. The line is written inline, as every audit line is:
	// a stalled audit file stalls the tunnel before any byte flows, which
	// is the fail-closed choice for an audited entry.
	if owned != nil {
		s.recordAudit(audit.ConnectEvent{
			Event:    audit.EventConnect,
			Identity: auditIdentity(caller),
			Remote:   r.RemoteAddr,
			Shed:     owned.Name,
			Owner:    owned.Owner,
			Port:     int(port),
		})
		endEntry()
	}

	// The HTTP server's bufio.Reader may have read past the request headers
	// into the body before Hijack returned. Wrap clientConn with BufferedConn
	// so BidirectionalCopy reads those buffered bytes first; otherwise they
	// are stranded in bufrw and lost. Mirrors internal/tunnels/connect.go.
	bufferedClient := &vmutil.BufferedConn{Conn: clientConn, Reader: bufrw.Reader}

	// Bidirectional proxy.
	vmutil.BidirectionalCopy(bufferedClient, vmConn)
	clientConn.Close()
	vmConn.Close()

	log.Printf("Connect API: tunnel %s:%d closed", shedName, port)
}

// writeConnectError answers a Connect whose check or dial failed.
func (s *Server) writeConnectError(w http.ResponseWriter, err error) {
	if status, code, msg, ok := mapAccessError(err); ok {
		writeError(w, status, code, msg)
		return
	}
	if errors.Is(err, config.ErrShedNotRunningSentinel) {
		// An asynchronous backend's shed may be starting or stopping:
		// that is a state conflict (409, as exec answers), not an outage.
		// Firecracker and VZ keep their 503.
		status := http.StatusServiceUnavailable
		if _, ok := backend.IsAsync(s.backend); ok {
			status = http.StatusConflict
		}
		writeError(w, status, config.ErrShedNotRunning, err.Error())
		return
	}
	// Dial failure (connection refused, timeout, etc.)
	writeError(w, http.StatusBadGateway, "CONNECT_FAILED", err.Error())
}

// CloseEntries refuses every later Connect a person opens into a service
// client's shed (503). It does not wait: DrainEntries does. shed-server calls
// it first thing when its shutdown begins, before it waits on any server.
func (s *Server) CloseEntries() { s.connects.Close() }

// DrainEntries refuses every later Connect a person opens into a service
// client's shed, and waits, until ctx ends, for those already on their way in
// to write their connect lines. shed-server runs it beside the HTTP servers'
// shutdown (which does not wait for a Connect's hijacked connection), under
// the same bound, and closes the audit log only after it returns.
func (s *Server) DrainEntries(ctx context.Context) error {
	return s.connects.Drain(ctx)
}
