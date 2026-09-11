package roostprovider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// End-to-end over the fake ssh (see fakessh_test.go): the real remote command
// strings, a real login shell, a real NDJSON dialogue, roost's own bytes.

func e2eContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestProbeThroughFakeSSH is the whole of §3.2 step 2's first half against a
// far side that really has to be discovered: the $HOME comes back from the
// remote shell, and the agent paths come back from a login-PATH lookup rather
// than from anything this side arranged.
func TestProbeThroughFakeSSH(t *testing.T) {
	r := newRig(t, rigOpts{
		agents:  []string{"claude", "opencode", "gx"},
		landing: "proj",
	})

	got, err := r.remote.Probe(e2eContext(t), r.target(), filepath.Join(r.home, "proj"))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	// A $HOME with a space survives the remote shell, the NUL framing and the
	// parser.
	if got.Home != r.home {
		t.Errorf("home = %q, want %q", got.Home, r.home)
	}
	if !strings.Contains(got.Home, " ") {
		t.Fatalf("the rig's home lost its space: %q", got.Home)
	}
	if !got.LandingDirExists {
		t.Errorf("the landing dir exists but the probe did not find it")
	}

	// Keyed by KIND; `cursor-agent` is a binary and `cursor` is a kind.
	want := map[string]string{
		"claude-rc": filepath.Join(r.home, ".local", "bin", "claude"),
		"opencode":  filepath.Join(r.home, ".local", "bin", "opencode"),
		"gx":        filepath.Join(r.home, ".local", "bin", "gx"),
	}
	if len(got.Found) != len(want) {
		t.Fatalf("found = %v, want %v", got.Found, want)
	}
	for kind, path := range want {
		if got.Found[kind] != path {
			t.Errorf("%s = %q, want %q", kind, got.Found[kind], path)
		}
	}
}

func TestProbeWithNoAgentsAndNoLandingDir(t *testing.T) {
	r := newRig(t, rigOpts{})
	got, err := r.remote.Probe(e2eContext(t), r.target(), "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(got.Found) != 0 {
		t.Errorf("found = %v", got.Found)
	}
	if got.LandingDirExists {
		t.Errorf("no landing dir was passed but the probe reported one")
	}

	// And the menu that falls out of it.
	host := Token{Machine: "mini2"}
	m := AgentMenu(host, got)
	assertNoneRow(t, m.Items[0], "no agents found on mini2",
		"looked for claude, codex, cursor-agent, opencode, gx, grok under bash -lc")
}

