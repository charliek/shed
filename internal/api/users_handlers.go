package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/users"
)

// usersRefreshTimeout bounds how long POST /api/users/refresh waits for the
// fetch. A fetch is three requests per login with a 15 s timeout each, and
// logins are fetched concurrently; a fetch still running when this ends
// carries on and lands in the live key set.
const usersRefreshTimeout = 60 * time.Second

// SetKeyRefresher attaches what POST /api/users/refresh asks to fetch the
// GitHub logins again. In shed-server it is the same users.ConfigLookup the
// SSH server and the mtls middleware ask, in every auth mode. nil: the route
// answers with no rows.
func (s *Server) SetKeyRefresher(r users.Refresher) { s.keyRefresher = r }

// handleUsersRefresh is POST /api/users/refresh (`shed users refresh`): an
// admin forces a fetch of every GitHub login, checked against its pinned
// account id, and gets one row per login.
func (s *Server) handleUsersRefresh(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "refreshing user keys") {
		return
	}
	resp := config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{}}
	if s.keyRefresher == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), usersRefreshTimeout)
	defer cancel()
	report, err := s.keyRefresher.Refresh(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, config.ErrInternalError,
				"the GitHub fetch is still running; its result applies when it ends")
			return
		}
		writeError(w, http.StatusInternalServerError, config.ErrInternalError, "refreshing keys: "+err.Error())
		return
	}
	for _, row := range report.Logins {
		resp.Logins = append(resp.Logins, config.GitHubLoginRefresh{
			User:     row.User,
			Login:    row.Login,
			Status:   row.Status,
			GitHubID: row.PinnedID,
			SeenID:   row.SeenID,
			Keys:     row.Keys,
			Added:    row.Added,
			Removed:  row.Removed,
			Error:    row.Error,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}
