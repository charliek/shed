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
