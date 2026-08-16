package rc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Tests for the cursor hook preseed (plan 008 §3.5). They mirror trust_test.go's shape,
// because the two preseeds share the same contract: merge — never clobber; a malformed
// file is left untouched; the write is atomic under a sibling flock. What is NEW here is
// the hub-owned script (overwrite-always, executable, mute) and the mount guard.

// cursorHooksPath / cursorScriptPath name the two files the preseed produces.
func cursorHooksPath(home string) string { return filepath.Join(home, ".cursor", "hooks.json") }
func cursorScriptPath(home string) string {
	return filepath.Join(home, hubDirName, cursorHookScriptName)
}

// hookEntries returns the command strings wired to one event in a hooks.json document.
func hookEntries(t *testing.T, m map[string]any, event string) []string {
	t.Helper()
	hooks, _ := m["hooks"].(map[string]any)
	raw, _ := hooks[event].([]any)
	var out []string
	for _, e := range raw {
		entry, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("hooks[%q] entry is not an object: %#v", event, e)
		}
		cmd, _ := entry["command"].(string)
		out = append(out, cmd)
	}
	return out
}

func TestPreseedCursorHooksFreshWrite(t *testing.T) {
	home := t.TempDir()
	if err := PreseedCursorHooks("/home/shed/proj", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}
	m := readConfig(t, cursorHooksPath(home))
	if v, _ := m["version"].(float64); int(v) != cursorHooksConfigVersion {
		t.Errorf("version = %v, want %d", m["version"], cursorHooksConfigVersion)
	}
	// EVERY wired event gets exactly our entry, and the argv[1] event name rides along —
	// one script serves all nine, which is why the whole matrix costs one file.
	for _, event := range cursorHookEvents {
		cmds := hookEntries(t, m, event)
		if len(cmds) != 1 {
			t.Fatalf("hooks[%q] = %v, want exactly one entry", event, cmds)
		}
		if !strings.Contains(cmds[0], cursorScriptPath(home)) || !strings.HasSuffix(cmds[0], " "+event) {
			t.Errorf("hooks[%q][0] = %q, want the hub script invoked with %q", event, cmds[0], event)
		}
	}
	// The events deliberately NOT wired (see cursorHookEvents' doc) must be absent.
	for _, event := range []string{"afterAgentThought", "workspaceOpen", "beforeReadFile"} {
		if cmds := hookEntries(t, m, event); len(cmds) != 0 {
			t.Errorf("hooks[%q] = %v, want the event left unwired", event, cmds)
		}
	}
}

