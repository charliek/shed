package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

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

// assignBodyLimit bounds the body of POST /api/sheds/{name}/assign.
const assignBodyLimit = 4 << 10

// handleAssignShed is POST /api/sheds/{name}/assign (`shed assign <shed>
// <user>`): an admin gives a shed with no user and no service client to a
// configured user. Body {"user":"<name>"}; the answer is the shed.
//
//	403 FORBIDDEN              the caller is not an admin
//	400 INVALID_REQUEST        the body, or no such configured user
//	404 SHED_NOT_FOUND         no such shed (or it was deleted and created
//	                           again since it was looked up)
//	409 SHED_ALREADY_ASSIGNED  the shed has a user or a service client
//	409 SHED_HOST_RESOURCES    the shed carries host directories or server
//	                           mounts: recreate it without them
//
// The admin check comes first, so a caller who is not one learns nothing of
// the user list or the shed. (Only a name that cannot be a shed is answered
// before it, 404 SHED_NOT_FOUND by the router's requireShedName: no shed has
// such a name, so that tells nobody anything.) The guard checks it again and
// binds the shed's incarnation; the backend decides the rest under its lock.
func (s *Server) handleAssignShed(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "assigning a shed to a user") {
		return
	}
	name := chi.URLParam(r, "name")
	var req config.AssignShedRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, assignBodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, `the body must be {"user":"<name>"}: `+err.Error())
		return
	}
	if !s.configuredUser(req.User) {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest,
			fmt.Sprintf("no user named %q is configured on this server (auth.users)", req.User))
		return
	}
	shed, err := s.backend.AssignUser(r.Context(), name, config.AssignShedRequest{User: req.User})
	if err != nil {
		code, errCode, msg := backendErrorFor(r.Context(), err)
		writeError(w, code, errCode, msg)
		return
	}
	writeJSON(w, http.StatusOK, shed)
}

// configuredUser reports whether name is a user of this server's user list.
// The implicit user of a server with no list is not one: there is nobody to
// assign a shed to.
func (s *Server) configuredUser(name string) bool {
	if s.users == nil || !config.ValidUserName(name) {
		return false
	}
	u, ok := s.users.User(name)
	return ok && !u.Implicit && u.Name == name
}
