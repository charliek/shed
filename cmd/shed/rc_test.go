package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/ext/rc"
)

// TestGoldenFixtureDecodesCanonical decodes the cross-repo golden DTO fixture
// (byte-identical to internal/ext/rc/testdata/rcSessionDto.golden.json and
// shed-remote-agent's) through the canonical internal/ext/rc types + the shared
// rc.IndexByTmux merge helper. Keeping a copy here makes the shed CLI a
// participant in the shed-ext-rc stdout contract guard even though listing
// enrichment now happens server-side.
func TestGoldenFixtureDecodesCanonical(t *testing.T) {
	data, err := os.ReadFile("testdata/rcSessionDto.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp rc.ListResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decoding golden fixture: %v", err)
	}
	byTmux := rc.IndexByTmux(resp.RCSessions)
	if len(byTmux) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(byTmux))
	}

	full, ok := byTmux["rc-abc234"]
	if !ok {
		t.Fatal("missing rc-abc234")
	}
	if full.Kind != rc.KindClaudeRC || full.State != rc.StateReady || !full.Managed ||
		full.DisplayName != "charliek/abc234" || !strings.Contains(full.URL, "session_") ||
		full.CreatedBy != "shed-remote-agent/0.1.0" {
		t.Fatalf("full session mismatch: %+v", full)
	}

	minimal, ok := byTmux["rc-brk900"]
	if !ok {
		t.Fatal("missing rc-brk900")
	}
	if minimal.Kind != rc.KindClaudeBroker || minimal.Managed ||
		minimal.DisplayName != "" || minimal.URL != "" {
		t.Fatalf("minimal session should omit optionals: %+v", minimal)
	}
}

func TestSSHCaptureArgs(t *testing.T) {
	entry := &config.ServerEntry{Host: "mini3", SSHPort: 2222}
	args := sshCaptureArgs("myshed", entry, "shed-ext-rc", "list")
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-p 2222",
		"StrictHostKeyChecking=yes",
		"BatchMode=yes",
		"myshed@mini3",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	// The remote argv must come after the "--" separator, in order.
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep == -1 || sep+2 >= len(args) || args[sep+1] != "shed-ext-rc" || args[sep+2] != "list" {
		t.Errorf("remote argv not placed after --: %v", args)
	}
}

func TestRCColumns(t *testing.T) {
	tests := []struct {
		name      string
		session   config.Session
		wantKind  string
		wantState string
	}{
		{
			name:    "non-rc session renders blank",
			session: config.Session{Name: "default"},
		},
		{
			name:      "rc session without metadata renders dashes",
			session:   config.Session{Name: "rc-abc234"},
			wantKind:  "-",
			wantState: "-",
		},
		{
			name: "managed rc session",
			session: config.Session{
				Name: "rc-abc234",
				RC:   &config.SessionRC{Kind: "claude-rc", State: "ready", Managed: true},
			},
			wantKind:  "claude-rc",
			wantState: "ready",
		},
		{
			name: "legacy rc session labelled",
			session: config.Session{
				Name: "rc-brk900",
				RC:   &config.SessionRC{Kind: "claude-broker", State: "starting", Managed: false},
			},
			wantKind:  "claude-broker (legacy)",
			wantState: "starting",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, state := rcColumns(tc.session)
			if kind != tc.wantKind || state != tc.wantState {
				t.Errorf("rcColumns = (%q,%q), want (%q,%q)", kind, state, tc.wantKind, tc.wantState)
			}
		})
	}
}

func TestResolveRCInputs(t *testing.T) {
	t.Run("multiple prompt sources error", func(t *testing.T) {
		_, _, _, err := resolveRCInputs(rcInputs{prompt: "x", edit: true})
		if err == nil {
			t.Fatal("want error for >1 prompt source")
		}
	})
	t.Run("multiple plan sources error", func(t *testing.T) {
		_, _, _, err := resolveRCInputs(rcInputs{plan: "p.md", planEdit: true})
		if err == nil {
			t.Fatal("want error for >1 plan source")
		}
	})
	t.Run("both stdin error", func(t *testing.T) {
		_, _, _, err := resolveRCInputs(rcInputs{promptFile: "-", plan: "-"})
		if err == nil {
			t.Fatal("want error when prompt and plan both read stdin")
		}
	})
	t.Run("multiline prompt allowed", func(t *testing.T) {
		p, _, _, err := resolveRCInputs(rcInputs{prompt: "line one\nline two"})
		if err != nil || p != "line one\nline two" {
			t.Fatalf("multi-line prompt should pass through, got (%q,%v)", p, err)
		}
	})
	t.Run("prompt flag passes through", func(t *testing.T) {
		p, _, havePlan, err := resolveRCInputs(rcInputs{prompt: "do the thing"})
		if err != nil || p != "do the thing" || havePlan {
			t.Fatalf("got (%q,%v,%v)", p, havePlan, err)
		}
	})
	t.Run("plan file read; markdown headers preserved", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/plan.md"
		body := "# Goal\n\nDo step 1\nDo step 2\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, plan, havePlan, err := resolveRCInputs(rcInputs{plan: path})
		if err != nil || !havePlan {
			t.Fatalf("got havePlan=%v err=%v", havePlan, err)
		}
		if !strings.Contains(plan, "# Goal") {
			t.Fatalf("markdown header stripped: %q", plan)
		}
	})
}

