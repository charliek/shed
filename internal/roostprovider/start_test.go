package roostprovider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The start rung's tests (plan 023 §3.4): the wire shape of the command, the
// verdict grammar, and Remote.Start end to end over the fake ssh.

// TestStartCommandIsTheExecChainWithTheStartVerb pins the second ladder.
//
// Two assertions, and they are not the same one twice. The literal is the WIRE
// SHAPE, asserted directly the way Argv and ProbeCommand are — a test that only
// compared the two builder calls would pass with the builder broken in the same
// way for both verbs. The difference check is the relationship the refactor
// exists to guarantee: `client-bridge` is pinned to roost's generated golden
// (TestExecChainCommandMatchesGolden), so a start command that differs from it
// in nothing but the subcommand inherits that pin instead of needing a second
// golden nobody regenerates.
func TestStartCommandIsTheExecChainWithTheStartVerb(t *testing.T) {
	const want = `sh -c 'if [ -n "${HOME:-}" ]; then p="$HOME/.local/bin/roost-session"; ` +
		`[ -f "$p" ] && [ -x "$p" ] && exec "$p" start; fi; ` +
		`p=$(command -v roost-session 2>/dev/null) || p=; ` +
		`case "$p" in /*) [ -f "$p" ] && [ -x "$p" ] && exec "$p" start;; esac; ` +
		`p="/usr/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" start; ` +
		`p="/home/linuxbrew/.linuxbrew/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" start; ` +
		`if [ -n "${HOME:-}" ]; then p="$HOME/.nix-profile/bin/roost-session"; ` +
		`[ -f "$p" ] && [ -x "$p" ] && exec "$p" start; fi; ` +
		`if [ -n "${USER:-}" ]; then p="/etc/profiles/per-user/$USER/bin/roost-session"; ` +
		`[ -f "$p" ] && [ -x "$p" ] && exec "$p" start; fi; ` +
		`p="/nix/var/nix/profiles/default/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" start; ` +
		`p="/run/current-system/sw/bin/roost-session"; [ -f "$p" ] && [ -x "$p" ] && exec "$p" start; ` +
		`printf "%s\n" "roost-session: command not found" >&2; exit 127'`
	if StartCommand != want {
		t.Errorf("StartCommand:\n got %q\nwant %q", StartCommand, want)
	}

	// The two chains differ ONLY in the verb.
	rebridged := strings.ReplaceAll(StartCommand, `exec "$p" start`, `exec "$p" client-bridge`)
	if rebridged != ExecChainCommand {
		t.Errorf("the start chain differs from the bridge chain by more than the verb:\n got %q\nwant %q",
			rebridged, ExecChainCommand)
	}
	// And the verb really is in there eight times — once per rung. A builder
	// that dropped a rung would still pass the difference check above.
	if n := strings.Count(StartCommand, `exec "$p" start`); n != 8 {
		t.Errorf("the start chain execs the verb %d times, want one per rung (8)", n)
	}
}

// TestStartCommandCarriesNoEmbeddedSingleQuote is the property the bridge
// command's own test pins, for the ladder's second spelling: the whole thing is
// ONE single-quoted word, because the close-escape-reopen trick is not an
// escape in csh/tcsh/fish and a far side's login shell is entitled to be any of
// those.
func TestStartCommandCarriesNoEmbeddedSingleQuote(t *testing.T) {
	inner, ok := strings.CutPrefix(StartCommand, "sh -c '")
	if !ok {
		t.Fatalf("the start chain must start with `sh -c '`")
	}
	inner, ok = strings.CutSuffix(inner, "'")
	if !ok {
		t.Fatalf("the start chain must end with a closing single quote")
	}
	if strings.Contains(inner, "'") {
		t.Errorf("the start chain carries an embedded single quote: %q", inner)
	}
}

