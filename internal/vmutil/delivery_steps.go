package vmutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/delivery"
	"github.com/charliek/shed/internal/provision"
	"github.com/charliek/shed/internal/remote/provider"
)

// The clone and the hooks as tracked delivery steps, shared by every backend:
// each returns the step's state (and reason) for the shed's record instead of
// only logging.

// workTreeProbe reports what stands at a repository's project directory, $1
// (absolute): exit 10 when nothing does; exit 11 when something that is not
// the top of a git work tree with a commit checked out does; exit 0, with the
// origin URL on stdout, when it is one. Run as the shed user; it changes
// nothing.
const workTreeProbe = `d=$1
if [ ! -e "$d" ] && [ ! -L "$d" ]; then exit 10; fi
if [ -L "$d" ] || [ ! -d "$d" ]; then exit 11; fi
top=$(git -C "$d" rev-parse --show-toplevel 2>/dev/null) || exit 11
[ "$top" = "$d" ] || exit 11
git -C "$d" rev-parse --verify -q HEAD >/dev/null 2>&1 || exit 11
git -C "$d" config --get remote.origin.url 2>/dev/null || exit 11
`

// maxOriginRead bounds what the server reads of the origin URL.
const maxOriginRead = 8 << 10

// WorkTree is what stands at a repository's project directory.
type WorkTree int

const (
	// WorkTreeAbsent: nothing is there; a clone may go ahead.
	WorkTreeAbsent WorkTree = iota
	// WorkTreeRepo: the repository's own work tree (its origin is the
	// shed's repository, with a commit checked out).
	WorkTreeRepo
	// WorkTreeOther: something else is there; it is left alone.
	WorkTreeOther
)

// ProjectDirFor is the directory a clone of repo lands in: ~/<name>, the
// name git would choose.
func ProjectDirFor(repo string) (string, error) {
	name, err := config.RepoDirName(repo)
	if err != nil {
		return "", err
	}
	return config.HomePath + "/" + name, nil
}

// ProbeWorkTree reports what stands at repo's project directory in the
// guest. The origin URL is compared with repo with any userinfo removed from
// both: a clone made with a credential in its URL is still the repository's.
// Neither URL is logged.
func ProbeWorkTree(ctx context.Context, agent *AgentClient, repo string) (WorkTree, error) {
	dir, err := ProjectDirFor(repo)
	if err != nil {
		return WorkTreeOther, err
	}
	ectx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &cappedBuffer{max: maxOriginRead, onOver: cancel}
	err = agent.Exec(ectx, backend.ExecOptions{
		Cmd:        []string{"sh", "-c", workTreeProbe, "sh", dir},
		Stdout:     NopWriteCloser(out),
		Stderr:     NopWriteCloser(io.Discard),
		WorkingDir: "/",
	})
	if out.over {
		return WorkTreeOther, nil
	}
	var exitErr *ExitError
	switch {
	case errors.As(err, &exitErr) && exitErr.Code == 10:
		return WorkTreeAbsent, nil
	case errors.As(err, &exitErr) && exitErr.Code == 11:
		return WorkTreeOther, nil
	case err != nil:
		return WorkTreeOther, fmt.Errorf("probing the project directory %s: %w", dir, err)
	}
	if SameRepo(strings.TrimSpace(out.String()), repo) {
		return WorkTreeRepo, nil
	}
	return WorkTreeOther, nil
}

// SameRepo reports whether two repository URLs name one repository: equal
// once any userinfo is removed from each.
func SameRepo(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && provider.StripRepoUserinfo(a) == provider.StripRepoUserinfo(b)
}

// CloneStep is the clone step for a shed whose repository is repo (the
// stored URL; clone may use another carrying a credential). It never removes
// anything:
//
//   - the project directory is already the repository's work tree: done,
//     without cloning;
//   - something else stands there: failed, project_dir_exists, and it is
//     left alone;
//   - nothing stands there: clone runs; done, or failed clone_failed.
//
// A failure is logged (the URL without its credentials) and warned about.
func CloneStep(ctx context.Context, agent *AgentClient, repo string, clone func(ctx context.Context) error) delivery.Step {
	probe := func(ctx context.Context) (WorkTree, error) { return ProbeWorkTree(ctx, agent, repo) }
	return CloneStepWith(ctx, probe, repo, clone)
}

