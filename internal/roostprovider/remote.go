package roostprovider

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // a mux path discriminator, not a security primitive — see controlPath
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charliek/shed/internal/config"
)

// ExecChainCommand is the remote command that reaches a far side's
// roost-session: roost's own candidate ladder, first executable rung winning,
// `exec`ed with `client-bridge`.
//
// **A hand-copy of `roost_ipc::bootstrap::exec_chain_command(false)`, pinned
// byte-for-byte by crates/fixtures/roost-vectors/bootstrap/exec-chain-command.txt.**
// That is the design, not duplication waiting to be factored out: the Go tree
// has no Rust in it (`server` is the Go component), and the alternative — a
// second, independently-written ladder — is exactly the two-ladder drift roost's
// own module comments exist to prevent. The golden is GENERATED from the live
// Rust function and asserted from both sides, so a `roost-ipc` bump that changes
// the ladder fails the Rust twin test loudly rather than leaving this constant
// quietly wrong.
//
// Falling off the end of the ladder exits 127 with `roost-session: command not
// found` on stderr, which ClassifySSHFailure reads as ClassNotFound — the
// family whose row becomes "roost-session is not installed on <host>".
const ExecChainCommand = `sh -c 'if [ -n "${HOME:-}" ]; then p="$HOME/.local/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" client-bridge; fi; p=$(command -v roost-session 2>/dev/null) || p=; case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] && exec "$p" client-bridge;; esac; p="/usr/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" client-bridge; p="/home/linuxbrew/.linuxbrew/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" client-bridge; if [ -n "${HOME:-}" ]; then p="$HOME/.nix-profile/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" client-bridge; fi; if [ -n "${USER:-}" ]; then p="/etc/profiles/per-user/$USER/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" client-bridge; fi; p="/nix/var/nix/profiles/default/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" client-bridge; p="/run/current-system/sw/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" client-bridge; printf "%s\n" "roost-session: command not found" >&2; exit 127'`

const (
	// sshConnectTimeoutSecs is ssh's own ConnectTimeout. roost budgets a
	// provider phase at 5s by default, and a handshake is the only part of a
	// step this side cannot bound from Go — so the two numbers are the same
	// on purpose. (`shed_core::machine` uses 10s for its own RC execs; the
	// provider is stricter because it runs inside somebody else's timeout.)
	sshConnectTimeoutSecs = 5
	// controlPersistArg keeps the mux master alive between a wizard's steps.
	// roost's own transport uses the same 60s.
	controlPersistArg = "60s"
	// probeStdoutCap bounds a probe's stdout, matching roost's
	// PROBE_STDOUT_CAP. The real answer is a few hundred bytes; the cap is
	// for a login shell that decides to cat something.
	probeStdoutCap = 64 << 10
	// stderrTailCap bounds how much of a failed exec's stderr is kept,
	// matching roost's SMALL_STDOUT_CAP. Only the LAST bytes are worth
	// keeping — a failing ssh puts its diagnosis last and its banner first.
	stderrTailCap = 4 << 10
)

// sshFallbackPaths are the absolute paths tried, in order, when `ssh` is not on
// PATH (plan 019 §3.3).
//
// This list exists because of how roost runs a provider: by absolute path, with
// roost's OWN environment and no `env_clear`. A Finder-launched macOS roost has
// the minimal `/usr/bin:/bin:/usr/sbin:/sbin` PATH, and a Linux one launched
// from a desktop entry is not much richer — so "ssh is on PATH" is an
// assumption that holds in a terminal and fails in the app. The launcher script
// `--install` writes prefixes these same directories onto PATH; this is the
// belt to that braces, for a launcher written by an older shed.
var sshFallbackPaths = []string{
	"/usr/bin/ssh",
	"/opt/homebrew/bin/ssh",
	"/usr/local/bin/ssh",
}

// ErrNoSSH is returned by ResolveSSH when no `ssh` could be found. The caller
// answers with NoSSHRow, which names the same places this searched.
var ErrNoSSH = errors.New("no ssh binary found")

