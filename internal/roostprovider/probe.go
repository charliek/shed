package roostprovider

import (
	"fmt"
	"strings"
)

// probeSentinel is the first record every probe run emits.
//
// A login shell is allowed to print. `/etc/profile.d/*.sh`, mise's activation,
// nvm, a motd fragment sourced from a dotfile — any of them can put bytes on
// stdout BEFORE the script this package wrote ever runs, and those bytes carry
// no NUL, so they would fuse onto the front of record 1 and hand the wizard a
// `$HOME` of "Welcome to mini3\n/home/shed". That value goes straight into
// `tab.open`'s `cwd`, so a silently-corrupted first record is a tab opened in
// the wrong place, not a visible error.
//
// The sentinel makes the framing self-locating: the parser finds the field
// ENDING in this string and reads the records after it, so arbitrary leading
// chatter is discarded rather than absorbed. The `-v1` suffix is the record-set
// version — if the records below ever change shape, this changes with them and
// an old provider talking to a new script fails loudly instead of
// misinterpreting.
const probeSentinel = "shed-roost-probe-v1"

// probeRecordCount is how many records follow the sentinel: $HOME, one per
// agent binary, and the landing-dir flag.
var probeRecordCount = 1 + len(agentTable) + 1

// ProbeScript is the one-round-trip discovery script (plan 019 §3.2 step 2):
// the far side's absolute $HOME, where each of the six agent binaries resolves
// (empty when it does not), and whether $1 — a shed's landing dir — is a
// directory.
//
// Three properties are load-bearing, each for its own reason:
//
//  1. **No embedded single quote, anywhere.** This whole script becomes one
//     single-quoted word in the remote command (`bash -lc '<script>'`), and a
//     POSIX close-escape-reopen quote trick is not an escape in csh/tcsh/fish —
//     which a `machines:` entry's login shell is entitled to be. Everything is
//     double-quoted instead, exactly as roost's own `exec_chain_command` is and
//     for exactly the same reason. The landing dir is the one variable part,
//     and it is passed as a positional argument rather than interpolated.
//
//  2. **One line, `; `-joined.** Same audience: an unescaped newline inside a
//     single-quoted word is not portable across every login shell either, and a
//     one-line remote command is also one line in a log.
//
//  3. **`command -v` under `bash -l`, then roost's own `[ -f ] && [ -x ]`
//     gate.** `command -v` is the same lookup the tab's own
//     `bash -lc 'exec "$@"' shed <bin>` will do, which is the whole point —
//     the menu must not offer an agent the tab would then fail to find, and the
//     login-PATH trap has to bite the probe and the launch identically. But
//     `command -v` alone is not enough, twice over: it reports a shell
//     function, an alias or a builtin as a bare word (which `exec` cannot run),
//     and — measured, not assumed — bash reports a file with NO EXECUTE BIT as
//     a hit, which `exec` then refuses with 126. So the answer is gated exactly
//     the way roost gates its own `PATH` rung (`bootstrap.rs`'s
//     `Candidate::guard`): absolute, a regular file, and executable. Erring
//     toward under-offering is the safe direction — a missing row is a
//     nuisance, a row whose tab dies on open is a bug report.
//
// No `set -eu`: a far side with no `$HOME`, or with `command -v` missing, still
// has an answer, and the answer is "nothing here" rather than a failed exec —
// roost's discovery script makes the same call for the same reason.
func ProbeScript() string {
	steps := []string{
		fmt.Sprintf("printf \"%%s\\0\" \"%s\"", probeSentinel),
		"printf \"%s\\0\" \"${HOME:-}\"",
	}
	for _, a := range agentTable {
		steps = append(steps, fmt.Sprintf(
			"p=$(command -v %s 2>/dev/null) || p=\"\"; case \"$p\" in /*) [ -f \"$p\" ] && [ -x \"$p\" ] || p=\"\" ;; *) p=\"\" ;; esac; printf \"%%s\\0\" \"$p\"",
			a.Binary,
		))
	}
	// `${1:-}` rather than `$1`: a machine's probe passes no positional
	// arguments at all, and an unset `$1` under a login shell that happens to
	// run with `set -u` would abort the script after it had already emitted
	// most of its records.
	steps = append(steps, "if [ -n \"${1:-}\" ] && [ -d \"$1\" ]; then printf \"%s\\0\" 1; else printf \"%s\\0\" \"\"; fi")
	steps = append(steps, "exit 0")
	return strings.Join(steps, "; ")
}

