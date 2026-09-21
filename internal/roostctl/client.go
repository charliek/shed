// Package roostctl drives the LOCAL roost app by exec'ing its own CLI.
//
// **A second wire client is deliberately not built here.** shed already speaks
// roost's session protocol directly — over SSH, to a shed's roost-session, in
// internal/roostprovider — and that is a REMOTE wire with no CLI in front of
// it. The local app is a different problem: it is a running UI with its own
// socket, its own protocol generation and its own ops, and roost ships
// `roostctl` as the supported way in. Re-implementing that socket here would
// be a second thing to keep in step with a crate that publishes no Rust-API
// stability promise, for ops (`tab.focus`, `app.sidebar_dump`, `app.activate`,
// the `host.*` family) shed only ever calls one at a time.
//
// So: exec, `--json`, decode. The argv IS the contract with the real binary,
// which is why this package's tests assert argv exactly rather than only the
// parsed result.
package roostctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// DefaultBin is the binary name resolved on PATH when Client.Bin is empty.
const DefaultBin = "roostctl"

// ExecTimeout bounds EVERY roostctl invocation.
//
// roostctl dials a socket and waits for an answer, and the app on the other
// end is a GUI that can be wedged, paused in a debugger, or mid-relaunch.
// `shed attach` calls this on its way to doing something else, so a far side
// that never answers has to become a failed call rather than a CLI that sits
// there. Ten seconds is generous for a local UDS round trip and short enough
// that a person notices it as a pause rather than as a hang.
//
// A var rather than a const SOLELY so the timeout's own test can shorten it —
// proving the ceiling fires takes a run that reaches it, and a ten-second unit
// test is a test nobody runs. Nothing in production writes to it.
var ExecTimeout = 10 * time.Second

// waitDelay is how long Exec waits for the output pipes to close after the
// child has been killed — see the WaitDelay assignment in Exec for why the
// timeout is not a bound without it. Short, because by this point the answer
// is already lost; the only thing left is to stop waiting for it.
const waitDelay = time.Second

// Result is one roostctl invocation's raw outcome. A non-zero ExitCode is
// DATA, not an error: the caller reads roost's refusal out of it.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// RunFunc is the exec seam. A nil Client.Run means Exec.
type RunFunc func(ctx context.Context, bin string, args []string) (Result, error)

// Client drives one local roost app.
//
// The zero value is usable and is what a caller wants: `roostctl` off PATH,
// through the real exec. Both fields exist for tests (a shim binary, or no
// binary at all).
type Client struct {
	// Bin is the binary to exec. Empty means DefaultBin, resolved on PATH.
	Bin string
	// Run is the exec seam. Nil means Exec.
	Run RunFunc
}

// Available reports whether a local roost app is running and reachable through
// roostctl — the gate a caller uses to choose the roost path over its fallback.
//
// **Total, and deliberately a bool.** Every way this can come out negative —
// no roostctl on PATH, no app running, an app that answers with something
// unreadable, a roostctl that hangs — means the same thing to the caller: do
// not take the roost path. Returning an error here would hand the caller a
// value it has no decision to make about, and the shapes it would have to
// distinguish are exactly the ones that must NOT become a user-visible failure
// on a machine that simply has no roost.
//
// Two calls, not one. `identify` proves a UI is listening and answering;
// `host status` proves it serves the host family this package's other methods
// need. Not every roost build does: roost's own `SidebarDumpResult` notes that
// the Swift Mac app answers `app.sidebar_dump` and never emits `hosts`, so an
// app that identifies cleanly and has no host sessions is a real shape. Either
// way an app that cannot answer the host family is one shed cannot drive
// through here, and finding that out at the gate is cheaper than finding it
// out halfway through attaching.
func (c *Client) Available(ctx context.Context) bool {
	identify, err := c.Identify(ctx)
	if err != nil {
		return false
	}
	// A `{}` that decodes cleanly is not an identify document. roost's own
	// field is a plain String, so a real reply always names its socket.
	if identify.SocketPath == "" {
		return false
	}
	_, err = c.HostStatus(ctx, "")
	return err == nil
}

