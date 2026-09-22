package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/roostctl"
	"github.com/charliek/shed/internal/roostprovider"
	"github.com/charliek/shed/internal/sshconfig"
)

// Tests for the roost-native `shed attach` path (plan 022 §3.3 / C5b).
//
// Three fixtures, each owning one layer: the `roostctl` shim
// (roostctl_shim_test.go) is the LOCAL app, the fake shed
// (roost_fakeshed_test.go) is the far side's roost-session over a fake ssh,
// and fakeClock is the wall clock — so a 20 s cap and a 2 s settle window are
// asserted to the millisecond and cost nothing to run.

// fakeClock is the clock seam roostAttach polls against: `sleep` advances
// `now` and nothing else does, so every deadline in the flow is decided by the
// number of polls the test staged answers for.
type fakeClock struct {
	t     time.Time
	slept time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(d time.Duration) {
	c.t = c.t.Add(d)
	c.slept += d
}

// roostRig is a roostAttach wired to both fixtures and the fake clock.
type roostRig struct {
	shim   *roostShim
	shed   *fakeShed
	clock  *fakeClock
	attach *roostAttach
	out    *bytes.Buffer
	errOut *bytes.Buffer
	// sshConfig is the temp `~/.ssh/config` the alias step writes to.
	sshConfig string
}

// newRoostRig builds the rig. tabs are what the far side's `tab.list` reports;
// withLanding decides whether the shed's landing dir exists over there.
func newRoostRig(t *testing.T, tabs []roostprovider.Tab, withLanding bool) *roostRig {
	t.Helper()
	rig := &roostRig{
		shim:   newRoostShim(t),
		shed:   newFakeShed(t, tabs, withLanding),
		clock:  newFakeClock(),
		out:    &bytes.Buffer{},
		errOut: &bytes.Buffer{},
	}
	rig.sshConfig = filepath.Join(t.TempDir(), ".ssh", "config")
	rig.attach = &roostAttach{
		ctl:           &roostctl.Client{},
		sshBin:        rig.shed.sshBin,
		sshConfigPath: rig.sshConfig,
		out:           rig.out,
		errOut:        rig.errOut,
		connectPoll:   defaultRoostConnectPoll,
		connectCap:    defaultRoostConnectCap,
		connectSettle: defaultRoostConnectSettle,
		sidebarPoll:   defaultRoostSidebarPoll,
		sidebarCap:    defaultRoostSidebarCap,
		remoteCap:     defaultRoostRemoteCap,
		now:           rig.clock.now,
		sleep:         rig.clock.sleep,
	}
	return rig
}

// shedEntry is the server entry the fake shed is reached through.
func (r *roostRig) shedEntry() *config.ServerEntry {
	return &config.ServerEntry{Host: "mini3", SSHPort: 2222}
}

// shed returns the config.Shed for the fake far side, with the landing dir the
// rig created (or named but did not create).
func (r *roostRig) shedConfig() *config.Shed {
	return &config.Shed{Name: "myproj", Status: config.StatusRunning, LandingDir: r.shed.landingDir}
}

// -----------------------------------------------------------------------
// Shim reply documents, spelled the way roost spells them.
//
// Written as JSON literals rather than marshalled from internal/roostctl's own
// structs on purpose: a fixture built from the decoder's types cannot catch a
// field this side named wrong, because both halves would be wrong together.
// The shapes are the vendored vectors' (crates/fixtures/roost-vectors/
// host.status.response.json, host.list.response.json, app.sidebar_dump.
// response.json).
// -----------------------------------------------------------------------

const (
	testHostID    = "a1b2c3d4e5f60718"
	testHostLabel = "shed-myproj"
)

// hostRowJSON is one `host status` row. reason and detail are omitted when
// empty, the way roost's `skip_serializing_if` omits them.
func hostRowJSON(id, label, target string, generation uint64, state, reason, detail string) string {
	row := fmt.Sprintf(`{"id":%q,"label":%q,"target":%q,"generation":%d,"state":%q`,
		id, label, target, generation, state)
	if reason != "" {
		row += fmt.Sprintf(`,"reason":%q`, reason)
	}
	if detail != "" {
		row += fmt.Sprintf(`,"detail":%q`, detail)
	}
	return row + `,"tabs":0}`
}

// hostStatusDoc is a `host status --json` answer.
func hostStatusDoc(rows ...string) string {
	return `{"hosts":[` + strings.Join(rows, ",") + "]}\n"
}

// statusRow is the ordinary row for the rig's host.
func statusRow(generation uint64, state, reason string) string {
	return hostRowJSON(testHostID, testHostLabel, testHostLabel, generation, state, reason, "")
}

// hostAddDoc is a `host add --json` answer.
func hostAddDoc(id, label, target string) string {
	return fmt.Sprintf(`{"host":{"id":%q,"label":%q,"target":%q,"last_connected":null}}`+"\n", id, label, target)
}

// hostConnectDoc is a `host connect --json` answer: the host plus the state
// the ATTEMPT is in, which is routinely `connecting`.
func hostConnectDoc(id, label, target string) string {
	return fmt.Sprintf(`{"host":{"id":%q,"label":%q,"target":%q,"last_connected":null},"state":"connecting"}`+"\n",
		id, label, target)
}

// sidebarDumpDoc is an `app.sidebar_dump` answer carrying two host sections:
// the rig's, with the given tab keys, and a DECOY whose tab key has the same
// numeric suffix under a different host. The decoy is the point — it is what
// fails a lookup that joined on the tab id alone and ignored the host.
func sidebarDumpDoc(hostID string, tabKeys ...string) string {
	rows := make([]string, 0, len(tabKeys))
	for _, key := range tabKeys {
		rows = append(rows, fmt.Sprintf(`{"key":%q,"title":"default"}`, key))
	}
	return fmt.Sprintf(`{"agents_visible":true,"projects":[],"hosts":[`+
		`{"id":"decoy-host","label":"laptop","state":"connected","projects":`+
		`[{"key":"h9.1","name":"elsewhere","tabs":[{"key":"h9.5","title":"default"}]}]},`+
		`{"id":%q,"label":%q,"state":"connected","projects":`+
		`[{"key":"h3.2","name":"shed","tabs":[%s]}]}]}`+"\n",
		hostID, testHostLabel, strings.Join(rows, ","))
}

// -----------------------------------------------------------------------
// Step 2 — the gate, and the tmux floor behind it.
// -----------------------------------------------------------------------

// tmuxGoldenArgv reads one scenario's pinned argv out of the tmux floor
// golden, so the fallback tests assert against the SAME bytes
// TestAttachPlainTmuxArgvGolden does rather than against a copy of them.
func tmuxGoldenArgv(t *testing.T, scenario string) []string {
	t.Helper()
	for _, sc := range readAttachTmuxGolden(t).Scenarios {
		if sc.Name == scenario {
			return sc.WantArgv
		}
	}
	t.Fatalf("golden has no scenario %q", scenario)
	return nil
}

// newAvailableShim installs a shim that answers the GATE: an `identify`
// carrying a socket path, and a `host status` proving the host family is
// served. That pair is all Available() asks for, so a shim staged this way is
// "a roost app is running on this machine".
func newAvailableShim(t *testing.T) *roostShim {
	t.Helper()
	shim := newRoostShim(t)
	shim.reply("identify", roostVectorResult(t, "identify.response.json"))
	shim.reply("host.status", hostStatusDoc())
	return shim
}

// TestAttachGateFallsBackToTmux is §3.3 step 2's whole contract: every way the
// roost path can be declined produces the tmux floor's argv, byte for byte.
//
// The three cases are the three negative controls the plan names — no roostctl
// on PATH, `--tmux` with one present, `SHED_ATTACH=tmux` with one present —
// and the last two also assert that the shim was NEVER RUN. That is the part a
// golden-argv comparison alone would miss: an implementation that asked the
// local app first and then threw the answer away would still produce the right
// argv while pausing on every attach of a person who asked for tmux.
func TestAttachGateFallsBackToTmux(t *testing.T) {
	want := tmuxGoldenArgv(t, "default_session")

	cases := []struct {
		name string
		// setup installs the shim (or not) and sets the flag/env under test.
		setup func(t *testing.T) *roostShim
	}{
		{
			name: "no roostctl on PATH",
			setup: func(t *testing.T) *roostShim {
				// A PATH with nothing on it but an `ssh` — execSSHTmux still
				// resolves ssh through LookPath, and no roostctl can be found
				// whatever this machine happens to have installed.
				dir := t.TempDir()
				writeTestFile(t, filepath.Join(dir, "ssh"), "#!/bin/sh\nexit 0\n", 0o755)
				t.Setenv("PATH", dir)
				return nil
			},
		},
		{
			name: "--tmux with a roost app running",
			setup: func(t *testing.T) *roostShim {
				shim := newAvailableShim(t)
				attachTmuxFlag = true
				return shim
			},
		},
		{
			name: "SHED_ATTACH=tmux with a roost app running",
			setup: func(t *testing.T) *roostShim {
				shim := newAvailableShim(t)
				t.Setenv("SHED_ATTACH", "tmux")
				return shim
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetAttachState(t)
			// The golden pins UserKnownHostsFile=, which resolves from $HOME.
			t.Setenv("HOME", "/home/tester")

			shim := tc.setup(t)

			var captured []string
			execSSH = func(_ string, argv []string, _ []string) error {
				captured = argv
				return nil
			}

			entry := &config.ServerEntry{Host: "mini3", SSHPort: 2222}
			shed := &config.Shed{LandingDir: "/home/shed/myproj"}
			if err := attachShed("myshed", "myserver", entry, shed); err != nil {
				t.Fatalf("attachShed: %v", err)
			}
			assertShimArgv(t, captured, want)

			if shim != nil {
				if runs := shim.argvRuns(); len(runs) != 0 {
					t.Errorf("the tmux floor was asked for, but roostctl ran %d times: %v", len(runs), runs)
				}
			}
		})
	}
}

// resetAttachState isolates a test from the attach command's package-level
// state: it puts the flags and the exec/config/rig seams back afterwards, and
// starts the test from the defaults a fresh `shed attach` would see — flags at
// their cobra defaults and no SHED_ATTACH, so a case that wants the tmux floor
// has to ask for it rather than inherit it from the developer's shell.
func resetAttachState(t *testing.T) {
	t.Helper()
	session, forceNew, tmux := attachSessionFlag, attachNewFlag, attachTmuxFlag
	exec, cfg, newRoost := execSSH, clientConfig, newRoostAttach
	t.Cleanup(func() {
		attachSessionFlag, attachNewFlag, attachTmuxFlag = session, forceNew, tmux
		execSSH, clientConfig, newRoostAttach = exec, cfg, newRoost
	})
	attachSessionFlag, attachNewFlag, attachTmuxFlag = config.DefaultSessionName, false, false
	t.Setenv("SHED_ATTACH", "")
}

// TestAttachWantsTmuxReadsTheEnvironment pins the env spelling: `tmux` in any
// case is the floor, and any other value is not a mode selector.
func TestAttachWantsTmuxReadsTheEnvironment(t *testing.T) {
	cases := []struct {
		env  string
		flag bool
		want bool
	}{
		{env: "", flag: false, want: false},
		{env: "tmux", flag: false, want: true},
		{env: "TMUX", flag: false, want: true},
		{env: " tmux ", flag: false, want: true},
		{env: "roost", flag: false, want: false},
		{env: "1", flag: false, want: false},
		{env: "", flag: true, want: true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("SHED_ATTACH=%q flag=%v", tc.env, tc.flag), func(t *testing.T) {
			resetAttachState(t)
			t.Setenv("SHED_ATTACH", tc.env)
			attachTmuxFlag = tc.flag
			if got := attachWantsTmux(); got != tc.want {
				t.Errorf("attachWantsTmux() = %v, want %v", got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Step 3 — naming and the ssh alias.
// -----------------------------------------------------------------------

// shedServer is an httptest server that answers GET /api/sheds/<name> for the
// sheds it was given and 404s for everything else.
func shedServer(t *testing.T, sheds ...string) *httptest.Server {
	t.Helper()
	has := map[string]bool{}
	for _, name := range sheds {
		has[name] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/sheds/")
		if !has[name] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(config.Shed{Name: name, Status: config.StatusRunning})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newAliasAttach builds the least roostAttach the naming rule needs: a temp
// `~/.ssh/config` path (the file need not exist) and a roostctl that is not
// there.
//
// The binary is pinned to a path that does NOT exist rather than left to
// resolve on PATH, so a developer with a real roost app installed does not
// have their own saved hosts decide a unit test. A HostList that errors is a
// miss, which is exactly what "this machine knows nothing about this shed yet"
// has to look like.
func newAliasAttach(t *testing.T) *roostAttach {
	t.Helper()
	dir := t.TempDir()
	return &roostAttach{
		ctl:           &roostctl.Client{Bin: filepath.Join(dir, "no-such-roostctl")},
		sshConfigPath: filepath.Join(dir, "config"),
	}
}

// noProbeServer is an API server that fails the test if it is ever asked
// anything. It is how "the ambiguity probe did not run" is asserted: once a
// shed already has an alias on this machine, the probe's answer must not be
// consulted at all — and a probe that ran and was ignored would still be a
// network round trip on every attach.
func noProbeServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the ambiguity probe asked for %s, but this shed already has an alias", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRoostAliasForNaming covers §3.3 step 3's naming rule, including the
// direction an unreachable server resolves in.
func TestRoostAliasForNaming(t *testing.T) {
	t.Run("unique across servers keeps the short form", func(t *testing.T) {
		resetAttachState(t)
		mine := shedServer(t, "web")
		other := shedServer(t, "api")
		clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{
			"mini3": {Host: "mini3", APIURL: mine.URL, SSHPort: 2222},
			"mini4": {Host: "mini4", APIURL: other.URL, SSHPort: 2222},
		}}
		if got := newAliasAttach(t).aliasFor(context.Background(), "web", "mini3"); got != "shed-web" {
			t.Errorf("aliasFor = %q, want %q", got, "shed-web")
		}
	})

	t.Run("a same-named shed elsewhere qualifies both servers' aliases", func(t *testing.T) {
		resetAttachState(t)
		mine := shedServer(t, "web")
		other := shedServer(t, "web")
		clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{
			"mini3": {Host: "mini3", APIURL: mine.URL, SSHPort: 2222},
			"mini4": {Host: "mini4", APIURL: other.URL, SSHPort: 2222},
		}}
		if got := newAliasAttach(t).aliasFor(context.Background(), "web", "mini3"); got != "shed-mini3-web" {
			t.Errorf("aliasFor = %q, want %q", got, "shed-mini3-web")
		}
		if got := newAliasAttach(t).aliasFor(context.Background(), "web", "mini4"); got != "shed-mini4-web" {
			t.Errorf("aliasFor from the other side = %q, want %q", got, "shed-mini4-web")
		}
	})

	t.Run("a server that never answers cannot stall the attach", func(t *testing.T) {
		resetAttachState(t)
		// The budget is the whole point: a hung server must cost the alias
		// step its budget and no more, however long the API client itself
		// would have waited (30s).
		origBudget := aliasAmbiguityBudget
		aliasAmbiguityBudget = 50 * time.Millisecond
		t.Cleanup(func() { aliasAmbiguityBudget = origBudget })

		// Cleanups run LIFO, so the unblock is registered LAST: httptest's
		// Close waits for the in-flight request, and a handler still parked
		// on `block` would deadlock the test rather than fail it.
		block := make(chan struct{})
		hung := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			<-block
		}))
		t.Cleanup(hung.Close)
		t.Cleanup(func() { close(block) })
		mine := shedServer(t, "web")
		clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{
			"mini3": {Host: "mini3", APIURL: mine.URL, SSHPort: 2222},
			"hung":  {Host: "hung", APIURL: hung.URL, SSHPort: 2222},
		}}

		start := time.Now()
		got := newAliasAttach(t).aliasFor(context.Background(), "web", "mini3")
		elapsed := time.Since(start)

		if got != "shed-web" {
			t.Errorf("aliasFor = %q, want %q", got, "shed-web")
		}
		if elapsed > time.Second {
			t.Errorf("the alias step took %s; the budget is %s", elapsed, aliasAmbiguityBudget)
		}
	})

	t.Run("an unreachable server cannot make a name ambiguous", func(t *testing.T) {
		resetAttachState(t)
		mine := shedServer(t, "web")
		dead := shedServer(t, "web")
		deadURL := dead.URL
		dead.Close() // nothing is listening there any more
		clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{
			"mini3": {Host: "mini3", APIURL: mine.URL, SSHPort: 2222},
			"gone":  {Host: "gone", APIURL: deadURL, SSHPort: 2222},
		}}
		if got := newAliasAttach(t).aliasFor(context.Background(), "web", "mini3"); got != "shed-web" {
			t.Errorf("aliasFor = %q, want %q (a server that answers nothing makes nothing ambiguous)", got, "shed-web")
		}
	})
}

// TestShedNameIsAmbiguousDoesNotRaceOnTheConfig is the regression test for the
// crash astra found. A probe worker builds an API client, and building one can
// re-mint a near-expiry credential and write it back into
// `clientConfig.Servers` through updateClientConfig — which holds configMu and
// mutates that very map — while the launching loop was still reading the same
// map, unsynchronised, to hand the NEXT worker its entry. In Go that is not a
// stale read; it is a fatal "concurrent map read and map write" that takes
// `shed` down mid-attach.
//
// The writer here is the real one's SHAPE rather than the real one itself: the
// credential path wants an mTLS server and a config file to write through, and
// the only thing that decides this race is that a writer holds configMu while
// this function touches the map. Run under `-race`, which is where an
// unsynchronised read is visible without having to lose the coin toss.
func TestShedNameIsAmbiguousDoesNotRaceOnTheConfig(t *testing.T) {
	resetAttachState(t)
	origBudget := aliasAmbiguityBudget
	aliasAmbiguityBudget = 50 * time.Millisecond
	t.Cleanup(func() { aliasAmbiguityBudget = origBudget })

	// A dead endpoint: every probe fails fast, because what is under test is
	// the fan-out's bookkeeping and not its answer.
	dead := shedServer(t, "web")
	deadURL := dead.URL
	dead.Close()
	servers := map[string]config.ServerEntry{"mini3": {Host: "mini3", APIURL: deadURL, SSHPort: 2222}}
	for i := range 4 {
		servers[fmt.Sprintf("other%d", i)] = config.ServerEntry{
			Host: fmt.Sprintf("other%d", i), APIURL: deadURL, SSHPort: 2222,
		}
	}
	clientConfig = &config.ClientConfig{Servers: servers}

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// Exactly what updateClientConfig's mutation does to this map,
			// under the same lock, minus the file lock its cross-process half
			// also takes.
			configMu.Lock()
			clientConfig.Servers[fmt.Sprintf("churn%d", i%4)] = config.ServerEntry{
				Host: "churn", APIURL: deadURL, SSHPort: 2222,
			}
			configMu.Unlock()
		}
	}()
	t.Cleanup(func() { close(stop); <-done })

	for range 10 {
		shedNameIsAmbiguous("web", "mini3")
	}
}

