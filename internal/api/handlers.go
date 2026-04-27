package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/version"
	"github.com/go-chi/chi/v5"
)

// handleGetInfo returns server information.
// GET /api/info
func (s *Server) handleGetInfo(w http.ResponseWriter, r *http.Request) {
	info := config.ServerInfo{
		Name:     s.cfg.Name,
		Version:  version.Info(),
		SSHPort:  s.cfg.SSHPort,
		HTTPPort: s.cfg.HTTPPort,
		Backend:  s.cfg.DefaultBackend,
	}

	writeJSON(w, http.StatusOK, info)
}

// handleGetSSHHostKey returns the server's SSH host key.
// GET /api/ssh-host-key
func (s *Server) handleGetSSHHostKey(w http.ResponseWriter, r *http.Request) {
	resp := config.SSHHostKeyResponse{
		HostKey: s.sshHostKey,
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleListSheds returns all sheds.
// GET /api/sheds
func (s *Server) handleListSheds(w http.ResponseWriter, r *http.Request) {
	sheds, err := s.backend.ListSheds(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, config.ErrBackendError, err.Error())
		return
	}

	resp := config.ShedsResponse{
		Sheds: sheds,
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleCreateShed creates a new shed.
// POST /api/sheds
// If the client sends Accept: text/event-stream, progress is streamed as SSE.
func (s *Server) handleCreateShed(w http.ResponseWriter, r *http.Request) {
	var req config.CreateShedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidShedName, "invalid request body: "+err.Error())
		return
	}

	// Validate shed name
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, config.ErrInvalidShedName, "shed name is required")
		return
	}
	if err := config.ValidateShedName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidShedName, err.Error())
		return
	}

	// Validate local_dir and repo are mutually exclusive
	if req.LocalDir != "" && req.Repo != "" {
		writeError(w, http.StatusBadRequest, config.ErrInvalidLocalDir, "local_dir and repo are mutually exclusive")
		return
	}

	// from_snapshot validation runs BEFORE either the SSE branch or the
	// backend call: handleCreateShedSSE writes "200 OK" before the backend
	// runs, so a backend-only sentinel would surface as a streamed error
	// instead of the 400 INVALID_REQUEST clients expect.
	if req.FromSnapshot != "" {
		if err := config.ValidateSnapshotName(req.FromSnapshot); err != nil {
			writeError(w, http.StatusBadRequest, config.ErrInvalidSnapshotName, err.Error())
			return
		}
		if req.Image != "" || req.Repo != "" {
			writeError(w, http.StatusBadRequest, config.ErrInvalidRequest,
				"invalid create-shed request: --from-snapshot cannot be combined with --image or --repo")
			return
		}
	}

	// Validate local_dir exists on the server
	if req.LocalDir != "" {
		if !filepath.IsAbs(req.LocalDir) {
			writeError(w, http.StatusBadRequest, config.ErrInvalidLocalDir, "local_dir must be an absolute path")
			return
		}
		if strings.Contains(req.LocalDir, ",") {
			writeError(w, http.StatusBadRequest, config.ErrInvalidLocalDir, "local_dir path must not contain commas")
			return
		}
		info, err := os.Stat(req.LocalDir)
		if err != nil {
			if os.IsNotExist(err) {
				writeError(w, http.StatusBadRequest, config.ErrInvalidLocalDir, fmt.Sprintf("local_dir %q does not exist", req.LocalDir))
			} else if os.IsPermission(err) {
				writeError(w, http.StatusBadRequest, config.ErrInvalidLocalDir, fmt.Sprintf("local_dir %q: permission denied", req.LocalDir))
			} else {
				writeError(w, http.StatusBadRequest, config.ErrInvalidLocalDir, fmt.Sprintf("local_dir %q: %v", req.LocalDir, err))
			}
			return
		}
		if !info.IsDir() {
			writeError(w, http.StatusBadRequest, config.ErrInvalidLocalDir, fmt.Sprintf("local_dir %q is not a directory", req.LocalDir))
			return
		}
	}

	// Expand repo shorthand (e.g., "owner/repo" -> "git@github.com:owner/repo.git")
	// and validate the resulting URL
	req.Repo = config.ExpandRepoShorthand(req.Repo)
	if err := config.ValidateGitRepoURL(req.Repo); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRepoURL, err.Error())
		return
	}

	// Stream progress via SSE if the client requests it
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.handleCreateShedSSE(w, r, req)
		return
	}

	shed, err := s.backend.CreateShed(r.Context(), req)
	if err != nil {
		log.Printf("CreateShed failed for %q (backend=%s): %v", req.Name, req.Backend, err)
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}

	writeJSON(w, http.StatusCreated, shed)
}