// The script is hub-owned: executable, mute, slug-gated, and pointed at the hub's port.
func TestPreseedCursorHooksScriptContentAndMode(t *testing.T) {
	home := t.TempDir()
	if err := PreseedCursorHooks("/x", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}
	path := cursorScriptPath(home)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("script mode = %v, want 0755 (cursor execs it directly)", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, want := range []string{
		"SHED_RC_SLUG", // the slug gate: no slug, no POST, no stdin read
		"http://" + HubAddr + "/v1/ingest/cursor", // one source of truth for the hub port
		"--max-time 2", "--connect-timeout 1", // bounded: hooks block the agent's turn
		// The hub is on LOOPBACK: an http_proxy in the environment would otherwise carry
		// every prompt and every command's output off-box. --globoff keeps curl from
		// reading URL punctuation as its glob syntax.
		"--noproxy '*'", "--globoff",
		"exit 0", // fail-open, always
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}
	// Mute by construction: cursor reads hook stdout as a VERDICT, so the script must
	// print nothing — curl's output goes to /dev/null and there is no echo anywhere.
	if !strings.Contains(script, "--output /dev/null") || strings.Contains(script, "echo ") {
		t.Errorf("the hook script must never write to stdout:\n%s", script)
	}
	// Overwrite-always: a stale script from an older binary must not survive.
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho stale\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PreseedCursorHooks("/x", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(path); string(again) != script {
		t.Error("the hub-owned script must be rewritten on every preseed")
	}
}

// Merge — never clobber: a user's own hooks (on a wired event, on an unwired one) and any
// unknown top-level keys survive, and a user-declared version is not overwritten.
func TestPreseedCursorHooksMergePreservesUserHooks(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"version": 2,
		"hooks": map[string]any{
			"beforeSubmitPrompt": []any{map[string]any{"command": "/usr/local/bin/audit.sh", "failClosed": true}},
			"afterAgentThought":  []any{map[string]any{"command": "/usr/local/bin/thoughts.sh"}},
		},
		"somethingElse": map[string]any{"keep": "me"},
	}
	data, _ := json.Marshal(seed)
	if err := os.WriteFile(cursorHooksPath(home), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := PreseedCursorHooks("/x", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}

	m := readConfig(t, cursorHooksPath(home))
	if v, _ := m["version"].(float64); int(v) != 2 {
		t.Errorf("version = %v, want the user's 2 (never clobbered)", m["version"])
	}
	if _, ok := m["somethingElse"]; !ok {
		t.Error("unknown top-level key dropped")
	}
	cmds := hookEntries(t, m, "beforeSubmitPrompt")
	if len(cmds) != 2 || cmds[0] != "/usr/local/bin/audit.sh" {
		t.Fatalf("beforeSubmitPrompt = %v, want the user's entry first then ours", cmds)
	}
	if !strings.Contains(cmds[1], cursorScriptPath(home)) {
		t.Errorf("our entry was not appended: %v", cmds)
	}
	// The user's entry keeps its own fields verbatim (round-tripped as a map, never a
	// typed struct that would drop failClosed).
	hooks, _ := m["hooks"].(map[string]any)
	raw, _ := hooks["beforeSubmitPrompt"].([]any)
	first, _ := raw[0].(map[string]any)
	if first["failClosed"] != true {
		t.Errorf("the user entry's failClosed field was dropped: %#v", first)
	}
	// An event we do not wire keeps the user's entry and gains nothing.
	if cmds := hookEntries(t, m, "afterAgentThought"); len(cmds) != 1 {
		t.Errorf("afterAgentThought = %v, want only the user's entry", cmds)
	}
}

// Idempotent: running the preseed repeatedly (every create does) must never grow the
// arrays, and a hand-edited invocation of OUR script still counts as ours.
func TestPreseedCursorHooksIdempotent(t *testing.T) {
	home := t.TempDir()
	env := envFunc(map[string]string{"HOME": home})
	for i := 0; i < 3; i++ {
		if err := PreseedCursorHooks("/x", env); err != nil {
			t.Fatal(err)
		}
	}
	m := readConfig(t, cursorHooksPath(home))
	for _, event := range cursorHookEvents {
		if cmds := hookEntries(t, m, event); len(cmds) != 1 {
			t.Fatalf("hooks[%q] = %v after 3 preseeds, want one entry", event, cmds)
		}
	}

	// A hand-edited command that still invokes our script is left alone (matched on the
	// script path, not the exact string) — otherwise every create would append a duplicate.
	hooks, _ := m["hooks"].(map[string]any)
	hooks["stop"] = []any{map[string]any{"command": "SHED_DEBUG=1 " + cursorScriptPath(home) + " stop"}}
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(cursorHooksPath(home), out, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PreseedCursorHooks("/x", env); err != nil {
		t.Fatal(err)
	}
	if cmds := hookEntries(t, readConfig(t, cursorHooksPath(home)), "stop"); len(cmds) != 1 {
		t.Errorf("stop = %v, want the hand-edited invocation kept as ours", cmds)
	}
}

// A HOME containing a single quote produces a scriptPath that shellQuote must escape (an
// embedded quote becomes the POSIX `'\”` trick), which used to defeat cursorHookEntryPresent's
// raw-Contains check on the SECOND preseed and append a duplicate entry every run. Regression
// test: a second preseed against such a HOME must still be idempotent.
func TestPreseedCursorHooksIdempotentWithQuoteInPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), "o'brien")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := envFunc(map[string]string{"HOME": home})
	for i := 0; i < 2; i++ {
		if err := PreseedCursorHooks("/x", env); err != nil {
			t.Fatal(err)
		}
	}
	m := readConfig(t, cursorHooksPath(home))
	for _, event := range cursorHookEvents {
		if cmds := hookEntries(t, m, event); len(cmds) != 1 {
			t.Fatalf("hooks[%q] = %v after 2 preseeds with a quote in HOME, want one entry (idempotent)", event, cmds)
		}
	}
}

func TestPreseedCursorHooksLeavesMalformedUntouched(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o700); err != nil {
		t.Fatal(err)
	}
	garbage := []byte("{ not json at all")
	if err := os.WriteFile(cursorHooksPath(home), garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PreseedCursorHooks("/x", envFunc(map[string]string{"HOME": home})); err == nil {
		t.Fatal("expected an error for a malformed hooks.json")
	}
	if data, _ := os.ReadFile(cursorHooksPath(home)); string(data) != string(garbage) {
		t.Fatalf("malformed hooks.json was modified: %q", data)
	}
}

