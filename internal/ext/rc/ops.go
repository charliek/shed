package rc

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors mapped to process exit codes by main (see ExitCode).
var (
	// ErrBadArgs is a validation failure (exit 2): e.g. a prompt for claude-broker,
	// control chars, an invalid slug/kind.
	ErrBadArgs = errors.New("invalid arguments")
	// ErrDuplicateSlug means the tmux session name is already taken (exit 3 →
	// the orchestrator maps to 409 RC_SLUG_TAKEN).
	ErrDuplicateSlug = errors.New("rc session already exists")
	// ErrSessionNotFound means the target session is gone (exit 4).
	ErrSessionNotFound = errors.New("rc session not found")
)

const (
	defaultWaitTimeout = 20 * time.Second
	defaultPollEvery   = 750 * time.Millisecond
	// promptDeliverSettle lets a just-ready REPL finish wiring up its input before
	// the kickoff line is typed (driven through the injected sleep, so tests skip it).
	promptDeliverSettle = 1 * time.Second
)

// Getenv reads an environment variable (injected for testing).
type Getenv func(string) string

// CreateOptions configures Create.
type CreateOptions struct {
	Kind        Kind
	DisplayName string // defaults to the slug
	Slug        string // optional; generated when empty
	Workdir     string // optional; defaults to $SHED_WORKSPACE
	CreatedBy   string // optional; defaults to ToolName
	Target      string // optional advisory label
	Prompt      string // optional kickoff line (implies Wait); mutually exclusive with Plan
	// Plan is optional plan-delivery content: when set, it is written to a per-kind
	// HOME-rooted file (see plan.go) and a kickoff referencing that file is composed
	// and delivered — so Plan also implies Wait. Mutually exclusive with Prompt.
	Plan string
	// PlanFraming is optional caller framing prepended to the composed plan kickoff
	// (only meaningful with Plan). Normalized + control-char-validated like a prompt.
	PlanFraming      string
	Wait             bool // block until ready, accept trust, deliver prompt
	InteractiveShell bool // wrap claude kinds in `bash -ic` (native machines)
	// PermissionMode sets claude's --permission-mode for claude kinds ("" = omit,
	// claude's own default). e.g. "auto" or "bypassPermissions" for an unattended
	// run; with bypassPermissions, Wait also auto-accepts the one-time bypass dialog.
	PermissionMode string
	// Warnf reports a NON-FATAL create-time diagnostic. Today it carries preseed
	// outcomes: a preseed never fails a create (the session is usable either way), but a
	// silently skipped one is invisible — most sharply cursor's, whose mount guard
	// deliberately declines to write hooks.json into a host auth mount and would
	// otherwise leave the operator wondering why the session has no feed. nil discards.
	Warnf func(format string, args ...any)
	// EnsureHub, when non-nil, is invoked (best-effort) once a session has been
	// created, to make sure the local rc activity hub is running so the new session
	// is watched. It must never fail or meaningfully delay the create — a spawn
	// error is the hook's own concern (it logs and swallows). nil in tests and for
	// any caller that doesn't want the hub; production wires the detached-serve spawn.
	EnsureHub func()
}

