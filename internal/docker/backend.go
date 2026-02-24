package docker

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// Compile-time check that DockerBackend implements backend.Backend.
var _ backend.Backend = (*DockerBackend)(nil)

// DockerBackend implements backend.Backend using Docker containers.
type DockerBackend struct {
	client *Client
}

// NewBackend creates a new DockerBackend wrapping the given Client.
func NewBackend(client *Client) *DockerBackend {
	return &DockerBackend{client: client}
}

// Type returns the backend type identifier.
func (b *DockerBackend) Type() backend.Type {
	return backend.TypeDocker
}

// Close releases any resources held by the backend.
func (b *DockerBackend) Close() error {
	return b.client.Close()
}

// CreateShed creates a new shed with the given configuration.
func (b *DockerBackend) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	return b.client.CreateShed(ctx, req)
}

// GetShed returns a shed by name, or an error if not found.
func (b *DockerBackend) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.client.GetShed(ctx, name)
}

// ListSheds returns all sheds managed by this backend.
func (b *DockerBackend) ListSheds(ctx context.Context) ([]config.Shed, error) {
	return b.client.ListSheds(ctx)
}

// DeleteShed removes a shed. If keepVolume is true, the workspace is preserved.
func (b *DockerBackend) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
	return b.client.DeleteShed(ctx, name, keepVolume)
}

// StartShed starts a stopped shed.
func (b *DockerBackend) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.client.StartShed(ctx, name)
}

// StopShed stops a running shed.
func (b *DockerBackend) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	return b.client.StopShed(ctx, name)
}

// ListSessions returns all sessions in a shed.
func (b *DockerBackend) ListSessions(ctx context.Context, shedName string) ([]config.Session, error) {
	return b.client.ListSessions(ctx, shedName)
}

// KillSession terminates a session in a shed.
func (b *DockerBackend) KillSession(ctx context.Context, shedName, sessionName string) error {
	return b.client.KillSession(ctx, shedName, sessionName)
}

// Exec executes a command in a shed with the given options.
func (b *DockerBackend) Exec(ctx context.Context, shedName string, opts backend.ExecOptions) error {
	// Get the shed to find the container ID
	shed, err := b.client.GetShed(ctx, shedName)
	if err != nil {
		return err
	}

	return b.execInContainer(ctx, shed.ContainerID, opts)
}

// execInContainer executes a command in a container with the given options.
func (b *DockerBackend) execInContainer(ctx context.Context, containerID string, opts backend.ExecOptions) error {
	dockerClient := b.client.Docker()

	// Build command - if empty, use default login shell
	cmd := opts.Cmd
	if len(cmd) == 0 {
		cmd = []string{"/bin/bash", "--login"}
	} else {
		// Wrap command in shell to support operators like &&, ||, |, etc.
		// SSH sends the command as space-separated tokens, but Docker exec
		// expects an argv. The shell parses operators correctly.
		cmd = []string{"/bin/sh", "-c", strings.Join(cmd, " ")}
	}

	// Create exec configuration
	execConfig := buildExecConfig(cmd, opts)

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
			// In non-TTY mode, Docker multiplexes stdout/stderr with headers
			// Use stdcopy to demux the stream
			if opts.Stdout != nil {
				stderr := opts.Stderr
				if stderr == nil {
					stderr = opts.Stdout // fallback: send stderr to stdout
				}
				_, _ = stdcopy.StdCopy(opts.Stdout, stderr, attachResp.Reader)
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

func buildExecConfig(cmd []string, opts backend.ExecOptions) container.ExecOptions {
	workingDir := opts.WorkingDir
	if workingDir == "" {
		workingDir = config.WorkspacePath
	}

	return container.ExecOptions{
		Cmd:          cmd,
		AttachStdin:  opts.Stdin != nil,
		AttachStdout: opts.Stdout != nil,
		AttachStderr: opts.Stderr != nil,
		Tty:          opts.TTY,
		Env:          opts.Env,
		WorkingDir:   workingDir,
		User:         config.ContainerUser,
	}
}

// GetNetworkEndpoint returns the network endpoint (IP) for a shed.
func (b *DockerBackend) GetNetworkEndpoint(ctx context.Context, shedName string) (string, error) {
	shed, err := b.client.GetShed(ctx, shedName)
	if err != nil {
		return "", err
	}

	return b.client.GetContainerIP(ctx, shed.ContainerID)
}
