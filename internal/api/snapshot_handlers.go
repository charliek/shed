package api

import (
	"encoding/json"
	"net/http"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/go-chi/chi/v5"
)

// handleListSnapshots returns all snapshots managed by this server.
// GET /api/snapshots
func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.backend.ListSnapshots(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, config.ErrBackendError, err.Error())
		return
	}
	if snaps == nil {
		snaps = []config.Snapshot{}
	}
	writeJSON(w, http.StatusOK, config.SnapshotsResponse{Snapshots: snaps})
}

// handleCreateSnapshot creates a new snapshot from a stopped shed.
// POST /api/snapshots
//
// Non-fatal warnings emitted by the backend via backend.StatusWarning
// are collected and returned in the response body alongside the snapshot,
// so the operator can see e.g. the "--local-dir not captured" warning
// without needing SSE plumbing on this endpoint.
func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	var req config.SnapshotCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "invalid request body: "+err.Error())
		return
	}

	if err := config.ValidateSnapshotName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidSnapshotName, err.Error())
		return
	}
	if err := config.ValidateShedName(req.SourceShed); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidShedName, err.Error())
		return
	}

	var warnings []string
	collect := func(event backend.ProgressEvent) {
		if event.Warning {
			warnings = append(warnings, event.Message)
		}
	}
	ctx := backend.ContextWithProgress(r.Context(), collect)

	snap, err := s.backend.CreateSnapshot(ctx, req)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	writeJSON(w, http.StatusCreated, config.SnapshotCreateResponse{
		Snapshot: snap,
		Warnings: warnings,
	})
}

// handleGetSnapshot returns a single snapshot by name.
// GET /api/snapshots/{name}
func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := config.ValidateSnapshotName(name); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidSnapshotName, err.Error())
		return
	}

	snap, err := s.backend.GetSnapshot(r.Context(), name)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleDeleteSnapshot removes a snapshot.
// DELETE /api/snapshots/{name}
func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := config.ValidateSnapshotName(name); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidSnapshotName, err.Error())
		return
	}

	if err := s.backend.DeleteSnapshot(r.Context(), name); err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
