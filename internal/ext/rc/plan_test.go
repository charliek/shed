package rc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envFrom builds a Getenv over a fixed map (nil values read as "").
func envFrom(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

// TestPlanPathPerKind pins the per-kind, HOME-rooted plan location: claude kinds go
// to claude's native plans dir (honoring CLAUDE_CONFIG_DIR), every other kind goes to
// ~/.shed-plans — never the workdir.
func TestPlanPathPerKind(t *testing.T) {
	home := "/home/shed"
	cases := []struct {
		name string
		kind Kind
		env  map[string]string
		want string
	}{
		{"claude-rc default", KindClaudeRC, map[string]string{"HOME": home}, "/home/shed/.claude/plans/plan-abc123.md"},
		{"claude-broker default", KindClaudeBroker, map[string]string{"HOME": home}, "/home/shed/.claude/plans/plan-abc123.md"},
		{"claude honors CLAUDE_CONFIG_DIR", KindClaudeRC, map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": "/cfg"}, "/cfg/plans/plan-abc123.md"},
		{"codex home-rooted", KindCodex, map[string]string{"HOME": home}, "/home/shed/.shed-plans/plan-abc123.md"},
		{"opencode home-rooted", KindOpencode, map[string]string{"HOME": home}, "/home/shed/.shed-plans/plan-abc123.md"},
		{"cursor home-rooted", KindCursor, map[string]string{"HOME": home}, "/home/shed/.shed-plans/plan-abc123.md"},
		{"shell home-rooted", KindShell, map[string]string{"HOME": home}, "/home/shed/.shed-plans/plan-abc123.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanPath(tc.kind, "abc123", envFrom(tc.env))
			if err != nil {
				t.Fatalf("PlanPath error: %v", err)
			}
			if got != tc.want {
				t.Errorf("PlanPath = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlanPathRequiresHome(t *testing.T) {
	if _, err := PlanPath(KindCodex, "abc123", envFrom(nil)); !errors.Is(err, ErrBadArgs) {
		t.Errorf("want ErrBadArgs when HOME unset, got %v", err)
	}
	// claude with neither CLAUDE_CONFIG_DIR nor HOME also fails.
	if _, err := PlanPath(KindClaudeRC, "abc123", envFrom(nil)); !errors.Is(err, ErrBadArgs) {
		t.Errorf("want ErrBadArgs for claude with no HOME/CLAUDE_CONFIG_DIR, got %v", err)
	}
}

// TestCreatePlanWritesFile drives Create through the plan path and asserts the file
// lands at the per-kind location with 0600 mode, holds the exact bytes, and the
// composed kickoff (referencing the absolute path) is delivered to the ready pane.
func TestCreatePlanWritesFile(t *testing.T) {
	home := t.TempDir()
	// A non-empty pane so classifyShell reports ready and the kickoff is delivered.
	f := &fakeTmux{handler: func(args []string) Result {
		if args[0] == "capture-pane" {
			return Result{Stdout: "[shed:x] ~ $"}
		}
		return Result{}
	}}
	plan := "# Plan\n\nDo the thing.\n"
	s, err := Create(f, envFrom(map[string]string{"HOME": home}), CreateOptions{
		Kind: KindShell, Slug: "abc123", Plan: plan,
	}, noSleep)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.State != StateReady {
		t.Fatalf("state = %s, want ready", s.State)
	}

	path := filepath.Join(home, ".shed-plans", "plan-abc123.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("plan file not written: %v", err)
	}
	if string(data) != plan {
		t.Errorf("plan contents = %q, want %q", data, plan)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("plan file mode = %o, want 600", perm)
	}

	// The composed kickoff is a single line (no framing) → delivered via
	// `send-keys -l`; it references the absolute plan path and the instruction.
	sk := f.callWith("send-keys")
	if sk == nil {
		t.Fatal("no send-keys (kickoff not delivered)")
	}
	sent := sk[len(sk)-1]
	if !strings.Contains(sent, path) || !strings.Contains(sent, "implement it") {
		t.Errorf("kickoff missing path/instruction: %q", sent)
	}
}

// TestCreatePlanFramingPrepended verifies caller framing leads the composed kickoff.
func TestCreatePlanFramingPrepended(t *testing.T) {
	home := t.TempDir()
	f := &fakeTmux{handler: func(args []string) Result {
		if args[0] == "capture-pane" {
			return Result{Stdout: "ready pane text"}
		}
		return Result{}
	}}
	_, err := Create(f, envFrom(map[string]string{"HOME": home}), CreateOptions{
		Kind: KindShell, Slug: "abc123", Plan: "plan body", PlanFraming: "focus on X first",
	}, noSleep)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	buf := f.callWith("set-buffer")
	if buf == nil {
		t.Fatal("no kickoff delivered")
	}
	sent := buf[len(buf)-1]
	if !strings.HasPrefix(sent, "focus on X first\n\n") {
		t.Errorf("framing not prepended: %q", sent)
	}
}

func TestCreatePlanValidation(t *testing.T) {
	home := "/home/shed"
	env := envFrom(map[string]string{"HOME": home})

	t.Run("broker rejects a plan", func(t *testing.T) {
		f := &fakeTmux{}
		_, err := Create(f, env, CreateOptions{Kind: KindClaudeBroker, Plan: "x"}, noSleep)
		if !errors.Is(err, ErrBadArgs) {
			t.Fatalf("want ErrBadArgs, got %v", err)
		}
		if len(f.calls) != 0 {
			t.Error("tmux touched on a validation failure")
		}
	})

	t.Run("plan and prompt are mutually exclusive", func(t *testing.T) {
		_, err := Create(&fakeTmux{}, env, CreateOptions{Kind: KindClaudeRC, Plan: "x", Prompt: "y"}, noSleep)
		if !errors.Is(err, ErrBadArgs) {
			t.Fatalf("want ErrBadArgs, got %v", err)
		}
	})

	t.Run("oversized plan rejected", func(t *testing.T) {
		big := strings.Repeat("a", PlanMaxBytes+1)
		_, err := Create(&fakeTmux{}, env, CreateOptions{Kind: KindClaudeRC, Plan: big}, noSleep)
		if !errors.Is(err, ErrBadArgs) {
			t.Fatalf("want ErrBadArgs, got %v", err)
		}
	})

	t.Run("non-UTF8 plan rejected", func(t *testing.T) {
		_, err := Create(&fakeTmux{}, env, CreateOptions{Kind: KindClaudeRC, Plan: "a\xffb"}, noSleep)
		if !errors.Is(err, ErrBadArgs) {
			t.Fatalf("want ErrBadArgs, got %v", err)
		}
	})

	t.Run("framing with unsafe control char rejected", func(t *testing.T) {
		_, err := Create(&fakeTmux{}, env, CreateOptions{Kind: KindClaudeRC, Plan: "x", PlanFraming: "a\x1bb"}, noSleep)
		if !errors.Is(err, ErrBadArgs) {
			t.Fatalf("want ErrBadArgs, got %v", err)
		}
	})

	t.Run("framing without a plan rejected", func(t *testing.T) {
		_, err := Create(&fakeTmux{}, env, CreateOptions{Kind: KindClaudeRC, PlanFraming: "orphan"}, noSleep)
		if !errors.Is(err, ErrBadArgs) {
			t.Fatalf("want ErrBadArgs, got %v", err)
		}
	})
}

func TestComposePlanKickoff(t *testing.T) {
	got := composePlanKickoff("/p/plan.md", "")
	if !strings.Contains(got, "/p/plan.md") || strings.Contains(got, "\n\n") {
		t.Errorf("no-framing kickoff wrong: %q", got)
	}
	withFraming := composePlanKickoff("/p/plan.md", "lead line")
	if !strings.HasPrefix(withFraming, "lead line\n\n") || !strings.Contains(withFraming, "/p/plan.md") {
		t.Errorf("framing kickoff wrong: %q", withFraming)
	}
}

// TestWritePlanEnforcesModeOnExistingFile pins the chmod-on-rewrite behavior:
// os.WriteFile's mode only applies at create, so a pre-existing looser file must be
// tightened to 0600 by the explicit Chmod.
func TestWritePlanEnforcesModeOnExistingFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".shed-plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "plan-abc123.md")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := writePlan(KindShell, "abc123", "new plan", envFrom(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("writePlan: %v", err)
	}
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600 (pre-existing 644 not tightened)", perm)
	}
	if data, _ := os.ReadFile(path); string(data) != "new plan" {
		t.Errorf("content = %q, want %q", data, "new plan")
	}
}

// TestCreateDuplicateSlugLeavesPlanFileUntouched: the plan file is written only
// after tmux accepts the session name, so create --slug X --plan-stdin against a
// live rc-X errors with ErrDuplicateSlug and must NOT clobber that session's plan.
func TestCreateDuplicateSlugLeavesPlanFileUntouched(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".shed-plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "plan-abc123.md")
	if err := os.WriteFile(existing, []byte("live session's plan"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := &fakeTmux{handler: func(args []string) Result {
		if args[0] == "new-session" {
			return Result{Code: 1, Stderr: "duplicate session: rc-abc123"}
		}
		return Result{}
	}}
	_, err := Create(f, envFrom(map[string]string{"HOME": home}),
		CreateOptions{Kind: KindShell, Slug: "abc123", Plan: "usurper plan"}, noSleep)
	if !errors.Is(err, ErrDuplicateSlug) {
		t.Fatalf("want ErrDuplicateSlug, got %v", err)
	}
	if data, _ := os.ReadFile(existing); string(data) != "live session's plan" {
		t.Errorf("existing plan clobbered: %q", data)
	}
}

// TestCreatePlanWriteFailureKillsSession: a plan-write failure after the tmux
// session was created must fail the create AND tear the just-created session down,
// so a failed plan create leaves nothing behind.
func TestCreatePlanWriteFailureKillsSession(t *testing.T) {
	f := &fakeTmux{handler: func(args []string) Result { return Result{} }}
	// HOME with a control char makes PlanPath (inside writePlan) fail after the
	// session exists; Workdir is supplied so create-side workdir resolution is fine.
	_, err := Create(f, envFrom(map[string]string{"HOME": "/home/\x1bevil"}),
		CreateOptions{Kind: KindShell, Slug: "abc123", Workdir: "/tmp", Plan: "plan body"}, noSleep)
	if !errors.Is(err, ErrBadArgs) {
		t.Fatalf("want ErrBadArgs from the plan path, got %v", err)
	}
	if f.callWith("new-session") == nil {
		t.Fatal("expected the tmux session to have been created before the plan write")
	}
	if f.callWith("kill-session") == nil {
		t.Error("plan-write failure must kill the just-created session")
	}
}

// TestCreateKickoffDeliveryFailureIsAnError: once ready, a failed kickoff send must
// surface as a create error (not a state=ready success) — otherwise a plan/prompt
// run would exit 0 with nothing delivered.
func TestCreateKickoffDeliveryFailureIsAnError(t *testing.T) {
	home := t.TempDir()
	f := &fakeTmux{handler: func(args []string) Result {
		switch args[0] {
		case "capture-pane":
			return Result{Stdout: "ready pane text"} // shell classifies ready
		case "send-keys":
			return Result{Code: 1, Stderr: "send-keys: server exited unexpectedly"}
		}
		return Result{}
	}}
	s, err := Create(f, envFrom(map[string]string{"HOME": home}),
		CreateOptions{Kind: KindShell, Slug: "abc123", Plan: "plan body"}, noSleep)
	if err == nil {
		t.Fatalf("want a delivery error, got success: %+v", s)
	}
	if !strings.Contains(err.Error(), "kickoff delivery failed") {
		t.Errorf("error = %v, want a kickoff-delivery failure", err)
	}
}

// TestCreateKickoffDeliveryToKilledSessionIsDead: a session killed between ready
// classification and the send reads as dead (a session outcome), not a transport
// error.
func TestCreateKickoffDeliveryToKilledSessionIsDead(t *testing.T) {
	home := t.TempDir()
	f := &fakeTmux{handler: func(args []string) Result {
		switch args[0] {
		case "capture-pane":
			return Result{Stdout: "ready pane text"}
		case "send-keys":
			return Result{Code: 1, Stderr: "can't find session: rc-abc123"}
		}
		return Result{}
	}}
	s, err := Create(f, envFrom(map[string]string{"HOME": home}),
		CreateOptions{Kind: KindShell, Slug: "abc123", Plan: "plan body"}, noSleep)
	if err != nil {
		t.Fatalf("killed-session delivery should be a dead result, got error %v", err)
	}
	if s.State != StateDead {
		t.Errorf("state = %s, want dead", s.State)
	}
}

// TestPlanDirRejectsUntrustedClaudeConfigDir: a relative or control-char
// CLAUDE_CONFIG_DIR is ignored in favor of the $HOME/.claude default; a hostile
// HOME (control chars) fails PlanPath outright.
func TestPlanDirRejectsUntrustedClaudeConfigDir(t *testing.T) {
	home := "/home/shed"

	t.Run("relative CLAUDE_CONFIG_DIR falls back", func(t *testing.T) {
		got, err := PlanPath(KindClaudeRC, "abc123",
			envFrom(map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": "relative/dir"}))
		if err != nil {
			t.Fatal(err)
		}
		if got != "/home/shed/.claude/plans/plan-abc123.md" {
			t.Errorf("path = %q, want the $HOME/.claude fallback", got)
		}
	})

	t.Run("control-char CLAUDE_CONFIG_DIR falls back", func(t *testing.T) {
		got, err := PlanPath(KindClaudeRC, "abc123",
			envFrom(map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": "/cfg\x1b]0;pwn"}))
		if err != nil {
			t.Fatal(err)
		}
		if got != "/home/shed/.claude/plans/plan-abc123.md" {
			t.Errorf("path = %q, want the $HOME/.claude fallback", got)
		}
	})

	t.Run("control-char HOME errors", func(t *testing.T) {
		if _, err := PlanPath(KindCodex, "abc123",
			envFrom(map[string]string{"HOME": "/home/\nshed"})); !errors.Is(err, ErrBadArgs) {
			t.Errorf("want ErrBadArgs for a control-char HOME, got %v", err)
		}
	})

	t.Run("relative HOME errors", func(t *testing.T) {
		if _, err := PlanPath(KindCodex, "abc123",
			envFrom(map[string]string{"HOME": "home/shed"})); !errors.Is(err, ErrBadArgs) {
			t.Errorf("want ErrBadArgs for a relative HOME, got %v", err)
		}
	})
}
