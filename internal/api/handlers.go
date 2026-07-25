package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/version"
	"github.com/charliek/shed/internal/vmimage"
	"github.com/go-chi/chi/v5"
)

// handleGetInfo returns server information.
// GET /api/info
func (s *Server) handleGetInfo(w http.ResponseWriter, r *http.Request) {
	info := config.ServerInfo{
		Name:         s.cfg.Name,
		Version:      version.Info(),
		SSHPort:      s.cfg.SSHPort,
		HTTPPort:     s.cfg.HTTPPort,
		Backend:      s.cfg.DefaultBackend,
		AuthMode:     s.cfg.AuthModeValue(),
		DefaultImage: s.cfg.ActiveDefaultImage(),
		HTTPSPort:    s.cfg.HTTPSPort,
		Features:     serverFeatures(),
	}
	// Client-CA visibility, mtls only. Reaching /api/info in mtls mode already
	// required a valid client certificate (that mode has no bootstrap
	// exemptions), so this is reported to authenticated callers only.
	if s.cfg.MTLSMode() {
		info.CAFingerprint = s.caFingerprint
		if !s.caNotAfter.IsZero() {
			info.CANotAfter = s.caNotAfter.UTC().Format(time.RFC3339)
		}
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
	if len(req.AddDirs) > 0 && req.LocalDir == "" {
		writeError(w, http.StatusBadRequest, config.ErrInvalidLocalDir, "add_dirs requires local_dir")
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

	// Validate local_dir / add_dirs exist on the server. validateMountDir
	// writes the appropriate 400 and returns false on the first problem.
	if req.LocalDir != "" {
		validateMountDir := func(field, p string) bool {
			if err := config.ValidateMountDir(p); err != nil {
				writeError(w, http.StatusBadRequest, config.ErrInvalidLocalDir, fmt.Sprintf("%s %q %s", field, p, err.Error()))
				return false
			}
			return true
		}

		if !validateMountDir("local_dir", req.LocalDir) {
			return
		}
		for _, d := range req.AddDirs {
			if !validateMountDir("add_dir", d) {
				return
			}
		}

		// Structural validation: basename uniqueness + dotfile/home-infra guard.
		if _, _, err := config.BuildProjectMounts(req.LocalDir, req.AddDirs); err != nil {
			writeError(w, http.StatusBadRequest, config.ErrInvalidLocalDir, err.Error())
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

	timer := s.createTimer(req)
	ctx := backend.ContextWithProgress(r.Context(), timer.Track)
	shed, err := s.backend.CreateShed(ctx, req)
	log.Printf("timing: %s", timer.Finish(err))
	if err != nil {
		log.Printf("CreateShed failed for %q (backend=%s): %v", req.Name, req.Backend, err)
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}

	writeJSON(w, http.StatusCreated, shed)
}

// createTimer returns a PhaseTimer labelled for a CreateShed operation.
// The timer is installed for every create (SSE or not) so the per-phase
// breakdown always lands in the server log, independent of whether a
// client is streaming progress.
func (s *Server) createTimer(req config.CreateShedRequest) *backend.PhaseTimer {
	return backend.NewPhaseTimer(
		fmt.Sprintf("create name=%s backend=%s", req.Name, s.backend.Type()), nil)
}

// sseProgressBuffer is the depth of the per-request progress channel. The
// sink send is ctx-aware-blocking (never a silent drop), so this is just
// decoupling headroom for bursts; byte ticks are coalesced at the source.
const sseProgressBuffer = 64

// newProgressSink builds the SSE progress sink. It forwards user-visible
// events into ch and:
//   - gates structured byte-progress (Kind=="blob") behind the client's
//     ?progress=blob opt-in, so older / line-mode clients never receive
//     byte-tick events (no spam, no behavior change);
//   - drops phase-only (Message=="") events, which are server-side timer
//     boundaries, not user-visible lines;
//   - blocks until the drain goroutine catches up or the request is
//     cancelled, so a status transition is never silently dropped (the old
//     `default:`-drop could lose a terminal "done", freezing a bar at 99%).
func newProgressSink(r *http.Request, ch chan<- backend.ProgressEvent) backend.ProgressFunc {
	blobCapable := r.URL.Query().Get("progress") == backend.KindBlob
	done := r.Context().Done()
	return func(event backend.ProgressEvent) {
		if event.Kind == backend.KindBlob && !blobCapable {
			return
		}
		if event.Message == "" {
			return
		}
		select {
		case ch <- event:
		case <-done:
		}
	}
}

// handleCreateShedSSE streams create progress as Server-Sent Events.
func (s *Server) handleCreateShedSSE(w http.ResponseWriter, r *http.Request, req config.CreateShedRequest) {
	timer := s.createTimer(req)
	s.streamSSE(w, r, r.Context(), timer.Track, logTimingFinish(timer), func(ctx context.Context) (any, error) {
		shed, err := s.backend.CreateShed(ctx, req)
		if err != nil {
			log.Printf("CreateShed failed for %q (backend=%s): %v", req.Name, req.Backend, err)
		}
		return shed, err
	})
}

// streamSSE runs a backend operation while streaming its progress to the client
// as Server-Sent Events. It owns the flusher/header preamble, the pump+drain
// loop (a select rather than closing the channel, which would race sends), and
// the terminal complete/error event. baseCtx is the context work runs under —
// create/pull pass r.Context(); delete passes a cancellation-detached one so a
// client disconnect can't strand a half-torn-down shed. The progress sink is
// always tied to the request, so a disconnected client simply stops receiving
// events. work returns the payload for the terminal "complete" event (ignored
// on error).
//
// track is an optional extra progress sink teed alongside the client stream
// (create/delete pass timer.Track to record per-phase durations; pull passes
// nil). onFinish, if non-nil, runs once with the operation's error after the
// stream drains (create/delete log the timing summary via logTimingFinish; pull
// passes nil and logs its own digest+duration inside work). Passing the resolved
// sink + finalizer rather than a *PhaseTimer keeps this pump backend-agnostic.
func (s *Server) streamSSE(w http.ResponseWriter, r *http.Request, baseCtx context.Context, track backend.ProgressFunc, onFinish func(error), work func(ctx context.Context) (any, error)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, config.ErrBackendError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events := make(chan backend.ProgressEvent, sseProgressBuffer)
	sseFn := newProgressSink(r, events)

	// Tee the progress stream: track (when non-nil) records per-phase durations
	// server-side; sseFn forwards the human-readable messages to the client.
	// Timing never goes on the wire. TeeProgress drops a nil track.
	ctx := backend.ContextWithProgress(baseCtx, backend.TeeProgress(track, sseFn))

	type result struct {
		payload any
		err     error
	}
	done := make(chan result, 1)
	go func() {
		payload, err := work(ctx)
		done <- result{payload, err}
	}()

	var res result
	for streaming := true; streaming; {
		select {
		case event := <-events:
			writeSSEEvent(w, "progress", event)
			flusher.Flush()
		case res = <-done:
			streaming = false
		}
	}

	// Drain any remaining buffered progress events.
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

	if onFinish != nil {
		onFinish(res.err)
	}

	if res.err != nil {
		_, errCode, msg := mapBackendError(res.err)
		writeSSEEvent(w, "error", config.NewAPIError(errCode, msg))
	} else {
		writeSSEEvent(w, "complete", res.payload)
	}
	flusher.Flush()
}

// logTimingFinish returns a streamSSE onFinish callback that stops the timer and
// logs the per-phase timing summary. Create/delete pass it; pull passes nil (it
// has no PhaseTimer). The timer is captured non-nil at the call site, so no
// nil-receiver method value ever reaches streamSSE.
func logTimingFinish(timer *backend.PhaseTimer) func(error) {
	return func(err error) {
		log.Printf("timing: %s", timer.Finish(err))
	}
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

// handleEgressShow returns a shed's egress status: effective profiles, assigned
// listener port, the resolved profile definitions, and recent decisions.
// GET /api/egress/{name}
func (s *Server) handleEgressShow(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	shed, err := s.backend.GetShed(r.Context(), name)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}

	status := config.EgressStatus{
		Shed:     name,
		Enabled:  s.cfg.Egress != nil && s.cfg.Egress.Enabled,
		Profiles: shed.EgressProfiles,
		Port:     shed.EgressPort,
	}
	// A shed created with the server default has no explicit profiles but does
	// have a listener — show the effective default selection.
	if len(status.Profiles) == 0 && shed.EgressPort != 0 && s.cfg.Egress != nil {
		status.Profiles = s.cfg.Egress.Default
	}
	if s.cfg.Egress != nil {
		status.Rules = map[string]config.EgressProfile{}
		for _, p := range status.Profiles {
			if def, ok := s.cfg.Egress.Profiles[p]; ok {
				status.Rules[p] = def // config wins on a name collision
			} else if def, ok := s.egressStore.Get(p); ok { // nil-safe; runtime user profile
				status.Rules[p] = def
			}
		}
	}
	if s.egressAudit != nil {
		status.Recent = s.egressAudit.Recent(name, 50)
	}
	writeJSON(w, http.StatusOK, status)
}

// egressController is the optional backend capability for live egress changes.
// The native vz/fc backends implement it; a backend that doesn't yields a 501.
type egressController interface {
	SetShedEgress(ctx context.Context, name string, profiles []string) (*config.Shed, error)
	ClearShedEgress(ctx context.Context, name string) (*config.Shed, error)
}

// handleEgressSet applies a profile selection to a shed (live on a running one).
// POST /api/egress/{name}  body: {"profiles":["base","github"]}
func (s *Server) handleEgressSet(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ec, ok := s.backend.(egressController)
	if !ok {
		writeError(w, http.StatusNotImplemented, config.ErrBackendError, "egress control is not supported by this backend")
		return
	}
	var req config.EgressSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "invalid request body")
		return
	}
	shed, err := ec.SetShedEgress(r.Context(), name, req.Profiles)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	writeJSON(w, http.StatusOK, shed)
}

// handleEgressStream streams egress decisions as Server-Sent Events for the
// host-agent subscriber (shed-extensions). GET /api/egress/stream
//
// In secure mode this route accepts a control OR credentials token (the
// host-agent holds credentials; a control token may tail it) — see
// authMiddleware. It is fleet-global: a subscriber sees every shed's decisions.
func (s *Server) handleEgressStream(w http.ResponseWriter, r *http.Request) {
	if s.egressAudit == nil {
		writeError(w, http.StatusNotImplemented, config.ErrBackendError, "egress control is not enabled")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, config.ErrBackendError, "streaming not supported")
		return
	}

	events := make(chan egress.AuditRecord, 256)
	unsub := s.egressAudit.Subscribe(func(rec egress.AuditRecord) {
		select {
		case events <- rec:
		default: // drop on overflow; never block the data path
		}
	})
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-events:
			writeSSEEvent(w, "egress", rec)
			flusher.Flush()
		}
	}
}

