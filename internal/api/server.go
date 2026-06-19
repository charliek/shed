// Package api provides the HTTP API server for shed.
package api

import (
	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/plugin"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server is the HTTP API server for shed.
type Server struct {
	backend     backend.Backend
	cfg         *config.ServerConfig
	sshHostKey  string
	plugins     *plugin.Registry
	bridge      *plugin.Bridge
	egressAudit *egress.AuditLog // nil when egress is disabled
	tokens      *authtoken.Store // nil until SetTokenStore; consulted only in secure mode (auth.mode: secure)
}

// SetEgressAudit attaches the durable egress audit log so `shed egress show`
// can return recent decisions. Called by shed-server at startup when egress is
// enabled; nil leaves the egress routes reporting no recent activity.
func (s *Server) SetEgressAudit(a *egress.AuditLog) { s.egressAudit = a }

// SetTokenStore attaches the HTTP bearer-token store. The same store is shared
// with the SSH bootstrap handler (which mints into it); the auth middleware
// validates against it. Constructed once in shed-server.
func (s *Server) SetTokenStore(t *authtoken.Store) { s.tokens = t }

// NewServer creates a new API server.
func NewServer(b backend.Backend, cfg *config.ServerConfig, sshHostKey string, plugins *plugin.Registry, bridge *plugin.Bridge) *Server {
	return &Server{
		backend:    b,
		cfg:        cfg,
		sshHostKey: sshHostKey,
		plugins:    plugins,
		bridge:     bridge,
	}
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
// Connect tunnel (/api/sheds/*/connect/*), which require a credentials-scoped
// token in secure mode (see authMiddleware).
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

		// Egress control: the audit SSE stream (literal), plus per-shed
		// status + live set/off. The /{name} routes are wrapped so the
		// literal /stream sibling isn't shadowed at the chi trie level.
		r.Route("/egress", func(r chi.Router) {
			r.Get("/stream", s.handleEgressStream)
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

		// Sheds: lifecycle routes plus the Connect tunnel leaf (credentials
		// scope) — all on the single listener.
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
			})
		})
	})

	return r
}
