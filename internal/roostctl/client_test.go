package roostctl

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestArgvForEveryVerb is this package's primary assertion: the exact argument
// vector each method hands the real binary.
//
// Position-exact, every element, for every verb. A test that only checked the
// decoded result would pass with `--id` spelled `--host`, with `--json`
// dropped (which turns every refusal from a readable envelope into a prose
// line this package cannot parse), or with a flag attached to the wrong
// subcommand — the shim would answer identically in all three cases, and the
// real roostctl would not.
func TestArgvForEveryVerb(t *testing.T) {
	tests := []struct {
		name string
		// staged is the shim verb key whose reply is laid down.
		staged string
		// stdout is what the shim prints for it.
		stdout string
		call   func(t *testing.T, c *Client) error
		want   []string
	}{
		{
			name:   "identify",
			staged: "identify",
			stdout: `{"socket_path":"/tmp/roost.sock","pid":1,"ui_version":"0.7.0","protocol_version":1}`,
			call: func(t *testing.T, c *Client) error {
				_, err := c.Identify(testContext(t))
				return err
			},
			want: []string{"identify", "--json"},
		},
		{
			name:   "host list",
			staged: "host.list",
			stdout: `{"hosts":[]}`,
			call: func(t *testing.T, c *Client) error {
				_, err := c.HostList(testContext(t))
				return err
			},
			want: []string{"host", "list", "--json"},
		},
		{
			name:   "host add",
			staged: "host.add",
			stdout: `{"host":{"id":"hs-1","label":"shed-demo","target":"shed-demo"}}`,
			call: func(t *testing.T, c *Client) error {
				_, err := c.HostAdd(testContext(t), "shed-demo", "shed-demo", false)
				return err
			},
			want: []string{"host", "add", "--label", "shed-demo", "--target", "shed-demo", "--json"},
		},
		{
			// `--verify` sits between the target and `--json`, and it is a
			// bare flag: roost's own arg takes no value.
			name:   "host add with verify",
			staged: "host.add",
			stdout: `{"host":{"id":"hs-1","label":"shed-demo","target":"shed-demo"}}`,
			call: func(t *testing.T, c *Client) error {
				_, err := c.HostAdd(testContext(t), "shed-demo", "shed-demo", true)
				return err
			},
			want: []string{"host", "add", "--label", "shed-demo", "--target", "shed-demo", "--verify", "--json"},
		},
		{
			name:   "host connect",
			staged: "host.connect",
			stdout: `{"host":{"id":"hs-1","label":"shed-demo","target":"shed-demo"},"state":"connecting"}`,
			call: func(t *testing.T, c *Client) error {
				_, err := c.HostConnect(testContext(t), "hs-1")
				return err
			},
			want: []string{"host", "connect", "--id", "hs-1", "--json"},
		},
		{
			name:   "host status for one host",
			staged: "host.status",
			stdout: `{"hosts":[]}`,
			call: func(t *testing.T, c *Client) error {
				_, err := c.HostStatus(testContext(t), "hs-1")
				return err
			},
			want: []string{"host", "status", "--id", "hs-1", "--json"},
		},
		{
			// An empty id means every host, and `--id` must not appear at all
			// — roost's param is an Option and an empty string is not one.
			name:   "host status for every host",
			staged: "host.status",
			stdout: `{"hosts":[]}`,
			call: func(t *testing.T, c *Client) error {
				_, err := c.HostStatus(testContext(t), "")
				return err
			},
			want: []string{"host", "status", "--json"},
		},
		{
			name:   "tab focus on a host tab",
			staged: "tab.focus",
			stdout: `{}`,
			call: func(t *testing.T, c *Client) error {
				return c.TabFocus(testContext(t), "h3.9")
			},
			want: []string{"tab", "focus", "--tab", "h3.9", "--json"},
		},
		{
			// `rpc` takes the op name as a bare positional, and no params
			// argument at all — roost reads an omitted one as `{}`.
			name:   "sidebar dump",
			staged: "rpc.app.sidebar_dump",
			stdout: `{"agents_visible":true}`,
			call: func(t *testing.T, c *Client) error {
				_, err := c.SidebarDump(testContext(t))
				return err
			},
			want: []string{"rpc", "app.sidebar_dump", "--json"},
		},
		{
			name:   "activate",
			staged: "rpc.app.activate",
			stdout: `{}`,
			call: func(t *testing.T, c *Client) error {
				return c.Activate(testContext(t))
			},
			want: []string{"rpc", "app.activate", "--json"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newShim(t)
			s.reply(tc.staged, tc.stdout)
			if err := tc.call(t, &Client{}); err != nil {
				t.Fatalf("call: %v", err)
			}
			assertArgv(t, s.onlyRun(), tc.want)
		})
	}
}

