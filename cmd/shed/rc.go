package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/ext/rc"
	"github.com/charliek/shed/internal/version"
)

// rcTmuxPrefix is the session-name prefix that marks a Remote Control session
// under the RC Session Convention (e.g. "rc-abc234").
const rcTmuxPrefix = "rc-"

// Session-listing RC enrichment now lives entirely server-side: the server execs
// `shed-ext-rc list` over the guest agent channel and populates Session.RC on GET
// /api/sessions (see internal/api/rcenrich.go). The CLI just renders whatever the
// server returns — it opens no SSH connection to enrich a listing. The canonical
// `list` DTO is internal/ext/rc.Session; createRCSession below decodes into it.

// baseSSHArgs returns the SSH args common to every shed connection: port, pinned
// known_hosts, strict host-key check, any extra -o options, then <shed>@<host>.
// Callers prepend "ssh" (and "-t" for an interactive PTY) and append "--" + the
// remote command. Shared by the interactive attach path and the capture path so
// the connection options can't drift.
func baseSSHArgs(shedName string, entry *config.ServerEntry, extraOpts ...string) []string {
	args := []string{
		"-p", strconv.Itoa(entry.SSHPort),
		"-o", "UserKnownHostsFile=" + config.GetKnownHostsPath(),
		"-o", "StrictHostKeyChecking=yes",
	}
	args = append(args, extraOpts...)
	return append(args, shedName+"@"+entry.Host)
}

// sshCaptureArgs builds the `ssh` argv for a non-interactive, output-capturing
// command against a shed (no PTY; BatchMode so a key/auth issue fails fast instead
// of hanging). remoteArgv elements are sent to the server as-is (joined by ssh and
// re-parsed by the server's `bash -lc`), so a caller passing user data as a single
// element must shell-quote it first (see createRCSession); literal tokens like
// "shed-ext-rc","create" need no quoting.
func sshCaptureArgs(shedName string, entry *config.ServerEntry, remoteArgv ...string) []string {
	// -T disables PTY allocation so ssh doesn't print "Pseudo-terminal will not be
	// allocated…" to our captured stderr, and so the non-PTY exec channel keeps the
	// remote command's stdout and stderr on their own SSH streams (guest-binary
	// diagnostics arrive on stderr; see the sshShell stderr fallback).
	args := baseSSHArgs(shedName, entry, "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10")
	args = append(args, "--")
	return append(args, remoteArgv...)
}

// --- RC session creation (shed attach --kind ...) ---------------------------

// rcCreateTimeout bounds a `shed-ext-rc create --wait` round-trip (the binary
// itself polls ~20s for ready, then delivers the prompt; allow generous headroom).
const rcCreateTimeout = 75 * time.Second

// rcSlugAlphabet is the convention's confusable-free alphabet (no 0/o, 1/l/i).
const rcSlugAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// genRCSlug returns a 6-char slug. Generated CLI-side so the caller knows the slug
// before create (to name the plan file and reference it in the kickoff prompt).
func genRCSlug() (string, error) {
	var b strings.Builder
	for range 6 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(rcSlugAlphabet))))
		if err != nil {
			return "", fmt.Errorf("generating slug: %w", err)
		}
		b.WriteByte(rcSlugAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// sshShell runs a single shell command in a shed over SSH (the server wraps it in
// `bash -lc`), feeding stdin and capturing stdout. A non-zero exit returns an error
// carrying stderr. Used for one-shot guest commands (shed-ext-rc, plan transfer).
func sshShell(ctx context.Context, shedName string, entry *config.ServerEntry, stdin, cmdStr string) ([]byte, error) {
	args := sshCaptureArgs(shedName, entry, cmdStr)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		// Prefer stderr for the diagnostic (e.g. a guest binary's "flag provided but
		// not defined: …"), falling back to stdout. The non-PTY exec channel now
		// keeps stderr on its own stream, but older baked agents still fold it into
		// stdout, so the fallback covers both.
		detail := strings.TrimSpace(errb.String())
		if detail == "" {
			detail = strings.TrimSpace(out.String())
		}
		if detail != "" {
			return out.Bytes(), fmt.Errorf("%w: %s", err, detail)
		}
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}

// isOldBinaryRCErr reports whether a createRCSession error is the shed's baked-in
// shed-ext-rc rejecting our request because its image predates multi-agent RC. Only
// the two exact signatures an OLD binary emits for a request it cannot understand are
// matched:
//   - `invalid arguments: unknown kind "<k>"` — rc.Create's kind rejection (the CLI
//     already validated the kind against the current registry, so this can only mean
//     the guest binary's registry is older);
//   - `flag provided but not defined: -<flag>` — Go's flag package rejecting a create
//     flag the old binary doesn't have.
//
// Any other guest error — including a NEW binary's legitimate input validation
// ("invalid arguments: prompt contains an unsupported control character",
// "invalid arguments: invalid slug", …) — passes through unchanged so it is never
// misreported as "recreate this shed".
func isOldBinaryRCErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "unknown kind") ||
		strings.Contains(s, "flag provided but not defined")
}