// handleEgressOff turns egress off for a shed.
// DELETE /api/egress/{name}
func (s *Server) handleEgressOff(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	ec, ok := s.backend.(egressController)
	if !ok {
		writeError(w, http.StatusNotImplemented, config.ErrBackendError, "egress control is not supported by this backend")
		return
	}
	shed, err := ec.ClearShedEgress(r.Context(), name)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	writeJSON(w, http.StatusOK, shed)
}

// handleDeleteShed deletes a shed. Delete always discards the writable volume;
// any stray ?keep_volume query is ignored for back-compat with old clients (it
// was a no-op on both backends).
// DELETE /api/sheds/{name}
func (s *Server) handleDeleteShed(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	// Drop the per-shed rc state (cached capabilities + hub breaker entry) up
	// front — the shed is being torn down, so it is stale regardless of which
	// teardown path (plain/SSE) runs and regardless of outcome (a failed delete
	// just re-probes on the next list).
	s.invalidateShedRC(name)

	// Stream teardown progress via SSE if the client requests it.
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.handleDeleteShedSSE(w, r, name)
		return
	}

	// Detach the teardown from client cancellation (as the SSE path does): a
	// non-streaming client or proxy must not be able to abort a destroy
	// mid-cleanup — the backend kills the VM then does multi-step host cleanup.
	timer := s.deleteTimer(name)
	ctx := backend.ContextWithProgress(context.WithoutCancel(r.Context()), timer.Track)
	err := s.backend.DeleteShed(ctx, name)
	log.Printf("timing: %s", timer.Finish(err))
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// deleteTimer returns a PhaseTimer labelled for a DeleteShed operation. Like
// createTimer it's installed for every delete (SSE or not) so the per-phase
// breakdown lands in the server log regardless of streaming.
func (s *Server) deleteTimer(name string) *backend.PhaseTimer {
	return backend.NewPhaseTimer(
		fmt.Sprintf("delete name=%s backend=%s", name, s.backend.Type()), nil)
}

