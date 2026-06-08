package vmutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/provision"
	"github.com/charliek/shed/internal/retry"
)

// Provisioner runs provisioning hooks in VMs using the agent.
type Provisioner struct {
	agent    *AgentClient
	shedName string
	workDir  string
	output   io.Writer
	errOut   io.Writer
}

// NewProvisioner creates a new Provisioner. The working directory defaults to
// the home directory; callers should SetWorkDir to the shed's landing directory
// so hooks run in (and provision.yaml is read from) the project directory.
func NewProvisioner(agent *AgentClient, shedName string) *Provisioner {
	return &Provisioner{
		agent:    agent,
		shedName: shedName,
		workDir:  config.HomePath,
		output:   os.Stdout,
		errOut:   os.Stderr,
	}
}

// SetWorkDir sets the directory provisioning hooks run in and where
// .shed/provision.yaml is read from (the shed's landing directory). Empty is
// ignored, keeping the default (HomePath).
func (p *Provisioner) SetWorkDir(dir string) {
	if dir != "" {
		p.workDir = dir
	}
}

// SetOutput sets the output writers for streaming hook output.
func (p *Provisioner) SetOutput(stdout, stderr io.Writer) {
	p.output = stdout
	p.errOut = stderr
}

// provisionReadBackoffs bounds the retry of the provision-config read. The
// read can momentarily fail right after the --local-dir VirtioFS/9P mount: the
// mount syscall returns before the host share is serving reads, so the first
// `cat` of .shed/provision.yaml hits a transient I/O error. Without a retry the
// entire provisioning step is silently skipped (RunProvisioning's caller
// log-and-continues), so a shed comes up with no hooks run. A genuinely-absent
// config is the common case and is NOT retried — see LoadConfig.
var provisionReadBackoffs = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
}

// errProvisionConfigNotReady marks a read that succeeded but returned no content
// on a project mount — treated as coherence lag and retried.
var errProvisionConfigNotReady = errors.New("provision config not ready (empty read on project mount)")

// loadConfigProbe reads .shed/provision.yaml and reports, via exit code,
// whether a missing file looks like a genuinely-absent config or a mount that
// is not serving yet. $1 is the config path, $2 the landing (work) dir:
//
//	0  → config read; its contents are on stdout
//	66 → no config file, and the landing dir lists content (mount serving)
//	75 → landing dir empty/unreadable (mount almost certainly not serving)
const loadConfigProbe = `if out=$(cat "$1" 2>/dev/null); then printf '%s' "$out"; exit 0; fi; ` +
	`if [ -n "$(ls -A "$2" 2>/dev/null)" ]; then exit 66; fi; exit 75`

// LoadConfig loads provisioning configuration from within the VM.
// It reads .shed/provision.yaml from the workspace directory.
//
// On a --local-dir shed the read can race the project mount: the VirtioFS/9P
// mount syscall returns before the host share is coherent, so the landing dir
// momentarily lists empty (probe exit 75) — or lists a partial set without
// .shed yet (probe exit 66) — and the config read finds nothing. Without
// resilience here, provisioning is silently skipped and the shed comes up with
// no hooks run.
//
// The retry distinguishes the common no-provisioning case from the race using
// the landing dir: a bare shed (landing == HomePath) that reports "no config"
// is terminal, so sheds without provisioning pay no latency. A project landing
// dir (--local-dir / --repo) keeps retrying a "no config" result, since it may
// be a mount that hasn't settled; if it stays absent the retries drain and we
// conclude there genuinely is no config.
func (p *Provisioner) LoadConfig(ctx context.Context) (*provision.Config, error) {
	configPath := filepath.Join(p.workDir, provision.ShedProvisionYAML)
	projectDir := p.workDir != config.HomePath

	var content string
	lastCode := -1
	readErr := retry.Do(ctx, "read provision config", provisionReadBackoffs, nil, func() error {
		var stdout, stderr strings.Builder
		opts := backend.ExecOptions{
			Cmd: []string{"sh", "-c", loadConfigProbe, "sh", configPath, p.workDir},
			// Run from "/" so the exec itself never depends on the racing mount.
			Stdout:     NopWriteCloser(&stdout),
			Stderr:     NopWriteCloser(&stderr),
			WorkingDir: "/",
			TTY:        false,
		}
		err := p.agent.Exec(ctx, opts)
		if err == nil {
			content = stdout.String()
			lastCode = 0
			// A successful-but-empty read on a project mount is most likely
			// coherence lag (the dentry appeared before the file data), so
			// retry. A genuinely-empty provision.yaml is a no-op anyway, so
			// retrying then concluding "no config" is harmless.
			if projectDir && strings.TrimSpace(content) == "" {
				return errProvisionConfigNotReady
			}
			return nil
		}
		var exitErr *ExitError
		if errors.As(err, &exitErr) {
			lastCode = exitErr.Code
		} else {
			lastCode = -1
		}
		// On a bare shed, "no config" (66) is terminal — the common case stays
		// fast. On a project landing dir it may be an unsettled mount, so retry.
		if lastCode == 66 && !projectDir {
			return nil
		}
		return err
	})

	switch {
	case strings.TrimSpace(content) != "":
		// Got config content (possibly after retries) — parse it.
		return provision.ParseConfigContent([]byte(content))
	case lastCode == 66 || lastCode == 0:
		// Dir served but no config file (66), or an empty config file (0):
		// genuinely no provisioning. Not an error.
		return &provision.Config{Env: make(map[string]string)}, nil
	case readErr != nil:
		// Drained retries on a mount that never served (75) or an RPC error.
		return nil, fmt.Errorf("failed to read provision config: %w", readErr)
	default:
		return &provision.Config{Env: make(map[string]string)}, nil
	}
}

