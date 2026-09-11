package roostprovider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fake-`ssh` rig (plan 019 §5, C2's test row).
//
// `ssh` is replaced by a shell script that runs the remote command LOCALLY, in
// a fake `$HOME`, against a fake `$HOME/.local/bin/roost-session` — so the
// thing under test is the real one at every layer that matters: the real remote
// command strings (roost's own exec chain, and this package's probe script),
// the real login shell resolving the real PATH out of a real `~/.profile`, the
// real NDJSON dialogue, and roost's own published replies as the bytes on the
// wire.
//
// What it deliberately does NOT fake: nothing between `Remote.Argv` and the
// parser. A test that stubbed the transport would pass with
// `StrictHostKeyChecking` silently dropped, with `$HOME` expanded on the wrong
// side, or with the exec chain producing 127 for a reason nobody noticed.
//
// The fake `$HOME` has a SPACE IN ITS NAME on purpose. `/Users/First Last` is
// ordinary on macOS, it is what breaks an unquoted `$HOME` in a shell script,
// and it is what breaks a naively-encoded row id.

// rigOpts configures one rig.
type rigOpts struct {
	// agents are the agent BINARIES to place in <home>/.local/bin.
	agents []string
	// session picks the fake roost-session's behaviour:
	//   sessionAnswering (zero value) — installed and answering.
	//   sessionAbsent    — not installed at all, so roost's ladder falls off
	//                      its end with `command not found` and exit 127.
	//   sessionNoSession — installed, but `client-bridge` refuses because
	//                      nothing is listening.
	session sessionMode
	// protocol is what the session reports. 0 means SpokenProtocol.
	protocol int
	// unreachable makes the fake ssh fail the way an unreachable host does,
	// before it ever gets to the remote command.
	unreachable bool
	// landing, when non-empty, is a directory created under <home>.
	landing string
	// projects is the `tab.list` reply's project set.
	projects []Project
}

type sessionMode int

const (
	sessionAnswering sessionMode = iota
	sessionAbsent
	sessionNoSession
)

type rig struct {
	t *testing.T
	// home is the fake far side's $HOME. It has a space in its name.
	home string
	// remote is wired to the fake ssh, with no ControlDir: a mux against a
	// script that is not a real ssh would only add a failure mode.
	remote *Remote
	// requests is the file every request line the bridge saw is appended to.
	requests string
	// argv is the file every fake-ssh invocation's argv is appended to.
	argv string
	// lcAll is the file every fake-ssh invocation's $LC_ALL is appended to,
	// one line per run. The classifier reads ssh's stderr by English
	// substring, so the child's locale is part of this package's contract and
	// is asserted like any other part of it.
	lcAll string
}

func newRig(t *testing.T, opts rigOpts) *rig {
	t.Helper()
	dir := shortTempDir(t)
	home := filepath.Join(dir, "fake home")
	binDir := filepath.Join(home, ".local", "bin")
	mustMkdirAll(t, binDir)

	r := &rig{
		t:        t,
		home:     home,
		requests: filepath.Join(dir, "requests.ndjson"),
		argv:     filepath.Join(dir, "argv.log"),
		lcAll:    filepath.Join(dir, "lcall.log"),
	}

	// A login shell's PATH comes from its profile files — which is exactly the
	// shape the real far side has (a shed's `/etc/profile.d/shed-path.sh`, a
	// developer's `~/.profile`), and exactly the trap the probe exists to run
	// into identically to the launch.
	mustWrite(t, filepath.Join(home, ".profile"), "PATH=\"$HOME/.local/bin:$PATH\"\nexport PATH\n", 0o644)

	for _, binary := range opts.agents {
		mustWrite(t, filepath.Join(binDir, binary),
			fmt.Sprintf("#!/bin/sh\necho %s\n", binary), 0o755)
	}
	if opts.landing != "" {
		mustMkdirAll(t, filepath.Join(home, opts.landing))
	}

	protocol := opts.protocol
	if protocol == 0 {
		protocol = SpokenProtocol
	}
	r.writeReplies(dir, protocol, opts.projects)

	replayFallThrough := false
	if opts.session == sessionAbsent {
		// roost's ladder has four rungs at ABSOLUTE paths that no fake $HOME
		// or $USER can jail (`/usr/bin`, linuxbrew, the nix default profile,
		// the NixOS system profile). On a machine that has roost installed,
		// "the far side has no roost-session" is simply not a state this rig
		// can produce — the chain finds the HOST's binary and the far side
		// really does have one.
		//
		// Where that is the case the fake ssh replays the ladder's own
		// documented fall-through instead of running it. The classification
		// and the row are then still asserted end to end on every machine,
		// and the "roost's real chain really does fall through" half runs
		// wherever roost is not installed (CI, and any box without it). Which
		// half ran is logged, never silently chosen.
		if rungs := hostLadderRungs(); len(rungs) > 0 {
			t.Logf("roost is installed here (%s), so the exec chain cannot fall through; "+
				"replaying its documented fall-through instead", strings.Join(rungs, ", "))
			replayFallThrough = true
		}
	} else {
		mustWrite(t, filepath.Join(binDir, "roost-session"), r.sessionScript(dir, opts.session), 0o755)
	}

	sshBin := filepath.Join(dir, "ssh")
	mustWrite(t, sshBin, r.sshScript(opts.unreachable, replayFallThrough), 0o755)
	r.remote = &Remote{SSHBin: sshBin}
	return r
}

// target is the rig's far side. A machine entry, because that is the shape
// with the fewest ssh options in the way; the shed shape is asserted purely in
// Argv's own tests.
func (r *rig) target() Target {
	return Target{Dest: "fake-host", Port: 22}
}

// The rig's two fake binaries, as shell rather than as a sequence of
// WriteString calls.
//
// They are the rig's contract with every end-to-end test in this package, so
// they have to be READABLE AS SHELL — which a line-by-line builder full of
// `\"$seen\"` and doubled `%%s` is not. The variable parts are `__NAME__`
// placeholders substituted by a strings.Replacer (a Replacer, not Sprintf, so
// the scripts' own printf formats need no escaping), and each variant is its
// own named constant instead of an early return in the middle of a builder.
const (
	// sshLogArgv is every fake-ssh variant's prologue: append this
	// invocation's argv, one argument per line and a blank line after, so a
	// test can assert what really reached ssh — and, on its own line in its
	// own file, the locale the child was handed.
	sshLogArgv = `#!/bin/sh
{ for a in "$@"; do printf '%s\n' "$a"; done; printf '\n'; } >> __ARGV_LOG__
printf '%s\n' "${LC_ALL-<unset>}" >> __LCALL_LOG__
`

	// sshRunRemote is the ordinary tail: find the remote command the way
	// OpenSSH does, and run it locally.
	//
	// **It parses argv the way ssh parses it, and that is the whole point.**
	// The obvious fake — scan for `--` anywhere and run the next word — is
	// not a fake of ssh at all: it accepts any layout, so it would pass with
	// the destination on either side of the `--`, and it cannot reproduce the
	// one behaviour that matters here. Real ssh parses OPTIONS FIRST
	// (verified: `ssh -G -oPort=7777 -- echo hi` reports `port 7777` and
	// `hostname echo`), takes the first non-option word as the destination,
	// then takes the rest as the command — consuming a `--` that follows the
	// destination. So a destination beginning with a dash is eaten as an
	// option here exactly as ssh would eat it, which is what makes
	// Target.Validate's guard a real guard rather than a comment.
	//
	// Options after the destination are NOT re-parsed, which real ssh does do
	// (`ssh host -p 22 cmd` works). This package never emits that layout, and
	// faking it would add a branch no test can reach.
	//
	// The command runs through `/bin/sh -c`, which is what a far-side sshd
	// does with the one string ssh hands it (a shed's sshd uses `bash -lc`;
	// either way the command string is re-parsed by a shell on the far side,
	// which is the property under test).
	//
	// HOME, USER and PATH are all reset to the FAR SIDE's, not this process's.
	// A real sshd hands a remote command a bare PATH and the target user's own
	// HOME/USER; without resetting all three, the developer's own agents and
	// roost-session leak into the fake far side and every discovery assertion
	// becomes a statement about the machine the test ran on.
	sshRunRemote = `noopts=0
while [ $# -gt 0 ]; do
  case "$1" in
    --) noopts=1; shift; break ;;
    -B|-b|-c|-D|-E|-e|-F|-I|-i|-J|-L|-l|-m|-O|-o|-P|-p|-Q|-R|-S|-W|-w)
      if [ $# -lt 2 ]; then printf '%s\n' "fake ssh: $1 wants an argument" >&2; exit 255; fi
      shift 2 ;;
    -*) shift ;;
    *) break ;;
  esac
done
if [ $# -eq 0 ]; then printf '%s\n' 'fake ssh: no destination in argv' >&2; exit 255; fi
dest=$1; shift
if [ "$noopts" = 0 ] && [ "${1-}" = "--" ]; then shift; fi
if [ $# -eq 0 ]; then printf '%s\n' "fake ssh: no remote command for destination $dest" >&2; exit 255; fi
cmd=$*
HOME=__HOME__; export HOME
USER=fakeshed; export USER
PATH=/usr/bin:/bin; export PATH
exec /bin/sh -c "$cmd"
`

	// sshFallThrough is byte-for-byte what roost's own `exec_chain_command`
	// emits when every rung misses (`bootstrap.rs`: the printf-and-127 tail of
	// the chain). See newRig for when it is replayed instead of run.
	sshFallThrough = `printf '%s\n' 'roost-session: command not found' >&2
exit 127
`

	// sshUnreachable is ssh's own shape for a host that is not there: its
	// message on stderr, and 255.
	sshUnreachable = `printf '%s\n' 'ssh: connect to host fake-host port 22: Connection refused' >&2
exit 255
`
)

const (
	// sessionHead opens the fake `roost-session`'s dispatch. `identify` and
	// `client-bridge` are the two subcommands anything in this package can
	// reach.
	sessionHead = `#!/bin/sh
case "$1" in
  identify) cat __DIR__/identify.json ;;
  client-bridge)
`

	// sessionAnswer reads the one request line, records it, and answers from
	// the NDJSON the rig laid down.
	//
	// An unsolicited event frame goes out BEFORE every answer. A session
	// pushes these at a connection that never subscribed, and a reader that
	// did not skip them would fail every call made while anything else was
	// happening on the far side — so every call in this rig exercises the
	// skip, not just the one test that names it.
	sessionAnswer = `    line=$(head -n 1)
    printf '%s\n' "$line" >> __REQUEST_LOG__
    op=unknown
    case "$line" in
      *session.identify*) op=identify ;;
      *tab.list*) op=tablist ;;
      *tab.open*) op=tabopen ;;
    esac
    cat __DIR__/event.ndjson
    cat __DIR__/reply.$op.ndjson ;;
`

	// sessionRefuse is roost's own wording for an installed-but-not-running
	// session, and carries the exact substring its classifier matches on
	// (`roost-session/src/bridge.rs`: "The hint is a contract, not just
	// prose"). The exit code is deliberately 1, not 255: roost's bridge
	// refuses to use ssh's own transport code so the client can tell the two
	// apart.
	sessionRefuse = `    printf '%s\n' 'client-bridge: no session is listening at /run/user/1000/roost/session.sock; run roostctl session start on this machine' >&2
    exit 1 ;;
`

	sessionTail = `  *) printf '%s\n' "roost-session: unknown subcommand $1" >&2; exit 2 ;;
esac
`
)

// sshScript is the fake `ssh`: log the argv, then one of three tails.
func (r *rig) sshScript(unreachable, replayFallThrough bool) string {
	tail := sshRunRemote
	switch {
	case replayFallThrough:
		tail = sshFallThrough
	case unreachable:
		tail = sshUnreachable
	}
	return strings.NewReplacer(
		"__ARGV_LOG__", shellQuote(r.argv),
		"__LCALL_LOG__", shellQuote(r.lcAll),
		"__HOME__", shellQuote(r.home),
	).Replace(sshLogArgv + tail)
}

// sessionScript is the fake `roost-session`. dir is where writeReplies laid the
// NDJSON down; it is quoted, so `__DIR__/x` renders as `'<dir>'/x` and a dir
// with a space in it still works.
func (r *rig) sessionScript(dir string, mode sessionMode) string {
	body := sessionAnswer
	if mode == sessionNoSession {
		body = sessionRefuse
	}
	return strings.NewReplacer(
		"__DIR__", shellQuote(dir),
		"__REQUEST_LOG__", shellQuote(r.requests),
	).Replace(sessionHead + body + sessionTail)
}

// writeReplies lays down the NDJSON the fake bridge answers with, built from
// roost's OWN vendored vectors wherever one exists — the bytes on this wire are
// roost's, not a shape remembered off a doc page.
func (r *rig) writeReplies(dir string, protocol int, projects []Project) {
	t := r.t
	t.Helper()

	// `identify` (the subcommand, not the op) is the bootstrap probe's reply.
	// Not reached by anything in C2; written so the fake is a complete
	// roost-session rather than a half of one.
	mustWrite(t, filepath.Join(dir, "identify.json"),
		fmt.Sprintf("roost-session 0.0.19 protocol %d\n", protocol), 0o644)

	identify := readVectorMap(t, "session.identify.response.v4.json")
	identify["id"] = wireRequestID
	identify["result"].(map[string]any)["session_protocol"] = protocol
	mustWrite(t, filepath.Join(dir, "reply.identify.ndjson"), compactLine(t, identify), 0o644)

	tabOpen := readVectorMap(t, "tab.open.response.json")
	tabOpen["id"] = wireRequestID
	mustWrite(t, filepath.Join(dir, "reply.tabopen.ndjson"), compactLine(t, tabOpen), 0o644)

	// `tab.list`'s projects are the one part a fixture cannot supply: each test
	// needs its own set. The envelope around them is still roost's.
	rows := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		rows = append(rows, map[string]any{
			"id": p.ID, "name": p.Name, "cwd": p.Cwd,
			"position": 0, "created_at": 1700000000, "tabs": []any{},
		})
	}
	mustWrite(t, filepath.Join(dir, "reply.tablist.ndjson"), compactLine(t, map[string]any{
		"id": wireRequestID, "ok": true,
		"result": map[string]any{"projects": rows, "revision": 42},
	}), 0o644)

	errorEnvelope := readVectorMap(t, "response.error.json")
	errorEnvelope["id"] = wireRequestID
	mustWrite(t, filepath.Join(dir, "reply.unknown.ndjson"), compactLine(t, errorEnvelope), 0o644)

	event := readVectorMap(t, "tab.opened.event.json")
	mustWrite(t, filepath.Join(dir, "event.ndjson"), compactLine(t, event), 0o644)
}

