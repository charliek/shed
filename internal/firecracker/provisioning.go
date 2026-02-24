//go:build linux
// +build linux

package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/provision"
)

// Provisioner runs provisioning hooks in Firecracker VMs using vsock.
type Provisioner struct {
	vsock    *VsockClient
	shedName string
	output   io.Writer
	errOut   io.Writer
}

// NewProvisioner creates a new Provisioner.
func NewProvisioner(vsock *VsockClient, shedName string) *Provisioner {
	return &Provisioner{
		vsock:    vsock,
		shedName: shedName,
		output:   os.Stdout,
		errOut:   os.Stderr,
	}
}

// SetOutput sets the output writers for streaming hook output.
func (p *Provisioner) SetOutput(stdout, stderr io.Writer) {
	p.output = stdout
	p.errOut = stderr
}

// LoadConfig loads provisioning configuration from within the VM.
// It reads .shed/provision.yaml from the workspace directory.
func (p *Provisioner) LoadConfig(ctx context.Context) (*provision.Config, error) {
	configPath := filepath.Join(config.WorkspacePath, provision.ShedProvisionYAML)

	// Read the config file via vsock exec
	var stdout, stderr strings.Builder
	opts := backend.ExecOptions{
		Cmd:        []string{"cat", configPath},
		Stdout:     &nopWriteCloser{&stdout},
		Stderr:     &nopWriteCloser{&stderr},
		WorkingDir: config.WorkspacePath,
		TTY:        false,
	}

	if err := p.vsock.Exec(ctx, opts); err != nil {
		// Check if file doesn't exist (expected case - no config file).
		// The vsock protocol merges stdout and stderr into a single stream,
		// so check both for the "No such file" indicator.
		combined := stdout.String() + stderr.String()
		if strings.Contains(combined, "No such file") {
			return &provision.Config{
				Env: make(map[string]string),
			}, nil
		}
		return nil, fmt.Errorf("failed to read provision config: %w", err)
	}

	// Parse the YAML content
	return provision.ParseConfigContent([]byte(stdout.String()))
}

// RunProvisioning runs provisioning hooks for a VM.
// If runInstall is true, both install and startup hooks are run.
// If runInstall is false, only the startup hook is run.
func (p *Provisioner) RunProvisioning(ctx context.Context, cfg *provision.Config, runInstall bool) error {
	if cfg == nil || !cfg.HasAnyHooks() {
		return nil
	}

	// Create state tracker for this VM
	state := NewProvisionState(p.vsock)

	// Run install hook if requested and not already run
	if runInstall && cfg.HasInstallHook() {
		installRan, err := state.HasInstallRun(ctx)
		if err != nil {
			return fmt.Errorf("failed to check install state: %w", err)
		}
		if !installRan {
			fmt.Fprintln(p.output, "Running install hook...")
			if err := p.runHook(ctx, cfg, provision.HookTypeInstall, cfg.Hooks.Install); err != nil {
				if hookErr, ok := err.(*provision.HookError); ok {
					fmt.Fprintf(p.errOut, "Install hook failed (exit code %d)\n", hookErr.ExitCode)
					fmt.Fprintf(p.errOut, "  Last output: %s\n", hookErr.LastOutput)
					fmt.Fprintf(p.errOut, "  Full log: %s\n", hookErr.LogFile)
					_ = state.MarkInstallFailed(ctx, err)
				}
				return err
			}
			fmt.Fprintln(p.output, "Install hook complete")
			_ = state.MarkInstallComplete(ctx)

			// Capture installed tool paths for subsequent hooks.
			// Install hooks often modify ~/.bashrc (e.g., bun adds PATH).
			// Non-interactive shells don't source .bashrc, so we persist
			// the PATH to /etc/profile.d/ which login shells source.
			if err := p.captureInstalledPaths(ctx); err != nil {
				fmt.Fprintf(p.errOut, "Warning: failed to capture installed paths: %v\n", err)
			}
		}
	}

	// Run startup hook
	if cfg.HasStartupHook() {
		fmt.Fprintln(p.output, "Running startup hook...")
		if err := p.runHook(ctx, cfg, provision.HookTypeStartup, cfg.Hooks.Startup); err != nil {
			if hookErr, ok := err.(*provision.HookError); ok {
				fmt.Fprintf(p.errOut, "Startup hook failed (exit code %d)\n", hookErr.ExitCode)
				fmt.Fprintf(p.errOut, "  Last output: %s\n", hookErr.LastOutput)
				fmt.Fprintf(p.errOut, "  Full log: %s\n", hookErr.LogFile)
			}
			return err
		}
		fmt.Fprintln(p.output, "Startup hook complete")
	}

	return nil
}

