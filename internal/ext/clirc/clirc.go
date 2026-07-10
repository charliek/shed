// Package clirc is the shared command dispatcher for the RC Session Convention v2
// command-line tools — the guest binary shed-ext-rc and the host binary
// shed-machine-rc. Both are thin mains over this package; Config supplies each
// binary's program name + default created-by provenance (and whether the host-only
// `claude` convenience verb is exposed), so the two binaries share one dispatch,
// one set of subcommands, and one JSON DTO contract.
//
// The dispatch is parameterized over an injectable deps struct (runner, env, stdio,
// hostname) so every subcommand is unit-testable against a fake rc.Runner without
// touching the real process stdio or tmux. Run wires the real os.* dependencies;
// tests call the unexported run with fakes.
package clirc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charliek/shed/internal/ext/rc"
	"github.com/charliek/shed/internal/version"
)

// Config is the per-binary identity supplied by each cmd/* main.
type Config struct {
	// ProgName is the binary's name, used in usage / version / error output.
	ProgName string
	// DefaultCreatedBy stamps SHED_RC_CREATED_BY when --created-by is empty. Each
	// binary owns its default here so internal/rc's ToolName fallback is never the
	// one that decides provenance (shed-machine-rc must not stamp "shed-ext-rc").
	DefaultCreatedBy string
	// EnableClaudeVerb exposes the host-only `claude` convenience subcommand.
	EnableClaudeVerb bool
}

// deps are the side-effecting dependencies — real in Run, fakes in tests.
type deps struct {
	runner   rc.Runner
	getenv   rc.Getenv
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
	hostname func() string
	// sleep is the poll delay used by rc.Create's wait loop. nil → real time.Sleep
	// (production); tests inject a no-op so a --wait/claude create returns instantly.
	sleep func(time.Duration)
	// probe reports an agent binary's installed state + version for the capabilities
	// payload. nil → the real command-based probe (command -v + --version, 2s budget);
	// tests inject a fake so no external process is spawned.
	probe rc.AgentProbe
}

// Run dispatches args with real process dependencies and returns a process exit code.
func Run(cfg Config, args []string) int {
	return run(cfg, deps{
		runner:   rc.DefaultRunner(),
		getenv:   os.Getenv,
		stdin:    os.Stdin,
		stdout:   os.Stdout,
		stderr:   os.Stderr,
		hostname: shortHostname,
	}, args)
}

// run is the testable dispatch core.
func run(cfg Config, d deps, args []string) int {
	if len(args) == 0 {
		usage(cfg, d)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		return doCreate(cfg, d, rest)
	case "list":
		return doList(cfg, d, rest)
	case "capabilities":
		return doCapabilities(cfg, d, rest)
	case "probe":
		return doProbe(cfg, d, rest)
	case "accept-trust":
		return doAcceptTrust(cfg, d, rest)
	case "prompt":
		return doPrompt(cfg, d, rest)
	case "kill":
		return doKill(cfg, d, rest)
	case "claude":
		if !cfg.EnableClaudeVerb {
			return unknown(cfg, d, cmd)
		}
		return doClaude(cfg, d, rest)
	case "version", "--version", "-v":
		fmt.Fprintf(d.stdout, "%s %s\n", cfg.ProgName, version.FullInfo())
		return 0
	case "help", "-h", "--help":
		usage(cfg, d)
		return 0
	default:
		return unknown(cfg, d, cmd)
	}
}

func usage(cfg Config, d deps) {
	var b strings.Builder
	fmt.Fprintf(&b, "usage: %s <command> [flags]\n\ncommands:\n", cfg.ProgName)
	if cfg.EnableClaudeVerb {
		b.WriteString("  claude   [--name n] [--workdir d] [--slug s] [--permission-mode m | --skip]\n")
		b.WriteString("           start a local auto-mode claude remote-control session and print its URL\n")
	}
	b.WriteString(`  create   --kind <k> --name <display> [--slug s] [--workdir d] [--created-by t/v]
           [--target label] [--wait] [--interactive-shell]
           [--prompt-stdin | --plan-stdin [--prompt-b64 <b64>]]
           [--permission-mode <m> | --skip]
  list
  capabilities
  probe    --slug <s>
  accept-trust --slug <s>
  prompt   --slug <s> [--session-id <uuid>]   (text read from stdin)
  kill     --slug <s>
  version
`)
	fmt.Fprint(d.stderr, b.String())
}