// TestProbeDoesNotOfferANonExecutableAgent: `command -v` under a login shell is
// the SAME lookup the tab's `bash -lc 'exec "$@"'` will do, and a file without
// an execute bit is not on it. Offering one would put a dead tab in the palette.
func TestProbeDoesNotOfferANonExecutableAgent(t *testing.T) {
	r := newRig(t, rigOpts{agents: []string{"codex"}})
	if err := os.Chmod(filepath.Join(r.home, ".local", "bin", "codex"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := r.remote.Probe(e2eContext(t), r.target(), "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if _, ok := got.Found["codex"]; ok {
		t.Errorf("a non-executable file was offered as an agent")
	}
}

// TestProbeDropsAMissingLandingDir: the landing dir the SERVER reported may not
// exist on the far side (a stale record, a mount that did not come up). A tab
// opened in a missing cwd dies at the PTY.
func TestProbeDropsAMissingLandingDir(t *testing.T) {
	r := newRig(t, rigOpts{})
	got, err := r.remote.Probe(e2eContext(t), r.target(), filepath.Join(r.home, "gone"))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got.LandingDirExists {
		t.Errorf("a missing landing dir reported as existing")
	}
	candidates := WorkdirCandidates(nil, got.Home, filepath.Join(r.home, "gone"), got.LandingDirExists)
	assertSeq(t, "candidates", candidates, []Workdir{{Title: "Home", Cwd: r.home}})
}

// TestBridgeOpsThroughFakeSSH drives all three ops over roost's real exec chain
// and asserts the EXACT request line each one put on the wire — the assertion
// that catches a numeric id, a null params, or an int project_id.
func TestBridgeOpsThroughFakeSSH(t *testing.T) {
	r := newRig(t, rigOpts{
		projects: []Project{
			{ID: "1", Name: "roost", Cwd: "/home/shed/roost"},
			{ID: "2", Name: "shed", Cwd: "/home/shed/shed"},
		},
	})
	ctx := e2eContext(t)
	target := r.target()

	identify, err := r.remote.Identify(ctx, target)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if identify.SessionProtocol != SpokenProtocol {
		t.Errorf("session_protocol = %d", identify.SessionProtocol)
	}

	projects, err := r.remote.TabList(ctx, target)
	if err != nil {
		t.Fatalf("TabList: %v", err)
	}
	if len(projects) != 2 || projects[0].Name != "roost" {
		t.Fatalf("projects = %+v", projects)
	}

	params, err := TabOpenFor(Token{
		Machine: "mini2", Agent: "cursor", Home: "/home/shed", Cwd: "/home/shed/roost", Project: "1",
	})
	if err != nil {
		t.Fatalf("TabOpenFor: %v", err)
	}
	tabID, err := r.remote.TabOpen(ctx, target, params)
	if err != nil {
		t.Fatalf("TabOpen: %v", err)
	}
	if tabID != "5" {
		t.Errorf("tab id = %q", tabID)
	}

	want := []string{
		`{"id":"1","op":"session.identify","params":{}}`,
		`{"id":"1","op":"tab.list","params":{}}`,
		`{"id":"1","op":"tab.open","params":{"project_id":"1","cwd":"/home/shed/roost","title":"cursor",` +
			`"argv":["bash","-lc","exec \"$@\"","shed","cursor-agent"]}}`,
	}
	assertSeq(t, "request lines", r.requestLines(), want)
}

// TestBridgeSkipsEventFramesOnTheWire: the rig's bridge pushes an unsolicited
// `tab.opened` before EVERY answer, so this is really asserted by every call in
// this file — this test just names the property and proves the frame is
// actually there to be skipped.
func TestBridgeSkipsEventFramesOnTheWire(t *testing.T) {
	r := newRig(t, rigOpts{})
	if _, err := r.remote.Identify(e2eContext(t), r.target()); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	// Prove the fake really did emit an event frame ahead of the reply — a rig
	// that silently stopped doing so would make this whole file's coverage of
	// the skip evaporate.
	event, err := os.ReadFile(filepath.Join(filepath.Dir(r.requests), "event.ndjson"))
	if err != nil {
		t.Fatalf("reading the event frame: %v", err)
	}
	if !strings.Contains(string(event), `"event"`) {
		t.Fatalf("the rig's event frame is not an event: %s", event)
	}
}

// TestArgvReachesTheFarSide checks the argv the transport really handed ssh —
// the pure Argv tests say what it should be, this one says it is what ran.
//
// **Position-exact, not "these strings appear somewhere".** Order is the whole
// safety property on an ssh command line: ssh parses options up to the first
// non-option word, so "the destination and the `--` are both in argv" is true
// of a layout in which the destination is parsed as an option. A containment
// check passes for every permutation; this one passes for exactly one.
func TestArgvReachesTheFarSide(t *testing.T) {
	r := newRig(t, rigOpts{})
	if _, err := r.remote.Probe(e2eContext(t), r.target(), ""); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	runs := r.argvRuns()
	if len(runs) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	// argv[0] is the script's own name, dropped by the shell; the log starts
	// at the first real argument. The rig's target is unpinned and on 22, so
	// there are no host-key options and no `-p`.
	assertSeq(t, "argv", runs[0], []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		"-T",
		"fake-host",
		"--", ProbeCommand(""),
	})
}

// TestFakeSSHParsesArgvLikeOpenSSH is what makes the option-shaped destination
// guard testable at all: it proves the rig's ssh really does parse argv the way
// ssh does, so a test that drives an option-shaped destination through it is
// observing ssh's behaviour rather than a fake's convenience.
//
// The layout under test is the one this package emits — `<options> <dest> --
// <cmd>`. With an option-shaped destination, ssh eats the destination as an
// option, the `--` ends option parsing, and the REMOTE COMMAND becomes the
// destination. Verified against the real ssh on the development machine:
// `ssh -G -oPort=7777 -- echo hi` reports `port 7777` and `hostname echo`.
func TestFakeSSHParsesArgvLikeOpenSSH(t *testing.T) {
	r := newRig(t, rigOpts{})
	// A marker the remote command would create if it ran. It must not.
	marker := filepath.Join(filepath.Dir(r.requests), "the-remote-command-ran")

	t.Run("an option-shaped destination is eaten as an option", func(t *testing.T) {
		// Hand-built argv in the provider's own layout, bypassing Argv —
		// which is exactly what Argv now refuses to build.
		out, err := exec.Command(r.remote.SSHBin, //nolint:gosec // a test script path this test just wrote
			"-o", "BatchMode=yes",
			"-o", "ConnectTimeout=5",
			"-T",
			"-oProxyCommand=whatever",
			"--", "touch "+shellQuote(marker),
		).CombinedOutput()
		if err == nil {
			t.Fatalf("the fake ssh accepted an option-shaped destination: %s", out)
		}
		// The remote command was consumed as the DESTINATION, so the fake ran
		// nothing — which is the mis-parse, reproduced.
		if !strings.Contains(string(out), "no remote command for destination touch ") {
			t.Fatalf("the fake did not mis-parse the way ssh does: %s", out)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("the remote command ran despite being parsed as the destination")
		}
	})

	t.Run("and Argv never builds that argv in the first place", func(t *testing.T) {
		if _, err := r.remote.Argv(Target{Dest: "-oProxyCommand=whatever", Port: 22}, "cmd"); err == nil {
			t.Errorf("Argv built an option-shaped destination")
		}
	})
}

// TestSSHChildRunsInTheCLocale: this package classifies ssh's stderr by ENGLISH
// substring, so a child in a French locale would fall through to the generic
// transport class for every auth and host-key failure. sdk/bootstrap's
// cLocaleEnv forces the same thing for the same stated reason.
func TestSSHChildRunsInTheCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "fr_FR.UTF-8")
	r := newRig(t, rigOpts{})
	if _, err := r.remote.Probe(e2eContext(t), r.target(), ""); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	assertSeq(t, "the ssh child's LC_ALL", r.lcAllRuns(), []string{"C"})
}

