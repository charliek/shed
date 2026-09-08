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
	// kickoffSettle is how long a LIVE session must have been settling before a
	// kickoff line is typed into it. It exists because liveness (S2,
	// charliek/shed#324) says nothing about whether the agent's TUI has finished
	// drawing its composer — the pane classifier used to be that (bad) proxy.
	//
	// The origin the window is measured from is max(first successful capture, last
	// SUCCESSFUL control keystroke): a trust/bypass dialog accepted at 4.9 s must not
	// get the kickoff typed into its repaint, and a send-keys that FAILED did not
	// change the screen, so it must not push the origin out either. Bounded by
	// defaultWaitTimeout — a session whose settle would cross the deadline is
	// delivered at the deadline rather than not at all.
	//
	// Paid only when there IS a kickoff: a --wait with nothing to deliver has nothing
	// to settle for and returns on the first successful capture. A fixed delay is not
	// a heuristic about screen content; it is the honest interim until the roost
	// provider script owns kickoff (S4).
	kickoffSettle = 5 * time.Second
	// promptDeliverSettle lets a just-live REPL finish wiring up its input before the
	// kickoff line is typed (driven through the injected sleep, so tests skip it). It
	// inspects nothing — it is a plain sleep on top of kickoffSettle, so a kickoff
	// lands at least 6 s after liveness.
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
	// BinProbe gates Create for a non-shell kind: before any tmux work, it checks
	// whether the kind's agent binary is reachable on the launch PATH via
	// `bash -lc/-ic 'command -v <bin>'` — the caller (clirc.go's effectiveBinProbe)
	// MUST bind the same shell VERB as this same call's InteractiveShell, because the
	// two launch paths genuinely differ:
	//
	//   - InteractiveShell=false (the dominant guest path — shed-ext-rc's `create`
	//     with no --interactive-shell) — the inner tmux command is a bare, unwrapped
	//     exec that inherits the pane's environment, and that pane was itself created
	//     by shed-ext-rc running over SSH under the server's `bash -lc` wrap: a LOGIN
	//     shell (/etc/profile.d/*.sh + /etc/environment.d). The probe must match with
	//     a login shell too, or it consults a NARROWER PATH than the real launch and
	//     false-negative-rejects an agent that lives on the login PATH.
	//   - InteractiveShell=true (native-machine callers, e.g. shed-machine-rc's
	//     `claude` verb) — innerCommandTUI genuinely wraps the inner command in
	//     `bash -ic` (an rc-file PATH: nvm/asdf/mise shims sourced from .bashrc), so
	//     the probe must match with an interactive shell.
	//
	// A plain exec.LookPath, or the fixed login-shell probe capabilities.go uses for
	// discovery, would get the wrong answer for one of the two cases above. Skipped
	// entirely for a kind whose spec declares no Bin (shell has none to probe). nil
	// skips the check (tests, and any caller with nothing to probe); production wires
	// the real bash-backed probe (see clirc.go's realBinProbe).
	BinProbe InstalledProbe
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
	// Installed-agent gate: before any tmux work, confirm the kind's binary is
	// actually reachable on the launch PATH. Without this, a missing binary surfaces
	// only as an opaque "session died on create (state=dead)" once the tmux inner
	// command exits immediately — this turns that into a named, actionable error.
	// Skipped for a kind with no Bin (shell) and when no probe is wired (nil BinProbe
	// — see the field doc).
	if spec, ok := specForKind(opts.Kind); ok && spec.Bin != "" && opts.BinProbe != nil {
		if !opts.BinProbe(spec.Bin) {
			// internal/ext/rc backs both a shed's shed-ext-rc (agents baked into the
			// rootfs image) and a native machine's shed-machine-rc (agents user-installed),
			// so the remediation names both possibilities rather than assuming one.
			return Session{}, fmt.Errorf("%w: agent %q was not found on the session PATH — it may be missing from this shed's image (recreate from a newer image) or not installed on this machine; or pick another --kind",
				ErrBadArgs, spec.Bin)
		}
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
	// A leading ~ is expanded HERE, on the target, where HOME is the right home.
	// Nothing else on the path would do it: the CLI quotes every argv element
	// (that is its safety property) and tmux takes -c as a literal path, so
	// `--workdir ~/prox` silently started the agent in HOME instead — and then
	// the hub could not correlate its feed, because the pane's real directory did
	// not match the workdir it had been told. Two symptoms, one unexpanded tilde.
	if home := env("HOME"); home == "" && (workdir == "~" || strings.HasPrefix(workdir, "~/")) {
		// Handing tmux a literal ~ would put the agent somewhere other than where the
		// session says it is — the exact silent mismatch this expansion exists to
		// prevent. Say so instead.
		return Session{}, fmt.Errorf("%w: --workdir starts with ~ but HOME is unset", ErrBadArgs)
	}
	workdir = expandTilde(workdir, env("HOME"))

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
	// compose the kickoff that waitUntilLive types once the session is live. This
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
		state, url, derr := waitUntilLive(r, name, opts.Kind, opts.Prompt, bypass, sleep, nil)
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

// hasControlDialog reports whether the pane is showing ANY of the one-time control
// dialogs the kept matchers know: claude's workspace-trust prompt, codex's
// directory-trust prompt, or claude's bypass acceptance.
//
// NOT kind-gated, unlike isTrustDialog: this is the DELIVERY REFUSAL's question, and
// refusing is the safe direction — a look-alike phrase costs a caller one retry,
// whereas typing a kickoff into a modal answers it by accident.
func hasControlDialog(pane string) bool {
	return IsTrustPrompt(pane) || IsCodexTrustPrompt(pane) || IsBypassAcceptPrompt(pane)
}

// errControlDialogUp is the refusal BOTH delivery paths make — the one-shot `prompt`
// verb and the `--wait` kickoff — spelled once so the two cannot diverge in message
// or exit class (ErrBadArgs → exit 2).
func errControlDialogUp() error {
	return fmt.Errorf("%w: session is showing a one-time trust/bypass dialog; accept it first", ErrBadArgs)
}

// pollDelay is defaultPollEvery CLAMPED to what is left before the deadline, so the
// wait loop leaves AT the deadline instead of up to a full poll past it (and then
// adding promptDeliverSettle on top of the overshoot).
func pollDelay(now func() time.Time, deadline time.Time) time.Duration {
	left := deadline.Sub(now())
	switch {
	case left <= 0:
		return 0
	case left < defaultPollEvery:
		return left
	default:
		return defaultPollEvery
	}
}

// isTrustDialog reports whether the pane is showing this kind's one-time
// directory/workspace-trust dialog — claude's (IsTrustPrompt) or codex's
// (IsCodexTrustPrompt). Both are CONTROL matchers, the last pane-reading left in the
// wait path after S2 (charliek/shed#324) deleted the classifiers, and both dialogs
// pre-select "yes", so a single Enter accepts either.
//
// KIND-GATED, deliberately: this decides whether to SEND A KEYSTROKE, and a
// look-alike phrase in a cursor/opencode transcript must never draw one. cursor
// launches with --trust and has no dialog at all.
func isTrustDialog(kind Kind, pane string) bool {
	switch {
	case IsClaudeKind(kind):
		return IsTrustPrompt(pane)
	case kind == KindCodex:
		return IsCodexTrustPrompt(pane)
	default:
		return false
	}
}

// waitUntilLive polls the pane until the session is LIVE (a capture succeeds) or the
// deadline passes, accepting the one-time control dialogs on the way, then delivers
// prompt. The returned error is non-nil only for a kickoff-delivery failure — a
// session that never came up is a result, not an error.
//
// Since S2 (charliek/shed#324) there is no classifier here: a successful capture IS
// the ready signal, and a missing tmux session is the only dead one. What the pane is
// still read for is CONTROL — claude's bypass-acceptance dialog, claude's and codex's
// trust dialogs, and the claude.ai remote-control URL.
//
// WITHOUT a prompt the loop returns on the first successful capture (after examining
// that capture for the control dialogs) — there is nothing to settle for. WITH a
// prompt it returns once kickoffSettle has elapsed since the origin (see the
// constant), bounded by the deadline.
//
// now is injected beside sleep so the settle arithmetic is unit-testable on a fake
// clock (nil → time.Now / time.Sleep).
func waitUntilLive(r Runner, name string, kind Kind, prompt string, bypass bool, sleep func(time.Duration), now func() time.Time) (State, string, error) {
	if sleep == nil {
		sleep = time.Sleep
	}
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(defaultWaitTimeout)
	state, url := StateStarting, ""
	trustAccepted := false
	bypassAccepted := false
	// origin is the settle window's start: the first successful capture, pushed
	// forward by each SUCCESSFUL control keystroke. Zero until the first capture.
	var origin time.Time
	for now().Before(deadline) {
		capRes := capturePane(r, name)
		if capRes.Code != 0 {
			// The session is gone (the inner command exited immediately) — report
			// dead now rather than polling empty output until the deadline.
			if isMissingSession(capRes.Stderr) {
				return StateDead, "", nil
			}
			sleep(pollDelay(now, deadline)) // transient capture error; keep polling
			continue
		}
		// A capture succeeded: the session exists and is drawing. That is liveness,
		// and liveness is the whole of `ready` now.
		state = StateReady
		url = extractURL(kind, capRes.Stdout)
		if origin.IsZero() {
			origin = now()
		}
		// A bypassPermissions session shows a one-time acceptance dialog before
		// anything else; accept it once so the session can proceed unattended. Gated
		// on bypass so a look-alike screen never draws a stray keypress otherwise.
		if bypass && IsClaudeKind(kind) && !bypassAccepted && IsBypassAcceptPrompt(capRes.Stdout) {
			// Only latch accepted on a successful send; a transient send-keys failure
			// must remain retryable rather than stalling the session until timeout.
			// The settle origin moves only on that same success — a failed keystroke
			// changed nothing on screen.
			if res := acceptBypassPrompt(r, name); res.Code == 0 {
				bypassAccepted = true
				origin = now()
			}
			sleep(pollDelay(now, deadline))
			continue
		}
		if !trustAccepted && isTrustDialog(kind, capRes.Stdout) {
			// Both captured trust gates pre-select "yes", so a single Enter accepts
			// either. Latched ONLY on a successful send, exactly like the bypass arm
			// above: a transient send-keys failure that left the dialog up must stay
			// retryable, because a latch there would make every later capture ignore
			// a modal that still owns the keyboard. The settle origin moves on that
			// same success — a failed keystroke changed nothing on screen.
			if res := sendEnter(r, name); res.Code == 0 {
				trustAccepted = true
				origin = now()
			}
			sleep(pollDelay(now, deadline))
			continue
		}
		if prompt == "" {
			break // nothing to settle for
		}
		if !now().Before(origin.Add(kickoffSettle)) {
			break // the settle window has elapsed
		}
		sleep(pollDelay(now, deadline))
	}
	if state == StateReady && prompt != "" {
		// One more plain settle (it inspects nothing) before the kickoff line is
		// typed. A delivery failure is surfaced — otherwise a create --wait would
		// report ready with the kickoff never typed (and a plan run would exit 0 with
		// the plan unstarted).
		sleep(promptDeliverSettle)
		// THE DELIVERY GATE. Liveness is not permission to type: the loop above can
		// exit with a modal still on screen — an accept whose keystroke failed every
		// time, a bypass dialog the deadline ran out under, a dialog that reappeared
		// after the settle — and a kickoff typed there answers it by accident. So
		// delivery re-checks the pane itself, with the same matchers and the same
		// refusal the one-shot `prompt` verb makes, rather than trusting the loop's
		// bookkeeping. A transient capture failure is NO EVIDENCE, not proof of a
		// dialog, so it falls through to the send (whose own failure is surfaced).
		if capRes := capturePane(r, name); capRes.Code != 0 {
			if isMissingSession(capRes.Stderr) {
				return StateDead, "", nil
			}
		} else if hasControlDialog(capRes.Stdout) {
			return state, url, errControlDialogUp()
		}
		if res := sendLine(r, name, prompt); res.Code != 0 {
			if isMissingSession(res.Stderr) {
				// Killed between the last poll and delivery: that's a dead session,
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

// Prompt delivers a single line to a live session (re-captures and verifies kind +
// the control gate + optional session-id before sending).
//
// THE GATE IS CONTROL, NOT STATUS (S2, charliek/shed#324). It used to refuse anything
// the classifier did not call `ready`; with `state` reduced to liveness that check
// would always pass and the verb would type blind. What it refuses instead is a pane
// with a one-time dialog on it — claude's or codex's trust dialog, claude's bypass
// acceptance — because a line typed there answers the dialog by accident. Unlike
// waitUntilLive's accept path this is NOT kind-gated: refusing is the safe direction,
// so every kept matcher is consulted for every kind.
//
// Everything else is DELIVERY OF TERMINAL INPUT INTO A LIVE SESSION — the same thing
// a person typing at the attached terminal does. The hub cannot tell an auth screen
// or an approval modal from a composer any more, and that hazard is accepted (and
// documented in docs/extensions/rc-helper.md) until roost owns the guest's kickoff.
func Prompt(r Runner, opts PromptOptions) error {
	opts.Text = NormalizeNewlines(opts.Text)
	if HasUnsafePromptChars(opts.Text) {
		return fmt.Errorf("%w: text contains an unsupported control character", ErrBadArgs)
	}
	name := TmuxName(opts.Slug)
	pane, err := capturePaneChecked(r, name)
	if err != nil {
		return err
	}
	session := ParseSession(name, showEnvironment(r, name), pane, nil)
	if opts.SessionID != "" && session.ID != opts.SessionID {
		return fmt.Errorf("%w: session id mismatch (recreated?)", ErrSessionNotFound)
	}
	if !AcceptsTypedInput(session.Kind) {
		return fmt.Errorf("%w: kind %q does not accept a prompt", ErrBadArgs, session.Kind)
	}
	if hasControlDialog(pane) {
		return errControlDialogUp()
	}
	// Surface a delivery failure (e.g. the session was killed between the check and
	// the send) instead of reporting a false success.
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

// ToolFor returns the human-facing tool token backing a kind's sessions ("claude",
// "codex", "opencode", "cursor", "shell"), or "the agent" for an unregistered kind.
// Registry-sourced so a CLI's needs-auth guidance names the actual tool instead of
// hand-hardcoding "Claude" (see cmd/shed/attach.go's reportRCCreateOutcome).
func ToolFor(k Kind) string {
	if spec, ok := specForKind(k); ok && spec.Tool != "" {
		return spec.Tool
	}
	return "the agent"
}

// expandTilde expands a leading ~ against home: "~" -> home, "~/x" -> home/x.
// Anything else — including ~user (this is not a shell and has no passwd lookup)
// and a bare relative path — is returned unchanged.
func expandTilde(dir, home string) string {
	if home == "" {
		return dir
	}
	if dir == "~" {
		return home
	}
	if strings.HasPrefix(dir, "~/") {
		return strings.TrimSuffix(home, "/") + "/" + dir[2:]
	}
	return dir
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