// TestRoostAliasIsStableAcrossRuns is the regression test for the defect sol
// found: the ambiguity probe answers over a network under a two-second budget,
// so its answer is not the same twice, and an alias derived from it afresh on
// every attach flips. The run that timed out minted `shed-web`; the next run,
// with a fast answer, would mint `shed-mini3-web` — two `Host` entries and two
// saved roost hosts for one shed.
//
// The fix is STABILITY, not a more reliable probe: an alias this shed already
// has wins outright and nothing is probed. Every case below therefore sets the
// configured servers so that probing would give the OTHER answer, and asserts
// both that the existing alias is kept and (through noProbeServer) that no
// probe was made at all.
func TestRoostAliasIsStableAcrossRuns(t *testing.T) {
	// ambiguousConfig makes "web" ambiguous across mini3 and mini4, so any
	// case that reached the probe would come back qualified.
	ambiguousConfig := func(t *testing.T) {
		t.Helper()
		mine := noProbeServer(t)
		other := noProbeServer(t)
		clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{
			"mini3": {Host: "mini3", APIURL: mine.URL, SSHPort: 2222},
			"mini4": {Host: "mini4", APIURL: other.URL, SSHPort: 2222},
		}}
	}

	t.Run("an ssh alias from a timed-out first run sticks", func(t *testing.T) {
		resetAttachState(t)
		ambiguousConfig(t)
		a := newAliasAttach(t)
		// What attach #1 wrote after its probe timed out.
		writeTestFile(t, a.sshConfigPath, "Host shed-web\n  HostName mini3\n  User web\n", 0o600)

		if got := a.aliasFor(context.Background(), "web", "mini3"); got != "shed-web" {
			t.Errorf("aliasFor = %q, want the alias this shed already has (%q)", got, "shed-web")
		}
	})

	t.Run("a qualified ssh alias sticks even when the name is not ambiguous", func(t *testing.T) {
		resetAttachState(t)
		// Only one server, so the probe would say "unambiguous" and mint the
		// short form — undoing the qualified alias an earlier run wrote.
		clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{
			"mini3": {Host: "mini3", APIURL: noProbeServer(t).URL, SSHPort: 2222},
		}}
		a := newAliasAttach(t)
		writeTestFile(t, a.sshConfigPath, "Host shed-mini3-web\n  HostName mini3\n  User web\n", 0o600)

		if got := a.aliasFor(context.Background(), "web", "mini3"); got != "shed-mini3-web" {
			t.Errorf("aliasFor = %q, want %q", got, "shed-mini3-web")
		}
	})

	t.Run("the qualified alias wins when the machine carries both", func(t *testing.T) {
		resetAttachState(t)
		ambiguousConfig(t)
		a := newAliasAttach(t)
		// A machine that already has the duplication this fix prevents. The
		// qualified one can only ever have meant mini3's web; the short one
		// may belong to mini4's.
		writeTestFile(t, a.sshConfigPath,
			"Host shed-web\n  HostName mini4\n\nHost shed-mini3-web\n  HostName mini3\n", 0o600)

		if got := a.aliasFor(context.Background(), "web", "mini3"); got != "shed-mini3-web" {
			t.Errorf("aliasFor = %q, want the unambiguous %q", got, "shed-mini3-web")
		}
	})

	t.Run("a saved roost host is enough when the ssh entry was removed", func(t *testing.T) {
		resetAttachState(t)
		ambiguousConfig(t)
		shim := newRoostShim(t)
		shim.reply("host.list", hostStatusDoc(
			hostRowJSON(testHostID, "shed-web", "shed-web", 0, roostctl.StateDisconnected, "", ""),
		))
		a := &roostAttach{
			ctl:           &roostctl.Client{},
			sshConfigPath: filepath.Join(t.TempDir(), "config"), // never written
		}

		if got := a.aliasFor(context.Background(), "web", "mini3"); got != "shed-web" {
			t.Errorf("aliasFor = %q, want the alias roost already has saved (%q)", got, "shed-web")
		}
	})

	t.Run("no alias anywhere still probes", func(t *testing.T) {
		resetAttachState(t)
		mine := shedServer(t, "web")
		other := shedServer(t, "web")
		clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{
			"mini3": {Host: "mini3", APIURL: mine.URL, SSHPort: 2222},
			"mini4": {Host: "mini4", APIURL: other.URL, SSHPort: 2222},
		}}
		// Empty ssh config, no roostctl: the probe is the only thing left to
		// answer, and it must still be asked.
		if got := newAliasAttach(t).aliasFor(context.Background(), "web", "mini3"); got != "shed-mini3-web" {
			t.Errorf("aliasFor = %q, want the probe's answer %q", got, "shed-mini3-web")
		}
	})
}

