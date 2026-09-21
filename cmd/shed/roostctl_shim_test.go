package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The fake-`roostctl` shim that drives every LOCAL step of the roost attach
// path, and the vendored-vector reader beside it.
//
// Same rig as internal/roostctl/shim_test.go's — a shell script on PATH under
// the name `roostctl`, logging every argv it was handed and answering from
// files the test lays down — with one capability that package's shim does not
// need: **a reply SEQUENCE per verb**. The generation fence and the sidebar
// poll are both defined by how their answers CHANGE across consecutive calls
// (a stale row then a fresh one; a sidebar that has not caught up yet, then
// one that has), and a shim that answers the same bytes every time cannot
// express either. That is why this is a sibling rather than an import: it is a
// different fixture, not a copy of the same one. (C5a's shim is also
// unexported test code in another package, so there is nothing to import.)
//
// The argv log is the point, here as there: roostctl is reached by exec, so
// the argv IS the contract, and a `--tab` spelled `--id` is a bug no assertion
// on a decoded result can see.

type roostShim struct {
	t *testing.T
	// dir holds the script, its reply files, the per-verb sequence cursors
	// and the argv log.
	dir string
	// argvLog is the file every invocation's argv is appended to.
	argvLog string
}

// newRoostShim installs a fake `roostctl` on PATH and returns the rig.
func newRoostShim(t *testing.T) *roostShim {
	t.Helper()
	dir := t.TempDir()
	s := &roostShim{t: t, dir: dir, argvLog: filepath.Join(dir, "argv.log")}

	script := strings.NewReplacer(
		"__DIR__", shellQuoteArg(dir),
		"__ARGV_LOG__", shellQuoteArg(s.argvLog),
	).Replace(roostShimScript)
	writeTestFile(t, filepath.Join(dir, "roostctl"), script, 0o755)

	// Prepended, not replaced: the script is `/bin/sh` and needs the real
	// tools behind it.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return s
}

// roostShimScript is the fake `roostctl`, as shell.
//
// The verb key is built from the first one or two words of argv —
// `identify`, `host.status`, `tab.focus`, `rpc.app.sidebar_dump` — which is a
// check in itself: a call that put `--json` first, or misspelled a
// subcommand, looks for a reply file that was never written and falls through
// to the "unexpected verb" tail rather than quietly reusing another verb's
// answer.
//
// A verb answered by a SEQUENCE keeps its cursor in `reply.<verb>.n`. Each
// call serves `reply.<verb>.<n>.stdout` (plus that entry's own `.stderr` and
// `.exit`, when staged) and advances, and the LAST staged entry repeats
// forever — so "poll until it changes" and "poll something that never changes"
// are both expressible, and a test that polls one more time than it expected
// to does not fall off the end into a confusing exit 97.
//
// A per-entry exit code is what makes "refused once, then fine" expressible:
// roost's refusal envelope is only read as one when the exit is non-zero
// (roostctl's `call`), and `fail` below is a whole-VERB stub that would refuse
// every call including the retry.
//
// A sequence entry with a `.hang` file never answers at all: the shim `exec`s
// a `sleep` in its own process instead of printing. That is the shape a
// wedged roost app has — the socket accepted the call and nothing came back —
// and it is what proves a ceiling actually bounds a CALL rather than only the
// gap between two of them. `exec` rather than a plain `sleep` on purpose: the
// sleeping process IS the child os/exec is waiting on, so a context that
// expires kills it and the pipes close at once, instead of leaving a
// grandchild holding them open past the deadline.
const roostShimScript = `#!/bin/sh
{ for a in "$@"; do printf '%s\n' "$a"; done; printf '\n'; } >> __ARGV_LOG__
op=$1
case "$1" in
  host|tab|session|project) op="$1.$2" ;;
  rpc) op="rpc.$2" ;;
esac
base=__DIR__/reply.$op
if [ -f "$base.1.stdout" ]; then
  n=1
  if [ -f "$base.n" ]; then n=$(cat "$base.n"); fi
  if [ -f "$base.$n.hang" ]; then exec sleep "$(cat "$base.$n.hang")"; fi
  cat "$base.$n.stdout"
  if [ -f "$base.$n.stderr" ]; then cat "$base.$n.stderr" >&2; fi
  next=$((n+1))
  if [ -f "$base.$next.stdout" ]; then printf '%s\n' "$next" > "$base.n"; fi
  if [ -f "$base.$n.exit" ]; then exit "$(cat "$base.$n.exit")"; fi
  exit 0
fi
if [ ! -f "$base.stdout" ] && [ ! -f "$base.stderr" ] && [ ! -f "$base.exit" ]; then
  printf '%s\n' "fake roostctl: no reply staged for verb $op" >&2
  exit 97
fi
if [ -f "$base.stdout" ]; then cat "$base.stdout"; fi
if [ -f "$base.stderr" ]; then cat "$base.stderr" >&2; fi
if [ -f "$base.exit" ]; then exit "$(cat "$base.exit")"; fi
exit 0
`