// TestIdentifyDecodesRoostsOwnVector reads roost's published `identify` reply
// rather than a shape remembered off a doc page.
func TestIdentifyDecodesRoostsOwnVector(t *testing.T) {
	s := newShim(t)
	s.reply("identify", vectorResult(t, "identify.response.json"))

	got, err := (&Client{}).Identify(testContext(t))
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	want := Identify{
		SocketPath:      "/Users/me/Library/Caches/Roost/roost.sock",
		PID:             12345,
		UIVersion:       "0.7.0",
		ProtocolVersion: 1,
	}
	if got != want {
		t.Errorf("identify = %+v, want %+v", got, want)
	}
}

// TestSidebarDumpDecodesRoostsOwnVector is the one that matters for the
// consumer C5b will build: the `h<incarnation>.<id>` tab key, off roost's own
// published dump.
func TestSidebarDumpDecodesRoostsOwnVector(t *testing.T) {
	s := newShim(t)
	s.reply("rpc.app.sidebar_dump", vectorResult(t, "app.sidebar_dump.response.json"))

	dump, err := (&Client{}).SidebarDump(testContext(t))
	if err != nil {
		t.Fatalf("SidebarDump: %v", err)
	}
	if !dump.AgentsVisible {
		t.Error("agents_visible = false")
	}
	if len(dump.Hosts) != 2 {
		t.Fatalf("hosts = %+v, want two", dump.Hosts)
	}

	connected := dump.Hosts[0]
	if connected.ID != "hs-2f1c" || connected.Label != "workbench" || connected.State != StateConnected {
		t.Errorf("host = %+v", connected)
	}
	if len(connected.Projects) != 1 {
		t.Fatalf("projects = %+v", connected.Projects)
	}
	project := connected.Projects[0]
	if project.Key != "h3.4" || project.Name != "roost" {
		t.Errorf("project = %+v", project)
	}
	want := []SidebarHostTab{{Key: "h3.9", Title: "zsh"}, {Key: "h3.7", Title: "claude"}}
	if len(project.Tabs) != len(want) {
		t.Fatalf("tabs = %+v, want %+v", project.Tabs, want)
	}
	for i := range want {
		if project.Tabs[i] != want[i] {
			t.Errorf("tab %d = %+v, want %+v", i, project.Tabs[i], want[i])
		}
	}
	// Every tab key the dump reported must be a reference `tab focus` accepts
	// — that is the only reason this op is read.
	for _, tab := range project.Tabs {
		if !tabRefPattern.MatchString(tab.Key) {
			t.Errorf("tab key %q is not a focusable reference", tab.Key)
		}
	}

	// A host that has never connected has no projects, and that is a state,
	// not a gap.
	if dump.Hosts[1].State != StateDisconnected || len(dump.Hosts[1].Projects) != 0 {
		t.Errorf("disconnected host = %+v", dump.Hosts[1])
	}

	// The local half decodes too, agent rows and all.
	if len(dump.Projects) != 2 || len(dump.Projects[0].Agents) != 1 {
		t.Fatalf("local projects = %+v", dump.Projects)
	}
	agent := dump.Projects[0].Agents[0]
	if agent.TabID != "7" || agent.Name != "slauth-refactor" || agent.Lifecycle != "waiting" {
		t.Errorf("agent row = %+v", agent)
	}
}