// Identify runs `roostctl identify --json`.
func (c *Client) Identify(ctx context.Context) (Identify, error) {
	var out Identify
	err := c.call(ctx, []string{"identify", "--json"}, &out)
	return out, err
}

// HostList runs `roostctl host list --json`.
//
// **The registry alone.** roost answers this with the saved hosts and their
// targets and nothing else — State, Generation, Reason and Tabs come back
// zero. That is roost's own choice (merging two ops' answers under one key
// would leave a reader unable to say which it read), and HostStatus is the op
// that owns state. The rows are HostStatus values only because the two share
// their registry half.
func (c *Client) HostList(ctx context.Context) ([]HostStatus, error) {
	var out hostsResult
	if err := c.call(ctx, []string{"host", "list", "--json"}, &out); err != nil {
		return nil, err
	}
	return out.Hosts, nil
}

// HostAdd runs `roostctl host add --label … --target … [--verify] --json`.
//
// Registry-only unless verify is set: saving does not reach the target at all,
// so a host that is merely down saves cleanly. What it always checks is that
// the target string MEANS something. `--verify` goes further and asks the
// target for a `session.identify` first, refusing to save an unreachable or
// incompatible session — the same bar the app's "Add & Connect" applies.
func (c *Client) HostAdd(ctx context.Context, label, target string, verify bool) (Host, error) {
	args := []string{"host", "add", "--label", label, "--target", target}
	if verify {
		args = append(args, "--verify")
	}
	args = append(args, "--json")

	var out hostResult
	if err := c.call(ctx, args, &out); err != nil {
		return Host{}, err
	}
	return out.Host, nil
}

// HostConnect runs `roostctl host connect --id … --json`.
//
// It displaces nobody, and on localhost it starts the session if one is not
// already running. It returns once the attempt is UNDER WAY — poll HostStatus
// for the settled state, watching Generation rather than Reason, since two
// consecutive attempts can fail identically.
func (c *Client) HostConnect(ctx context.Context, id string) (HostConnection, error) {
	var out HostConnection
	err := c.call(ctx, []string{"host", "connect", "--id", id, "--json"}, &out)
	return out, err
}

// HostStatus runs `roostctl host status [--id …] --json`.
//
// An empty id asks for every saved host; naming one narrows it. A named host
// that does not exist comes back as an empty list, not as an error — roost
// answers the op, and "no such host" is a thing the answer says rather than a
// refusal.
func (c *Client) HostStatus(ctx context.Context, id string) ([]HostStatus, error) {
	args := []string{"host", "status"}
	if id != "" {
		args = append(args, "--id", id)
	}
	args = append(args, "--json")

	var out hostsResult
	if err := c.call(ctx, args, &out); err != nil {
		return nil, err
	}
	return out.Hosts, nil
}

// TabFocus runs `roostctl tab focus --tab <ref> --json`.
//
// ref is a bare tab id, or the `h<incarnation>.<id>` spelling that selects
// (and attaches) a connected host's tab — see SidebarHostTab.Key, which is
// where the host form comes from.
func (c *Client) TabFocus(ctx context.Context, ref string) error {
	if !tabRefPattern.MatchString(ref) {
		return &InvalidTabRefError{Ref: ref}
	}
	return c.call(ctx, []string{"tab", "focus", "--tab", ref, "--json"}, nil)
}

// SidebarDump runs `roostctl rpc app.sidebar_dump --json`.
//
// Through `rpc` rather than a named verb because roostctl has no `sidebar`
// subcommand — `rpc` calls any op by name, which is exactly what it is for.
// Params are omitted, which roost reads as `{}`.
func (c *Client) SidebarDump(ctx context.Context) (SidebarDump, error) {
	var out SidebarDump
	err := c.call(ctx, []string{"rpc", "app.sidebar_dump", "--json"}, &out)
	return out, err
}