// TestUsableRungsRequiresAnExecutableRegularFile pins the rig's ladder check
// against roost's own gate (`[ -f "$p" ] && [ -x "$p" ]`).
//
// A bare `os.Stat` calls a present-but-non-executable rung "roost is installed
// here", and newRig then REPLAYS the exec chain's documented fall-through
// instead of running it — silently skipping the real fall-through the
// "not installed" case claims to exercise.
func TestUsableRungsRequiresAnExecutableRegularFile(t *testing.T) {
	dir := t.TempDir()

	runnable := filepath.Join(dir, "runnable")
	mustWrite(t, runnable, "#!/bin/sh\n", 0o755)
	// Present, a regular file, and NOT executable: roost's ladder skips it and
	// keeps climbing, so it is not a rung.
	notRunnable := filepath.Join(dir, "not-runnable")
	mustWrite(t, notRunnable, "#!/bin/sh\n", 0o644)
	// A DIRECTORY passes a naive mode check — every directory has an execute
	// bit — and is not a rung either.
	asDir := filepath.Join(dir, "adir")
	mustMkdirAll(t, asDir)
	absent := filepath.Join(dir, "absent")

	assertSeq(t, "usable rungs",
		usableRungs([]string{notRunnable, asDir, absent, runnable}),
		[]string{runnable})
}