// handleDeleteShedSSE streams delete/teardown progress as Server-Sent Events.
func (s *Server) handleDeleteShedSSE(w http.ResponseWriter, r *http.Request, name string) {
	// Detach the teardown from client cancellation: once delete starts
	// destroying resources, a client disconnect (or its request timeout) must
	// NOT strand it half-done — the destroy path may sync host-backed mounts
	// before SIGKILL, and cancelling that sync could lose host data. Every
	// internal step is self-bounded (bounded sync, SIGKILL wait, quick fs
	// removes), so the goroutine still completes without a deadline. (A ceiling
	// here would be worse than none: it would also cover DeleteShed's unbounded
	// lock-acquire wait and could expire before teardown even starts, skipping
	// the sync.) Progress still flows — the sink unblocks on the request's own
	// Done, so a disconnected client just stops receiving events.
	timer := s.deleteTimer(name)
	s.streamSSE(w, r, context.WithoutCancel(r.Context()), timer.Track, logTimingFinish(timer), func(ctx context.Context) (any, error) {
		// Delete has no resource to return; a benign payload keeps the terminal
		// event shape consistent with create/pull. The client ignores it.
		return map[string]string{"name": name}, s.backend.DeleteShed(ctx, name)
	})
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
	// A restart can pick up an in-place agent install; drop the stale per-shed rc
	// state (caps + hub breaker — a fresh boot deserves fresh start attempts).
	s.invalidateShedRC(name)

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
	// A stopped shed can't be re-probed; drop its per-shed rc state (caps + hub
	// breaker) so a later start re-probes fresh.
	s.invalidateShedRC(name)

	writeJSON(w, http.StatusOK, shed)
}

