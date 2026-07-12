package clirc

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/ext/rc"
)

var (
	extCfg     = Config{ProgName: "shed-ext-rc", DefaultCreatedBy: "shed-ext-rc"}
	machineCfg = Config{ProgName: "shed-machine-rc", DefaultCreatedBy: "shed-machine-rc", EnableClaudeVerb: true}
)

// fakeRunner records every tmux invocation and returns canned results, so the
// dispatch is exercised end-to-end (flags → rc options → tmux argv) without a real
// tmux or any filesystem/network side effect beyond the injected temp HOME.
type fakeRunner struct {
	calls      [][]string
	pane       string     // capture-pane stdout
	env        string     // show-environment stdout
	lsOut      string     // `tmux ls` stdout (session names, one per line)
	newSessErr *rc.Result // returned for new-session when set
	captErr    *rc.Result // returned for capture-pane when set
}

func (f *fakeRunner) Run(args ...string) rc.Result {
	f.calls = append(f.calls, append([]string(nil), args...))
	switch args[0] {
	case "new-session":
		if f.newSessErr != nil {
			return *f.newSessErr
		}
	case "ls":
		return rc.Result{Stdout: f.lsOut}
	case "capture-pane":
		if f.captErr != nil {
			return *f.captErr
		}
		return rc.Result{Stdout: f.pane}
	case "show-environment":
		return rc.Result{Stdout: f.env}
	}
	return rc.Result{}
}

// runCLI dispatches one command with fully-faked deps and returns (exit, stdout,
// stderr). The agent probe is a no-op fake (nothing installed) so list/capabilities
// never spawn a real process; use runCLIProbe to inject probe results.
func runCLI(cfg Config, r rc.Runner, env map[string]string, stdin string, args ...string) (int, string, string) {
	return runCLIProbe(cfg, r, env, stdin, func(string) rc.AgentInfo { return rc.AgentInfo{} }, args...)
}

// runCLIProbe is runCLI with an injectable agent probe for the capabilities paths.
func runCLIProbe(cfg Config, r rc.Runner, env map[string]string, stdin string, probe rc.AgentProbe, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	d := deps{
		runner:   r,
		getenv:   func(k string) string { return env[k] },
		stdin:    strings.NewReader(stdin),
		stdout:   &out,
		stderr:   &errb,
		hostname: func() string { return "testhost" },
		sleep:    func(time.Duration) {},
		probe:    probe,
	}
	return run(cfg, d, args), out.String(), errb.String()
}

