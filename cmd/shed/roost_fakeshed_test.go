package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/roostprovider"
)

// The fake far side for the attach flow's REMOTE half (step 6): a fake `ssh`
// that runs the remote command locally, and a fake `roost-session` answering
// `client-bridge` from staged NDJSON.
//
// Same rig as internal/roostprovider/fakessh_test.go's, trimmed to what this
// package's tests need and living here because that one is unexported test
// code in another package. What it keeps is everything load-bearing: the real
// `Remote.Argv`, the real probe script under a real `bash -l`, roost's real
// exec chain resolving a real binary, the real NDJSON dialogue and the real
// decoder. Nothing between the argv and the parser is stubbed — a test that
// faked the transport would pass with the cwd computed on the wrong side.
//
// What it drops relative to the provider's rig: the agent-discovery fixtures
// (this flow opens a tab with NO argv, so which agents the far side has does
// not matter), the unreachable/absent-session variants (classified failures
// are that package's tests to own), the LC_ALL log, and the ssh argv log —
// Remote.Argv is pinned by that package's own tests and by the
// machine-transport differential, so nothing here has an assertion to make
// about it.

type fakeShed struct {
	t *testing.T
	// home is the fake far side's $HOME. It has a space in its name, because
	// `/Users/First Last` is ordinary and is what breaks an unquoted $HOME.
	home string
	// landingDir is an absolute path under home that EXISTS on the far side,
	// or "" when the rig was built without one.
	landingDir string
	// sshBin is the fake ssh; hand it to roostAttach.sshBin.
	sshBin string
	// requests is the file every request line the bridge saw is appended to.
	requests string
	// starts is the file every `roost-session start` invocation is appended
	// to, one line each.
	starts string
	// protocol is what `session.identify` answers with. 0 means the vendored
	// vector's own (v6, which is this side's SpokenProtocol).
	protocol int
	// tabs is the set `tab.list` currently answers with, kept so one reply can
	// be restaged without rewriting the others from nothing.
	tabs []roostprovider.Tab
}

// newFakeShed builds the rig. tabs are the tabs `tab.list` reports, filed
// under one project; withLanding creates the landing directory on the far
// side (a shed whose landing dir is missing is the other case worth testing).
func newFakeShed(t *testing.T, tabs []roostprovider.Tab, withLanding bool) *fakeShed {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "fake home")
	binDir := filepath.Join(home, ".local", "bin")
	mustMkdirAllShed(t, binDir)

	f := &fakeShed{
		t:        t,
		home:     home,
		requests: filepath.Join(dir, "requests.ndjson"),
		starts:   filepath.Join(dir, "starts.log"),
		sshBin:   filepath.Join(dir, "ssh"),
	}
	// The landing dir is named whether or not it exists: a shed's server
	// reports one either way, and "the server named a dir the far side does
	// not have" is exactly the case the probe's LandingDirExists answers.
	f.landingDir = filepath.Join(home, "proj")
	if withLanding {
		mustMkdirAllShed(t, f.landingDir)
	}

	f.writeReplies(dir, tabs)
	writeTestFile(t, filepath.Join(binDir, "roost-session"), f.sessionScript(dir), 0o755)
	writeTestFile(t, f.sshBin, f.sshScript(), 0o755)
	return f
}