// Activate runs `roostctl rpc app.activate --json` — bring the roost window
// forward. Empty params, empty result; nothing to decode.
func (c *Client) Activate(ctx context.Context) error {
	return c.call(ctx, []string{"rpc", "app.activate", "--json"}, nil)
}

// tabRefPattern is what `tab focus --tab` accepts, copied from roost's own
// spelling: a bare id (which may be negative — roost's synthetic ids are), or
// the host-qualified `h<incarnation>.<id>` form.
//
// Checked here rather than left to the far side because the two failures are
// worth telling apart: a malformed reference is this side's bug and comes back
// as an InvalidTabRefError, where letting roostctl refuse it would arrive as a
// usage exit that looks exactly like every other argv mistake.
var tabRefPattern = regexp.MustCompile(`^(0|-?[1-9][0-9]*|h[1-9][0-9]*\.(0|-?[1-9][0-9]*))$`)

// call runs one roostctl invocation and decodes its stdout into out (nil to
// discard it), turning each way it can go wrong into its own error type.
func (c *Client) call(ctx context.Context, args []string, out any) error {
	run := c.Run
	if run == nil {
		run = Exec
	}
	bin := c.Bin
	if bin == "" {
		bin = DefaultBin
	}

	res, err := run(ctx, bin, args)
	if err != nil {
		return &ExecError{
			Bin:  bin,
			Args: args,
			// exec.ErrWaitDelay counts as a timeout too. It means the child
			// exited but a GRANDCHILD still held the output pipes open past
			// WaitDelay, so the call was cut short on a clock rather than on
			// an answer — which is what a caller branching on TimedOut needs
			// to know. Checking only context.DeadlineExceeded reports that
			// case as an ordinary exec failure. (sol review finding.)
			TimedOut: errors.Is(err, context.DeadlineExceeded) || errors.Is(err, exec.ErrWaitDelay),
			Err:      err,
		}
	}
	if res.ExitCode != 0 {
		if code, message, ok := parseErrorEnvelope(res.Stderr); ok {
			return &ServerError{Code: code, Message: message, ExitCode: res.ExitCode, Args: args}
		}
		return &ExitError{Bin: bin, Args: args, ExitCode: res.ExitCode, Stderr: string(res.Stderr)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(res.Stdout, out); err != nil {
		return &DecodeError{Args: args, Stdout: res.Stdout, Err: err}
	}
	return nil
}

// Exec is the default RunFunc: run the binary with a ExecTimeout ceiling and
// collect both streams.
//
// A non-zero exit is returned as a Result, never as an error — the caller's
// whole job is to read roost's refusal out of it. An error here means the
// process could not be run or did not finish: a missing binary, a broken
// exec, a cancelled parent, or the timeout.
func Exec(ctx context.Context, bin string, args []string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, ExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// roostctl reads no stdin in any invocation this package makes (`rpc`
	// would, with a `-` params argument, and nothing here passes one). Handing
	// it a closed stdin rather than inheriting shed's keeps it from ever
	// consuming a byte meant for the terminal.
	cmd.Stdin = nil
	// **Without this, the timeout does not actually bound the call.**
	// CommandContext kills the child when the deadline passes, but Wait then
	// blocks until the output pipes close — and a grandchild that inherited
	// them holds them open for as long as IT lives. A wedged roostctl that had
	// forked anything would sail straight past ExecTimeout on that second
	// wait, which is the exact hang this ceiling exists to prevent (and is
	// reproducible: the timeout test's shim sleeps in a subprocess, and
	// without a WaitDelay the call returns in 30s rather than in the quarter
	// second the deadline asked for). A second, short bound on the I/O wait is
	// what makes the first one real.
	cmd.WaitDelay = waitDelay

	runErr := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}

	// The deadline is checked BEFORE the exit status, because CommandContext
	// kills the child on expiry and a killed child looks like an ordinary
	// signal death. Reading that as an exit code would turn a wedged app into
	// "roostctl exited -1", which is the one diagnosis that names neither the
	// timeout nor the app.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, runErr
	}
	return res, nil
}

