package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
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
	attachPromptFlag     string
	attachPromptFileFlag string
	attachEditFlag       bool
	attachPlanFlag       string
	attachPlanEditFlag   bool
	attachPermModeFlag   string
	attachSkipFlag       bool
	attachDetachFlag     bool
)

// validRCKinds / validRCPermModes mirror shed-ext-rc's accepted values so the CLI
// can reject a bad flag before the SSH round-trip; shed-ext-rc re-validates as the
// source of truth. Keep in sync with shed-extensions internal/rc.
var validRCKinds = map[string]bool{"claude-rc": true, "claude-broker": true, "shell": true}
var validRCPermModes = map[string]bool{
	"default": true, "acceptEdits": true, "plan": true,
	"auto": true, "dontAsk": true, "bypassPermissions": true,
}
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

	attachCmd.Flags().StringVar(&attachKindFlag, "kind", "", "RC session kind: claude-rc|claude-broker|shell (triggers RC create)")
	attachCmd.Flags().StringVar(&attachNameFlag, "name", "", "RC session display name (default <shed>/<slug>)")
	attachCmd.Flags().StringVar(&attachSlugFlag, "slug", "", "RC slug: connect to rc-<slug>, or set the slug for a new session")
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
// modifiers (--skip, --permission-mode, --name, -d/--detach) also trigger RC create
// rather than silently routing to plain attach and discarding the user's intent.
func rcCreateRequested() bool {
	return attachKindFlag != "" || rcInputRequested() ||
		attachSkipFlag || attachPermModeFlag != "" || attachNameFlag != "" || attachDetachFlag
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
		if !validRCKinds[kind] {
			return fmt.Errorf("invalid --kind %q (want claude-rc|claude-broker|shell)", kind)
		}
		if attachSkipFlag && attachPermModeFlag != "" {
			return fmt.Errorf("--skip and --permission-mode are mutually exclusive")
		}
		if attachPermModeFlag != "" && !validRCPermModes[attachPermModeFlag] {
			return fmt.Errorf("invalid --permission-mode %q", attachPermModeFlag)
		}
		if kind == "claude-broker" && rcInputRequested() {
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
	client := NewAPIClientFromEntry(entry, clientConfig.GetCreateTimeout())
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
		sessions, err := listShedSessions(entry, name)
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
	explicitMode := attachSkipFlag || attachPermModeFlag != ""
	mode := attachPermModeFlag
	if attachSkipFlag {
		mode = "bypassPermissions"
	}
	if mode == "" {
		mode = "auto"
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

	// Ship the plan and default the kickoff prompt to reference it.
	if havePlan {
		relPath, perr := streamPlanToShed(name, entry, slug, planContent)
		if perr != nil {
			return perr
		}
		if verboseLevel > 0 {
			fmt.Printf("Shipped plan to %s in %s\n", relPath, name)
		}
		if prompt == "" {
			prompt = "Read and execute the plan at " + relPath + " autonomously to completion. Do not ask for confirmation; make reasonable decisions and keep going until the plan is done."
		}
	}

	if verboseLevel > 0 {
		fmt.Printf("Creating %s session rc-%s in %s on %s (mode=%s)...\n", kind, slug, name, serverName, modeLabel(mode))
	}
	opts := rcCreateOptions{
		shedName:       name,
		entry:          entry,
		kind:           kind,
		displayName:    displayName,
		slug:           slug,
		permissionMode: mode,
		prompt:         prompt,
	}
	dto, err := createRCSession(opts)
	if isOldBinaryPermModeErr(err) {
		// Image predates --permission-mode. Fail loudly if the user asked for a
		// posture; otherwise drop the default and retry so the session still comes up.
		// (create rejects the flag before making the tmux session, so retry is clean.)
		if explicitMode {
			return fmt.Errorf("this shed's shed-ext-rc predates --permission-mode/--skip; upgrade the shed image, or omit it to start without an autonomous posture")
		}
		fmt.Fprintln(os.Stderr, "Warning: shed-ext-rc predates --permission-mode; starting without an autonomous posture (upgrade the shed image to enable).")
		opts.permissionMode = ""
		dto, err = createRCSession(opts)
	}
	if err != nil {
		return err
	}

	switch dto.State {
	case "needs-auth":
		fmt.Printf("Session rc-%s created but Claude is not logged in in this shed.\n", slug)
		fmt.Printf("Log in once: shed attach %s  →  run `claude` →  /login, then retry.\n", name)
		return nil
	case "needs-trust":
		fmt.Printf("Session rc-%s created but the workspace trust prompt is showing; attach to accept it:\n  shed attach %s --slug %s\n", slug, name, slug)
		return nil
	}

	if attachDetachFlag || kind == "claude-broker" {
		printRCSummary(name, dto)
		return nil
	}
	return attachToRCSlug(name, entry, slug)
}

func modeLabel(mode string) string {
	if mode == "" {
		return "default (no posture)"
	}
	return mode
}

func printRCSummary(shedName string, dto rcSessionDTO) {
	fmt.Printf("Started %s session rc-%s (%s)\n", dto.Kind, dto.Slug, dto.State)
	if dto.URL != "" {
		fmt.Printf("  URL:    %s\n", dto.URL)
	}
	fmt.Printf("  Watch:  shed attach %s --slug %s\n", shedName, dto.Slug)
	fmt.Printf("  Status: shed sessions %s\n", shedName)
}

// listShedSessions is a thin wrapper so attachPlain can list sessions for the
// --new guard without reaching for a package-level client.
func listShedSessions(entry *config.ServerEntry, name string) ([]config.Session, error) {
	return NewAPIClientFromEntry(entry, DefaultTimeout).ListSessions(name)
}