// exitCode maps a domain error to a process exit code. SSH-transport classification
// (auth/unreachable) is the orchestrator's job; these are the binary-local outcomes.
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, rc.ErrBadArgs):
		return 2
	case errors.Is(err, rc.ErrDuplicateSlug):
		return 3
	case errors.Is(err, rc.ErrSessionNotFound):
		return 4
	default:
		return 1
	}
}

// fail prints err to stderr with the program-name prefix and returns its exit code.
func fail(cfg Config, d deps, err error) int {
	fmt.Fprintf(d.stderr, "%s: %s\n", cfg.ProgName, err)
	return exitCode(err)
}

func unknown(cfg Config, d deps, cmd string) int {
	fmt.Fprintf(d.stderr, "%s: unknown command %q\n", cfg.ProgName, cmd)
	usage(cfg, d)
	return 2
}

// readStdinLine reads all of stdin and strips a single trailing newline (and CR), so a
// kickoff line piped in is not mistaken for a CLI flag.
func readStdinLine(d deps) (string, error) {
	data, err := io.ReadAll(d.stdin)
	if err != nil {
		return "", fmt.Errorf("reading stdin: %w", err)
	}
	s := strings.TrimSuffix(string(data), "\n")
	s = strings.TrimSuffix(s, "\r")
	return s, nil
}

func printJSON(cfg Config, d deps, v any) int {
	enc := json.NewEncoder(d.stdout)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(d.stderr, "%s: encoding output: %s\n", cfg.ProgName, err)
		return 1
	}
	return 0
}

// claudeDefaultMode is the posture the host-only `claude` convenience verb runs in
// when neither --permission-mode nor --skip is given: unattended "auto" (the whole
// point of the verb — start it and walk away). It lives here, not in internal/rc,
// because it is a host-CLI policy, not part of the engine.
const claudeDefaultMode = "auto"

// resolveMode applies the --skip shorthand and the --skip/--permission-mode mutual
// exclusion, falling back to dflt when neither is given (dflt "" = pass no posture
// flag). --skip is the generic full-bypass mode ("skip"), mapped per agent to its real
// flag by the registry (claude → bypassPermissions, codex → --dangerously-…, etc.).
func resolveMode(permMode string, skip bool, dflt string) (string, error) {
	if skip {
		if permMode != "" {
			return "", fmt.Errorf("%w: --skip and --permission-mode are mutually exclusive", rc.ErrBadArgs)
		}
		return rc.PermModeSkip, nil
	}
	if permMode == "" {
		return dflt, nil
	}
	return permMode, nil
}

// parseArgs parses fs and rejects any leftover positional argument (a stray token is
// almost always a typo; silently dropping it hides the mistake). Returns (exitCode,
// ok) — when ok is false the caller returns exitCode.
func parseArgs(cfg Config, d deps, fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		return 2, false // flag already printed usage to d.stderr
	}
	if fs.NArg() > 0 {
		return fail(cfg, d, fmt.Errorf("%w: unexpected argument %q", rc.ErrBadArgs, fs.Arg(0))), false
	}
	return 0, true
}

