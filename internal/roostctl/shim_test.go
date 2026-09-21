package roostctl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The fake-`roostctl` shim rig, the same shape as
// internal/roostprovider/fakessh_test.go's fake ssh: a shell script that logs
// every argument it was handed and answers from files the test laid down.
//
// **The argv log is the point.** roostctl is reached by exec, so the argv IS
// this package's contract with the real binary — a flag spelled `--host`
// instead of `--id`, or a `--json` that never reaches the child, is a bug no
// amount of asserting on the decoded result can see, because the shim would
// answer the same either way. Every verb's test therefore pins the exact
// argument vector, position by position, and only then looks at what came
// back.
//
// The script lives on PATH under the name `roostctl`, so the default
// `Client{}` — the one production uses, with no Bin and no Run — is what the
// tests drive. A rig that set Bin to an absolute path would never exercise the
// PATH lookup, which is exactly the thing Available's first check depends on.

type shim struct {
	t *testing.T
	// dir holds the script, its reply files and the argv log.
	dir string
	// argvLog is the file every invocation's argv is appended to.
	argvLog string
}

// newShim installs a fake `roostctl` on PATH and returns the rig.
func newShim(t *testing.T) *shim {
	t.Helper()
	dir := t.TempDir()
	s := &shim{t: t, dir: dir, argvLog: filepath.Join(dir, "argv.log")}

	script := strings.NewReplacer(
		"__DIR__", shellQuote(dir),
		"__ARGV_LOG__", shellQuote(s.argvLog),
	).Replace(shimScript)
	writeFile(t, filepath.Join(dir, "roostctl"), script, 0o755)

	// Prepended, not replaced: the script is `/bin/sh` and needs the real
	// tools behind it.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return s
}

// shimScript is the fake `roostctl`, as shell.
//
// The verb key is built from the first one or two words of argv — `identify`,
// `host.status`, `tab.focus`, `rpc.app.sidebar_dump` — which is also a check
// in itself: a call that put `--json` first, or that misspelled a subcommand,
// looks for a reply file that was never written and falls through to the
// "unexpected verb" tail rather than quietly reusing another verb's answer.
const shimScript = `#!/bin/sh
{ for a in "$@"; do printf '%s\n' "$a"; done; printf '\n'; } >> __ARGV_LOG__
if [ -f __DIR__/delay ]; then sleep "$(cat __DIR__/delay)"; fi
op=$1
case "$1" in
  host|tab|session|project) op="$1.$2" ;;
  rpc) op="rpc.$2" ;;
esac
base=__DIR__/reply.$op
if [ ! -f "$base.stdout" ] && [ ! -f "$base.stderr" ] && [ ! -f "$base.exit" ]; then
  printf '%s\n' "fake roostctl: no reply staged for verb $op" >&2
  exit 97
fi
if [ -f "$base.stdout" ]; then cat "$base.stdout"; fi
if [ -f "$base.stderr" ]; then cat "$base.stderr" >&2; fi
if [ -f "$base.exit" ]; then exit "$(cat "$base.exit")"; fi
exit 0
`

// reply stages a verb's stdout. verb is the shim's own key
// ("identify", "host.status", "rpc.app.sidebar_dump", …).
func (s *shim) reply(verb, stdout string) {
	s.t.Helper()
	writeFile(s.t, filepath.Join(s.dir, "reply."+verb+".stdout"), stdout, 0o644)
}

// fail stages a verb's stderr and exit code.
func (s *shim) fail(verb, stderr string, code int) {
	s.t.Helper()
	writeFile(s.t, filepath.Join(s.dir, "reply."+verb+".stderr"), stderr, 0o644)
	writeFile(s.t, filepath.Join(s.dir, "reply."+verb+".exit"), strconv.Itoa(code)+"\n", 0o644)
}

// sleepFor makes every invocation sleep before answering, for the timeout
// case. The value is shell's `sleep` argument, so "5" is five seconds.
func (s *shim) sleepFor(seconds string) {
	s.t.Helper()
	writeFile(s.t, filepath.Join(s.dir, "delay"), seconds+"\n", 0o644)
}

// argvRuns returns every invocation's argv, in order.
func (s *shim) argvRuns() [][]string {
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

// onlyRun returns the single invocation this rig saw, failing if there was not
// exactly one.
func (s *shim) onlyRun() []string {
	s.t.Helper()
	runs := s.argvRuns()
	if len(runs) != 1 {
		s.t.Fatalf("roostctl ran %d times, want exactly once: %v", len(runs), runs)
	}
	return runs[0]
}

// assertArgv pins one invocation's argv position by position.
func assertArgv(t *testing.T, got, want []string) {
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

// vectorResult reads a vendored roost vector and returns its `result` object
// as the one JSON document `roostctl --json` would print for that op.
//
// roost's vectors are whole ENVELOPES (`{id, ok, result}`); roostctl's `--json`
// prints the result alone. Unwrapping here rather than storing a trimmed copy
// keeps the vendored file byte-identical to roost's, which is the whole rule
// crates/fixtures/roost-vectors/README.md states.
func vectorResult(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "crates", "fixtures", "roost-vectors", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	if !envelope.OK || len(envelope.Result) == 0 {
		t.Fatalf("%s is not an ok envelope with a result", name)
	}
	return string(envelope.Result)
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	// WriteFile honours the mode only when it CREATES the file.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// shellQuote wraps a path as one single-quoted shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