// CloneStepWith is CloneStep with the probe of the project directory given
// (the remote backend's tests replace it).
func CloneStepWith(ctx context.Context, probe func(ctx context.Context) (WorkTree, error), repo string, clone func(ctx context.Context) error) delivery.Step {
	shown := config.SanitizeRepoURL(repo)
	state, err := probe(ctx)
	if err != nil {
		// The probe's error may quote the guest: it is not repeated.
		log.Printf("Warning: clone of %s: the project directory could not be probed (%s)", shown, CloneErrorClass(err))
		backend.StatusWarning(ctx, "Failed to clone repository (see server logs for details)")
		return delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonCloneFailed}
	}
	switch state {
	case WorkTreeRepo:
		backend.Status(ctx, "Repository already cloned")
		return delivery.Step{State: delivery.StepDone}
	case WorkTreeOther:
		dir, _ := ProjectDirFor(repo)
		log.Printf("Warning: clone of %s: %s exists and is not the repository's work tree; it is left alone", shown, dir)
		backend.StatusWarning(ctx, fmt.Sprintf("Did not clone the repository: %s exists and is not its work tree (left as it is)", dir))
		return delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonProjectDirExists}
	}
	if err := clone(ctx); err != nil {
		// git's own output went to the log as it ran (the documented
		// exception); the error itself is only classified.
		log.Printf("Warning: failed to clone repo %s (%s)", shown, CloneErrorClass(err))
		backend.StatusWarning(ctx, "Failed to clone repository (see server logs for details)")
		return delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonCloneFailed}
	}
	backend.Status(ctx, "Repository cloned")
	return delivery.Step{State: delivery.StepDone}
}

// HooksStep loads the provisioning config once and runs the hooks as the
// install step (see delivery.Host.Hooks): with install, the install hook,
// unless the state file says it ran before (done, not run again); then the
// startup hook when startup says so. Without install, only the startup hook
// when startup says so. A config that cannot be read fails the install step
// (hook_failed) and runs no hook.
func (p *Provisioner) HooksStep(ctx context.Context, install bool, startup func(st delivery.Step, ran bool) bool) (delivery.Step, bool) {
	cfg, err := p.LoadConfig(ctx)
	if err != nil {
		// LoadConfig's errors are fixed texts (never the config's content).
		log.Printf("Warning: shed %s: %v", p.shedName, err)
		if install {
			backend.StatusWarning(ctx, "Could not read the provisioning config (see server logs)")
			return delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonHookFailed}, false
		}
		return delivery.Step{}, false
	}
	var st delivery.Step
	ran := false
	if install {
		st, ran = p.installStep(ctx, cfg)
	}
	if startup == nil || !startup(st, ran) || !cfg.HasStartupHook() {
		return st, ran
	}
	if err := p.runStartup(ctx, cfg); err != nil {
		// The hook's own output went to the log as it ran (the documented
		// exception); its error is not repeated.
		log.Printf("Warning: shed %s: the startup hook failed", p.shedName)
	}
	return st, ran
}

// installStep runs cfg's install hook as the install step.
func (p *Provisioner) installStep(ctx context.Context, cfg *provision.Config) (delivery.Step, bool) {
	if cfg == nil || !cfg.HasInstallHook() {
		return delivery.Step{State: delivery.StepSkipped, Reason: delivery.ReasonNoHook}, false
	}
	state := NewProvisionState(p.agent)
	done, err := state.HasInstallRun(ctx)
	if err != nil {
		// The guest's answer is not repeated.
		log.Printf("Warning: shed %s: the install hook's state could not be read", p.shedName)
		return delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonHookFailed}, false
	}
	if done {
		return delivery.Step{State: delivery.StepDone}, false
	}
	if err := p.runInstall(ctx, cfg, state); err != nil {
		log.Printf("Warning: shed %s: the install hook failed", p.shedName)
		return delivery.Step{State: delivery.StepFailed, Reason: delivery.ReasonHookFailed}, true
	}
	return delivery.Step{State: delivery.StepDone}, true
}

// CloneErrorClass is a fixed classification of an error from the clone or
// the probe of its project directory, for a log line: the error itself may
// quote the guest (an exec's failure text, its exit status), so it is never
// repeated.
func CloneErrorClass(err error) string {
	var exitErr *ExitError
	switch {
	case err == nil:
		return "no error"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.As(err, &exitErr):
		return "the command exited with a failure status"
	case errors.Is(err, errInvalidRepoURL):
		return "invalid repository URL"
	}
	return "the agent did not run the command"
}
