package rc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureAgentKind maps a fixture filename's agent prefix to the kind the fixture is
// classified under. Non-claude agents are the bare-tool kinds; claude fixtures use
// claude-rc (the classifier is shared across claude kinds).
var fixtureAgentKind = map[string]Kind{
	"claude":   KindClaudeRC,
	"codex":    KindCodex,
	"opencode": KindOpencode,
	"cursor":   KindCursor,
}

// fixtureStates are the states a fixture filename can encode, longest-first so
// "needs-auth"/"needs-trust" match before the bare "auth"/"trust"/"starting" tokens
// would (there are none, but keep the order robust) and so a "-login" variant of a
// state still resolves to that state.
var fixtureStates = []State{
	StateNeedsTrust, StateNeedsAuth, StateReconnecting, StateStarting, StateReady, StateDead,
}

// parseFixtureName splits "<agent>-<state>[-variant]" into the agent prefix and the
// lifecycle state its content must classify to.
func parseFixtureName(name string) (agent string, want State, ok bool) {
	dash := strings.IndexByte(name, '-')
	if dash < 0 {
		return "", "", false
	}
	agent = name[:dash]
	rest := name[dash+1:]
	for _, s := range fixtureStates {
		if rest == string(s) || strings.HasPrefix(rest, string(s)+"-") {
			return agent, s, true
		}
	}
	return agent, "", false
}

