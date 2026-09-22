package roostprovider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// The start rung: run `roost-session start` over the ladder and read roost's
// readiness verdict back off stdout (plan 023 §3.4).
//
// **The verdict contract is roost's, restated here.** It is one line of ASCII
// on stdout and nothing else — `roost_ipc::session_launch`'s module doc is
// explicit that "either way stdout carries this line and nothing else — the log
// lives in a file and the console tee is on stderr — so a caller can read
// stdout without a parser". The three shapes and their spellings are copied
// from `Verdict`'s Display and `Verdict::parse` at the pinned rev
// (~/.cargo/git/checkouts/roost-251d7f489d96afc6/8c91ce9/crates/roost-ipc/src/
// session_launch.rs:186-246), never guessed at. roost publishes no vector for a
// verdict line, so the shapes are pinned here by TestParseStartVerdict instead.
//
// **The choreography is shed-core's**, restated for the same reason: Rust's
// `State::Start` / `State::PostStart`
// (crates/shed-core/src/roost/bootstrap/machines.rs:1036-1119) is the twin of
// this file, and every rule below cites the line that owns it. Two clients that
// start the same daemon must agree on what "it started" means.

const (
	// startStdoutCap bounds a start's stdout, matching roost's
	// SMALL_STDOUT_CAP (shed-core's `bootstrap::mod`: "Cap for an exec whose
	// output is a line or nothing — prepare, commit, the readiness verdict").
	// A session that answers this question with more than 4 KiB is not
	// answering it.
	startStdoutCap = 4 << 10

	// startRetries is how many times a `already-running` verdict is re-tried
	// before it is accepted — shed-core's START_RETRIES, and its reasoning
	// verbatim: the window `already-running` covers is the sub-second
	// socket-lock race between two starts, not a slow boot, and what makes
	// ACCEPTING it safe at the end is the post-start `session.identify` the
	// caller runs, which asks the session that is actually serving who it is
	// rather than trusting the one that was launched.
	//
	// There is no sleep between attempts: a full ssh round trip already paces
	// them at the same order of magnitude as roost's own one-second wait, and
	// the loop runs inside the caller's context, so the budget — not this
	// count — is the real ceiling.
	startRetries = 5
)

// StartVerdictKind is which of roost's two SUCCESSFUL verdicts a start got.
// The third (`error: …`) is never a StartVerdict; it is a *StartError.
type StartVerdictKind string

const (
	// VerdictReady — this start brought the session up. The pid is the
	// daemonized child's.
	VerdictReady StartVerdictKind = "ready"
	// VerdictAlreadyRunning — somebody else already owns this profile's
	// socket. roost calls losing that race a successful no-op, and so does
	// this: the post-start identify is what decides whether the winner is a
	// session shed can talk to.
	VerdictAlreadyRunning StartVerdictKind = "already-running"
)

// StartVerdict is one accepted readiness line.
type StartVerdict struct {
	Kind StartVerdictKind
	// PID is the pid the line carried, or 0 when it carried none. roost's
	// `AlreadyRunning(None)` is a diagnostic loss and not a different outcome
	// — "the word stays and only the suffix goes" — so a missing pid never
	// changes the Kind.
	PID int
}

// StartError is a start that did not produce an accepted verdict.
//
// Reason is roost's own text wherever roost gave one (an `error: …` line, or
// the unrecognized-line rendering its own parser produces), and otherwise the
// exec's detail. It is what the caller quotes.
//
// Reach is non-nil exactly when the ssh exec itself failed — a host that is not
// there, a 127 off the end of the ladder, a budget that expired — so a caller
// that needs the classification (`errors.As(err, &reach)`) gets it without this
// type re-deriving one. It is nil when the exec succeeded and the far side
// simply said something unusable.
type StartError struct {
	Reason string
	Reach  *ReachError
	// cause is the underlying error when the exec never produced an outcome at
	// all — the budget expiring between two attempts, an option-shaped target,
	// a scratch file that could not be made. Unexported because it is for
	// errors.As, not for display: Reason is what the caller quotes.
	cause error
}

func (e *StartError) Error() string {
	return "roost-session would not start: " + e.Reason
}

// Unwrap exposes the ReachError when there is one. Spelled with the explicit
// nil check because a typed nil in an interface is not nil.
func (e *StartError) Unwrap() error {
	if e.Reach != nil {
		return e.Reach
	}
	if e.cause == nil {
		return nil
	}
	return e.cause
}

// Start runs `roost-session start` on the far side and returns the verdict it
// answered with.
//
// The Probe shape, not call()'s: this is a plain shell command whose stdout is
// parsed, with no NDJSON and no request to write. Nothing about the session's
// IDENTITY is established here — Start says a daemon came up, and only
// Identify can say it is one this shed speaks to. The caller runs both, inside
// one budget (pin: shed-core's START_BUDGET covers the start and the post-start
// identify together).
//
// **A verdict is only a verdict when the step succeeded** (machines.rs:1041).
// A failed step's stdout is still read, but only to mine a REASON out of it —
// a start that refuses writes `error: …` there and exits 1, and demanding a
// zero exit before looking would throw that away for "it exited 1". An exec
// that timed out holding `ready pid=4242` in its buffer is a budget that
// expired, not a session that came up.
func (r *Remote) Start(ctx context.Context, t Target) (StartVerdict, error) {
	for attempt := 0; ; attempt++ {
		verdict, err := r.startOnce(ctx, t)
		if err != nil {
			return StartVerdict{}, err
		}
		if verdict.Kind != VerdictAlreadyRunning || attempt+1 >= startRetries {
			return verdict, nil
		}
		if startRetryGap != nil {
			startRetryGap(attempt)
		}
	}
}