// TestEnsureSSHAliasWritesOnce covers §3.3 step 3's ssh-alias half against a
// temp config file: it writes exactly one entry, says so once, and never
// touches an alias that already exists.
func TestEnsureSSHAliasWritesOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), ".ssh", "config")
	out := &bytes.Buffer{}
	a := &roostAttach{sshConfigPath: path, out: out}
	entry := &config.ServerEntry{Host: "mini3", SSHPort: 2222}

	if err := a.ensureSSHAlias("shed-myproj", "myproj", entry); err != nil {
		t.Fatalf("ensureSSHAlias: %v", err)
	}
	if want := "wrote Host shed-myproj to " + path + "\n"; out.String() != want {
		t.Errorf("first call printed %q, want %q", out.String(), want)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	for _, want := range []string{"Host shed-myproj", "HostName mini3", "Port 2222", "User myproj", "UserKnownHostsFile "} {
		if !strings.Contains(string(first), want) {
			t.Errorf("written entry is missing %q:\n%s", want, first)
		}
	}
	if strings.Contains(string(first), "IdentityFile") {
		t.Errorf("the entry must carry no IdentityFile (same shape as generateEntries):\n%s", first)
	}

	// Second attach, same shed: nothing written, nothing said.
	out.Reset()
	if err := a.ensureSSHAlias("shed-myproj", "myproj", entry); err != nil {
		t.Fatalf("second ensureSSHAlias: %v", err)
	}
	if out.String() != "" {
		t.Errorf("second call printed %q, want nothing", out.String())
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading the config: %v", err)
	}
	if string(second) != string(first) {
		t.Errorf("second call rewrote the config:\n%s\n---\n%s", first, second)
	}
}

// TestEnsureSSHAliasLeavesAUserEntryAlone is the guarantee that matters most
// here: a hand-written `Host shed-myproj` outside the managed block wins, and
// the file is left byte-identical.
func TestEnsureSSHAliasLeavesAUserEntryAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config")
	original := "Host shed-myproj\n  HostName bastion\n  ProxyJump jump\n  IdentityFile ~/.ssh/special\n"
	writeTestFile(t, path, original, 0o600)

	out := &bytes.Buffer{}
	a := &roostAttach{sshConfigPath: path, out: out}
	if err := a.ensureSSHAlias("shed-myproj", "myproj", &config.ServerEntry{Host: "mini3", SSHPort: 2222}); err != nil {
		t.Fatalf("ensureSSHAlias: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the config: %v", err)
	}
	if string(after) != original {
		t.Errorf("a user-authored entry was modified:\n%s", after)
	}
	if out.String() != "" {
		t.Errorf("nothing was written, but the flow printed %q", out.String())
	}
	// Belt and braces on the outcome the helper returns, so a future change
	// that started writing here cannot pass by printing nothing.
	outcome, err := sshconfig.AddEntryIfAbsent(path, sshconfig.Entry{Name: "shed-myproj"})
	if err != nil {
		t.Fatalf("AddEntryIfAbsent: %v", err)
	}
	if outcome != sshconfig.AlreadyUserDefined {
		t.Errorf("outcome = %v, want AlreadyUserDefined", outcome)
	}
}

