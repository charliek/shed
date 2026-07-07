package rc

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Tests drive a fake tmux, so the real inter-key settle would only slow them down.
func init() { sendLineSettle = 0 }

// fakeTmux is an injectable Runner that records calls and answers via a handler.
type fakeTmux struct {
	calls   [][]string
	handler func(args []string) Result
}

func (f *fakeTmux) Run(args ...string) Result {
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.handler != nil {
		return f.handler(args)
	}
	return Result{}
}

func (f *fakeTmux) callWith(first string) []string {
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == first {
			return c
		}
	}
	return nil
}

func TestGenSlug(t *testing.T) {
	for range 50 {
		s, err := GenSlug()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 6 {
			t.Fatalf("slug %q len = %d, want 6", s, len(s))
		}
		for _, c := range s {
			if !strings.ContainsRune(slugAlphabet, c) {
				t.Fatalf("slug %q has confusable/invalid char %q", s, c)
			}
		}
		if !ValidCallerSlug(s) {
			t.Fatalf("generated slug %q fails the caller grammar", s)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":     "'plain'",
		"two words": "'two words'",
		"it's mine": `'it'\''s mine'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInnerCommand(t *testing.T) {
	cases := []struct {
		kind        Kind
		name        string
		permMode    string
		interactive bool
		want        string
	}{
		// No permission mode -> original, backward-compatible forms.
		{KindClaudeBroker, "my-shed/abc", "", false, "claude remote-control --name 'my-shed/abc' --spawn same-dir"},
		{KindClaudeRC, "my-shed/abc", "", false, "claude --name 'my-shed/abc' /rc"},
		{KindShell, "my-shed/abc", "", false, "bash -l"},
		{KindClaudeRC, "Friday Bug Fix", "", false, "claude --name 'Friday Bug Fix' /rc"},
		{KindClaudeRC, "x", "", true, `bash -ic 'claude --name '\''x'\'' /rc'`},
		{KindShell, "x", "", true, "bash -l"}, // shell ignores interactive wrap
		// With a permission mode -> claude-rc switches to the --remote-control form.
		{KindClaudeRC, "my-shed/abc", "auto", false, "claude --remote-control --name 'my-shed/abc' --permission-mode auto"},
		{KindClaudeRC, "x", "bypassPermissions", false, "claude --remote-control --name 'x' --permission-mode bypassPermissions"},
		{KindClaudeBroker, "b", "auto", false, "claude remote-control --name 'b' --permission-mode auto --spawn same-dir"},
		{KindClaudeRC, "x", "auto", true, `bash -ic 'claude --remote-control --name '\''x'\'' --permission-mode auto'`},
		{KindShell, "x", "bypassPermissions", false, "bash -l"}, // shell ignores mode
	}
	for _, c := range cases {
		if got := InnerCommand(c.kind, c.name, c.permMode, c.interactive); got != c.want {
			t.Errorf("InnerCommand(%s,%q,%q,%v) = %q, want %q", c.kind, c.name, c.permMode, c.interactive, got, c.want)
		}
	}
}

func TestValidPermissionMode(t *testing.T) {
	for _, m := range []string{"default", "acceptEdits", "plan", "auto", "dontAsk", "bypassPermissions"} {
		if !ValidPermissionMode(m) {
			t.Errorf("ValidPermissionMode(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"", "yolo", "Auto", "bypass", "skip"} {
		if ValidPermissionMode(m) {
			t.Errorf("ValidPermissionMode(%q) = true, want false", m)
		}
	}
}

func TestIsBypassAcceptPrompt(t *testing.T) {
	yes := "WARNING: Claude Code running in Bypass Permissions mode\n  1. No, exit\n  2. Yes, I accept\n"
	if !IsBypassAcceptPrompt(yes) {
		t.Error("want bypass-accept prompt detected")
	}
	for _, pane := range []string{
		"",
		"Workspace not trusted",
		"Bypass Permissions mode", // warning without the accept option
		"2. Yes, I accept",        // accept option without the bypass warning
	} {
		if IsBypassAcceptPrompt(pane) {
			t.Errorf("false positive on %q", pane)
		}
	}
}

func TestClassifyPane(t *testing.T) {
	cases := []struct {
		name      string
		kind      Kind
		pane      string
		wantState State
		wantURL   string
	}{
		{"broker ready+url", KindClaudeBroker,
			"·✔︎· Connected · my-shed\nhttps://claude.ai/code?environment=env_01ABC", StateReady,
			"https://claude.ai/code?environment=env_01ABC"},
		{"broker reconnecting", KindClaudeBroker, "·|· Reconnecting · retrying", StateReconnecting, ""},
		{"broker needs-trust", KindClaudeBroker, "Error: Workspace not trusted. run claude", StateNeedsTrust, ""},
		{"broker needs-auth", KindClaudeBroker, "Remote Control requires a claude.ai subscription.", StateNeedsAuth, ""},
		{"broker starting", KindClaudeBroker, "booting...", StateStarting, ""},
		{"rc ready", KindClaudeRC,
			"/remote-control is active · https://claude.ai/code/session_01RC\nRemote Control active", StateReady,
			"https://claude.ai/code/session_01RC"},
		{"rc connecting", KindClaudeRC, "❯ /remote-control\n  ⎿  Remote Control connecting…", StateStarting, ""},
		{"rc needs-trust quick-check", KindClaudeRC, "Quick safety check: is this a project", StateNeedsTrust, ""},
		{"rc starting", KindClaudeRC, `❯ Try "fix typecheck errors"`, StateStarting, ""},
		{"rc ignores broker url", KindClaudeRC, "banner https://claude.ai/code?environment=env_01ABC", StateStarting, ""},
		{"shell ready", KindShell, "charliek@shed:~$ ", StateReady, ""},
		{"shell starting", KindShell, "   \n  ", StateStarting, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, url := ClassifyPane(c.kind, c.pane)
			if state != c.wantState || url != c.wantURL {
				t.Errorf("ClassifyPane = (%s,%q), want (%s,%q)", state, url, c.wantState, c.wantURL)
			}
		})
	}
}

func TestParseKind(t *testing.T) {
	cases := map[string]Kind{
		"claude-broker": KindClaudeBroker,
		"claude-rc":     KindClaudeRC,
		"shell":         KindShell,
		"agent":         KindClaudeBroker, // legacy v1 value → unrecognized → broker default
		"repl":          KindClaudeBroker,
		"":              KindClaudeBroker,
		"opencode-rc":   KindClaudeBroker, // future/foreign → broker default
	}
	for in, want := range cases {
		if got := parseKind(in); got != want {
			t.Errorf("parseKind(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestIsManagedVersion(t *testing.T) {
	managed := []string{"2", "3", "10"}
	unmanaged := []string{"1", "0", "", "1.0", "1e3", "0x1", " 2", "+2", "abc"}
	for _, v := range managed {
		if !isManagedVersion(v) {
			t.Errorf("isManagedVersion(%q) = false, want true", v)
		}
	}
	for _, v := range unmanaged {
		if isManagedVersion(v) {
			t.Errorf("isManagedVersion(%q) = true, want false", v)
		}
	}
}

func TestBuildEnvArgsRoundTrip(t *testing.T) {
	m := Metadata{
		ID: "id-1", DisplayName: "Friday Bug Fix", Kind: KindClaudeRC,
		Workdir: "/home/shed/proj", CreatedBy: "shed-ext-rc/0.5.0",
		CreatedAt: "2026-06-19T18:53:00Z", Target: "shed:t1@host",
	}
	args, err := BuildEnvArgs(m)
	if err != nil {
		t.Fatal(err)
	}
	// Reassemble a show-environment dump from the -e KEY=value pairs.
	var dump strings.Builder
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "-e" {
			dump.WriteString(args[i+1])
			dump.WriteByte('\n')
		}
	}
	s := ParseSession(TmuxName("abc"), dump.String(), "", nil)
	if !s.Managed || s.Kind != KindClaudeRC || s.DisplayName != "Friday Bug Fix" ||
		s.Workdir != "/home/shed/proj" || s.ID != "id-1" || s.CreatedBy != "shed-ext-rc/0.5.0" ||
		s.CreatedAt != "2026-06-19T18:53:00Z" || s.TargetLabel != "shed:t1@host" {
		t.Fatalf("round-trip mismatch: %+v", s)
	}
}

func TestBuildEnvArgsRejectsControlChars(t *testing.T) {
	_, err := BuildEnvArgs(Metadata{ID: "x", DisplayName: "a\nb", Kind: KindShell, Workdir: "/x", CreatedBy: "t/1", CreatedAt: "2026-06-19T18:53:00Z"})
	if err == nil {
		t.Fatal("expected control-char rejection")
	}
}

func TestParseSessionUnmanaged(t *testing.T) {
	// Legacy: no SHED_RC_V → unmanaged, kind=broker, stored values ignored.
	s := ParseSession("rc-legacy", "SHED_RC_KIND=shell\nSHED_RC_DISPLAY_NAME=spoof", "charliek@shed:~$ ",
		func(slug string) string { return "fb/" + slug })
	if s.Managed || s.Kind != KindClaudeBroker || s.DisplayName != "fb/legacy" {
		t.Fatalf("unmanaged parse wrong: %+v", s)
	}
}

func TestParseSessionV1IsUnmanaged(t *testing.T) {
	// A v1 session (below the floor) is treated as unmanaged — no aliasing.
	s := ParseSession("rc-old", "SHED_RC_V=1\nSHED_RC_KIND=claude-rc", "", nil)
	if s.Managed {
		t.Fatalf("v1 session should be unmanaged, got %+v", s)
	}
}

func TestParseSessionOmitsDisplayNameWhenUnstored(t *testing.T) {
	// nil fallback + no stored name → empty display_name (omitted from the DTO so
	// the app applies its own fallback).
	s := ParseSession("rc-brk900", "SHED_RC_V=2\nSHED_RC_KIND=claude-broker", "", nil)
	if s.DisplayName != "" {
		t.Fatalf("display_name should be empty/omitted, got %q", s.DisplayName)
	}
}

// --- ops (fake tmux) ---

func TestCreateSuccess(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) Result { return Result{Code: 0} }}
	s, err := Create(f, func(string) string { return "/home/shed" }, CreateOptions{
		Kind: KindClaudeRC, DisplayName: "demo", Slug: "abc123", CreatedBy: "shed-ext-rc/0.5.0",
	}, noSleep)
	if err != nil {
		t.Fatal(err)
	}
	if s.Slug != "abc123" || s.TmuxSession != "rc-abc123" || s.Kind != KindClaudeRC ||
		s.State != StateStarting || !s.Managed || s.Workdir != "/home/shed" || s.ID == "" {
		t.Fatalf("unexpected session: %+v", s)
	}
	ns := f.callWith("new-session")
	if ns == nil {
		t.Fatal("no new-session call")
	}
	joined := strings.Join(ns, " ")
	if !strings.Contains(joined, "-s rc-abc123") || !strings.Contains(joined, "-c /home/shed") ||
		!strings.Contains(joined, "SHED_RC_V=2") || !strings.Contains(joined, "SHED_RC_KIND=claude-rc") ||
		ns[len(ns)-1] != "claude --name 'demo' /rc" {
		t.Fatalf("new-session argv wrong: %v", ns)
	}
}

func TestCreateDuplicateSlug(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) Result {
		return Result{Code: 1, Stderr: "duplicate session: rc-abc123"}
	}}
	_, err := Create(f, func(string) string { return "/home/shed" },
		CreateOptions{Kind: KindShell, Slug: "abc123"}, noSleep)
	if !errors.Is(err, ErrDuplicateSlug) {
		t.Fatalf("want ErrDuplicateSlug, got %v", err)
	}
}