// TestBuildRCCreateArgv pins the client-side flag threading into the guest
// `shed-ext-rc create` argv — notably --workdir (plan 008 §3.7 item 3), which was
// previously dropped on the floor by createRCSession.
func TestBuildRCCreateArgv(t *testing.T) {
	entry := &config.ServerEntry{Host: "mini3", SSHPort: 2222}

	t.Run("workdir omitted when empty", func(t *testing.T) {
		argv, _ := buildRCCreateArgv(rcCreateOptions{shedName: "s", entry: entry, kind: "shell", slug: "abc123"})
		for _, a := range argv {
			if a == "--workdir" {
				t.Fatalf("--workdir should be omitted when opts.workdir is empty: %v", argv)
			}
		}
	})

	t.Run("workdir threaded through", func(t *testing.T) {
		argv, _ := buildRCCreateArgv(rcCreateOptions{
			shedName: "s", entry: entry, kind: "shell", slug: "abc123", workdir: "/home/shed/myproj",
		})
		if !argvHasPair(argv, "--workdir", "/home/shed/myproj") {
			t.Fatalf("--workdir /home/shed/myproj not found in argv: %v", argv)
		}
	})

	t.Run("permission mode threaded through", func(t *testing.T) {
		argv, _ := buildRCCreateArgv(rcCreateOptions{
			shedName: "s", entry: entry, kind: "codex", slug: "abc123", permissionMode: "auto",
		})
		if !argvHasPair(argv, "--permission-mode", "auto") {
			t.Fatalf("--permission-mode auto not found in argv: %v", argv)
		}
	})

	t.Run("plan delivery uses --plan-stdin, not --prompt-stdin, and carries stdin", func(t *testing.T) {
		argv, stdin := buildRCCreateArgv(rcCreateOptions{
			shedName: "s", entry: entry, kind: "claude-rc", slug: "abc123", plan: "do the thing",
		})
		if !containsStr(argv, "--plan-stdin") || containsStr(argv, "--prompt-stdin") {
			t.Fatalf("plan delivery argv wrong: %v", argv)
		}
		if stdin != "do the thing" {
			t.Fatalf("stdin = %q, want the plan content", stdin)
		}
	})

	t.Run("prompt delivery uses --prompt-stdin, and carries stdin", func(t *testing.T) {
		argv, stdin := buildRCCreateArgv(rcCreateOptions{
			shedName: "s", entry: entry, kind: "claude-rc", slug: "abc123", prompt: "hi",
		})
		if !containsStr(argv, "--prompt-stdin") || containsStr(argv, "--plan-stdin") {
			t.Fatalf("prompt delivery argv wrong: %v", argv)
		}
		if stdin != "hi" {
			t.Fatalf("stdin = %q, want the prompt", stdin)
		}
	})
}

func containsStr(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

// argvHasPair reports whether argv contains flag immediately followed by value.
func argvHasPair(argv []string, flag, value string) bool {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

func TestGenRCSlug(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		s, err := genRCSlug()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 6 {
			t.Fatalf("slug len = %d, want 6 (%q)", len(s), s)
		}
		for _, r := range s {
			if !strings.ContainsRune(rcSlugAlphabet, r) {
				t.Fatalf("slug %q has char outside alphabet", s)
			}
		}
		seen[s] = true
	}
	if len(seen) < 40 {
		t.Fatalf("slugs not random enough: %d unique of 50", len(seen))
	}
}

// TestIsOldBinaryRCErr pins the old-binary detection to the two exact signatures an
// old shed-ext-rc emits (unknown --kind value, undefined flag). A NEW binary's
// legitimate input-validation errors must pass through unmapped — they are user
// errors, not "recreate this shed".
func TestIsOldBinaryRCErr(t *testing.T) {
	old := []string{
		`shed-ext-rc create: exit status 2: shed-ext-rc: invalid arguments: unknown kind "codex"`,
		`shed-ext-rc create: exit status 2: flag provided but not defined: -permission-mode`,
	}
	for _, s := range old {
		if !isOldBinaryRCErr(errors.New(s)) {
			t.Errorf("old-binary signature not detected: %q", s)
		}
	}
	notOld := []string{
		`shed-ext-rc create: exit status 2: shed-ext-rc: invalid arguments: prompt contains an unsupported control character`,
		`shed-ext-rc create: exit status 2: shed-ext-rc: invalid arguments: invalid slug "UPPER"`,
		`shed-ext-rc create: exit status 2: shed-ext-rc: invalid arguments: --prompt-stdin given but stdin is empty`,
		`shed-ext-rc create: exit status 3: shed-ext-rc: rc session already exists: rc-abc123`,
		`shed-ext-rc create: exit status 255: ssh: connect to host x port 2222: Connection refused`,
	}
	for _, s := range notOld {
		if isOldBinaryRCErr(errors.New(s)) {
			t.Errorf("new-binary/transport error misdetected as old binary: %q", s)
		}
	}
	if isOldBinaryRCErr(nil) {
		t.Error("nil error must not be detected")
	}
}
