// Package api provides the HTTP API server for shed.
package api

import (
	"context"

	"github.com/charliek/shed/internal/config"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// DockerClient defines the interface for Docker operations required by the API.
// This interface will be implemented by the docker package.
type DockerClient interface {
	// ListSheds returns all shed containers.
	ListSheds(ctx context.Context) ([]config.Shed, error)

	// GetShed returns a single shed by name.
	GetShed(ctx context.Context, name string) (*config.Shed, error)

	// CreateShed creates a new shed container.
	CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error)

	// DeleteShed removes a shed container and optionally its volume.
	DeleteShed(ctx context.Context, name string, keepVolume bool) error

	// StartShed starts a stopped shed container.
	StartShed(ctx context.Context, name string) (*config.Shed, error)

	// StopShed stops a running shed container.
	StopShed(ctx context.Context, name string) (*config.Shed, error)

	// ListSessions returns all tmux sessions in a shed container.
	ListSessions(ctx context.Context, shedName string) ([]config.Session, error)

	// KillSession terminates a tmux session in a shed container.
	KillSession(ctx context.Context, shedName, sessionName string) error
}

// Server is the HTTP API server for shed.
type Server struct {
	docker     DockerClient
	cfg        *config.ServerConfig
	sshHostKey string
}

// NewServer creates a new API server.
func NewServer(dockerClient DockerClient, cfg *config.ServerConfig, sshHostKey string) *Server {
	return &Server{
		docker:     dockerClient,
		cfg:        cfg,
		sshHostKey: sshHostKey,
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

		// Sheds
		r.Route("/sheds", func(r chi.Router) {
			r.Get("/", s.handleListSheds)
			r.Post("/", s.handleCreateShed)
			r.Route("/{name}", func(r chi.Router) {
				r.Get("/", s.handleGetShed)
				r.Delete("/", s.handleDeleteShed)
				r.Post("/start", s.handleStartShed)
				r.Post("/stop", s.handleStopShed)

				// Sessions within a shed
				r.Get("/sessions", s.handleListSessions)
				r.Delete("/sessions/{session}", s.handleKillSession)
			})
		})
	})

	return r
}