// replaceReply overwrites the NDJSON the fake bridge answers one op with
// (`identify`, `tablist`, `tabopen` — writeReplies' own file names).
//
// For the replies a fixture cannot express: a well-formed envelope whose
// `result` is empty. That shape has to come from a literal, because every
// vendored vector is by construction a WELL-FORMED answer, and the thing under
// test is what this side does with an answer that is structurally valid and
// semantically nothing.
func (r *rig) replaceReply(op, line string) {
	r.t.Helper()
	mustWrite(r.t, filepath.Join(filepath.Dir(r.requests), "reply."+op+".ndjson"), line+"\n", 0o644)
}

// requestLines returns every request line the fake bridge recorded, in order.
func (r *rig) requestLines() []string {
	r.t.Helper()
	data, err := os.ReadFile(r.requests)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		r.t.Fatalf("reading the request log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// argvRuns returns every fake-ssh invocation's argv, in order.
func (r *rig) argvRuns() [][]string {
	r.t.Helper()
	data, err := os.ReadFile(r.argv)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		r.t.Fatalf("reading the argv log: %v", err)
	}
	var out [][]string
	for _, run := range strings.Split(strings.TrimSuffix(string(data), "\n\n"), "\n\n") {
		if strings.TrimSpace(run) == "" {
			continue
		}
		out = append(out, strings.Split(run, "\n"))
	}
	return out
}

// lcAllRuns returns the $LC_ALL every fake-ssh invocation saw, in order.
// `<unset>` for a run that was handed no LC_ALL at all.
func (r *rig) lcAllRuns() []string {
	r.t.Helper()
	data, err := os.ReadFile(r.lcAll)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		r.t.Fatalf("reading the locale log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func readVectorMap(t *testing.T, name string) map[string]any {
	t.Helper()
	var out map[string]any
	readVectorJSON(t, name, &out)
	return out
}

// compactLine renders a value as one NDJSON line. The vendored vectors are
// pretty-printed; the wire is line-delimited.
func compactLine(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encoding a reply: %v", err)
	}
	if bytes.Count(buf.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("a reply must be one line: %q", buf.String())
	}
	return buf.String()
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	// WriteFile honours the mode only when it CREATES the file; an existing
	// one keeps whatever it had.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// hostLadderRungs returns roost's ABSOLUTE candidate rungs that are USABLE on
// THIS machine — the ones a fake `$HOME`/`$USER` cannot jail.
//
// Read off ExecChainCommand rather than typed from memory, so a roost-ipc bump
// that adds a rung cannot leave this list quietly short. (`/bin` is listed
// because it is where the PATH rung resolves under the fake ssh's minimal
// PATH; on most Linux systems it is a symlink to /usr/bin.)
func hostLadderRungs() []string {
	rungs := []string{
		"/usr/bin/roost-session",
		"/bin/roost-session",
		"/home/linuxbrew/.linuxbrew/bin/roost-session",
		"/nix/var/nix/profiles/default/bin/roost-session",
		"/run/current-system/sw/bin/roost-session",
	}
	for _, rung := range rungs {
		// Every rung but /bin must literally appear in the chain; /bin is the
		// PATH rung's resolution, not a listed one.
		if rung != "/bin/roost-session" && !strings.Contains(ExecChainCommand, rung) {
			panic("hostLadderRungs lists " + rung + ", which is not in the exec chain")
		}
	}
	return usableRungs(rungs)
}

// usableRungs filters a rung list the way roost's ladder gates each candidate:
// `[ -f "$p" ] && [ -x "$p" ]`, a regular file with an execute bit.
//
// **Not a bare `os.Stat`.** The ladder SKIPS a present-but-non-executable
// rung and keeps climbing, so `/usr/bin/roost-session` with no execute bit is a
// host on which the chain really does fall through. A stat-only check calls
// that host "roost is installed here", and newRig then replays the documented
// fall-through instead of running it — silently skipping the very thing the
// case claims to exercise, on the one kind of host where it was available.
func usableRungs(rungs []string) []string {
	var found []string
	for _, rung := range rungs {
		if isExecutableFile(rung) {
			found = append(found, rung)
		}
	}
	return found
}
