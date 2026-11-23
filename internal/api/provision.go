package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/charliek/shed/internal/backend"
)

// handleProvisionShed is `shed provision`: it asks the shed user's source for
// a bundle (reason provision) and runs stage 1, the clone, stage 2 and the
// install hook, each when owed or failed (the two stages always when a bundle
// arrived). With Accept: text/event-stream its progress is streamed as SSE,
// as create's is, and a failed delivery is a progress warning naming the
// command; either way the shed record it ends with is returned.
//
// The shed must be running, and the caller must be its own user (or the
// implicit user, for a shed with no user): the right to enter it is not
// enough (backend.AuthorizeProvision). Those refusals, and a provision
// already in flight (409 PROVISION_IN_PROGRESS), are answered before any
// stream is opened. The work is detached from the request: a client that
// goes away does not cut a step short.
//
// POST /api/sheds/{name}/provision
func (s *Server) handleProvisionShed(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	pv, ok := backend.AsProvisioner(s.backend)
	if !ok {
		s.writeFeatureUnsupported(w, http.StatusNotImplemented, "provision")
		return
	}
	// Admission first, before any stream opens: the guard's rules, the
	// shed's state and its one provisioning slot. A refusal is a plain
	// status; the slot, once taken, is held until the run is done.
	run, err := pv.BeginProvision(r.Context(), name)
	if err != nil {
		code, errCode, msg := backendErrorFor(r.Context(), err)
		writeError(w, code, errCode, msg)
		return
	}
	defer run.Release()

	timer := backend.NewPhaseTimer(fmt.Sprintf("provision name=%s backend=%s", name, s.backend.Type()), nil)
	work := func(ctx context.Context) (any, error) {
		shed, err := run.Run(ctx)
		if err != nil {
			log.Printf("ProvisionShed failed for %q (backend=%s): %v", name, s.backend.Type(), err)
		}
		return shed, err
	}
	base := context.WithoutCancel(r.Context())
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamSSE(w, r, base, timer.Track, logTimingFinish(timer), work)
		return
	}
	shed, err := work(backend.ContextWithProgress(base, timer.Track))
	log.Printf("timing: %s", timer.Finish(err))
	if err != nil {
		code, errCode, msg := backendErrorFor(r.Context(), err)
		writeError(w, code, errCode, msg)
		return
	}
	writeJSON(w, http.StatusOK, shed)
}