// reply stages one verb's stdout, answered to every call. verb is the shim's
// own key ("identify", "host.status", "rpc.app.sidebar_dump", …).
func (s *roostShim) reply(verb, stdout string) {
	s.t.Helper()
	writeTestFile(s.t, filepath.Join(s.dir, "reply."+verb+".stdout"), stdout, 0o644)
}

// replySeq stages a verb's answers in order: call 1 gets the first, call 2 the
// second, and the last repeats for every call after it.
func (s *roostShim) replySeq(verb string, stdouts ...string) {
	s.t.Helper()
	if len(stdouts) == 0 {
		s.t.Fatal("replySeq needs at least one reply")
	}
	for i, out := range stdouts {
		writeTestFile(s.t, filepath.Join(s.dir, "reply."+verb+"."+strconv.Itoa(i+1)+".stdout"), out, 0o644)
	}
}

// hangSeq makes a verb's nth staged answer never arrive: that call (and,
// because the cursor only advances past an answered call, every call after it)
// sleeps for seconds instead of printing. The empty stdout beside it is what
// puts the entry in the sequence at all — the shim keys a sequence off
// `reply.<verb>.1.stdout` existing.
func (s *roostShim) hangSeq(verb string, index, seconds int) {
	s.t.Helper()
	base := filepath.Join(s.dir, "reply."+verb+"."+strconv.Itoa(index))
	writeTestFile(s.t, base+".stdout", "", 0o644)
	writeTestFile(s.t, base+".hang", strconv.Itoa(seconds)+"\n", 0o644)
}

// failSeq stages a REFUSAL as one entry of a verb's sequence: that call alone
// answers with roost's error envelope and a non-zero exit, and the call after
// it gets the next staged entry. Use it alongside replySeq, which lays the
// stdout entries down; this adds the stderr and the exit to one of them.
func (s *roostShim) failSeq(verb string, index int, stderr string, code int) {
	s.t.Helper()
	base := filepath.Join(s.dir, "reply."+verb+"."+strconv.Itoa(index))
	writeTestFile(s.t, base+".stderr", stderr, 0o644)
	writeTestFile(s.t, base+".exit", strconv.Itoa(code)+"\n", 0o644)
}

// fail stages a verb's stderr and exit code — roost's own
// `{"error":{"code","message"}}` envelope, the way roostctl prints a refusal
// under --json.
func (s *roostShim) fail(verb, stderr string, code int) {
	s.t.Helper()
	writeTestFile(s.t, filepath.Join(s.dir, "reply."+verb+".stderr"), stderr, 0o644)
	writeTestFile(s.t, filepath.Join(s.dir, "reply."+verb+".exit"), strconv.Itoa(code)+"\n", 0o644)
}

// argvRuns returns every invocation's argv, in order.
func (s *roostShim) argvRuns() [][]string {
	s.t.Helper()
	data, err := os.ReadFile(s.argvLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		s.t.Fatalf("reading the argv log: %v", err)
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

// runsOf returns every invocation whose argv starts with the given words —
// `runsOf("host", "connect")`, `runsOf("tab", "focus")`.
func (s *roostShim) runsOf(prefix ...string) [][]string {
	s.t.Helper()
	var out [][]string
	for _, run := range s.argvRuns() {
		if len(run) < len(prefix) {
			continue
		}
		match := true
		for i, word := range prefix {
			if run[i] != word {
				match = false
				break
			}
		}
		if match {
			out = append(out, run)
		}
	}
	return out
}

// assertShimArgv pins one invocation's argv position by position.
func assertShimArgv(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv:\n got %q\nwant %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q\n got %q\nwant %q", i, got[i], want[i], got, want)
		}
	}
}

// roostVectorResult reads a vendored roost vector and returns its `result`
// object as the one JSON document `roostctl --json` would print for that op.
//
// roost's vectors are whole ENVELOPES (`{id, ok, result}`); roostctl's `--json`
// prints the result alone. Unwrapping here rather than storing a trimmed copy
// keeps the vendored file byte-identical to roost's, which is the rule
// crates/fixtures/roost-vectors/README.md states.
func roostVectorResult(t *testing.T, name string) string {
	t.Helper()
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(roostVectorBytes(t, name), &envelope); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	if !envelope.OK || len(envelope.Result) == 0 {
		t.Fatalf("%s is not an ok envelope with a result", name)
	}
	return string(envelope.Result)
}

// roostVectorBytes reads one vendored roost vector verbatim — the single place
// this package spells that path. Its two readers want different halves of the
// file (the `result` above, the whole envelope in roost_fakeshed_test.go's
// readShedVector), which is no reason for two of them to know where the
// vectors live.
func roostVectorBytes(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "crates", "fixtures", "roost-vectors", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return data
}

// writeTestFile writes one fixture file at an exact mode — a shim script, a
// staged reply, an ssh config. Shared by both rigs in this package (the
// roostctl shim here and the fake shed in roost_fakeshed_test.go), which is
// why it is neither's method.
func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	// WriteFile honours the mode only when it CREATES the file.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}