// handleCreateShedSSE streams create progress as Server-Sent Events.
func (s *Server) handleCreateShedSSE(w http.ResponseWriter, r *http.Request, req config.CreateShedRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, config.ErrBackendError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events := make(chan backend.ProgressEvent, 16)
	progressFn := func(event backend.ProgressEvent) {
		select {
		case events <- event:
		default:
		}
	}

	ctx := backend.ContextWithProgress(r.Context(), progressFn)

	type createResult struct {
		shed *config.Shed
		err  error
	}
	done := make(chan createResult, 1)

	go func() {
		shed, err := s.backend.CreateShed(ctx, req)
		done <- createResult{shed, err}
	}()

	// Stream progress events until creation completes.
	// Uses select to avoid closing the events channel (which would race with sends).
	var res createResult
	streaming := true
	for streaming {
		select {
		case event := <-events:
			writeSSEEvent(w, "progress", event)
			flusher.Flush()
		case res = <-done:
			streaming = false
		}
	}

	// Drain any remaining buffered progress events
drain:
	for {
		select {
		case event := <-events:
			writeSSEEvent(w, "progress", event)
			flusher.Flush()
		default:
			break drain
		}
	}

	if res.err != nil {
		log.Printf("CreateShed failed for %q (backend=%s): %v", req.Name, req.Backend, res.err)
		_, errCode, msg := mapBackendError(res.err)
		writeSSEEvent(w, "error", config.NewAPIError(errCode, msg))
	} else {
		writeSSEEvent(w, "complete", res.shed)
	}
	flusher.Flush()
}

// writeSSEEvent writes a single Server-Sent Event.
func writeSSEEvent(w http.ResponseWriter, eventType string, data any) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Printf("writeSSEEvent: failed to marshal %s event: %v", eventType, err)
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(jsonData))
}

// handleGetShed returns a single shed by name.
// GET /api/sheds/{name}
func (s *Server) handleGetShed(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	shed, err := s.backend.GetShed(r.Context(), name)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}

	writeJSON(w, http.StatusOK, shed)
}

// handleDeleteShed deletes a shed.
// DELETE /api/sheds/{name}?keep_volume=bool
func (s *Server) handleDeleteShed(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	keepVolume := r.URL.Query().Get("keep_volume") == "true"

	if err := s.backend.DeleteShed(r.Context(), name, keepVolume); err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleStartShed starts a stopped shed.
// POST /api/sheds/{name}/start
func (s *Server) handleStartShed(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	shed, err := s.backend.StartShed(r.Context(), name)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}

	writeJSON(w, http.StatusOK, shed)
}

// handleStopShed stops a running shed.
// POST /api/sheds/{name}/stop
func (s *Server) handleStopShed(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	shed, err := s.backend.StopShed(r.Context(), name)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}

	writeJSON(w, http.StatusOK, shed)
}

// handleListSessions returns all tmux sessions in a shed.
// GET /api/sheds/{name}/sessions
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	sessions, err := s.backend.ListSessions(r.Context(), name)
	if err != nil {
		code, errCode, msg := mapSessionError(err)
		writeError(w, code, errCode, msg)
		return
	}

	resp := config.SessionsResponse{
		Sessions: sessions,
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleKillSession terminates a tmux session in a shed.
// DELETE /api/sheds/{name}/sessions/{session}
func (s *Server) handleKillSession(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	sessionName := chi.URLParam(r, "session")

	// Validate session name to return 400 for invalid input
	if err := config.ValidateSessionName(sessionName); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidSessionName, err.Error())
		return
	}

	if err := s.backend.KillSession(r.Context(), name, sessionName); err != nil {
		code, errCode, msg := mapSessionError(err)
		writeError(w, code, errCode, msg)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleListAllSessions returns all tmux sessions across all running sheds.
// GET /api/sessions
func (s *Server) handleListAllSessions(w http.ResponseWriter, r *http.Request) {
	sheds, err := s.backend.ListSheds(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, config.ErrBackendError, err.Error())
		return
	}

	var allSessions []config.Session
	var warnings []string
	for _, shed := range sheds {
		if shed.Status != config.StatusRunning {
			continue
		}
		sessions, err := s.backend.ListSessions(r.Context(), shed.Name)
		if err != nil {
			// Record warning for sheds where we can't list sessions
			if errors.Is(err, config.ErrTmuxNotAvailableSentinel) {
				warnings = append(warnings, "shed "+shed.Name+": tmux not available")
			} else {
				warnings = append(warnings, "shed "+shed.Name+": "+err.Error())
			}
			continue
		}
		allSessions = append(allSessions, sessions...)
	}

	resp := config.SessionsResponse{
		Sessions: allSessions,
		Warnings: warnings,
	}

	writeJSON(w, http.StatusOK, resp)
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.WriteHeader(status)
	if data != nil {
		if err := json.NewEncoder(w).Encode(data); err != nil {
			// Log error but can't change response at this point
			return
		}
	}
}

// writeError writes an error response.
func writeError(w http.ResponseWriter, status int, code, message string) {
	apiErr := config.NewAPIError(code, message)
	writeJSON(w, status, apiErr)
}

