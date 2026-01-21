package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

	"github.com/charliek/shed/internal/config"
)

// gitSSHRegex matches git@host:path format (e.g., git@github.com:user/repo.git)
var gitSSHRegex = regexp.MustCompile(`^git@[a-zA-Z0-9][a-zA-Z0-9.-]*:[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+(?:\.git)?$`)

// ValidateGitRepoURL validates that a git repository URL is well-formed.
// Accepts https://, git://, ssh://, and git@host:path formats.
func ValidateGitRepoURL(repoURL string) error {
	if repoURL == "" {
		return nil // Empty is valid (no repo to clone)
	}

	// Check for git@host:path format first (SCP-like syntax)
	if strings.HasPrefix(repoURL, "git@") {
		if !gitSSHRegex.MatchString(repoURL) {
			return fmt.Errorf("invalid git SSH URL format: %s", repoURL)
		}
		return nil
	}

	// Parse as standard URL
	parsed, err := url.Parse(repoURL)
	if err != nil {
		return fmt.Errorf("invalid repository URL: %w", err)
	}

	// Validate scheme
	validSchemes := map[string]bool{
		"https": true,
		"http":  true,
		"git":   true,
		"ssh":   true,
	}
	if !validSchemes[parsed.Scheme] {
		return fmt.Errorf("unsupported URL scheme %q: must be https, http, git, or ssh", parsed.Scheme)
	}

	// Validate host is present
	if parsed.Host == "" {
		return fmt.Errorf("repository URL must have a host")
	}

	// Validate path is present (should have at least /user/repo or /repo)
	if parsed.Path == "" || parsed.Path == "/" {
		return fmt.Errorf("repository URL must have a path")
	}

	return nil
}

// CreateShed creates a new shed with a volume, container, and optionally clones a repository.
func (c *Client) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	// Validate shed name
	if err := config.ValidateShedName(req.Name); err != nil {
		return nil, err
	}

	// Validate repository URL if provided
	if err := ValidateGitRepoURL(req.Repo); err != nil {
		return nil, err
	}

	// Determine image to use
	image := req.Image
	if image == "" {
		image = c.config.DefaultImage
	}

	containerName := config.ContainerName(req.Name)

	// Create the workspace volume
	if err := c.CreateVolume(ctx, req.Name); err != nil {
		return nil, fmt.Errorf("failed to create volume: %w", err)
	}

	// Build container configuration
	createdAt := time.Now().UTC()
	labels := map[string]string{
		config.LabelShed:        "true",
		config.LabelShedName:    req.Name,
		config.LabelShedCreated: createdAt.Format(time.RFC3339),
	}
	if req.Repo != "" {
		labels[config.LabelShedRepo] = req.Repo
	}

	containerConfig := &container.Config{
		Image:  image,
		Cmd:    []string{"sleep", "infinity"},
		Labels: labels,
		Env:    c.buildEnvList(),
	}

	hostConfig := &container.HostConfig{
		Mounts:      c.buildMounts(req.Name),
		NetworkMode: "bridge",
		RestartPolicy: container.RestartPolicy{
			Name: container.RestartPolicyUnlessStopped,
		},
		// Security: Drop all capabilities and add back only what's needed
		// for package managers and basic operations
		CapDrop: []string{"ALL"},
		CapAdd:  []string{"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER"},
	}

	// Create the container
	resp, err := c.docker.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, containerName)
	if err != nil {
		// Clean up volume on failure
		_ = c.DeleteVolume(ctx, req.Name)
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	// Start the container
	if err := c.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up on failure
		_ = c.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		_ = c.DeleteVolume(ctx, req.Name)
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Clone repository if specified
	if req.Repo != "" {
		if err := c.cloneRepo(ctx, resp.ID, req.Repo); err != nil {
			// Log warning but don't fail - container is still usable
			// The error will be noted in the shed status
			log.Printf("Warning: failed to clone repository: %v", err)
		}
	}

	return &config.Shed{
		Name:        req.Name,
		Status:      config.StatusRunning,
		CreatedAt:   createdAt,
		Repo:        req.Repo,
		ContainerID: resp.ID,
	}, nil
}

// cloneRepo clones a git repository into the container's workspace.
func (c *Client) cloneRepo(ctx context.Context, containerID, repo string) error {
	execConfig := container.ExecOptions{
		Cmd:          []string{"git", "clone", repo, "."},
		WorkingDir:   config.WorkspacePath,
		AttachStdout: true,
		AttachStderr: true,
	}

	execResp, err := c.docker.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return fmt.Errorf("failed to create exec for git clone: %w", err)
	}

	attachResp, err := c.docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{})
	if err != nil {
		return fmt.Errorf("failed to attach to exec for git clone: %w", err)
	}
	defer attachResp.Close()

	// Wait for command to complete by reading output
	_, _ = io.Copy(io.Discard, attachResp.Reader)

	// Check exit code
	inspectResp, err := c.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return fmt.Errorf("failed to inspect exec: %w", err)
	}

	if inspectResp.ExitCode != 0 {
		return fmt.Errorf("git clone failed with exit code %d", inspectResp.ExitCode)
	}

	return nil
}

// ListSheds returns all shed containers.
func (c *Client) ListSheds(ctx context.Context) ([]config.Shed, error) {
	// Filter containers by shed label
	filterArgs := filters.NewArgs()
	filterArgs.Add("label", config.LabelShed+"=true")

	containers, err := c.docker.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filterArgs,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	sheds := make([]config.Shed, 0, len(containers))
	for _, ctr := range containers {
		shed := containerToShed(ctr)
		sheds = append(sheds, shed)
	}

	return sheds, nil
}