// rcCreateOptions configures createRCSession.
type rcCreateOptions struct {
	shedName       string
	entry          *config.ServerEntry
	kind           string
	displayName    string
	slug           string
	permissionMode string // "" omits the flag
	// workdir, when set, is passed through as `--workdir` (the guest's create --workdir
	// flag) so the RC session's tmux pane starts there instead of the guest's
	// $SHED_WORKSPACE/$HOME default. Empty means "let the guest decide" (unchanged
	// behavior).
	workdir string
	prompt  string // kickoff line delivered via --prompt-stdin (empty -> none)
	// plan, when set, is delivered via --plan-stdin: the guest writes it to a per-kind
	// HOME-rooted file and composes+delivers the kickoff, so the plan never touches the
	// workdir (a --repo clone / --local-dir host dir). Mutually exclusive with prompt.
	plan string
	// planFraming is optional caller framing shipped as base64 (--prompt-b64) alongside
	// the plan, so stdin stays reserved for the plan itself. Only meaningful with plan.
	planFraming string
}

// buildRCCreateArgv builds the `shed-ext-rc create` argv from opts, plus the stdin
// payload it implies (the kickoff prompt or the plan — never argv, per the
// convention) — split out from createRCSession so the flag-threading logic
// (kind/slug/name/workdir/mode/plan-vs-prompt) is unit-testable without a real SSH
// round-trip.
func buildRCCreateArgv(opts rcCreateOptions) (argv []string, stdin string) {
	argv = []string{
		"shed-ext-rc", "create",
		"--kind", opts.kind,
		"--slug", opts.slug,
		"--created-by", "shed/" + version.Version,
		"--target", "shed:" + opts.shedName + "@" + opts.entry.Host,
		"--wait",
	}
	if opts.displayName != "" {
		argv = append(argv, "--name", opts.displayName)
	}
	if opts.workdir != "" {
		argv = append(argv, "--workdir", opts.workdir)
	}
	if opts.permissionMode != "" {
		argv = append(argv, "--permission-mode", opts.permissionMode)
	}
	switch {
	case opts.plan != "":
		argv = append(argv, "--plan-stdin")
		if opts.planFraming != "" {
			argv = append(argv, "--prompt-b64", base64.StdEncoding.EncodeToString([]byte(opts.planFraming)))
		}
		stdin = opts.plan
	case opts.prompt != "":
		argv = append(argv, "--prompt-stdin")
		stdin = opts.prompt
	}
	return argv, stdin
}

// createRCSession invokes `shed-ext-rc create --wait` over SSH and returns the
// parsed session DTO. Each shed-ext-rc argument is shell-quoted into one command
// string (the server re-parses through bash -lc). stdin carries the kickoff prompt
// (--prompt-stdin) or the plan (--plan-stdin) — never argv, per the convention — with
// any plan framing base64-encoded onto argv so a single exec ships plan + framing.
func createRCSession(opts rcCreateOptions) (rc.Session, error) {
	argv, stdin := buildRCCreateArgv(opts)
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuoteArg(a)
	}

	ctx, cancel := context.WithTimeout(context.Background(), rcCreateTimeout)
	defer cancel()
	out, err := sshShell(ctx, opts.shedName, opts.entry, stdin, strings.Join(quoted, " "))
	if err != nil {
		return rc.Session{}, fmt.Errorf("shed-ext-rc create: %w", err)
	}
	var dto rc.Session
	if err := json.Unmarshal(bytes.TrimSpace(out), &dto); err != nil {
		return rc.Session{}, fmt.Errorf("decoding shed-ext-rc create output: %w (raw: %q)", err, string(out))
	}
	return dto, nil
}

