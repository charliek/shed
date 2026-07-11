// Package api provides the HTTP API server for shed.
package api

import (
	"net/http"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/plugin"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/sync/singleflight"
)

// Server is the HTTP API server for shed.
type Server struct {
	backend     backend.Backend
	cfg         *config.ServerConfig
	sshHostKey  string
	plugins     *plugin.Registry
	bridge      *plugin.Bridge
	egressAudit *egress.AuditLog         // nil when egress is disabled
	egressStore *config.UserProfileStore // nil when egress is disabled
	tokens      *authtoken.Store         // nil until SetTokenStore; consulted only in secure mode (auth.mode: secure)
	rcCaps      *rcCapsCache             // per-shed rc capabilities cache (session enrichment + overview)
	// rcCapsFlight dedupes concurrent capability probes per shed across requests
	// (singleflight keyed by shed name), so M concurrent overview requests —
	// ?fresh=1 or a shared cache miss — share one guest exec per shed instead of
	// fanning out M execs (see RCCapabilities). Zero value is ready to use.
	rcCapsFlight singleflight.Group

	// rcHubFlight dedupes concurrent rc-hub ensure-start attempts per shed
	// (singleflight keyed by shed name): N racing proxy requests that find the hub
	// absent share ONE `shed-ext-rc serve --detach` exec and record ONE breaker
	// outcome. Zero value is ready to use.
	rcHubFlight singleflight.Group
	// rcHubBreaker is the per-shed circuit breaker over hub start attempts (3 fails
	// in 5 min → immediate 503 for the window), so a shed whose hub can't start
	// (FC loopback-unreachable, broken binary) can't drive an exec storm.
	rcHubBreaker *hubBreaker
	// rcAgg is the demand-driven GET /api/rc/events aggregator: zero connected
	// clients ⇒ zero upstream hub connections.
	rcAgg *rcAggregator
}

// SetEgressAudit attaches the durable egress audit log so `shed egress show`
// can return recent decisions. Called by shed-server at startup when egress is
// enabled; nil leaves the egress routes reporting no recent activity.
func (s *Server) SetEgressAudit(a *egress.AuditLog) { s.egressAudit = a }

// SetEgressUserStore attaches the runtime user-profile store backing the
// `/api/egress/profiles` routes and merged into `shed egress show`. Called at
// startup when egress is enabled; nil leaves the profile routes reporting only
// config profiles (and PUT/DELETE returning 501).
func (s *Server) SetEgressUserStore(st *config.UserProfileStore) { s.egressStore = st }

// SetTokenStore attaches the HTTP bearer-token store. The same store is shared
// with the SSH bootstrap handler (which mints into it); the auth middleware
// validates against it. Constructed once in shed-server.
func (s *Server) SetTokenStore(t *authtoken.Store) { s.tokens = t }

// NewServer creates a new API server.
func NewServer(b backend.Backend, cfg *config.ServerConfig, sshHostKey string, plugins *plugin.Registry, bridge *plugin.Bridge) *Server {
	s := &Server{
		backend:      b,
		cfg:          cfg,
		sshHostKey:   sshHostKey,
		plugins:      plugins,
		bridge:       bridge,
		rcCaps:       newRCCapsCache(),
		rcHubBreaker: newHubBreaker(),
	}
	s.rcAgg = s.newRCAggregator()
	return s
}

// useCommonMiddleware installs the shared middleware stack. RealIP is
// applied only behind a trusted proxy (it trusts client X-Forwarded-For);
// otherwise the real TCP peer address is used so a source IP can't be
// forged to evade per-IP controls or poison audit logs.
func (s *Server) useCommonMiddleware(r chi.Router) {
	r.Use(middleware.RequestID)
	if s.cfg.TrustedProxy {
		r.Use(middleware.RealIP)
	}
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(ContentTypeJSON)
	r.Use(s.authMiddleware)
}