// handleResetShed resets a stopped shed by deleting and recreating its
// per-shed writable upper layer. Workspace contents (mounted post-boot
// from outside the overlay) are not affected.
// POST /api/sheds/{name}/reset
func (s *Server) handleResetShed(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	shed, err := s.backend.ResetShed(r.Context(), name)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	// Recreated upper layer: the cached caps and any hub-start breaker entry are
	// for the old rootfs state.
	s.invalidateShedRC(name)

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
	if rcEnrichEnabled(r) {
		resp.Warnings = s.enrichSessionsRC(r.Context(), sessions)
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

	if rcEnrichEnabled(r) {
		warnings = append(warnings, s.enrichSessionsRC(r.Context(), allSessions)...)
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
	if errors.Is(err, config.ErrShedNotStoppedSentinel) {
		return http.StatusConflict, config.ErrShedNotStopped, err.Error()
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
	if errors.Is(err, vmimage.ErrLayersMissing) {
		// Pushing a boot-only image: client-recoverable (re-pull
		// --with-layers). Preserve the actionable message.
		return http.StatusConflict, config.ErrBackendError, err.Error()
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

// imageIdent extracts the image identifier (a Docker ref, digest, or cosmetic
// tag) a single-image request targets. A Docker ref contains slashes that
// chi's {name} path param can't carry, so it rides in ?ref=; the {name} path
// param remains for slash-free identifiers from older clients.
func imageIdent(r *http.Request) string {
	if ref := r.URL.Query().Get("ref"); ref != "" {
		return ref
	}
	return chi.URLParam(r, "name")
}

// handleDeleteImage removes a cached image by ref, digest, or tag.
// DELETE /api/images?ref={ref} (preferred) or DELETE /api/images/{name} (legacy)
func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	name := imageIdent(r)
	if name == "" {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "missing image identifier (pass ?ref=)")
		return
	}

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

// handleInspectImage returns the manifest + info for a ref, tag, or digest.
// GET /api/images/inspect?ref={ref} (preferred) or GET /api/images/inspect/{name} (legacy)
func (s *Server) handleInspectImage(w http.ResponseWriter, r *http.Request) {
	name := imageIdent(r)
	if name == "" {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "missing image identifier (pass ?ref=)")
		return
	}
	resp, err := s.backend.InspectImage(r.Context(), name)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleTagImage points a new tag at the digest currently held by another
// tag (or a digest passed directly).
// POST /api/images/tag
func (s *Server) handleTagImage(w http.ResponseWriter, r *http.Request) {
	var req config.ImageTagRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Source == "" || req.Target == "" {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "source and target are required")
		return
	}
	if err := vmimage.ValidateImageName(req.Target); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "target: "+err.Error())
		return
	}
	if err := s.backend.TagImage(r.Context(), req.Source, req.Target); err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePullImage pulls a Docker reference into the blob store under the
// named tag. Returns the resulting digest.
// POST /api/images/pull
func (s *Server) handlePullImage(w http.ResponseWriter, r *http.Request) {
	var req config.ImagePullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.DockerRef == "" || req.Tag == "" {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "docker_ref and tag are required")
		return
	}
	if err := vmimage.ValidateImageName(req.Tag); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "tag: "+err.Error())
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.handlePullImageSSE(w, r, req)
		return
	}
	digest, err := s.backend.PullImage(r.Context(), req.DockerRef, req.Tag, req.Platform, req.WithLayers)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	writeJSON(w, http.StatusOK, config.ImagePullResponse{Tag: req.Tag, Digest: digest})
}