// editorInput opens $VISUAL/$EDITOR on a temp file seeded with template, returns the
// saved content with lines starting with commentPrefix stripped (use "#" for a
// git-commit-style prompt, "<!--" for a markdown plan so real `#` headers survive).
// Errors (never hangs) when no editor is set or the session isn't interactive — the
// skill path must not use it.
func editorInput(template, suffix, commentPrefix string) (string, error) {
	editor := firstNonEmptyEnv("VISUAL", "EDITOR")
	if editor == "" {
		return "", fmt.Errorf("no $VISUAL or $EDITOR set; pass the text with a flag instead")
	}
	if !stdioIsInteractive() {
		return "", fmt.Errorf("editor input needs an interactive terminal; pass the text with a flag instead")
	}
	f, err := os.CreateTemp("", "shed-*"+suffix)
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	if template != "" {
		_, _ = f.WriteString(template)
	}
	_ = f.Close()

	cmd := exec.Command("sh", "-c", editor+" \"$1\"", "sh", path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor exited with error: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if commentPrefix != "" && strings.HasPrefix(line, commentPrefix) {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String()), nil
}

const promptEditTemplate = "# Write the single-line kickoff prompt for the agent.\n# Lines starting with '#' are ignored.\n"

// Single-line so the whole template is one stripped comment (a wrapped second line
// would not start with the prefix and would leak into the plan); markdown '#'
// headers in the body are preserved because the prefix is "<!--", not "#".
const planEditTemplate = "<!-- Write the plan (markdown) below and delete this line; markdown '#' headers are kept. -->\n\n"

// rcInputs holds the raw prompt/plan flag values for resolveRCInputs.
type rcInputs struct {
	prompt     string // -p value
	promptFile string // --prompt-file value ("-" = stdin)
	edit       bool   // --edit ($EDITOR)
	plan       string // --plan value ("-" = stdin)
	planEdit   bool   // --plan-edit ($EDITOR)
}

// resolveRCInputs resolves the kickoff prompt and (optional) plan content from the
// flags, enforcing: at most one prompt source, at most one plan source, at most one
// stdin (`-`) reader, and non-empty editor results. The prompt may be multi-line
// (shed-ext-rc delivers it as one input via a bracketed paste); prefer a plan file
// for large/multi-step work.
func resolveRCInputs(in rcInputs) (prompt, planContent string, havePlan bool, err error) {
	if n := boolCount(in.prompt != "", in.promptFile != "", in.edit); n > 1 {
		return "", "", false, fmt.Errorf("choose at most one of --prompt/--prompt-file/--edit")
	}
	if n := boolCount(in.plan != "", in.planEdit); n > 1 {
		return "", "", false, fmt.Errorf("choose at most one of --plan/--plan-edit")
	}
	promptStdin := in.promptFile == "-"
	planStdin := in.plan == "-"
	if promptStdin && planStdin {
		return "", "", false, fmt.Errorf("only one input can read stdin (-)")
	}

	// Plan.
	switch {
	case in.plan != "":
		if planStdin {
			if planContent, err = readStdinTrimmed(); err != nil {
				return "", "", false, err
			}
		} else {
			b, e := os.ReadFile(in.plan)
			if e != nil {
				return "", "", false, fmt.Errorf("reading plan file: %w", e)
			}
			planContent = string(b)
		}
		if strings.TrimSpace(planContent) == "" {
			return "", "", false, fmt.Errorf("plan is empty; nothing to execute")
		}
		havePlan = true
	case in.planEdit:
		if planContent, err = editorInput(planEditTemplate, ".md", "<!--"); err != nil {
			return "", "", false, err
		}
		if strings.TrimSpace(planContent) == "" {
			return "", "", false, fmt.Errorf("empty plan; aborting")
		}
		havePlan = true
	}

	// Prompt.
	switch {
	case in.prompt != "":
		prompt = in.prompt
	case in.promptFile != "":
		if promptStdin {
			if prompt, err = readStdinTrimmed(); err != nil {
				return "", "", false, err
			}
		} else {
			b, e := os.ReadFile(in.promptFile)
			if e != nil {
				return "", "", false, fmt.Errorf("reading prompt file: %w", e)
			}
			prompt = strings.TrimSpace(string(b))
		}
	case in.edit:
		if prompt, err = editorInput(promptEditTemplate, ".txt", "#"); err != nil {
			return "", "", false, err
		}
		if prompt == "" {
			return "", "", false, fmt.Errorf("empty prompt; aborting")
		}
	}
	return prompt, planContent, havePlan, nil
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// stdioIsInteractive reports whether stdin and stdout are both terminals.
func stdioIsInteractive() bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		fi, err := f.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	return true
}

// readStdinTrimmed reads all of stdin and trims trailing newlines (for `-` sources).
func readStdinTrimmed() (string, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\n"), nil
}