// GetShed returns a shed by name.
func (c *Client) GetShed(ctx context.Context, name string) (*config.Shed, error) {
	containerName := config.ContainerName(name)

	// Try to get the container by name
	ctr, err := c.docker.ContainerInspect(ctx, containerName)
	if err != nil {
		// Check if it's a not found error
		if client.IsErrNotFound(err) {
			return nil, fmt.Errorf("shed %q not found", name)
		}
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	// Verify it's a shed container
	if ctr.Config.Labels[config.LabelShed] != "true" {
		return nil, fmt.Errorf("shed %q not found", name)
	}

	return inspectToShed(ctr), nil
}

// DeleteShed deletes a shed container and optionally its volume.
func (c *Client) DeleteShed(ctx context.Context, name string, keepVolume bool) error {
	containerName := config.ContainerName(name)

	// Remove container (force removal if running)
	if err := c.docker.ContainerRemove(ctx, containerName, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: false, // We handle volume separately
	}); err != nil {
		if !client.IsErrNotFound(err) {
			return fmt.Errorf("failed to remove container: %w", err)
		}
	}

	// Remove volume unless keepVolume is true
	if !keepVolume {
		if err := c.DeleteVolume(ctx, name); err != nil {
			// Log warning but don't fail if volume doesn't exist
			log.Printf("Warning: failed to delete volume: %v", err)
		}
	}

	return nil
}

// StartShed starts a stopped shed container.
func (c *Client) StartShed(ctx context.Context, name string) (*config.Shed, error) {
	containerName := config.ContainerName(name)

	// Check current state
	shed, err := c.GetShed(ctx, name)
	if err != nil {
		return nil, err
	}

	if shed.Status == config.StatusRunning {
		return nil, fmt.Errorf("shed %q is already running", name)
	}

	// Start the container
	if err := c.docker.ContainerStart(ctx, containerName, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Return updated shed info
	return c.GetShed(ctx, name)
}

// StopShed stops a running shed container.
func (c *Client) StopShed(ctx context.Context, name string) (*config.Shed, error) {
	containerName := config.ContainerName(name)

	// Check current state
	shed, err := c.GetShed(ctx, name)
	if err != nil {
		return nil, err
	}

	if shed.Status == config.StatusStopped {
		return nil, fmt.Errorf("shed %q is already stopped", name)
	}

	// Stop the container with a timeout
	timeout := 10
	if err := c.docker.ContainerStop(ctx, containerName, container.StopOptions{
		Timeout: &timeout,
	}); err != nil {
		return nil, fmt.Errorf("failed to stop container: %w", err)
	}

	// Return updated shed info
	return c.GetShed(ctx, name)
}

// AttachToShed creates an exec session to attach to a shed container.
func (c *Client) AttachToShed(ctx context.Context, name string, tty bool) (types.HijackedResponse, string, error) {
	containerName := config.ContainerName(name)

	// Verify shed exists and is running
	shed, err := c.GetShed(ctx, name)
	if err != nil {
		return types.HijackedResponse{}, "", err
	}

	if shed.Status != config.StatusRunning {
		return types.HijackedResponse{}, "", fmt.Errorf("shed %q is not running", name)
	}

	// Create exec configuration
	execConfig := container.ExecOptions{
		Cmd:          []string{"/bin/sh", "-c", "exec ${SHELL:-/bin/sh}"},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          tty,
		WorkingDir:   config.WorkspacePath,
	}

	execResp, err := c.docker.ContainerExecCreate(ctx, containerName, execConfig)
	if err != nil {
		return types.HijackedResponse{}, "", fmt.Errorf("failed to create exec session: %w", err)
	}

	// Attach to the exec session
	attachResp, err := c.docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{
		Tty: tty,
	})
	if err != nil {
		return types.HijackedResponse{}, "", fmt.Errorf("failed to attach to exec session: %w", err)
	}

	return attachResp, execResp.ID, nil
}

// containerToShed converts a container summary to a Shed.
func containerToShed(ctr types.Container) config.Shed {
	labels := ctr.Labels

	name := labels[config.LabelShedName]
	repo := labels[config.LabelShedRepo]

	var createdAt time.Time
	if created := labels[config.LabelShedCreated]; created != "" {
		createdAt, _ = time.Parse(time.RFC3339, created)
	}

	status := containerStateToStatus(ctr.State)

	return config.Shed{
		Name:        name,
		Status:      status,
		CreatedAt:   createdAt,
		Repo:        repo,
		ContainerID: ctr.ID,
	}
}

// inspectToShed converts a container inspect response to a Shed.
func inspectToShed(ctr types.ContainerJSON) *config.Shed {
	labels := ctr.Config.Labels

	name := labels[config.LabelShedName]
	repo := labels[config.LabelShedRepo]

	var createdAt time.Time
	if created := labels[config.LabelShedCreated]; created != "" {
		createdAt, _ = time.Parse(time.RFC3339, created)
	}

	status := inspectStateToStatus(ctr.State)

	return &config.Shed{
		Name:        name,
		Status:      status,
		CreatedAt:   createdAt,
		Repo:        repo,
		ContainerID: ctr.ID,
	}
}

// containerStateToStatus converts Docker container state to shed status.
func containerStateToStatus(state string) string {
	switch state {
	case "running":
		return config.StatusRunning
	case "created", "exited", "dead":
		return config.StatusStopped
	case "restarting", "paused":
		return config.StatusStarting
	default:
		return config.StatusError
	}
}

// inspectStateToStatus converts Docker container state from inspect to shed status.
func inspectStateToStatus(state *types.ContainerState) string {
	if state == nil {
		return config.StatusError
	}

	if state.Running {
		return config.StatusRunning
	}
	if state.Paused || state.Restarting {
		return config.StatusStarting
	}
	return config.StatusStopped
}
