package clirc

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/ext/rc"
)

// TestWarnHookUsesProgName pins that create's non-fatal diagnostics are prefixed
// with the BINARY'S OWN prog name — shed-machine-rc must not report as
// shed-ext-rc (the pre-seam behavior hardcoded the guest binary's name).
func TestWarnHookUsesProgName(t *testing.T) {
	for _, cfg := range []Config{extCfg, machineCfg} {
		var errb bytes.Buffer
		hook := warnHook(cfg, deps{stderr: &errb})
		if hook == nil {
			t.Fatalf("%s: warnHook = nil with stderr wired", cfg.ProgName)
		}
		hook("preseed skipped: %s", "boom")
		want := cfg.ProgName + ": preseed skipped: boom\n"
		if got := errb.String(); got != want {
			t.Fatalf("%s: warnHook wrote %q, want %q", cfg.ProgName, got, want)
		}
	}
	if hook := warnHook(extCfg, deps{}); hook != nil {
		t.Fatal("warnHook with no stderr should be nil (diagnostics discarded)")
	}
}

// TestEnsureHubHookKillSwitch pins the rc.EnvNoHub process-env kill-switch: a
// hermetic harness sets it so `create` performs no hub side effect at all, while
// the default (unset) path still invokes the wired ensureHub exactly once.
func TestEnsureHubHookKillSwitch(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		wired    bool
		wantNil  bool
		wantCall bool
	}{
		{name: "default invokes ensureHub", env: map[string]string{}, wired: true, wantCall: true},
		{name: "kill-switch yields nil hook", env: map[string]string{rc.EnvNoHub: "1"}, wired: true, wantNil: true},
		{name: "any non-empty value disables", env: map[string]string{rc.EnvNoHub: "true"}, wired: true, wantNil: true},
		{name: "no ensureHub wired yields nil", env: map[string]string{}, wired: false, wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := 0
			d := deps{getenv: func(k string) string { return tc.env[k] }}
			if tc.wired {
				d.ensureHub = func(cfg Config, _ deps) {
					called++
					if cfg.ProgName != machineCfg.ProgName {
						t.Fatalf("ensureHub received cfg %q, want %q", cfg.ProgName, machineCfg.ProgName)
					}
				}
			}
			hook := ensureHubHook(machineCfg, d)
			if tc.wantNil {
				if hook != nil {
					t.Fatal("ensureHubHook should be nil")
				}
				return
			}
			if hook == nil {
				t.Fatal("ensureHubHook = nil, want callable")
			}
			hook()
			if got := called; (got == 1) != tc.wantCall {
				t.Fatalf("ensureHub called %d times, wantCall=%v", got, tc.wantCall)
			}
		})
	}
}

// TestCreateHonorsKillSwitchEndToEnd drives a full dispatch-level create with an
// ensureHub wired (unlike the other dispatch tests) and proves the env var is the
// thing that gates it.
func TestCreateHonorsKillSwitchEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name      string
		noHub     string
		wantSpawn bool
	}{
		{name: "unset spawns", noHub: "", wantSpawn: true},
		{name: "set skips", noHub: "1", wantSpawn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spawned := false
			env := map[string]string{"HOME": t.TempDir()}
			if tc.noHub != "" {
				env[rc.EnvNoHub] = tc.noHub
			}
			var out, errb bytes.Buffer
			d := deps{
				runner:    &fakeRunner{},
				getenv:    func(k string) string { return env[k] },
				stdin:     strings.NewReader(""),
				stdout:    &out,
				stderr:    &errb,
				hostname:  func() string { return "testhost" },
				sleep:     func(time.Duration) {},
				probe:     func(string) rc.AgentInfo { return rc.AgentInfo{} },
				ensureHub: func(Config, deps) { spawned = true },
			}
			code := run(extCfg, d, []string{"create", "--kind", "shell", "--slug", "abc123"})
			if code != 0 {
				t.Fatalf("create exit = %d (stderr %q), want 0", code, errb.String())
			}
			if spawned != tc.wantSpawn {
				t.Fatalf("ensureHub spawned = %v, want %v", spawned, tc.wantSpawn)
			}
		})
	}
}