// TestPaneFixturesClassify is the drift guard: every committed pane fixture must
// classify to the state named in its filename. A fixture that falls through to the
// starting default when its name says otherwise fails — that is the signal a TUI
// redraw/rebrand broke an anchor.
func TestPaneFixturesClassify(t *testing.T) {
	files, err := filepath.Glob("testdata/panes/*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no pane fixtures found")
	}
	seen := 0
	for _, f := range files {
		base := filepath.Base(f)
		if base == "SUMMARY.txt" {
			continue
		}
		name := strings.TrimSuffix(base, ".txt")
		agent, want, ok := parseFixtureName(name)
		if !ok {
			t.Errorf("%s: filename does not encode a known <agent>-<state>", base)
			continue
		}
		kind, ok := fixtureAgentKind[agent]
		if !ok {
			t.Errorf("%s: unknown agent prefix %q", base, agent)
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		state, url := ClassifyPane(kind, string(data))
		if state != want {
			t.Errorf("%s: ClassifyPane = %s, want %s", base, state, want)
		}
		// Drift guard: a non-starting fixture must never silently fall through.
		if want != StateStarting && state == StateStarting {
			t.Errorf("%s: classified to the starting default (broken anchor?)", base)
		}
		// url/id stay claude-remote-control-specific — never leak for other agents.
		if agent != "claude" && url != "" {
			t.Errorf("%s: non-claude fixture leaked url %q", base, url)
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no state fixtures were exercised")
	}
}

// TestClassifyFalsePositives pins the tricky discriminations the fixtures can't cover
// on their own: shell-prompt-vs-agent-death, empty-vs-exited, auth-before-delivery,
// and codex ready winning over its inline MCP token_expired noise.
func TestClassifyFalsePositives(t *testing.T) {
	const shedPrompt = "[shed:agent-fixtures] ~ $ "

	t.Run("shell prompt is ready for shell, dead for agent kinds", func(t *testing.T) {
		if s, _ := ClassifyPane(KindShell, shedPrompt); s != StateReady {
			t.Errorf("shell + prompt = %s, want ready", s)
		}
		for _, k := range []Kind{KindCodex, KindOpencode, KindCursor, KindClaudeRC} {
			if s, _ := ClassifyPane(k, "some agent output\n"+shedPrompt); s != StateDead {
				t.Errorf("%s + trailing shell prompt = %s, want dead", k, s)
			}
		}
	})

	t.Run("empty pane is starting, not dead", func(t *testing.T) {
		for _, k := range []Kind{KindCodex, KindOpencode, KindCursor, KindClaudeRC, KindShell} {
			if s, _ := ClassifyPane(k, "   \n\n"); s != StateStarting {
				t.Errorf("%s + empty pane = %s, want starting (real death is capture failure)", k, s)
			}
		}
	})

	t.Run("launch-echo shell prompt is not a death", func(t *testing.T) {
		// The command-echo of the launch line sits at the TOP with the agent still
		// running below — only a trailing prompt means the agent exited.
		pane := shedPrompt + "codex\nDo you trust the contents of this directory?\n1. Yes, continue\nPress enter to continue"
		if s, _ := ClassifyPane(KindCodex, pane); s != StateNeedsTrust {
			t.Errorf("codex launch-echo + trust prompt = %s, want needs-trust (not dead)", s)
		}
	})

	t.Run("prompt-shaped agent output is not a death", func(t *testing.T) {
		// A running agent PRINTING a line that quotes a prompt + command must not
		// classify as dead: only a bare prompt — nothing after the $ — as the last
		// non-empty line means "exited to shell".
		pane := ">_ OpenAI Codex (v0.142.4)\nI'll run the tests now:\n[shed:x] ~ $ make test"
		if s, _ := ClassifyPane(KindCodex, pane); s != StateReady {
			t.Errorf("codex quoting a prompt+command = %s, want ready (not dead)", s)
		}
		if exitedToShell("agent output\n[shed:x] ~ $ make test") {
			t.Error("prompt followed by a command must not read as an exit")
		}
		if !exitedToShell("agent output\n[shed:x] ~ $ ") {
			t.Error("a bare trailing prompt must read as an exit")
		}
	})

	t.Run("codex stale auth without composer is needs-auth", func(t *testing.T) {
		pane := "MCP startup failed\n\"code\": \"token_expired\"\nProvided authentication token is expired."
		if s, _ := ClassifyPane(KindCodex, pane); s != StateNeedsAuth {
			t.Errorf("codex token_expired (no composer) = %s, want needs-auth", s)
		}
	})

	t.Run("codex ready wins over inline token_expired MCP noise", func(t *testing.T) {
		pane := ">_ OpenAI Codex (v0.142.4)\n\"code\": \"token_expired\"\nMCP startup incomplete"
		if s, _ := ClassifyPane(KindCodex, pane); s != StateReady {
			t.Errorf("codex composer + MCP token_expired = %s, want ready", s)
		}
	})

	t.Run("opencode footer-only ready is guarded against auth screens", func(t *testing.T) {
		// Synthetic: no live logged-out opencode fixture exists yet. If an
		// auth/onboarding screen renders the persistent footer chrome, footer-only
		// ready must NOT fire (a wrong ready would deliver a prompt into a login
		// screen) — it stays starting until a recheck.
		authPane := "  Sign in to opencode to continue\n\n  ctrl+p commands"
		if s, _ := ClassifyPane(KindOpencode, authPane); s != StateStarting {
			t.Errorf("opencode footer + auth screen = %s, want starting (guard must trip)", s)
		}
		// Ordinary conversation chatter without auth phrasing keeps footer-only ready.
		chatPane := "  Hello! How can I help you today?\n\n  8.4K (4%)  ctrl+p commands"
		if s, _ := ClassifyPane(KindOpencode, chatPane); s != StateReady {
			t.Errorf("opencode footer + chat = %s, want ready", s)
		}
		// The composer placeholder is unconditional ready, even alongside auth-ish text.
		freshPane := "  Ask anything... \"Fix broken tests\"\n  sign in tips\n  ctrl+p commands"
		if s, _ := ClassifyPane(KindOpencode, freshPane); s != StateReady {
			t.Errorf("opencode placeholder = %s, want ready (unconditional)", s)
		}
	})

	t.Run("cursor auth-before-delivery is needs-auth", func(t *testing.T) {
		if s, _ := ClassifyPane(KindCursor, "Cursor Agent\nPress any key to log in..."); s != StateNeedsAuth {
			t.Errorf("cursor login splash = %s, want needs-auth", s)
		}
	})

	t.Run("unknown kind renders neutrally (no url, no dead misfire)", func(t *testing.T) {
		// Unknown-kind policy: a preserved raw kind falls back to a neutral shell-style
		// classification — a state, never a claude URL affordance.
		if s, url := ClassifyPane(Kind("opencode-hub"), "banner https://claude.ai/code/session_01RC"); url != "" || s == StateDead {
			t.Errorf("unknown kind = (%s,%q), want a neutral state and no url", s, url)
		}
	})
}
