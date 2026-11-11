package api

import (
	"crypto/x509"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// clientAllowedRoutes is the whole surface a service client may reach, as
// "METHOD pattern" with chi's route pattern normalized by normalizeRoutePattern.
// Everything else is 403 FORBIDDEN (deny by default). The patterns with {name}
// additionally require that the client owns the shed (else 404).
//
// client_routes_test.go walks Router() and fails when a registered route is
// neither here nor on its explicit forbidden list, so a new route cannot
// become reachable by a client by accident.
var clientAllowedRoutes = map[string]bool{
	"GET /api/info":                true,
	"GET /api/images":              true,
	"GET /api/sheds":               true,
	"POST /api/sheds":              true,
	"GET /api/sheds/{name}":        true,
	"DELETE /api/sheds/{name}":     true,
	"POST /api/sheds/{name}/start": true,
	"POST /api/sheds/{name}/stop":  true,
	"POST /api/sheds/{name}/exec":  true,
}

// normalizeRoutePattern drops a trailing "/" from a chi route pattern (but
// not from "/"). chi serves "/api/sheds" and "/api/sheds/" with the same
// handler (a Route's Get("/")), and Find reports whichever form the request
// used, so the allowlist compares the normalized form.
func normalizeRoutePattern(p string) string {
	if len(p) > 1 {
		return strings.TrimSuffix(p, "/")
	}
	return p
}

// routePath is the path chi routes on: the raw (still-escaped) path when the
// URL has one, as chi's own routeHTTP does, so the pattern and the {name}
// param resolved here are the ones the handler will see.
func routePath(r *http.Request) string {
	p := r.URL.Path
	if r.URL.RawPath != "" {
		p = r.URL.RawPath
	}
	if p == "" {
		p = "/"
	}
	return p
}

// serveServiceClient authorizes and dispatches one request from a service
// client whose certificate already passed the expiry and shape checks and
// whose name is a configured client.
//
// The auth middleware runs at the root, before chi has routed, so the route
// is resolved here with the router's own Find. A route that is not on the
// allowlist (including an unknown path or a wrong method) is 403 FORBIDDEN.
// On a {name} route the shed must exist and be owned by this client; a
// missing shed and someone else's shed get the same 404 SHED_NOT_FOUND, so a
// client cannot learn which names other callers use.
func (s *Server) serveServiceClient(w http.ResponseWriter, r *http.Request, next http.Handler, name string, cc *config.ServiceClientConfig, leaf *x509.Certificate) {
	pattern := ""
	rctx := chi.NewRouteContext()
	if s.mux != nil {
		pattern = s.mux.Find(rctx, r.Method, routePath(r))
	}
	route := ""
	if pattern != "" {
		route = normalizeRoutePattern(pattern)
	}
	if e := auditEntryFrom(r.Context()); e != nil {
		e.route = route
		if strings.Contains(pattern, "{name}") {
			e.shed = rctx.URLParam("name")
		}
	}
	if pattern == "" || !clientAllowedRoutes[r.Method+" "+route] {
		writeError(w, http.StatusForbidden, config.ErrForbidden, "route not available to service clients")
		return
	}
	ctx := r.Context()
	if strings.Contains(pattern, "{name}") {
		// The backend checks the owner again against the shed it loads,
		// under its per-shed lock (and exec at the dial): this lookup alone
		// would let an operation that waits (a lock, a slow body) act on a
		// shed someone else recreated under the name in the meantime.
		ctx = backend.WithRequiredOwner(ctx, name)
		shedName := rctx.URLParam("name")
		owned, err := s.clientOwnsShed(r, shedName, name)
		if err != nil {
			// The lookup itself failed (not "no such shed"): a 404 here
			// would tell the client its shed is gone when it may not be.
			log.Printf("service client %q: look up shed %q: %v", name, shedName, err)
			writeError(w, http.StatusInternalServerError, config.ErrBackendError, "could not look up shed "+shedName)
			return
		}
		if !owned {
			writeError(w, http.StatusNotFound, config.ErrShedNotFound, "shed not found: "+shedName)
			return
		}
	}
	caller := Caller{Kind: CallerClient, Name: name, Client: cc, Serial: certSerial(leaf)}
	next.ServeHTTP(w, r.WithContext(withCaller(ctx, caller)))
}

// clientOwnsShed reports whether shedName names an existing shed whose owner
// is client. An invalid name is never looked up. A missing shed is (false,
// nil); err is set only when the lookup failed for another reason.
func (s *Server) clientOwnsShed(r *http.Request, shedName, client string) (bool, error) {
	if config.ValidateShedName(shedName) != nil {
		return false, nil
	}
	shed, err := s.backend.GetShed(r.Context(), shedName)
	if err != nil {
		if status, _, _ := mapBackendError(err); status == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return shed != nil && shed.Owner == client, nil
}
