package api

import (
	"net/http"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/version"
)

// Feature tokens advertised on GET /api/info and in the GET /api/overview server
// block for endpoint discovery. A client learns which endpoints and behaviors a
// server supports from this set without probing each one.
const (
	// FeatureOverview signals the GET /api/overview single-call host snapshot.
	FeatureOverview = "overview"
)

// serverFeatures returns the feature-token set as a fresh slice, so the
// advertised set lives in exactly one place yet no caller can mutate it through
// the returned value (an append can't alias a shared backing array).
func serverFeatures() []string {
	return []string{FeatureOverview}
}

// OverviewServer is the server block of GET /api/overview: the server's version
// and the feature-token set (mirrored from GET /api/info).
type OverviewServer struct {
	Version  string   `json:"version"`
	Features []string `json:"features"`
}

// OverviewShed is one shed in GET /api/overview: the full shed record (embedded,
// so its fields flatten into the object) plus the shed's tmux sessions. Stopped
// sheds carry an empty Sessions slice.
type OverviewShed struct {
	config.Shed
	Sessions []config.Session `json:"sessions"`
}

// OverviewResponse is the payload of GET /api/overview — a single call a client
// (phone/desktop) renders a whole host from: server identity + feature set, disk
// usage, and every shed with its sessions. Each sub-block degrades independently
// into Warnings rather than failing the whole call.
type OverviewResponse struct {
	Server   OverviewServer    `json:"server"`
	DF       *config.DiskUsage `json:"df,omitempty"`
	Sheds    []OverviewShed    `json:"sheds"`
	Warnings []string          `json:"warnings,omitempty"`
}

// handleOverview returns a single-call host snapshot: server identity + feature
// set, disk usage, and every shed with its sessions. Control scope, GET-only —
// the router registers only GET, and the auth middleware's default branch
// requires a control-scoped token (see authMiddleware; the #237/#239 dual-scope
// carve-outs deliberately do not apply here, so a credentials token is
// rejected).
//
// Every sub-block degrades independently: a df or session-list failure
// omits/empties that block and appends to `warnings` rather than failing the
// whole call. Only the top-level shed listing (ListSheds) is a hard 500 —
// without it there is nothing to render.
//
// GET /api/overview
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	resp := OverviewResponse{
		Server: OverviewServer{Version: version.Info(), Features: serverFeatures()},
		Sheds:  []OverviewShed{},
	}
	var warnings []string

	// df block (same shape as GET /api/system/df). A failure degrades to an
	// omitted block + a warning — never a 500.
	if usage, err := s.backend.DiskUsage(ctx); err != nil {
		warnings = append(warnings, "df unavailable: "+err.Error())
	} else {
		normalizeDiskUsage(&usage)
		resp.DF = &usage
	}

	sheds, err := s.backend.ListSheds(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, config.ErrBackendError, err.Error())
		return
	}

	// Gather every running shed's sessions into one flat slice; each shed's
	// [start,end) range is remembered so the rows slice back per shed (shared
	// backing array, order preserved). A per-shed session-list failure degrades
	// that shed to an empty Sessions slice + a warning.
	var allSessions []config.Session
	type span struct{ start, end int }
	spans := map[string]span{}
	for i := range sheds {
		if sheds[i].Status != config.StatusRunning {
			continue
		}
		sess, err := s.backend.ListSessions(ctx, sheds[i].Name)
		if err != nil {
			warnings = append(warnings, "shed "+sheds[i].Name+": sessions unavailable: "+err.Error())
			continue
		}
		start := len(allSessions)
		allSessions = append(allSessions, sess...)
		spans[sheds[i].Name] = span{start, len(allSessions)}
	}

	for i := range sheds {
		entry := OverviewShed{Shed: sheds[i], Sessions: []config.Session{}}
		if sheds[i].Status == config.StatusRunning {
			if sp, ok := spans[sheds[i].Name]; ok && sp.end > sp.start {
				entry.Sessions = allSessions[sp.start:sp.end]
			}
		}
		resp.Sheds = append(resp.Sheds, entry)
	}

	resp.Warnings = warnings
	writeJSON(w, http.StatusOK, resp)
}