// TestTildePath covers the one cosmetic thing the alias step promises: the
// line it prints names `~/.ssh/config`, which is what the user would type.
func TestTildePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := []struct{ path, want string }{
		{path: filepath.Join(home, ".ssh", "config"), want: "~" + string(filepath.Separator) + filepath.Join(".ssh", "config")},
		{path: home, want: "~"},
		{path: "/etc/ssh/ssh_config", want: "/etc/ssh/ssh_config"},
	}
	for _, tc := range cases {
		if got := tildePath(tc.path); got != tc.want {
			t.Errorf("tildePath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// -----------------------------------------------------------------------
// Step 4 — the saved host.
// -----------------------------------------------------------------------

// TestEnsureSavedHostMatching covers §3.3 step 4: target, then add — and the
// label collision that is neither.
func TestEnsureSavedHostMatching(t *testing.T) {
	t.Run("matches on target even when a label collides", func(t *testing.T) {
		rig := newRoostRig(t, nil, true)
		// A host the user renamed (label "old name", target shed-myproj) and
		// a DIFFERENT host that happens to be labelled shed-myproj. Target
		// wins: it is what the host actually reaches.
		rig.shim.reply("host.list", hostStatusDoc(
			hostRowJSON("renamed-id", "old name", testHostLabel, 2, roostctl.StateDisconnected, "", ""),
			hostRowJSON("other-id", testHostLabel, "someone@else", 0, roostctl.StateDisconnected, "", ""),
		))
		host, err := rig.attach.ensureSavedHost(context.Background(), testHostLabel)
		if err != nil {
			t.Fatalf("ensureSavedHost: %v", err)
		}
		if host.ID != "renamed-id" {
			t.Errorf("matched host %q, want the one whose TARGET is the alias", host.ID)
		}
		if runs := rig.shim.runsOf("host", "add"); len(runs) != 0 {
			t.Errorf("an existing host must not be re-added: %v", runs)
		}
	})

	// A label that matches while the TARGET does not used to be accepted as a
	// fallback, and that is the defect both reviewers found: the saved host
	// labelled `shed-myproj` here reaches a different machine entirely, so
	// connecting it connects the wrong box — after which step 6 still opens
	// the tab on the real shed over its own ssh and step 7 hunts for that
	// numeric tab id under the WRONG host's sidebar section, where one may
	// well exist. The user gets `attached …`, exit 0, and a stranger's tab.
	//
	// Adding a second host under the same label is not the way out either:
	// roost rejects duplicate labels case-insensitively, so the add fails with
	// roost's own refusal naming nothing the user can find. Naming both the
	// label and the target roost has is the only actionable outcome.
	t.Run("a label match with a different target is refused, not used", func(t *testing.T) {
		for _, label := range []string{testHostLabel, strings.ToUpper(testHostLabel)} {
			t.Run(label, func(t *testing.T) {
				rig := newRoostRig(t, nil, true)
				rig.shim.reply("host.list", hostStatusDoc(
					hostRowJSON(testHostID, label, "someone@else", 1, roostctl.StateDisconnected, "", ""),
				))
				_, err := rig.attach.ensureSavedHost(context.Background(), testHostLabel)
				if err == nil {
					t.Fatal("a host labelled like this shed but targeting another machine must not be used")
				}
				for _, want := range []string{label, "someone@else", testHostLabel} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("message %q does not name %q", err.Error(), want)
					}
				}
				// And nothing was saved: a duplicate label would be refused by
				// roost anyway, with a worse message.
				if runs := rig.shim.runsOf("host", "add"); len(runs) != 0 {
					t.Errorf("nothing should have been added: %v", runs)
				}
			})
		}
	})

	t.Run("a label match whose target IS the alias is the ordinary target match", func(t *testing.T) {
		rig := newRoostRig(t, nil, true)
		rig.shim.reply("host.list", hostStatusDoc(
			hostRowJSON(testHostID, testHostLabel, testHostLabel, 1, roostctl.StateDisconnected, "", ""),
		))
		host, err := rig.attach.ensureSavedHost(context.Background(), testHostLabel)
		if err != nil {
			t.Fatalf("ensureSavedHost: %v", err)
		}
		if host.ID != testHostID {
			t.Errorf("matched host %q, want %q", host.ID, testHostID)
		}
	})

	t.Run("adds one without --verify", func(t *testing.T) {
		rig := newRoostRig(t, nil, true)
		rig.shim.reply("host.list", hostStatusDoc())
		rig.shim.reply("host.add", hostAddDoc(testHostID, testHostLabel, testHostLabel))
		host, err := rig.attach.ensureSavedHost(context.Background(), testHostLabel)
		if err != nil {
			t.Fatalf("ensureSavedHost: %v", err)
		}
		if host.ID != testHostID {
			t.Errorf("added host %q, want %q", host.ID, testHostID)
		}
		runs := rig.shim.runsOf("host", "add")
		if len(runs) != 1 {
			t.Fatalf("want exactly one `host add`, got %v", runs)
		}
		// The argv IS the contract: label and target are both the alias, and
		// --verify is absent (a shed that has just started may have no
		// session listening yet — step 5 is what starts one).
		assertShimArgv(t, runs[0], []string{
			"host", "add", "--label", testHostLabel, "--target", testHostLabel, "--json",
		})
	})
}

// -----------------------------------------------------------------------
// Step 5 — the generation fence.
// -----------------------------------------------------------------------

// The reason strings below are roost's own, copied from
// roost-ipc/src/ssh.rs's `SshFailure::message` at the pinned rev (8c91ce9,
// roost v0.0.20; that file is byte-identical to ee71e44's, where they were copied from) —
// the copy a failed host connection puts in `HostStatus.reason`.
const (
	roostReasonNoSession = "shed-myproj is reachable but has no roost session running. " +
		"Run `roostctl session start` on that machine, then try again."
	roostReasonNotFoundCopy = "roost-session isn't installed on shed-myproj (or isn't on the non-interactive " +
		"PATH ssh uses there) — connect from the Roost app to install it."
	roostReasonCommandNotFound = "connecting to shed-myproj failed: bash: roost-session: command not found"
	roostReasonAuth            = "shed-myproj refused authentication. Check that your key is loaded in an agent, " +
		"then try `ssh shed-myproj` in a terminal to confirm you can log in."
	roostReasonChangedHostKey = "the host key for shed-myproj has CHANGED since it was last seen — this can mean " +
		"the host was reinstalled, or that something is impersonating it. Do not accept the new key from here; " +
		"verify its fingerprint with shed-myproj out-of-band (e.g. a call, or a channel other than this one) " +
		"before connecting again."
	roostReasonTransport = "connecting to shed-myproj failed: ssh: connect to host mini3 port 2222: Connection refused"
)

// wantNoSessionMessage is the pinned copy §3.3 step 5 requires.
const wantNoSessionMessage = `shed-myproj has no roost-session. In roost: Cmd/Alt-Shift-P → ` +
	`"Connect Host: shed-myproj" installs and starts one. Or connect to it from the shed desktop.`

// TestConnectFenceSucceedsThroughAStaleDisconnectedRow is the regression test
// for the fence defect measured live (live-09-generation-fence.txt): roost
// bumps `generation` when the attempt STARTS, so the first poll after a
// connect shows the new generation with `state=disconnected` and the previous
// attempt's reason still attached — ~250 ms before the connect succeeds.
//
// The trace here is that measurement, one poll per recorded sample. The fence
// must return SUCCESS.
func TestConnectFenceSucceedsThroughAStaleDisconnectedRow(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.replySeq("host.status",
		// baseline: generation 2, not connected
		hostStatusDoc(statusRow(2, roostctl.StateDisconnected, "")),
		// 250ms: the attempt has bumped the generation; the state has not
		// caught up and the reason is the PREVIOUS attempt's
		hostStatusDoc(statusRow(3, roostctl.StateDisconnected, roostReasonNoSession)),
		// 500ms: connected
		hostStatusDoc(statusRow(3, roostctl.StateConnected, "")),
	)

	if err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID); err != nil {
		t.Fatalf("the live success trace must connect, got: %v", err)
	}
	if rig.clock.slept > time.Second {
		t.Errorf("the fence waited %s for a connect that lands at 500ms", rig.clock.slept)
	}
}

// TestConnectFenceReportsAPersistentFailure is the other half of the same
// amendment: the same `disconnected`-with-a-reason row, this time never
// changing, must settle into a failure once it has persisted — and fast,
// nowhere near the 20 s cap.
func TestConnectFenceReportsAPersistentFailure(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.replySeq("host.status",
		hostStatusDoc(statusRow(2, roostctl.StateDisconnected, "")),
		// Every poll from here on is this same row — what a genuine failure
		// looks like (measured: unchanged for 24 s+).
		hostStatusDoc(statusRow(3, roostctl.StateDisconnected, roostReasonNoSession)),
	)

	err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID)
	if err == nil {
		t.Fatal("a reason that never changes must settle into a failure")
	}
	if err.Error() != wantNoSessionMessage {
		t.Errorf("message:\n got %q\nwant %q", err.Error(), wantNoSessionMessage)
	}
	// It must settle on the persistence window, not on the cap.
	if rig.clock.slept >= defaultRoostConnectCap {
		t.Errorf("failed only at the cap (%s); the settle window should have fired at ~%s",
			rig.clock.slept, defaultRoostConnectSettle)
	}
	if rig.clock.slept < defaultRoostConnectSettle {
		t.Errorf("settled after %s, which is inside the %s window", rig.clock.slept, defaultRoostConnectSettle)
	}
}

