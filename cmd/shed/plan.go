package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/ext/rc"
)

var (
	planShedFlag    string
	planRepoFlag    string
	planKindFlag    string
	planFramingFlag string
	planDetachFlag  bool
)

var planCmd = &cobra.Command{
	Use:   "plan <file>",
	Short: "Ship a plan file to a shed and run it autonomously",
	Long: `Ship a plan to a Remote Control agent session in a shed and run it.

This collapses the multi-step "create a shed, ship the plan, start the agent,
watch it" flow into one command. The plan file (or - for stdin) is written to a
per-kind location inside the shed (never the workspace), an agent session is
started under an autonomous permission posture, and a kickoff referencing the
plan is delivered.

With --repo the shed is created if it doesn't exist yet; without --repo a
missing shed is an error (and --repo is ignored for a shed that already exists).

Exit status is 0 only when the session reaches "ready" and the kickoff is
delivered; a session left at needs-auth / needs-trust (or a plan that could not
be shipped) exits non-zero and leaves the session/shed in place for you to fix.

Examples:
  shed plan ./plan.md --shed my-topic                     # existing shed
  shed plan ./plan.md --shed my-topic --repo owner/repo   # create if missing
  shed plan ./plan.md --shed my-topic --kind codex -d     # a codex session, detached
  shed plan ./plan.md --shed my-topic -p "focus on the API layer first"`,
	Args: cobra.ExactArgs(1),
	RunE: runPlan,
}

func init() {
	planCmd.Flags().StringVar(&planShedFlag, "shed", "", "Target shed name (required)")
	planCmd.Flags().StringVar(&planRepoFlag, "repo", "", "Create the shed from this repo (owner/repo or URL) if it doesn't exist")
	planCmd.Flags().StringVar(&planKindFlag, "kind", "", "Agent kind ("+strings.Join(rc.KindStrings(), "|")+"); default claude-rc")
	planCmd.Flags().StringVarP(&planFramingFlag, "prompt", "p", "", "Optional framing prepended to the composed plan kickoff")
	planCmd.Flags().BoolVarP(&planDetachFlag, "detach", "d", false, "Report the session and return instead of attaching when it is ready")

	rootCmd.AddCommand(planCmd)
}

func runPlan(cmd *cobra.Command, args []string) error {
	planFile := args[0]

	kind := planKindFlag
	if kind == "" {
		kind = "claude-rc"
	}
	if err := validateRCKind(kind); err != nil {
		return err
	}
	if !rc.AcceptsTypedInput(rc.Kind(kind)) {
		return fmt.Errorf("--kind %s does not accept a plan (it is driven from elsewhere)", kind)
	}
	if planShedFlag == "" {
		return fmt.Errorf("--shed <name> is required")
	}

	// Read + validate the plan before any side effect, so an empty/oversized/binary
	// plan never creates a shed or a session.
	planContent, err := readPlanArg(planFile)
	if err != nil {
		return err
	}

	serverName, entry, found := locateShed(planShedFlag)
	shedCreated := false
	switch decidePlanShed(found, planRepoFlag) {
	case planErrorMissingNoRepo:
		return fmt.Errorf("shed %q not found; pass --repo owner/repo to create it, or run `shed create %s` first", planShedFlag, planShedFlag)
	case planCreateMissing:
		if !jsonFlag {
			fmt.Printf("Shed %s not found; creating it from %s...\n", planShedFlag, planRepoFlag)
		}
		// Reuse the `shed create` pathway verbatim (backend resolution, progress,
		// caching, auto-sync) by driving its RunE with the repo set.
		createRepo = planRepoFlag
		if cerr := runCreate(createCmd, []string{planShedFlag}); cerr != nil {
			return fmt.Errorf("creating shed %q: %w", planShedFlag, cerr)
		}
		shedCreated = true
		if serverName, entry, found = locateShed(planShedFlag); !found {
			return fmt.Errorf("shed %q was created but could not be located afterward", planShedFlag)
		}
	case planWarnIgnoreRepo:
		fmt.Fprintf(os.Stderr, "Warning: shed %q already exists; ignoring --repo %s\n", planShedFlag, planRepoFlag)
	case planUseExisting:
	}

	client := NewAPIClientFromNamedEntry(serverName, entry, clientConfig.GetCreateTimeout())
	if _, err := ensureRunningShed(client, planShedFlag); err != nil {
		return planFailAfterCreate(shedCreated, planShedFlag, err)
	}

	slug, err := genRCSlug()
	if err != nil {
		return err
	}
	if verboseLevel > 0 {
		fmt.Printf("Shipping plan to %s as %s session rc-%s (mode=%s)...\n", planShedFlag, kind, slug, rc.PermModeAuto)
	}
	dto, err := createRCSession(rcCreateOptions{
		shedName:       planShedFlag,
		entry:          entry,
		kind:           kind,
		displayName:    planShedFlag + "/" + slug,
		slug:           slug,
		permissionMode: rc.PermModeAuto,
		plan:           planContent,
		planFraming:    planFramingFlag,
	})
	if err != nil {
		if isOldBinaryRCErr(err) {
			return planFailAfterCreate(shedCreated, planShedFlag,
				fmt.Errorf("this shed's image predates multi-agent RC (recreate it to use --kind %s / plan delivery)", kind))
		}
		return planFailAfterCreate(shedCreated, planShedFlag, fmt.Errorf("shipping plan: %w", err))
	}
	return reportPlanOutcome(planShedFlag, entry, dto, kind)
}