// fakeSSHScript is the fake `ssh`: parse options the way OpenSSH does, then
// run the remote command locally as the far side.
//
// **The option parse is not decoration.** Real ssh parses OPTIONS FIRST, takes
// the first non-option word as the destination, then takes the rest as the
// command — consuming a `--` that follows the destination. A fake that just
// scanned for `--` would accept layouts ssh would not, and would hide the one
// behaviour Target.Validate exists to guard.
//
// HOME, USER and PATH are all reset to the FAR SIDE's. A real sshd hands a
// remote command a bare PATH and the target user's own HOME; without resetting
// them the developer's own roost-session leaks into the fake far side and
// every assertion becomes a statement about the machine the test ran on.
const fakeSSHScript = `#!/bin/sh
noopts=0
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

// fakeSessionScript is the fake `roost-session`. Two subcommands are reachable
// from this package: `client-bridge` (one request line in, the staged answer
// out) and `start` (the recovery rung's readiness verdict).
//
// `start` answers from files the test stages (stageStart), one entry per call
// with the last repeating, because the thing under test is how the verdict
// CHANGES across consecutive starts. Every call is logged, so "how many starts
// did this attach make" is an assertion rather than an inference — which is
// what the one-recovery pin rests on.
//
// A `bridge.<op>` script, when one is staged, REPLACES that op's reply — the
// only way to express a bridge that accepts a request and then fails rather
// than answering (it hangs, or it refuses on stderr with an exit code). The
// request line is logged before the script runs either way, so a stalled call
// is still a call that happened.
const fakeSessionScript = `#!/bin/sh
case "$1" in
  start)
    printf 'start\n' >> __START_LOG__
    n=$(wc -l < __START_LOG__ | tr -d ' ')
    [ -f __DIR__/start.$n ] || n=last
    exec /bin/sh __DIR__/start.$n ;;
  client-bridge)
    line=$(head -n 1)
    printf '%s\n' "$line" >> __REQUEST_LOG__
    op=unknown
    case "$line" in
      *session.identify*) op=identify ;;
      *tab.list*) op=tablist ;;
      *tab.open*) op=tabopen ;;
      *tab.set_title*) op=tabsettitle ;;
      *tab.close*) op=tabclose ;;
    esac
    if [ -f __DIR__/bridge.$op ]; then exec /bin/sh __DIR__/bridge.$op; fi
    cat __DIR__/reply.$op.ndjson ;;
  *) printf '%s\n' "fake roost-session: unknown subcommand $1" >&2; exit 2 ;;