// TestHostStatusDecodesNullOptionFields covers the two shapes roost's own
// vectors do not carry: an Option field sent as an explicit `null` rather than
// omitted, and a `detail` present at all. Everything else about the row —
// state, generation, tabs, last_connected, the registry half — is pinned
// against roost's published replies further down this file rather than against
// a literal invented here.
//
// **There is no "no session" state.** A shed whose far side has no
// roost-session running comes back `disconnected` with that sentence in
// `reason` — asserted here, because a reader switching on `state` alone would
// never see it and would call the host merely offline.
func TestHostStatusDecodesNullOptionFields(t *testing.T) {
	s := newShim(t)
	s.reply("host.status", `{"hosts":[
		{"id":"hs-2f1c","label":"shed-demo","target":"shed-demo",
		 "last_connected":"2026-09-01T17:40:02Z","generation":3,"state":"connected",
		 "reason":null,"detail":null,"rollup":"3 agents","tabs":5},
		{"id":"hs-9d40","label":"laptop","target":"localhost","last_connected":null,
		 "generation":1,"state":"disconnected",
		 "reason":"cannot find roost-session","detail":"tried /usr/bin/roost-session","tabs":0}
	]}`)

	hosts, err := (&Client{}).HostStatus(testContext(t), "")
	if err != nil {
		t.Fatalf("HostStatus: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %+v", hosts)
	}

	// `null` is not an error and it is not a literal "null".
	if live := hosts[0]; live.Reason != "" || live.Detail != "" {
		t.Errorf("a null reason/detail decoded to %q/%q", live.Reason, live.Detail)
	}

	down := hosts[1]
	if down.Reason != "cannot find roost-session" {
		t.Errorf("reason = %q — the no-session case arrives here, not as a state", down.Reason)
	}
	if down.Detail != "tried /usr/bin/roost-session" {
		t.Errorf("detail = %q", down.Detail)
	}
}

// TestFailureEnvelopeBecomesAServerError: roost's own refusal, with its own
// stable code, is the error a caller branches on.
func TestFailureEnvelopeBecomesAServerError(t *testing.T) {
	s := newShim(t)
	s.fail("host.connect", `{"error":{"code":"not-found","message":"no host hs-nope"}}`+"\n", 1)

	_, err := (&Client{}).HostConnect(testContext(t), "hs-nope")
	var refusal *ServerError
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %v (%T), want a *ServerError", err, err)
	}
	if refusal.Code != "not-found" || refusal.Message != "no host hs-nope" {
		t.Errorf("refusal = %+v", refusal)
	}
	if refusal.ExitCode != 1 {
		t.Errorf("exit code = %d", refusal.ExitCode)
	}
}

// TestFailureEnvelopeBehindOtherStderr: something else reaching the same
// stream — a linker warning, a panic line — must not turn a readable refusal
// into an opaque exit.
func TestFailureEnvelopeBehindOtherStderr(t *testing.T) {
	s := newShim(t)
	s.fail("host.status",
		"warning: something else wrote here\n"+
			`{"error":{"code":"busy","message":"the UI is busy"}}`+"\n", 1)

	_, err := (&Client{}).HostStatus(testContext(t), "")
	var refusal *ServerError
	if !errors.As(err, &refusal) {
		t.Fatalf("error = %v (%T), want a *ServerError", err, err)
	}
	if refusal.Code != "busy" {
		t.Errorf("code = %q", refusal.Code)
	}
}

