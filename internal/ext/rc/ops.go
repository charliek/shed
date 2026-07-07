package rc

import (
	"errors"
	"fmt"
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
	Kind             Kind
	DisplayName      string // defaults to the slug
	Slug             string // optional; generated when empty
	Workdir          string // optional; defaults to $SHED_WORKSPACE
	CreatedBy        string // optional; defaults to ToolName
	Target           string // optional advisory label
	Prompt           string // optional kickoff line (implies Wait)
	Wait             bool   // block until ready, accept trust, deliver prompt
	InteractiveShell bool   // wrap claude kinds in `bash -ic` (native machines)
	// PermissionMode sets claude's --permission-mode for claude kinds ("" = omit,
	// claude's own default). e.g. "auto" or "bypassPermissions" for an unattended
	// run; with bypassPermissions, Wait also auto-accepts the one-time bypass dialog.
	PermissionMode string
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
	if opts.PermissionMode != "" {
		if !IsClaudeKind(opts.Kind) {
			return Session{}, fmt.Errorf("%w: --permission-mode applies only to claude kinds", ErrBadArgs)
		}
		if !ValidPermissionMode(opts.PermissionMode) {
			return Session{}, fmt.Errorf("%w: invalid permission mode %q", ErrBadArgs, opts.PermissionMode)
		}
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
	meta := Metadata{
		ID:          uuid.NewString(),
		DisplayName: displayName,
		Kind:        opts.Kind,
		Workdir:     workdir,
		CreatedBy:   createdBy,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Target:      opts.Target,
	}
	envArgs, err := BuildEnvArgs(meta)
	if err != nil {
		return Session{}, fmt.Errorf("%w: %v", ErrBadArgs, err)
	}

	// Best-effort trust pre-seed for claude kinds (the accept-trust fallback covers
	// any failure, so a preseed error never fails the create).
	if IsClaudeKind(opts.Kind) {
		_ = PreseedClaudeConfig(workdir, env)
	}

	inner := InnerCommand(opts.Kind, displayName, opts.PermissionMode, opts.InteractiveShell)
	res := createSession(r, name, workdir, envArgs, inner)
	if res.Code != 0 {
		if isDuplicateSession(res.Stderr) {
			return Session{}, fmt.Errorf("%w: %s", ErrDuplicateSlug, name)
		}
		return Session{}, fmt.Errorf("tmux new-session failed: %s", strings.TrimSpace(res.Stderr+res.Stdout))
	}

	session := Session{
		Slug:        slug,
		TmuxSession: name,
		Kind:        opts.Kind,
		State:       StateStarting,
		Managed:     true,
		DisplayName: displayName,
		Workdir:     workdir,
		ID:          meta.ID,
		CreatedBy:   createdBy,
		CreatedAt:   meta.CreatedAt,
		TargetLabel: opts.Target,
	}

	if opts.Wait || opts.Prompt != "" {
		bypass := opts.PermissionMode == PermissionModeBypass
		state, url := waitUntilReady(r, name, opts.Kind, opts.Prompt, bypass, sleep)
		session.State, session.URL = state, url
	}
	return session, nil
}

// waitUntilReady polls the pane until a terminal state (or timeout), auto-accepting
// the trust prompt once, then delivers prompt if the session reached ready.
func waitUntilReady(r Runner, name string, kind Kind, prompt string, bypass bool, sleep func(time.Duration)) (State, string) {
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
				return StateDead, ""
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
		if state == StateNeedsTrust && IsClaudeKind(kind) && !trustAccepted {
			trustAccepted = true
			if IsTrustPrompt(capRes.Stdout) {
				sendEnter(r, name)
			}
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
		// input; settle once more before typing the kickoff line.
		sleep(promptDeliverSettle)
		sendLine(r, name, prompt)
	}
	return state, url
}

// List returns every rc-* session's DTO. displayFallback receives a slug.
func List(r Runner, displayFallback func(slug string) string) ListResponse {
	names := listSessionNames(r)
	sessions := make([]Session, 0, len(names))
	for _, name := range names {
		env := showEnvironment(r, name)
		pane := capturePane(r, name).Stdout
		sessions = append(sessions, ParseSession(name, env, pane, displayFallback))
	}
	return ListResponse{RCSessions: sessions}
}

// capturePaneChecked returns a session's pane text, mapping a gone session to
// ErrSessionNotFound (shared by probe/prompt/accept-trust).
func capturePaneChecked(r Runner, name string) (string, error) {
	res := capturePane(r, name)
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