// RunProvisioning runs provisioning hooks for a VM.
// If runInstall is true, both install and startup hooks are run.
// If runInstall is false, only the startup hook is run.
func (p *Provisioner) RunProvisioning(ctx context.Context, cfg *provision.Config, runInstall bool) error {
	if cfg == nil || !cfg.HasAnyHooks() {
		return nil
	}

	// Create state tracker for this VM
	state := NewProvisionState(p.agent)

	// Run install hook if requested and not already run
	if runInstall && cfg.HasInstallHook() {
		installRan, err := state.HasInstallRun(ctx)
		if err != nil {
			return fmt.Errorf("failed to check install state: %w", err)
		}
		if !installRan {
			fmt.Fprintln(p.output, "Running install hook...")
			backend.Phase(ctx, "provision")
			backend.Status(ctx, "Running install hook...")
			if err := p.runHook(ctx, cfg, provision.HookTypeInstall, cfg.Hooks.Install); err != nil {
				if hookErr, ok := err.(*provision.HookError); ok {
					fmt.Fprintf(p.errOut, "Install hook failed (exit code %d)\n", hookErr.ExitCode)
					fmt.Fprintf(p.errOut, "  Last output: %s\n", hookErr.LastOutput)
					fmt.Fprintf(p.errOut, "  Full log: %s\n", hookErr.LogFile)
					backend.Phase(ctx, "provision")
					backend.StatusWarning(ctx, fmt.Sprintf("Install hook failed (exit code %d)", hookErr.ExitCode))
					_ = state.MarkInstallFailed(ctx, err)
				}
				return err
			}
			fmt.Fprintln(p.output, "Install hook complete")
			backend.Phase(ctx, "provision")
			backend.Status(ctx, "Install hook complete")
			_ = state.MarkInstallComplete(ctx)
		}
	}

	// Run startup hook
	if cfg.HasStartupHook() {
		fmt.Fprintln(p.output, "Running startup hook...")
		backend.Phase(ctx, "provision")
		backend.Status(ctx, "Running startup hook...")
		if err := p.runHook(ctx, cfg, provision.HookTypeStartup, cfg.Hooks.Startup); err != nil {
			if hookErr, ok := err.(*provision.HookError); ok {
				fmt.Fprintf(p.errOut, "Startup hook failed (exit code %d)\n", hookErr.ExitCode)
				fmt.Fprintf(p.errOut, "  Last output: %s\n", hookErr.LastOutput)
				fmt.Fprintf(p.errOut, "  Full log: %s\n", hookErr.LogFile)
				backend.Phase(ctx, "provision")
				backend.StatusWarning(ctx, fmt.Sprintf("Startup hook failed (exit code %d)", hookErr.ExitCode))
			}
			return err
		}
		fmt.Fprintln(p.output, "Startup hook complete")
		backend.Phase(ctx, "provision")
		backend.Status(ctx, "Startup hook complete")
	}

	return nil
}