// TestEveryNonActionableRowEndToEnd walks the far-side states of §3.2 step 2
// from a real failed exec all the way to the row's pinned copy. The two ssh-
// free rows (no local ssh; no agents) are covered in TestResolveSSHNotFound and
// TestProbeWithNoAgentsAndNoLandingDir.
func TestEveryNonActionableRowEndToEnd(t *testing.T) {
	host := Token{Shed: "dev", Server: "my-server"}

	// ssh's own wording for a host that is not there, which the rig's fake ssh
	// emits verbatim and which ends up as the "unreachable" row's subtitle.
	const refused = "ssh: connect to host fake-host port 22: Connection refused"

	// Each case drives a real failed exec and names, by constructor, the row it
	// must become. §3.2's copy is pinned once, in TestPinnedNonActionableRows —
	// what these cases are for is the far-side state → class → row chain.
	tests := []struct {
		name string
		opts rigOpts
		// probe runs the discovery leg instead of a bridge call. The same
		// class on the probe leg is a reachability failure whatever it is,
		// because the probe runs a login shell and nothing else — it cannot
		// tell anybody anything about roost-session.
		probe bool
		class SSHClass
		phase ReachPhase
		want  Row
	}{
		{
			// No roost-session anywhere: roost's ladder falls off its end,
			// prints `roost-session: command not found`, and exits 127.
			name:  "not installed",
			opts:  rigOpts{session: sessionAbsent},
			class: ClassNotFound,
			phase: PhaseBridge,
			want:  NotInstalledRow(host),
		},
		{
			name:  "installed but not running",
			opts:  rigOpts{session: sessionNoSession},
			class: ClassNoSession,
			phase: PhaseBridge,
			want:  NotRunningRow(host),
		},
		{
			name:  "unreachable",
			opts:  rigOpts{unreachable: true},
			class: ClassTransport,
			phase: PhaseBridge,
			want:  UnreachableRow(host, refused),
		},
		{
			name:  "an unreachable probe",
			opts:  rigOpts{unreachable: true},
			probe: true,
			class: ClassTransport,
			phase: PhaseProbe,
			want:  UnreachableRow(host, refused),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.opts)
			var err error
			if tc.probe {
				_, err = r.remote.Probe(e2eContext(t), r.target(), "")
			} else {
				_, err = r.remote.Identify(e2eContext(t), r.target())
			}
			assertReachClass(t, err, tc.class, tc.phase)
			row, ok := RowForError(host, err)
			if !ok {
				t.Fatalf("not a row: %v", err)
			}
			assertRow(t, row, tc.want)
		})
	}

	// Not in the table: this one is not an ssh failure at all — the bridge
	// answered, and answered with a protocol this build does not speak — so it
	// asserts a different error type on the way to its row. Pin P6: a
	// mismatched session is REPORTED, never restarted.
	t.Run("protocol mismatch", func(t *testing.T) {
		r := newRig(t, rigOpts{protocol: 2})
		_, err := r.remote.Identify(e2eContext(t), r.target())
		var mismatch *ProtocolMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("err = %v, want a ProtocolMismatchError", err)
		}
		if mismatch.Spoken != 2 {
			t.Errorf("spoken = %d", mismatch.Spoken)
		}
		row, ok := RowForError(host, err)
		if !ok {
			t.Fatalf("not a row: %v", err)
		}
		assertRow(t, row, ProtocolMismatchRow(host, 2))
	})
}

func assertReachClass(t *testing.T, err error, class SSHClass, phase ReachPhase) {
	t.Helper()
	var reach *ReachError
	if !errors.As(err, &reach) {
		t.Fatalf("err = %v, want a ReachError", err)
	}
	if reach.Class != class {
		t.Errorf("class = %q, want %q (stderr said: %q)", reach.Class, class, reach.LastLine)
	}
	if reach.Phase != phase {
		t.Errorf("phase = %q, want %q", reach.Phase, phase)
	}
}

// TestActivateStepTwoEndToEnd is §3.2 step 2 as a whole: probe, identify, build
// the agent menu, and confirm every row's id is a token the NEXT step can read.
func TestActivateStepTwoEndToEnd(t *testing.T) {
	r := newRig(t, rigOpts{agents: []string{"claude", "codex", "cursor-agent"}})
	ctx := e2eContext(t)
	host := Token{Shed: "dev", Server: "my-server"}

	probe, err := r.remote.Probe(ctx, r.target(), "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if _, err := r.remote.Identify(ctx, r.target()); err != nil {
		t.Fatalf("Identify: %v", err)
	}

	m := AgentMenu(host, probe)
	if len(m.Items) != 3 {
		t.Fatalf("rows = %+v", m.Items)
	}
	for _, row := range m.Items {
		tok, err := ParseToken(row.ID)
		if err != nil {
			t.Fatalf("row %q id does not parse: %v", row.Title, err)
		}
		if NextStep(tok) != StepWorkdirs {
			t.Errorf("row %q lands on %v, want the workdir step", row.Title, NextStep(tok))
		}
		if tok.Home != r.home {
			t.Errorf("row %q carries home %q", row.Title, tok.Home)
		}
	}
}