func TestCreatePromptOnBrokerRejected(t *testing.T) {
	f := &fakeTmux{}
	_, err := Create(f, func(string) string { return "/home/shed" },
		CreateOptions{Kind: KindClaudeBroker, Prompt: "hi"}, noSleep)
	if !errors.Is(err, ErrBadArgs) {
		t.Fatalf("want ErrBadArgs, got %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatal("should not have touched tmux on a validation failure")
	}
}

func TestCreateUnsafePromptRejected(t *testing.T) {
	// A newline is now allowed (multi-line prompt); an ESC (or other control char)
	// is rejected so a paste can't break out of the bracketed paste.
	_, err := Create(&fakeTmux{}, func(string) string { return "/home/shed" },
		CreateOptions{Kind: KindClaudeRC, Prompt: "a\x1bb"}, noSleep)
	if !errors.Is(err, ErrBadArgs) {
		t.Fatalf("want ErrBadArgs, got %v", err)
	}
}

func TestPromptCharGuards(t *testing.T) {
	t.Run("HasUnsafePromptChars allows newline/tab, rejects others", func(t *testing.T) {
		for _, ok := range []string{"a\nb", "a\tb", "plain", "multi\nline\nplan"} {
			if HasUnsafePromptChars(ok) {
				t.Errorf("%q should be allowed", ok)
			}
		}
		for _, bad := range []string{"a\x1bb", "a\x00b", "a\rb", "bell\x07", "a\u009bb", "c\u0080"} {
			if !HasUnsafePromptChars(bad) {
				t.Errorf("%q should be rejected", bad)
			}
		}
	})
	t.Run("NormalizeNewlines collapses CR/CRLF", func(t *testing.T) {
		if got := NormalizeNewlines("a\r\nb\rc"); got != "a\nb\nc" {
			t.Errorf("got %q", got)
		}
	})
}

func TestSendLineMultilineUsesBracketedPaste(t *testing.T) {
	t.Run("multi-line -> set-buffer + paste-buffer", func(t *testing.T) {
		f := &fakeTmux{}
		sendLine(f, "rc-x", "line one\nline two")
		var verbs []string
		for _, c := range f.calls {
			if len(c) > 0 {
				verbs = append(verbs, c[0])
			}
		}
		joined := strings.Join(verbs, ",")
		if !strings.Contains(joined, "set-buffer") || !strings.Contains(joined, "paste-buffer") {
			t.Fatalf("multi-line should paste via buffer, got calls: %v", f.calls)
		}
		if strings.Contains(joined, "send-keys") && containsArg(f.calls, "-l") {
			t.Fatalf("multi-line should not type with send-keys -l: %v", f.calls)
		}
	})
	t.Run("single-line -> send-keys -l", func(t *testing.T) {
		f := &fakeTmux{}
		sendLine(f, "rc-x", "just one line")
		if !containsArg(f.calls, "-l") {
			t.Fatalf("single-line should use send-keys -l, got: %v", f.calls)
		}
	})
}

func containsArg(calls [][]string, arg string) bool {
	for _, c := range calls {
		for _, a := range c {
			if a == arg {
				return true
			}
		}
	}
	return false
}

func TestCreatePermissionModeValidation(t *testing.T) {
	t.Run("invalid mode rejected", func(t *testing.T) {
		f := &fakeTmux{}
		_, err := Create(f, func(string) string { return "/home/shed" },
			CreateOptions{Kind: KindClaudeRC, PermissionMode: "yolo"}, noSleep)
		if !errors.Is(err, ErrBadArgs) {
			t.Fatalf("want ErrBadArgs, got %v", err)
		}
		if len(f.calls) != 0 {
			t.Fatal("should not have touched tmux on a validation failure")
		}
	})
	t.Run("mode on shell kind rejected", func(t *testing.T) {
		_, err := Create(&fakeTmux{}, func(string) string { return "/home/shed" },
			CreateOptions{Kind: KindShell, PermissionMode: "auto"}, noSleep)
		if !errors.Is(err, ErrBadArgs) {
			t.Fatalf("want ErrBadArgs, got %v", err)
		}
	})
	t.Run("valid mode flows into the inner command", func(t *testing.T) {
		f := &fakeTmux{}
		if _, err := Create(f, func(string) string { return "/home/shed" },
			CreateOptions{Kind: KindClaudeRC, Slug: "abc123", PermissionMode: "auto"}, noSleep); err != nil {
			t.Fatalf("Create: %v", err)
		}
		var newSession []string
		for _, c := range f.calls {
			if len(c) > 0 && c[0] == "new-session" {
				newSession = c
			}
		}
		inner := newSession[len(newSession)-1]
		if !strings.Contains(inner, "--remote-control") || !strings.Contains(inner, "--permission-mode auto") {
			t.Fatalf("inner command missing permission posture: %q", inner)
		}
	})
}

func TestKillIdempotent(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) Result {
		return Result{Code: 1, Stderr: "can't find session: rc-x"}
	}}
	if err := Kill(f, "x"); err != nil {
		t.Fatalf("kill of missing session should be nil, got %v", err)
	}
}

