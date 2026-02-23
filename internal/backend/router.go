package backend

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/charliek/shed/internal/config"
)

// Router routes backend operations to the appropriate implementation.
// It supports multiple enabled backends with a configured default.
type Router struct {
	defaultType Type
	backends    map[Type]Backend
	order       []Type
}

// NewRouter creates a backend router with a default backend and enabled backends.
func NewRouter(defaultType Type, backends map[Type]Backend) (*Router, error) {
	if len(backends) == 0 {
		return nil, fmt.Errorf("no backends configured")
	}
	if _, ok := backends[defaultType]; !ok {
		return nil, fmt.Errorf("default backend %q is not enabled", defaultType)
	}

	order := make([]Type, 0, len(backends))
	for t := range backends {
		order = append(order, t)
	}
	sort.Slice(order, func(i, j int) bool {
		return order[i] < order[j]
	})

	return &Router{
		defaultType: defaultType,
		backends:    backends,
		order:       order,
	}, nil
}

// Type returns the backend type identifier.
func (r *Router) Type() Type {
	return r.defaultType
}

// Close releases any resources held by the backends.
func (r *Router) Close() error {
	var firstErr error
	for _, backend := range r.backends {
		if err := backend.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// CreateShed creates a new shed with the given configuration.
func (r *Router) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	backend, err := r.backendForCreate(req)
	if err != nil {
		return nil, err
	}
	return backend.CreateShed(ctx, req)
}

// GetShed returns a shed by name.
func (r *Router) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	_, shed, err := r.backendForShed(ctx, name)
	return shed, err
}

// ListSheds returns all sheds managed by enabled backends.
func (r *Router) ListSheds(ctx context.Context) ([]config.Shed, error) {
	var all []config.Shed
	for _, backend := range r.backends {
		sheds, err := backend.ListSheds(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, sheds...)
	}
	return all, nil
}

// DeleteShed removes a shed. If keepVolume is true, the workspace is preserved.
func (r *Router) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
	backend, _, err := r.backendForShed(ctx, name)
	if err != nil {
		return err
	}
	return backend.DeleteShed(ctx, name, keepVolume)
}

// StartShed starts a stopped shed.
func (r *Router) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	backend, _, err := r.backendForShed(ctx, name)
	if err != nil {
		return nil, err
	}
	return backend.StartShed(ctx, name)
}

// StopShed stops a running shed.
func (r *Router) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	backend, _, err := r.backendForShed(ctx, name)
	if err != nil {
		return nil, err
	}
	return backend.StopShed(ctx, name)
}

// ListSessions returns all sessions in a shed.
func (r *Router) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	backend, _, err := r.backendForShed(ctx, shedName)
	if err != nil {
		return nil, err
	}
	return backend.ListSessions(ctx, shedName)
}

// KillSession terminates a session in a shed.
func (r *Router) KillSession(ctx context.Context, shedName, sessionName string) error {
	backend, _, err := r.backendForShed(ctx, shedName)
	if err != nil {
		return err
	}
	return backend.KillSession(ctx, shedName, sessionName)
}

// Exec executes a command in a shed with the given options.
func (r *Router) Exec(ctx context.Context, shedName string, opts ExecOptions) error {
	backend, _, err := r.backendForShed(ctx, shedName)
	if err != nil {
		return err
	}
	return backend.Exec(ctx, shedName, opts)
}

// GetNetworkEndpoint returns the network endpoint (IP or hostname) for a shed.
func (r *Router) GetNetworkEndpoint(ctx context.Context, shedName string) (string, error) {
	backend, _, err := r.backendForShed(ctx, shedName)
	if err != nil {
		return "", err
	}
	return backend.GetNetworkEndpoint(ctx, shedName)
}

func (r *Router) backendForCreate(req config.CreateShedRequest) (Backend, error) {
	if req.Backend != "" {
		backend, ok := r.backends[Type(req.Backend)]
		if !ok {
			return nil, fmt.Errorf("backend %q is not enabled", req.Backend)
		}
		return backend, nil
	}

	return r.backends[r.defaultType], nil
}

func (r *Router) backendForShed(ctx context.Context, name string) (Backend, *config.Shed, error) {
	var (
		foundBackend Backend
		foundShed    *config.Shed
	)

	for _, backendType := range r.order {
		backend := r.backends[backendType]
		shed, err := backend.GetShed(ctx, name)
		if err != nil {
			if isShedNotFound(err) {
				continue
			}
			return nil, nil, err
		}

		if foundBackend != nil {
			return nil, nil, fmt.Errorf("shed %q exists in multiple backends", name)
		}

		foundBackend = backend
		foundShed = shed
	}

	if foundBackend == nil {
		return nil, nil, fmt.Errorf("shed %q not found", name)
	}

	return foundBackend, foundShed, nil
}

func isShedNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}