// TestActivateStepThreeEndToEnd covers §3.2 step 3 at both ends of the project
// count, including the one-candidate collapse.
func TestActivateStepThreeEndToEnd(t *testing.T) {
	t.Run("zero projects and no landing dir collapses straight to open", func(t *testing.T) {
		r := newRig(t, rigOpts{agents: []string{"codex"}})
		ctx := e2eContext(t)

		probe, err := r.remote.Probe(ctx, r.target(), "")
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		projects, err := r.remote.TabList(ctx, r.target())
		if err != nil {
			t.Fatalf("TabList: %v", err)
		}
		if len(projects) != 0 {
			t.Fatalf("projects = %+v", projects)
		}

		tok := Token{Machine: "mini2", Agent: "codex", Home: probe.Home}
		candidates := WorkdirCandidates(projects, probe.Home, "", probe.LandingDirExists)
		opened, collapsed := CollapseWorkdirs(tok, candidates)
		if !collapsed {
			t.Fatalf("a lone Home candidate did not collapse: %+v", candidates)
		}
		if opened.Cwd != r.home {
			t.Errorf("cwd = %q, want %q", opened.Cwd, r.home)
		}

		// And the open really goes through, with the absolute $HOME as cwd —
		// `~` never reaches `tab.open`, which stores a non-empty cwd verbatim.
		params, err := TabOpenFor(opened)
		if err != nil {
			t.Fatalf("TabOpenFor: %v", err)
		}
		if strings.Contains(params.Cwd, "~") {
			t.Errorf("a tilde reached tab.open: %q", params.Cwd)
		}
		if _, err := r.remote.TabOpen(ctx, r.target(), params); err != nil {
			t.Fatalf("TabOpen: %v", err)
		}
		lines := r.requestLines()
		last := lines[len(lines)-1]
		if !strings.Contains(last, `"project_id":"0"`) {
			t.Errorf("a projectless open did not use the \"0\" sentinel: %s", last)
		}
	})

	t.Run("several projects offer a menu that does not collapse", func(t *testing.T) {
		r := newRig(t, rigOpts{
			agents:  []string{"codex"},
			landing: "proj",
			projects: []Project{
				{ID: "1", Name: "roost", Cwd: "/src/roost"},
				{ID: "2", Name: "shed", Cwd: "/src/shed"},
			},
		})
		ctx := e2eContext(t)

		probe, err := r.remote.Probe(ctx, r.target(), filepath.Join(r.home, "proj"))
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		projects, err := r.remote.TabList(ctx, r.target())
		if err != nil {
			t.Fatalf("TabList: %v", err)
		}

		tok := Token{Machine: "mini2", Agent: "codex", Home: probe.Home}
		candidates := WorkdirCandidates(projects, probe.Home, filepath.Join(r.home, "proj"), probe.LandingDirExists)
		if _, collapsed := CollapseWorkdirs(tok, candidates); collapsed {
			t.Fatalf("four candidates collapsed")
		}
		m := WorkdirMenu(tok, candidates)
		if len(m.Items) != 4 {
			t.Fatalf("rows = %+v", m.Items)
		}
		// Projects first, then Home, then the landing dir.
		assertSeq(t, "titles", rowTitles(m), []string{"roost", "shed", "Home", "Landing dir"})
		// Every row's id opens.
		for _, row := range m.Items {
			next, err := ParseToken(row.ID)
			if err != nil {
				t.Fatalf("row %q id does not parse: %v", row.Title, err)
			}
			if NextStep(next) != StepOpen {
				t.Errorf("row %q lands on %v, want the open step", row.Title, NextStep(next))
			}
		}
	})
}

