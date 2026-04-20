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

// defaultPruneUntil is applied when the `until` query param is omitted.
// Explicit `until=0s` preserves its "any age" meaning for power users who
// genuinely want to prune every stopped instance regardless of age.
const defaultPruneUntil = 72 * time.Hour

// maxLogTailBytes caps the server-side allocation for console log tail
// preservation. Without this, a request like log_tail_bytes=20GB on a 20GB
// console.log would allocate a 20GB buffer and OOM the daemon.
const maxLogTailBytes = 64 * 1024 * 1024 // 64 MiB

// handleSystemPrune runs a disk cleanup pass. Scope flags are passed as
// repeated `scope=` query params (any of images, instances, logs, orphans).
// When no scope is specified, defaults to images+instances+orphans (NOT
// logs, which is always opt-in).
//
// POST /api/system/prune?scope=...&dry_run=bool&until=72h&log_tail_bytes=N
func (s *Server) handleSystemPrune(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var opts backend.PruneOptions

	if v := q.Get("dry_run"); v != "" {
		dryRun, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "invalid dry_run value: "+v)
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
			writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "unknown scope: "+s)
			return
		}
	}

	// `until` param: present-and-parseable wins; absent falls back to the
	// 72h default. An explicit until=0s is valid and means "any age."
	//
	// Distinguishing "omitted" from "explicit 0" requires the map lookup
	// rather than q.Get(), since an empty-string value is not the same as
	// a missing key for our purposes.
	if vals, ok := q["until"]; ok && len(vals) > 0 && vals[0] != "" {
		d, err := time.ParseDuration(vals[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "invalid until value: "+vals[0])
			return
		}
		if d < 0 {
			writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "until must be non-negative")
			return
		}
		opts.Until = d
	} else {
		opts.Until = defaultPruneUntil
	}

	if v := q.Get("log_tail_bytes"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "invalid log_tail_bytes: "+v)
			return
		}
		if n > maxLogTailBytes {
			writeError(w, http.StatusBadRequest, config.ErrInvalidRequest,
				"log_tail_bytes exceeds server cap of "+strconv.FormatInt(maxLogTailBytes, 10))
			return
		}
		opts.LogTailBytes = n
	}

	report, err := s.backend.Prune(r.Context(), opts)
	if err != nil {
		// Surface partial progress even on error: the backend may have
		// deleted instances before an image-prune step failed, and the
		// client needs visibility into what actually happened.
		if report.Totals.Items > 0 {
			if report.Items == nil {
				report.Items = []config.PrunedItem{}
			}
			if report.Skipped == nil {
				report.Skipped = []config.SkippedItem{}
			}
			report.Notes = append(report.Notes, "partial failure: "+err.Error())
			writeJSON(w, http.StatusOK, report)
			return
		}
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