// TestNonZeroExitWithoutAnEnvelopeIsAnExitError: roostctl failed and did not
// say how. Distinct from a refusal on purpose — there is no code to branch on,
// so the stderr tail is the whole diagnosis.
func TestNonZeroExitWithoutAnEnvelopeIsAnExitError(t *testing.T) {
	s := newShim(t)
	s.fail("identify", "roostctl: no-target: no Roost is listening\n", 1)

	_, err := (&Client{}).Identify(testContext(t))
	var exited *ExitError
	if !errors.As(err, &exited) {
		t.Fatalf("error = %v (%T), want an *ExitError", err, err)
	}
	if exited.ExitCode != 1 {
		t.Errorf("exit code = %d", exited.ExitCode)
	}
	var refusal *ServerError
	if errors.As(err, &refusal) {
		t.Error("a bare non-zero exit was reported as roost's own refusal")
	}
	if exited.Stderr == "" {
		t.Error("the stderr tail — the only diagnosis there is — was dropped")
	}
}

// TestUnparseableOutputIsADecodeError: roostctl ran, exited 0, and printed
// something this package cannot read. That is a skew or a bug, never a
// decision — so it must not look like a refusal.
func TestUnparseableOutputIsADecodeError(t *testing.T) {
	s := newShim(t)
	s.reply("identify", "not json at all\n")

	_, err := (&Client{}).Identify(testContext(t))
	var decode *DecodeError
	if !errors.As(err, &decode) {
		t.Fatalf("error = %v (%T), want a *DecodeError", err, err)
	}
	var refusal *ServerError
	var exited *ExitError
	if errors.As(err, &refusal) || errors.As(err, &exited) {
		t.Error("unreadable output was reported as a refusal or a failed exit")
	}
}

// TestATimeoutIsAnExecError proves the package's OWN ceiling fires: a shim
// that never answers is killed, and the failure names the timeout rather than
// a signal death.
//
// ExecTimeout is shortened for the run — see its doc comment. Without that
// this test would take ten seconds, which is a test nobody runs.
func TestATimeoutIsAnExecError(t *testing.T) {
	original := ExecTimeout
	ExecTimeout = 250 * time.Millisecond
	t.Cleanup(func() { ExecTimeout = original })

	s := newShim(t)
	s.reply("identify", `{"socket_path":"/tmp/roost.sock"}`)
	s.sleepFor("30")

	started := time.Now()
	// A generous parent, so the deadline that fires is demonstrably this
	// package's and not the caller's.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := (&Client{}).Identify(ctx)
	elapsed := time.Since(started)

	var execErr *ExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("error = %v (%T), want an *ExecError", err, err)
	}
	if !execErr.TimedOut {
		t.Errorf("the timeout was not reported as one: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false: %v", err)
	}
	// Generously above ExecTimeout+waitDelay and far below the shim's own
	// 30-second sleep, so this fails on a call that waited for the child
	// rather than on a slow machine.
	if elapsed > 5*time.Second {
		t.Errorf("the call took %s — the ceiling did not fire", elapsed)
	}
}

// TestAMissingBinaryIsAnExecError: no roostctl on PATH at all.
func TestAMissingBinaryIsAnExecError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := (&Client{}).Identify(testContext(t))
	var execErr *ExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("error = %v (%T), want an *ExecError", err, err)
	}
	if execErr.TimedOut {
		t.Error("a missing binary was reported as a timeout")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("errors.Is(err, exec.ErrNotFound) = false: %v", err)
	}
}

// TestTabFocusRefusesABadReferenceWithoutExecing: a malformed reference is
// this side's bug, and it is caught before anything runs — so it arrives as
// its own type rather than as a usage exit indistinguishable from every other
// argv mistake.
func TestTabFocusRefusesABadReferenceWithoutExecing(t *testing.T) {
	s := newShim(t)
	s.reply("tab.focus", `{}`)

	err := (&Client{}).TabFocus(testContext(t), "h3.9 ; rm -rf /")
	var bad *InvalidTabRefError
	if !errors.As(err, &bad) {
		t.Fatalf("error = %v (%T), want an *InvalidTabRefError", err, err)
	}
	if runs := s.argvRuns(); len(runs) != 0 {
		t.Errorf("roostctl ran anyway: %v", runs)
	}
}