// ResolveSSH finds the `ssh` binary: PATH first, then sshFallbackPaths.
//
// It does NOT report what it searched. The one thing that wants that list is
// NoSSHRow's subtitle, and NoSSHRow derives it from sshFallbackPaths itself —
// the same way NoAgentsRow derives its list from agentTable, and for the same
// reason: a list threaded through a return value can be handed to the row
// stale, while a derived one cannot drift from what was actually looked at.
func ResolveSSH() (string, error) {
	return resolveSSHWith(exec.LookPath, isExecutableFile)
}

// resolveSSHWith is ResolveSSH's injectable core. lookPath is `exec.LookPath`;
// isExec reports whether an absolute path is a runnable file.
func resolveSSHWith(lookPath func(string) (string, error), isExec func(string) bool) (string, error) {
	if path, err := lookPath("ssh"); err == nil {
		return path, nil
	}
	for _, path := range sshFallbackPaths {
		if isExec(path) {
			return path, nil
		}
	}
	return "", ErrNoSSH
}

// isExecutableFile reports whether path is a regular file with an execute bit.
// A directory named `ssh`, or a file with no execute bit, is not a candidate —
// exec'ing either fails in a way the caller would then have to classify.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// ErrOptionShapedTarget is a Target whose destination would reach `ssh` as an
// OPTION rather than as a host.
var ErrOptionShapedTarget = errors.New("ssh destination begins with a dash")

// Target is one far side's ssh identity: everything the argv builder needs and
// nothing else.
//
// Two constructors build it, because the two reaches genuinely differ. A shed
// is always `<shed>@<server host> -p <ssh port>` pinned against
// `~/.shed/known_hosts` — shed mints those host keys itself, so there is no
// user config to defer to. A machine defers to the user's own `~/.ssh/config`
// for everything the `machines:` entry does not state.
type Target struct {
	// Dest is ssh's destination word: `<shed>@<host>` or `[user@]host`.
	Dest string
	// Port is the ssh port; EmitPort decides whether `-p` is passed at all.
	Port int
	// EmitPort is true when the port must be forced on the command line.
	// False for a machine on 22, so `~/.ssh/config`'s own `Port` still wins —
	// the same rule `shed_core::machine::port_opts` follows.
	EmitPort bool
	// KnownHosts, when non-empty, pins the host key: StrictHostKeyChecking=yes
	// against this file. Empty means the user's own ssh config decides, which
	// is what an unpinned `machines:` entry asks for.
	KnownHosts string
}

// ShedTarget builds the Target for a running shed. knownHosts is
// `config.GetKnownHostsPath()` in production, injected in tests.
//
// The port is always emitted: a shed's sshd is the SERVER's port (2222 by
// default, and routinely something else for a dev server), never a value
// `~/.ssh/config` could be expected to know.
func ShedTarget(s RunningShed, knownHosts string) Target {
	return Target{
		Dest:       s.Name + "@" + s.ServerHost,
		Port:       s.ServerSSHPort,
		EmitPort:   true,
		KnownHosts: knownHosts,
	}
}

// MachineTarget builds the Target for a `machines:` entry, with
// `shed_core::machine`'s exact deferral semantics: no `user@` when the entry
// names none, no `-p` on 22, host-key options only when `known_hosts` is set.
func MachineTarget(m config.MachineEntry) Target {
	dest := m.Host
	if m.User != "" {
		dest = m.User + "@" + dest
	}
	return Target{
		Dest:       dest,
		Port:       m.SSHPort,
		EmitPort:   m.SSHPort != 22,
		KnownHosts: m.KnownHosts,
	}
}

