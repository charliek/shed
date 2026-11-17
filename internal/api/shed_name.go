package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/charliek/shed/internal/config"
)

// requireShedName is the one check of a shed name taken from a request path:
// the router installs it on every route of the subtrees whose {name} is a
// shed (/api/sheds/{name}/... and /api/egress/{name}), so it runs before the
// route's own checks and handler and before anything asks the backend. It is
// installed with With rather than Use, so it runs only once chi has matched
// a route: an unrouted method or path below {name} answers as it did.
//
// A name that is not a valid shed name (config.ValidateShedName) is answered
// as an unknown shed is on these routes, 404 SHED_NOT_FOUND "shed not found:
// <name>", and never reaches a backend. That answer comes before the route's
// own refusals, so for such a name a caller sees 404 where an unknown valid
// name would have drawn the route's 403 (assign by a non-admin), 501 (a
// capability the backend lacks) or 400 (a bad body, port or session name).
// Whether a name is well formed is public, so this says nothing about any
// shed. Without it, a name the rule
// refuses could still find a shed: on a case-insensitive volume (the VZ
// backend on macOS) /api/sheds/Git finds the shed stored as git, and records,
// locks, the credential bus and audit lines would key that one shed under a
// second spelling, so two operations on one VM need not exclude each other.
// The SSH door applies the same rule (internal/sshd's shedNameFor), and a
// service client's {name} is refused by the same rule in clientOwnsShed
// before this runs.
//
// It is ValidateShedName and not the create-only ValidateNewShedName on
// purpose: a shed named git made before that name was reserved must stay
// listable, stoppable, snapshottable and deletable.
//
// sseDelete is set on the /api/sheds/{name} subtree, whose DELETE streams its
// teardown when asked for text/event-stream: an unknown shed is reported
// there as a 200 stream whose only event is the error, and so is this.
func requireShedName(sseDelete bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := chi.URLParam(r, "name")
			if config.ValidateShedName(name) == nil {
				next.ServeHTTP(w, r)
				return
			}
			apiErr := config.NewAPIError(config.ErrShedNotFound, "shed not found: "+name)
			if sseDelete && deletesShedItself(r) && strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
				// As streamSSE does: no stream without a flusher.
				f, ok := w.(http.Flusher)
				if !ok {
					writeError(w, http.StatusInternalServerError, config.ErrBackendError, "streaming not supported")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.WriteHeader(http.StatusOK)
				writeSSEEvent(w, "error", apiErr)
				f.Flush()
				return
			}
			writeJSON(w, http.StatusNotFound, apiErr)
		})
	}
}

// deletesShedItself reports whether r, seen by a middleware of the {name}
// subrouter, is a DELETE of the {name} resource itself rather than of
// something under it (a session). chi has set RoutePath to what is left of
// the path below {name}.
func deletesShedItself(r *http.Request) bool {
	if r.Method != http.MethodDelete {
		return false
	}
	rctx := chi.RouteContext(r.Context())
	return rctx == nil || rctx.RoutePath == "" || rctx.RoutePath == "/"
}