// Create bootstraps a managed RC session and returns its DTO. With Wait (or a
// Prompt), it blocks until ready, auto-accepts the trust prompt, and delivers the
// prompt line. env/now/sleep are injected for testing.
func Create(r Runner, env Getenv, opts CreateOptions, sleep func(time.Duration)) (Session, error) {
	if !IsValidKind(opts.Kind) {
		return Session{}, fmt.Errorf("%w: unknown kind %q", ErrBadArgs, opts.Kind)
	}
	if opts.Prompt != "" {
		if !AcceptsTypedInput(opts.Kind) {
			return Session{}, fmt.Errorf("%w: kind %q does not accept a prompt", ErrBadArgs, opts.Kind)
		}
		opts.Prompt = NormalizeNewlines(opts.Prompt)
		if HasUnsafePromptChars(opts.Prompt) {
			return Session{}, fmt.Errorf("%w: prompt contains an unsupported control character", ErrBadArgs)
		}
	}
	// Plan-delivery validation (kind, size, UTF-8, framing, Plan/Prompt exclusion)
	// runs before any side effect; the file is written and the kickoff composed after
	// the slug is resolved below.
	if opts.Plan != "" {
		framing, err := validatePlanInputs(opts.Kind, opts.Plan, opts.Prompt, opts.PlanFraming)
		if err != nil {
			return Session{}, err
		}
		opts.PlanFraming = framing
	} else if opts.PlanFraming != "" {
		return Session{}, fmt.Errorf("%w: plan framing given without a plan", ErrBadArgs)
	}
	if err := validatePermissionMode(opts.Kind, opts.PermissionMode); err != nil {
		return Session{}, err
	}

	slug := opts.Slug
	if slug == "" {
		gen, err := GenSlug()
		if err != nil {
			return Session{}, err
		}
		slug = gen
	} else if !ValidCallerSlug(slug) {
		return Session{}, fmt.Errorf("%w: invalid slug %q", ErrBadArgs, slug)
	}

	workdir := firstNonEmpty(opts.Workdir, env("SHED_WORKSPACE"), env("HOME"))
	if workdir == "" {
		return Session{}, fmt.Errorf("%w: no --workdir and SHED_WORKSPACE/HOME unset", ErrBadArgs)
	}

	displayName := opts.DisplayName
	if displayName == "" {
		displayName = slug
	}
	createdBy := opts.CreatedBy
	if createdBy == "" {
		createdBy = ToolName
	}

	name := TmuxName(slug)
	// opencode-only: allocate a per-session loopback port BEFORE Metadata is built, so
	// BuildEnvArgs below can stamp it into the session env for the hub's opencode
	// watcher to read back later (opencodePortEnv, watch.go), and so it's available to
	// pass into InnerCommand. A failed allocation is non-fatal — port stays 0, the
	// session is created and usable exactly as before, just not watchable over SSE
	// (opencodePortEnv reads it back as absent/invalid and the watcher never attaches).
	port := 0
	if opts.Kind == KindOpencode {
		if p, perr := freeLoopbackPort(); perr == nil {
			port = p
		}
	}
	meta := Metadata{
		ID:          uuid.NewString(),
		DisplayName: displayName,
		Kind:        opts.Kind,
		Workdir:     workdir,
		CreatedBy:   createdBy,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Target:      opts.Target,
		Port:        port,
		Slug:        slug,
	}
	envArgs, err := BuildEnvArgs(meta)
	if err != nil {
		return Session{}, fmt.Errorf("%w: %v", ErrBadArgs, err)
	}

	// Best-effort per-tool preseed (claude: trust + onboarding, where the accept-trust
	// fallback covers any failure; cursor: the hub's hook relay, where a failure costs the
	// session its message feed but not its usability). Dispatched through the agent
	// registry — nil Preseed = no-op. A failure NEVER fails the create; it is reported
	// through Warnf so a skipped preseed is visible instead of silent.
	if spec, ok := specForKind(opts.Kind); ok && spec.Preseed != nil {
		if err := spec.Preseed(workdir, env); err != nil && opts.Warnf != nil {
			opts.Warnf("%s preseed skipped: %v", spec.Tool, err)
		}
	}

	inner := InnerCommand(opts.Kind, displayName, opts.PermissionMode, opts.InteractiveShell, port)
	res := createSession(r, name, workdir, envArgs, inner)
	if res.Code != 0 {
		if isDuplicateSession(res.Stderr) {
			return Session{}, fmt.Errorf("%w: %s", ErrDuplicateSlug, name)
		}
		return Session{}, fmt.Errorf("tmux new-session failed: %s", strings.TrimSpace(res.Stderr+res.Stdout))
	}

	// Plan delivery: write the plan to its per-kind HOME-rooted file (0600) and
	// compose the kickoff that waitUntilReady types once the session is ready. This
	// happens AFTER the tmux create so a duplicate --slug never clobbers the live
	// session's plan file (delivery only occurs below, so the ordering is safe). A
	// write failure is fatal (unlike the best-effort preseed) — the whole point of a
	// plan run is that the file is present for the agent to read — and the
	// just-created session is torn down (best-effort) so a failed plan create leaves
	// nothing behind, matching the pre-create validation failures.
	if opts.Plan != "" {
		planFile, err := writePlan(opts.Kind, slug, opts.Plan, env)
		if err != nil {
			_ = killSession(r, name)
			return Session{}, err
		}
		opts.Prompt = composePlanKickoff(planFile, opts.PlanFraming)
	}

	// The session now exists in tmux. Best-effort ensure the local hub is running so
	// it starts watching this session — deferred so it fires on the way out
	// regardless of the wait/kickoff outcome, and never blocks the create result.
	if opts.EnsureHub != nil {
		defer opts.EnsureHub()
	}

	session := Session{
		Slug:        slug,
		TmuxSession: name,
		Kind:        opts.Kind,
		State:       StateStarting,
		// lane is derived from the kind exactly as ParseSession derives it, so the
		// create DTO and a later list/probe of the same session agree.
		Lane:        laneForKind(opts.Kind),
		Managed:     true,
		DisplayName: displayName,
		Workdir:     workdir,
		ID:          meta.ID,
		CreatedBy:   createdBy,
		CreatedAt:   meta.CreatedAt,
		TargetLabel: opts.Target,
	}

	if opts.Wait || opts.Prompt != "" {
		// The one-time bypass-acceptance dialog appears only for a claude session whose
		// resolved posture is full bypass — true for both "skip" (generic) and
		// "bypassPermissions" (claude-historical), since both map to the same flag.
		flags, _ := permFlagsFor(opts.Kind, opts.PermissionMode)
		bypass := slices.Contains(flags, PermissionModeBypass)
		state, url, derr := waitUntilReady(r, name, opts.Kind, opts.Prompt, bypass, sleep)
		session.State, session.URL = state, url
		if derr != nil {
			// The session reached ready but the kickoff could not be delivered. A
			// success here would let a plan/prompt run exit 0 with nothing started, so
			// the delivery failure is the create outcome (the session is left running
			// for the caller to inspect/retry).
			return session, derr
		}
	}
	return session, nil
}