func newSessionCall(t *testing.T, r *fakeRunner) []string {
	t.Helper()
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "new-session" {
			return c
		}
	}
	t.Fatal("no new-session call recorded")
	return nil
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestVersionCarriesProgName(t *testing.T) {
	for _, cfg := range []Config{extCfg, machineCfg} {
		code, out, _ := runCLI(cfg, &fakeRunner{}, nil, "", "version")
		if code != 0 {
			t.Fatalf("%s version: code=%d", cfg.ProgName, code)
		}
		if !strings.HasPrefix(out, cfg.ProgName+" ") {
			t.Errorf("version output %q missing prog name %q", out, cfg.ProgName)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, errOut := runCLI(machineCfg, &fakeRunner{}, nil, "", "bogus")
	if code != 2 {
		t.Fatalf("code=%d, want 2", code)
	}
	if !strings.Contains(errOut, `unknown command "bogus"`) {
		t.Errorf("stderr=%q", errOut)
	}
}

func TestHelpClaudeVerbGating(t *testing.T) {
	_, _, extErr := runCLI(extCfg, &fakeRunner{}, nil, "", "help")
	if strings.Contains(extErr, "  claude ") {
		t.Errorf("shed-ext-rc help must not list the claude verb:\n%s", extErr)
	}
	_, _, machErr := runCLI(machineCfg, &fakeRunner{}, nil, "", "help")
	if !strings.Contains(machErr, "claude") {
		t.Errorf("shed-machine-rc help must list the claude verb:\n%s", machErr)
	}
}

func TestClaudeVerbDisabledOnExtRc(t *testing.T) {
	code, _, errOut := runCLI(extCfg, &fakeRunner{}, nil, "", "claude")
	if code != 2 {
		t.Fatalf("code=%d, want 2", code)
	}
	if !strings.Contains(errOut, `unknown command "claude"`) {
		t.Errorf("stderr=%q", errOut)
	}
}

func TestSkipMutualExclusion(t *testing.T) {
	for _, args := range [][]string{
		{"create", "--kind", "claude-rc", "--skip", "--permission-mode", "plan"},
		{"claude", "--skip", "--permission-mode", "plan"},
	} {
		code, _, errOut := runCLI(machineCfg, &fakeRunner{}, nil, "", args...)
		if code != 2 {
			t.Fatalf("%v: code=%d, want 2", args, code)
		}
		if !strings.Contains(errOut, "mutually exclusive") {
			t.Errorf("%v: stderr=%q", args, errOut)
		}
	}
}

func TestSlugRequired(t *testing.T) {
	for _, cmd := range []string{"probe", "accept-trust", "kill"} {
		code, _, errOut := runCLI(machineCfg, &fakeRunner{}, nil, "", cmd)
		if code != 2 {
			t.Fatalf("%s: code=%d, want 2", cmd, code)
		}
		if !strings.Contains(errOut, "--slug is required") {
			t.Errorf("%s: stderr=%q", cmd, errOut)
		}
	}
}

func TestRejectsExtraPositionalArgs(t *testing.T) {
	for _, args := range [][]string{
		{"list", "extra"},
		{"kill", "--slug", "abc123", "typo"},
		{"create", "--kind", "shell", "stray"},
	} {
		code, _, errOut := runCLI(machineCfg, &fakeRunner{}, nil, "", args...)
		if code != 2 {
			t.Fatalf("%v: code=%d, want 2", args, code)
		}
		if !strings.Contains(errOut, "unexpected argument") {
			t.Errorf("%v: stderr=%q", args, errOut)
		}
	}
}

// The default created-by is resolved in clirc (NOT internal/rc's ToolName fallback),
// so each binary stamps its own provenance.
func TestCreateCreatedByDefaultPerBinary(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{extCfg, "SHED_RC_CREATED_BY=shed-ext-rc"},
		{machineCfg, "SHED_RC_CREATED_BY=shed-machine-rc"},
	} {
		r := &fakeRunner{}
		code, _, errOut := runCLI(tc.cfg, r, nil, "", "create", "--kind", "shell", "--slug", "abc123", "--workdir", "/tmp")
		if code != 0 {
			t.Fatalf("%s: code=%d stderr=%q", tc.cfg.ProgName, code, errOut)
		}
		if ns := newSessionCall(t, r); !containsArg(ns, tc.want) {
			t.Errorf("%s: new-session missing %q; got %v", tc.cfg.ProgName, tc.want, ns)
		}
	}
}

// An explicit --created-by overrides the per-binary default.
func TestCreateCreatedByExplicit(t *testing.T) {
	r := &fakeRunner{}
	code, _, errOut := runCLI(machineCfg, r, nil, "",
		"create", "--kind", "shell", "--slug", "abc123", "--workdir", "/tmp", "--created-by", "shed-remote-agent/9.9")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if ns := newSessionCall(t, r); !containsArg(ns, "SHED_RC_CREATED_BY=shed-remote-agent/9.9") {
		t.Errorf("explicit created-by not honored; got %v", ns)
	}
}

func TestCreatePromptStdinEmpty(t *testing.T) {
	code, _, errOut := runCLI(machineCfg, &fakeRunner{}, nil, "",
		"create", "--kind", "claude-rc", "--slug", "abc123", "--workdir", "/tmp", "--prompt-stdin")
	if code != 2 {
		t.Fatalf("code=%d, want 2", code)
	}
	if !strings.Contains(errOut, "stdin is empty") {
		t.Errorf("stderr=%q", errOut)
	}
}

func TestCreateDuplicateSlugExit3(t *testing.T) {
	r := &fakeRunner{newSessErr: &rc.Result{Code: 1, Stderr: "duplicate session: rc-abc123"}}
	code, _, _ := runCLI(machineCfg, r, nil, "", "create", "--kind", "shell", "--slug", "abc123", "--workdir", "/tmp")
	if code != 3 {
		t.Fatalf("want exit 3 (duplicate slug), got %d", code)
	}
}

func TestProbeNotFoundExit4(t *testing.T) {
	r := &fakeRunner{captErr: &rc.Result{Code: 1, Stderr: "can't find pane: rc-abc123"}}
	code, _, _ := runCLI(machineCfg, r, nil, "", "probe", "--slug", "abc123")
	if code != 4 {
		t.Fatalf("want exit 4 (session not found), got %d", code)
	}
}

// The claude convenience verb resolves to an autonomous claude-rc session: kind
// claude-rc, --permission-mode auto by default, interactive-shell on, wait on, with a
// <hostname>/<slug> display name and a human-facing unattended-warning summary.
func TestClaudeVerbDefaults(t *testing.T) {
	home := t.TempDir() // PreseedClaudeConfig writes $HOME/.claude.json — keep it off the real home
	r := &fakeRunner{pane: "Remote Control active\nhttps://claude.ai/code/session_TEST123\n"}
	code, out, errOut := runCLI(machineCfg, r, map[string]string{"HOME": home}, "", "claude", "--slug", "abc123")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	ns := newSessionCall(t, r)
	inner := ns[len(ns)-1]
	for _, want := range []string{"bash -ic", "--remote-control", "--permission-mode auto", "testhost/abc123"} {
		if !strings.Contains(inner, want) {
			t.Errorf("inner command missing %q:\n%s", want, inner)
		}
	}
	if !containsArg(ns, "SHED_RC_KIND=claude-rc") {
		t.Errorf("session kind is not claude-rc: %v", ns)
	}
	if !strings.Contains(out, "UNATTENDED") {
		t.Errorf("summary missing the unattended warning:\n%s", out)
	}
	if !strings.Contains(out, "session_TEST123") {
		t.Errorf("summary missing the session URL:\n%s", out)
	}
}

func TestClaudeVerbSkipUsesBypass(t *testing.T) {
	home := t.TempDir()
	r := &fakeRunner{pane: "Remote Control active\nhttps://claude.ai/code/session_X\n"}
	code, _, errOut := runCLI(machineCfg, r, map[string]string{"HOME": home}, "", "claude", "--slug", "abc123", "--skip")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	ns := newSessionCall(t, r)
	if inner := ns[len(ns)-1]; !strings.Contains(inner, "--permission-mode bypassPermissions") {
		t.Errorf("--skip did not produce bypassPermissions:\n%s", inner)
	}
}

// fakeProbe returns canned install/version info per binary for the capabilities tests.
func fakeProbe(installed map[string]string) rc.AgentProbe {
	return func(bin string) rc.AgentInfo {
		if v, ok := installed[bin]; ok {
			return rc.AgentInfo{Installed: true, Version: v}
		}
		return rc.AgentInfo{Installed: false}
	}
}

func TestCapabilitiesSubcommand(t *testing.T) {
	probe := fakeProbe(map[string]string{"claude": "2.1.206", "codex": "0.143.0"})
	code, out, errOut := runCLIProbe(extCfg, &fakeRunner{}, nil, "", probe, "capabilities")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	var caps rc.Capabilities
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&caps); err != nil {
		t.Fatalf("capabilities output failed to decode: %v\n%s", err, out)
	}
	if caps.RCVersion != rc.CapabilityVersion {
		t.Errorf("rc_version = %d, want %d", caps.RCVersion, rc.CapabilityVersion)
	}
	if len(caps.Kinds) != len(rc.KindStrings()) {
		t.Errorf("kinds = %v, want %d", caps.Kinds, len(rc.KindStrings()))
	}
	if info := caps.Agents["codex"]; !info.Installed || info.Version != "0.143.0" {
		t.Errorf("codex probe wrong: %+v", info)
	}
	if info := caps.Agents["cursor"]; info.Installed || info.Version != "" {
		t.Errorf("uninstalled cursor should have no version: %+v", info)
	}
	if _, ok := caps.Agents["shell"]; ok {
		t.Errorf("shell has no agent binary and must not appear in agents{}")
	}
}

