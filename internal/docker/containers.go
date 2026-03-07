package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/provision"
)

// CreateShed creates a new shed with a volume, container, and optionally clones a repository.
func (c *Client) CreateShed(ctx context.Context, req config.CreateShedRequest) (*config.Shed, error) {
	// Validate shed name
	if err := config.ValidateShedName(req.Name); err != nil {
		return nil, err
	}

	// Determine image to use
	image := req.Image
	if image == "" {
		image = c.config.DefaultImage
	}

	containerName := config.ContainerName(req.Name)

	// Create the workspace volume (skip when using a local directory bind mount)
	if req.LocalDir == "" {
		backend.Progress(ctx, "volume", "Creating workspace volume...")
		if err := c.CreateVolume(ctx, req.Name); err != nil {
			return nil, fmt.Errorf("failed to create volume: %w", err)
		}
	}

	// Build container configuration
	createdAt := time.Now().UTC()
	labels := map[string]string{
		config.LabelShed:        "true",
		config.LabelShedName:    req.Name,
		config.LabelShedCreated: createdAt.Format(time.RFC3339),
		config.LabelShedBackend: config.BackendDocker,
	}
	if req.Repo != "" {
		labels[config.LabelShedRepo] = req.Repo
	}
	if req.LocalDir != "" {
		labels[config.LabelShedLocalDir] = req.LocalDir
	}

	containerConfig := &container.Config{
		Image:  image,
		Cmd:    []string{"sleep", "infinity"},
		Labels: labels,
		Env:    c.buildEnvList(),
	}

	hostConfig := &container.HostConfig{
		Mounts:      c.buildMounts(req.Name, req.LocalDir),
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
	backend.Progress(ctx, "container", "Creating container...")
	resp, err := c.docker.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, containerName)
	if err != nil {
		// Clean up volume on failure (only if we created one)
		if req.LocalDir == "" {
			_ = c.DeleteVolume(ctx, req.Name)
		}
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	// Start the container
	backend.Progress(ctx, "start", "Starting container...")
	if err := c.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up on failure
		_ = c.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		if req.LocalDir == "" {
			_ = c.DeleteVolume(ctx, req.Name)
		}
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Fix permissions for the non-root shed user
	if req.LocalDir != "" {
		// Remap container shed user UID/GID to match host workspace owner.
		// This avoids chowning the bind-mounted host directory, which would
		// destructively change file ownership on Linux.
		if err := c.fixLocalDirPermissions(ctx, resp.ID); err != nil {
			log.Printf("Warning: failed to fix local dir permissions: %v", err)
		}
	} else {
		if err := c.fixWorkspaceOwnership(ctx, resp.ID); err != nil {
			log.Printf("Warning: failed to fix workspace ownership: %v", err)
		}
	}

	// Clone repository if specified (skip when using local dir — the directory IS the workspace)
	if req.Repo != "" && req.LocalDir == "" {
		backend.Progress(ctx, "repo", "Cloning repository...")
		if err := c.cloneRepo(ctx, resp.ID, req.Repo); err != nil {
			// Log warning but don't fail - container is still usable
			// The error will be noted in the shed status
			log.Printf("Warning: failed to clone repository: %v", err)
		}
	}

	// Run provisioning hooks if not disabled
	if !req.NoProvision {
		backend.Progress(ctx, "provision", "Running provisioning...")
		if err := c.runProvisioning(ctx, resp.ID, req.Name, true, os.Stdout, os.Stderr); err != nil {
			log.Printf("Warning: provisioning failed: %v", err)
		}
	}

	return c.GetShed(ctx, req.Name)
}

// cloneRepo clones a git repository into the container's workspace.
func (c *Client) cloneRepo(ctx context.Context, containerID, repo string) error {
	execConfig := container.ExecOptions{
		Cmd:          []string{"git", "clone", repo, "."},
		WorkingDir:   config.WorkspacePath,
		AttachStdout: true,
		AttachStderr: true,
		User:         config.ContainerUser,
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
	if _, err := io.Copy(io.Discard, attachResp.Reader); err != nil {
		log.Printf("Warning: error reading git clone output: %v", err)
	}

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
		if cerrdefs.IsNotFound(err) {
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

	// Check if this shed uses a local directory (no volume to delete)
	isLocalDir := false
	if ctr, err := c.docker.ContainerInspect(ctx, containerName); err == nil {
		isLocalDir = ctr.Config.Labels[config.LabelShedLocalDir] != ""
	}

	// Remove container (force removal if running)
	if err := c.docker.ContainerRemove(ctx, containerName, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: false, // We handle volume separately
	}); err != nil {
		if !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("failed to remove container: %w", err)
		}
	}

	// Remove volume unless keepVolume is true or this is a local-dir shed
	if !keepVolume && !isLocalDir {
		if err := c.DeleteVolume(ctx, name); err != nil {
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

	// Fix permissions for the non-root shed user
	if shed.LocalDir != "" {
		if err := c.fixLocalDirPermissions(ctx, shed.ContainerID); err != nil {
			log.Printf("Warning: failed to fix local dir permissions: %v", err)
		}
	} else {
		// Handles migrated volumes from old root-based containers
		if err := c.fixWorkspaceOwnership(ctx, shed.ContainerID); err != nil {
			log.Printf("Warning: failed to fix workspace ownership: %v", err)
		}
	}

	// Run startup hook only (install already ran on create)
	if err := c.runProvisioning(ctx, shed.ContainerID, name, false, os.Stdout, os.Stderr); err != nil {
		log.Printf("Warning: startup hook failed: %v", err)
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
		User:         config.ContainerUser,
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
func containerToShed(ctr container.Summary) config.Shed {
	labels := ctr.Labels

	name := labels[config.LabelShedName]
	repo := labels[config.LabelShedRepo]
	localDir := labels[config.LabelShedLocalDir]
	// Default to docker for backwards compatibility with existing containers
	backend := labels[config.LabelShedBackend]
	if backend == "" {
		backend = config.BackendDocker
	}

	var createdAt time.Time
	if created := labels[config.LabelShedCreated]; created != "" {
		createdAt, _ = time.Parse(time.RFC3339, created)
	}

	status := containerStateToStatus(ctr.State)

	// Extract IP address from first network
	var ipAddress string
	if ctr.NetworkSettings != nil {
		for _, net := range ctr.NetworkSettings.Networks {
			if net.IPAddress != "" {
				ipAddress = net.IPAddress
				break
			}
		}
	}

	return config.Shed{
		Name:        name,
		Status:      status,
		CreatedAt:   createdAt,
		Repo:        repo,
		ContainerID: ctr.ID,
		Backend:     backend,
		IPAddress:   ipAddress,
		LocalDir:    localDir,
	}
}

// inspectToShed converts a container inspect response to a Shed.
func inspectToShed(ctr container.InspectResponse) *config.Shed {
	labels := ctr.Config.Labels

	name := labels[config.LabelShedName]
	repo := labels[config.LabelShedRepo]
	localDir := labels[config.LabelShedLocalDir]
	// Default to docker for backwards compatibility with existing containers
	backend := labels[config.LabelShedBackend]
	if backend == "" {
		backend = config.BackendDocker
	}

	var createdAt time.Time
	if created := labels[config.LabelShedCreated]; created != "" {
		createdAt, _ = time.Parse(time.RFC3339, created)
	}

	status := inspectStateToStatus(ctr.State)

	// Extract IP address from first network
	var ipAddress string
	if ctr.NetworkSettings != nil {
		for _, net := range ctr.NetworkSettings.Networks {
			if net.IPAddress != "" {
				ipAddress = net.IPAddress
				break
			}
		}
	}

	return &config.Shed{
		Name:        name,
		Status:      status,
		CreatedAt:   createdAt,
		Repo:        repo,
		ContainerID: ctr.ID,
		Backend:     backend,
		IPAddress:   ipAddress,
		LocalDir:    localDir,
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
func inspectStateToStatus(state *container.State) string {
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

// runProvisioning loads and runs provisioning hooks for a container.
// If runInstall is true, both install and startup hooks are run.
// If runInstall is false, only the startup hook is run.
// Output is written to stdout and stderr writers.
func (c *Client) runProvisioning(ctx context.Context, containerID, shedName string, runInstall bool, stdout, stderr io.Writer) error {
	// Load provisioning config from within the container
	cfg, err := provision.LoadConfigFromContainer(ctx, c.docker, containerID, config.WorkspacePath)
	if err != nil {
		return fmt.Errorf("failed to load provisioning config: %w", err)
	}

	if !cfg.HasAnyHooks() {
		return nil
	}

	// Create executor
	executor := provision.NewExecutor(c.docker, shedName, cfg)
	executor.SetOutput(stdout, stderr)

	// Create state tracker
	state := provision.NewState(c.docker)

	// Run install hook if requested and not already run
	if runInstall && cfg.HasInstallHook() {
		installRan, _ := state.HasInstallRun(ctx, containerID)
		if !installRan {
			fmt.Fprintln(stdout, "Running install hook...")
			if err := executor.RunInstall(ctx, containerID); err != nil {
				if hookErr, ok := err.(*provision.HookError); ok {
					fmt.Fprintf(stderr, "✗ Install hook failed (exit code %d)\n", hookErr.ExitCode)
					fmt.Fprintf(stderr, "  Last output: %s\n", hookErr.LastOutput)
					fmt.Fprintf(stderr, "  Full log: %s\n", hookErr.LogFile)
					_ = state.MarkInstallFailed(ctx, containerID, err)
				}
				return err
			}
			fmt.Fprintln(stdout, "✓ Install hook complete")
			_ = state.MarkInstallComplete(ctx, containerID)

			// Capture installed tool paths for subsequent hooks.
			// Install hooks often modify ~/.bashrc (e.g., bun adds PATH).
			// Non-interactive shells don't source .bashrc, so we persist
			// the PATH to /etc/profile.d/ which login shells source.
			if err := c.captureInstalledPaths(ctx, containerID); err != nil {
				fmt.Fprintf(stderr, "Warning: failed to capture installed paths: %v\n", err)
			}
		}
	}

	// Run startup hook
	if cfg.HasStartupHook() {
		fmt.Fprintln(stdout, "Running startup hook...")
		if err := executor.RunStartup(ctx, containerID); err != nil {
			if hookErr, ok := err.(*provision.HookError); ok {
				fmt.Fprintf(stderr, "✗ Startup hook failed (exit code %d)\n", hookErr.ExitCode)
				fmt.Fprintf(stderr, "  Last output: %s\n", hookErr.LastOutput)
				fmt.Fprintf(stderr, "  Full log: %s\n", hookErr.LogFile)
			}
			return err
		}
		fmt.Fprintln(stdout, "✓ Startup hook complete")
	}

	return nil
}

// captureInstalledPaths captures PATH from an interactive shell and persists
// it to /etc/profile.d/ so login shells (used by subsequent hooks) inherit
// tools installed by the install hook (e.g., bun adds itself to ~/.bashrc).
// Also detects mise shims directory, since mise doesn't modify .bashrc.
func (c *Client) captureInstalledPaths(ctx context.Context, containerID string) error {
	cmd := []string{
		"bash", "-c",
		`PATH_VAL=$(bash -ic 'echo "$PATH"' 2>/dev/null | tail -1)
if [ -z "$PATH_VAL" ]; then
  echo "ERROR: failed to capture PATH" >&2
  exit 1
fi
MISE_SHIMS="$HOME/.local/share/mise/shims"
if [ -d "$MISE_SHIMS" ] && ! echo "$PATH_VAL" | grep -q "$MISE_SHIMS"; then
  PATH_VAL="$MISE_SHIMS:$PATH_VAL"
fi
echo "export PATH=\"$PATH_VAL\"" | sudo tee /etc/profile.d/shed-installed-tools.sh > /dev/null`,
	}
	execConfig := container.ExecOptions{Cmd: cmd, User: config.ContainerUser}
	execResp, err := c.docker.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return err
	}
	attachResp, err := c.docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{})
	if err != nil {
		return err
	}
	defer attachResp.Close()
	_, _ = io.Copy(io.Discard, attachResp.Reader)

	inspectResp, err := c.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return fmt.Errorf("failed to inspect exec: %w", err)
	}
	if inspectResp.ExitCode != 0 {
		return fmt.Errorf("captureInstalledPaths failed with exit code %d", inspectResp.ExitCode)
	}
	return nil
}

// fixWorkspaceOwnership ensures the workspace and home directory intermediates
// are owned by the shed user. Docker creates parent directories as root when
// setting up bind mounts (e.g. /home/shed/.local/state/ for an opencode mount
// at /home/shed/.local/state/opencode). Without this fix, tools like mise
// can't create sibling directories.
func (c *Client) fixWorkspaceOwnership(ctx context.Context, containerID string) error {
	homeDir := fmt.Sprintf("/home/%s", config.ContainerUser)
	cmd := []string{
		"bash", "-c",
		fmt.Sprintf(`user="%s"
# Fix workspace
owner=$(stat -c %%U %s 2>/dev/null)
if [ "$owner" != "$user" ]; then chown "$user:$user" %s; fi
# Fix home directory intermediates created by Docker bind mounts
for dir in %s/.local %s/.local/state %s/.local/share %s/.cache %s/.config; do
  if [ -d "$dir" ] && [ "$(stat -c %%U "$dir")" != "$user" ]; then
    chown "$user:$user" "$dir"
  fi
done`,
			config.ContainerUser,
			config.WorkspacePath, config.WorkspacePath,
			homeDir, homeDir, homeDir, homeDir, homeDir),
	}
	execConfig := container.ExecOptions{
		Cmd:  cmd,
		User: "root",
	}
	execResp, err := c.docker.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return err
	}
	attachResp, err := c.docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{})
	if err != nil {
		return err
	}
	defer attachResp.Close()
	_, _ = io.Copy(io.Discard, attachResp.Reader)

	inspectResp, err := c.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return fmt.Errorf("failed to inspect exec: %w", err)
	}
	if inspectResp.ExitCode != 0 {
		return fmt.Errorf("fixWorkspaceOwnership failed with exit code %d", inspectResp.ExitCode)
	}
	return nil
}

// fixLocalDirPermissions remaps the container's shed user UID/GID to match
// the bind-mounted workspace directory owner. This avoids running chown on
// the host directory, which would destructively change file ownership on Linux.
func (c *Client) fixLocalDirPermissions(ctx context.Context, containerID string) error {
	homeDir := fmt.Sprintf("/home/%s", config.ContainerUser)
	cmd := []string{
		"bash", "-c",
		fmt.Sprintf(`user="%s"
# Detect workspace owner UID/GID
WS_UID=$(stat -c %%u %s)
WS_GID=$(stat -c %%g %s)
SHED_UID=$(id -u "$user")
SHED_GID=$(id -g "$user")

# Remap shed user/group if UID doesn't match
if [ "$WS_UID" != "$SHED_UID" ]; then
  usermod -u "$WS_UID" "$user" 2>/dev/null
  chown -R "$user" "%s"
fi
if [ "$WS_GID" != "$SHED_GID" ]; then
  groupmod -g "$WS_GID" "$user" 2>/dev/null
  chgrp -R "$user" "%s"
fi

# Fix home directory intermediates created by Docker bind mounts
for dir in %s/.local %s/.local/state %s/.local/share %s/.cache %s/.config; do
  if [ -d "$dir" ] && [ "$(stat -c %%U "$dir")" != "$user" ]; then
    chown "$user:$user" "$dir"
  fi
done`,
			config.ContainerUser,
			config.WorkspacePath, config.WorkspacePath,
			homeDir, homeDir,
			homeDir, homeDir, homeDir, homeDir, homeDir),
	}
	execConfig := container.ExecOptions{
		Cmd:  cmd,
		User: "root",
	}
	execResp, err := c.docker.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return err
	}
	attachResp, err := c.docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{})
	if err != nil {
		return err
	}
	defer attachResp.Close()
	_, _ = io.Copy(io.Discard, attachResp.Reader)

	inspectResp, err := c.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return fmt.Errorf("failed to inspect exec: %w", err)
	}
	if inspectResp.ExitCode != 0 {
		return fmt.Errorf("fixLocalDirPermissions failed with exit code %d", inspectResp.ExitCode)
	}
	return nil
}