// A VALID-BUT-UNEXPECTED shape is treated exactly like a malformed file: the preseed
// declines and the user's config is left byte-identical. The failed type assertion that
// used to sit here discarded the value and then overwrote it — deleting real user config
// that merely disagreed with this code's idea of the schema.
func TestPreseedCursorHooksRefusesUnexpectedShapes(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"hooks is an array", `{"version":1,"hooks":[{"command":"/usr/local/bin/audit.sh"}]}`},
		{"hooks is a string", `{"version":1,"hooks":"see ~/.cursor/hooks.d"}`},
		{"an event maps to an object", `{"version":1,"hooks":{"stop":{"command":"/usr/local/bin/audit.sh"}}}`},
		{"an event maps to a string", `{"version":1,"hooks":{"beforeSubmitPrompt":"/usr/local/bin/audit.sh"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cursorHooksPath(home), []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := PreseedCursorHooks("/x", envFunc(map[string]string{"HOME": home})); err == nil {
				t.Fatal("expected the preseed to decline rather than rewrite the file")
			}
			data, _ := os.ReadFile(cursorHooksPath(home))
			if string(data) != c.body {
				t.Fatalf("user config was rewritten:\n got %s\nwant %s", data, c.body)
			}
		})
	}
}

// Concurrent creates serialize through the sibling flock: every writer's merge survives,
// and the file is always valid JSON (atomic rename, never a partial write).
func TestPreseedCursorHooksConcurrent(t *testing.T) {
	home := t.TempDir()
	env := envFunc(map[string]string{"HOME": home})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := PreseedCursorHooks("/x", env); err != nil {
				t.Errorf("preseed: %v", err)
			}
		}()
	}
	wg.Wait()
	m := readConfig(t, cursorHooksPath(home))
	for _, event := range cursorHookEvents {
		if cmds := hookEntries(t, m, event); len(cmds) != 1 {
			t.Errorf("hooks[%q] = %v after concurrent preseeds, want exactly one entry", event, cmds)
		}
	}
}

// THE MOUNT GUARD: a ~/.cursor on another device is an auth mount from the host, and
// writing a hook config there would push a script path that does not exist on the host
// into the user's real cursor setup. The config half is skipped and reported; the hub-owned
// script is still written (inert until something references it).
func TestPreseedCursorHooksSkipsForeignDevice(t *testing.T) {
	home := t.TempDir()
	prev := sameDevice
	t.Cleanup(func() { sameDevice = prev })
	sameDevice = func(_, _ string) (bool, error) { return false, nil }

	err := PreseedCursorHooks("/x", envFunc(map[string]string{"HOME": home}))
	if !errors.Is(err, ErrCursorHooksForeignDevice) {
		t.Fatalf("err = %v, want ErrCursorHooksForeignDevice", err)
	}
	if _, statErr := os.Stat(cursorHooksPath(home)); !os.IsNotExist(statErr) {
		t.Error("hooks.json must NOT be written into a foreign-device ~/.cursor")
	}
	if _, statErr := os.Stat(cursorScriptPath(home)); statErr != nil {
		t.Errorf("the hub-owned script should still be written: %v", statErr)
	}
}

// The real device check: $HOME and a directory inside it are always on one filesystem, and
// a path that does not exist is an error rather than a silent "same".
func TestStatSameDevice(t *testing.T) {
	home := t.TempDir()
	sub := filepath.Join(home, ".cursor")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	same, err := statSameDevice(sub, home)
	if err != nil || !same {
		t.Errorf("statSameDevice(%q, %q) = (%v, %v), want (true, nil)", sub, home, same, err)
	}
	if _, err := statSameDevice(filepath.Join(home, "nope"), home); err == nil {
		t.Error("a missing path must report an error, not a verdict")
	}
}

func TestPreseedCursorHooksNoHomeIsNoOp(t *testing.T) {
	if err := PreseedCursorHooks("/x", envFunc(map[string]string{})); err == nil {
		t.Fatal("expected an error (skipped) when HOME is unset")
	}
}

// The registry wiring: the cursor spec must actually dispatch to this preseed, since
// Create invokes it through spec.Preseed and nothing else would.
func TestCursorSpecWiresTheHookPreseed(t *testing.T) {
	spec, ok := specForKind(KindCursor)
	if !ok || spec.Preseed == nil {
		t.Fatal("the cursor spec must declare a Preseed")
	}
	home := t.TempDir()
	if err := spec.Preseed("/home/shed/proj", envFunc(map[string]string{"HOME": home})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cursorHooksPath(home)); err != nil {
		t.Errorf("the spec's Preseed did not write hooks.json: %v", err)
	}
}