func TestListEnvelopeCarriesCapabilities(t *testing.T) {
	// A running shed with one rc-* session; probe reports codex installed.
	r := &fakeRunner{
		pane: "Remote Control active\nhttps://claude.ai/code/session_01\n",
		env:  "SHED_RC_V=2\nSHED_RC_KIND=claude-rc",
	}
	r.lsOut = "rc-aaa\n"
	probe := fakeProbe(map[string]string{"codex": "0.143.0"})
	code, out, errOut := runCLIProbe(extCfg, r, nil, "", probe, "list")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	var resp rc.ListResponse
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("list output failed to decode: %v\n%s", err, out)
	}
	if resp.Capabilities == nil {
		t.Fatal("list envelope must carry capabilities")
	}
	if resp.Capabilities.RCVersion != rc.CapabilityVersion {
		t.Errorf("rc_version = %d, want %d", resp.Capabilities.RCVersion, rc.CapabilityVersion)
	}
}

func TestClaudeVerbNeedsAuthSummary(t *testing.T) {
	home := t.TempDir()
	r := &fakeRunner{pane: "You are not logged in. Run claude auth login.\n"}
	code, out, errOut := runCLI(machineCfg, r, map[string]string{"HOME": home}, "", "claude", "--slug", "abc123")
	if code != 1 {
		t.Fatalf("needs-auth should exit 1 (no usable URL), got code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "not logged in") {
		t.Errorf("needs-auth state should surface a login hint:\n%s", out)
	}
}

// --- plan delivery (create --plan-stdin) ------------------------------------

// TestCreatePlanStdin is the shed-machine-rc verification in unit form: the shared
// clirc `create --plan-stdin` reads a plan from stdin, writes it to the per-kind
// HOME-rooted file (0600), and returns the session DTO. Driven with machineCfg (the
// shed-machine-rc identity) so it proves the machine binary inherits the verb.
func TestCreatePlanStdin(t *testing.T) {
	home := t.TempDir()
	r := &fakeRunner{pane: "[shed:x] ~ $"} // non-empty → shell reaches ready
	plan := "# Plan\n\nStep one.\n"
	code, out, errOut := runCLI(machineCfg, r, map[string]string{"HOME": home}, plan,
		"create", "--kind", "shell", "--slug", "abc123", "--plan-stdin")
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	var dto rc.Session
	if err := json.Unmarshal([]byte(out), &dto); err != nil {
		t.Fatalf("decoding create output: %v\n%s", err, out)
	}
	if dto.Slug != "abc123" || dto.Kind != rc.KindShell {
		t.Fatalf("unexpected DTO: %+v", dto)
	}
	// shell is not a claude kind → ~/.shed-plans.
	path := filepath.Join(home, ".shed-plans", "plan-abc123.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("plan not written to %s: %v", path, err)
	}
	if string(data) != plan {
		t.Errorf("plan contents = %q, want %q", data, plan)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("plan mode = %o, want 600", info.Mode().Perm())
	}
}

// TestCreatePlanStdinPromptB64 decodes base64 framing and prepends it to the kickoff.
func TestCreatePlanStdinPromptB64(t *testing.T) {
	home := t.TempDir()
	r := &fakeRunner{pane: "[shed:x] ~ $"}
	framing := base64.StdEncoding.EncodeToString([]byte("focus on X"))
	code, _, errOut := runCLI(machineCfg, r, map[string]string{"HOME": home}, "plan body",
		"create", "--kind", "shell", "--slug", "abc123", "--plan-stdin", "--prompt-b64", framing)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	// The delivered kickoff (bracketed paste buffer) leads with the framing.
	var buf []string
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == "set-buffer" {
			buf = c
		}
	}
	if buf == nil {
		t.Fatal("no kickoff delivered")
	}
	if sent := buf[len(buf)-1]; !strings.HasPrefix(sent, "focus on X\n\n") {
		t.Errorf("framing not prepended: %q", sent)
	}
}

