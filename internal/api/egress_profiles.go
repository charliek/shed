package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/go-chi/chi/v5"
)

const (
	egressSourceConfig = "config"
	egressSourceUser   = "user"
)

// rePushTimeout bounds the live re-push fan-out (independent of the request ctx).
const rePushTimeout = 60 * time.Second

// egressProfilesEnabled reports whether egress + the user-profile store are active.
func (s *Server) egressProfilesEnabled() bool {
	return s.cfg.Egress != nil && s.cfg.Egress.Enabled && s.egressStore != nil
}

// requireEgressProfiles writes a 501 and returns false when egress / the profile
// store is not active; the profile handlers early-return on false.
func (s *Server) requireEgressProfiles(w http.ResponseWriter) bool {
	if s.egressProfilesEnabled() {
		return true
	}
	writeError(w, http.StatusNotImplemented, config.ErrBackendError, "egress control is not enabled")
	return false
}

// isConfigProfile reports whether name is a read-only server-config profile.
func (s *Server) isConfigProfile(name string) bool {
	_, ok := s.cfg.Egress.Profiles[name]
	return ok
}

// shedReferencesProfile reports whether sh's egress profile list includes name.
func shedReferencesProfile(sh config.Shed, name string) bool {
	for _, p := range sh.EgressProfiles {
		if p == name {
			return true
		}
	}
	return false
}

// handleListProfiles returns every egress profile — the server-config baseline
// plus the runtime user store — each tagged with its source. Config wins on a
// name collision. GET /api/egress/profiles
func (s *Server) handleListProfiles(w http.ResponseWriter, r *http.Request) {
	if !s.requireEgressProfiles(w) {
		return
	}
	byName := map[string]config.EgressProfileInfo{}
	for name, p := range s.egressStore.List() {
		byName[name] = config.EgressProfileInfo{Name: name, Source: egressSourceUser, Profile: p}
	}
	for name, p := range s.cfg.Egress.Profiles {
		byName[name] = config.EgressProfileInfo{Name: name, Source: egressSourceConfig, Profile: p} // config wins
	}
	infos := make([]config.EgressProfileInfo, 0, len(byName))
	for _, info := range byName {
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	writeJSON(w, http.StatusOK, infos)
}

// handleGetProfile returns one profile (config wins on collision).
// GET /api/egress/profiles/{name}
func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireEgressProfiles(w) {
		return
	}
	name := chi.URLParam(r, "name")
	if p, ok := s.cfg.Egress.Profiles[name]; ok {
		writeJSON(w, http.StatusOK, config.EgressProfileInfo{Name: name, Source: egressSourceConfig, Profile: p})
		return
	}
	if p, ok := s.egressStore.Get(name); ok {
		writeJSON(w, http.StatusOK, config.EgressProfileInfo{Name: name, Source: egressSourceUser, Profile: p})
		return
	}
	writeError(w, http.StatusNotFound, config.ErrProfileNotFound, fmt.Sprintf("egress profile %q not found", name))
}

// handlePutProfile creates or replaces a user profile (whole document), then
// live-re-pushes running sheds that reference it. PUT /api/egress/profiles/{name}
func (s *Server) handlePutProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireEgressProfiles(w) {
		return
	}
	name := chi.URLParam(r, "name")
	// Server-config profiles are a read-only baseline — a user PUT must not shadow one.
	if s.isConfigProfile(name) {
		writeError(w, http.StatusConflict, config.ErrProfileReserved, fmt.Sprintf("%q is a server-config profile (read-only); choose another name", name))
		return
	}
	if config.IsReservedEgressName(name) {
		writeError(w, http.StatusConflict, config.ErrProfileReserved, fmt.Sprintf("%q is a reserved name", name))
		return
	}
	var p config.EgressProfile
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "invalid request body")
		return
	}
	if err := s.egressStore.Put(name, p); err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, err.Error())
		return
	}
	if warnings := s.rePushProfile(name); len(warnings) > 0 {
		log.Printf("egress: profile %q saved with %d live re-push warning(s): %s", name, len(warnings), strings.Join(warnings, "; "))
	}
	log.Printf("egress: profile %q saved (user)", name)
	stored, _ := s.egressStore.Get(name)
	writeJSON(w, http.StatusOK, config.EgressProfileInfo{Name: name, Source: egressSourceUser, Profile: stored})
}

// handleDeleteProfile removes a user profile, rejecting the delete if any shed
// still references it. DELETE /api/egress/profiles/{name}
func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireEgressProfiles(w) {
		return
	}
	name := chi.URLParam(r, "name")
	if s.isConfigProfile(name) {
		writeError(w, http.StatusConflict, config.ErrProfileReserved, fmt.Sprintf("%q is a server-config profile (read-only)", name))
		return
	}
	if _, ok := s.egressStore.Get(name); !ok {
		writeError(w, http.StatusNotFound, config.ErrProfileNotFound, fmt.Sprintf("egress profile %q not found", name))
		return
	}
	refs, err := s.profileReferencedBy(r.Context(), name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, config.ErrInternalError, err.Error())
		return
	}
	if len(refs) > 0 {
		writeError(w, http.StatusConflict, config.ErrProfileInUse, "egress profile in use by sheds: "+strings.Join(refs, ", "))
		return
	}
	if err := s.egressStore.Delete(name); err != nil {
		// Existence was checked above, so an error here is a storage failure.
		writeError(w, http.StatusInternalServerError, config.ErrInternalError, err.Error())
		return
	}
	log.Printf("egress: profile %q deleted", name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "profile": name})
}

// profileReferencedBy returns the sorted names of sheds whose egress profile list
// includes name (running OR stopped — a stopped shed would fail to resolve on
// next start if its profile vanished).
func (s *Server) profileReferencedBy(ctx context.Context, name string) ([]string, error) {
	sheds, err := s.backend.ListSheds(ctx)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, sh := range sheds {
		if shedReferencesProfile(sh, name) {
			refs = append(refs, sh.Name)
		}
	}
	sort.Strings(refs)
	return refs, nil
}

// rePushProfile re-applies egress to every RUNNING shed referencing name so a
// profile edit takes effect live. Best-effort: the profile is already persisted,
// so a per-shed failure is logged and collected (returned as warnings), never
// failing the caller. Runs on a bounded internal context (NOT the request ctx)
// so a client disconnect can't leave running sheds stale. Stopped sheds
// re-resolve on next start, so they need no fan-out.
func (s *Server) rePushProfile(name string) []string {
	ec, ok := s.backend.(egressController)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), rePushTimeout)
	defer cancel()
	sheds, err := s.backend.ListSheds(ctx)
	if err != nil {
		log.Printf("egress: re-push %q: list sheds: %v", name, err)
		return []string{fmt.Sprintf("list sheds: %v", err)}
	}
	var warnings []string
	for _, sh := range sheds {
		if sh.Status != config.StatusRunning || !shedReferencesProfile(sh, name) {
			continue
		}
		if _, err := ec.SetShedEgress(ctx, sh.Name, sh.EgressProfiles); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", sh.Name, err))
		}
	}
	return warnings
}
