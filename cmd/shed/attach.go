package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/ext/rc"
)

var (
	attachSessionFlag string
	attachNewFlag     bool

	// Remote Control (RC) flags. When any create flag is set, `attach` creates an
	// rc-<slug> session via the in-shed shed-ext-rc binary instead of a plain tmux
	// session. --slug alone (no create flags) connects to an existing rc-<slug>.
	attachKindFlag       string
	attachNameFlag       string
	attachSlugFlag       string
	attachWorkdirFlag    string
	attachPromptFlag     string
	attachPromptFileFlag string
	attachEditFlag       bool
	attachPlanFlag       string
	attachPlanEditFlag   bool
	attachPermModeFlag   string
	attachSkipFlag       bool
	attachDetachFlag     bool
)

var rcSlugRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

var attachCmd = &cobra.Command{
	Use:   "attach <name>",
	Short: "Attach to a tmux session in a shed (or start a Remote Control session)",
	Long: `Attach to a tmux session in a shed container.

By default, attaches to or creates a session named "default" and drops you into it
(tmux gives you detach/reconnect persistence).

Remote Control (autonomous agent) sessions:
  With --kind (or --plan/--prompt), attach instead creates an rc-<slug> Remote
  Control session via the in-shed shed-ext-rc binary: it starts claude connected to
  claude.ai, ships an optional plan, and types a kickoff prompt. With -d/--detach it
  prints the session URL and returns (laptop can close); otherwise it attaches.
  Permission posture defaults to "auto"; use --skip for full bypass.

Examples:
  shed attach myproj                         # attach/create the "default" tmux session
  shed attach myproj --session debug         # a named tmux session
  shed attach myproj --kind claude-rc -d     # start an RC agent, print the URL, return
  shed attach myproj --plan plan.md -d       # ship a plan and run it autonomously (auto)
  shed attach myproj --plan plan.md --skip -d  # ...with full permission bypass
  shed attach myproj --slug abc234           # attach to an existing rc-abc234 session`,
	Args: cobra.ExactArgs(1),
	RunE: runAttach,
}

func init() {
	attachCmd.Flags().StringVarP(&attachSessionFlag, "session", "S", config.DefaultSessionName, "Session name to attach to (plain mode)")
	attachCmd.Flags().BoolVar(&attachNewFlag, "new", false, "Force create a new session (error if exists)")

	attachCmd.Flags().StringVar(&attachKindFlag, "kind", "", "RC session kind ("+strings.Join(rc.KindStrings(), "|")+"); triggers RC create")
	attachCmd.Flags().StringVar(&attachNameFlag, "name", "", "RC session display name (default <shed>/<slug>)")
	attachCmd.Flags().StringVar(&attachSlugFlag, "slug", "", "RC slug: connect to rc-<slug>, or set the slug for a new session")
	attachCmd.Flags().StringVar(&attachWorkdirFlag, "workdir", "", "Working directory inside the shed for the RC session (default $SHED_WORKSPACE/$HOME)")
	attachCmd.Flags().StringVarP(&attachPromptFlag, "prompt", "p", "", "Kickoff prompt (use --prompt-file/--edit for multi-line)")
	attachCmd.Flags().StringVar(&attachPromptFileFlag, "prompt-file", "", "Read the kickoff prompt from a file (- for stdin)")
	attachCmd.Flags().BoolVar(&attachEditFlag, "edit", false, "Compose the kickoff prompt in $EDITOR")
	attachCmd.Flags().StringVar(&attachPlanFlag, "plan", "", "Ship a plan file to the shed and execute it (- for stdin)")
	attachCmd.Flags().BoolVar(&attachPlanEditFlag, "plan-edit", false, "Compose the plan in $EDITOR")
	attachCmd.Flags().StringVar(&attachPermModeFlag, "permission-mode", "", "Claude permission mode (default auto): acceptEdits|auto|bypassPermissions|default|dontAsk|plan")
	attachCmd.Flags().BoolVar(&attachSkipFlag, "skip", false, "Shorthand for --permission-mode bypassPermissions")
	attachCmd.Flags().BoolVarP(&attachDetachFlag, "detach", "d", false, "Create the RC session and print its URL without attaching")

	rootCmd.AddCommand(attachCmd)
}

