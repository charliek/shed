package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

// fakeSessionScript is the fake `roost-session`. Only `client-bridge` is
// reachable from this package: one request line in, the staged answer out.
const fakeSessionScript = `#!/bin/sh
case "$1" in
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
	).Replace(fakeSessionScript)
}

// writeReplies lays down the NDJSON the fake bridge answers with, built from
// roost's OWN vendored vectors where one exists — so the bytes on this wire
// are roost's, not a shape remembered off a doc page.
func (f *fakeShed) writeReplies(dir string, tabs []roostprovider.Tab) {
	t := f.t
	t.Helper()

	identify := readShedVector(t, "session.identify.response.v6.json")
	identify["id"] = "1"
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