// TestParseStartVerdict pins roost's verdict grammar, copied from
// `Verdict`'s Display and `Verdict::parse` at the pinned rev
// (roost-ipc/src/session_launch.rs:186-246). roost publishes no vector for a
// verdict line, so this table IS the pin on this side.
func TestParseStartVerdict(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		kind    StartVerdictKind
		pid     int
		failure string
	}{
		{name: "ready", line: "ready pid=4242", kind: VerdictReady, pid: 4242},
		{name: "ready is trimmed", line: "  ready pid=7  ", kind: VerdictReady, pid: 7},
		{name: "already-running with no pid", line: "already-running", kind: VerdictAlreadyRunning},
		{name: "already-running with a pid", line: "already-running pid=91", kind: VerdictAlreadyRunning, pid: 91},
		{
			// roost's `.ok()`: an unreadable pid is a diagnostic loss, not a
			// different outcome — "the word stays and only the suffix goes".
			name: "already-running with an unreadable pid is still already-running",
			line: "already-running pid=", kind: VerdictAlreadyRunning,
		},
		{name: "an error carries roost's reason", line: "error: could not bind the socket", failure: "could not bind the socket"},
		{
			// roost's own fallback for anything else, including a `ready` with
			// no readable pid — `Ready` without one is not a shape roost emits.
			name: "an unreadable ready pid is not a verdict",
			line: "ready pid=abc", failure: `unrecognized readiness verdict: "ready pid=abc"`,
		},
		{name: "a login banner is not a verdict", line: "Welcome to mini3", failure: `unrecognized readiness verdict: "Welcome to mini3"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, failure := parseStartVerdict(tc.line)
			if failure != tc.failure {
				t.Errorf("failure = %q, want %q", failure, tc.failure)
			}
			if verdict.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", verdict.Kind, tc.kind)
			}
			if verdict.PID != tc.pid {
				t.Errorf("pid = %d, want %d", verdict.PID, tc.pid)
			}
		})
	}
}

// TestStartReady is the ordinary case, end to end over the fake ssh: the real
// StartCommand reaches the far side's real ladder, the real `start` runs, and
// its readiness line comes back parsed.
func TestStartReady(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.stageStart(startReply{stdout: "ready pid=4242\n"})

	verdict, err := r.remote.Start(context.Background(), r.target())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if verdict.Kind != VerdictReady || verdict.PID != 4242 {
		t.Errorf("verdict = %+v, want ready pid=4242", verdict)
	}
	if n := r.startCalls(); n != 1 {
		t.Errorf("a ready verdict must be taken at once, got %d starts", n)
	}
	// The WIRE shape, asserted where it really lands: the last argv element is
	// the remote command, and it must be the ladder rather than a bare
	// `roost-session start` that a far side's PATH may not resolve.
	runs := r.argvRuns()
	if len(runs) != 1 {
		t.Fatalf("want exactly one ssh run, got %d", len(runs))
	}
	if got := runs[0][len(runs[0])-1]; got != StartCommand {
		t.Errorf("the remote command was\n %q\nwant %q", got, StartCommand)
	}
}

// TestStartFailures is every way a start does not produce a verdict. Each row
// names the machines.rs rule it is parity with.
func TestStartFailures(t *testing.T) {
	cases := []struct {
		name  string
		reply startReply
		// wantReason is asserted as a SUBSTRING of *StartError.Reason, so a
		// row does not have to restate the exec detail's whole sentence.
		wantReason string
	}{
		{
			// machines.rs:1080 — `Verdict::Error(reason) => start_failed(reason)`.
			name:       "an explicit error verdict fails with roost's own reason",
			reply:      startReply{stdout: "error: could not bind /run/user/1000/roost/session.sock\n", exit: 1},
			wantReason: "could not bind /run/user/1000/roost/session.sock",
		},
		{
			// machines.rs:1071 — stdout carries the verdict and nothing else.
			name:       "empty output is a failure",
			reply:      startReply{},
			wantReason: "it printed no readiness line at all",
		},
		{
			name:       "a malformed verdict is a failure",
			reply:      startReply{stdout: "started, probably\n"},
			wantReason: `unrecognized readiness verdict: "started, probably"`,
		},
		{
			// machines.rs:1041 — **a verdict is only a verdict when the step
			// succeeded.** A ready-looking line under a non-zero exit is not a
			// session that came up; the exec's own detail is the diagnosis.
			name:       "a ready line under a non-zero exit is not a verdict",
			reply:      startReply{stdout: "ready pid=4242\n", stderr: "roost-session: the socket dir is not writable\n", exit: 3},
			wantReason: "the socket dir is not writable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, rigOpts{})
			r.stageStart(tc.reply)

			_, err := r.remote.Start(context.Background(), r.target())
			var startErr *StartError
			if !errors.As(err, &startErr) {
				t.Fatalf("want a *StartError, got %#v", err)
			}
			if !strings.Contains(startErr.Reason, tc.wantReason) {
				t.Errorf("reason %q does not carry %q", startErr.Reason, tc.wantReason)
			}
		})
	}
}

// TestStartNotInstalledIsAReachErrorNotAVerdict: falling off the end of the
// ladder is exit 127 with `command not found`, which is the far side having
// nothing to START. It must arrive classified — the caller's whole decision
// ("is there anything here to start?") rests on it — and never as a verdict.
func TestStartNotInstalledIsAReachErrorNotAVerdict(t *testing.T) {
	r := newRig(t, rigOpts{session: sessionAbsent})

	_, err := r.remote.Start(context.Background(), r.target())
	var reach *ReachError
	if !errors.As(err, &reach) {
		t.Fatalf("want a *ReachError, got %#v", err)
	}
	if reach.Class != ClassNotFound {
		t.Errorf("class = %q, want %q", reach.Class, ClassNotFound)
	}
	if reach.Phase != PhaseStart {
		t.Errorf("phase = %q, want %q", reach.Phase, PhaseStart)
	}
}

// TestStartTransportFailureIsAReachError: a host that is not there fails
// before any roost-session runs, so there is no stdout to mine and the answer
// is the reach classification (rule f).
func TestStartTransportFailureIsAReachError(t *testing.T) {
	r := newRig(t, rigOpts{unreachable: true})

	_, err := r.remote.Start(context.Background(), r.target())
	var reach *ReachError
	if !errors.As(err, &reach) {
		t.Fatalf("want a *ReachError, got %#v", err)
	}
	if reach.Class != ClassTransport {
		t.Errorf("class = %q, want %q", reach.Class, ClassTransport)
	}
	if !strings.Contains(reach.LastLine, "Connection refused") {
		t.Errorf("the failure does not carry ssh's own line: %q", reach.LastLine)
	}
}

// TestStartRetriesAlreadyRunning is roost's socket-lock race: a start that
// loses it is re-tried, and the run that comes up is the answer.
//
// Four losses then a win, so the FIFTH call is the one that succeeds — the last
// attempt startRetries allows.
func TestStartRetriesAlreadyRunning(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.stageStart(
		startReply{stdout: "already-running pid=11\n"},
		startReply{stdout: "already-running pid=11\n"},
		startReply{stdout: "already-running pid=11\n"},
		startReply{stdout: "already-running pid=11\n"},
		startReply{stdout: "ready pid=4242\n"},
	)

	verdict, err := r.remote.Start(context.Background(), r.target())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if verdict.Kind != VerdictReady || verdict.PID != 4242 {
		t.Errorf("verdict = %+v, want ready pid=4242", verdict)
	}
	if n := r.startCalls(); n != startRetries {
		t.Errorf("made %d starts, want %d", n, startRetries)
	}
}

// TestStartAcceptsAlreadyRunningAtTheCap is the other end of the same rule, and
// the reason there is never a sixth attempt: an `already-running` that survives
// every retry is ACCEPTED, exactly as shed-core accepts it (machines.rs:1086,
// `attempts + 1 < START_RETRIES`). What makes that safe is the caller's
// post-start `session.identify`, which asks the session that is actually
// serving who it is rather than trusting the one that was launched.
func TestStartAcceptsAlreadyRunningAtTheCap(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.stageStart(startReply{stdout: "already-running pid=11\n"})

	verdict, err := r.remote.Start(context.Background(), r.target())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if verdict.Kind != VerdictAlreadyRunning || verdict.PID != 11 {
		t.Errorf("verdict = %+v, want already-running pid=11", verdict)
	}
	if n := r.startCalls(); n != startRetries {
		t.Errorf("made %d starts, want exactly %d — there is never a sixth", n, startRetries)
	}
}

// TestStartBudgetExpiryMinesTheReasonFromStdout is rule (a) in its most
// dangerous shape: the budget ran out, and what is sitting in the buffer is
// whatever the far side had managed to print. It must NOT be read as a verdict
// — that is exactly the `exit: None, stdout: "ready pid=…"` case shed-core's
// comment records as a defect — and the reason the user sees must come from
// that stdout when it says something.
//
// Real time and a real deadline: what is under test is that the context cuts
// the exec short, which is a property of the child os/exec is waiting on.
func TestStartBudgetExpiryMinesTheReasonFromStdout(t *testing.T) {
	r := newRig(t, rigOpts{})
	// The far side prints its refusal and then hangs — a session that logged
	// the problem and is now stuck on the socket it cannot have.
	r.stageStart(startReply{stdout: "error: could not bind /run/user/1000/roost/session.sock\n", sleep: 30})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.remote.Start(ctx, r.target())
	elapsed := time.Since(start)

	var startErr *StartError
	if !errors.As(err, &startErr) {
		t.Fatalf("want a *StartError, got %#v", err)
	}
	if !strings.Contains(startErr.Reason, "could not bind /run/user/1000/roost/session.sock") {
		t.Errorf("reason %q was not mined from the stdout that existed", startErr.Reason)
	}
	if startErr.Reach == nil || !startErr.Reach.TimedOut {
		t.Errorf("a budget that expired must arrive as a timed-out reach: %+v", startErr.Reach)
	}
	// Generous, because what it separates is 300 ms from the 30 s hang.
	if elapsed > 10*time.Second {
		t.Errorf("Start took %s; the deadline did not bound the exec", elapsed)
	}
}

// TestStartBudgetExpiryWithAReadyLineIsStillAFailure is the same rule with the
// line that would be a success if the exit status were not checked FIRST.
func TestStartBudgetExpiryWithAReadyLineIsStillAFailure(t *testing.T) {
	r := newRig(t, rigOpts{})
	r.stageStart(startReply{stdout: "ready pid=4242\n", sleep: 30})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	verdict, err := r.remote.Start(ctx, r.target())
	if err == nil {
		t.Fatalf("a ready line on a step that never finished must not be a verdict, got %+v", verdict)
	}
	var startErr *StartError
	if !errors.As(err, &startErr) {
		t.Fatalf("want a *StartError, got %#v", err)
	}
	// The exec's own detail, not the line: `ready` says nothing about a step
	// the deadline killed.
	if strings.Contains(startErr.Reason, "ready") {
		t.Errorf("reason %q quoted the readiness line of a step that never finished", startErr.Reason)
	}
}

// TestStartAlwaysAnswersWithAStartError is the invariant every caller leans on:
// whatever goes wrong, the error out of Start is a *StartError, and a budget
// that ran out is classified rather than raw.
//
// The escape this closes is narrow and entirely real: `run` returns os/exec's
// bare `context deadline exceeded` when the deadline lands BEFORE `cmd.Start`
// rather than during the exec, which is precisely what happens when an
// `already-running` attempt finishes a few milliseconds short of the budget.
func TestStartAlwaysAnswersWithAStartError(t *testing.T) {
	t.Run("a budget that is already spent", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.stageStart(startReply{stdout: "ready pid=4242\n"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := r.remote.Start(ctx, r.target())
		var startErr *StartError
		if !errors.As(err, &startErr) {
			t.Fatalf("want a *StartError, got %#v", err)
		}
		if startErr.Reach == nil || !startErr.Reach.TimedOut {
			t.Errorf("a spent budget must arrive classified and timed out: %+v", startErr.Reach)
		}
		if !strings.Contains(startErr.Reason, "before the deadline") {
			t.Errorf("reason %q does not name the budget", startErr.Reason)
		}
		// Nothing ran over there: the deadline was gone before the exec.
		if n := r.startCalls(); n != 0 {
			t.Errorf("made %d starts against a spent budget, want 0", n)
		}
	})

	t.Run("a budget that expires between retries", func(t *testing.T) {
		r := newRig(t, rigOpts{})
		r.stageStart(startReply{stdout: "already-running pid=11\n"})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Cancel in the gap after the second answer — exactly the window this
		// invariant is about. A goroutine watching the far side's log could
		// only ever cancel while the next attempt was already running, which
		// is the other, already-covered path (sol review finding); the seam
		// makes the gap itself the moment.
		startRetryGap = func(attempt int) {
			if attempt == 1 {
				cancel()
			}
		}
		defer func() { startRetryGap = nil }()

		_, err := r.remote.Start(ctx, r.target())
		var startErr *StartError
		if !errors.As(err, &startErr) {
			t.Fatalf("want a *StartError, got %#v", err)
		}
		if startErr.Reach == nil || !startErr.Reach.TimedOut {
			t.Errorf("a budget that ran out mid-retry must arrive classified: %+v", startErr.Reach)
		}
		if n := r.startCalls(); n < 2 {
			t.Errorf("made %d starts, want the retries to have run", n)
		}
	})
}
