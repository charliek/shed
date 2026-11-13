package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// This file holds what the API does differently for a backend that reports
// less than every feature (backend.CapabilityReporter) or runs its lifecycle
// in server-owned jobs (backend.AsyncLifecycle). Firecracker and VZ implement
// neither, so none of these paths is taken for them.

// FeatureAsyncLifecycle signals that create, start, stop and delete return
// before the work is done: a JSON create answers 201 with status "starting",
// and a client polls GET /api/sheds/{name} until the shed is running.
const FeatureAsyncLifecycle = "async_lifecycle"

// createFieldRefusal names the first create field the backend cannot honour,
// or returns "" when it can honour them all. It is the M6 gate: it runs after
// the client policy check and before anything reads the host's disk or calls
// the backend.
func createFieldRefusal(caps backend.Capabilities, req *config.CreateShedRequest) string {
	if !caps.Mounts {
		switch {
		case req.LocalDir != "":
			return "local_dir"
		case len(req.AddDirs) > 0:
			return "add_dirs"
		case req.Mounts != nil && len(*req.Mounts) > 0:
			return "mounts"
		}
	}
	if !caps.Snapshots && req.FromSnapshot != "" {
		return "from_snapshot"
	}
	if !caps.EgressProfiles && len(req.Egress) > 0 && !(len(req.Egress) == 1 && req.Egress[0] == "off") {
		return "egress"
	}
	return ""
}

// capabilities returns what the backend offers. A server with no backend
// (handler tests that never reach one) offers every feature, so the gates
// below never fire for it.
func (s *Server) capabilities() backend.Capabilities {
	if s.backend == nil {
		return backend.Capabilities{Mounts: true, Snapshots: true, Reset: true, ImageManagement: true, EgressProfiles: true}
	}
	return backend.CapabilitiesOf(s.backend)
}

// writeFeatureUnsupported writes FEATURE_UNSUPPORTED with the given status
// (422 for a create field, 501 for a route).
func (s *Server) writeFeatureUnsupported(w http.ResponseWriter, status int, what string) {
	writeError(w, status, config.ErrFeatureUnsupported,
		fmt.Sprintf("%s is not supported by the %s backend", what, s.backend.Type()))
}

// requireCapability returns route middleware that answers 501
// FEATURE_UNSUPPORTED, before the handler runs, when the backend lacks the
// feature has reads from its capabilities.
func (s *Server) requireCapability(what string, has func(backend.Capabilities) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !has(s.capabilities()) {
				s.writeFeatureUnsupported(w, http.StatusNotImplemented, what)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func capSnapshots(c backend.Capabilities) bool       { return c.Snapshots }
func capReset(c backend.Capabilities) bool           { return c.Reset }
func capImageManagement(c backend.Capabilities) bool { return c.ImageManagement }
func capEgressProfiles(c backend.Capabilities) bool  { return c.EgressProfiles }

// errSettledNotRunning is returned for a create whose job settled in a state
// other than running.
var errSettledNotRunning = errors.New("shed did not reach running")

// settledError turns the shed a create's job settled on into the error the
// SSE stream reports, or nil when the shed is running. A refusal the job met
// keeps its 503 code (capacity, quota, limit) so a client can tell it apart
// from a failure.
func settledError(shed *config.Shed) error {
	if shed == nil {
		return fmt.Errorf("%w: no shed returned", errSettledNotRunning)
	}
	if shed.Status == config.StatusRunning {
		return nil
	}
	msg := fmt.Sprintf("shed %s is %s", shed.Name, shed.Status)
	if shed.StatusReason != "" {
		msg += " (" + shed.StatusReason + ")"
	}
	switch shed.StatusReason {
	case config.StatusReasonCapacity:
		return fmt.Errorf("%w: %s", config.ErrZoneCapacityUnavailableSentinel, msg)
	case config.StatusReasonQuota:
		return fmt.Errorf("%w: %s", config.ErrCloudQuotaExceededSentinel, msg)
	case config.StatusReasonLimit:
		return fmt.Errorf("%w: %s", config.ErrServerLimitReachedSentinel, msg)
	}
	return fmt.Errorf("%w: %s", errSettledNotRunning, msg)
}

// createAndWait is the SSE create on an AsyncLifecycle backend: it starts the
// job on a context the request cannot cancel and with no progress sink (the
// job's progress reaches the stream through WaitSettled's replay, so wiring
// the sink into the job as well would print every line twice), then waits on
// ctx, which the request's disconnect ends without touching the job.
func (s *Server) createAndWait(ctx context.Context, jobCtx context.Context, al backend.AsyncLifecycle, req config.CreateShedRequest) (any, error) {
	if _, err := s.backend.CreateShed(jobCtx, req); err != nil {
		return nil, err
	}
	shed, err := al.WaitSettled(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if err := settledError(shed); err != nil {
		return nil, err
	}
	return shed, nil
}

// deleteAndWait is the SSE delete on an AsyncLifecycle backend: the delete
// returns once the record is hidden, then the stream waits for the job to
// destroy the VM. WaitSettled reports the finished delete as
// ErrShedNotFoundSentinel, which here means success.
func (s *Server) deleteAndWait(ctx context.Context, jobCtx context.Context, al backend.AsyncLifecycle, name string) error {
	if err := s.backend.DeleteShed(jobCtx, name); err != nil {
		return err
	}
	if _, err := al.WaitSettled(ctx, name); err != nil && !errors.Is(err, config.ErrShedNotFoundSentinel) {
		return err
	}
	return nil
}
