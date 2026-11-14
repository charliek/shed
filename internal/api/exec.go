package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmutil"
	"github.com/go-chi/chi/v5"
)

// HTTPS exec request limits that are not configurable.
const (
	execDefaultTimeout        = 60 * time.Second
	execDefaultMaxOutputBytes = 4 << 20
	execMaxOutputBytesCeiling = 256 << 20
	execMaxArgv               = 1024
	execMaxEnv                = 256
	// execMaxArgvEnvBytes caps argv plus env (keys and values) in bytes.
	execMaxArgvEnvBytes = 1 << 20
	// execBodySlack is the room a request body gets beyond its base64 stdin:
	// argv, env and the JSON around them.
	execBodySlack = 1 << 20
	// execTimeoutExitCode is the exit code a timed-out command reports, as
	// coreutils timeout(1) does.
	execTimeoutExitCode = 124
)

// execFinishWriteGrace bounds every write to the client once the command is
// over (finished, timed out or cancelled): the exit frame, a JSON refusal, and
// any output still in flight. A client that stopped reading would otherwise
// hold the handler, and its exec slot, for as long as it kept the connection
// open. While the command runs, writes have no deadline, so a long quiet
// command is never cut; a stalled client only stalls the command's output
// until its timeout. A variable so tests can shorten it.
var execFinishWriteGrace = 10 * time.Second

var execEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// execRequestError is a refusal before anything ran: the status, the API code
// and the message for writeError.
type execRequestError struct {
	status  int
	code    string
	message string
}

func badExecRequest(format string, args ...any) *execRequestError {
	return &execRequestError{http.StatusBadRequest, config.ErrInvalidRequest, fmt.Sprintf(format, args...)}
}

// execPlan is a validated exec request, ready for the backend.
type execPlan struct {
	argv      []string
	env       []string
	cwd       string
	stdin     []byte // nil: no stdin (EOF at once)
	timeout   time.Duration
	maxOutput int64
}