// rcCreateRequested reports whether any RC-only flag is set — so posture/presentation
// modifiers (--skip, --permission-mode, --name, -d/--detach, --workdir) also trigger RC
// create rather than silently routing to plain attach and discarding the user's intent.
func rcCreateRequested() bool {
	return attachKindFlag != "" || rcInputRequested() ||
		attachSkipFlag || attachPermModeFlag != "" || attachNameFlag != "" || attachDetachFlag ||
		attachWorkdirFlag != ""
}

// rcInputRequested reports whether any prompt/plan input flag is set.
func rcInputRequested() bool {
	return attachPromptFlag != "" || attachPromptFileFlag != "" || attachEditFlag ||
		attachPlanFlag != "" || attachPlanEditFlag
}

// validateAttachFlags runs cheap, mode-specific validation before the shed is found
// or auto-started — so a bad invocation fails fast without side effects (no
// auto-start, no $EDITOR, no SSH).
func validateAttachFlags() error {
	switch {
	case rcCreateRequested():
		kind := attachKindFlag
		if kind == "" {
			kind = "claude-rc"
		}
		if err := validateRCKind(kind); err != nil {
			return err
		}
		if _, err := resolveRCPermMode(kind, attachPermModeFlag, attachSkipFlag, rc.PermModeAuto); err != nil {
			return err
		}
		if kind == string(rc.KindClaudeBroker) && rcInputRequested() {
			return fmt.Errorf("claude-broker sessions are driven from claude.ai and take no prompt/plan")
		}
		if attachSlugFlag != "" && !rcSlugRe.MatchString(attachSlugFlag) {
			return fmt.Errorf("invalid slug %q", attachSlugFlag)
		}
	case attachSlugFlag != "":
		if !rcSlugRe.MatchString(attachSlugFlag) {
			return fmt.Errorf("invalid slug %q", attachSlugFlag)
		}
	default:
		if err := config.ValidateSessionName(attachSessionFlag); err != nil {
			return fmt.Errorf("invalid session name: %w", err)
		}
	}
	return nil
}

func runAttach(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Validate flags before touching the network, so a bad invocation fails fast
	// without auto-starting a stopped shed (or opening $EDITOR) just to error.
	if err := validateAttachFlags(); err != nil {
		return err
	}

	serverName, entry, err := findShedServer(name)
	if err != nil {
		return err
	}
	client := NewAPIClientFromNamedEntry(serverName, entry, clientConfig.GetCreateTimeout())
	shed, err := ensureRunningShed(client, name)
	if err != nil {
		return err
	}

	switch {
	case rcCreateRequested():
		return runAttachRCCreate(name, serverName, entry)
	case attachSlugFlag != "":
		return attachToRCSlug(name, entry, attachSlugFlag)
	default:
		return attachPlain(name, serverName, entry, shed)
	}
}

// attachPlain is the original behavior: attach to (or create) a named tmux session.
// (The session name was validated in validateAttachFlags.)
func attachPlain(name, serverName string, entry *config.ServerEntry, shed *config.Shed) error {
	if attachNewFlag {
		sessions, err := listShedSessions(serverName, entry, name)
		if err != nil {
			return fmt.Errorf("failed to check existing sessions: %w", err)
		}
		for _, s := range sessions {
			if s.Name == attachSessionFlag {
				return fmt.Errorf("session %q already exists (use without --new to attach)", attachSessionFlag)
			}
		}
	}
	if verboseLevel > 0 {
		fmt.Printf("Attaching to session %q in %s on %s...\n", attachSessionFlag, name, serverName)
	}
	landingDir := shed.LandingDir
	if landingDir == "" {
		landingDir = config.HomePath
	}
	var tmuxCmd string
	if attachNewFlag {
		tmuxCmd = fmt.Sprintf("tmux new-session -s %s -c %s", attachSessionFlag, shellQuoteArg(landingDir))
	} else {
		tmuxCmd = fmt.Sprintf("tmux new-session -A -s %s -c %s", attachSessionFlag, shellQuoteArg(landingDir))
	}
	return execSSHTmux(name, entry, tmuxCmd)
}

// attachToRCSlug attaches the terminal to an existing rc-<slug> session.
func attachToRCSlug(name string, entry *config.ServerEntry, slug string) error {
	if !rcSlugRe.MatchString(slug) {
		return fmt.Errorf("invalid slug %q", slug)
	}
	return execSSHTmux(name, entry, "tmux attach -t "+rcTmuxPrefix+slug)
}

