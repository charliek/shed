package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// Cloud sheds' leases and usage (plan 041 §3.4): a remote server's machines
// come from the account service, which deletes each at its lease's end. A
// person extends a lease; anyone may read the machines held against the
// service's limits, as AuthorizeUsage lets them.

// extendBodyLimit bounds the extend request body.
const extendBodyLimit = 1024

// offersLease reports whether the backend has leases: it can extend one
// (backend.Extender), which the remote backend can.
func (s *Server) offersLease() bool {
	if s.backend == nil {
		return false
	}
	_, ok := backend.AsExtender(s.backend)
	return ok
}

// handleExtendShed moves a cloud shed's lease later.
// POST /api/sheds/{name}/extend {"hours": N, "request_id": "<uuid>"?} -> 200 the shed
//
// request_id, optional, is the caller's id for this extend: a POST sent again
// with it (a reply the caller did not get) moves the lease once. Without it
// every POST is a new extend.
//
// 501 FEATURE_UNSUPPORTED on a backend without leases; 400 INVALID_REQUEST
// for hours outside 1 to 720; 403 FORBIDDEN for a service client, a script
// key, or a user who is neither the shed's nor an admin; the service's
// refusals by their codes (EXTEND_NOT_ALLOWED, LEASE_AT_CAP, LEASE_ENDED,
// PERSON_INACTIVE, COMPUTE_DELETING, COMPUTE_UNAVAILABLE).
func (s *Server) handleExtendShed(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ex, ok := backend.AsExtender(s.backend)
	if !ok {
		s.writeFeatureUnsupported(w, http.StatusNotImplemented, "extend")
		return
	}
	var req config.ExtendShedRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, extendBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, `the body must be {"hours": N}: `+err.Error())
		return
	}
	if req.Hours < 1 || req.Hours > config.MaxExtendHours {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, fmt.Sprintf("hours must be 1 to %d", config.MaxExtendHours))
		return
	}
	if req.RequestID != "" && !config.ValidUUID(req.RequestID) {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "request_id must be a UUID")
		return
	}
	// The extend's request id is stored before it is sent and its retries
	// carry it: the call finishes once begun, whatever the caller does.
	shed, err := ex.ExtendShed(context.WithoutCancel(r.Context()), name, req.Hours, req.RequestID)
	if err != nil {
		writeBackendError(w, r.Context(), err)
		return
	}
	writeJSON(w, http.StatusOK, shed)
}

// handleCloudUsage reports the machines held against the account service's
// limits.
// GET /api/cloud/usage[?user=<name>] -> 200 {"person": …|null, "org": …}
//
// A person's own counts and the organization's; an admin may name another
// user; a local admin and a service client get the organization's only. 501
// FEATURE_UNSUPPORTED on a server without compute.
func (s *Server) handleCloudUsage(w http.ResponseWriter, r *http.Request) {
	cu, ok := backend.AsCloudUsager(s.backend)
	if !ok {
		s.writeFeatureUnsupported(w, http.StatusNotImplemented, "cloud usage")
		return
	}
	q := r.URL.Query()
	if len(q["user"]) > 1 {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "user may be given once")
		return
	}
	asked := q.Get("user")
	if asked != "" && !config.ValidUserName(asked) {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, fmt.Sprintf("user %q is not a user name", asked))
		return
	}
	usage, err := cu.CloudUsage(r.Context(), asked)
	if err != nil {
		writeBackendError(w, r.Context(), err)
		return
	}
	writeJSON(w, http.StatusOK, usage)
}

// createLeaseRefusal checks a create's lease: only a service client sends
// one, and only to a server with leases (422 FEATURE_UNSUPPORTED otherwise,
// written here); it must be a duration of whole seconds, at least a minute
// (400). It sets req.ClientLease. ok is false when a refusal was written.
// The client's policy.max_age is checked with the rest of its policy
// (clientCreateRefusal).
func (s *Server) createLeaseRefusal(w http.ResponseWriter, c Caller, req *config.CreateShedRequest) bool {
	req.ClientLease = 0
	if req.Lease == "" {
		return true
	}
	if !c.IsClient() || !s.offersLease() {
		writeError(w, http.StatusUnprocessableEntity, config.ErrFeatureUnsupported,
			"lease is only for a service client's cloud shed, on a server whose sheds have leases")
		return false
	}
	d, err := time.ParseDuration(req.Lease)
	if err != nil || d < time.Minute || d%time.Second != 0 {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest,
			fmt.Sprintf("lease %q is not a duration of whole seconds, at least 1m (for example 30h)", req.Lease))
		return false
	}
	req.ClientLease = d
	return true
}