// TestTabRefPattern pins the reference grammar against roost's own spelling.
func TestTabRefPattern(t *testing.T) {
	for _, ref := range []string{"0", "5", "-5", "h3.9", "h3.0", "h12.-4"} {
		if !tabRefPattern.MatchString(ref) {
			t.Errorf("%q should be a valid tab reference", ref)
		}
	}
	for _, ref := range []string{"", "h0.1", "h3", "3.9", "05", "h3.05", "h-1.2", "abc", "h3.9 "} {
		if tabRefPattern.MatchString(ref) {
			t.Errorf("%q should not be a valid tab reference", ref)
		}
	}
}

// TestAvailable is the gate, and it is total: every way it can come out
// negative means the same thing to the caller, and none of them is an error
// the caller has to interpret.
func TestAvailable(t *testing.T) {
	const goodIdentify = `{"socket_path":"/tmp/roost.sock","pid":1,"ui_version":"0.7.0","protocol_version":1}`

	t.Run("a running app answering both calls", func(t *testing.T) {
		s := newShim(t)
		s.reply("identify", goodIdentify)
		s.reply("host.status", `{"hosts":[]}`)

		if !(&Client{}).Available(testContext(t)) {
			t.Fatal("Available = false")
		}
		// Both calls really happened, in order — the second is what proves
		// the app serves the host family, and skipping it would make the gate
		// pass against a roost this package cannot drive.
		runs := s.argvRuns()
		if len(runs) != 2 {
			t.Fatalf("roostctl ran %d times: %v", len(runs), runs)
		}
		assertArgv(t, runs[0], []string{"identify", "--json"})
		assertArgv(t, runs[1], []string{"host", "status", "--json"})
	})

	t.Run("no roostctl on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if (&Client{}).Available(testContext(t)) {
			t.Fatal("Available = true with no binary")
		}
	})

	t.Run("no app running", func(t *testing.T) {
		s := newShim(t)
		s.fail("identify", `{"error":{"code":"no-target","message":"no Roost is listening"}}`+"\n", 1)

		if (&Client{}).Available(testContext(t)) {
			t.Fatal("Available = true with nothing listening")
		}
		if len(s.argvRuns()) != 1 {
			t.Error("the gate kept going after identify failed")
		}
	})

	t.Run("unreadable identify output", func(t *testing.T) {
		s := newShim(t)
		s.reply("identify", "roost is fine, honest\n")

		if (&Client{}).Available(testContext(t)) {
			t.Fatal("Available = true on output that does not parse")
		}
	})

	t.Run("an identify that parses but says nothing", func(t *testing.T) {
		s := newShim(t)
		// `{}` decodes cleanly into every field's zero value. It is not an
		// identify document, and treating it as one is how a gate passes
		// against something that is not roost at all.
		s.reply("identify", `{}`)
		s.reply("host.status", `{"hosts":[]}`)

		if (&Client{}).Available(testContext(t)) {
			t.Fatal("Available = true on an empty identify result")
		}
	})

	t.Run("an app that refuses host status", func(t *testing.T) {
		s := newShim(t)
		s.reply("identify", goodIdentify)
		s.fail("host.status", `{"error":{"code":"unsupported","message":"no such op"}}`+"\n", 1)

		if (&Client{}).Available(testContext(t)) {
			t.Fatal("Available = true against an app with no host family")
		}
	})

	t.Run("a wedged app", func(t *testing.T) {
		original := ExecTimeout
		ExecTimeout = 250 * time.Millisecond
		t.Cleanup(func() { ExecTimeout = original })

		s := newShim(t)
		s.reply("identify", goodIdentify)
		s.sleepFor("30")

		started := time.Now()
		if (&Client{}).Available(testContext(t)) {
			t.Fatal("Available = true against an app that never answers")
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("the gate took %s — a hang wedged it", elapsed)
		}
	})
}