// execSSHTmux replaces this process with `ssh -t … <tmuxCmd>` (the plain/connect
// paths). Interactive, so it intentionally omits BatchMode/ConnectTimeout (an
// interactive session may legitimately prompt); the rest is shared via baseSSHArgs.
func execSSHTmux(name string, entry *config.ServerEntry, tmuxCmd string) error {
	sshArgs := append([]string{"ssh", "-t"}, baseSSHArgs(name, entry)...)
	sshArgs = append(sshArgs, "--", tmuxCmd)
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found in PATH: %w", err)
	}
	if err := syscall.Exec(sshPath, sshArgs, os.Environ()); err != nil {
		return fmt.Errorf("failed to exec ssh: %w", err)
	}
	return nil
}

// validateRCKind rejects a --kind the current registry doesn't know, before any SSH
// round-trip; shed-ext-rc re-validates as the source of truth, and rc.IsValidKind reads
// the same registry so this never drifts. Shared by `shed attach` and `shed plan`.
func validateRCKind(kind string) error {
	if !rc.IsValidKind(rc.Kind(kind)) {
		return fmt.Errorf("invalid --kind %q (want %s)", kind, strings.Join(rc.KindStrings(), "|"))
	}
	return nil
}

// resolveRCPermMode applies the --skip shorthand and the --skip/--permission-mode
// mutual exclusion, validates the resolved mode against kind's registry
// (rc.PermModeAcceptedBy), and falls back to dflt when neither flag is given ("" dflt
// means "no posture flag"). Shared by `shed attach` and `shed plan` so the two
// commands' permission-mode handling — and its validation — can't drift apart.
func resolveRCPermMode(kind, permMode string, skip bool, dflt string) (string, error) {
	if skip && permMode != "" {
		return "", fmt.Errorf("--skip and --permission-mode are mutually exclusive")
	}
	mode := permMode
	if skip {
		mode = rc.PermModeSkip
	}
	if mode == "" {
		mode = dflt
	}
	if mode != "" && !rc.PermModeAcceptedBy(rc.Kind(kind), mode) {
		return "", fmt.Errorf("invalid --permission-mode %q for --kind %s (all kinds accept default|auto|skip; claude also accepts acceptEdits|plan|dontAsk|bypassPermissions)", mode, kind)
	}
	return mode, nil
}

// runAttachRCCreate creates an rc-<slug> Remote Control session via shed-ext-rc:
// resolves prompt/plan, ships the plan, sets the permission posture, starts the
// session, then prints the URL (--detach) or attaches.
func runAttachRCCreate(name, serverName string, entry *config.ServerEntry) error {
	// Kind, posture mutual-exclusivity/validity, and broker/input rules were
	// validated in validateAttachFlags; here we just derive the effective values.
	kind := attachKindFlag
	if kind == "" {
		kind = "claude-rc"
	}
	// Already validated in validateAttachFlags; the error here is unreachable in
	// practice but handled rather than ignored.
	mode, err := resolveRCPermMode(kind, attachPermModeFlag, attachSkipFlag, rc.PermModeAuto)
	if err != nil {
		return err
	}

	prompt, planContent, havePlan, err := resolveRCInputs(rcInputs{
		prompt:     attachPromptFlag,
		promptFile: attachPromptFileFlag,
		edit:       attachEditFlag,
		plan:       attachPlanFlag,
		planEdit:   attachPlanEditFlag,
	})
	if err != nil {
		return err
	}

	slug := attachSlugFlag
	if slug == "" {
		if slug, err = genRCSlug(); err != nil {
			return err
		}
	}
	displayName := attachNameFlag
	if displayName == "" {
		displayName = name + "/" + slug
	}

	if verboseLevel > 0 {
		fmt.Printf("Creating %s session rc-%s in %s on %s (mode=%s)...\n", kind, slug, name, serverName, modeLabel(mode))
	}
	// A plan is piped to the guest via --plan-stdin: shed-ext-rc writes it to the
	// per-kind HOME-rooted location and composes the kickoff (any -p/--prompt-file
	// value becomes the framing prepended to it). Without a plan, a bare prompt is
	// delivered directly. Delivery + composition now live in the rc core, shared by
	// every orchestrator — the CLI only routes the inputs.
	opts := rcCreateOptions{
		shedName:       name,
		entry:          entry,
		kind:           kind,
		displayName:    displayName,
		slug:           slug,
		workdir:        attachWorkdirFlag,
		permissionMode: mode,
	}
	if havePlan {
		opts.plan = planContent
		opts.planFraming = prompt
	} else {
		opts.prompt = prompt
	}
	dto, err := createRCSession(opts)
	if err != nil {
		if isOldBinaryRCErr(err) {
			// The shed's baked-in shed-ext-rc rejected our create request (an unknown
			// --kind or flag): its image predates multi-agent RC. One clear message —
			// recreate the shed to pick up the new agent kinds and permission modes.
			return fmt.Errorf("this shed's image predates multi-agent RC (its shed-ext-rc rejected --kind %s / permission mode); recreate the shed to use the new agent kinds", kind)
		}
		return err
	}

	if handled, err := reportRCCreateOutcome(os.Stdout, name, slug, kind, dto); handled {
		return err
	}

	if attachDetachFlag || kind == string(rc.KindClaudeBroker) {
		printRCSummary(name, dto)
		return nil
	}
	return attachToRCSlug(name, entry, slug)
}

