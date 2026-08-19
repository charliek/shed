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

// TestHubConfigEnvOverrides pins the plan-010 oracle seams: SHED_RC_HUB_ADDR
// (loopback-enforced) and the six *_MS interval overrides, wired only through
// hubConfig — inert unless set, ignored (with a prog-name-prefixed stderr note)
// when malformed, and never able to widen the bind off 127.0.0.1.
func TestHubConfigEnvOverrides(t *testing.T) {
	mkGetenv := func(env map[string]string) rc.Getenv {
		return func(k string) string { return env[k] }
	}

	t.Run("unset env leaves config untouched", func(t *testing.T) {
		var errb bytes.Buffer
		hc := hubConfig(machineCfg, deps{getenv: mkGetenv(nil), stderr: &errb})
		if hc.Addr != "" || hc.ActiveInterval != 0 || hc.IdleInterval != 0 ||
			hc.QuietPeriod != 0 || hc.IdleTimeout != 0 || hc.Heartbeat != 0 || hc.WriteTimeout != 0 {
			t.Fatalf("unset env mutated config: %+v", hc)
		}
		if errb.Len() != 0 {
			t.Fatalf("unset env wrote stderr: %q", errb.String())
		}
	})

	t.Run("nil getenv is a no-op", func(t *testing.T) {
		hc := hubConfig(machineCfg, deps{})
		if hc.Addr != "" {
			t.Fatalf("nil getenv set Addr %q", hc.Addr)
		}
	})

	t.Run("loopback addr accepted", func(t *testing.T) {
		for _, addr := range []string{"127.0.0.1:1", "127.0.0.1:45555", "127.0.0.1:65535"} {
			var errb bytes.Buffer
			hc := hubConfig(machineCfg, deps{getenv: mkGetenv(map[string]string{"SHED_RC_HUB_ADDR": addr}), stderr: &errb})
			if hc.Addr != addr {
				t.Fatalf("addr %q: got %q", addr, hc.Addr)
			}
			if errb.Len() != 0 {
				t.Fatalf("addr %q: unexpected stderr %q", addr, errb.String())
			}
		}
	})

	t.Run("non-loopback addr rejected with note", func(t *testing.T) {
		for _, cfg := range []Config{extCfg, machineCfg} {
			// 127.0.0.1:0 rejected on purpose: the child would bind a random
			// port the parent's probe can't find, and port 0 can never EADDRINUSE
			// so bind-as-lock would silently vanish (opus review finding).
			for _, addr := range []string{"0.0.0.0:1029", "[::1]:1029", "localhost:1029", "10.0.0.5:1029", "127.0.0.1", "garbage", "127.0.0.1:0", "127.0.0.1:", "127.0.0.1:99999", "127.0.0.1:80 "} {
				var errb bytes.Buffer
				hc := hubConfig(cfg, deps{getenv: mkGetenv(map[string]string{"SHED_RC_HUB_ADDR": addr}), stderr: &errb})
				if hc.Addr != "" {
					t.Fatalf("%s: addr %q leaked into config as %q", cfg.ProgName, addr, hc.Addr)
				}
				note := errb.String()
				if !strings.HasPrefix(note, cfg.ProgName+": ignoring SHED_RC_HUB_ADDR=") {
					t.Fatalf("%s: addr %q note = %q", cfg.ProgName, addr, note)
				}
			}
		}
	})

	t.Run("interval overrides map to their fields", func(t *testing.T) {
		env := map[string]string{
			"SHED_RC_HUB_ACTIVE_MS":        "100",
			"SHED_RC_HUB_IDLE_MS":          "250",
			"SHED_RC_HUB_QUIET_MS":         "500",
			"SHED_RC_HUB_IDLE_EXIT_MS":     "86400000",
			"SHED_RC_HUB_HEARTBEAT_MS":     "1000",
			"SHED_RC_HUB_WRITE_TIMEOUT_MS": "2000",
		}
		var errb bytes.Buffer
		hc := hubConfig(machineCfg, deps{getenv: mkGetenv(env), stderr: &errb})
		want := []struct {
			name string
			got  time.Duration
			ms   int
		}{
			{"ActiveInterval", hc.ActiveInterval, 100},
			{"IdleInterval", hc.IdleInterval, 250},
			{"QuietPeriod", hc.QuietPeriod, 500},
			{"IdleTimeout", hc.IdleTimeout, 86400000},
			{"Heartbeat", hc.Heartbeat, 1000},
			{"WriteTimeout", hc.WriteTimeout, 2000},
		}
		for _, w := range want {
			if w.got != time.Duration(w.ms)*time.Millisecond {
				t.Fatalf("%s = %v, want %dms", w.name, w.got, w.ms)
			}
		}
		if errb.Len() != 0 {
			t.Fatalf("valid intervals wrote stderr: %q", errb.String())
		}
	})

	t.Run("invalid intervals ignored with note", func(t *testing.T) {
		// 9999999999999999999 (>MaxInt64/1e6 ms) would overflow the Duration
		// multiply into a NEGATIVE value that resolve() silently maps back to
		// the DEFAULT — the opposite of the override's intent (opus review
		// finding); the ceiling rejects it with a note instead.
		for _, bad := range []string{"0", "-5", "abc", "100ms", "1.5", "9999999999999999999", "9223372036854775807"} {
			var errb bytes.Buffer
			hc := hubConfig(machineCfg, deps{getenv: mkGetenv(map[string]string{"SHED_RC_HUB_ACTIVE_MS": bad}), stderr: &errb})
			if hc.ActiveInterval != 0 {
				t.Fatalf("bad %q set ActiveInterval %v", bad, hc.ActiveInterval)
			}
			if !strings.Contains(errb.String(), "ignoring SHED_RC_HUB_ACTIVE_MS=") {
				t.Fatalf("bad %q: note = %q", bad, errb.String())
			}
		}
	})

	t.Run("runner and getenv still wired", func(t *testing.T) {
		getenv := mkGetenv(map[string]string{"HOME": "/tmp/x"})
		hc := hubConfig(machineCfg, deps{getenv: getenv})
		if hc.Getenv == nil || hc.Getenv("HOME") != "/tmp/x" {
			t.Fatal("Getenv not wired through")
		}
	})
}