// TestTheRunSeamIsHonoured: a caller can replace the exec entirely, which is
// the seam a consumer's own tests will use rather than installing a shim.
func TestTheRunSeamIsHonoured(t *testing.T) {
	var sawBin string
	var sawArgs []string
	c := &Client{
		Bin: "/nowhere/roostctl",
		Run: func(_ context.Context, bin string, args []string) (Result, error) {
			sawBin, sawArgs = bin, args
			return Result{Stdout: []byte(`{"hosts":[]}`)}, nil
		},
	}
	if _, err := c.HostList(testContext(t)); err != nil {
		t.Fatalf("HostList: %v", err)
	}
	if sawBin != "/nowhere/roostctl" {
		t.Errorf("bin = %q", sawBin)
	}
	assertArgv(t, sawArgs, []string{"host", "list", "--json"})
}

// TestTheShimRefusesAnUnstagedVerb keeps the rig honest: a call that reached
// roostctl under a verb no test staged must fail loudly rather than pick up
// another verb's reply and pass.
func TestTheShimRefusesAnUnstagedVerb(t *testing.T) {
	s := newShim(t)
	s.reply("identify", `{"socket_path":"/tmp/roost.sock"}`)

	err := (&Client{}).Activate(testContext(t))
	var exited *ExitError
	if !errors.As(err, &exited) || exited.ExitCode != 97 {
		t.Fatalf("error = %v (%T), want the shim's unstaged-verb exit", err, err)
	}
	if _, statErr := os.Stat(s.argvLog); statErr != nil {
		t.Errorf("the rig logged no argv: %v", statErr)
	}
}

// The tests below pin the host decode against roost's OWN published vectors
// rather than literals this repo invented. That matters more here than
// anywhere else in this package: the attach flow's generation fence reads
// `generation` and `state` off exactly these replies to decide whether a
// connect has landed, so a decode that is subtly wrong would make the fence
// wait forever or, worse, proceed against a host that is not connected.

// TestHostStatusDecodesRoostsOwnVector covers the two shapes a NOT-connected
// host arrives in, both present in roost's vector: one mid-retry with a reason,
// and one that has never connected at all.
func TestHostStatusDecodesRoostsOwnVector(t *testing.T) {
	s := newShim(t)
	s.reply("host.status", vectorResult(t, "host.status.response.json"))

	hosts, err := (&Client{}).HostStatus(testContext(t), "")
	if err != nil {
		t.Fatalf("HostStatus: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %d, want 2: %+v", len(hosts), hosts)
	}

	retrying := hosts[0]
	if retrying.State != StateDisconnected {
		t.Errorf("state = %q, want %q", retrying.State, StateDisconnected)
	}
	if retrying.Generation != 3 {
		t.Errorf("generation = %d, want 3 — the fence's baseline comes from here", retrying.Generation)
	}
	if retrying.Reason != "reconnecting in 8s (3/10)" {
		t.Errorf("reason = %q", retrying.Reason)
	}

	// A host that has never connected: generation 0 is LEGITIMATE, not a
	// missing field. The fence must treat it as a real baseline and wait for
	// something greater, not mistake it for "no data".
	fresh := hosts[1]
	if fresh.Generation != 0 {
		t.Errorf("generation = %d, want 0", fresh.Generation)
	}
	if fresh.State != StateDisconnected {
		t.Errorf("state = %q, want %q", fresh.State, StateDisconnected)
	}
	if fresh.Reason != "" {
		t.Errorf("a host with no reason decoded reason = %q", fresh.Reason)
	}
	if fresh.LastConnected != "" {
		t.Errorf("a never-connected host decoded last_connected = %q", fresh.LastConnected)
	}
}