// Validate refuses a Target that cannot be safely spelled on an ssh command
// line.
//
// **OpenSSH parses options BEFORE the destination word.** Verified against the
// ssh on this machine: `ssh -G -oPort=7777 -- echo hi` reports `port 7777` and
// `hostname echo` — the option-shaped word was eaten as an option, the `--`
// then ended option parsing, and the REMOTE COMMAND became the host. So a
// destination of `-oProxyCommand=<anything>` runs that command locally, and the
// `--` Argv emits after the destination cannot prevent it: it lands too late,
// after ssh has already consumed the option. (`--` is still correct and still
// emitted — it protects the remote command, which is what it is there for.)
//
// roost refuses the same shape in its own target classifier
// (roost-ipc/src/ssh.rs: "target starts with '-'; that looks like an option,
// not a host"), and internal/config's `machines:` decoder skips such an entry.
// This is the defence in depth behind both: the decoder is not the only way a
// Target is built, and Argv is the one place a Target becomes argv.
//
// Both halves of `user@host` are checked rather than just the rendered
// destination. `me@-oProxyCommand=x` does not itself begin with a dash and ssh
// would read the whole word as the destination — but an option-shaped hostname
// is never what a config meant, and a shed's destination is composed here from
// two independent sources (the shed name from a server's API, the host from
// config.yaml), either of which would otherwise have to be trusted alone.
func (t Target) Validate() error {
	if t.Dest == "" {
		return fmt.Errorf("ssh target has no destination")
	}
	// Split at the LAST `@`, which is where OpenSSH's own
	// `parse_user_host_port` splits it.
	user, host := "", t.Dest
	if i := strings.LastIndex(t.Dest, "@"); i >= 0 {
		user, host = t.Dest[:i], t.Dest[i+1:]
	}
	if host == "" {
		return fmt.Errorf("ssh target %q names no host", t.Dest)
	}
	if strings.HasPrefix(user, "-") {
		return fmt.Errorf("%w: user %q in %q", ErrOptionShapedTarget, user, t.Dest)
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("%w: host %q in %q", ErrOptionShapedTarget, host, t.Dest)
	}
	return nil
}

// muxKey is the string the ControlPath hashes: every field that decides WHICH
// connection this is, so two targets never share a master.
//
// All four are load-bearing. Dest and Port are the obvious half. EmitPort is
// not cosmetic — with it false, `~/.ssh/config`'s own `Port` for that host
// wins, so `mini2` on 22-not-emitted and `mini2` on 22-emitted can be two
// different sshds. KnownHosts is the one a "destination and port" key gets
// dangerously wrong: two targets with the same `user@host:port` but different
// host-key pins would share one master, and the second target's activation
// inside the ControlPersist window would ride the first's connection with its
// own pin NEVER CHECKED. Every other option Argv emits (BatchMode,
// StrictHostKeyChecking, ConnectTimeout, -T) is either a constant or derived
// from KnownHosts, so this is the whole connection-affecting set.
//
// NUL-joined with a version prefix: none of these values can contain a NUL, so
// no combination of them can collide with another by running together.
func (t Target) muxKey() string {
	return fmt.Sprintf("v1\x00%s\x00%d\x00%t\x00%s", t.Dest, t.Port, t.EmitPort, t.KnownHosts)
}

// Label renders this target the way ssh will actually dial it:
// `[user@]host[:port]`, with each optional half present exactly when Argv
// passes it.
//
// The list menu's subtitle is built from this rather than from the
// `machines:` entry directly, so "the subtitle describes the connection that
// will really be made" is structural instead of two files happening to agree.
// A port that is NOT emitted is also not shown — `~/.ssh/config`'s own `Port`
// is free to win, and a subtitle claiming 22 would be guessing.
func (t Target) Label() string {
	if !t.EmitPort {
		return t.Dest
	}
	return t.Dest + ":" + strconv.Itoa(t.Port)
}

// Remote execs `ssh`. One per provider run; every call against it reuses the
// same binary and the same mux scratch dir.
type Remote struct {
	// SSHBin is the ssh binary to exec. Injected in tests with a fake that
	// runs the remote command locally.
	SSHBin string
	// ControlDir enables the per-target ControlMaster when non-empty. Empty
	// disables muxing entirely, which is correct-but-slower — see
	// ControlDirFor for when that happens.
	ControlDir string
}

