package api

import (
	"context"
	"net/http"
	"sync"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/ext/rc"
	"github.com/charliek/shed/internal/version"
	"golang.org/x/sync/errgroup"
)

// Feature tokens advertised on GET /api/info and in the GET /api/overview server
// block for endpoint discovery. A client learns which endpoints and behaviors a
// server supports from this set without probing each one.
const (
	// FeatureOverview signals the GET /api/overview single-call host snapshot.
	FeatureOverview = "overview"
	// FeatureRCEnrich signals server-side rc enrichment of session listings
	// (Session.RC populated on GET /api/sessions and the per-shed listing).
	FeatureRCEnrich = "rc-enrich"
	// FeatureRCEvents signals the GET /api/rc/events aggregate activity SSE stream.
	FeatureRCEvents = "rc-events"
	// FeatureRCProxy signals the GET/POST /api/sheds/{name}/rc/* hub reverse proxy.
	FeatureRCProxy = "rc-proxy"
)

// serverFeatures returns the feature-token set as a fresh slice, so the
// advertised set lives in exactly one place yet no caller can mutate it through
// the returned value (an append can't alias a shared backing array).
func serverFeatures() []string {
	return []string{FeatureOverview, FeatureRCEnrich, FeatureRCEvents, FeatureRCProxy}
}

// OverviewServer is the server block of GET /api/overview: the server's version
// and the feature-token set (mirrored from GET /api/info).
type OverviewServer struct {
	Version  string   `json:"version"`
	Features []string `json:"features"`
}

// OverviewShed is one shed in GET /api/overview: the full shed record (embedded,
// so its fields flatten into the object) plus the shed's tmux sessions and, for
// running sheds, its rc capabilities. Running sheds carry their sessions
// (rc-enriched unless ?rc=0); stopped sheds carry an empty Sessions slice and
// omit RCCapabilities.
type OverviewShed struct {
	config.Shed
	Sessions       []config.Session `json:"sessions"`
	RCCapabilities *rc.Capabilities `json:"rc_capabilities,omitempty"`
}

// OverviewResponse is the payload of GET /api/overview — a single call a client
// (phone/desktop) renders a whole host from: server identity + feature set, disk
// usage, and every shed with its sessions and capabilities. Each sub-block
// degrades independently into Warnings rather than failing the whole call.
type OverviewResponse struct {
	Server   OverviewServer    `json:"server"`
	DF       *config.DiskUsage `json:"df,omitempty"`
	Sheds    []OverviewShed    `json:"sheds"`
	Warnings []string          `json:"warnings,omitempty"`
}

// handleOverview returns a single-call host snapshot: server identity + feature
// set, disk usage, and every shed with its (rc-enriched) sessions and rc
// capabilities. Control scope, GET-only — the router registers only GET, and the
// auth middleware's default branch requires a control-scoped token (see
// authMiddleware; the #237/#239 dual-scope carve-outs deliberately do not apply
// here, so a credentials token is rejected).
//
// Every sub-block degrades independently: a df, session-list, enrichment, or
// capabilities failure omits/empties that block and appends to `warnings` rather
// than failing the whole call. Only the top-level shed listing (ListSheds) is a
// hard 500 — without it there is nothing to render.
//
// GET /api/overview[?rc=0][&fresh=1]
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	enrich := rcEnrichEnabled(r) // ?rc=0 opts out of all guest execs
	fresh := r.URL.Query().Get("fresh") == "1"

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

	// Gather every running shed's sessions into one flat slice so enrichment runs
	// once with the shared concurrency cap; each shed's [start,end) range is
	// remembered so the enriched rows slice back per shed (shared backing array,
	// order preserved). A per-shed session-list failure degrades that shed to an
	// empty Sessions slice + a warning.
	var allSessions []config.Session
	type span struct{ start, end int }
	spans := map[string]span{}
	var runningNames []string
	for i := range sheds {
		if sheds[i].Status != config.StatusRunning {
			continue
		}
		runningNames = append(runningNames, sheds[i].Name)
		sess, err := s.backend.ListSessions(ctx, sheds[i].Name)
		if err != nil {
			warnings = append(warnings, "shed "+sheds[i].Name+": sessions unavailable: "+err.Error())
			continue
		}
		start := len(allSessions)
		allSessions = append(allSessions, sess...)
		spans[sheds[i].Name] = span{start, len(allSessions)}
	}

	// Under enrichment (the default; ?rc=0 opts out of all guest execs) do both
	// guest-facing passes. First enrich the flat slice once, reusing the
	// session-listing machinery (same concurrency cap, per-shed exec timeout,
	// degrade-to-warning behavior); this also caches each rc-bearing shed's caps.
	// Then probe capabilities per running shed (cache-served within TTL, so a shed
	// already enriched above needs no second exec; ?fresh=1 forces a re-probe).
	var caps map[string]*rc.Capabilities
	if enrich {
		warnings = append(warnings, s.enrichSessionsRC(ctx, allSessions)...)
		var capWarn []string
		caps, capWarn = s.overviewCapabilities(ctx, runningNames, fresh)
		warnings = append(warnings, capWarn...)
	}

	for i := range sheds {
		entry := OverviewShed{Shed: sheds[i], Sessions: []config.Session{}}
		if sheds[i].Status == config.StatusRunning {
			if sp, ok := spans[sheds[i].Name]; ok && sp.end > sp.start {
				entry.Sessions = allSessions[sp.start:sp.end]
			}
			if c := caps[sheds[i].Name]; c != nil {
				entry.RCCapabilities = c
			}
		}
		resp.Sheds = append(resp.Sheds, entry)
	}

	resp.Warnings = warnings
	writeJSON(w, http.StatusOK, resp)
}

// overviewCapabilities probes each running shed's rc capabilities concurrently
// (bounded by rcEnrichConcurrency), serving from the per-shed cache within TTL
// unless fresh forces a re-probe. A shed that can't be probed (no rc binary, exec
// failure) is omitted from the map and contributes a warning — degrade, not fail.
// A shed whose binary is too old to advertise capabilities returns (nil, nil) and
// is simply omitted (no warning).
func (s *Server) overviewCapabilities(ctx context.Context, shedNames []string, fresh bool) (map[string]*rc.Capabilities, []string) {
	var (
		mu       sync.Mutex
		caps     = map[string]*rc.Capabilities{}
		warnings []string
		g        errgroup.Group
	)
	g.SetLimit(rcEnrichConcurrency)
	for _, name := range shedNames {
		g.Go(func() error {
			c, err := s.RCCapabilities(ctx, name, fresh)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				warnings = append(warnings, "shed "+name+": RC capabilities unavailable: "+err.Error())
				return nil
			}
			if c != nil {
				caps[name] = c
			}
			return nil
		})
	}
	// Per-shed failures are captured as warnings, so Wait's error is always nil.
	_ = g.Wait()
	return caps, warnings
}
