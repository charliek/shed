// Package api provides the HTTP API server for shed.
package api

import (
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server is the HTTP API server for shed.
type Server struct {
	backend    backend.Backend
	cfg        *config.ServerConfig
	sshHostKey string
	plugins    *plugin.Registry
	bridge     *plugin.Bridge
}

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

// Router returns the chi router for the public HTTP listener.
//
// When the internal-listener split is disabled (InternalHTTPPort <= 0,
// the default), this carries every route — the legacy single-listener
// behavior. When the split is enabled, the credential bus (/api/plugins/*)
// and the Connect tunnel (/api/sheds/*/connect/*) are omitted here and
// served only by InternalRouter on a loopback listener.
func (s *Server) Router() chi.Router {
	// Split off: the bus stays on the public router. Split on: it moves to
	// InternalRouter and the public router omits it.
	includeBus := !s.cfg.InternalListenerEnabled()
	return s.buildRouter(true, includeBus)
}

// InternalRouter returns the chi router for the loopback-only internal
// listener: the credential bus and the Connect tunnel. Only used when the
// split is enabled (InternalHTTPPort > 0).
func (s *Server) InternalRouter() chi.Router {
	return s.buildRouter(false, true)
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
}

// buildRouter registers routes gated by surface. includePublic covers the
// control plane (info, lifecycle, images, system, snapshots, sessions);
// includeBus covers the credential bus and the Connect tunnel.
func (s *Server) buildRouter(includePublic, includeBus bool) chi.Router {
	r := chi.NewRouter()
	s.useCommonMiddleware(r)

	r.Route("/api", func(r chi.Router) {
		if includePublic {
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
		}

		// Plugins / Extensions (the credential bus)
		if includeBus {
			r.Route("/plugins", func(r chi.Router) {
				r.Get("/listeners", s.handleListListeners)
				r.Get("/listeners/{namespace}/messages", s.handlePluginSubscribe)
				r.Post("/listeners/{namespace}/respond", s.handlePluginRespond)
				r.Get("/sheds", s.handleListPluginSheds)
			})
		}

		// Sheds: lifecycle routes are public; the Connect tunnel leaf is bus.
		if includePublic || includeBus {
			r.Route("/sheds", func(r chi.Router) {
				if includePublic {
					r.Get("/", s.handleListSheds)
					r.Post("/", s.handleCreateShed)
				}
				r.Route("/{name}", func(r chi.Router) {
					if includePublic {
						r.Get("/", s.handleGetShed)
						r.Delete("/", s.handleDeleteShed)
						r.Post("/start", s.handleStartShed)
						r.Post("/stop", s.handleStopShed)
						r.Post("/reset", s.handleResetShed)

						// Sessions within a shed
						r.Get("/sessions", s.handleListSessions)
						r.Delete("/sessions/{session}", s.handleKillSession)
					}
					if includeBus {
						// Connect API: TCP tunnel via HTTP upgrade
						r.Get("/connect/{port}", s.handleConnect)
					}
				})
			})
		}
	})

	return r
}
