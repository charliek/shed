package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/api"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/docker"
	"github.com/charliek/shed/internal/sshd"
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

	// Initialize Docker client
	dockerClient, err := docker.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("failed to create docker client: %w", err)
	}
	defer dockerClient.Close()
	log.Printf("Connected to Docker")

	// Create adapters for the different interfaces
	apiAdapter := &dockerAPIAdapter{client: dockerClient}
	sshAdapter := &dockerSSHAdapter{client: dockerClient}

	// Initialize SSH server
	sshServer, err := sshd.NewServer(sshAdapter, DefaultHostKeyPath, cfg.SSHPort, cfg.Terminal)
	if err != nil {
		return fmt.Errorf("failed to create SSH server: %w", err)
	}
	hostKey := sshServer.GetHostPublicKey()

	// Initialize HTTP API server
	apiServer := api.NewServer(apiAdapter, cfg, hostKey)
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

// dockerAPIAdapter adapts the docker.Client to the api.DockerClient interface.
type dockerAPIAdapter struct {
	client *docker.Client
}

// ListSheds returns all shed containers.
func (a *dockerAPIAdapter) ListSheds(ctx context.Context) ([]config.Shed, error) {
	return a.client.ListSheds(ctx)
}

// GetShed returns a single shed by name.
func (a *dockerAPIAdapter) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	return a.client.GetShed(ctx, name)
}

// CreateShed creates a new shed container.
func (a *dockerAPIAdapter) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	return a.client.CreateShed(ctx, req)
}

// DeleteShed removes a shed container and optionally its volume.
func (a *dockerAPIAdapter) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
	return a.client.DeleteShed(ctx, name, keepVolume)
}

// StartShed starts a stopped shed container.
func (a *dockerAPIAdapter) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	return a.client.StartShed(ctx, name)
}

// StopShed stops a running shed container.
func (a *dockerAPIAdapter) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	return a.client.StopShed(ctx, name)
}

// dockerSSHAdapter adapts the docker.Client to the sshd.DockerClient interface.
type dockerSSHAdapter struct {
	client *docker.Client
}

// GetShed returns a shed by name.
func (a *dockerSSHAdapter) GetShed(ctx context.Context, name string) (*sshd.ShedInfo, error) {
	shed, err := a.client.GetShed(ctx, name)
	if err != nil {
		return nil, err
	}

	return &sshd.ShedInfo{
		Name:        shed.Name,
		Status:      shed.Status,
		ContainerID: shed.ContainerID,
	}, nil
}

// StartShed starts a stopped shed.
func (a *dockerSSHAdapter) StartShed(ctx context.Context, name string) error {
	_, err := a.client.StartShed(ctx, name)
	return err
}

// ExecInContainer executes a command in a container with the given options.
func (a *dockerSSHAdapter) ExecInContainer(ctx context.Context, containerID string, opts sshd.ExecOptions) error {
	dockerClient := a.client.Docker()

	// Build command - if empty, use default login shell
	cmd := opts.Cmd
	if len(cmd) == 0 {
		cmd = []string{"/bin/bash", "--login"}
	}

	// Create exec configuration
	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdin:  opts.Stdin != nil,
		AttachStdout: opts.Stdout != nil,
		AttachStderr: opts.Stderr != nil,
		Tty:          opts.TTY,
		Env:          opts.Env,
		WorkingDir:   config.WorkspacePath,
	}

	execResp, err := dockerClient.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return fmt.Errorf("failed to create exec: %w", err)
	}

	// Attach to the exec session
	attachResp, err := dockerClient.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{
		Tty: opts.TTY,
	})
	if err != nil {
		return fmt.Errorf("failed to attach to exec: %w", err)
	}
	defer attachResp.Close()

	// Handle terminal resize if TTY is enabled
	if opts.TTY && opts.ResizeChan != nil {
		go func() {
			for size := range opts.ResizeChan {
				_ = dockerClient.ContainerExecResize(ctx, execResp.ID, container.ResizeOptions{
					Height: size.Height,
					Width:  size.Width,
				})
			}
		}()

		// Set initial size
		if opts.InitialSize != nil {
			_ = dockerClient.ContainerExecResize(ctx, execResp.ID, container.ResizeOptions{
				Height: opts.InitialSize.Height,
				Width:  opts.InitialSize.Width,
			})
		}
	}

	// Channel to signal when stdout completes (container exited)
	done := make(chan struct{})

	// Copy stdin to container (fire and forget - don't wait for it)
	if opts.Stdin != nil {
		go func() {
			_, _ = io.Copy(attachResp.Conn, opts.Stdin)
			// Close the connection's write side when stdin is done
			if cw, ok := attachResp.Conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}()
	}

	// Copy container output to stdout - when this finishes, container has exited
	go func() {
		defer close(done)
		if opts.TTY {
			// In TTY mode, all output goes to stdout
			if opts.Stdout != nil {
				_, _ = io.Copy(opts.Stdout, attachResp.Reader)
			}
		} else {
			// In non-TTY mode, we need to demux stdout/stderr
			// For simplicity, we'll just copy everything to stdout
			if opts.Stdout != nil {
				_, _ = io.Copy(opts.Stdout, attachResp.Reader)
			}
		}
	}()

	// Wait only for stdout to complete (container exit), not stdin
	<-done

	// Check exit code
	inspectResp, err := dockerClient.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return fmt.Errorf("failed to inspect exec: %w", err)
	}

	if inspectResp.ExitCode != 0 {
		return fmt.Errorf("command exited with code %d", inspectResp.ExitCode)
	}

	return nil
}