// handleExecShed runs one command in a running shed and streams its output.
// POST /api/sheds/{name}/exec
//
// Until the command has started (the agent has the request), every refusal is
// an ordinary JSON error: 400/413 for the request, 404 SHED_NOT_FOUND, 409
// SHED_NOT_RUNNING (exec never auto-starts a shed), 429 EXEC_LIMIT. From then
// on the response is 200 application/x-ndjson: one {"t":"stdout"|"stderr",
// "d":<base64>} line per chunk of output, flushed as it arrives, and last an
// {"t":"exit",...} line. A stream with no exit line means the result was lost.
//
// The command's context is the request's plus the timeout, so a client that
// disconnects, or a timeout, cancels it: the agent connection closes and the
// guest agent SIGHUPs the command's process group. A timeout ends with exit
// code 124 and timed_out; a disconnect writes nothing more.
func (s *Server) handleExecShed(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	name := chi.URLParam(r, "name")

	plan, reqErr := s.parseExecRequest(w, r)
	if reqErr != nil {
		writeError(w, reqErr.status, reqErr.code, reqErr.message)
		return
	}

	// Resolve the shed first so a missing shed is always 404 SHED_NOT_FOUND
	// with that code, and a stopped one is refused before anything dials.
	// A name that can't be a shed is missing, not a path to load.
	if err := config.ValidateShedName(name); err != nil {
		writeError(w, http.StatusNotFound, config.ErrShedNotFound, "shed not found: "+name)
		return
	}
	shed, err := s.backend.GetShed(r.Context(), name)
	if err != nil {
		code, errCode, msg := backendErrorFor(r.Context(), err)
		writeError(w, code, errCode, msg)
		return
	}
	if shed.Status != config.StatusRunning {
		writeError(w, http.StatusConflict, config.ErrShedNotRunning,
			fmt.Sprintf("shed %s is %s; start it before exec", name, shed.Status))
		return
	}
	if plan.cwd == "" {
		plan.cwd = shed.LandingDir
	}

	release, ok := s.acquireExecSlot(name)
	if !ok {
		writeError(w, http.StatusTooManyRequests, config.ErrExecLimit,
			fmt.Sprintf("shed %s already runs %d execs (exec.max_concurrent_per_shed)", name, s.cfg.ExecMaxConcurrentPerShed()))
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), plan.timeout)
	defer cancel()

	stream := newExecStream(w, r.Context().Done(), plan.maxOutput)
	opts := backend.ExecOptions{
		Cmd:        plan.argv,
		Env:        plan.env,
		WorkingDir: plan.cwd,
		Stdout:     execFrameWriter{stream, config.ExecFrameStdout},
		Stderr:     execFrameWriter{stream, config.ExecFrameStderr},
		OnStarted:  stream.commit,
	}
	if plan.stdin != nil {
		opts.Stdin = io.NopCloser(bytes.NewReader(plan.stdin))
	}

	err = s.backend.Exec(ctx, name, opts)
	// From here every write is bounded. This takes no lock, so it also cuts
	// short a write the output path is blocked in while holding the stream.
	stream.boundWrites(time.Now().Add(execFinishWriteGrace))

	// Every exec that reached the backend gets one exec.end audit line,
	// however it ended (exit_code -1 when there is no exit status).
	end := s.newExecEndEvent(r, name, plan.argv)
	defer func() {
		end.Truncated = stream.wasTruncated()
		end.DurationMS = time.Since(start).Milliseconds()
		s.recordAudit(end)
	}()

	exit := config.ExecExitFrame{T: config.ExecFrameExit}
	var exitErr *vmutil.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		exit.Code = exitErr.Code
	case r.Context().Err() != nil:
		// The client went away: nobody is left to read an exit frame.
		stream.abandon()
		end.Disconnected = true
		log.Printf("exec %s: client disconnected after %s; command cancelled", name, time.Since(start).Round(time.Millisecond))
		return
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		end.TimedOut = true
		if stream.refuseIfNotStarted() {
			writeError(w, http.StatusGatewayTimeout, config.ErrBackendError,
				fmt.Sprintf("exec in %s did not start within its %s timeout", name, plan.timeout))
			return
		}
		exit.Code = execTimeoutExitCode
		exit.TimedOut = true
	default:
		if stream.refuseIfNotStarted() {
			if errors.Is(err, config.ErrShedNotRunningSentinel) {
				writeError(w, http.StatusConflict, config.ErrShedNotRunning, err.Error())
				return
			}
			code, errCode, msg := backendErrorFor(r.Context(), err)
			writeError(w, code, errCode, msg)
			return
		}
		// Started, then lost: end the stream with no exit frame, which is how
		// a caller tells "result lost" from any exit code.
		stream.abandon()
		log.Printf("exec %s: result lost after start: %v", name, err)
		return
	}
	exit.DurationMS = time.Since(start).Milliseconds()
	end.ExitCode = exit.Code
	stream.finish(exit)
}