// TestHostStatusConnectedDecodesRoostsOwnVector pins the state the generation
// fence is waiting FOR, from roost's own vector — including the fields shed
// deliberately does not model (`connect`, `payload_kind`, `rollup`), which
// must not break the decode.
func TestHostStatusConnectedDecodesRoostsOwnVector(t *testing.T) {
	s := newShim(t)
	s.reply("host.status", vectorResult(t, "host.status.connect.response.json"))

	hosts, err := (&Client{}).HostStatus(testContext(t), "a1b2c3d4e5f60718")
	if err != nil {
		t.Fatalf("HostStatus: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("hosts = %d, want 1", len(hosts))
	}
	h := hosts[0]
	if h.State != StateConnected {
		t.Errorf("state = %q, want %q — this is the fence's success condition", h.State, StateConnected)
	}
	if h.Generation != 5 {
		t.Errorf("generation = %d, want 5", h.Generation)
	}
	if h.Tabs != 5 {
		t.Errorf("tabs = %d, want 5", h.Tabs)
	}
	if h.Reason != "" {
		t.Errorf("a connected host decoded reason = %q", h.Reason)
	}
}

// TestHostListDecodesRoostsOwnVector pins the documented difference between
// `host list` and `host status`: list is the saved-host REGISTRY alone. If a
// caller reached for state or generation here it would silently read zero
// values, so this asserts the registry fields arrive and nothing more is
// implied.
func TestHostListDecodesRoostsOwnVector(t *testing.T) {
	s := newShim(t)
	s.reply("host.list", vectorResult(t, "host.list.response.json"))

	hosts, err := (&Client{}).HostList(testContext(t))
	if err != nil {
		t.Fatalf("HostList: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("hosts = %d, want 2", len(hosts))
	}
	if hosts[0].ID != "3f9a2b7c1d4e4f5a" || hosts[0].Label != "pop-os" {
		t.Errorf("registry row = %+v", hosts[0])
	}
	// The target grammar is anything ssh(1) accepts, which includes a socket
	// path — shed's own aliases are the `user@host` form the second row uses.
	if hosts[1].Target != "test1@localhost" {
		t.Errorf("target = %q", hosts[1].Target)
	}
	if hosts[1].LastConnected != "" {
		t.Errorf("a null last_connected decoded to %q", hosts[1].LastConnected)
	}
}

// TestHostAddAndConnectDecodeRoostsOwnVectors pins the two envelopes the
// registry verbs answer with — `{"host":{…}}` and `{"host":…,"state":…}` —
// against roost's published replies.
//
// The argv table above calls both verbs and DISCARDS what they return, so
// without this an envelope key spelled wrong would decode to a zero Host and
// pass every other test in this file. It also pins StateConnecting against the
// wire spelling of the state a fresh connect lands in.
func TestHostAddAndConnectDecodeRoostsOwnVectors(t *testing.T) {
	const target = "/home/charlie/.local/state/roost/roost-session.sock"
	s := newShim(t)
	s.reply("host.add", vectorResult(t, "host.add.response.json"))
	s.reply("host.connect", vectorResult(t, "host.connect.response.json"))
	c := &Client{}

	added, err := c.HostAdd(testContext(t), "pop-os", target, true)
	if err != nil {
		t.Fatalf("HostAdd: %v", err)
	}
	// A host saved and never connected: `last_connected` is `null`, which is
	// an empty string here and not a missing field.
	want := Host{ID: "3f9a2b7c1d4e4f5a", Label: "pop-os", Target: target}
	if added != want {
		t.Errorf("added host = %+v, want %+v", added, want)
	}

	// `host connect` answers with the same registry row plus the state the
	// attempt is in — `connecting`, because connect returns once the attempt
	// is under way rather than once it has settled.
	conn, err := c.HostConnect(testContext(t), added.ID)
	if err != nil {
		t.Fatalf("HostConnect: %v", err)
	}
	if conn.State != StateConnecting {
		t.Errorf("state = %q, want %q", conn.State, StateConnecting)
	}
	if conn.Host.ID != want.ID || conn.Host.LastConnected != "2026-08-29T17:04:11Z" {
		t.Errorf("connected host = %+v", conn.Host)
	}
}