// mapBackendError maps a backend error to an HTTP status code, error code, and message.
func mapBackendError(err error) (int, string, string) {
	// Check sentinel errors
	if errors.Is(err, config.ErrShedNotFoundSentinel) {
		return http.StatusNotFound, config.ErrShedNotFound, err.Error()
	}
	if errors.Is(err, config.ErrShedAlreadyExistsSentinel) {
		return http.StatusConflict, config.ErrShedAlreadyExists, err.Error()
	}
	if errors.Is(err, config.ErrShedAlreadyRunningSentinel) {
		return http.StatusConflict, config.ErrShedAlreadyRunning, err.Error()
	}
	if errors.Is(err, config.ErrShedNotRunningSentinel) {
		return http.StatusConflict, config.ErrShedAlreadyStopped, err.Error()
	}
	if errors.Is(err, config.ErrUnknownImageSentinel) {
		return http.StatusBadRequest, config.ErrUnknownImage, err.Error()
	}
	if errors.Is(err, config.ErrImageNotFoundSentinel) {
		return http.StatusNotFound, config.ErrImageNotFound, err.Error()
	}
	if errors.Is(err, config.ErrImageInUseSentinel) {
		return http.StatusConflict, config.ErrImageInUse, err.Error()
	}
	if errors.Is(err, config.ErrNotSupportedSentinel) {
		return http.StatusNotImplemented, config.ErrBackendError, err.Error()
	}
	if errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
		return http.StatusNotFound, config.ErrSnapshotNotFound, err.Error()
	}
	if errors.Is(err, config.ErrSnapshotAlreadyExistsSentinel) {
		return http.StatusConflict, config.ErrSnapshotAlreadyExists, err.Error()
	}
	if errors.Is(err, config.ErrSnapshotSourceRunningSentinel) {
		return http.StatusConflict, config.ErrSnapshotSourceRunning, err.Error()
	}
	if errors.Is(err, config.ErrSnapshotBackendMismatchSentinel) {
		return http.StatusBadRequest, config.ErrSnapshotBackendMismatch, err.Error()
	}
	if errors.Is(err, config.ErrInvalidShedRequestSentinel) {
		return http.StatusBadRequest, config.ErrInvalidRequest, err.Error()
	}

	// Fallback to string matching for errors without sentinels
	errMsg := err.Error()
	if strings.Contains(errMsg, "not found") {
		return http.StatusNotFound, config.ErrShedNotFound, errMsg
	}
	if strings.Contains(errMsg, "already exists") {
		return http.StatusConflict, config.ErrShedAlreadyExists, errMsg
	}
	if strings.Contains(errMsg, "already running") {
		return http.StatusConflict, config.ErrShedAlreadyRunning, errMsg
	}
	if strings.Contains(errMsg, "already stopped") || strings.Contains(errMsg, "not running") {
		return http.StatusConflict, config.ErrShedAlreadyStopped, errMsg
	}
	if strings.Contains(errMsg, "backend") && strings.Contains(errMsg, "not enabled") {
		return http.StatusBadRequest, config.ErrBackendNotEnabled, errMsg
	}

	// Pass through unrecognized backend errors. Shed is a developer tool
	// where the user owns the server, so error details help debugging.
	return http.StatusInternalServerError, config.ErrBackendError, errMsg
}

// mapSessionError maps a session-related error to an HTTP status code, error code, and message.
func mapSessionError(err error) (int, string, string) {
	// Check for specific sentinel errors using errors.Is
	if errors.Is(err, config.ErrSessionNotFoundSentinel) {
		return http.StatusNotFound, config.ErrSessionNotFound, err.Error()
	}
	if errors.Is(err, config.ErrTmuxNotAvailableSentinel) {
		return http.StatusServiceUnavailable, config.ErrTmuxNotAvailable, "tmux is not available in this container"
	}
	if errors.Is(err, config.ErrShedNotRunningSentinel) {
		return http.StatusConflict, config.ErrShedAlreadyStopped, err.Error()
	}

	// Fall back to string matching for errors that don't use sentinel pattern
	errMsg := err.Error()
	if strings.Contains(errMsg, "not found") {
		return http.StatusNotFound, config.ErrShedNotFound, errMsg
	}

	// Pass through unrecognized session errors — details help debugging.
	return http.StatusInternalServerError, config.ErrBackendError, errMsg
}

// handleListImages returns available image variants across all backends.
// GET /api/images
func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	images, err := s.backend.ListImages(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, config.ErrBackendError, err.Error())
		return
	}
	if images == nil {
		images = []config.ImageInfo{}
	}
	writeJSON(w, http.StatusOK, config.ImagesResponse{Images: images})
}

// handleDeleteImage removes a cached image by name.
// DELETE /api/images/{name}
func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	if err := s.backend.DeleteImage(r.Context(), name); err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handlePruneImages removes unused cached images.
// POST /api/images/prune?dry_run=bool
func (s *Server) handlePruneImages(w http.ResponseWriter, r *http.Request) {
	var dryRun bool
	if v := r.URL.Query().Get("dry_run"); v != "" {
		var err error
		dryRun, err = strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, config.ErrBackendError, "invalid dry_run value: "+v)
			return
		}
	}

	deleted, err := s.backend.PruneImages(r.Context(), dryRun)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	if deleted == nil {
		deleted = []config.ImageInfo{}
	}

	writeJSON(w, http.StatusOK, config.PruneImagesResponse{Deleted: deleted})
}