// startRetryGap, when set, runs between two start attempts with the index of
// the attempt that just answered `already-running`. It exists so a test can
// spend the budget exactly in that gap — the window a racing goroutine cannot
// hit deterministically — and is nil outside tests.
var startRetryGap func(attempt int)

// startOnce is one start exec and its verdict.
func (r *Remote) startOnce(ctx context.Context, t Target) (StartVerdict, error) {
	out, err := r.run(ctx, t, StartCommand, nil, startStdoutCap)
	if err != nil {
		return StartVerdict{}, startRunError(ctx, err)
	}
	line, hasLine := firstNonEmptyLine(out.stdout)
	if out.failed() {
		reach := out.reachError(PhaseStart, "start")
		reason := reach.Detail()
		// machines.rs:1063-1069: the failed step's first line is read as a
		// verdict ONLY to mine a reason out of it. A `ready` or
		// `already-running` line on a failed step says nothing, so the exec's
		// own detail stands.
		if hasLine {
			if _, failure := parseStartVerdict(line); failure != "" {
				reason = failure
			}
		}
		return StartVerdict{}, &StartError{Reason: reason, Reach: reach}
	}
	if !hasLine {
		// machines.rs:1071-1073, wording included: stdout carries the verdict
		// and nothing else, so an empty one is a session that answered the
		// question with silence.
		return StartVerdict{}, &StartError{Reason: "it printed no readiness line at all"}
	}
	verdict, failure := parseStartVerdict(line)
	if failure != "" {
		return StartVerdict{}, &StartError{Reason: failure}
	}
	return verdict, nil
}

// startRunError is the one way an error leaves this file WITHOUT an exec
// outcome behind it: `run` never got as far as a finished process.
//
// **Every error out of Start is a *StartError**, and this is why that needed
// saying. The shape that motivated it is a budget expiring BETWEEN retries: an
// `already-running` attempt finishes a few milliseconds before the deadline,
// the next `cmd.Start` meets an already-done context, and os/exec answers with
// a bare `context deadline exceeded`. Raw, that reaches the user as a Go
// runtime phrase with no classification on it at all — and the caller, which
// decides between "this is an ordinary rung failure" and a hard refusal by
// inspecting the error, has nothing to inspect.
//
// A budget that expired is ClassTransport with TimedOut set, which is exactly
// what a killed exec produces one branch over: the two are the same event
// separated only by which side of `cmd.Start` the deadline landed on, so they
// must not be two different errors. `ReachError.Detail` then renders it as "no
// answer before the deadline" — deliberately numberless, see that method.
//
// Anything else (an option-shaped target, a scratch file that could not be
// created) keeps its own text as the Reason and travels as the cause, so
// errors.As still finds ErrOptionShapedTarget behind it.
func startRunError(ctx context.Context, err error) *StartError {
	if ctx.Err() == nil {
		return &StartError{Reason: err.Error(), cause: err}
	}
	reach := &ReachError{Phase: PhaseStart, Op: "start", Class: ClassTransport, TimedOut: true}
	return &StartError{Reason: reach.Detail(), Reach: reach, cause: err}
}

// parseStartVerdict reads one readiness line. A non-empty second return is
// roost's `Verdict::Error` — its own `error: …` reason, or its own rendering of
// a line that is not a verdict at all — and means the first return is unset.
//
// A direct port of `Verdict::parse`, including the two things about it that are
// easy to get wrong: an `already-running pid=` whose pid does not parse is
// still `AlreadyRunning` (roost's `.ok()` — the pid is a diagnostic, not the
// outcome), while a `ready pid=` whose pid does not parse falls all the way
// through to the unrecognized-line error (roost's nested `if let Ok`), because
// `Ready` without a pid is not a shape roost can emit.
func parseStartVerdict(line string) (StartVerdict, string) {
	line = strings.TrimSpace(line)
	if raw, ok := strings.CutPrefix(line, "ready pid="); ok {
		if pid, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			return StartVerdict{Kind: VerdictReady, PID: pid}, ""
		}
	}
	if line == "already-running" {
		return StartVerdict{Kind: VerdictAlreadyRunning}, ""
	}
	if raw, ok := strings.CutPrefix(line, "already-running pid="); ok {
		pid, _ := strconv.Atoi(strings.TrimSpace(raw))
		return StartVerdict{Kind: VerdictAlreadyRunning, PID: pid}, ""
	}
	if reason, ok := strings.CutPrefix(line, "error: "); ok {
		return StartVerdict{}, reason
	}
	// roost's own fallback, rendered the same way: `{line:?}` in Rust and %q
	// here both produce a quoted, escaped rendering, so a line full of control
	// characters is quotable back at the user.
	return StartVerdict{}, fmt.Sprintf("unrecognized readiness verdict: %q", line)
}

// firstNonEmptyLine is roost's `first_line`: the FIRST non-empty trimmed line
// of stdout. Its mirror image, lastNonEmptyLine, reads stderr from the other
// end — a verdict is written first and is the only thing on stdout, while a
// failing exec puts its banner first and its diagnosis last.
func firstNonEmptyLine(stdout []byte) (string, bool) {
	for _, line := range strings.Split(string(stdout), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed, true
		}
	}
	return "", false
}
