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
	"github.com/charliek/shed/internal/docker"
	"github.com/charliek/shed/internal/firecracker"
	"github.com/charliek/shed/internal/sshd"
	"github.com/charliek/shed/internal/vz"
)

const (
	// DefaultHostKeyPath is the default path for the SSH host key
	DefaultHostKeyPath = "/etc/shed/host_key"

	// shutdownTimeout is the maximum time to wait for graceful shutdown
	shutdownTimeout = 30 * time.Second
)

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
	log.Printf("HTTP port: %d", cfg.HTTPPort)
	log.Printf("SSH port: %d", cfg.SSHPort)

	// Initialize enabled backends
	backends := make(map[backend.Type]backend.Backend)
	for _, backendType := range cfg.EnabledBackends {
		switch backendType {
		case config.BackendDocker:
			dockerClient, err := docker.NewClient(cfg)
			if err != nil {
				return fmt.Errorf("failed to create docker client: %w", err)
			}
			log.Printf("Connected to Docker")
			backends[backend.TypeDocker] = docker.NewBackend(dockerClient)

		case config.BackendFirecracker:
			fcCfg := cfg.Firecracker
			if fcCfg == nil {
				fcCfg = config.DefaultFirecrackerConfig()
			}
			fcClient, err := firecracker.NewClient(fcCfg, cfg)
			if err != nil {
				return fmt.Errorf("failed to create firecracker client: %w", err)
			}
			log.Printf("Initialized Firecracker backend")
			backends[backend.TypeFirecracker] = firecracker.NewBackend(fcClient)

		case config.BackendVZ:
			vzCfg := cfg.VZ
			if vzCfg == nil {
				return fmt.Errorf("vz backend enabled but vz config is missing")
			}
			vzClient, err := vz.NewClient(vzCfg, cfg)
			if err != nil {
				return fmt.Errorf("failed to create vz client: %w", err)
			}
			log.Printf("Initialized VZ backend")
			backends[backend.TypeVZ] = vz.NewBackend(vzClient)

		default:
			return fmt.Errorf("unknown backend type: %s", backendType)
		}
	}

	be, err := backend.NewRouter(backend.Type(cfg.DefaultBackend), backends)
	if err != nil {
		return fmt.Errorf("failed to initialize backend router: %w", err)
	}
	defer be.Close()

	// Initialize SSH server
	sshServer, err := sshd.NewServer(be, DefaultHostKeyPath, cfg.SSHPort, cfg.Terminal)
	if err != nil {
		return fmt.Errorf("failed to create SSH server: %w", err)
	}
	hostKey := sshServer.GetHostPublicKey()

	// Initialize HTTP API server
	apiServer := api.NewServer(be, cfg, hostKey)
	router := apiServer.Router()

	// Create HTTP server
	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler: router,
	}

	// Channel to collect errors from servers
	errChan := make(chan error, 2)

	// Start HTTP server in goroutine
	go func() {
		log.Printf("HTTP server listening on :%d", cfg.HTTPPort)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- fmt.Errorf("HTTP server error: %w", err)
		}
	}()

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
