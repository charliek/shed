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

// Router returns a configured chi router with all API routes.
func (s *Server) Router() chi.Router {
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(ContentTypeJSON)

	// API routes
	r.Route("/api", func(r chi.Router) {
		// Server info
		r.Get("/info", s.handleGetInfo)
		r.Get("/ssh-host-key", s.handleGetSSHHostKey)

		// Sessions (aggregate across all sheds)
		r.Get("/sessions", s.handleListAllSessions)

		// Images
		r.Route("/images", func(r chi.Router) {
			r.Get("/", s.handleListImages)
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

		// Plugins / Extensions
		r.Route("/plugins", func(r chi.Router) {
			r.Get("/listeners", s.handleListListeners)
			r.Get("/listeners/{namespace}/messages", s.handlePluginSubscribe)
			r.Post("/listeners/{namespace}/respond", s.handlePluginRespond)
			r.Get("/sheds", s.handleListPluginSheds)
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

		// Sheds
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