// TestConnectFenceIgnoresAStaleConnectedStatus is the generation comparison's
// own discriminator, and the most important test in this commit.
//
// Every status here says `connected` — but at the BASELINE generation, so none
// of them describes the attempt this run started. (That is the real shape of a
// host that was connected a moment ago and dropped: the row lingers.) The
// fence must not accept any of them, and must run out the cap instead.
//
// Delete the `status.Generation > baseline` comparison and keep the state
// check, and this test goes green-to-red immediately: the flow returns nil.
func TestConnectFenceIgnoresAStaleConnectedStatus(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.replySeq("host.status",
		// baseline: generation 7, disconnected (so a connect is issued)
		hostStatusDoc(statusRow(7, roostctl.StateDisconnected, "")),
		// and then a `connected` row that never leaves generation 7
		hostStatusDoc(statusRow(7, roostctl.StateConnected, "")),
	)

	err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID)
	if err == nil {
		t.Fatal("a `connected` status at the BASELINE generation must not satisfy the fence")
	}
	want := fmt.Sprintf("still connecting to %s; check roostctl host status --id %s or retry", testHostLabel, testHostID)
	if err.Error() != want {
		t.Errorf("message:\n got %q\nwant %q", err.Error(), want)
	}
	if rig.clock.slept < defaultRoostConnectCap {
		t.Errorf("gave up after %s, want the full %s cap", rig.clock.slept, defaultRoostConnectCap)
	}
}

// TestConnectFenceSkipsAnAlreadyConnectedHost covers step 5's first sentence:
// read the status FIRST, and if it is connected, do not connect at all.
func TestConnectFenceSkipsAnAlreadyConnectedHost(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.shim.reply("host.status", hostStatusDoc(statusRow(4, roostctl.StateConnected, "")))

	if err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID); err != nil {
		t.Fatalf("connectHost: %v", err)
	}
	if runs := rig.shim.runsOf("host", "connect"); len(runs) != 0 {
		t.Errorf("an already-connected host must not be reconnected: %v", runs)
	}
	if runs := rig.shim.runsOf("host", "status"); len(runs) != 1 {
		t.Errorf("want exactly one status read, got %d", len(runs))
	}
	// The argv is the contract here too.
	assertShimArgv(t, rig.shim.runsOf("host", "status")[0],
		[]string{"host", "status", "--id", testHostID, "--json"})
}

// TestConnectFenceTreatsTerminalStatesAsSettledAtOnce: `stopped` and
// `needs-restart` are roost saying this will not come up on its own, so they
// do not wait out the persistence window.
func TestConnectFenceTreatsTerminalStatesAsSettledAtOnce(t *testing.T) {
	for _, state := range []string{roostctl.StateStopped, roostctl.StateNeedsRestart} {
		t.Run(state, func(t *testing.T) {
			rig := newRoostRig(t, nil, true)
			rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
			rig.shim.replySeq("host.status",
				hostStatusDoc(statusRow(1, roostctl.StateDisconnected, "")),
				hostStatusDoc(statusRow(2, state, roostReasonTransport)),
			)
			err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID)
			if err == nil {
				t.Fatalf("state %q must be a settled failure", state)
			}
			if err.Error() != roostReasonTransport {
				t.Errorf("message:\n got %q\nwant roost's reason verbatim %q", err.Error(), roostReasonTransport)
			}
			if rig.clock.slept != defaultRoostConnectPoll {
				t.Errorf("settled after %s, want the first poll (%s)", rig.clock.slept, defaultRoostConnectPoll)
			}
		})
	}
}

// TestConnectFenceWaitsOutConnecting: a `connecting` status is this attempt in
// flight and never settles anything, however long it goes on.
func TestConnectFenceWaitsOutConnecting(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.replySeq("host.status",
		hostStatusDoc(statusRow(0, roostctl.StateDisconnected, "")),
		hostStatusDoc(statusRow(1, roostctl.StateConnecting, "")),
	)
	err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID)
	if err == nil || !strings.HasPrefix(err.Error(), "still connecting to") {
		t.Fatalf("want the cap message, got: %v", err)
	}
	// generation 0 is a legitimate baseline — a host that has never connected
	// — so the fence must have issued the connect rather than treating 0 as
	// "no data".
	if runs := rig.shim.runsOf("host", "connect"); len(runs) != 1 {
		t.Errorf("want exactly one `host connect` from a generation-0 baseline, got %v", runs)
	}
}

// TestConnectFenceCapBoundsEachStatusRead is the regression test for the
// overrun sol found: the cap was compared to the clock only BETWEEN polls, and
// a single `host status` can block for roostctl.ExecTimeout (10 s) on its own
// — so the advertised 20 s could take 30. Deriving the polling context from
// the same deadline is what makes the number real.
//
// Real time and the real clock here, not the rig's fake one: what is asserted
// is that a CALL is cut short, which is a property of the context handed to
// os/exec and entirely invisible to a clock the test advances by hand.
func TestConnectFenceCapBoundsEachStatusRead(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.attach.now, rig.attach.sleep = time.Now, time.Sleep
	rig.attach.connectPoll = 10 * time.Millisecond
	rig.attach.connectCap = 300 * time.Millisecond
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.replySeq("host.status", hostStatusDoc(statusRow(2, roostctl.StateDisconnected, "")))
	// Every poll after the baseline wedges: the socket took the call and
	// nothing ever comes back, which is what a roost app paused in a debugger
	// or mid-relaunch looks like from here.
	rig.shim.hangSeq("host.status", 2, 30)

	start := time.Now()
	err := rig.attach.connectHost(context.Background(), testHostLabel, testHostID)
	elapsed := time.Since(start)

	want := fmt.Sprintf("still connecting to %s; check roostctl host status --id %s or retry", testHostLabel, testHostID)
	if err == nil || err.Error() != want {
		t.Fatalf("the cap must fire with its own message, got: %v", err)
	}
	// Generous, because what it has to separate is 300 ms from roostctl's own
	// 10 s ceiling — the number the unbounded version would have taken.
	if elapsed > 5*time.Second {
		t.Errorf("connectHost took %s; one wedged `host status` outlived the %s cap", elapsed, rig.attach.connectCap)
	}
}