// handlePullImageSSE streams pull progress as Server-Sent Events. The backend
// already emits per-stage progress into the context via backend.Phase/Status
// (see internal/vz/image.go, internal/firecracker/image.go); this delegates the
// streaming to streamSSE with no timing tee/finalizer (nil, nil) — pull has no
// PhaseTimer and times itself, keeping its own digest+duration log lines.
func (s *Server) handlePullImageSSE(w http.ResponseWriter, r *http.Request, req config.ImagePullRequest) {
	s.streamSSE(w, r, r.Context(), nil, nil, func(ctx context.Context) (any, error) {
		start := time.Now()
		digest, err := s.backend.PullImage(ctx, req.DockerRef, req.Tag, req.Platform, req.WithLayers)
		if err != nil {
			log.Printf("PullImage failed for %q after %s: %v", req.DockerRef, time.Since(start).Round(time.Millisecond), err)
			return nil, err
		}
		log.Printf("PullImage %q -> %s in %s", req.DockerRef, vmimage.ShortDigest(digest), time.Since(start).Round(time.Millisecond))
		return config.ImagePullResponse{Tag: req.Tag, Digest: digest}, nil
	})
}

// handlePushImage uploads the manifest held by the named tag (or by a
// digest) to a destination registry ref, byte-perfect.
// POST /api/images/push
func (s *Server) handlePushImage(w http.ResponseWriter, r *http.Request) {
	var req config.ImagePushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Source == "" || req.Destination == "" {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "source and destination are required")
		return
	}
	if err := s.backend.PushImage(r.Context(), req.Source, req.Destination); err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	writeJSON(w, http.StatusOK, config.ImagePushResponse(req))
}