func TestIsMissingSession(t *testing.T) {
	missing := []string{
		"can't find session: rc-x",
		"can't find pane: rc-x", // capture-pane / send-keys phrasing
		"no server running on /tmp/tmux-501/default",
		"no session found",
	}
	for _, s := range missing {
		if !isMissingSession(s) {
			t.Errorf("isMissingSession(%q) = false, want true", s)
		}
	}
	if isMissingSession("connection refused") {
		t.Error("isMissingSession should not match unrelated errors")
	}
}

func TestProbeMissing(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) Result {
		return Result{Code: 1, Stderr: "no server running on /tmp/tmux"}
	}}
	_, err := Probe(f, "gone", nil)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("want ErrSessionNotFound, got %v", err)
	}
}

func TestAcceptTrustOnlyWhenPromptPresent(t *testing.T) {
	// Pane WITHOUT a trust dialog → no Enter sent.
	f := &fakeTmux{handler: func(args []string) Result {
		if args[0] == "capture-pane" {
			return Result{Code: 0, Stdout: "❯ ready prompt"}
		}
		return Result{}
	}}
	if err := AcceptTrust(f, "abc"); err != nil {
		t.Fatal(err)
	}
	if f.callWith("send-keys") != nil {
		t.Fatal("should not send Enter when no trust dialog is present")
	}

	// Pane WITH a trust dialog → Enter sent.
	g := &fakeTmux{handler: func(args []string) Result {
		if args[0] == "capture-pane" {
			return Result{Code: 0, Stdout: "Quick safety check: Yes, I trust this folder"}
		}
		return Result{}
	}}
	if err := AcceptTrust(g, "abc"); err != nil {
		t.Fatal(err)
	}
	if g.callWith("send-keys") == nil {
		t.Fatal("expected Enter to be sent for a present trust dialog")
	}
}

