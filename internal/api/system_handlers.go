package api

import (
	"net/http"

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