func doCreate(cfg Config, d deps, args []string) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var (
		kind          = fs.String("kind", string(rc.DefaultKind), "session kind: "+strings.Join(rc.KindStrings(), "|"))
		name          = fs.String("name", "", "display name (--name to claude); defaults to the slug")
		slug          = fs.String("slug", "", "caller-supplied slug (generated when empty)")
		workdir       = fs.String("workdir", "", "working directory (defaults to $SHED_WORKSPACE)")
		createdByFlag = fs.String("created-by", "", "provenance <tool>/<version>")
		target        = fs.String("target", "", "advisory target label")
		wait          = fs.Bool("wait", false, "block until ready, accept trust, deliver prompt")
		interactive   = fs.Bool("interactive-shell", false, "wrap claude kinds in `bash -ic`")
		promptStdin   = fs.Bool("prompt-stdin", false, "read a kickoff prompt line from stdin")
		planStdin     = fs.Bool("plan-stdin", false, "read a plan from stdin; write it to a per-kind file and deliver a composed kickoff")
		promptB64     = fs.String("prompt-b64", "", "base64 caller framing prepended to the composed plan kickoff (only with --plan-stdin)")
		permMode      = fs.String("permission-mode", "", "permission mode: default|auto|skip (all kinds); claude also accepts acceptEdits|plan|dontAsk|bypassPermissions")
		skip          = fs.Bool("skip", false, "shorthand for --permission-mode skip (full bypass)")
	)
	if code, ok := parseArgs(cfg, d, fs, args); !ok {
		return code
	}

	mode, err := resolveMode(*permMode, *skip, "")
	if err != nil {
		return fail(cfg, d, err)
	}

	// stdin carries at most one payload: a prompt line (--prompt-stdin) OR a plan
	// (--plan-stdin). Caller framing for a plan travels out-of-band as base64
	// (--prompt-b64), never on stdin, so a single guest exec ships plan + framing.
	if *promptStdin && *planStdin {
		return fail(cfg, d, fmt.Errorf("%w: --prompt-stdin and --plan-stdin are mutually exclusive", rc.ErrBadArgs))
	}
	if *promptB64 != "" && !*planStdin {
		return fail(cfg, d, fmt.Errorf("%w: --prompt-b64 is only valid with --plan-stdin", rc.ErrBadArgs))
	}

	prompt, plan, framing := "", "", ""
	switch {
	case *promptStdin:
		p, err := readStdinLine(d)
		if err != nil {
			return fail(cfg, d, err)
		}
		if p == "" {
			return fail(cfg, d, fmt.Errorf("%w: --prompt-stdin given but stdin is empty", rc.ErrBadArgs))
		}
		prompt = p
	case *planStdin:
		p, err := readPlanStdin(d)
		if err != nil {
			return fail(cfg, d, err)
		}
		plan = p
		if *promptB64 != "" {
			raw, derr := base64.StdEncoding.DecodeString(*promptB64)
			if derr != nil {
				return fail(cfg, d, fmt.Errorf("%w: --prompt-b64 is not valid base64: %v", rc.ErrBadArgs, derr))
			}
			// Reject non-UTF-8 payloads BEFORE the []byte→string conversion: an
			// invalid byte (e.g. a lone C1 0x9b) would otherwise become RuneError in
			// the rune-based control-char scan and slip past HasUnsafePromptChars.
			if !utf8.Valid(raw) {
				return fail(cfg, d, fmt.Errorf("%w: --prompt-b64 does not decode to valid UTF-8", rc.ErrBadArgs))
			}
			framing = string(raw)
		}
	}

	createdBy := *createdByFlag
	if createdBy == "" {
		createdBy = cfg.DefaultCreatedBy
	}

	session, err := rc.Create(d.runner, d.getenv, rc.CreateOptions{
		Kind:             rc.Kind(*kind),
		DisplayName:      *name,
		Slug:             *slug,
		Workdir:          *workdir,
		CreatedBy:        createdBy,
		Target:           *target,
		Prompt:           prompt,
		Plan:             plan,
		PlanFraming:      framing,
		Wait:             *wait,
		InteractiveShell: *interactive,
		PermissionMode:   mode,
	}, d.sleep)
	if err != nil {
		return fail(cfg, d, err)
	}
	return printJSON(cfg, d, session)
}