// reportPlanOutcome prints the session summary and returns the exit-contract error
// (nil only when the session is ready + the kickoff was delivered). A needs-auth /
// needs-trust / not-ready session is left running; the returned error makes the exit
// non-zero without deleting anything.
func reportPlanOutcome(shedName string, entry *config.ServerEntry, dto rc.Session, kind string) error {
	switch dto.State {
	case "ready":
		printSuccess("Plan shipped to rc-%s and started (%s)", dto.Slug, dto.Kind)
		printRCFollowups(shedName, dto)
		// Default (no -d) on an interactive terminal drops you into the session to
		// watch it work; -d or a non-interactive stdout just reports and returns.
		if !planDetachFlag && stdioIsInteractive() {
			return attachToRCSlug(shedName, entry, dto.Slug)
		}
		return nil
	case "needs-auth":
		fmt.Printf("Session rc-%s created but the agent is not logged in in %s (session left running).\n", dto.Slug, shedName)
		fmt.Printf("  Log in: shed attach %s  →  %s\n", shedName, rc.AuthHintFor(rc.Kind(kind)))
		fmt.Printf("  Retry:  shed attach %s --slug %s\n", shedName, dto.Slug)
		return fmt.Errorf("plan not started: rc-%s needs authentication", dto.Slug)
	case "needs-trust":
		fmt.Printf("Session rc-%s created but the workspace-trust prompt is showing (session left running).\n", dto.Slug)
		fmt.Printf("  Accept: shed attach %s --slug %s\n", shedName, dto.Slug)
		return fmt.Errorf("plan not started: rc-%s needs workspace trust", dto.Slug)
	default:
		fmt.Printf("Session rc-%s created but did not reach ready (state=%s); session left running.\n", dto.Slug, dto.State)
		fmt.Printf("  Inspect: shed attach %s --slug %s\n", shedName, dto.Slug)
		return fmt.Errorf("plan not started: rc-%s state=%s", dto.Slug, dto.State)
	}
}

// planShedAction is the create-if-missing decision for a `shed plan` invocation.
type planShedAction int

const (
	planUseExisting planShedAction = iota
	planCreateMissing
	planErrorMissingNoRepo
	planWarnIgnoreRepo
)

// decidePlanShed maps (shed found?, --repo value) to the action shed plan takes:
// create only when the shed is missing AND a repo is given; a repo on an existing
// shed is a warn-and-ignore; a missing shed with no repo is a hard error.
func decidePlanShed(found bool, repo string) planShedAction {
	switch {
	case found && repo != "":
		return planWarnIgnoreRepo
	case found:
		return planUseExisting
	case repo != "":
		return planCreateMissing
	default:
		return planErrorMissingNoRepo
	}
}

// planFailAfterCreate wraps a post-create failure so the message reports BOTH facts
// (the shed was created AND the plan failed) and makes clear the shed was left in
// place to retry — never auto-deleted. When the shed already existed it passes the
// cause through unchanged.
func planFailAfterCreate(shedCreated bool, shedName string, cause error) error {
	if shedCreated {
		return fmt.Errorf("shed %q was created but the plan could not be shipped: %w — the shed was NOT deleted; retry with `shed plan ... --shed %s` after fixing the issue", shedName, cause, shedName)
	}
	return cause
}

// readPlanArg reads a plan from a file path (or - for stdin), enforcing the same
// non-empty / size / UTF-8 guards the guest applies, so a bad plan fails fast
// client-side before any shed or session is touched. Both sources are read through
// a LimitReader (and a regular file is Stat'd first) so an oversized file is
// rejected without ever loading more than the cap into memory.
func readPlanArg(file string) (string, error) {
	src := io.Reader(os.Stdin)
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return "", fmt.Errorf("reading plan: %w", err)
		}
		defer func() { _ = f.Close() }()
		if info, err := f.Stat(); err == nil && info.Mode().IsRegular() && info.Size() > rc.PlanMaxBytes {
			return "", fmt.Errorf("plan exceeds %d bytes", rc.PlanMaxBytes)
		}
		src = f
	}
	data, err := io.ReadAll(io.LimitReader(src, rc.PlanMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading plan: %w", err)
	}
	if len(data) > rc.PlanMaxBytes {
		return "", fmt.Errorf("plan exceeds %d bytes", rc.PlanMaxBytes)
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("plan is empty; nothing to ship")
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("plan is not valid UTF-8 (is it a binary file?)")
	}
	return string(data), nil
}

// locateShed resolves which server hosts a shed WITHOUT emitting the "not found"
// guidance findShedServer prints — shed plan may legitimately create the shed next,
// so the miss must be quiet. Honors -s/--server; otherwise checks the cached server,
// then every configured server. Returns found=false when none has it.
func locateShed(name string) (string, *config.ServerEntry, bool) {
	exists := func(serverName string, entry *config.ServerEntry) bool {
		_, err := NewAPIClientFromNamedEntry(serverName, entry, DefaultTimeout).GetShed(name)
		return err == nil
	}
	if serverFlag != "" {
		entry, err := clientConfig.GetServer(serverFlag)
		if err != nil {
			return "", nil, false
		}
		if exists(serverFlag, entry) {
			return serverFlag, entry, true
		}
		return "", nil, false
	}
	if cached, err := clientConfig.GetShedServer(name); err == nil {
		if entry, err := clientConfig.GetServer(cached); err == nil && exists(cached, entry) {
			return cached, entry, true
		}
	}
	for serverName, e := range clientConfig.Servers {
		entry := e
		if exists(serverName, &entry) {
			return serverName, &entry, true
		}
	}
	return "", nil, false
}