// Argv is the full ssh argv for one exec, argv[0] included.
//
// Exported and pure so the wire shape is asserted directly rather than inferred
// from a fake ssh's behaviour: a test that only checked "the command ran" would
// pass with StrictHostKeyChecking silently dropped.
//
// It is also the gate: a Target that would reach ssh as an option rather than
// as a host gets an error instead of an argv (see Target.Validate). Every exec
// in this package goes through here, so there is no second way for one to be
// spelled.
//
// `-T` is not decoration. The bridge's stdout IS the wire — raw bytes, no
// framing — and a PTY would echo the request back, translate LF to CRLF, and
// corrupt every frame. `-T` on the command line also overrides a `RequestTTY
// force` in the user's own ssh config, which `-o RequestTTY=no` alone would
// not.
func (r *Remote) Argv(t Target, remoteCmd string) ([]string, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	argv := []string{r.SSHBin, "-o", "BatchMode=yes"}
	if t.KnownHosts != "" {
		argv = append(argv, "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+t.KnownHosts)
	}
	argv = append(argv, "-o", "ConnectTimeout="+strconv.Itoa(sshConnectTimeoutSecs))
	if r.ControlDir != "" {
		argv = append(argv,
			"-o", "ControlMaster=auto",
			"-o", "ControlPersist="+controlPersistArg,
			"-o", "ControlPath="+controlPath(r.ControlDir, t.muxKey()),
		)
	}
	argv = append(argv, "-T")
	if t.EmitPort {
		argv = append(argv, "-p", strconv.Itoa(t.Port))
	}
	// `--` ends option parsing, so a remote command that begins with a dash is
	// data rather than a flag. It does NOT protect the destination — ssh has
	// already parsed options by the time it reads this — which is what
	// Target.Validate is for.
	return append(argv, t.Dest, "--", remoteCmd), nil
}

// controlDirName is the mux scratch dir's basename.
//
// **Deliberately NOT `roost-*`.** roost sweeps stale `roost-ssh-*` scratch
// directories out of the temp dir on its own schedule, and this directory is
// shed's, holding live control sockets for a wizard that may be mid-step. A
// name in roost's namespace would eventually be swept out from under a running
// master.
const controlDirName = "shed-roost-provider"

// maxControlPathBytes bounds the ControlPath. A control socket is a Unix
// socket, and `sun_path` is 108 bytes on Linux and 104 on macOS — so a path
// that is merely "not too long" on one platform silently fails to bind on the
// other, and ssh reports that as a connection failure rather than as a path
// problem. 100 is under both with room for the mode ssh appends nothing to.
const maxControlPathBytes = 100

// tmpControlRoot is the fallback scratch root when $XDG_RUNTIME_DIR is unset.
//
// Literal `/tmp`, not `os.TempDir()`: on macOS `$TMPDIR` is a per-user
// `/var/folders/<2>/<n>/T/` path that is already ~50 bytes, which leaves no
// room under maxControlPathBytes. The plan pins `/tmp/shed-roost-provider-<uid>`
// for exactly that reason.
const tmpControlRoot = "/tmp"

// ControlDirFor picks and creates the per-user mux scratch directory, or
// returns "" when it cannot get a usable one.
//
// "" is a supported answer, not a failure: without a ControlDir every step pays
// its own handshake, which is slower but correct. Refusing to run because a
// scratch dir was unavailable would trade a working menu for a faster one.
//
// getenv is `os.Getenv` in production. uid is `os.Getuid()`.
func ControlDirFor(getenv func(string) string, uid int) string {
	var candidates []string
	if runtimeDir := getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		candidates = append(candidates, filepath.Join(runtimeDir, controlDirName))
	}
	candidates = append(candidates, filepath.Join(tmpControlRoot, fmt.Sprintf("%s-%d", controlDirName, uid)))

	for _, dir := range candidates {
		// Length is checked against a WORST-CASE member, not against the
		// directory: every control path in a directory is the same length
		// (16 hex characters plus ".ctl"), so one probe answers for all.
		if len(controlPath(dir, "probe")) >= maxControlPathBytes {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			continue
		}
		// Chmod after MkdirAll because MkdirAll leaves an EXISTING directory's
		// mode alone. A directory already at this path with loose permissions
		// would let another local user replace a control socket and so
		// intercept a session; chmod fails outright when the directory belongs
		// to somebody else, and failing to "" (no mux) is the safe outcome.
		if err := os.Chmod(dir, 0o700); err != nil {
			continue
		}
		return dir
	}
	return ""
}

