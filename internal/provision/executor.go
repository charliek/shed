package provision

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/charliek/shed/internal/config"
)

// Default environment variables set by shed in containers.
const (
	EnvShedContainer = "SHED_CONTAINER"
	EnvShedName      = "SHED_NAME"
	EnvShedWorkspace = "SHED_WORKSPACE"
)

// Log file paths in the container.
const (
	LogDir     = "/var/log/shed"
	InstallLog = "/var/log/shed/install.log"
	StartupLog = "/var/log/shed/startup.log"
	SyncLog    = "/var/log/shed/sync.log"
)

// HookType identifies which type of hook is being run.
type HookType string

const (
	HookTypeInstall HookType = "install"
	HookTypeStartup HookType = "startup"
)

// Executor runs provisioning hooks in containers.
type Executor struct {
	docker    *client.Client
	shedName  string
	config    *Config
	output    io.Writer // Output writer for streaming (defaults to os.Stdout)
	errOutput io.Writer // Error output writer (defaults to os.Stderr)
}

// NewExecutor creates a new provisioning executor.
func NewExecutor(docker *client.Client, shedName string, cfg *Config) *Executor {
	return &Executor{
		docker:    docker,
		shedName:  shedName,
		config:    cfg,
		output:    os.Stdout,
		errOutput: os.Stderr,
	}
}

// SetOutput sets the output writers for streaming hook output.
func (e *Executor) SetOutput(stdout, stderr io.Writer) {
	e.output = stdout
	e.errOutput = stderr
}

// RunInstall runs the install hook if configured.
// Returns nil if no install hook is configured.
func (e *Executor) RunInstall(ctx context.Context, containerID string) error {
	if !e.config.HasInstallHook() {
		return nil
	}
	return e.runHook(ctx, containerID, HookTypeInstall, e.config.Hooks.Install)
}

// RunStartup runs the startup hook if configured.
// Returns nil if no startup hook is configured.
func (e *Executor) RunStartup(ctx context.Context, containerID string) error {
	if !e.config.HasStartupHook() {
		return nil
	}
	return e.runHook(ctx, containerID, HookTypeStartup, e.config.Hooks.Startup)
}

// runHook executes a hook script in the container.
func (e *Executor) runHook(ctx context.Context, containerID string, hookType HookType, scriptPath string) error {
	// Apply timeout to context
	timeout := e.config.GetTimeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Ensure log directory exists
	if err := e.ensureLogDir(ctx, containerID); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	// Build environment variables
	env := e.buildEnv()

	// Resolve script path relative to workspace
	fullScriptPath := scriptPath
	if !filepath.IsAbs(scriptPath) {
		fullScriptPath = filepath.Join(config.WorkspacePath, scriptPath)
	}

	// Get the log file path for this hook type
	logFile := e.logFileForHook(hookType)

	// Build the command to run the script and tee output to log file
	// We use bash to handle script execution and output redirection
	cmd := []string{
		"bash", "--login", "-c",
		fmt.Sprintf(`
			set -o pipefail
			chmod +x %q 2>/dev/null || true
			%q 2>&1 | tee %q
			exit ${PIPESTATUS[0]}
		`, fullScriptPath, fullScriptPath, logFile),
	}

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		Env:          env,
		WorkingDir:   config.WorkspacePath,
		AttachStdout: true,
		AttachStderr: true,
		User:         config.ContainerUser,
	}

	// Create exec
	execResp, err := e.docker.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return fmt.Errorf("failed to create exec for %s hook: %w", hookType, err)
	}

	// Attach to exec
	attachResp, err := e.docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{})
	if err != nil {
		return fmt.Errorf("failed to attach to exec for %s hook: %w", hookType, err)
	}
	defer attachResp.Close()

	// Stream output to both the provided writer and capture for error reporting
	var outputBuf bytes.Buffer
	multiWriter := io.MultiWriter(e.output, &outputBuf)

	// Copy output - Docker uses multiplexed streams for non-TTY
	_, err = stdcopy.StdCopy(multiWriter, e.errOutput, attachResp.Reader)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s hook timed out after %v: %w", hookType, timeout, ctx.Err())
		}
		return fmt.Errorf("failed to read %s hook output: %w", hookType, err)
	}

	// Check exit code
	inspectResp, err := e.docker.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return fmt.Errorf("failed to inspect %s hook execution: %w", hookType, err)
	}

	if inspectResp.ExitCode != 0 {
		// Get last few lines for error context
		lastOutput := getLastLines(outputBuf.String(), 5)
		return &HookError{
			HookType:   hookType,
			ExitCode:   inspectResp.ExitCode,
			LogFile:    logFile,
			LastOutput: lastOutput,
		}
	}

	return nil
}

// ensureLogDir creates the log directory in the container if it doesn't exist.
// Runs as root and sets open permissions so the shed user can write log files.
func (e *Executor) ensureLogDir(ctx context.Context, containerID string) error {
	execConfig := container.ExecOptions{
		Cmd:  []string{"bash", "-c", fmt.Sprintf("mkdir -p %s && chmod 777 %s", LogDir, LogDir)},
		User: "root",
	}

	execResp, err := e.docker.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return err
	}

	attachResp, err := e.docker.ContainerExecAttach(ctx, execResp.ID, container.ExecStartOptions{})
	if err != nil {
		return err
	}
	defer attachResp.Close()

	// Wait for completion by reading output
	if _, err := io.Copy(io.Discard, attachResp.Reader); err != nil {
		log.Printf("Warning: error reading exec output: %v", err)
	}

	return nil
}

// buildEnv builds the environment variables list for hook execution.
func (e *Executor) buildEnv() []string {
	env := make([]string, 0, len(e.config.Env)+3)

	// Add default shed environment variables
	env = append(env,
		fmt.Sprintf("%s=true", EnvShedContainer),
		fmt.Sprintf("%s=%s", EnvShedName, e.shedName),
		fmt.Sprintf("%s=%s", EnvShedWorkspace, config.WorkspacePath),
	)

	// Add user-configured environment variables
	for key, value := range e.config.Env {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	return env
}

// logFileForHook returns the log file path for a given hook type.
func (e *Executor) logFileForHook(hookType HookType) string {
	switch hookType {
	case HookTypeInstall:
		return InstallLog
	case HookTypeStartup:
		return StartupLog
	default:
		return filepath.Join(LogDir, string(hookType)+".log")
	}
}

// HookError represents an error from a failed hook execution.
type HookError struct {
	HookType   HookType
	ExitCode   int
	LogFile    string
	LastOutput string
	Err        error // Optional underlying error
}

func (e *HookError) Error() string {
	return fmt.Sprintf("%s hook failed (exit code %d)", e.HookType, e.ExitCode)
}

// Unwrap returns the underlying error for errors.Is/As compatibility.
func (e *HookError) Unwrap() error {
	return e.Err
}

// getLastLines returns the last n lines from a string.
func getLastLines(s string, n int) string {
	lines := bytes.Split([]byte(s), []byte("\n"))
	if len(lines) <= n {
		return s
	}
	start := len(lines) - n
	if start < 0 {
		start = 0
	}
	return string(bytes.Join(lines[start:], []byte("\n")))
}
