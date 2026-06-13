package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/api"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/firecracker"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/sshd"
	"github.com/charliek/shed/internal/vz"
)

const (
	// shutdownTimeout is the maximum time to wait for graceful shutdown
	shutdownTimeout = 30 * time.Second

	// HTTP server timeouts. WriteTimeout is intentionally omitted (0):
	// SSE streams (create/pull progress and the plugin bus) write for
	// arbitrarily long, and a WriteTimeout would kill them mid-stream.
	// ReadHeaderTimeout/ReadTimeout bound slowloris-style header/body
	// reads; IdleTimeout reaps idle keep-alive conns (it does not apply
	// during an active SSE response).
	httpReadHeaderTimeout = 10 * time.Second
	httpReadTimeout       = 60 * time.Second
	httpIdleTimeout       = 120 * time.Second
)

// newHTTPServer builds an http.Server with the shared, SSE-safe timeouts.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
}

// defaultHostKeyPath returns the default path for the SSH host key.
// Uses /etc/shed/host_key when running as root (Linux servers), otherwise
// falls back to ~/.shed/host_key (macOS development without sudo).
func defaultHostKeyPath() string {
	if os.Getuid() == 0 {
		return "/etc/shed/host_key"
	}
	return config.ExpandPath("~/.shed/host_key")
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the shed server",
	Long:  `Start the shed server which provides HTTP API and SSH access to development environments.`,
	RunE:  runServe,
}

func runServe(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	log.Printf("Starting shed-server...")
	log.Printf("HTTP listen: %s", cfg.HTTPListenAddr())
	log.Printf("SSH listen: %s", cfg.SSHListenAddr())

	// Initialize plugin system (before backends, since backends use the bridge)
	pluginRegistry := plugin.NewRegistry()
	pluginBridge := plugin.NewBridge(pluginRegistry)

	// Initialize the configured backend
	var be backend.Backend
	switch cfg.DefaultBackend {
	case config.BackendFirecracker:
		fcCfg := cfg.Firecracker
		if fcCfg == nil {
			fcCfg = config.DefaultFirecrackerConfig()
		}
		fcClient, err := firecracker.NewClient(fcCfg, cfg, pluginBridge)
		if err != nil {
			return fmt.Errorf("failed to create firecracker client: %w", err)
		}
		log.Printf("Initialized Firecracker backend (default_image=%q, pull_policy=%s)", fcCfg.DefaultImage, fcCfg.PullPolicy)
		be = firecracker.NewBackend(fcClient)

	case config.BackendVZ:
		vzCfg := cfg.VZ
		if vzCfg == nil {
			return fmt.Errorf("vz backend enabled but vz config is missing")
		}
		vzClient, err := vz.NewClient(vzCfg, cfg, pluginBridge)
		if err != nil {
			return fmt.Errorf("failed to create vz client: %w", err)
		}
		log.Printf("Initialized VZ backend (default_image=%q, pull_policy=%s)", vzCfg.DefaultImage, vzCfg.PullPolicy)
		be = vz.NewBackend(vzClient)

	default:
		return fmt.Errorf("unsupported backend type: %s", cfg.DefaultBackend)
	}
	defer be.Close()

	// Initialize SSH server
	sshServer, err := sshd.NewServer(be, defaultHostKeyPath(), cfg.SSHListenAddr(), cfg.Terminal)
	if err != nil {
		return fmt.Errorf("failed to create SSH server: %w", err)
	}
	hostKey := sshServer.GetHostPublicKey()

	// Initialize HTTP API server
	apiServer := api.NewServer(be, cfg, hostKey, pluginRegistry, pluginBridge)

	// Public HTTP listener. When the internal-listener split is enabled,
	// apiServer.Router() omits the credential bus + Connect tunnel.
	httpServer := newHTTPServer(cfg.HTTPListenAddr(), apiServer.Router())

	// Optional internal (loopback) listener carrying the credential bus +
	// Connect tunnel, kept off the public interface.
	var internalServer *http.Server
	if addr := cfg.InternalHTTPListenAddr(); addr != "" {
		internalServer = newHTTPServer(addr, apiServer.InternalRouter())
	}

	// Channel to collect errors from servers (HTTP, SSH, internal HTTP).
	errChan := make(chan error, 3)

	// Start HTTP server in goroutine
	go func() {
		log.Printf("HTTP server listening on %s", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("HTTP server error: %w", err)
		}
	}()

	// Start internal HTTP server in goroutine (when split enabled)
	if internalServer != nil {
		go func() {
			log.Printf("Internal HTTP server (bus+connect) listening on %s", internalServer.Addr)
			if err := internalServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errChan <- fmt.Errorf("internal HTTP server error: %w", err)
			}
		}()
	}

	// Start SSH server in goroutine
	go func() {
		if err := sshServer.Start(); err != nil {
			errChan <- fmt.Errorf("SSH server error: %w", err)
		}
	}()

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("Shed server is ready")

	// Wait for signal or error
	select {
	case sig := <-sigChan:
		log.Printf("Received signal %v, initiating graceful shutdown...", sig)
	case err := <-errChan:
		log.Printf("Server error: %v", err)
		return err
	}

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Shutdown HTTP server
	log.Printf("Shutting down HTTP server...")
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	// Shutdown internal HTTP server (when enabled)
	if internalServer != nil {
		log.Printf("Shutting down internal HTTP server...")
		if err := internalServer.Shutdown(ctx); err != nil {
			log.Printf("internal HTTP server shutdown error: %v", err)
		}
	}

	// Shutdown SSH server
	log.Printf("Shutting down SSH server...")
	if err := sshServer.Shutdown(ctx); err != nil {
		log.Printf("SSH server shutdown error: %v", err)
	}

	log.Printf("Shutdown complete")
	return nil
}

// loadConfig loads the server configuration from the specified path or default locations.
func loadConfig() (*config.ServerConfig, error) {
	if configPath != "" {
		return config.LoadServerConfigFromPath(configPath)
	}
	return config.LoadServerConfig()
}
