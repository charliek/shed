package roostprovider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestResolveSSHPrefersPath(t *testing.T) {
	lookPath := func(name string) (string, error) {
		if name != "ssh" {
			t.Fatalf("looked up %q", name)
		}
		return "/somewhere/ssh", nil
	}
	got, err := resolveSSHWith(lookPath, func(string) bool {
		t.Fatalf("a fallback was probed even though PATH answered")
		return false
	})
	if err != nil {
		t.Fatalf("resolveSSHWith: %v", err)
	}
	if got != "/somewhere/ssh" {
		t.Errorf("ssh = %q", got)
	}
}

// TestResolveSSHFallbackOrder pins plan 019 §3.3's list. It exists because
// roost runs a provider with roost's OWN environment: a Finder-launched macOS
// roost has the minimal `/usr/bin:/bin:/usr/sbin:/sbin` PATH, so "ssh is on
// PATH" holds in a terminal and fails in the app.
func TestResolveSSHFallbackOrder(t *testing.T) {
	notOnPath := func(string) (string, error) { return "", errors.New("not found") }

	tests := []struct {
		name      string
		available map[string]bool
		want      string
	}{
		{"only /usr/bin", map[string]bool{"/usr/bin/ssh": true}, "/usr/bin/ssh"},
		{"only homebrew", map[string]bool{"/opt/homebrew/bin/ssh": true}, "/opt/homebrew/bin/ssh"},
		{"only /usr/local", map[string]bool{"/usr/local/bin/ssh": true}, "/usr/local/bin/ssh"},
		{
			"the first available wins",
			map[string]bool{"/usr/bin/ssh": true, "/opt/homebrew/bin/ssh": true, "/usr/local/bin/ssh": true},
			"/usr/bin/ssh",
		},
		{
			"homebrew before /usr/local",
			map[string]bool{"/opt/homebrew/bin/ssh": true, "/usr/local/bin/ssh": true},
			"/opt/homebrew/bin/ssh",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSSHWith(notOnPath, func(p string) bool { return tc.available[p] })
			if err != nil {
				t.Fatalf("resolveSSHWith: %v", err)
			}
			if got != tc.want {
				t.Errorf("ssh = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveSSHNotFound(t *testing.T) {
	_, err := resolveSSHWith(
		func(string) (string, error) { return "", errors.New("not found") },
		func(string) bool { return false },
	)
	if !errors.Is(err, ErrNoSSH) {
		t.Fatalf("err = %v, want ErrNoSSH", err)
	}
	// The row this error becomes is the sixth non-actionable one, and the only
	// one no ssh exec can produce; its copy — including the subtitle naming
	// every path resolveSSHWith just searched — is pinned in
	// TestPinnedNonActionableRows. Nothing maps the error to it, because
	// NoSSHRow now derives the list itself: the caller answers ErrNoSSH with
	// NoSSHRow() and there is no argument to get wrong.
}

func TestIsExecutableFile(t *testing.T) {
	dir := t.TempDir()

	runnable := filepath.Join(dir, "runnable")
	if err := os.WriteFile(runnable, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isExecutableFile(runnable) {
		t.Errorf("a 0755 file is not executable?")
	}

	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isExecutableFile(plain) {
		t.Errorf("a 0644 file reported as executable")
	}

	// A DIRECTORY named ssh would pass a naive stat-and-mode check — every
	// directory has an execute bit.
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isExecutableFile(filepath.Join(dir, "adir")) {
		t.Errorf("a directory reported as executable")
	}

	if isExecutableFile(filepath.Join(dir, "absent")) {
		t.Errorf("a missing path reported as executable")
	}
}

// mustArgv is Argv for a target that is expected to be spellable. Every test
// that is not ABOUT the guard goes through here, so the guard's own failure
// mode cannot hide inside an unchecked error.
func mustArgv(t *testing.T, r *Remote, target Target, remoteCmd string) []string {
	t.Helper()
	argv, err := r.Argv(target, remoteCmd)
	if err != nil {
		t.Fatalf("Argv(%+v): %v", target, err)
	}
	return argv
}

func TestShedTargetArgv(t *testing.T) {
	r := &Remote{SSHBin: "/usr/bin/ssh"}
	target := ShedTarget(
		RunningShed{Name: "dev", Server: "my-server", ServerHost: "localhost", ServerSSHPort: 2222},
		"/home/me/.shed/known_hosts",
	)
	got := mustArgv(t, r, target, "echo hi")
	want := []string{
		"/usr/bin/ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=/home/me/.shed/known_hosts",
		"-o", "ConnectTimeout=5",
		"-T",
		"-p", "2222",
		"dev@localhost",
		"--", "echo hi",
	}
	assertSeq(t, "argv", got, want)
}

// TestMachineTargetArgv pins shed-core's own deferral semantics: a `machines:`
// entry states what it states and `~/.ssh/config` decides the rest.
func TestMachineTargetArgv(t *testing.T) {
	r := &Remote{SSHBin: "ssh"}

	t.Run("unpinned, default port, no user", func(t *testing.T) {
		got := mustArgv(t, r, MachineTarget(config.MachineEntry{Name: "mini2", Host: "mini2", SSHPort: 22}), "cmd")
		assertSeq(t, "argv", got, []string{
			"ssh",
			"-o", "BatchMode=yes",
			"-o", "ConnectTimeout=5",
			"-T",
			"mini2",
			"--", "cmd",
		})
	})

	t.Run("pinned known_hosts, a user and a port", func(t *testing.T) {
		got := mustArgv(t, r, MachineTarget(config.MachineEntry{
			Name: "mini2", Host: "mini2.local", User: "charliek", SSHPort: 2222,
			KnownHosts: "/home/me/.ssh/roost_known_hosts",
		}), "cmd")
		assertSeq(t, "argv", got, []string{
			"ssh",
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=yes",
			"-o", "UserKnownHostsFile=/home/me/.ssh/roost_known_hosts",
			"-o", "ConnectTimeout=5",
			"-T",
			"-p", "2222",
			"charliek@mini2.local",
			"--", "cmd",
		})
	})
}

// TestArgvRefusesAnOptionShapedTarget is the second half of the option-shaped
// destination guard (the first is internal/config's `machines:` decoder, which
// never lets one out of config.yaml).
//
// OpenSSH parses options BEFORE the destination word, so a `host:` of
// `-oProxyCommand=…` reaches ssh as an option and runs that command on THIS
// machine. The trailing `--` does not help — TestFakeSSHParsesArgvLikeOpenSSH
// demonstrates the mis-parse on a fake that parses argv the way ssh does. Argv
// is where a Target becomes a command line, so it is where the refusal has to
// be: a Target built by hand, or by a future constructor, gets no free pass.
func TestArgvRefusesAnOptionShapedTarget(t *testing.T) {
	r := &Remote{SSHBin: "ssh"}

	tests := []struct {
		name   string
		target Target
	}{
		{"a bare option as the destination", Target{Dest: "-oProxyCommand=touch /tmp/pwned", Port: 22}},
		{"a short option as the destination", Target{Dest: "-F/dev/null", Port: 22}},
		{"an option-shaped user", Target{Dest: "-oProxyCommand=id@mini2", Port: 22}},
		{"an option-shaped host behind a user", Target{Dest: "me@-oProxyCommand=id", Port: 22}},
		{
			"a machines: entry that somehow got past the decoder",
			MachineTarget(config.MachineEntry{Name: "evil", Host: "-oProxyCommand=id", SSHPort: 22}),
		},
		{
			"a shed whose server host is option-shaped",
			ShedTarget(RunningShed{
				Name: "dev", Server: "s", ServerHost: "-oProxyCommand=id", ServerSSHPort: 2222,
			}, "/home/me/.shed/known_hosts"),
		},
		{
			"a shed whose NAME is option-shaped",
			ShedTarget(RunningShed{
				Name: "-oProxyCommand=id", Server: "s", ServerHost: "mini2", ServerSSHPort: 2222,
			}, "/home/me/.shed/known_hosts"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			argv, err := r.Argv(tc.target, "cmd")
			if err == nil {
				t.Fatalf("Argv built %v for an option-shaped target", argv)
			}
			if !errors.Is(err, ErrOptionShapedTarget) {
				t.Errorf("err = %v, want ErrOptionShapedTarget", err)
			}
			if argv != nil {
				t.Errorf("a refused target still produced argv: %v", argv)
			}
		})
	}

	t.Run("an empty destination is refused too", func(t *testing.T) {
		if _, err := r.Argv(Target{Port: 22}, "cmd"); err == nil {
			t.Errorf("Argv accepted a target with no destination")
		}
	})

	// The far side of the same coin: an ordinary destination still builds.
	t.Run("an ordinary destination is unaffected", func(t *testing.T) {
		if _, err := r.Argv(Target{Dest: "shed@mini2", Port: 22}, "cmd"); err != nil {
			t.Errorf("Argv refused an ordinary target: %v", err)
		}
	})
}

// TestArgvControlMasterOptions pins the mux options and shows that the control
// path is per-target: two different far sides must never share a master.
func TestArgvControlMasterOptions(t *testing.T) {
	r := &Remote{SSHBin: "ssh", ControlDir: "/run/user/1000/shed-roost-provider"}
	a := MachineTarget(config.MachineEntry{Name: "a", Host: "a", SSHPort: 22})
	b := MachineTarget(config.MachineEntry{Name: "b", Host: "b", SSHPort: 22})
	// Same host, different port — still a different far side.
	aOther := MachineTarget(config.MachineEntry{Name: "a", Host: "a", SSHPort: 2222})

	argv := mustArgv(t, r, a, "cmd")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"ControlMaster=auto", "ControlPersist=60s", "ControlPath=/run/user/1000/shed-roost-provider/"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv is missing %q: %v", want, argv)
		}
	}

	pathOf := func(target Target) string {
		t.Helper()
		for _, arg := range mustArgv(t, r, target, "cmd") {
			if path, ok := strings.CutPrefix(arg, "ControlPath="); ok {
				return path
			}
		}
		t.Fatalf("no ControlPath in argv for %+v", target)
		return ""
	}
	if pathOf(a) == pathOf(b) {
		t.Errorf("two hosts share a control path")
	}
	if pathOf(a) == pathOf(aOther) {
		t.Errorf("two ports on one host share a control path")
	}
	// **The host-key pin is part of the identity.** Two targets with the same
	// `user@host:port` and DIFFERENT known_hosts files sharing one master is
	// not a cosmetic collision: activating the second inside the
	// ControlPersist window rides the first's already-authenticated
	// connection, and the second's pin is never checked against anything.
	pinnedA := Target{Dest: "a", Port: 22, KnownHosts: "/home/me/.shed/known_hosts"}
	pinnedB := Target{Dest: "a", Port: 22, KnownHosts: "/home/me/.ssh/other_known_hosts"}
	unpinned := Target{Dest: "a", Port: 22}
	if pathOf(pinnedA) == pathOf(pinnedB) {
		t.Errorf("two different known_hosts pins share a control path")
	}
	if pathOf(pinnedA) == pathOf(unpinned) {
		t.Errorf("a pinned and an unpinned target share a control path")
	}
	// EmitPort likewise: with it false, `~/.ssh/config`'s own Port for that
	// host wins, so the same Dest and the same Port number can be two
	// different sshds.
	if pathOf(Target{Dest: "a", Port: 22}) == pathOf(Target{Dest: "a", Port: 22, EmitPort: true}) {
		t.Errorf("an emitted and a deferred port share a control path")
	}
	// Stability: the same target, described twice, hashes the same. (A fresh
	// Target value rather than the same variable, so the check is about the
	// FIELDS deciding the path — a static analyzer is right that `pathOf(a) !=
	// pathOf(a)` compares one expression with itself.)
	if pathOf(a) != pathOf(MachineTarget(config.MachineEntry{Name: "a", Host: "a", SSHPort: 22})) {
		t.Errorf("the control path is not stable for one target")
	}
	if !strings.HasSuffix(pathOf(a), ".ctl") {
		t.Errorf("control path = %q", pathOf(a))
	}
}

func TestArgvWithoutControlDirHasNoMuxOptions(t *testing.T) {
	r := &Remote{SSHBin: "ssh"}
	for _, arg := range mustArgv(t, r, MachineTarget(config.MachineEntry{Name: "a", Host: "a", SSHPort: 22}), "cmd") {
		if strings.HasPrefix(arg, "Control") {
			t.Errorf("mux option %q leaked in with no ControlDir", arg)
		}
	}
}

func TestControlPathIsShortAndStable(t *testing.T) {
	dir := "/run/user/1000/shed-roost-provider"
	got := controlPath(dir, "shed@localhost:2222")
	if filepath.Dir(got) != dir {
		t.Errorf("control path is not in the dir: %q", got)
	}
	base := filepath.Base(got)
	if len(base) != len("0123456789abcdef.ctl") {
		t.Errorf("control path basename = %q, want 16 hex characters plus .ctl", base)
	}
	if got != controlPath(dir, "shed@localhost:2222") {
		t.Errorf("control path is not deterministic")
	}
}

func TestControlDirFor(t *testing.T) {
	t.Run("XDG_RUNTIME_DIR wins when it is usable", func(t *testing.T) {
		// NOT t.TempDir(): its path embeds the test's own (long) name, which
		// alone exceeds maxControlPathBytes — the test would then exercise the
		// skip path it is not about.
		runtimeDir := shortTempDir(t)
		// Pre-create the directory WORLD-WRITABLE, which is what makes the
		// chmod below a real assertion: os.MkdirAll leaves an existing
		// directory's mode alone, and a loose one would let another local user
		// replace a control socket and so intercept a session.
		mustMkdirAll(t, filepath.Join(runtimeDir, controlDirName))
		if err := os.Chmod(filepath.Join(runtimeDir, controlDirName), 0o777); err != nil {
			t.Fatal(err)
		}
		got := ControlDirFor(func(k string) string {
			if k == "XDG_RUNTIME_DIR" {
				return runtimeDir
			}
			return ""
		}, 1000)
		want := filepath.Join(runtimeDir, controlDirName)
		if got != want {
			t.Fatalf("dir = %q, want %q", got, want)
		}
		info, err := os.Stat(got)
		if err != nil {
			t.Fatalf("the directory was not created: %v", err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("mode = %v, want 0700 — a pre-existing loose mode was not "+
				"tightened, so another local user could replace a control socket",
				info.Mode().Perm())
		}
	})

	// Not `roost-*`: roost sweeps stale `roost-ssh-*` scratch directories out
	// of the temp dir on its own schedule, and this one holds live control
	// sockets for a wizard that may be mid-step.
	t.Run("the directory is not in roost's namespace", func(t *testing.T) {
		if strings.HasPrefix(controlDirName, "roost") {
			t.Errorf("controlDirName = %q is inside roost's scratch namespace", controlDirName)
		}
	})

	// A control socket is a Unix socket, and `sun_path` is 104 bytes on macOS.
	// A path that is merely "not too long" on Linux fails to BIND on macOS, and
	// ssh reports that as a connection failure rather than as a path problem —
	// so an over-long candidate is skipped rather than used.
	t.Run("an over-long XDG_RUNTIME_DIR is skipped", func(t *testing.T) {
		// A real, CREATABLE directory whose path is simply too long — so the
		// only thing that can reject it is the length check. (A bogus path
		// under `/` would be rejected by mkdir instead, and the test would
		// pass with the length check deleted.)
		long := filepath.Join(shortTempDir(t), strings.Repeat("d", 60))
		mustMkdirAll(t, long)
		if len(controlPath(filepath.Join(long, controlDirName), "probe")) < maxControlPathBytes {
			t.Fatalf("the over-long candidate is not actually over-long")
		}
		got := ControlDirFor(func(k string) string {
			if k == "XDG_RUNTIME_DIR" {
				return long
			}
			return ""
		}, 1000)
		if strings.HasPrefix(got, long) {
			t.Fatalf("an over-long candidate was used: %q", got)
		}
		t.Cleanup(func() { _ = os.Remove("/tmp/" + controlDirName + "-1000") })
	})

	t.Run("the fallback is /tmp with the uid", func(t *testing.T) {
		got := ControlDirFor(func(string) string { return "" }, os.Getuid())
		want := fmt.Sprintf("/tmp/%s-%d", controlDirName, os.Getuid())
		if got != want {
			t.Fatalf("dir = %q, want %q", got, want)
		}
		t.Cleanup(func() { _ = os.Remove(got) })
		if _, err := os.Stat(got); err != nil {
			t.Fatalf("the fallback directory was not created: %v", err)
		}
	})

	// "" is a supported answer, not a failure: without a mux every step pays
	// its own handshake, which is slower but correct.
	t.Run("an unusable candidate answers with no mux at all", func(t *testing.T) {
		notADir := filepath.Join(shortTempDir(t), "file")
		if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Both candidates have to be unusable, and making the SECOND one
		// unusable is the part that is easy to get wrong. `uid` is an int, so
		// no representable value makes the /tmp fallback exceed the length
		// limit — an earlier version of this test passed `1`, which produces a
		// perfectly usable `/tmp/shed-roost-provider-1`, so it asserted the
		// SUCCESS path under a name that promises the opposite (and left the
		// directory behind). What does make the fallback fail is a regular
		// FILE sitting at its path: MkdirAll stats it, finds a non-directory
		// and answers ENOTDIR.
		//
		// The uid is this process's pid so two packages running the suite
		// concurrently cannot fight over the same name.
		uid := os.Getpid()
		fallback := filepath.Join(tmpControlRoot, fmt.Sprintf("%s-%d", controlDirName, uid))
		if err := os.WriteFile(fallback, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(fallback) })

		got := ControlDirFor(func(k string) string {
			if k == "XDG_RUNTIME_DIR" {
				return notADir
			}
			return ""
		}, uid)
		if got != "" {
			t.Fatalf("both candidates were unusable, so the answer is no mux at all; got %q", got)
		}
	})
}

func TestReachErrorDetail(t *testing.T) {
	code := 255
	tests := []struct {
		name string
		err  ReachError
		want string
	}{
		{
			"ssh's last stderr line wins",
			ReachError{LastLine: "ssh: connect to host a port 22: Connection refused", ExitCode: &code},
			"ssh: connect to host a port 22: Connection refused",
		},
		{
			// An empty subtitle under "<host> is unreachable" reads as a
			// rendering bug rather than as "ssh said nothing", which is the
			// actual news when a connection times out.
			//
			// NUMBERLESS on purpose: this branch is reached only when ssh's
			// own ConnectTimeout did NOT fire, so printing
			// sshConnectTimeoutSecs would name the one deadline that
			// demonstrably did not expire.
			"a timeout with nothing on stderr still says something, without naming a deadline it did not use",
			ReachError{TimedOut: true},
			"no answer before the deadline",
		},
		{
			"a silent non-zero exit names the code",
			ReachError{ExitCode: &code},
			"ssh exited 255 with nothing on stderr",
		},
		{"nothing at all", ReachError{}, "ssh failed with nothing on stderr"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Detail(); got != tc.want {
				t.Errorf("Detail() = %q, want %q", got, tc.want)
			}
		})
	}

	// The specific regression: the timeout subtitle must not quote ssh's own
	// ConnectTimeout, because that is the deadline that did NOT expire.
	t.Run("the timeout subtitle names no seconds", func(t *testing.T) {
		got := (&ReachError{TimedOut: true}).Detail()
		if strings.Contains(got, strconv.Itoa(sshConnectTimeoutSecs)) {
			t.Errorf("Detail() = %q, which quotes ssh's own ConnectTimeout — the deadline that did not fire", got)
		}
	})
}

// TestBridgeRowPrecedence pins the rule that a timeout or ssh's own exit 255
// OUTRANKS the stderr class on the bridge leg.
//
// The motivating shape is not hypothetical: `session.identify` hangs, and the
// far side's login shell has already printed `foo: command not found` from a
// broken dotfile line. The substring classifier reads that as not-found, and
// the row tells the user roost-session is not installed — on a host where it
// was found and merely became unreachable.
func TestBridgeRowPrecedence(t *testing.T) {
	code127 := 127
	code255 := 255
	code1 := 1

	tests := []struct {
		name     string
		class    SSHClass
		exitCode *int
		timedOut bool
		want     RowKind
	}{
		{"a clean 127 is still the install offer", ClassNotFound, &code127, false, RowNotInstalled},
		{"a clean no-session is still the start offer", ClassNoSession, &code1, false, RowNotRunning},
		{
			"a TIMEOUT outranks a not-found substring from somebody's login shell",
			ClassNotFound, nil, true, RowUnreachable,
		},
		{
			"a timeout outranks a no-session substring too",
			ClassNoSession, nil, true, RowUnreachable,
		},
		{
			"ssh's own 255 outranks a not-found substring",
			ClassNotFound, &code255, false, RowUnreachable,
		},
		{
			"ssh's own 255 outranks a no-session substring",
			ClassNoSession, &code255, false, RowUnreachable,
		},
		{"transport is unreachable however it ended", ClassTransport, &code255, false, RowUnreachable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BridgeRow(tc.class, tc.exitCode, tc.timedOut); got != tc.want {
				t.Errorf("BridgeRow(%q, %v, %t) = %q, want %q", tc.class, tc.exitCode, tc.timedOut, got, tc.want)
			}
		})
	}

	// The PROBE leg is untouched: a login shell cannot say anything about
	// roost-session either way, so every probe failure is already a
	// reachability failure (RowForError, not BridgeRow).
	t.Run("the probe leg still forces unreachable on its own", func(t *testing.T) {
		host := Token{Machine: "mini2"}
		row, ok := RowForError(host, &ReachError{
			Phase: PhaseProbe, Op: "probe", Class: ClassNotFound,
			LastLine: "bash: command not found", ExitCode: &code127,
		})
		if !ok {
			t.Fatalf("not a row")
		}
		assertRow(t, row, UnreachableRow(host, "bash: command not found"))
	})
}

// TestRowForError walks every error a Remote call can produce onto the row a
// user sees — including the phase distinction, which is the one piece of this
// that is easy to get wrong: a PROBE that exits 127 means the far side has no
// `bash`, not that roost-session is missing.
func TestRowForError(t *testing.T) {
	host := Token{Shed: "dev", Server: "my-server"}
	code127 := 127
	code255 := 255

	// Each case names the row it must become, by constructor. The copy those
	// constructors carry is pinned once, in TestPinnedNonActionableRows — what
	// is under test HERE is the mapping, and re-typing §3.2's strings at every
	// mapping site would make a sanctioned reword a three-file edit.
	refused := "ssh: connect to host localhost port 2222: Connection refused"
	denied := "dev@localhost: Permission denied (publickey)."
	tests := []struct {
		name string
		err  error
		want Row
	}{
		{
			"a bridge 127 is the install offer",
			&ReachError{
				Phase: PhaseBridge, Op: "session.identify", Class: ClassNotFound,
				LastLine: "roost-session: command not found", ExitCode: &code127,
			},
			NotInstalledRow(host),
		},
		{
			"a bridge no-session is the start offer",
			&ReachError{
				Phase: PhaseBridge, Op: "session.identify", Class: ClassNoSession,
				LastLine: "client-bridge: no session",
			},
			NotRunningRow(host),
		},
		{
			"a bridge transport failure is unreachable",
			&ReachError{
				Phase: PhaseBridge, Op: "tab.list", Class: ClassTransport,
				LastLine: refused, ExitCode: &code255,
			},
			UnreachableRow(host, refused),
		},
		{
			// Folded in deliberately: §3.2 pins six rows and no more, and an
			// auth failure already states its own case as ssh's last line.
			"a bridge auth failure folds into unreachable, carrying its own line",
			&ReachError{
				Phase: PhaseBridge, Op: "session.identify", Class: ClassAuth,
				LastLine: denied, ExitCode: &code255,
			},
			UnreachableRow(host, denied),
		},
		{
			// Fix: a HUNG bridge call whose stderr happens to carry a
			// not-found substring (a login shell tripping over a broken
			// dotfile line) must not be reported as "roost-session is not
			// installed". Nothing answered; that is the news.
			"a timed-out bridge call outranks a stray not-found substring",
			&ReachError{
				Phase: PhaseBridge, Op: "session.identify", Class: ClassNotFound,
				LastLine: "foo: command not found", TimedOut: true,
			},
			UnreachableRow(host, "foo: command not found"),
		},
		{
			"ssh's own 255 outranks a stray not-found substring",
			&ReachError{
				Phase: PhaseBridge, Op: "session.identify", Class: ClassNotFound,
				LastLine: "ssh: connect to host localhost port 2222: Connection refused",
				ExitCode: &code255,
			},
			UnreachableRow(host, "ssh: connect to host localhost port 2222: Connection refused"),
		},
		{
			// The phase distinction. The same class, the same exit code, a
			// different row — because only a bridge call runs roost-session.
			"a probe 127 is unreachable, not the install offer",
			&ReachError{
				Phase: PhaseProbe, Op: "probe", Class: ClassNotFound,
				LastLine: "bash: command not found", ExitCode: &code127,
			},
			UnreachableRow(host, "bash: command not found"),
		},
		{
			"a protocol mismatch is its own row",
			&ProtocolMismatchError{Spoken: 2},
			ProtocolMismatchRow(host, 2),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, ok := RowForError(host, tc.err)
			if !ok {
				t.Fatalf("not a row")
			}
			assertRow(t, row, tc.want)
		})
	}

	// Acceptance criterion 3: malformed far-side output stays a PROVIDER
	// FAILURE. A row would claim to know something this side never learned.
	t.Run("anything else is not a row", func(t *testing.T) {
		if _, ok := RowForError(host, errors.New("probe output carried no sentinel")); ok {
			t.Errorf("a malformed-output error became a row")
		}
	})

	// Errors travel wrapped through call(); errors.As has to still find them.
	t.Run("a wrapped reach error is still a row", func(t *testing.T) {
		wrapped := fmt.Errorf("identify: %w", &ReachError{
			Phase: PhaseBridge, Class: ClassNoSession, LastLine: "client-bridge: no session",
		})
		if _, ok := RowForError(host, wrapped); !ok {
			t.Errorf("a wrapped ReachError was not recognized")
		}
	})
}

// shortTempDir is a temp directory with a SHORT path.
//
// `t.TempDir()` embeds the test's full name, which for anything in this file is
// already longer than maxControlPathBytes — so a control-path length test built
// on it would silently exercise the skip path instead of the case it names.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "shdrp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