// ProbeCommand is the remote command string for a probe: ProbeScript under a
// login shell, with landingDir handed to it as `$1`.
//
// `bash -lc`, not `sh -c`, because this must resolve agents the way the TAB
// will — roost's PTY execs a tab's argv verbatim in the daemon's environment,
// and the argv this provider opens tabs with is itself `bash -lc`. A shed's
// sshd wraps every remote command in `bash -lc` again (internal/sshd/wrap.go);
// that double wrap is harmless (the outer bash parses one command line and
// execs it — it does not re-expand the single-quoted script).
//
// landingDir is single-quoted with the POSIX close-escape-reopen trick, which is
// the one place this package assumes a POSIX-family login shell on the far side.
// That
// assumption is safe because a landing dir is only ever passed for a SHED, and
// a shed's login shell is bash by construction. A machine gets no positional
// argument, so a machine's probe command carries no single quote beyond the
// pair wrapping the script.
func ProbeCommand(landingDir string) string {
	cmd := "bash -lc " + shellQuote(ProbeScript())
	if landingDir != "" {
		// `shed` as $0 — a conventional stand-in, and what makes the landing
		// dir $1 rather than $0. Quoted for the same reason the dir is.
		cmd += " shed " + shellQuote(landingDir)
	}
	return cmd
}

// shellQuote wraps s in single quotes, escaping embedded single quotes with the
// POSIX close-escape-reopen trick, so it is a single safe shell token. Same as
// internal/ext/rc's shellQuote and cmd/shed/console.go's shellQuoteArg — copied
// rather than shared because neither is exported and this package must not take
// a dependency on either for four lines.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Probe is one far side's answer to ProbeScript.
type Probe struct {
	// Home is the absolute $HOME. Never empty and always `/`-rooted —
	// ParseProbe refuses a probe that reported neither, because every
	// downstream use (the "Home" workdir candidate, the `home=` token key,
	// `tab.open`'s cwd) needs an absolute path and there is nothing sane to
	// substitute.
	Home string
	// Found maps an agent's Kind to its resolved absolute path, for the agents
	// that resolved. An agent absent from this map was not found.
	Found map[string]string
	// LandingDirExists is true when the landing dir handed to ProbeCommand is
	// a directory on the far side. False for a machine, which passes none.
	LandingDirExists bool
}

// ParseProbe reads ProbeScript's NUL-delimited output.
//
// A missing sentinel, any field count other than N+1, a non-empty trailing
// field, or a $HOME that is empty or relative is an ERROR, not a degraded
// result: plan 019's acceptance criterion 3 is explicit that malformed
// far-side output stays a provider failure rather than becoming a row. A row
// would claim to know something about the far side; an error says the provider
// could not find out, which is the truth.
func ParseProbe(stdout []byte) (Probe, error) {
	fields := strings.Split(string(stdout), "\x00")
	start := -1
	for i, f := range fields {
		if strings.HasSuffix(f, probeSentinel) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return Probe{}, fmt.Errorf("probe output carried no %s record", probeSentinel)
	}
	// Every record is NUL-TERMINATED, not NUL-separated, so a complete run of
	// N records splits into EXACTLY N+1 fields with an empty one at the end.
	//
	// Both halves of that are checked, and `==` rather than `>=` is the point.
	// Accepting "at least N+1" accepts a far side that emitted an extra NUL —
	// inside `$HOME`, say — which shifts every later field by one: `$HOME`
	// becomes its own prefix, each agent's path becomes the previous agent's,
	// and the LAST agent's path is read as the landing-dir flag. That is a
	// wrong parse presented as a success, and it ends as a tab opened in a
	// directory nobody named. A short run is the mirror image: without the
	// trailing empty field, output cut one record short splits into exactly
	// probeRecordCount fields whose last is the empty tail, and the
	// landing-dir record silently reads as "does not exist".
	//
	// Neither shape is a row (§3.2 has no copy for "the far side answered
	// something we could not read", and inventing one would claim knowledge
	// this side does not have) — both are provider failures, per acceptance
	// criterion 3.
	if got := len(fields) - start; got != probeRecordCount+1 {
		return Probe{}, fmt.Errorf(
			"probe output carried %d NUL-terminated fields after the sentinel, want exactly %d (%d records and the empty terminator)",
			got, probeRecordCount+1, probeRecordCount)
	}
	if tail := fields[start+probeRecordCount]; tail != "" {
		return Probe{}, fmt.Errorf("probe output was not NUL-terminated: %q trails the %d records", tail, probeRecordCount)
	}
	records := fields[start : start+probeRecordCount]

	home := records[0]
	if home == "" {
		return Probe{}, fmt.Errorf("probe reported no $HOME")
	}
	// §3.2 pins that the probe returns the ABSOLUTE `$HOME` and that `~` never
	// reaches `tab.open` — which stores a non-empty cwd verbatim and hands it
	// to the PTY unexpanded. A relative value here would travel in the row id
	// and land as a relative `tab.open` cwd, resolved against whatever roost's
	// daemon happens to be sitting in. Checked as a literal `/` prefix rather
	// than with filepath.IsAbs: this is the FAR side's rule, and it is POSIX
	// whatever this side is.
	if !strings.HasPrefix(home, "/") {
		return Probe{}, fmt.Errorf("probe reported a relative $HOME %q; it must be absolute", home)
	}
	p := Probe{Home: home, Found: map[string]string{}}
	for i, a := range agentTable {
		if path := records[1+i]; path != "" {
			p.Found[a.Kind] = path
		}
	}
	p.LandingDirExists = records[1+len(agentTable)] != ""
	return p, nil
}