// waitUntilReady polls the pane until a terminal state (or timeout), auto-accepting
// the trust prompt once, then delivers prompt if the session reached ready. The
// returned error is non-nil only for a kickoff-delivery failure after ready — a
// classified non-ready state is a result, not an error.
func waitUntilReady(r Runner, name string, kind Kind, prompt string, bypass bool, sleep func(time.Duration)) (State, string, error) {
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := time.Now().Add(defaultWaitTimeout)
	state, url := StateStarting, ""
	trustAccepted := false
	bypassAccepted := false
	for time.Now().Before(deadline) {
		capRes := capturePane(r, name)
		if capRes.Code != 0 {
			// The session is gone (the inner command exited immediately) — report
			// dead now rather than polling empty output until the deadline.
			if isMissingSession(capRes.Stderr) {
				return StateDead, "", nil
			}
			sleep(defaultPollEvery) // transient capture error; keep polling
			continue
		}
		// A bypassPermissions session shows a one-time acceptance dialog before
		// anything else; accept it once so the session can proceed unattended. Gated
		// on bypass so a look-alike screen never draws a stray keypress otherwise.
		if bypass && IsClaudeKind(kind) && !bypassAccepted && IsBypassAcceptPrompt(capRes.Stdout) {
			// Only latch accepted on a successful send; a transient send-keys failure
			// must remain retryable rather than stalling the session until timeout.
			if res := acceptBypassPrompt(r, name); res.Code == 0 {
				bypassAccepted = true
			}
			sleep(defaultPollEvery)
			continue
		}
		state, url = ClassifyPane(kind, capRes.Stdout)
		if state == StateNeedsTrust && !trustAccepted {
			// Every agent's directory-trust gate captured so far pre-selects "yes" and
			// is accepted with Enter (claude's "Yes, I trust this folder"; codex's
			// "1. Yes, continue · Press enter to continue"). The classified needs-trust
			// state is the gate, so a single Enter accepts it for any kind.
			trustAccepted = true
			sendEnter(r, name)
			sleep(defaultPollEvery)
			continue
		}
		if state != StateStarting {
			break
		}
		sleep(defaultPollEvery)
	}
	if state == StateReady && prompt != "" {
		// A session can report ready (URL present) a beat before its REPL accepts
		// input; settle once more before typing the kickoff line. A delivery failure
		// is surfaced — otherwise a create --wait would report ready with the kickoff
		// never typed (and a plan run would exit 0 with the plan unstarted).
		sleep(promptDeliverSettle)
		if res := sendLine(r, name, prompt); res.Code != 0 {
			if isMissingSession(res.Stderr) {
				// Killed between classification and delivery: that's a dead session,
				// not a transport failure.
				return StateDead, "", nil
			}
			return state, url, fmt.Errorf("session %s is ready but kickoff delivery failed: %s",
				name, strings.TrimSpace(res.Stderr))
		}
	}
	return state, url, nil
}

// List returns every rc-* session's DTO. displayFallback receives a slug.
func List(r Runner, displayFallback func(slug string) string) ListResponse {
	return ListResponse{RCSessions: sessionsForNames(r, listSessionNames(r), displayFallback)}
}