// controlPath is `<dir>/<sha1(target)[:16]>.ctl`.
//
// SHA-1 truncated to 16 hex characters is roost's own precedent, and the choice
// is about LENGTH, not about collision resistance: the input is a destination
// string this process just built, the output only has to be a stable short
// filename per target, and the whole path has to fit in a `sun_path`. Nothing
// authenticates on this value.
func controlPath(dir, target string) string {
	sum := sha1.Sum([]byte(target)) //nolint:gosec // see above: a path discriminator
	return filepath.Join(dir, hex.EncodeToString(sum[:])[:16]+".ctl")
}

// ReachError is a failed ssh exec, already classified into roost's vocabulary.
type ReachError struct {
	// Phase distinguishes the probe from a bridge call. It decides which row
	// the failure becomes: only a BRIDGE failure can mean "roost-session is
	// not installed / not running", because only a bridge call runs
	// roost-session. A probe that exits 127 means the far side has no `bash`,
	// which is a reachability problem wearing the same exit code.
	Phase ReachPhase
	// Op names what was being attempted, for the error string.
	Op string
	// Class is roost's classification of the stderr and exit code.
	Class SSHClass
	// LastLine is the last non-empty trimmed stderr line, whatever the class.
	// (ClassifySSHFailure only returns a detail for ClassTransport, because
	// the other five classes ARE the diagnosis; this field is for display and
	// is populated either way.)
	LastLine string
	// ExitCode is nil when the child never exited on its own.
	ExitCode *int
	// TimedOut is true when the deadline, not the far side, ended the exec.
	TimedOut bool
}

// ReachPhase is which half of an activate a ReachError came from.
type ReachPhase string

const (
	// PhaseProbe — the `bash -lc` discovery round trip.
	PhaseProbe ReachPhase = "probe"
	// PhaseBridge — a `session.identify` / `tab.list` / `tab.open` call over
	// roost's exec chain.
	PhaseBridge ReachPhase = "bridge"
)

func (e *ReachError) Error() string {
	return fmt.Sprintf("%s failed (%s): %s", e.Op, e.Class, e.Detail())
}

// Detail is the row subtitle for this failure: ssh's last stderr line when
// there is one, and otherwise something that still says what happened. An empty
// subtitle under "<host> is unreachable" reads as a rendering bug rather than
// as "ssh said nothing", which is the actual news when a connection times out.
func (e *ReachError) Detail() string {
	switch {
	case e.LastLine != "":
		return e.LastLine
	case e.TimedOut:
		// Deliberately NUMBERLESS. This branch is reached only when the
		// deadline killed the child, which means ssh's OWN ConnectTimeout did
		// not fire — so quoting sshConnectTimeoutSecs here would print the one
		// number that demonstrably did not expire. The number that did is the
		// caller's ctx budget, which a ReachError built from an exec outcome
		// does not carry, and measuring the elapsed wall time instead would
		// make this row's copy jitter run to run for no diagnostic gain. The
		// actionable fact is the whole of it: the deadline ended this, not the
		// far side.
		return "no answer before the deadline"
	case e.ExitCode != nil:
		return fmt.Sprintf("ssh exited %d with nothing on stderr", *e.ExitCode)
	default:
		return "ssh failed with nothing on stderr"
	}
}

// ProtocolMismatchError is the far side answering `session.identify` with a
// protocol this build does not speak. Not a ReachError: the connection worked
// perfectly, and pin P6 says this is REPORTED, never repaired.
type ProtocolMismatchError struct {
	Spoken int
}

func (e *ProtocolMismatchError) Error() string {
	return fmt.Sprintf("the far side speaks roost protocol %d; this shed speaks %d", e.Spoken, SpokenProtocol)
}

// execOutcome is one finished ssh exec.
type execOutcome struct {
	stdout   []byte
	stderr   string
	exitCode *int
	timedOut bool
}

// failed reports whether this exec should be classified rather than parsed.
func (o execOutcome) failed() bool {
	return o.timedOut || o.exitCode == nil || *o.exitCode != 0
}