// runHook executes a provisioning hook script in the VM.
func (p *Provisioner) runHook(ctx context.Context, cfg *provision.Config, hookType provision.HookType, scriptPath string) error {
	// Apply timeout to context, respecting parent context deadline
	timeout := cfg.GetTimeout()
	if deadline, ok := ctx.Deadline(); ok {
		// Use the minimum of parent deadline and config timeout
		parentTimeout := time.Until(deadline)
		if parentTimeout < timeout {
			timeout = parentTimeout
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Ensure log directory exists
	if err := p.ensureLogDir(ctx); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	// Build environment variables
	env := p.buildEnv(cfg)

	// Resolve script path relative to workspace
	fullScriptPath := scriptPath
	if !filepath.IsAbs(scriptPath) {
		fullScriptPath = filepath.Join(config.WorkspacePath, scriptPath)
	}

	// Get the log file path for this hook type
	logFile := p.logFileForHook(hookType)

	// Build the command to run the script and tee output to log file
	cmd := fmt.Sprintf(`
		set -o pipefail
		chmod +x %q 2>/dev/null || true
		%q 2>&1 | tee %q
		exit ${PIPESTATUS[0]}
	`, fullScriptPath, fullScriptPath, logFile)

	// Capture output for error reporting
	var outputBuf bytes.Buffer
	multiWriter := io.MultiWriter(p.output, &outputBuf)

	opts := backend.ExecOptions{
		Cmd:        []string{"bash", "--login", "-c", cmd},
		Env:        env,
		Stdout:     &nopWriteCloser{multiWriter},
		Stderr:     &nopWriteCloser{p.errOut},
		WorkingDir: config.WorkspacePath,
		TTY:        false,
	}

	if err := p.vsock.Exec(ctx, opts); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s hook timed out after %v: %w", hookType, timeout, ctx.Err())
		}

		// Extract exit code from typed error
		exitCode := 1
		var exitErr *ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.Code
		}

		// Get last few lines for error context
		lastOutput := getLastLines(outputBuf.String(), 5)
		return &provision.HookError{
			HookType:   hookType,
			ExitCode:   exitCode,
			LogFile:    logFile,
			LastOutput: lastOutput,
		}
	}

	return nil
}

// captureInstalledPaths captures PATH from an interactive shell and persists
// it to /etc/profile.d/ so login shells (used by subsequent hooks) inherit
// tools installed by the install hook (e.g., bun adds itself to ~/.bashrc).
// Also detects mise shims directory, since mise doesn't modify .bashrc.
func (p *Provisioner) captureInstalledPaths(ctx context.Context) error {
	cmd := `
PATH_VAL=$(bash -ic 'echo "$PATH"' 2>/dev/null | tail -1)
# mise doesn't add shims to .bashrc; detect and prepend if present
MISE_SHIMS="$HOME/.local/share/mise/shims"
if [ -d "$MISE_SHIMS" ] && ! echo "$PATH_VAL" | grep -q "$MISE_SHIMS"; then
  PATH_VAL="$MISE_SHIMS:$PATH_VAL"
fi
echo "export PATH=\"$PATH_VAL\"" | sudo tee /etc/profile.d/shed-installed-tools.sh > /dev/null
`
	opts := backend.ExecOptions{
		Cmd:        []string{"bash", "-c", cmd},
		WorkingDir: "/",
		TTY:        false,
	}
	return p.vsock.Exec(ctx, opts)
}

// ensureLogDir creates the log directory in the VM if it doesn't exist.
// The directory is pre-created in the rootfs owned by shed, but this is a
// safety net for edge cases. Uses sudo since /var/log is a system path.
func (p *Provisioner) ensureLogDir(ctx context.Context) error {
	cmd := fmt.Sprintf("sudo mkdir -p %s && sudo chown shed:shed %s && sudo chmod 755 %s",
		provision.LogDir, provision.LogDir, provision.LogDir)
	opts := backend.ExecOptions{
		Cmd:        []string{"/bin/sh", "-c", cmd},
		WorkingDir: "/",
		TTY:        false,
	}
	return p.vsock.Exec(ctx, opts)
}