func TestCreatePlanStdinFlagErrors(t *testing.T) {
	home := t.TempDir()
	cases := []struct {
		name    string
		stdin   string
		args    []string
		wantSub string
	}{
		{
			"plan and prompt stdin mutually exclusive", "x",
			[]string{"create", "--kind", "shell", "--slug", "abc123", "--plan-stdin", "--prompt-stdin"},
			"mutually exclusive",
		},
		{
			"prompt-b64 requires plan-stdin", "",
			[]string{"create", "--kind", "shell", "--slug", "abc123", "--prompt-b64", "eA=="},
			"only valid with --plan-stdin",
		},
		{
			"empty plan stdin", "",
			[]string{"create", "--kind", "shell", "--slug", "abc123", "--plan-stdin"},
			"stdin is empty",
		},
		{
			"non-UTF8 plan stdin", "a\xffb",
			[]string{"create", "--kind", "shell", "--slug", "abc123", "--plan-stdin"},
			"not valid UTF-8",
		},
		{
			"bad base64 framing", "plan body",
			[]string{"create", "--kind", "shell", "--slug", "abc123", "--plan-stdin", "--prompt-b64", "not!base64!"},
			"not valid base64",
		},
		{
			// base64 "mw==" decodes to the lone C1 byte 0x9b — invalid UTF-8 that
			// would read as RuneError (and slip past the rune-based control-char
			// scan) if converted to string before the utf8.Valid gate.
			"non-UTF8 framing bytes rejected", "plan body",
			[]string{"create", "--kind", "shell", "--slug", "abc123", "--plan-stdin", "--prompt-b64", "mw=="},
			"not decode to valid UTF-8",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := runCLI(machineCfg, &fakeRunner{pane: "[shed:x] ~ $"},
				map[string]string{"HOME": home}, tc.stdin, tc.args...)
			if code != 2 {
				t.Fatalf("code=%d, want 2; stderr=%q", code, errOut)
			}
			if !strings.Contains(errOut, tc.wantSub) {
				t.Errorf("stderr=%q, want substring %q", errOut, tc.wantSub)
			}
		})
	}
}