func TestPromptGuards(t *testing.T) {
	ready := "SHED_RC_V=2\nSHED_RC_KIND=claude-rc\nSHED_RC_ID=id-1"
	mk := func(env, pane string) *fakeTmux {
		return &fakeTmux{handler: func(args []string) Result {
			switch args[0] {
			case "capture-pane":
				return Result{Code: 0, Stdout: pane}
			case "show-environment":
				return Result{Code: 0, Stdout: env}
			default:
				return Result{Code: 0}
			}
		}}
	}

	// Ready claude-rc → delivered (send-keys -l -- text, then Enter).
	f := mk(ready, "Remote Control active https://claude.ai/code/session_01RC")
	if err := Prompt(f, PromptOptions{Slug: "abc", Text: "do it", SessionID: "id-1"}); err != nil {
		t.Fatalf("ready prompt should succeed: %v", err)
	}
	sk := f.callWith("send-keys")
	if sk == nil || !contains(sk, "-l") || !contains(sk, "--") || !contains(sk, "do it") {
		t.Fatalf("send-keys argv wrong: %v", sk)
	}

	// Session-id mismatch → not found.
	if err := Prompt(mk(ready, "Remote Control active https://claude.ai/code/session_01RC"),
		PromptOptions{Slug: "abc", Text: "x", SessionID: "other"}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("want ErrSessionNotFound on id mismatch, got %v", err)
	}

	// Broker → bad args.
	if err := Prompt(mk("SHED_RC_V=2\nSHED_RC_KIND=claude-broker", "Connected https://claude.ai/code?environment=env_1"),
		PromptOptions{Slug: "abc", Text: "x"}); !errors.Is(err, ErrBadArgs) {
		t.Fatalf("want ErrBadArgs on broker, got %v", err)
	}

	// Not ready → bad args.
	if err := Prompt(mk(ready, "starting up"), PromptOptions{Slug: "abc", Text: "x"}); !errors.Is(err, ErrBadArgs) {
		t.Fatalf("want ErrBadArgs on not-ready, got %v", err)
	}

	// Control chars → bad args (no tmux touched).
	if err := Prompt(&fakeTmux{}, PromptOptions{Slug: "abc", Text: "a\nb"}); !errors.Is(err, ErrBadArgs) {
		t.Fatalf("want ErrBadArgs on control chars, got %v", err)
	}
}