// reachError classifies a failed exec.
func (o execOutcome) reachError(phase ReachPhase, op string) *ReachError {
	class, _ := ClassifySSHFailure(o.exitCode, o.stderr)
	return &ReachError{
		Phase:    phase,
		Op:       op,
		Class:    class,
		LastLine: lastNonEmptyLine(o.stderr),
		ExitCode: o.exitCode,
		TimedOut: o.timedOut,
	}
}

// run execs one ssh command and waits for it.
//
// **stdout and stderr go to temp FILES, not to pipes**, and that is the one
// non-obvious thing here. With `ControlPersist` the first connection to a
// target leaves behind a background master that outlives this child by up to
// 60 seconds. A pipe reaches EOF only when EVERY holder of its write end has
// closed it, and nothing in ssh's documented contract says the master must
// give up the fds it inherited — so anything that waits for EOF (`cmd.Output`,
// an `io.Copy` drain, `cmd.Wait` on a `bytes.Buffer` writer, which os/exec
// implements with a copy goroutine Wait blocks on) is betting the whole
// five-second provider phase on an implementation detail. An `*os.File` sink
// takes os/exec's no-goroutine path instead: Wait returns when the PROCESS
// exits, and nothing else can hold it.
//
// stdin is handed over as a byte slice for the same reason it is closed: the
// far-side bridge treats stdin EOF as a half-close (it keeps pumping the
// session's answer back), so writing the request and letting os/exec close the
// pipe is what makes the child exit on its own once the answer is through.
func (r *Remote) run(ctx context.Context, t Target, remoteCmd string, stdin []byte, stdoutCap int64) (execOutcome, error) {
	// Before anything is created: an option-shaped destination is refused
	// here, not classified later. It is a local config fault, not a far-side
	// state, so it must reach the caller as a plain error (which RowForError
	// declines to turn into a row) rather than as a ReachError.
	argv, err := r.Argv(t, remoteCmd)
	if err != nil {
		return execOutcome{}, err
	}

	outFile, err := os.CreateTemp("", "shed-roost-provider-out-*")
	if err != nil {
		return execOutcome{}, fmt.Errorf("create the stdout scratch file: %w", err)
	}
	defer os.Remove(outFile.Name())
	defer outFile.Close()

	errFile, err := os.CreateTemp("", "shed-roost-provider-err-*")
	if err != nil {
		return execOutcome{}, fmt.Errorf("create the stderr scratch file: %w", err)
	}
	defer os.Remove(errFile.Name())
	defer errFile.Close()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv[0] is a resolved ssh path, never user input
	// **LC_ALL=C.** ClassifySSHFailure reads ssh's stderr by ENGLISH
	// substring ("Permission denied", "Host key verification failed",
	// "command not found"), so a user in a non-English locale would silently
	// get the generic transport class for every one of them — an auth failure
	// rendered as "unreachable". Forcing the locale is what keeps the
	// classifier's input in the language it was written against;
	// sdk/bootstrap's cLocaleEnv does exactly this in this repo, for exactly
	// this reason. The full environment is forwarded rather than an allowlist
	// because a user's own ProxyCommand may need arbitrary variables, and
	// appending is enough: LC_ALL overrides every locale category, and
	// os/exec takes the LAST value for a duplicate key.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout = outFile
	cmd.Stderr = errFile

	if err := cmd.Start(); err != nil {
		return execOutcome{}, fmt.Errorf("exec %s: %w", argv[0], err)
	}
	// The Wait error is deliberately dropped: a non-zero exit is the NORMAL
	// case here (a far side with no roost-session exits 127), and
	// ProcessState carries everything that error would have said.
	_ = cmd.Wait()

	out := execOutcome{timedOut: ctx.Err() != nil}
	if cmd.ProcessState != nil {
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			out.exitCode = &code
		}
	}
	if out.stdout, err = readCapped(outFile, stdoutCap); err != nil {
		return out, fmt.Errorf("read the stdout scratch file: %w", err)
	}
	tail, err := readTail(errFile, stderrTailCap)
	if err != nil {
		return out, fmt.Errorf("read the stderr scratch file: %w", err)
	}
	out.stderr = tail
	return out, nil
}