// parseErrorEnvelope reads roostctl's `{"error":{"code","message"}}` failure
// document off stderr.
//
// roostctl writes exactly one such document under `--json`, so the whole
// buffer is tried first. The line scan behind it is for the case where
// something else reached the same stream — a Rust panic message, a dynamic
// linker warning — which would otherwise turn a perfectly readable refusal
// into an opaque ExitError.
func parseErrorEnvelope(stderr []byte) (code, message string, ok bool) {
	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decode := func(b []byte) bool {
		envelope.Error = nil
		if err := json.Unmarshal(b, &envelope); err != nil {
			return false
		}
		return envelope.Error != nil
	}

	if decode(bytes.TrimSpace(stderr)) {
		return envelope.Error.Code, envelope.Error.Message, true
	}
	lines := bytes.Split(stderr, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		if decode(line) {
			return envelope.Error.Code, envelope.Error.Message, true
		}
	}
	return "", "", false
}

// The four ways a call can fail, as four types. A caller needs to tell "roost
// said no" from "roostctl is broken" — the first is a decision, the second is
// a bug report — so nothing here collapses into a fmt.Errorf.

// ExecError is roostctl not running at all: the binary is missing, the exec
// failed, the parent context was cancelled, or ExecTimeout expired.
//
// Unwraps, so `errors.Is(err, exec.ErrNotFound)` answers "no roostctl on this
// machine" and `errors.Is(err, context.DeadlineExceeded)` answers "it hung".
type ExecError struct {
	Bin      string
	Args     []string
	TimedOut bool
	Err      error
}

func (e *ExecError) Error() string {
	if e.TimedOut {
		return fmt.Sprintf("%s %s did not answer within %s", e.Bin, strings.Join(e.Args, " "), ExecTimeout)
	}
	return fmt.Sprintf("running %s %s: %v", e.Bin, strings.Join(e.Args, " "), e.Err)
}

func (e *ExecError) Unwrap() error { return e.Err }

// ServerError is roost's own refusal: the `{"error":{"code","message"}}`
// envelope roostctl prints on stderr under `--json`. Code is roost's stable
// kebab-case code, verbatim — this is the error a caller branches on.
type ServerError struct {
	Code     string
	Message  string
	ExitCode int
	Args     []string
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("roostctl %s: %s (%s)", strings.Join(e.Args, " "), e.Message, e.Code)
}

// ExitError is a non-zero exit with no readable error envelope behind it —
// roostctl failed in a way it did not describe. Distinct from ServerError on
// purpose: there is no code to branch on, so the stderr tail is the diagnosis.
type ExitError struct {
	Bin      string
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *ExitError) Error() string {
	stderr := strings.TrimSpace(e.Stderr)
	if stderr == "" {
		stderr = "(no stderr)"
	}
	return fmt.Sprintf("%s %s exited %d: %s", e.Bin, strings.Join(e.Args, " "), e.ExitCode, stderr)
}

// DecodeError is a SUCCESSFUL roostctl run whose stdout is not the document
// this package expected: roostctl is there, roost answered, and the two sides
// disagree about the shape. That is a version skew or a bug, never a decision
// the caller can make — which is exactly why it must not arrive looking like
// a refusal.
type DecodeError struct {
	Args   []string
	Stdout []byte
	Err    error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("decoding `roostctl %s`: %v (output: %s)",
		strings.Join(e.Args, " "), e.Err, truncate(string(e.Stdout), 200))
}

func (e *DecodeError) Unwrap() error { return e.Err }

// InvalidTabRefError is a tab reference that does not match what
// `tab focus --tab` accepts. Caught before the exec — see tabRefPattern.
type InvalidTabRefError struct {
	Ref string
}

func (e *InvalidTabRefError) Error() string {
	return fmt.Sprintf("%q is not a tab reference: want a tab id or the h<host>.<id> spelling", e.Ref)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