// TestPromptnessWithRealSSH is the one leg that uses a REAL ssh (plan 019 §3.3:
// "One real-ssh test asserts the provider returns promptly while the master
// persists").
//
// It skips cleanly when no sshd is reachable on 127.0.0.1 with the provider's
// own options — which is the case on most development machines, including the
// one this was written on. The availability check runs the SAME argv the
// provider will, so a host where it passes is a host where the provider's
// options really work.
//
// What it asserts is the thing a mux can get wrong in a way no unit test sees:
// with `ControlPersist=60s` the first connection forks a master that OUTLIVES
// the child, and anything on this side that waited for a pipe to reach EOF
// would block for the whole persist window — inside a provider phase roost
// budgets at five seconds.
func TestPromptnessWithRealSSH(t *testing.T) {
	sshBin, err := ResolveSSH()
	if err != nil {
		t.Skip("no ssh on this machine")
	}
	controlDir := shortTempDir(t)
	remote := &Remote{SSHBin: sshBin, ControlDir: controlDir}
	target := Target{Dest: "127.0.0.1", Port: 22}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Availability: the provider's own argv, running `true`.
	argv, err := remote.Argv(target, "true")
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	probe := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if err := probe.Run(); err != nil {
		t.Skipf("no sshd reachable at 127.0.0.1 with the provider's ssh options: %v", err)
	}

	ctlPath := controlPath(controlDir, target.muxKey())
	if _, err := os.Stat(ctlPath); err != nil {
		t.Fatalf("the first connection left no control socket at %s: %v", ctlPath, err)
	}

	// A second call must return promptly — and the master must still be there
	// when it does, which is what proves the return was not the master exiting.
	start := time.Now()
	if _, err := remote.Probe(ctx, target, ""); err != nil {
		// A real far side may well have no agents and no roost-session; the
		// probe itself still has to answer. Only a REACH failure is fatal here.
		var reach *ReachError
		if errors.As(err, &reach) {
			t.Fatalf("the second probe did not reach 127.0.0.1: %v", err)
		}
		t.Logf("the probe ran but its output did not parse (fine for an arbitrary host): %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("the second call took %v — a muxed call must return well inside roost's 5s phase budget", elapsed)
	}
	if _, err := os.Stat(ctlPath); err != nil {
		t.Errorf("the control master did not persist past the call: %v", err)
	}

	t.Cleanup(func() {
		// Do not leave a 60s master behind for a test.
		_ = exec.Command(sshBin, "-o", "ControlPath="+ctlPath, "-O", "exit", target.Dest).Run()
	})
}

// TestEmptyResultIsAProviderFailure covers the two replies that are valid JSON
// and not answers: `{"ok":true,"result":{}}` to `session.identify` and to
// `tab.open`.
//
// Both used to be ACCEPTED. An empty identify decoded to `session_protocol: 0`
// and became a mismatch row claiming the far side "speaks protocol 0" — a
// number no roost has ever spoken, under a subtitle telling the user to
// upgrade whichever side is older. An empty tab.open decoded to an empty id and
// printed `opened tab  on <host>` with exit 0 — a success line for a tab
// nothing proved exists. §3.2 makes malformed far-side output the one case that
// is a PROVIDER FAILURE rather than a row, and that is what both must be now:
// a MalformedReplyError, and RowForError declining to make a row of it.
func TestEmptyResultIsAProviderFailure(t *testing.T) {
	host := Token{Shed: "dev", Server: "my-server"}

	assertMalformed := func(t *testing.T, op string, err error) {
		t.Helper()
		var malformed *MalformedReplyError
		if !errors.As(err, &malformed) {
			t.Fatalf("err = %v (%T), want a MalformedReplyError", err, err)
		}
		if malformed.Op != op {
			t.Errorf("op = %q, want %q", malformed.Op, op)
		}
		if row, ok := RowForError(host, err); ok {
			t.Errorf("malformed far-side output became a row: %+v", row)
		}
	}

	t.Run("session.identify without a session_protocol", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.replaceReply("identify", `{"id":"1","ok":true,"result":{}}`)

		_, err := r.remote.Identify(e2eContext(t), r.target())
		assertMalformed(t, "session.identify", err)

		// The specific wrong answer this replaced: a protocol-0 mismatch.
		var mismatch *ProtocolMismatchError
		if errors.As(err, &mismatch) {
			t.Errorf("an empty identify became a protocol-%d mismatch", mismatch.Spoken)
		}
	})

	t.Run("tab.open without a tab id", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.replaceReply("tabopen", `{"id":"1","ok":true,"result":{}}`)

		params, err := TabOpenFor(Token{
			Machine: "mini2", Agent: "codex", Home: "/home/shed", Cwd: "/home/shed",
		})
		if err != nil {
			t.Fatalf("TabOpenFor: %v", err)
		}
		id, err := r.remote.TabOpen(e2eContext(t), r.target(), params)
		assertMalformed(t, "tab.open", err)
		if id != "" {
			t.Errorf("tab id = %q, want the empty string alongside the error", id)
		}
	})

	// The control for both: the rig's ordinary (vendored) replies still pass
	// the new checks, so this is a rejection of empty results and not of
	// results.
	t.Run("roost's own replies still pass", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		ctx := e2eContext(t)
		if _, err := r.remote.Identify(ctx, r.target()); err != nil {
			t.Errorf("Identify against roost's own vector: %v", err)
		}
		params, err := TabOpenFor(Token{Machine: "mini2", Agent: "codex", Cwd: "/home/shed"})
		if err != nil {
			t.Fatalf("TabOpenFor: %v", err)
		}
		if _, err := r.remote.TabOpen(ctx, r.target(), params); err != nil {
			t.Errorf("TabOpen against roost's own vector: %v", err)
		}
	})
}
