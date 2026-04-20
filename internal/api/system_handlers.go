package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// handleSystemDF returns disk usage information for this server.
// GET /api/system/df
func (s *Server) handleSystemDF(w http.ResponseWriter, r *http.Request) {
	usage, err := s.backend.DiskUsage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, config.ErrBackendError, err.Error())
		return
	}
	// Defensive: ensure slices are non-nil so JSON renders `[]` not `null`.
	if usage.Images == nil {
		usage.Images = []config.ImageDiskEntry{}
	}
	if usage.Sheds == nil {
		usage.Sheds = []config.ShedDiskEntry{}
	}
	if usage.Orphans == nil {
		usage.Orphans = []config.FileEntry{}
	}
	writeJSON(w, http.StatusOK, usage)
}

// handleSystemPrune runs a disk cleanup pass. Scope flags are passed as
// repeated `scope=` query params (any of images, instances, logs, orphans).
// When no scope is specified, defaults to images+instances+orphans (NOT
// logs, which is always opt-in).
//
// POST /api/system/prune?scope=...&dry_run=bool&until=72h&log_tail_bytes=N
func (s *Server) handleSystemPrune(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var opts backend.PruneOptions

	// dry_run
	if v := q.Get("dry_run"); v != "" {
		dryRun, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, config.ErrBackendError, "invalid dry_run value: "+v)
			return
		}
		opts.DryRun = dryRun
	}

	// scope=... (repeatable). Unknown values are rejected so typos don't
	// silently fall through to the default scope.
	for _, s := range q["scope"] {
		switch s {
		case "images":
			opts.Images = true
		case "instances":
			opts.Instances = true
		case "logs":
			opts.Logs = true
		case "orphans":
			opts.Orphans = true
		default:
			writeError(w, http.StatusBadRequest, config.ErrBackendError, "unknown scope: "+s)
			return
		}
	}

	// until
	if v := q.Get("until"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, config.ErrBackendError, "invalid until value: "+v)
			return
		}
		if d < 0 {
			writeError(w, http.StatusBadRequest, config.ErrBackendError, "until must be non-negative")
			return
		}
		opts.Until = d
	}

	// log_tail_bytes
	if v := q.Get("log_tail_bytes"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, config.ErrBackendError, "invalid log_tail_bytes: "+v)
			return
		}
		opts.LogTailBytes = n
	}

	report, err := s.backend.Prune(r.Context(), opts)
	if err != nil {
		code, errCode, msg := mapBackendError(err)
		writeError(w, code, errCode, msg)
		return
	}
	if report.Items == nil {
		report.Items = []config.PrunedItem{}
	}
	if report.Skipped == nil {
		report.Skipped = []config.SkippedItem{}
	}
	writeJSON(w, http.StatusOK, report)
}