// readCapped reads all of f, erroring when it is longer than limit. Truncating
// silently would hand the parser a half record.
//
// The size is asked for rather than discovered, exactly as readTail does it: an
// over-cap file is then rejected without reading a byte of it, and an in-cap
// one is read into a single right-sized buffer instead of io.ReadAll's
// doubling growth — which for a bridge call, whose limit is 16 MiB, is the
// difference between one allocation and a dozen for a large `tab.list`.
func readCapped(f *os.File, limit int64) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("output exceeds the %d byte cap", limit)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, err
	}
	return data, nil
}

// readTail reads the LAST limit bytes of f. A failing ssh puts its banner first
// and its diagnosis last, so the tail is the half worth keeping — the same
// choice roost's own `drain_tail` makes.
func readTail(f *os.File, limit int64) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	offset := int64(0)
	if info.Size() > limit {
		offset = info.Size() - limit
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Probe runs the discovery round trip (plan 019 §3.2 step 2's first half).
//
// landingDir is a shed's landing dir, or "" for a machine and for a shed whose
// server reported none.
func (r *Remote) Probe(ctx context.Context, t Target, landingDir string) (Probe, error) {
	out, err := r.run(ctx, t, ProbeCommand(landingDir), nil, probeStdoutCap)
	if err != nil {
		return Probe{}, err
	}
	if out.failed() {
		return Probe{}, out.reachError(PhaseProbe, "probe")
	}
	return ParseProbe(out.stdout)
}

// call speaks one op over roost's exec chain and decodes its result into out.
//
// One request per connection: the request line goes in as stdin, os/exec closes
// the pipe behind it, and the far side answers and exits. Cheap because of the
// ControlMaster — the handshake was paid by whichever call went first.
func (r *Remote) call(ctx context.Context, t Target, op string, params any, out any) error {
	req, err := encodeRequest(op, params)
	if err != nil {
		return fmt.Errorf("encode the %s request: %w", op, err)
	}
	res, err := r.run(ctx, t, ExecChainCommand, req, maxWireBytes)
	if err != nil {
		return err
	}

	// The reply is parsed BEFORE the exit status is judged, because a far side
	// can legitimately answer and then exit non-zero (the bridge exits 1 on a
	// socket error it hit after flushing). Only when there is no answer at all
	// does the exit status become the diagnosis.
	resp, parseErr := readReply(bytes.NewReader(res.stdout), op)
	if parseErr != nil {
		if res.failed() {
			return res.reachError(PhaseBridge, op)
		}
		return parseErr
	}
	result, err := resp.result()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(result, out); err != nil {
		return fmt.Errorf("decode the %s result: %w", op, err)
	}
	return nil
}

// Identify runs `session.identify` and applies the protocol gate (pin P6:
// a mismatch is reported, never repaired).
func (r *Remote) Identify(ctx context.Context, t Target) (IdentifyResult, error) {
	var res IdentifyResult
	if err := r.call(ctx, t, opSessionIdentify, emptyParams{}, &res); err != nil {
		return IdentifyResult{}, err
	}
	if res.SessionProtocol != SpokenProtocol {
		return res, &ProtocolMismatchError{Spoken: res.SessionProtocol}
	}
	return res, nil
}

// TabList runs `tab.list` and returns every project, tab-less ones included —
// they are perfectly good workdirs.
func (r *Remote) TabList(ctx context.Context, t Target) ([]Project, error) {
	var res TabListResult
	if err := r.call(ctx, t, opTabList, emptyParams{}, &res); err != nil {
		return nil, err
	}
	return res.Projects, nil
}

// TabOpen runs `tab.open` and returns the new tab's id for the confirmation
// line.
func (r *Remote) TabOpen(ctx context.Context, t Target, params TabOpenParams) (string, error) {
	var res TabOpenResult
	if err := r.call(ctx, t, opTabOpen, params, &res); err != nil {
		return "", err
	}
	return res.Tab.ID, nil
}