func TestPromptSendFailureSurfaces(t *testing.T) {
	// Ready claude-rc on capture/env, but the session is killed before send-keys.
	f := &fakeTmux{handler: func(args []string) Result {
		switch args[0] {
		case "capture-pane":
			return Result{Code: 0, Stdout: "Remote Control active https://claude.ai/code/session_01RC"}
		case "show-environment":
			return Result{Code: 0, Stdout: "SHED_RC_V=2\nSHED_RC_KIND=claude-rc\nSHED_RC_ID=id-1"}
		case "send-keys":
			return Result{Code: 1, Stderr: "can't find pane: rc-abc"}
		}
		return Result{Code: 0}
	}}
	if err := Prompt(f, PromptOptions{Slug: "abc", Text: "go"}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("a failed send-keys should surface ErrSessionNotFound, got %v", err)
	}
}

func TestCreateWaitDeadSession(t *testing.T) {
	// new-session succeeds, but the inner command exits immediately so capture-pane
	// reports the session gone — --wait must return dead, not spin to timeout.
	f := &fakeTmux{handler: func(args []string) Result {
		if args[0] == "capture-pane" {
			return Result{Code: 1, Stderr: "can't find pane: rc-d"}
		}
		return Result{Code: 0}
	}}
	s, err := Create(f, func(string) string { return "/home/shed" },
		CreateOptions{Kind: KindShell, Slug: "deadx", Wait: true}, noSleep)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != StateDead {
		t.Fatalf("want dead state for an immediately-gone session, got %s", s.State)
	}
}

func TestListParsesAll(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) Result {
		switch args[0] {
		case "ls":
			return Result{Code: 0, Stdout: "rc-aaa\nother\nrc-bbb\n"}
		case "show-environment":
			if contains(args, "rc-aaa") {
				return Result{Code: 0, Stdout: "SHED_RC_V=2\nSHED_RC_KIND=claude-rc\nSHED_RC_DISPLAY_NAME=A"}
			}
			return Result{Code: 0, Stdout: ""} // rc-bbb: legacy/unmanaged
		case "capture-pane":
			return Result{Code: 0, Stdout: "Remote Control active https://claude.ai/code/session_01"}
		}
		return Result{}
	}}
	resp := List(f, nil)
	if len(resp.RCSessions) != 2 {
		t.Fatalf("want 2 sessions (rc-aaa, rc-bbb), got %d", len(resp.RCSessions))
	}
	if resp.RCSessions[0].Slug != "aaa" || !resp.RCSessions[0].Managed ||
		resp.RCSessions[1].Slug != "bbb" || resp.RCSessions[1].Managed {
		t.Fatalf("unexpected list: %+v", resp.RCSessions)
	}
}

func noSleep(time.Duration) {}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