// buildEnv builds the environment variables list for hook execution.
func (p *Provisioner) buildEnv(cfg *provision.Config) []string {
	env := make([]string, 0, len(cfg.Env)+3)

	// Add default shed environment variables
	env = append(env,
		fmt.Sprintf("%s=true", provision.EnvShedContainer),
		fmt.Sprintf("%s=%s", provision.EnvShedName, p.shedName),
		fmt.Sprintf("%s=%s", provision.EnvShedWorkspace, config.WorkspacePath),
	)

	// Add user-configured environment variables from provision.yaml
	for key, value := range cfg.Env {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	return env
}

// logFileForHook returns the log file path for a given hook type.
func (p *Provisioner) logFileForHook(hookType provision.HookType) string {
	switch hookType {
	case provision.HookTypeInstall:
		return provision.InstallLog
	case provision.HookTypeStartup:
		return provision.StartupLog
	default:
		return filepath.Join(provision.LogDir, string(hookType)+".log")
	}
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

// ProvisionState tracks provisioning state in Firecracker VMs via files.
type ProvisionState struct {
	vsock *VsockClient
}

// State file path and keys (matching Docker implementation).
const (
	stateFilePath   = "/var/log/shed/.provision_state"
	stateKeyInstall = "install_ran"
	stateKeyError   = "error"
)

// NewProvisionState creates a new provisioning state tracker.
func NewProvisionState(vsock *VsockClient) *ProvisionState {
	return &ProvisionState{vsock: vsock}
}

// HasInstallRun checks if the install hook has already run.
func (s *ProvisionState) HasInstallRun(ctx context.Context) (bool, error) {
	state, err := s.readStateFile(ctx)
	if err != nil {
		return false, err
	}
	return state[stateKeyInstall] == "true", nil
}

// MarkInstallComplete marks the install hook as having run successfully.
func (s *ProvisionState) MarkInstallComplete(ctx context.Context) error {
	return s.writeStateFile(ctx, map[string]string{
		stateKeyInstall: "true",
	})
}

// MarkInstallFailed records that the install hook failed with the given error.
func (s *ProvisionState) MarkInstallFailed(ctx context.Context, err error) error {
	return s.writeStateFile(ctx, map[string]string{
		stateKeyError: err.Error(),
	})
}

// escapeStateValue escapes a value for safe storage in the key=value state file.
// Backslashes are escaped first, then newlines, ensuring round-trip safety.
func escapeStateValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

// unescapeStateValue reverses escapeStateValue.
func unescapeStateValue(s string) string {
	var result strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				result.WriteByte('\n')
				i++
			case '\\':
				result.WriteByte('\\')
				i++
			default:
				result.WriteByte(s[i])
			}
		} else {
			result.WriteByte(s[i])
		}
	}
	return result.String()
}

// writeStateFile writes provisioning state to a file in the VM.
func (s *ProvisionState) writeStateFile(ctx context.Context, state map[string]string) error {
	// Build content with escaped values to handle newlines safely
	var content strings.Builder
	for key, value := range state {
		content.WriteString(fmt.Sprintf("%s=%s\n", key, escapeStateValue(value)))
	}

	// Use heredoc to safely write content.
	// The log dir is pre-created and owned by shed, so the state file write
	// works without sudo. Use sudo mkdir as a safety net fallback.
	cmd := fmt.Sprintf(`sudo mkdir -p %s && sudo chown shed:shed %s && cat > %s << 'SHED_EOF'
%s
SHED_EOF`, provision.LogDir, provision.LogDir, stateFilePath, content.String())

	opts := backend.ExecOptions{
		Cmd:        []string{"bash", "-c", cmd},
		WorkingDir: "/",
		TTY:        false,
	}

	return s.vsock.Exec(ctx, opts)
}

// readStateFile reads provisioning state from the VM.
func (s *ProvisionState) readStateFile(ctx context.Context) (map[string]string, error) {
	var stdout, stderr strings.Builder
	opts := backend.ExecOptions{
		Cmd:        []string{"cat", stateFilePath},
		Stdout:     &nopWriteCloser{&stdout},
		Stderr:     &nopWriteCloser{&stderr},
		WorkingDir: "/",
		TTY:        false,
	}

	if err := s.vsock.Exec(ctx, opts); err != nil {
		// The vsock protocol merges stdout and stderr into a single stream,
		// so check both for the "No such file" indicator.
		combined := stdout.String() + stderr.String()
		if strings.Contains(combined, "No such file") {
			return nil, nil
		}
		return nil, err
	}

	// Parse key=value pairs, unescaping values that may contain encoded newlines
	state := make(map[string]string)
	lines := strings.Split(stdout.String(), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if idx := strings.Index(line, "="); idx > 0 {
			key := line[:idx]
			value := unescapeStateValue(line[idx+1:])
			state[key] = value
		}
	}

	return state, nil
}