// RunShutdownHook runs the shutdown hook if configured.
// Failures are logged as warnings but never returned.
func (p *Provisioner) RunShutdownHook(ctx context.Context, cfg *provision.Config) {
	if cfg == nil || !cfg.HasShutdownHook() {
		return
	}

	fmt.Fprintln(p.output, "Running shutdown hook...")
	if err := p.runHook(ctx, cfg, provision.HookTypeShutdown, cfg.Hooks.Shutdown); err != nil {
		if hookErr, ok := err.(*provision.HookError); ok {
			fmt.Fprintf(p.errOut, "Warning: shutdown hook failed (exit code %d)\n", hookErr.ExitCode)
			fmt.Fprintf(p.errOut, "  Last output: %s\n", hookErr.LastOutput)
			fmt.Fprintf(p.errOut, "  Full log: %s\n", hookErr.LogFile)
		} else {
			fmt.Fprintf(p.errOut, "Warning: shutdown hook failed: %v\n", err)
		}
		return
	}
	fmt.Fprintln(p.output, "Shutdown hook complete")
}

// runHook executes a provisioning hook script in the VM.
func (p *Provisioner) runHook(ctx context.Context, cfg *provision.Config, hookType provision.HookType, scriptPath string) error {
	timeout := cfg.GetTimeout()
	if deadline, ok := ctx.Deadline(); ok {
		parentTimeout := time.Until(deadline)
		if parentTimeout < timeout {
			timeout = parentTimeout
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := p.ensureLogDir(ctx); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	env := p.buildEnv(cfg)

	fullScriptPath := scriptPath
	if !filepath.IsAbs(scriptPath) {
		fullScriptPath = filepath.Join(p.workDir, scriptPath)
	}

	logFile := p.logFileForHook(hookType)

	cmd := fmt.Sprintf(`
		set -o pipefail
		chmod +x %q 2>/dev/null || true
		%q 2>&1 | tee %q
		exit ${PIPESTATUS[0]}
	`, fullScriptPath, fullScriptPath, logFile)

	var outputBuf bytes.Buffer
	multiWriter := io.MultiWriter(p.output, &outputBuf)

	opts := backend.ExecOptions{
		Cmd:        []string{"bash", "--login", "-c", cmd},
		Env:        env,
		Stdout:     NopWriteCloser(multiWriter),
		Stderr:     NopWriteCloser(p.errOut),
		WorkingDir: p.workDir,
		TTY:        false,
	}

	if err := p.agent.Exec(ctx, opts); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s hook timed out after %v: %w", hookType, timeout, ctx.Err())
		}

		exitCode := 1
		var exitErr *ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.Code
		}

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

// ensureLogDir creates the log directory in the VM if it doesn't exist.
func (p *Provisioner) ensureLogDir(ctx context.Context) error {
	cmd := fmt.Sprintf("sudo mkdir -p %s && sudo chown shed:shed %s && sudo chmod 755 %s",
		provision.LogDir, provision.LogDir, provision.LogDir)
	opts := backend.ExecOptions{
		Cmd:        []string{"/bin/sh", "-c", cmd},
		WorkingDir: "/",
		TTY:        false,
	}
	return p.agent.Exec(ctx, opts)
}

// buildEnv builds the environment variables list for hook execution.
func (p *Provisioner) buildEnv(cfg *provision.Config) []string {
	env := make([]string, 0, len(cfg.Env)+3)

	env = append(env,
		fmt.Sprintf("%s=true", provision.EnvShedContainer),
		fmt.Sprintf("%s=%s", provision.EnvShedName, p.shedName),
		fmt.Sprintf("%s=%s", provision.EnvShedWorkspace, p.workDir),
	)

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
	case provision.HookTypeShutdown:
		return provision.ShutdownLog
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

// RunShutdownSequence runs the shutdown hook for a VM with appropriate timeout budgeting.
// This encapsulates the identical shutdown pattern used by both VZ and Firecracker backends.
// Failures are logged as warnings but never cause the sequence to fail.
func RunShutdownSequence(ctx context.Context, agent *AgentClient, name, workDir string, stopTimeout time.Duration, stdout, stderr io.Writer) {
	hookBudget := stopTimeout / 2
	if hookBudget > 30*time.Second {
		hookBudget = 30 * time.Second
	}

	provisioner := NewProvisioner(agent, name)
	provisioner.SetWorkDir(workDir)
	provisioner.SetOutput(stdout, stderr)

	hookCtx, hookCancel := context.WithTimeout(ctx, hookBudget)
	defer hookCancel()

	cfg, err := provisioner.LoadConfig(hookCtx)
	if err != nil {
		log.Printf("Warning: failed to load provision config for shutdown hook: %v", err)
		return
	}
	if cfg.HasShutdownHook() {
		provisioner.RunShutdownHook(hookCtx, cfg)
	}
}

// SyncFilesystems asks the guest to flush its dirty page cache via `sync(1)`
// before the VM is signaled to stop. Without this, vfkit / firecracker
// terminate while there are still dirty pages buffered in the VM kernel
// and/or the host-side mmap of the upper image, and the on-disk
// upper.ext4 the host sees is an older state than what the guest had
// just observed. That diverges:
//   - `shed snapshot create` clones the on-disk file, so freshly written
//     guest data is missing from the snapshot;
//   - host-side debug / df / orphan-detection logic that opens the file
//     out-of-band sees a stale view.
//
// Failures here are logged but never fail the stop sequence — the agent
// may be unreachable on a sick VM, but we still need to stop it.
//
// Budget: tight, capped well below the shutdown-hook budget so the
// total pre-stop guest work stays within roughly stopTimeout/2 +
// stopTimeout/8 instead of unbounded. `sync(1)` on a healthy guest
// returns in milliseconds; the only reason it would hit the timeout
// is a hung agent, in which case stalling here doesn't help anyone.
func SyncFilesystems(ctx context.Context, agent *AgentClient, stopTimeout time.Duration) {
	if agent == nil {
		return
	}
	budget := stopTimeout / 8
	if budget < 2*time.Second {
		budget = 2 * time.Second
	}
	if budget > 5*time.Second {
		budget = 5 * time.Second
	}
	syncCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if err := agent.Exec(syncCtx, backend.ExecOptions{
		Cmd:        []string{"sync"},
		WorkingDir: "/",
	}); err != nil {
		log.Printf("Warning: in-guest sync before stop failed: %v", err)
	}
}

// ProvisionState tracks provisioning state in VMs via files.
type ProvisionState struct {
	agent *AgentClient
}

// State file path and keys (matching Docker implementation).
const (
	stateFilePath   = "/var/log/shed/.provision_state"
	stateKeyInstall = "install_ran"
	stateKeyError   = "error"
)

// NewProvisionState creates a new provisioning state tracker.
func NewProvisionState(agent *AgentClient) *ProvisionState {
	return &ProvisionState{agent: agent}
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
	var content strings.Builder
	for key, value := range state {
		fmt.Fprintf(&content, "%s=%s\n", key, escapeStateValue(value))
	}

	cmd := fmt.Sprintf(`sudo mkdir -p %s && sudo chown shed:shed %s && cat > %s << 'SHED_EOF'
%s
SHED_EOF`, provision.LogDir, provision.LogDir, stateFilePath, content.String())

	opts := backend.ExecOptions{
		Cmd:        []string{"bash", "-c", cmd},
		WorkingDir: "/",
		TTY:        false,
	}

	return s.agent.Exec(ctx, opts)
}

// readStateFile reads provisioning state from the VM.
func (s *ProvisionState) readStateFile(ctx context.Context) (map[string]string, error) {
	var stdout strings.Builder
	opts := backend.ExecOptions{
		Cmd:        []string{"sh", "-c", fmt.Sprintf("test -f %s && cat %s", stateFilePath, stateFilePath)},
		Stdout:     NopWriteCloser(&stdout),
		WorkingDir: "/",
		TTY:        false,
	}

	if err := s.agent.Exec(ctx, opts); err != nil {
		// test -f returns exit code 1 when file doesn't exist (expected on first run)
		var exitErr *ExitError
		if errors.As(err, &exitErr) {
			return nil, nil
		}
		return nil, err
	}

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