// TestConnectFailureMessage is the reason classifier, against roost's own
// strings (roost-ipc/src/ssh.rs `SshFailure::message`, pinned rev).
func TestConnectFailureMessage(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		detail string
		state  string
		want   string
	}{
		{
			name:   "roost's NoSession copy becomes shed's own remedy",
			reason: roostReasonNoSession,
			state:  roostctl.StateDisconnected,
			want:   wantNoSessionMessage,
		},
		{
			name:   "the exec chain's raw command-not-found does too",
			reason: roostReasonCommandNotFound,
			state:  roostctl.StateDisconnected,
			want:   wantNoSessionMessage,
		},
		{
			// roost's NotFound copy is KEPT and extended, not replaced (plan
			// 022 A9). It names the non-interactive PATH, which is the real
			// cause often enough to be worth more than a generic message;
			// shed appends only the second door D4 requires and roost cannot
			// know about. See TestNotFoundKeepsRoostsOwnRemedy.
			name:   "roost's NotFound copy is kept, with shed's second door appended",
			reason: roostReasonNotFoundCopy,
			state:  roostctl.StateDisconnected,
			want:   roostReasonNotFoundCopy + "\nOr connect to it from the shed desktop.",
		},
		{
			name:   "an auth failure is roost's to explain",
			reason: roostReasonAuth,
			state:  roostctl.StateDisconnected,
			want:   roostReasonAuth,
		},
		{
			name:   "a changed host key is never reworded",
			reason: roostReasonChangedHostKey,
			state:  roostctl.StateDisconnected,
			want:   roostReasonChangedHostKey,
		},
		{
			name:   "a detail is printed under the reason",
			reason: "roost-session failed to start",
			detail: "/usr/bin/roost-session: exited 2: could not bind /tmp/roost.sock",
			state:  roostctl.StateDisconnected,
			want:   "roost-session failed to start\n/usr/bin/roost-session: exited 2: could not bind /tmp/roost.sock",
		},
		{
			name:  "a settled state with no reason still says something",
			state: roostctl.StateNeedsRestart,
			want:  `roost could not connect to shed-myproj (state "needs-restart")`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := connectFailureMessage(testHostLabel, roostctl.HostStatus{
				State:  tc.state,
				Reason: tc.reason,
				Detail: tc.detail,
			})
			if got != tc.want {
				t.Errorf("message:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Step 6 — tabs, over the provider transport.
// -----------------------------------------------------------------------

// newReuseRig builds a rig whose far side lists tabs the test describes in
// terms of the shed's own landing dir — which is a temp path only the rig
// knows, so the tabs have to be staged after it exists.
func newReuseRig(t *testing.T, tabs func(landing, home string) []roostprovider.Tab) *roostRig {
	t.Helper()
	rig := newRoostRig(t, nil, true)
	rig.shed.setTabs(tabs(rig.shed.landingDir, rig.shed.home))
	return rig
}

// TestEnsureTabReusesATabOfTheSameTitle: `shed attach web` twice lands in the
// same tab, and opens nothing.
func TestEnsureTabReusesATabOfTheSameTitle(t *testing.T) {
	rig := newReuseRig(t, func(landing, home string) []roostprovider.Tab {
		return []roostprovider.Tab{
			{ID: "11", ProjectID: "1", Title: "shell", Cwd: home, State: "running"},
			{ID: "12", ProjectID: "1", Title: "default", Cwd: landing, State: "running"},
		}
	})

	tabID, err := rig.attach.ensureTab(context.Background(), "myproj", rig.shedEntry(), rig.shedConfig(), "default", false)
	if err != nil {
		t.Fatalf("ensureTab: %v", err)
	}
	if tabID != "12" {
		t.Errorf("reused tab %q, want %q", tabID, "12")
	}
	if n := rig.shed.countRequests("tab.open"); n != 0 {
		t.Errorf("reuse must open nothing, saw %d tab.open requests", n)
	}
}

// TestEnsureTabIgnoresATabInAnotherDirectory is the regression test for the
// defect both reviewers found: a shed's roost session holds EVERY project on
// that machine, the default title is `default`, and "the first tab titled
// `default`" is therefore routinely somebody else's. Reusing it focused a
// shell sitting in another project's directory and reported success.
//
// The tab here has the right title and the wrong cwd, so it must be passed
// over and a real tab opened in the landing dir.
func TestEnsureTabIgnoresATabInAnotherDirectory(t *testing.T) {
	rig := newReuseRig(t, func(landing, home string) []roostprovider.Tab {
		return []roostprovider.Tab{
			{ID: "11", ProjectID: "1", Title: "default", Cwd: filepath.Join(home, "another-project"), State: "running"},
		}
	})

	tabID, err := rig.attach.ensureTab(context.Background(), "myproj", rig.shedEntry(), rig.shedConfig(), "default", false)
	if err != nil {
		t.Fatalf("ensureTab: %v", err)
	}
	if tabID != "5" {
		t.Errorf("reused tab %q; another project's `default` is not this shed's tab", tabID)
	}
	// And the tab that WAS opened is in the landing dir, which is the whole
	// point of not reusing the other one.
	line := rig.shed.requestFor("tab.open")
	if !strings.Contains(line, fmt.Sprintf(`"cwd":%q`, rig.shed.landingDir)) {
		t.Errorf("tab.open did not use the landing dir:\n%s", line)
	}
}

// TestEnsureTabPrefersALiveTabOverAFinishedOne pins the tie-break: several
// tabs can legitimately share a title and a cwd (`--new` mints them), and a
// `finished` row is a dead shell the user cannot type in.
func TestEnsureTabPrefersALiveTabOverAFinishedOne(t *testing.T) {
	t.Run("a live tab wins", func(t *testing.T) {
		rig := newReuseRig(t, func(landing, home string) []roostprovider.Tab {
			return []roostprovider.Tab{
				{ID: "11", ProjectID: "1", Title: "default", Cwd: filepath.Join(home, "elsewhere"), State: "running"},
				{ID: "12", ProjectID: "1", Title: "default", Cwd: landing, State: "finished"},
				{ID: "13", ProjectID: "1", Title: "default", Cwd: landing, State: "running"},
			}
		})
		tabID, err := rig.attach.ensureTab(context.Background(), "myproj", rig.shedEntry(), rig.shedConfig(), "default", false)
		if err != nil {
			t.Fatalf("ensureTab: %v", err)
		}
		if tabID != "13" {
			t.Errorf("reused tab %q, want the LIVE one (%q)", tabID, "13")
		}
	})

	t.Run("a finished tab is the last resort, not a blocker", func(t *testing.T) {
		rig := newReuseRig(t, func(landing, _ string) []roostprovider.Tab {
			return []roostprovider.Tab{
				{ID: "12", ProjectID: "1", Title: "default", Cwd: landing, State: "finished"},
			}
		})
		tabID, err := rig.attach.ensureTab(context.Background(), "myproj", rig.shedEntry(), rig.shedConfig(), "default", false)
		if err != nil {
			t.Fatalf("ensureTab: %v", err)
		}
		if tabID != "12" {
			t.Errorf("reused tab %q, want %q — skipping it would open a fresh tab beside the dead one on every attach",
				tabID, "12")
		}
	})
}

// TestReusableTab is the picker on its own, where the orderings are cheap to
// state: the far side's own order decides within a class, and a finished tab
// only ever wins when there is no live one.
func TestReusableTab(t *testing.T) {
	const landing = "/home/shed/proj"
	tab := func(id, title, cwd, state string) roostprovider.Tab {
		return roostprovider.Tab{ID: id, Title: title, Cwd: cwd, State: state}
	}
	projects := func(tabs ...roostprovider.Tab) []roostprovider.Project {
		return []roostprovider.Project{{ID: "1", Tabs: tabs}}
	}
	cases := []struct {
		name     string
		projects []roostprovider.Project
		want     string
		wantOK   bool
	}{
		{name: "nothing at all", projects: nil},
		{
			name:     "the right title in the wrong directory",
			projects: projects(tab("11", "default", "/home/shed/other", "running")),
		},
		{
			name:     "the right directory under another title",
			projects: projects(tab("11", "notes", landing, "running")),
		},
		{
			name:     "title and cwd both match",
			projects: projects(tab("11", "default", landing, "running")),
			want:     "11", wantOK: true,
		},
		{
			name:     "the first live match wins, in the far side's order",
			projects: projects(tab("11", "default", landing, "running"), tab("12", "default", landing, "running")),
			want:     "11", wantOK: true,
		},
		{
			name:     "a live match later in the list still beats an earlier finished one",
			projects: projects(tab("11", "default", landing, "finished"), tab("12", "default", landing, "running")),
			want:     "12", wantOK: true,
		},
		{
			name:     "the first finished match is the fallback",
			projects: projects(tab("11", "default", landing, "finished"), tab("12", "default", landing, "finished")),
			want:     "11", wantOK: true,
		},
		{
			name: "matches are found across projects",
			projects: []roostprovider.Project{
				{ID: "1", Tabs: []roostprovider.Tab{tab("11", "default", "/home/shed/other", "running")}},
				{ID: "2", Tabs: []roostprovider.Tab{tab("12", "default", landing, "running")}},
			},
			want: "12", wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := reusableTab(tc.projects, "default", landing)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("reusableTab = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestEnsureTabOpensWithTheLandingDirAndNoArgv covers the open path's wire
// shape — including `"argv":[]`, which is NOT cosmetic: roost's
// `TabOpenParams.argv` is a `Vec<String>` whose serde default covers a missing
// key, not a null one, so a nil Go slice's `"argv":null` is refused by the far
// side.
func TestEnsureTabOpensWithTheLandingDirAndNoArgv(t *testing.T) {
	rig := newRoostRig(t, []roostprovider.Tab{
		{ID: "11", ProjectID: "1", Title: "some other tab", Cwd: "/home/shed", State: "running"},
	}, true)

	tabID, err := rig.attach.ensureTab(context.Background(), "myproj", rig.shedEntry(), rig.shedConfig(), "default", false)
	if err != nil {
		t.Fatalf("ensureTab: %v", err)
	}
	if tabID != "5" {
		t.Errorf("opened tab %q, want the vector's %q", tabID, "5")
	}
	line := rig.shed.requestFor("tab.open")
	want := fmt.Sprintf(`{"id":"1","op":"tab.open","params":{"project_id":"0","cwd":%q,"title":"default","argv":[]}}`,
		rig.shed.landingDir)
	if strings.TrimSpace(line) != want {
		t.Errorf("tab.open request:\n got %s\nwant %s", strings.TrimSpace(line), want)
	}
}

// TestEnsureTabFallsBackToHomeWhenTheLandingDirIsMissing: the server can name
// a landing dir the far side does not have, and `tab.open` stores a cwd
// verbatim — so a missing dir is a tab that dies on open unless Home is used.
func TestEnsureTabFallsBackToHomeWhenTheLandingDirIsMissing(t *testing.T) {
	rig := newRoostRig(t, nil, false)

	if _, err := rig.attach.ensureTab(context.Background(), "myproj", rig.shedEntry(), rig.shedConfig(), "default", false); err != nil {
		t.Fatalf("ensureTab: %v", err)
	}
	line := rig.shed.requestFor("tab.open")
	if !strings.Contains(line, fmt.Sprintf(`"cwd":%q`, rig.shed.home)) {
		t.Errorf("tab.open must fall back to the far side's $HOME:\n%s", line)
	}
}

// TestEnsureTabNewForcesAFreshTab: `--new` opens even when a tab of that title
// is sitting right there, and does not even ask for the list.
func TestEnsureTabNewForcesAFreshTab(t *testing.T) {
	rig := newRoostRig(t, []roostprovider.Tab{
		{ID: "12", ProjectID: "1", Title: "default", Cwd: "/home/shed", State: "running"},
	}, true)

	tabID, err := rig.attach.ensureTab(context.Background(), "myproj", rig.shedEntry(), rig.shedConfig(), "default", true)
	if err != nil {
		t.Fatalf("ensureTab: %v", err)
	}
	if tabID != "5" {
		t.Errorf("opened tab %q, want a new one (%q)", tabID, "5")
	}
	if n := rig.shed.countRequests("tab.list"); n != 0 {
		t.Errorf("--new needs no tab.list, saw %d", n)
	}
	if n := rig.shed.countRequests("tab.open"); n != 1 {
		t.Errorf("want exactly one tab.open, saw %d", n)
	}
}

// TestEnsureTabIsBoundedWhenTheFarSideHangs is the regression test for the
// hang both reviewers found. ssh's `ConnectTimeout` bounds the HANDSHAKE only:
// a connection that lands and then meets a `roost-session` bridge which never
// answers waits forever, and under a context.Background() there is nothing to
// kill it with — so `shed attach`, which is supposed to return promptly and
// print one line, never returns at all.
//
// The tmux floor's willingness to hang is not a precedent: it hangs inside an
// interactive session the user is watching and can interrupt.
func TestEnsureTabIsBoundedWhenTheFarSideHangs(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	// An ssh that connects and then says nothing at all. `exec` so the
	// sleeping process IS the child os/exec is waiting on, and an expired
	// context kills the thing that is actually holding the call open.
	hung := filepath.Join(t.TempDir(), "ssh")
	writeTestFile(t, hung, "#!/bin/sh\nexec sleep 30\n", 0o755)
	rig.attach.sshBin = hung
	rig.attach.remoteCap = 200 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := rig.attach.ensureTab(context.Background(), "myproj",
			rig.shedEntry(), rig.shedConfig(), "default", false)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a far side that never answers must not look like a successful probe")
		}
		if !strings.Contains(err.Error(), "probing myproj") {
			t.Errorf("want the probe's own failure, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		// Not t.Fatal in a goroutine — this IS the test goroutine, and the
		// leaked ssh gives up on its own well before the package does.
		t.Fatalf("ensureTab never returned; the remote half has no deadline (cap is %s)", rig.attach.remoteCap)
	}
}

// -----------------------------------------------------------------------
// Step 7 — the sidebar key, the focus, the activate.
// -----------------------------------------------------------------------

// TestFocusTabPollsUntilTheMirrorCatchesUp exercises the poll rather than
// hitting it on the first try: the sidebar lists the tab only on the third
// dump, which is what the UI mirror catching up after a far-side `tab.open`
// actually looks like.
func TestFocusTabPollsUntilTheMirrorCatchesUp(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.shim.replySeq("rpc.app.sidebar_dump",
		sidebarDumpDoc(testHostID),         // the host section, no tabs yet
		sidebarDumpDoc(testHostID, "h3.9"), // some other tab
		sidebarDumpDoc(testHostID, "h3.9", "h3.5"),
	)
	rig.shim.reply("tab.focus", "{}\n")
	rig.shim.reply("rpc.app.activate", "{}\n")

	if err := rig.attach.focusTab(context.Background(), testHostLabel, testHostID, "5", "default"); err != nil {
		t.Fatalf("focusTab: %v", err)
	}
	if n := len(rig.shim.runsOf("rpc", "app.sidebar_dump")); n != 3 {
		t.Errorf("want three sidebar dumps (the poll), got %d", n)
	}
	focus := rig.shim.runsOf("tab", "focus")
	if len(focus) != 1 {
		t.Fatalf("want exactly one `tab focus`, got %v", focus)
	}
	// The `h<incarnation>.<id>` key from the host's OWN section — not the
	// decoy host's h9.5, and not the saved host's opaque id, which
	// `tab focus --tab` would refuse.
	assertShimArgv(t, focus[0], []string{"tab", "focus", "--tab", "h3.5", "--json"})
	if n := len(rig.shim.runsOf("rpc", "app.activate")); n != 1 {
		t.Errorf("want one `rpc app.activate`, got %d", n)
	}
	if rig.errOut.Len() != 0 {
		t.Errorf("unexpected warnings: %s", rig.errOut)
	}
}

// TestFocusTabTimesOutNamingTheTab: the mirror never lists the tab, so the
// flow fails — and the message names the tab, which is the only way to find a
// row that is open and unreachable from here.
func TestFocusTabTimesOutNamingTheTab(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.shim.reply("rpc.app.sidebar_dump", sidebarDumpDoc(testHostID, "h3.9"))

	err := rig.attach.focusTab(context.Background(), testHostLabel, testHostID, "5", "default")
	if err == nil {
		t.Fatal("a sidebar that never lists the tab must fail")
	}
	for _, want := range []string{"5", `"default"`, testHostLabel, "the tab is open"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not name %s", err.Error(), want)
		}
	}
	if runs := rig.shim.runsOf("tab", "focus"); len(runs) != 0 {
		t.Errorf("nothing should be focused without a key: %v", runs)
	}
	if rig.clock.slept < defaultRoostSidebarCap {
		t.Errorf("gave up after %s, want the full %s cap", rig.clock.slept, defaultRoostSidebarCap)
	}
}

// TestFocusTabRetriesWithAFreshKey is the regression test for the stale
// incarnation astra found. `h3.5` and `h4.5` are the SAME session-side tab
// across a reconnect — the incarnation is the UI's own per-connection number —
// and the sidebar dump can still be painting the old one. A retry that
// resubmits the refused key retries a reference that cannot start working; the
// dump has to be re-read.
//
// The trace here is exactly that: the first dump says `h3.5`, roost refuses it
// with `tab_not_found`, the next dump says `h4.5`, and the retry must focus
// THAT.
func TestFocusTabRetriesWithAFreshKey(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.shim.replySeq("rpc.app.sidebar_dump",
		sidebarDumpDoc(testHostID, "h3.5"), // the stale incarnation
		sidebarDumpDoc(testHostID, "h4.5"), // the live one
	)
	rig.shim.replySeq("tab.focus", "", "{}\n")
	rig.shim.failSeq("tab.focus", 1, `{"error":{"code":"tab_not_found","message":"no tab h3.5"}}`+"\n", 1)
	rig.shim.reply("rpc.app.activate", "{}\n")

	if err := rig.attach.focusTab(context.Background(), testHostLabel, testHostID, "5", "default"); err != nil {
		t.Fatalf("the retry found the live key and focused it; that is a success: %v", err)
	}
	focus := rig.shim.runsOf("tab", "focus")
	if len(focus) != 2 {
		t.Fatalf("want one retry (two calls), got %v", focus)
	}
	assertShimArgv(t, focus[0], []string{"tab", "focus", "--tab", "h3.5", "--json"})
	// The point of the whole test: the retry used the key from a FRESH dump,
	// not the one roost had just refused.
	assertShimArgv(t, focus[1], []string{"tab", "focus", "--tab", "h4.5", "--json"})
	if n := len(rig.shim.runsOf("rpc", "app.sidebar_dump")); n != 2 {
		t.Errorf("want the dump re-read before the retry (two dumps), got %d", n)
	}
	if rig.errOut.Len() != 0 {
		t.Errorf("unexpected warnings: %s", rig.errOut)
	}
}

// TestFocusTabFailsWhenTheFocusFails is the other half of astra's finding: a
// focus that ultimately fails must NOT print `attached …` and exit 0.
//
// The distinction the plan draws is between the two halves of step 7. Failing
// to RAISE the window (`app.activate`) is a warning — the tab is focused
// underneath, one click away. Failing to FOCUS is the attach itself failing:
// the user is looking at whatever tab was already selected, and the shell has
// no way to know. So it is exit 1, naming the tab, exactly as never finding
// the key at all is.
func TestFocusTabFailsWhenTheFocusFails(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.shim.reply("rpc.app.sidebar_dump", sidebarDumpDoc(testHostID, "h3.5"))
	rig.shim.fail("tab.focus", `{"error":{"code":"tab_not_found","message":"no tab 5"}}`+"\n", 1)
	rig.shim.reply("rpc.app.activate", "{}\n")

	err := rig.attach.focusTab(context.Background(), testHostLabel, testHostID, "5", "default")
	if err == nil {
		t.Fatal("a focus that never landed must not report success")
	}
	for _, want := range []string{"h3.5", `"default"`, testHostLabel, "the tab is open"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not name %s", err.Error(), want)
		}
	}
	if n := len(rig.shim.runsOf("tab", "focus")); n != 2 {
		t.Errorf("want one retry (two calls), got %d", n)
	}
	// Nothing is raised over a tab that was never focused.
	if n := len(rig.shim.runsOf("rpc", "app.activate")); n != 0 {
		t.Errorf("a failed focus must not go on to raise the window, got %d activates", n)
	}
}

// TestFocusTabDoesNotRetryOtherRefusals: only a TabNotFound is a frame-skew
// retry. Anything else is roost declining, and asking twice would just make it
// decline twice — but it is still a failed attach, not a warning.
func TestFocusTabDoesNotRetryOtherRefusals(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.shim.reply("rpc.app.sidebar_dump", sidebarDumpDoc(testHostID, "h3.5"))
	rig.shim.fail("tab.focus", `{"error":{"code":"invalid_argument","message":"nope"}}`+"\n", 1)
	rig.shim.reply("rpc.app.activate", "{}\n")

	err := rig.attach.focusTab(context.Background(), testHostLabel, testHostID, "5", "default")
	if err == nil {
		t.Fatal("a refused focus is a failed attach")
	}
	if !strings.Contains(err.Error(), "h3.5") {
		t.Errorf("message %q does not name the tab", err.Error())
	}
	if n := len(rig.shim.runsOf("tab", "focus")); n != 1 {
		t.Errorf("want a single attempt, got %d", n)
	}
}

// TestFocusTabWarnsButSucceedsWhenTheWindowStaysBehind is the OTHER side of
// that line: `app.activate` failing leaves a focused tab behind another
// window, which is a warning and a zero exit.
func TestFocusTabWarnsButSucceedsWhenTheWindowStaysBehind(t *testing.T) {
	rig := newRoostRig(t, nil, true)
	rig.shim.reply("rpc.app.sidebar_dump", sidebarDumpDoc(testHostID, "h3.5"))
	rig.shim.reply("tab.focus", "{}\n")
	rig.shim.fail("rpc.app.activate", `{"error":{"code":"unsupported","message":"headless"}}`+"\n", 1)

	if err := rig.attach.focusTab(context.Background(), testHostLabel, testHostID, "5", "default"); err != nil {
		t.Fatalf("a window that stayed behind is a warning, not a failure: %v", err)
	}
	if !strings.Contains(rig.errOut.String(), "warning: roost did not come to the front") {
		t.Errorf("want a warning about the window, got %q", rig.errOut.String())
	}
}

// TestSidebarTabKeyJoin pins the join itself: the numeric suffix of a host
// tab's key is the session-side tab id (live-10-sidebar-key.txt), and the host
// section is matched on the SAVED host id.
func TestSidebarTabKeyJoin(t *testing.T) {
	var dump roostctl.SidebarDump
	if err := json.Unmarshal([]byte(sidebarDumpDoc(testHostID, "h3.5", "h3.51")), &dump); err != nil {
		t.Fatalf("decoding the dump: %v", err)
	}
	cases := []struct {
		name   string
		hostID string
		tabID  string
		want   string
		wantOK bool
	}{
		{name: "the host's own tab", hostID: testHostID, tabID: "5", want: "h3.5", wantOK: true},
		{name: "not a prefix match", hostID: testHostID, tabID: "51", want: "h3.51", wantOK: true},
		{name: "another host's identical suffix is not ours", hostID: "decoy-host", tabID: "5", want: "h9.5", wantOK: true},
		{name: "an unknown host", hostID: "nobody", tabID: "5", wantOK: false},
		{name: "an unknown tab", hostID: testHostID, tabID: "404", wantOK: false},
		{name: "an empty tab id matches nothing", hostID: testHostID, tabID: "", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sidebarTabKey(dump, tc.hostID, tc.tabID)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("sidebarTabKey = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// -----------------------------------------------------------------------
// The whole flow.
// -----------------------------------------------------------------------

// TestAttachRoostEndToEnd runs steps 2-8 together through attachShed: the
// gate, the alias, the saved host, the fence, the tab over the fake ssh, the
// key, the focus, and the line.
//
// The one assertion that is about the FEATURE rather than about a step: the
// terminal never becomes the session. execSSH is wired to fail the test.
func TestAttachRoostEndToEnd(t *testing.T) {
	resetAttachState(t)
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{}}

	rig := newRoostRig(t, nil, true)
	rig.shim.reply("identify", roostVectorResult(t, "identify.response.json"))
	// `host status` is called by Available() BEFORE the flow starts (verb
	// availability is the gate), so the fence's own reads are the entries
	// after it. Staging them in one sequence is what pins that order.
	rig.shim.replySeq("host.status",
		hostStatusDoc(), // Available()'s probe: no saved hosts yet
		hostStatusDoc(statusRow(0, roostctl.StateDisconnected, "")), // the fence's baseline
		hostStatusDoc(statusRow(1, roostctl.StateDisconnected, "")), // attempt started, transport not up
		hostStatusDoc(statusRow(1, roostctl.StateConnected, "")),    // connected
	)
	rig.shim.reply("host.list", hostStatusDoc())
	rig.shim.reply("host.add", hostAddDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.reply("host.connect", hostConnectDoc(testHostID, testHostLabel, testHostLabel))
	rig.shim.replySeq("rpc.app.sidebar_dump",
		sidebarDumpDoc(testHostID),
		sidebarDumpDoc(testHostID, "h3.5"),
	)
	rig.shim.reply("tab.focus", "{}\n")
	rig.shim.reply("rpc.app.activate", "{}\n")

	newRoostAttach = func() *roostAttach { return rig.attach }
	execSSH = func(string, []string, []string) error {
		t.Error("the roost path must never exec ssh -t: the terminal does not become the session")
		return nil
	}

	if err := attachShed("myproj", "mini3", rig.shedEntry(), rig.shedConfig()); err != nil {
		t.Fatalf("attachShed: %v", err)
	}

	got := rig.out.String()
	wantLines := []string{
		"wrote Host shed-myproj to " + rig.sshConfig,
		"attached shed-myproj › default in roost",
	}
	for _, want := range wantLines {
		if !strings.Contains(got, want) {
			t.Errorf("output %q is missing %q", got, want)
		}
	}
	if rig.errOut.Len() != 0 {
		t.Errorf("unexpected warnings: %s", rig.errOut)
	}
	// The tab really was opened on the far side, over the real provider wire.
	if n := rig.shed.countRequests("tab.open"); n != 1 {
		t.Errorf("want exactly one tab.open on the shed, saw %d", n)
	}
	if _, err := os.Stat(rig.sshConfig); err != nil {
		t.Errorf("the ssh alias was not written: %v", err)
	}
}

// TestNotFoundKeepsRoostsOwnRemedy covers the case plan 022 A9 found: roost's
// `NotFound` reason — the one produced when the daemon is absent or off the
// non-interactive PATH — contains NEITHER substring the plan originally pinned,
// so it would have fallen through to the verbatim branch.
//
// It is now recognised, but deliberately NOT replaced with shed's generic
// no-session message: roost's sentence names the non-interactive PATH, which is
// the real cause often enough to be worth keeping (it is exactly what cost an
// hour setting up this plan's own live rig). shed appends only the second door
// that owner decision D4 requires and roost cannot know about.
func TestNotFoundKeepsRoostsOwnRemedy(t *testing.T) {
	// Verbatim from roost-ipc/src/ssh.rs SshFailure::NotFound at the pinned rev.
	const roostNotFound = "roost-session isn't installed on shed-demo (or isn't on the " +
		"non-interactive PATH ssh uses there) — connect from the Roost app to install it."

	got := connectFailureMessage("shed-demo", roostctl.HostStatus{
		State:  roostctl.StateDisconnected,
		Reason: roostNotFound,
	})

	if !strings.HasPrefix(got, roostNotFound) {
		t.Errorf("roost's own diagnosis was not preserved verbatim:\n got: %q\nwant prefix: %q", got, roostNotFound)
	}
	if !strings.Contains(got, "non-interactive PATH") {
		t.Error("the PATH sentence — the most useful part — was dropped")
	}
	if !strings.Contains(got, "shed desktop") {
		t.Error("D4's second door (the shed desktop) is missing")
	}
	// It must NOT have been rewritten into the generic palette message.
	if strings.Contains(got, "Cmd/Alt-Shift-P") {
		t.Errorf("roost's remedy was replaced by shed's generic one:\n%s", got)
	}
}

// TestNoSessionStillGetsShedsPinnedMessage: the OTHER no-session shape, where
// roost's copy points only at `roostctl session start` on the far machine —
// advice a shed user cannot act on directly — is still replaced with shed's
// pinned message naming both apps.
func TestNoSessionStillGetsShedsPinnedMessage(t *testing.T) {
	// Verbatim from roost-ipc/src/ssh.rs SshFailure::NoSession at the pinned rev,
	// and observed live on the rig (live-09-generation-fence.txt).
	const roostNoSession = "shed-demo is reachable but has no roost session running. " +
		"Run `roostctl session start` on that machine, then try again."

	got := connectFailureMessage("shed-demo", roostctl.HostStatus{
		State:  roostctl.StateDisconnected,
		Reason: roostNoSession,
	})

	for _, want := range []string{"has no roost-session", "Connect Host: shed-demo", "shed desktop"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestEnsureTabLocksTheTitle: a tab opened with a title comes up
// `user_titled: false`, and the login shell inside it immediately renames it to
// its cwd via OSC. Measured live: a tab opened as `default` read back as
// `/home/shed`, so reuse-by-title never matched and a second `shed attach`
// opened a third tab. The flow therefore follows `tab.open` with an explicit
// `tab.set_title`, which marks the tab `user_titled: true` and locks it.
func TestEnsureTabLocksTheTitle(t *testing.T) {
	// No existing tabs, so the flow must open one — and then title it.
	rig := newReuseRig(t, func(landing, home string) []roostprovider.Tab { return nil })

	tabID, err := rig.attach.ensureTab(context.Background(), "myproj", rig.shedEntry(), rig.shedConfig(), "default", false)
	if err != nil {
		t.Fatalf("ensureTab: %v", err)
	}

	lines := rig.shed.requestLines()
	openAt, titleAt := -1, -1
	for i, line := range lines {
		switch {
		case strings.Contains(line, `"op":"tab.open"`):
			openAt = i
		case strings.Contains(line, `"op":"tab.set_title"`):
			titleAt = i
		}
	}
	if openAt == -1 {
		t.Fatalf("no tab.open in %q", lines)
	}
	if titleAt == -1 {
		t.Fatalf("the tab was opened but never titled — a later attach cannot find it: %q", lines)
	}
	if titleAt < openAt {
		t.Errorf("tab.set_title came before tab.open: %q", lines)
	}
	// It must title the tab it just opened, with the requested title.
	req := rig.shed.requestFor("tab.set_title")
	want := `"tab_id":"` + tabID + `"`
	if !strings.Contains(req, want) {
		t.Errorf("tab.set_title did not name the tab just opened (%s): %s", tabID, req)
	}
	if !strings.Contains(req, `"title":"default"`) {
		t.Errorf("tab.set_title did not carry the requested title: %s", req)
	}
}