// sessionsForNames builds the session DTOs for the given tmux session names — the
// shared enumeration loop behind List and the hub's reconcile pass (which lists names
// through listSessionNamesChecked first so a transient tmux failure skips the pass).
func sessionsForNames(r Runner, names []string, displayFallback func(slug string) string) []Session {
	sessions := make([]Session, 0, len(names))
	for _, name := range names {
		env := showEnvironment(r, name)
		pane := capturePane(r, name).Stdout
		sessions = append(sessions, ParseSession(name, env, pane, displayFallback))
	}
	return sessions
}

// capturePaneChecked returns a session's pane text (visible frame + 200 lines of
// scrollback), mapping a gone session to ErrSessionNotFound (shared by
// probe/prompt/accept-trust).
func capturePaneChecked(r Runner, name string) (string, error) {
	return checkedCapture(capturePane(r, name), name)
}

// captureVisiblePaneChecked is capturePaneChecked's VISIBLE-FRAME twin, with the same
// error mapping. Used wherever scrollback would be a lie about the present — the
// ApprovalAnchor evaluations (see captureVisiblePane).
func captureVisiblePaneChecked(r Runner, name string) (string, error) {
	return checkedCapture(captureVisiblePane(r, name), name)
}

// checkedCapture maps a capture-pane Result onto (text, error): a gone session becomes
// ErrSessionNotFound so callers can tell it from a transient tmux failure.
func checkedCapture(res Result, name string) (string, error) {
	if res.Code != 0 {
		if isMissingSession(res.Stderr) {
			return "", fmt.Errorf("%w: %s", ErrSessionNotFound, name)
		}
		return "", fmt.Errorf("tmux capture-pane failed: %s", strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// loadSession captures a session's pane + env and parses it into a DTO.
func loadSession(r Runner, slug string, displayFallback func(slug string) string) (Session, error) {
	name := TmuxName(slug)
	pane, err := capturePaneChecked(r, name)
	if err != nil {
		return Session{}, err
	}
	return ParseSession(name, showEnvironment(r, name), pane, displayFallback), nil
}

// Probe returns one session's DTO (state/url derived live). ErrSessionNotFound when
// the session is gone.
func Probe(r Runner, slug string, displayFallback func(slug string) string) (Session, error) {
	return loadSession(r, slug, displayFallback)
}

// AcceptTrust accepts a still-showing workspace-trust prompt (re-captures and
// verifies before sending Enter). A no-op when the dialog isn't present.
func AcceptTrust(r Runner, slug string) error {
	name := TmuxName(slug)
	pane, err := capturePaneChecked(r, name)
	if err != nil {
		return err
	}
	if IsTrustPrompt(pane) {
		sendEnter(r, name)
	}
	return nil
}

// PromptOptions configures Prompt.
type PromptOptions struct {
	Slug      string
	Text      string
	SessionID string // optional; must match SHED_RC_ID if set (guards a recreated slug)
}

// Prompt delivers a single line to a ready session (re-captures and verifies kind +
// state + optional session-id before sending).
func Prompt(r Runner, opts PromptOptions) error {
	opts.Text = NormalizeNewlines(opts.Text)
	if HasUnsafePromptChars(opts.Text) {
		return fmt.Errorf("%w: text contains an unsupported control character", ErrBadArgs)
	}
	session, err := loadSession(r, opts.Slug, nil)
	if err != nil {
		return err
	}
	if opts.SessionID != "" && session.ID != opts.SessionID {
		return fmt.Errorf("%w: session id mismatch (recreated?)", ErrSessionNotFound)
	}
	if !AcceptsTypedInput(session.Kind) {
		return fmt.Errorf("%w: kind %q does not accept a prompt", ErrBadArgs, session.Kind)
	}
	if session.State != StateReady {
		return fmt.Errorf("%w: session not ready (state=%s)", ErrBadArgs, session.State)
	}
	// Surface a delivery failure (e.g. the session was killed between the check and
	// the send) instead of reporting a false success.
	name := TmuxName(opts.Slug)
	if res := sendLine(r, name, opts.Text); res.Code != 0 {
		if isMissingSession(res.Stderr) {
			return fmt.Errorf("%w: %s", ErrSessionNotFound, name)
		}
		return fmt.Errorf("tmux send-keys failed: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// Kill tears down a session (idempotent: a missing session is success).
func Kill(r Runner, slug string) error {
	res := killSession(r, TmuxName(slug))
	if res.Code == 0 || isMissingSession(res.Stderr) {
		return nil
	}
	return fmt.Errorf("tmux kill-session failed: %s", strings.TrimSpace(res.Stderr))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
