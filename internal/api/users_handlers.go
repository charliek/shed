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
// account id, and gets one row per login; on a directory server, a pull of
// the directory's list, and its row (good or failed, both with 200). Only
// the request's own deadline is an error (504).
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
			what := "the GitHub fetch"
			if _, ok := s.keyRefresher.(UsersStatusReporter); ok {
				what = "the directory pull"
			}
			writeError(w, http.StatusGatewayTimeout, config.ErrInternalError,
				what+" is still running; its result applies when it ends")
			return
		}
		writeError(w, http.StatusInternalServerError, config.ErrInternalError, "refreshing keys: "+err.Error())
		return
	}
	// A directory server's pull is a row whether it was good or failed: a
	// failed pull is not an error of the request.
	if d := report.Directory; d != nil {
		resp.Directory = &config.DirectoryRefreshRow{
			Status:  d.Status,
			List:    d.List,
			Version: d.Version,
			Users:   d.Users,
			Skipped: d.Skipped,
			Error:   d.Error,
		}
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

// UsersStatusReporter is a user lookup whose users come from a source with a
// state of its own to report: a directory server's (auth.directory). GET
// /api/users/status asks the lookup for it. The lookups of a server with no
// user list or with auth.users do not implement it; the route answers for
// them from the lookup itself.
type UsersStatusReporter interface {
	// UsersStatus is the source's state now: Source directory, its
	// Directory block (without ExpiredLocalAdmins, which the route adds
	// from the server's config) and its users. It holds no key or label.
	UsersStatus() config.UsersStatusResponse
}

// handleUsersStatus is GET /api/users/status (`shed users status`): an admin
// sees where the server's users come from, who they are (name, admin flag
// and source, never a key), and on a directory server the state of its list:
// its state and why, the last good pull, the trust time, the last attempt
// and its error, the next attempt, the kept copy, and the entries skipped.
//
//	403 FORBIDDEN  the caller is not an admin, or uses a script key
//
// A script key is automation, not a person: like the credential source
// routes, this one is refused to it even when its user is an admin.
func (s *Server) handleUsersStatus(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r, "the status of the server's users") {
		return
	}
	if callerFrom(r.Context()).Script {
		writeError(w, http.StatusForbidden, config.ErrForbidden, "a script key cannot read the status of the server's users")
		return
	}
	writeJSON(w, http.StatusOK, s.usersStatus(time.Now()))
}

// usersStatus is what GET /api/users/status answers at now.
func (s *Server) usersStatus(now time.Time) config.UsersStatusResponse {
	if rep, ok := s.users.(UsersStatusReporter); ok {
		st := rep.UsersStatus()
		if st.Users == nil {
			st.Users = []config.UserStatus{}
		}
		if st.Directory != nil {
			if st.Directory.Skipped == nil {
				st.Directory.Skipped = []config.SkippedEntry{}
			}
			if s.cfg != nil {
				// By name only: no label, key or fingerprint reaches the
				// answer.
				st.Directory.ExpiredLocalAdmins = s.cfg.UsersWithEveryKeyExpired(now)
			}
		}
		return st
	}
	if s.users == nil || !users.HasUserList(s.users) {
		return config.UsersStatusResponse{Source: config.UsersSourceNone, Users: []config.UserStatus{}}
	}
	out := config.UsersStatusResponse{Source: config.UsersSourceConfig, Users: []config.UserStatus{}}
	for _, u := range s.users.Users() {
		if u.Implicit {
			continue
		}
		out.Users = append(out.Users, config.UserStatus{Name: u.Name, Admin: u.Admin, Source: config.UserFromConfig})
	}
	return out
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
		writeBackendError(w, r.Context(), err)
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