// parseExecRequest decodes and validates the body. A nil error means plan is
// ready to run.
func (s *Server) parseExecRequest(w http.ResponseWriter, r *http.Request) (*execPlan, *execRequestError) {
	maxStdin := s.cfg.ExecMaxStdinBytes()
	// Base64 grows stdin by 4/3; the slack carries argv, env and the JSON.
	limit := (maxStdin*4+2)/3 + execBodySlack
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	var req config.ExecRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, &execRequestError{http.StatusRequestEntityTooLarge, config.ErrRequestTooLarge,
				fmt.Sprintf("request body is over %d bytes", limit)}
		}
		return nil, badExecRequest("invalid request body: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, badExecRequest("invalid request body: one JSON object expected")
	}

	plan := &execPlan{argv: req.Argv}

	if len(req.Argv) == 0 {
		return nil, badExecRequest("argv is required")
	}
	if len(req.Argv) > execMaxArgv {
		return nil, badExecRequest("argv has %d elements (at most %d)", len(req.Argv), execMaxArgv)
	}
	size := 0
	for i, a := range req.Argv {
		if strings.IndexByte(a, 0) >= 0 {
			return nil, badExecRequest("argv[%d] contains a NUL byte", i)
		}
		size += len(a)
	}

	if len(req.Env) > execMaxEnv {
		return nil, badExecRequest("env has %d keys (at most %d)", len(req.Env), execMaxEnv)
	}
	for k, v := range req.Env {
		if !execEnvKeyPattern.MatchString(k) {
			return nil, badExecRequest("env key %q is not a valid name", k)
		}
		if strings.IndexByte(v, 0) >= 0 {
			return nil, badExecRequest("env value of %s contains a NUL byte", k)
		}
		size += len(k) + len(v) + 1
		plan.env = append(plan.env, k+"="+v)
	}
	sort.Strings(plan.env)
	if size > execMaxArgvEnvBytes {
		return nil, badExecRequest("argv and env are %d bytes (at most %d)", size, execMaxArgvEnvBytes)
	}

	if req.Cwd != "" {
		if strings.IndexByte(req.Cwd, 0) >= 0 || !path.IsAbs(req.Cwd) {
			return nil, badExecRequest("cwd must be an absolute path")
		}
		plan.cwd = req.Cwd
	}

	if req.Stdin != "" {
		stdin, err := base64.StdEncoding.DecodeString(req.Stdin)
		if err != nil {
			return nil, badExecRequest("stdin is not standard base64: %v", err)
		}
		if int64(len(stdin)) > maxStdin {
			return nil, &execRequestError{http.StatusRequestEntityTooLarge, config.ErrRequestTooLarge,
				fmt.Sprintf("stdin is %d bytes (at most %d, exec.max_stdin_bytes)", len(stdin), maxStdin)}
		}
		plan.stdin = stdin
	}

	maxTimeout := s.cfg.ExecMaxTimeout()
	switch {
	case req.TimeoutSeconds < 0:
		return nil, badExecRequest("timeout_seconds must not be negative")
	case req.TimeoutSeconds == 0:
		plan.timeout = min(execDefaultTimeout, maxTimeout)
	case time.Duration(req.TimeoutSeconds)*time.Second > maxTimeout:
		return nil, badExecRequest("timeout_seconds %d is over the server's exec.max_timeout (%s)", req.TimeoutSeconds, maxTimeout)
	default:
		plan.timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}

	switch {
	case req.MaxOutputBytes < 0:
		return nil, badExecRequest("max_output_bytes must not be negative")
	case req.MaxOutputBytes == 0:
		plan.maxOutput = execDefaultMaxOutputBytes
	case req.MaxOutputBytes > execMaxOutputBytesCeiling:
		return nil, badExecRequest("max_output_bytes %d is over %d", req.MaxOutputBytes, execMaxOutputBytesCeiling)
	default:
		plan.maxOutput = req.MaxOutputBytes
	}
	return plan, nil
}

// acquireExecSlot takes one of the shed's exec.max_concurrent_per_shed slots.
// ok=false means they are all taken; otherwise release must be called once.
func (s *Server) acquireExecSlot(name string) (release func(), ok bool) {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if s.execInFlight == nil {
		s.execInFlight = map[string]int{}
	}
	if s.execInFlight[name] >= s.cfg.ExecMaxConcurrentPerShed() {
		return nil, false
	}
	s.execInFlight[name]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.execMu.Lock()
			defer s.execMu.Unlock()
			if s.execInFlight[name]--; s.execInFlight[name] <= 0 {
				delete(s.execInFlight, name)
			}
		})
	}, true
}