// TestParseVersionFromLoginShell pins the login-shell noise handling: profile
// activation output (e.g. mise printing its own version) precedes the command's
// answer on stdout, and a whole-output first-token scan would take the noise
// version. The parse must anchor to the LAST non-empty line.
func TestParseVersionFromLoginShell(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "noisy profile before the real version",
			out:  "mise 2026.7.0 activating\nmise: using node@24.13.1\ncodex-cli 0.144.1\n",
			want: "0.144.1",
		},
		{
			name: "noise with trailing blank lines",
			out:  "mise 2026.7.0\n\n1.17.18\n\n\n",
			want: "1.17.18",
		},
		{
			name: "clean single-line output",
			out:  "2.1.206 (Claude Code)\n",
			want: "2.1.206",
		},
		{
			name: "leading v build-suffix form",
			out:  "profile: PATH updated\nv2026.07.09-a3815c0\n",
			want: "2026.07.09-a3815c0",
		},
		{
			name: "empty output",
			out:  "\n\n",
			want: "",
		},
		{
			name: "last line has no version token falls back to that line",
			out:  "mise 2026.7.0\nunknown output\n",
			want: "unknown output",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVersionFromLoginShell(tc.out); got != tc.want {
				t.Errorf("parseVersionFromLoginShell(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

// TestServeDispatch checks the `serve` subcommand's local validation: --detach and
// --foreground are mutually exclusive (rejected before any hub side effect), and an
// unexpected positional argument is rejected. The bind/run paths are covered by the
// hub package's own tests (they need an injectable address).
func TestServeDispatch(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantErrSub string // substring required in stderr ("" = not checked)
	}{
		{
			name:       "mutually exclusive detach and foreground",
			args:       []string{"serve", "--detach", "--foreground"},
			wantCode:   2,
			wantErrSub: "mutually exclusive",
		},
		{
			name:     "unexpected positional argument",
			args:     []string{"serve", "stray"},
			wantCode: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No ensureHub wired → a bad-args rejection can have no hub side effect.
			code, _, errOut := runCLI(extCfg, &fakeRunner{}, nil, "", tc.args...)
			if code != tc.wantCode {
				t.Fatalf("%v exit = %d, want %d (stderr %q)", tc.args, code, tc.wantCode, errOut)
			}
			if tc.wantErrSub != "" && !strings.Contains(errOut, tc.wantErrSub) {
				t.Fatalf("stderr = %q, want substring %q", errOut, tc.wantErrSub)
			}
		})
	}
}

// TestCreateDoesNotSpawnHubInTests guards the ensureHub gate: dispatch-level create
// (with no ensureHub wired, as in every test) must not attempt to spawn the daemon.
func TestCreateDoesNotSpawnHubInTests(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{
			name: "create shell session does not spawn hub",
			args: []string{"create", "--kind", "shell", "--slug", "abc123"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{}
			env := map[string]string{"HOME": t.TempDir()}
			code, _, errOut := runCLI(extCfg, r, env, "", tc.args...)
			if code != 0 {
				t.Fatalf("create exit = %d (stderr %q), want 0", code, errOut)
			}
			// A hub spawn would shell out to the real binary; the fake runner only sees
			// tmux calls, so reaching exit 0 with no panic confirms no daemon side effect.
		})
	}
}