// reportRCCreateOutcome prints the guidance for a terminal RC-create state and reports
// whether the caller is done. handled=true means "return err" (the session is either
// left running for the user to act on, or dead); handled=false means the session is live
// (ready/starting) and the caller proceeds to attach or print its summary.
//
// This encodes attach's OWN exit contract: needs-auth / needs-trust leave the session
// running and exit 0 (the create succeeded interactively; the user just has to log in /
// accept trust and reattach), while a dead-on-create is a session-level FAILURE that
// must exit non-zero — a bad posture flag or missing runtime that killed the agent must
// never read as success to a script or the -d path. This DELIBERATELY DIFFERS from
// `shed plan`'s reportPlanOutcome, which exits non-zero on needs-auth/needs-trust too:
// attach is interactive (a human is there to notice and fix it), while plan is meant to
// run unattended, so a session stuck on auth/trust IS a plan-run failure, not attach's
// "created, just finish the login" success.
func reportRCCreateOutcome(w io.Writer, name, slug, kind string, dto rc.Session) (bool, error) {
	switch dto.State {
	case rc.StateNeedsAuth:
		// Per-agent remediation: both the tool name and the login flow (claude's
		// /login, codex login, opencode auth login, cursor-agent login) come from the
		// agent registry — no hand-hardcoded "Claude".
		fmt.Fprintf(w, "Session rc-%s created but %s is not logged in in this shed.\n", slug, rc.ToolFor(rc.Kind(kind)))
		fmt.Fprintf(w, "Log in once: shed attach %s  →  %s, then retry.\n", name, rc.AuthHintFor(rc.Kind(kind)))
		return true, nil
	case rc.StateNeedsTrust:
		fmt.Fprintf(w, "Session rc-%s created but the workspace trust prompt is showing; attach to accept it:\n  shed attach %s --slug %s\n", slug, name, slug)
		return true, nil
	case rc.StateDead:
		fmt.Fprintf(w, "Session rc-%s was created but its agent process died immediately (state=dead).\n", slug)
		fmt.Fprintf(w, "  Inspect: shed attach %s --slug %s\n", name, slug)
		return true, fmt.Errorf("session rc-%s died on create (state=dead)", slug)
	}
	return false, nil
}

func modeLabel(mode string) string {
	if mode == "" {
		return "default (no posture)"
	}
	return mode
}

func printRCSummary(shedName string, dto rc.Session) {
	fmt.Printf("Started %s session rc-%s (%s)\n", dto.Kind, dto.Slug, dto.State)
	printRCFollowups(shedName, dto)
}

// printRCFollowups prints the URL (when present) and Watch/Status follow-up lines
// shared by every "session started" summary (`shed attach` and `shed plan`).
func printRCFollowups(shedName string, dto rc.Session) {
	if dto.URL != "" {
		fmt.Printf("  URL:    %s\n", dto.URL)
	}
	fmt.Printf("  Watch:  shed attach %s --slug %s\n", shedName, dto.Slug)
	fmt.Printf("  Status: shed sessions %s\n", shedName)
}

// listShedSessions is a thin wrapper so attachPlain can list sessions for the
// --new guard without reaching for a package-level client. Only the session rows
// are needed here (a name-existence check); warnings are for `shed sessions`.
func listShedSessions(serverName string, entry *config.ServerEntry, name string) ([]config.Session, error) {
	resp, err := NewAPIClientFromNamedEntry(serverName, entry, DefaultTimeout).ListSessions(name)
	if err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}