// execStream owns the exec response. It commits the 200 lazily (when the
// command starts, or with the first frame), writes one flushed NDJSON line per
// frame under one mutex (stdout and stderr arrive from the agent's reader
// goroutine), and enforces the output budget.
//
// The agent's reader can still be delivering output after the backend's Exec
// returns on a cancelled context, so once the handler is done with the stream
// (finish, abandon, or a JSON refusal) closed is set and later writes are
// dropped: nothing may touch the ResponseWriter after the handler returns.
type execStream struct {
	mu        sync.Mutex
	w         http.ResponseWriter
	rc        *http.ResponseController
	gone      <-chan struct{} // the request's Done: the client went away
	started   bool            // the 200 and its headers are written
	closed    bool            // the handler is done; drop every later write
	remaining int64           // output budget left, in raw bytes
	truncated bool
	writeErr  error // the first failed write to the client
}

func newExecStream(w http.ResponseWriter, gone <-chan struct{}, maxOutput int64) *execStream {
	return &execStream{w: w, rc: http.NewResponseController(w), gone: gone, remaining: maxOutput}
}

// clientGoneLocked reports whether the client has disconnected, after which
// output is dropped rather than written to a dead connection.
func (s *execStream) clientGoneLocked() bool {
	select {
	case <-s.gone:
		return true
	default:
		return false
	}
}

// commit writes the 200 header if it hasn't been. It is the backend's
// OnStarted hook.
func (s *execStream) commit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.commitLocked()
	}
}

func (s *execStream) commitLocked() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	s.w.WriteHeader(http.StatusOK)
	if err := s.rc.Flush(); err != nil {
		s.writeErr = err
	}
}

// writeFrameLocked writes one NDJSON line and flushes it.
func (s *execStream) writeFrameLocked(frame any) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	line, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if _, err := s.w.Write(append(line, '\n')); err != nil {
		s.writeErr = err
		return err
	}
	if err := s.rc.Flush(); err != nil {
		s.writeErr = err
		return err
	}
	return nil
}

// data is one chunk of output. Past the budget the rest is discarded but
// reported as written, so the agent keeps reading and the real exit code
// still arrives (bounded by the timeout). A failed client write is returned,
// which ends the backend's Exec at once.
func (s *execStream) data(t string, p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	if s.closed || s.clientGoneLocked() {
		return n, nil
	}
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	if int64(len(p)) > s.remaining {
		p = p[:s.remaining]
		s.truncated = true
	}
	if len(p) == 0 {
		return n, nil
	}
	s.remaining -= int64(len(p))
	s.commitLocked()
	if err := s.writeFrameLocked(config.ExecDataFrame{T: t, D: base64.StdEncoding.EncodeToString(p)}); err != nil {
		return 0, err
	}
	return n, nil
}

// finish writes the exit frame (committing the 200 if nothing else did) and
// closes the stream.
func (s *execStream) finish(exit config.ExecExitFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.commitLocked()
	exit.Truncated = s.truncated
	_ = s.writeFrameLocked(exit)
	s.closed = true
}

// wasTruncated reports whether output went past the budget.
func (s *execStream) wasTruncated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.truncated
}

// boundWrites sets a write deadline on the response. It deliberately takes no
// lock: it must reach a write that is blocked (on a client that stopped
// reading) while holding s.mu. A deadline applies to pending writes too.
func (s *execStream) boundWrites(deadline time.Time) {
	_ = s.rc.SetWriteDeadline(deadline)
}

// abandon closes the stream with no exit frame.
func (s *execStream) abandon() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// refuseIfNotStarted closes the stream and reports true when no 200 has been
// written, so the caller may answer with a JSON error instead.
func (s *execStream) refuseIfNotStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return false
	}
	s.closed = true
	return true
}

// execFrameWriter is the io.WriteCloser the backend writes one stream's output
// to. Close is a no-op: the handler ends the stream.
type execFrameWriter struct {
	s *execStream
	t string
}

func (fw execFrameWriter) Write(p []byte) (int, error) { return fw.s.data(fw.t, p) }
func (fw execFrameWriter) Close() error                { return nil }