// Router returns the chi router for the HTTP API — served over pinned TLS in
// secure mode, plain HTTP in open mode. It registers the full route surface on
// a single listener: the control plane (info, lifecycle, images, system,
// snapshots, sessions, egress) plus the credential bus (/api/plugins/*) and the
// Connect tunnel (/api/sheds/*/connect/*). In secure mode the bus requires a
// credentials-scoped token, the Connect tunnel accepts control or credentials,
// and everything else requires control (see authMiddleware).
func (s *Server) Router() chi.Router {
	r := chi.NewRouter()
	s.useCommonMiddleware(r)

	r.Route("/api", func(r chi.Router) {
		// Server info (also the unauthenticated bootstrap endpoints
		// used by `shed server add`).
		r.Get("/info", s.handleGetInfo)
		r.Get("/ssh-host-key", s.handleGetSSHHostKey)

		// Sessions (aggregate across all sheds)
		r.Get("/sessions", s.handleListAllSessions)

		// Single-call host snapshot: server identity + features, df, and every
		// shed with its rc-enriched sessions and capabilities (control scope,
		// GET-only — the auth middleware's default branch requires control).
		r.Get("/overview", s.handleOverview)

		// Aggregate rc activity SSE across every shed on this host (control scope,
		// GET-only, demand-driven). Registered as a literal before /sheds so it is
		// unambiguous; it is NOT the credential bus / Connect / egress stream, so
		// the auth middleware's default branch requires a control token.
		r.Get("/rc/events", s.handleRCEvents)

		// Images
		r.Route("/images", func(r chi.Router) {
			r.Get("/", s.handleListImages)
			// Ref-keyed forms (?ref=) for slash-bearing Docker refs; see
			// imageIdent. The legacy /{name} + /inspect/{name} forms below
			// stay for slash-free identifiers from older clients.
			r.Delete("/", s.handleDeleteImage)
			r.Get("/inspect", s.handleInspectImage)
			r.Post("/prune", s.handlePruneImages)
			r.Post("/tag", s.handleTagImage)
			r.Post("/pull", s.handlePullImage)
			r.Post("/push", s.handlePushImage)
			r.Get("/inspect/{name}", s.handleInspectImage)
			// Wrap parametric /{name} in a sub-router so it doesn't shadow
			// literal sibling routes like /pull, /push, /prune, /tag at the
			// chi trie level (which would return 405 Method Not Allowed
			// instead of dispatching to the literal POST handler).
			r.Route("/{name}", func(r chi.Router) {
				r.Delete("/", s.handleDeleteImage)
			})
		})

		// System (disk reporting + prune)
		r.Route("/system", func(r chi.Router) {
			r.Get("/df", s.handleSystemDF)
			r.Post("/prune", s.handleSystemPrune)
		})

		// Snapshots
		r.Route("/snapshots", func(r chi.Router) {
			r.Get("/", s.handleListSnapshots)
			r.Post("/", s.handleCreateSnapshot)
			// Same defensive wrap as /images — drift insurance if we ever
			// add literal sibling routes (e.g. /snapshots/prune).
			r.Route("/{name}", func(r chi.Router) {
				r.Get("/", s.handleGetSnapshot)
				r.Delete("/", s.handleDeleteSnapshot)
			})
		})

		// Egress control: the audit SSE stream (literal), user-profile CRUD
		// (literal /profiles), plus per-shed status + live set/off. The literal
		// /stream and /profiles siblings are registered before the /{name} wrap
		// so they aren't shadowed at the chi trie level — which also makes
		// "stream" and "profiles" reserved shed names on these routes.
		r.Route("/egress", func(r chi.Router) {
			r.Get("/stream", s.handleEgressStream)
			r.Get("/profiles", s.handleListProfiles)
			r.Route("/profiles/{name}", func(r chi.Router) {
				r.Get("/", s.handleGetProfile)
				r.Put("/", s.handlePutProfile)
				r.Delete("/", s.handleDeleteProfile)
			})
			r.Route("/{name}", func(r chi.Router) {
				r.Get("/", s.handleEgressShow)
				r.Post("/", s.handleEgressSet)
				r.Delete("/", s.handleEgressOff)
			})
		})

		// Plugins / Extensions (the credential bus)
		r.Route("/plugins", func(r chi.Router) {
			r.Get("/listeners", s.handleListListeners)
			r.Get("/listeners/{namespace}/messages", s.handlePluginSubscribe)
			r.Post("/listeners/{namespace}/respond", s.handlePluginRespond)
			r.Get("/sheds", s.handleListPluginSheds)
		})

		// Sheds: lifecycle routes (control scope) plus the Connect tunnel leaf
		// (control or credentials) — all on the single listener.
		r.Route("/sheds", func(r chi.Router) {
			r.Get("/", s.handleListSheds)
			r.Post("/", s.handleCreateShed)
			r.Route("/{name}", func(r chi.Router) {
				r.Get("/", s.handleGetShed)
				r.Delete("/", s.handleDeleteShed)
				r.Post("/start", s.handleStartShed)
				r.Post("/stop", s.handleStopShed)
				r.Post("/reset", s.handleResetShed)

				// Sessions within a shed
				r.Get("/sessions", s.handleListSessions)
				r.Delete("/sessions/{session}", s.handleKillSession)

				// Connect API: TCP tunnel via HTTP upgrade
				r.Get("/connect/{port}", s.handleConnect)

				// rc hub reverse proxy: a strict allowlist of hub endpoints
				// (GET v1/sessions|events|.../messages, POST .../input) forwarded
				// into the shed's guest-local rc hub. Method/path allowlist is
				// enforced inside the handler before any dial; the "*" wildcard
				// catches every sub-path so a disallowed one is 404/405, never
				// proxied. Control scope (default auth branch).
				r.Handle("/rc/*", http.HandlerFunc(s.handleRCProxy))
			})
		})
	})

	return r
}