// readPlanStdin reads a plan from stdin, capping it at rc.PlanMaxBytes and rejecting
// an empty or non-UTF-8 payload at the transport boundary (rc.Create re-validates as
// the library guarantee). The plan is NOT newline-trimmed — a plan is a document, not
// a single line, and trailing structure is content.
func readPlanStdin(d deps) (string, error) {
	data, err := io.ReadAll(io.LimitReader(d.stdin, rc.PlanMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading plan from stdin: %w", err)
	}
	if len(data) > rc.PlanMaxBytes {
		return "", fmt.Errorf("%w: plan exceeds %d bytes", rc.ErrBadArgs, rc.PlanMaxBytes)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("%w: --plan-stdin given but stdin is empty", rc.ErrBadArgs)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%w: plan is not valid UTF-8 (is stdin a binary file?)", rc.ErrBadArgs)
	}
	return string(data), nil
}

func doList(cfg Config, d deps, args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	if code, ok := parseArgs(cfg, d, fs, args); !ok {
		return code
	}
	resp := rc.List(d.runner, nil)
	// One guest exec feeds both the session list and capability discovery.
	caps := rc.BuildCapabilities(effectiveProbe(d))
	resp.Capabilities = &caps
	return printJSON(cfg, d, resp)
}

// doCapabilities prints the capabilities payload (kinds, per-agent install/version,
// features, per-kind UI hints) — the discovery mechanism that replaces error-string
// sniffing.
func doCapabilities(cfg Config, d deps, args []string) int {
	fs := flag.NewFlagSet("capabilities", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	if code, ok := parseArgs(cfg, d, fs, args); !ok {
		return code
	}
	return printJSON(cfg, d, rc.BuildCapabilities(effectiveProbe(d)))
}

// effectiveProbe returns the injected agent probe, or the real command-based probe
// when none was injected (production).
func effectiveProbe(d deps) rc.AgentProbe {
	if d.probe != nil {
		return d.probe
	}
	return realAgentProbe
}

// agentProbeTimeout bounds each `command -v` / `--version` call so an unresponsive or
// hung agent binary can't stall capability discovery.
const agentProbeTimeout = 2 * time.Second

// realAgentProbe reports whether an agent binary is installed (via `command -v`) and
// its version (via `<bin> --version`), each under a short timeout. bin values come
// from the registry (fixed literals), never user input.
func realAgentProbe(bin string) rc.AgentInfo {
	ctx, cancel := context.WithTimeout(context.Background(), agentProbeTimeout)
	defer cancel()
	if err := exec.CommandContext(ctx, "sh", "-c", "command -v "+shellQuote(bin)).Run(); err != nil {
		return rc.AgentInfo{Installed: false}
	}
	vctx, vcancel := context.WithTimeout(context.Background(), agentProbeTimeout)
	defer vcancel()
	out, _ := exec.CommandContext(vctx, bin, "--version").CombinedOutput()
	return rc.AgentInfo{Installed: true, Version: rc.ParseAgentVersion(string(out))}
}

// shellQuote wraps a token in single quotes for a `sh -c` string (POSIX '\” escape).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runSlugCmd parses a slug-only subcommand (probe/accept-trust/kill) and runs fn.
func runSlugCmd(cfg Config, d deps, name string, args []string, fn func(slug string) int) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	slug := fs.String("slug", "", "session slug")
	if code, ok := parseArgs(cfg, d, fs, args); !ok {
		return code
	}
	if *slug == "" {
		return fail(cfg, d, fmt.Errorf("%w: --slug is required", rc.ErrBadArgs))
	}
	return fn(*slug)
}

func doProbe(cfg Config, d deps, args []string) int {
	return runSlugCmd(cfg, d, "probe", args, func(slug string) int {
		session, err := rc.Probe(d.runner, slug, nil)
		if err != nil {
			return fail(cfg, d, err)
		}
		return printJSON(cfg, d, session)
	})
}

func doAcceptTrust(cfg Config, d deps, args []string) int {
	return runSlugCmd(cfg, d, "accept-trust", args, func(slug string) int {
		if err := rc.AcceptTrust(d.runner, slug); err != nil {
			return fail(cfg, d, err)
		}
		return 0
	})
}

func doKill(cfg Config, d deps, args []string) int {
	return runSlugCmd(cfg, d, "kill", args, func(slug string) int {
		if err := rc.Kill(d.runner, slug); err != nil {
			return fail(cfg, d, err)
		}
		return 0
	})
}

func doPrompt(cfg Config, d deps, args []string) int {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	slug := fs.String("slug", "", "session slug")
	sessionID := fs.String("session-id", "", "expected SHED_RC_ID (guards a recreated slug)")
	if code, ok := parseArgs(cfg, d, fs, args); !ok {
		return code
	}
	if *slug == "" {
		return fail(cfg, d, fmt.Errorf("%w: --slug is required", rc.ErrBadArgs))
	}
	text, err := readStdinLine(d)
	if err != nil {
		return fail(cfg, d, err)
	}
	if text == "" {
		return fail(cfg, d, fmt.Errorf("%w: prompt text (stdin) is empty", rc.ErrBadArgs))
	}
	if err := rc.Prompt(d.runner, rc.PromptOptions{Slug: *slug, Text: text, SessionID: *sessionID}); err != nil {
		return fail(cfg, d, err)
	}
	return 0
}

// doClaude is the host-only convenience verb: start a local claude-rc session in the
// autonomous posture (default --permission-mode auto), wait until ready, and print a
// human-facing summary with the claude.ai URL — then return, leaving the tmux session
// live and watchable from shed-remote-agent / shed-mobile. Unlike create it prints
// prose, not the JSON DTO, because it is meant for a person at a terminal.
func doClaude(cfg Config, d deps, args []string) int {
	fs := flag.NewFlagSet("claude", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	var (
		name     = fs.String("name", "", "display name (defaults to <hostname>/<slug>)")
		workdir  = fs.String("workdir", "", "working directory (defaults to $SHED_WORKSPACE/$HOME)")
		slug     = fs.String("slug", "", "caller-supplied slug (generated when empty)")
		permMode = fs.String("permission-mode", "", "claude --permission-mode (default: auto)")
		skip     = fs.Bool("skip", false, "shorthand for --permission-mode bypassPermissions")
	)
	if code, ok := parseArgs(cfg, d, fs, args); !ok {
		return code
	}

	mode, err := resolveMode(*permMode, *skip, claudeDefaultMode)
	if err != nil {
		return fail(cfg, d, err)
	}

	// Resolve slug + display name up front so the printed name reads nicely in the
	// watching tools (e.g. "mac-mini/ab12cd") instead of a bare slug.
	slugVal := *slug
	if slugVal == "" {
		g, gerr := rc.GenSlug()
		if gerr != nil {
			return fail(cfg, d, gerr)
		}
		slugVal = g
	} else if !rc.ValidCallerSlug(slugVal) {
		return fail(cfg, d, fmt.Errorf("%w: invalid slug %q", rc.ErrBadArgs, slugVal))
	}
	nameVal := *name
	if nameVal == "" {
		if h := d.hostname(); h != "" {
			nameVal = h + "/" + slugVal
		} else {
			nameVal = slugVal
		}
	}

	session, err := rc.Create(d.runner, d.getenv, rc.CreateOptions{
		Kind:             rc.KindClaudeRC,
		DisplayName:      nameVal,
		Slug:             slugVal,
		Workdir:          *workdir,
		CreatedBy:        cfg.DefaultCreatedBy,
		Wait:             true,
		InteractiveShell: true,
		PermissionMode:   mode,
	}, d.sleep)
	if err != nil {
		return fail(cfg, d, err)
	}

	fmt.Fprintf(d.stdout, "Started %s session %q — permission-mode=%s (tools run UNATTENDED).\n",
		session.Kind, session.Slug, mode)
	// Exit non-zero when the session didn't reach a usable URL, so a script that
	// runs `shed-machine-rc claude` can tell "ready" from "needs auth / still
	// starting" — the tmux session is left running either way.
	exit := 0
	switch session.State {
	case rc.StateReady:
		fmt.Fprintf(d.stdout, "  Watch/steer from your phone or browser: %s\n", session.URL)
	case rc.StateNeedsAuth:
		fmt.Fprintf(d.stdout, "  Claude is not logged in on this machine — run `claude` once to authenticate, then retry.\n")
		exit = 1
	default:
		fmt.Fprintf(d.stdout, "  State: %s (no URL yet — `%s probe --slug %s` to recheck).\n",
			session.State, cfg.ProgName, session.Slug)
		exit = 1
	}
	fmt.Fprintf(d.stdout, "  Attach locally:  tmux attach -t %s\n", session.TmuxSession)
	fmt.Fprintf(d.stdout, "  Visible to shed-remote-agent / shed-mobile on this machine.\n")
	return exit
}

// shortHostname returns the machine's hostname truncated at the first dot ("" on error),
// used only to prettify the default display name of the `claude` verb.
func shortHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}
