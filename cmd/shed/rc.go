package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/version"
)

// rcTmuxPrefix is the session-name prefix that marks a Remote Control session
// under the RC Session Convention (e.g. "rc-abc234").
const rcTmuxPrefix = "rc-"

// rcEnrichTimeout bounds a single `shed-ext-rc list` SSH round-trip so a slow or
// unreachable shed never stalls `shed sessions`.
const rcEnrichTimeout = 6 * time.Second

// rcEnrichConcurrency caps concurrent SSH enrichment calls so `sessions --all`
// across many sheds doesn't fan out unbounded.
const rcEnrichConcurrency = 6

// rcSessionDTO mirrors shed-ext-rc's neutral `list` DTO (one entry of the
// `{"rc_sessions":[...]}` payload). It is decoded verbatim from the binary's
// stdout so the shed CLI stays aligned with the cross-repo golden fixture
// (shed-extensions internal/rc + shed-remote-agent); fields beyond those we
// display are accepted and ignored.
type rcSessionDTO struct {
	Slug        string `json:"slug"`
	TmuxSession string `json:"tmux_session"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Managed     bool   `json:"managed"`
	DisplayName string `json:"display_name"`
	URL         string `json:"url"`
	CreatedBy   string `json:"created_by"`
}

// rcListResponse is the `shed-ext-rc list` stdout shape.
type rcListResponse struct {
	RCSessions []rcSessionDTO `json:"rc_sessions"`
}

// toSessionRC projects the decoded DTO onto the display type carried on
// config.Session (the decode type is kept separate so it stays aligned with the
// shed-ext-rc golden contract independent of presentation).
func (d rcSessionDTO) toSessionRC() *config.SessionRC {
	return &config.SessionRC{
		Kind:        d.Kind,
		State:       d.State,
		Managed:     d.Managed,
		DisplayName: d.DisplayName,
		URL:         d.URL,
		CreatedBy:   d.CreatedBy,
	}
}

// parseRcList decodes `shed-ext-rc list` stdout into a map keyed by tmux session
// name (e.g. "rc-abc234") for O(1) merge onto enumerated tmux sessions.
func parseRcList(stdout []byte) (map[string]rcSessionDTO, error) {
	var resp rcListResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("decoding shed-ext-rc list: %w", err)
	}
	out := make(map[string]rcSessionDTO, len(resp.RCSessions))
	for _, s := range resp.RCSessions {
		if s.TmuxSession != "" {
			out[s.TmuxSession] = s
		}
	}
	return out, nil
}

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
// "shed-ext-rc","list" need no quoting.
func sshCaptureArgs(shedName string, entry *config.ServerEntry, remoteArgv ...string) []string {
	// -T disables PTY allocation so ssh doesn't print "Pseudo-terminal will not be
	// allocated…" to our captured stderr (the shed's sshd folds the remote command's
	// stderr into the stdout channel, so command diagnostics arrive on stdout anyway).
	args := baseSSHArgs(shedName, entry, "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10")
	args = append(args, "--")
	return append(args, remoteArgv...)
}

// rcListOverSSH runs `shed-ext-rc list` in the shed and returns the parsed
// sessions keyed by tmux name. A missing binary, transport failure, or non-zero
// exit is reported as an error for the caller to treat as "no RC data".
func rcListOverSSH(ctx context.Context, shedName string, entry *config.ServerEntry) (map[string]rcSessionDTO, error) {
	args := sshCaptureArgs(shedName, entry, "shed-ext-rc", "list")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("shed-ext-rc list on %s: %w", shedName, err)
	}
	return parseRcList(out)
}

// rcShedKey identifies a shed by server + name (a shed name is unique only
// within a server, so the aggregated `--all` listing must key by both).
type rcShedKey struct{ server, shed string }

// enrichSessionsRC fills the RC field of every "rc-*" session in sessions by
// querying the in-shed shed-ext-rc binary. It runs one `shed-ext-rc list` per
// distinct (server, shed) that actually has an rc-* session — bounded by
// rcEnrichConcurrency and time-limited per call — resolving each shed's SSH
// endpoint from the client config by the session's ServerName. It mutates
// sessions in place and degrades silently (rows keep their plain tmux data) when
// a shed is unreachable, lacks the binary, or its server is unknown. Non-RC
// listings touch nothing and never dial. Call once, after the full session list
// (with ServerName populated) is assembled.
func enrichSessionsRC(sessions []config.Session) {
	if clientConfig == nil {
		return // config not loaded (e.g. a direct test caller); nothing to resolve
	}
	idxByShed := make(map[rcShedKey][]int)
	for i := range sessions {
		if strings.HasPrefix(sessions[i].Name, rcTmuxPrefix) {
			k := rcShedKey{sessions[i].ServerName, sessions[i].ShedName}
			idxByShed[k] = append(idxByShed[k], i)
		}
	}
	if len(idxByShed) == 0 {
		return
	}

	sem := make(chan struct{}, rcEnrichConcurrency)
	var wg sync.WaitGroup
	for key, idxs := range idxByShed {
		entry, ok := clientConfig.Servers[key.server]
		if !ok {
			continue // unknown server -> leave rows un-enriched
		}
		wg.Add(1)
		go func(key rcShedKey, idxs []int, entry config.ServerEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), rcEnrichTimeout)
			defer cancel()
			byTmux, err := rcListOverSSH(ctx, key.shed, &entry)
			if err != nil {
				if verboseLevel > 0 {
					fmt.Fprintf(os.Stderr, "Warning: RC metadata unavailable for %s: %v\n", key.shed, err)
				}
				return
			}
			// Distinct indices per (server, shed) -> safe concurrent writes.
			for _, i := range idxs {
				if dto, ok := byTmux[sessions[i].Name]; ok {
					sessions[i].RC = dto.toSessionRC()
				}
			}
		}(key, idxs, entry)
	}
	wg.Wait()
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
		// The shed's sshd folds the remote command's stderr into stdout, so prefer
		// stderr but fall back to stdout for the diagnostic (e.g. a guest binary's
		// "flag provided but not defined: …" arrives on stdout).
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

// isOldBinaryPermModeErr reports whether a createRCSession error is the shed's
// shed-ext-rc rejecting --permission-mode as an unknown flag (Go's flag package
// prints "flag provided but not defined: -permission-mode"), i.e. an image that
// predates posture support. Matched precisely so a transient or unrelated failure
// is never mistaken for an old binary.
func isOldBinaryPermModeErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "permission-mode") &&
		(strings.Contains(s, "not defined") || strings.Contains(s, "flag provided"))
}

// streamPlanToShed writes plan content to `<workdir>/.shed/plan-<slug>.md` inside the
// shed (workdir = $SHED_WORKSPACE, falling back to $HOME — matching how shed-ext-rc
// resolves its own workdir) and returns the workdir-relative path for the kickoff
// prompt (claude runs with cwd = workdir).
func streamPlanToShed(shedName string, entry *config.ServerEntry, slug, content string) (string, error) {
	rel := ".shed/plan-" + slug + ".md"
	ctx, cancel := context.WithTimeout(context.Background(), rcEnrichTimeout)
	defer cancel()
	// The server's bash -lc expands the shed's login env; mirror shed-ext-rc's
	// SHED_WORKSPACE-or-HOME workdir resolution so the plan lands in the session cwd.
	cmd := `wd="${SHED_WORKSPACE:-$HOME}"; mkdir -p "$wd/.shed" && cat > "$wd/` + rel + `"`
	if _, err := sshShell(ctx, shedName, entry, content, cmd); err != nil {
		return "", fmt.Errorf("shipping plan to shed: %w", err)
	}
	return rel, nil
}

// rcCreateOptions configures createRCSession.
type rcCreateOptions struct {
	shedName       string
	entry          *config.ServerEntry
	kind           string
	displayName    string
	slug           string
	permissionMode string // "" omits the flag
	prompt         string // kickoff line (empty -> none)
}

// createRCSession invokes `shed-ext-rc create --wait` over SSH and returns the
// parsed session DTO. Each shed-ext-rc argument is shell-quoted into one command
// string (the server re-parses through bash -lc), and the kickoff prompt is piped on
// stdin (never argv) per the convention.
func createRCSession(opts rcCreateOptions) (rcSessionDTO, error) {
	argv := []string{
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
	if opts.permissionMode != "" {
		argv = append(argv, "--permission-mode", opts.permissionMode)
	}
	if opts.prompt != "" {
		argv = append(argv, "--prompt-stdin")
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuoteArg(a)
	}

	ctx, cancel := context.WithTimeout(context.Background(), rcCreateTimeout)
	defer cancel()
	out, err := sshShell(ctx, opts.shedName, opts.entry, opts.prompt, strings.Join(quoted, " "))
	if err != nil {
		return rcSessionDTO{}, fmt.Errorf("shed-ext-rc create: %w", err)
	}
	var dto rcSessionDTO
	if err := json.Unmarshal(bytes.TrimSpace(out), &dto); err != nil {
		return rcSessionDTO{}, fmt.Errorf("decoding shed-ext-rc create output: %w (raw: %q)", err, string(out))
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