esac
`

func (f *fakeShed) sshScript() string {
	return strings.NewReplacer("__HOME__", shellQuoteArg(f.home)).Replace(fakeSSHScript)
}

// sessionScript renders the fake session. dir is quoted, so `__DIR__/x` comes
// out as `'<dir>'/x` and a directory with a space in it still works.
func (f *fakeShed) sessionScript(dir string) string {
	return strings.NewReplacer(
		"__DIR__", shellQuoteArg(dir),
		"__REQUEST_LOG__", shellQuoteArg(f.requests),
		"__START_LOG__", shellQuoteArg(f.starts),
	).Replace(fakeSessionScript)
}

// startReply is one staged answer to `roost-session start`: what it prints,
// what it says on stderr, how long it takes, and how it exits.
type startReply struct {
	stdout string
	stderr string
	// sleep is seconds the start hangs for AFTER printing — the shape a budget
	// has to cut short.
	sleep int
	exit  int
}

// notInstalledStart is roost's own fall-through, byte for byte: what the exec
// chain prints when no rung is executable. Staged as a `start` answer rather
// than produced by removing the fake binary, because the ladder's four
// ABSOLUTE rungs (/usr/bin, linuxbrew, the two nix profiles) are real paths on
// the machine running the test — so "the far side has no roost-session" is not
// a state this rig can produce by omission, only by replay. (The provider
// package's rig makes the same call, and logs which half it ran.)
var notInstalledStart = startReply{stderr: "roost-session: command not found\n", exit: 127}

// startScript renders one reply as the shell the fake `start` execs. The reply
// IS the script — one file per call rather than a sidecar per field, so there
// is nothing to probe for and no way to stage half of one.
func startScript(reply startReply) string {
	var b strings.Builder
	if reply.stdout != "" {
		b.WriteString("printf '%s' " + shellQuoteArg(reply.stdout) + "\n")
	}
	if reply.stderr != "" {
		b.WriteString("printf '%s' " + shellQuoteArg(reply.stderr) + " >&2\n")
	}
	if reply.sleep != 0 {
		b.WriteString("sleep " + strconv.Itoa(reply.sleep) + "\n")
	}
	b.WriteString("exit " + strconv.Itoa(reply.exit) + "\n")
	return b.String()
}

// stageStart lays down the answers `roost-session start` gives, in order. The
// LAST one repeats for every call after it.
func (f *fakeShed) stageStart(replies ...startReply) {
	f.t.Helper()
	if len(replies) == 0 {
		f.t.Fatal("stageStart needs at least one reply")
	}
	dir := filepath.Dir(f.requests)
	for i, reply := range replies {
		writeTestFile(f.t, filepath.Join(dir, "start."+strconv.Itoa(i+1)), startScript(reply), 0o644)
	}
	// The slot every call past the staged set falls back to.
	writeTestFile(f.t, filepath.Join(dir, "start.last"), startScript(replies[len(replies)-1]), 0o644)
}

// startCalls is how many times `roost-session start` ran on the far side.
func (f *fakeShed) startCalls() int {
	f.t.Helper()
	data, err := os.ReadFile(f.starts)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		f.t.Fatalf("reading the start log: %v", err)
	}
	return strings.Count(string(data), "\n")
}

// stallIdentify makes `session.identify` accept the request and never answer:
// the far-side bridge is alive, the socket took the call, and nothing comes
// back. The shape a hung session (or a stalled link) has, and the one that must
// NOT be read as "there is no session over there".
func (f *fakeShed) stallIdentify(seconds int) {
	f.t.Helper()
	writeTestFile(f.t, filepath.Join(filepath.Dir(f.requests), "bridge.identify"),
		"sleep "+strconv.Itoa(seconds)+"\n", 0o644)
}

// refuseIdentifyWithNoSession makes `session.identify` fail the way roost's own
// bridge does when nothing is listening — its exact stderr substring (the one
// ClassifySSHFailure matches on) and exit 1, which roost chose over ssh's 255
// so a client can tell a refusal from a transport failure.
func (f *fakeShed) refuseIdentifyWithNoSession() {
	f.t.Helper()
	f.refuseIdentifyWithNoSessionExit(1)
}

// refuseIdentifyWithNoSessionExit is the same stderr with a chosen exit code —
// the bridge's own refusal exits 1; anything else carrying that substring is a
// blob whose exit status, not its text, is what shed must believe.
func (f *fakeShed) refuseIdentifyWithNoSessionExit(code int) {
	f.t.Helper()
	writeTestFile(f.t, filepath.Join(filepath.Dir(f.requests), "bridge.identify"),
		"printf '%s\\n' 'client-bridge: no session is listening at "+
			"/run/user/1000/roost/session.sock; run roostctl session start on this machine' >&2\n"+
			fmt.Sprintf("exit %d\n", code), 0o644)
}

// setIdentifyReply overwrites `session.identify`'s answer with one line. For
// the replies a vendored vector cannot express: a well-formed envelope that is
// semantically nothing.
func (f *fakeShed) setIdentifyReply(line string) {
	f.t.Helper()
	writeTestFile(f.t, filepath.Join(filepath.Dir(f.requests), "reply.identify.ndjson"), line+"\n", 0o644)
}

// setProtocol restages `session.identify` to answer with a different protocol.
// The vendored vector is v6 — this side's own SpokenProtocol — so a mismatch
// has to be written on purpose.
func (f *fakeShed) setProtocol(protocol int) {
	f.t.Helper()
	f.protocol = protocol
	f.writeReplies(filepath.Dir(f.requests), f.tabs)
}

// writeReplies lays down the NDJSON the fake bridge answers with, built from
// roost's OWN vendored vectors where one exists — so the bytes on this wire
// are roost's, not a shape remembered off a doc page.
func (f *fakeShed) writeReplies(dir string, tabs []roostprovider.Tab) {
	t := f.t
	t.Helper()
	// Remembered so a later restage of one reply (setProtocol) does not drop
	// the tabs another one staged.
	f.tabs = tabs

	identify := readShedVector(t, "session.identify.response.v6.json")
	identify["id"] = "1"
	if f.protocol != 0 {
		identify["result"].(map[string]any)["session_protocol"] = f.protocol
	}
	writeTestFile(t, filepath.Join(dir, "reply.identify.ndjson"), compactShedLine(t, identify), 0o644)

	// `tab.open`'s answer is roost's vector verbatim but for the envelope id,
	// so the tab id this rig hands back is the vector's own "5".
	tabOpen := readShedVector(t, "tab.open.response.json")
	tabOpen["id"] = "1"
	writeTestFile(t, filepath.Join(dir, "reply.tabopen.ndjson"), compactShedLine(t, tabOpen), 0o644)

	// `tab.close` answers with an ack this side does not decode (see
	// Remote.TabClose): a well-formed ok:true with an empty result IS the whole
	// answer. Its value to the rig is the same as set_title's — the REQUEST
	// line lands in the log, and `sessions kill` is defined by which tab id
	// that line carries.
	writeTestFile(t, filepath.Join(dir, "reply.tabclose.ndjson"),
		`{"id":"1","ok":true,"result":{}}`+"\n", 0o644)

	// `tab.set_title` answers with an empty result — its VALUE to this rig is
	// that the request line lands in the log, because "was the title locked?"
	// is the difference between a tab a later attach can find and one it
	// cannot (live-11).
	writeTestFile(t, filepath.Join(dir, "reply.tabsettitle.ndjson"),
		`{"id":"1","ok":true,"result":{}}`+"\n", 0o644)

	// `tab.list`'s tabs are the one part a fixture cannot supply — each test
	// needs its own — but every field roost's `Tab` carries is written, not
	// just the six shed reads, so the decoder meets a real reply rather than
	// one trimmed to what it wants.
	rows := make([]map[string]any, 0, len(tabs))
	for _, tab := range tabs {
		// A tab that names no CreatedAt gets the fixture's own constant: the
		// attach flow does not read it, and a rig that forced every caller to
		// supply one would make those tests say something they do not mean.
		// `shed sessions` DOES read it (the CREATED column), so a test that
		// cares hands its own.
		createdAt := int64(1700000000)
		if tab.CreatedAt != 0 {
			createdAt = tab.CreatedAt
		}
		rows = append(rows, map[string]any{
			"id": tab.ID, "project_id": "1", "title": tab.Title,
			"cwd": tab.Cwd, "state": tab.State, "agent_lifecycle": tab.AgentLifecycle,
			"has_notification": false, "is_active": false, "user_titled": true,
			"position": 0, "created_at": createdAt, "last_active": 1700000050,
			"hook_active": false, "shell_state": "foreground_process",
		})
	}
	writeTestFile(t, filepath.Join(dir, "reply.tablist.ndjson"), compactShedLine(t, map[string]any{
		"id": "1", "ok": true,
		"result": map[string]any{
			"projects": []map[string]any{{
				"id": "1", "name": "shed", "cwd": f.home,
				"position": 0, "created_at": 1700000000, "tabs": rows,
			}},
			"revision": 42,
		},
	}), 0o644)

	unknown := readShedVector(t, "response.error.json")
	unknown["id"] = "1"
	writeTestFile(t, filepath.Join(dir, "reply.unknown.ndjson"), compactShedLine(t, unknown), 0o644)
}

// setTabs restages `tab.list`'s answer after the rig has been built.
//
// The tabs a reuse test needs are ones whose `cwd` is the far side's OWN
// landing dir — a temp path that does not exist until newFakeShed has made it
// — so they cannot be passed in at construction time. Everything else
// writeReplies stages is a fixture constant and is simply rewritten
// identically.
func (f *fakeShed) setTabs(tabs []roostprovider.Tab) {
	f.t.Helper()
	f.writeReplies(filepath.Dir(f.requests), tabs)
}

// requestLines returns every request line the fake bridge recorded, in order.
func (f *fakeShed) requestLines() []string {
	f.t.Helper()
	data, err := os.ReadFile(f.requests)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("reading the request log: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// requestFor returns the single request line for an op, failing if there was
// not exactly one.
func (f *fakeShed) requestFor(op string) string {
	f.t.Helper()
	var found []string
	for _, line := range f.requestLines() {
		if strings.Contains(line, `"op":"`+op+`"`) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		f.t.Fatalf("saw %d %s requests, want exactly 1: %q", len(found), op, f.requestLines())
	}
	return found[0]
}

// countRequests returns how many requests the bridge saw for an op.
func (f *fakeShed) countRequests(op string) int {
	f.t.Helper()
	n := 0
	for _, line := range f.requestLines() {
		if strings.Contains(line, `"op":"`+op+`"`) {
			n++
		}
	}
	return n
}

// readShedVector reads a vendored roost vector as the whole ENVELOPE it is on
// disk — this rig answers the bridge's own wire, where the envelope is what
// goes out (roostVectorResult, the shim's reader, unwraps to the `result`
// instead because that is what `roostctl --json` prints).
func readShedVector(t *testing.T, name string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(roostVectorBytes(t, name), &out); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return out
}

// compactShedLine renders a value as one NDJSON line. The vendored vectors are
// pretty-printed; the wire is line-delimited.
func compactShedLine(t *testing.T, v any) string {
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

func mustMkdirAllShed(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
