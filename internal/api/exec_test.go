package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/authtoken"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmutil"
)

// execFakeBackend is routesFakeBackend with a configurable shed and Exec. A
// nil shed is "not found".
type execFakeBackend struct {
	routesFakeBackend
	shed *config.Shed
	exec func(ctx context.Context, name string, opts backend.ExecOptions) error

	mu    sync.Mutex
	calls int
}

func (b *execFakeBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	if b.shed == nil || b.shed.Name != name {
		return nil, config.ErrShedNotFoundSentinel
	}
	sh := *b.shed
	return &sh, nil
}

func (b *execFakeBackend) Exec(ctx context.Context, name string, opts backend.ExecOptions) error {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	if b.exec == nil {
		return errors.New("exec not configured")
	}
	return b.exec(ctx, name, opts)
}

func (b *execFakeBackend) execCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func runningShed() *config.Shed {
	return &config.Shed{Name: "box", Status: config.StatusRunning, LandingDir: "/home/shed/proj"}
}

func newExecTestServer(b backend.Backend, exec *config.ExecConfig) *Server {
	return NewServer(b, &config.ServerConfig{Name: "test-server", HTTPPort: 8080, Exec: exec}, "", nil, nil)
}

type execResult struct {
	status      int
	contentType string
	body        []byte
	data        map[string]string // "stdout"/"stderr" -> decoded concatenation
	frames      []map[string]any
	exit        *config.ExecExitFrame
}

func postExec(t *testing.T, s *Server, name string, body any) execResult {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case string:
		raw = []byte(b)
	default:
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sheds/"+name+"/exec", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, req)
	return parseExecResponse(t, rr.Code, rr.Header().Get("Content-Type"), rr.Body.Bytes())
}

func parseExecResponse(t *testing.T, status int, ct string, body []byte) execResult {
	t.Helper()
	res := execResult{status: status, contentType: ct, body: body, data: map[string]string{}}
	if status != http.StatusOK {
		return res
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var f map[string]any
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("frame %q is not JSON: %v", sc.Text(), err)
		}
		res.frames = append(res.frames, f)
		switch f["t"] {
		case config.ExecFrameStdout, config.ExecFrameStderr:
			d, err := base64.StdEncoding.DecodeString(f["d"].(string))
			if err != nil {
				t.Fatalf("frame data is not base64: %v", err)
			}
			res.data[f["t"].(string)] += string(d)
		case config.ExecFrameExit:
			if res.exit != nil {
				t.Fatal("two exit frames")
			}
			var e config.ExecExitFrame
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatal(err)
			}
			res.exit = &e
		default:
			t.Fatalf("unknown frame type %v", f["t"])
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return res
}

func apiErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e config.APIError
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("body %q is not an API error: %v", body, err)
	}
	return e.Error.Code
}

func TestExecFramesAndOptions(t *testing.T) {
	var got backend.ExecOptions
	b := &execFakeBackend{shed: runningShed(), exec: func(_ context.Context, name string, opts backend.ExecOptions) error {
		got = opts
		opts.OnStarted()
		_, _ = opts.Stdout.Write([]byte("hello "))
		_, _ = opts.Stderr.Write([]byte("oops"))
		_, _ = opts.Stdout.Write([]byte("world\x00\xff"))
		return nil
	}}
	res := postExec(t, newExecTestServer(b, nil), "box", config.ExecRequest{
		Argv: []string{"bash", "-lc", "echo hi"},
		Env:  map[string]string{"B": "2", "A": "1"},
	})
	if res.status != http.StatusOK {
		t.Fatalf("status %d, body %s", res.status, res.body)
	}
	if res.contentType != "application/x-ndjson" {
		t.Errorf("content type %q", res.contentType)
	}
	wantOrder := []string{"stdout", "stderr", "stdout", "exit"}
	if len(res.frames) != len(wantOrder) {
		t.Fatalf("frames %v", res.frames)
	}
	for i, w := range wantOrder {
		if res.frames[i]["t"] != w {
			t.Errorf("frame %d is %v, want %s", i, res.frames[i]["t"], w)
		}
	}
	if res.data["stdout"] != "hello world\x00\xff" || res.data["stderr"] != "oops" {
		t.Errorf("data %q", res.data)
	}
	if res.exit.Code != 0 || res.exit.TimedOut || res.exit.Truncated {
		t.Errorf("exit %+v", res.exit)
	}
	if strings.Join(got.Cmd, " ") != "bash -lc echo hi" {
		t.Errorf("argv %q", got.Cmd)
	}
	if strings.Join(got.Env, ",") != "A=1,B=2" {
		t.Errorf("env %q", got.Env)
	}
	if got.WorkingDir != "/home/shed/proj" {
		t.Errorf("working dir %q, want the landing dir", got.WorkingDir)
	}
	if got.Stdin != nil {
		t.Error("stdin set without a stdin field")
	}
	if got.TTY {
		t.Error("TTY set")
	}
}

func TestExecCwdOverridesLandingDir(t *testing.T) {
	var dir string
	b := &execFakeBackend{shed: runningShed(), exec: func(_ context.Context, _ string, opts backend.ExecOptions) error {
		dir = opts.WorkingDir
		return nil
	}}
	res := postExec(t, newExecTestServer(b, nil), "box", config.ExecRequest{Argv: []string{"pwd"}, Cwd: "/tmp"})
	if res.status != http.StatusOK || dir != "/tmp" {
		t.Fatalf("status %d, dir %q", res.status, dir)
	}
}

func TestExecNonZeroExit(t *testing.T) {
	b := &execFakeBackend{shed: runningShed(), exec: func(_ context.Context, _ string, opts backend.ExecOptions) error {
		opts.OnStarted()
		return &vmutil.ExitError{Code: 3}
	}}
	res := postExec(t, newExecTestServer(b, nil), "box", config.ExecRequest{Argv: []string{"false"}})
	if res.status != http.StatusOK || res.exit == nil || res.exit.Code != 3 {
		t.Fatalf("status %d exit %+v body %s", res.status, res.exit, res.body)
	}
}

func TestExecStdinDelivered(t *testing.T) {
	payload := []byte("line one\n\x00binary\xfe")
	b := &execFakeBackend{shed: runningShed(), exec: func(_ context.Context, _ string, opts backend.ExecOptions) error {
		opts.OnStarted()
		in, err := io.ReadAll(opts.Stdin)
		if err != nil {
			return err
		}
		_, _ = opts.Stdout.Write(in)
		return nil
	}}
	res := postExec(t, newExecTestServer(b, nil), "box", config.ExecRequest{
		Argv:  []string{"cat"},
		Stdin: base64.StdEncoding.EncodeToString(payload),
	})
	if res.status != http.StatusOK || res.data["stdout"] != string(payload) {
		t.Fatalf("status %d stdout %q", res.status, res.data["stdout"])
	}
}

func TestExecTimeoutGives124(t *testing.T) {
	b := &execFakeBackend{shed: runningShed(), exec: func(ctx context.Context, _ string, opts backend.ExecOptions) error {
		opts.OnStarted()
		<-ctx.Done()
		return ctx.Err()
	}}
	start := time.Now()
	res := postExec(t, newExecTestServer(b, nil), "box", config.ExecRequest{Argv: []string{"sleep", "30"}, TimeoutSeconds: 1})
	if res.status != http.StatusOK || res.exit == nil {
		t.Fatalf("status %d body %s", res.status, res.body)
	}
	if res.exit.Code != 124 || !res.exit.TimedOut {
		t.Errorf("exit %+v, want code 124 timed_out", res.exit)
	}
	if res.exit.DurationMS < 1000 || time.Since(start) > 10*time.Second {
		t.Errorf("duration %dms", res.exit.DurationMS)
	}
}

func TestExecTimeoutBeforeStartIsJSON(t *testing.T) {
	b := &execFakeBackend{shed: runningShed(), exec: func(ctx context.Context, _ string, _ backend.ExecOptions) error {
		<-ctx.Done() // a dial that never completes
		return ctx.Err()
	}}
	res := postExec(t, newExecTestServer(b, nil), "box", config.ExecRequest{Argv: []string{"true"}, TimeoutSeconds: 1})
	if res.status != http.StatusGatewayTimeout || !strings.HasPrefix(res.contentType, "application/json") {
		t.Fatalf("status %d content type %q", res.status, res.contentType)
	}
}

// recordingWriter tees everything the handler writes, so a test can see what
// went out after the client stopped reading.
type recordingWriter struct {
	http.ResponseWriter
	mu  sync.Mutex
	buf bytes.Buffer
}

func (rw *recordingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	rw.buf.Write(p)
	rw.mu.Unlock()
	return rw.ResponseWriter.Write(p)
}

func (rw *recordingWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *recordingWriter) written() string {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.buf.String()
}

func TestExecClientDisconnectCancelsBackend(t *testing.T) {
	cancelled := make(chan struct{})
	b := &execFakeBackend{shed: runningShed(), exec: func(ctx context.Context, _ string, opts backend.ExecOptions) error {
		opts.OnStarted()
		_, _ = opts.Stdout.Write([]byte("started\n"))
		select {
		case <-ctx.Done():
			close(cancelled)
			// The agent's reader can deliver output after a cancel; it
			// must be dropped, not written.
			_, _ = opts.Stdout.Write([]byte("late"))
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return errors.New("never cancelled")
		}
	}}
	s := newExecTestServer(b, nil)
	router := s.Router()
	rec := &recordingWriter{}
	handlerDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		rec.ResponseWriter = w
		router.ServeHTTP(rec, r)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, _ := json.Marshal(config.ExecRequest{Argv: []string{"sleep", "600"}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/sheds/box/exec", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, `"stdout"`) {
		t.Fatalf("first frame %q, %v", line, err)
	}
	cancel()
	_ = resp.Body.Close()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the backend context was not cancelled by the client disconnect")
	}
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}
	out := rec.written()
	if strings.Contains(out, `"exit"`) {
		t.Errorf("an exit frame was written after the disconnect: %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("expected only the first frame, got %q", out)
	}
}

func TestExecTruncationKeepsExitCode(t *testing.T) {
	b := &execFakeBackend{shed: runningShed(), exec: func(_ context.Context, _ string, opts backend.ExecOptions) error {
		opts.OnStarted()
		for range 3 {
			n, err := opts.Stdout.Write([]byte("abcdef"))
			if err != nil || n != 6 {
				return errors.New("a write past the budget must report success")
			}
		}
		n, err := opts.Stderr.Write([]byte("more"))
		if err != nil || n != 4 {
			return errors.New("a stderr write past the budget must report success")
		}
		return &vmutil.ExitError{Code: 7}
	}}
	res := postExec(t, newExecTestServer(b, nil), "box", config.ExecRequest{Argv: []string{"yes"}, MaxOutputBytes: 10})
	if res.status != http.StatusOK || res.exit == nil {
		t.Fatalf("status %d body %s", res.status, res.body)
	}
	if got := res.data["stdout"] + res.data["stderr"]; got != "abcdefabcd" {
		t.Errorf("output %q, want the first 10 bytes", got)
	}
	if !res.exit.Truncated || res.exit.Code != 7 {
		t.Errorf("exit %+v, want truncated with code 7", res.exit)
	}
}

func TestExecOutputExactlyAtBudgetIsNotTruncated(t *testing.T) {
	b := &execFakeBackend{shed: runningShed(), exec: func(_ context.Context, _ string, opts backend.ExecOptions) error {
		_, _ = opts.Stdout.Write([]byte("0123456789"))
		return nil
	}}
	res := postExec(t, newExecTestServer(b, nil), "box", config.ExecRequest{Argv: []string{"x"}, MaxOutputBytes: 10})
	if res.exit == nil || res.exit.Truncated || res.data["stdout"] != "0123456789" {
		t.Fatalf("exit %+v data %q", res.exit, res.data)
	}
}

func TestExecShedStates(t *testing.T) {
	tests := []struct {
		name     string
		shed     *config.Shed
		target   string
		wantCode int
		wantAPI  string
	}{
		{"stopped", &config.Shed{Name: "box", Status: config.StatusStopped}, "box", http.StatusConflict, config.ErrShedNotRunning},
		{"starting", &config.Shed{Name: "box", Status: config.StatusStarting}, "box", http.StatusConflict, config.ErrShedNotRunning},
		{"missing", nil, "box", http.StatusNotFound, config.ErrShedNotFound},
		{"not a shed name", runningShed(), "..", http.StatusNotFound, config.ErrShedNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &execFakeBackend{shed: tt.shed, exec: func(context.Context, string, backend.ExecOptions) error { return nil }}
			res := postExec(t, newExecTestServer(b, nil), tt.target, config.ExecRequest{Argv: []string{"true"}})
			if res.status != tt.wantCode {
				t.Fatalf("status %d, want %d (body %s)", res.status, tt.wantCode, res.body)
			}
			if got := apiErrorCode(t, res.body); got != tt.wantAPI {
				t.Errorf("code %s, want %s", got, tt.wantAPI)
			}
			if b.execCalls() != 0 {
				t.Error("the backend ran a command for a shed that is not running")
			}
		})
	}
}

func TestExecPreStartErrorsAreJSON(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantAPI  string
	}{
		{"dial failure", errors.New("failed to connect to console port: connection refused"), http.StatusInternalServerError, config.ErrBackendError},
		{"backend says not running", config.ErrShedNotRunningSentinel, http.StatusConflict, config.ErrShedNotRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &execFakeBackend{shed: runningShed(), exec: func(context.Context, string, backend.ExecOptions) error { return tt.err }}
			res := postExec(t, newExecTestServer(b, nil), "box", config.ExecRequest{Argv: []string{"true"}})
			if res.status != tt.wantCode || !strings.HasPrefix(res.contentType, "application/json") {
				t.Fatalf("status %d content type %q, want %d JSON", res.status, res.contentType, tt.wantCode)
			}
			if got := apiErrorCode(t, res.body); got != tt.wantAPI {
				t.Errorf("code %s, want %s", got, tt.wantAPI)
			}
		})
	}
}

func TestExecLostResultHasNoExitFrame(t *testing.T) {
	b := &execFakeBackend{shed: runningShed(), exec: func(_ context.Context, _ string, opts backend.ExecOptions) error {
		opts.OnStarted()
		_, _ = opts.Stdout.Write([]byte("partial"))
		return errors.New("connection closed before exit code received")
	}}
	res := postExec(t, newExecTestServer(b, nil), "box", config.ExecRequest{Argv: []string{"x"}})
	if res.status != http.StatusOK {
		t.Fatalf("status %d", res.status)
	}
	if res.exit != nil {
		t.Errorf("a lost result must not carry an exit frame: %+v", res.exit)
	}
	if res.data["stdout"] != "partial" {
		t.Errorf("stdout %q", res.data["stdout"])
	}
}

func TestExecStartedWithNoOutputCommitsOnStart(t *testing.T) {
	b := &execFakeBackend{shed: runningShed(), exec: func(_ context.Context, _ string, opts backend.ExecOptions) error {
		opts.OnStarted()
		return errors.New("connection closed before exit code received")
	}}
	res := postExec(t, newExecTestServer(b, nil), "box", config.ExecRequest{Argv: []string{"x"}})
	if res.status != http.StatusOK || len(res.frames) != 0 {
		t.Fatalf("status %d frames %v: a started command is a 200 even when its result is lost", res.status, res.frames)
	}
}

func TestExecValidation(t *testing.T) {
	big := base64.StdEncoding.EncodeToString(make([]byte, 101))
	tests := []struct {
		name     string
		body     any
		wantCode int
		wantAPI  string
	}{
		{"empty argv", config.ExecRequest{}, 400, config.ErrInvalidRequest},
		{"NUL in argv", config.ExecRequest{Argv: []string{"echo", "a\x00b"}}, 400, config.ErrInvalidRequest},
		{"too many argv", config.ExecRequest{Argv: make([]string, 1025)}, 400, config.ErrInvalidRequest},
		{"bad env key", config.ExecRequest{Argv: []string{"env"}, Env: map[string]string{"1BAD": "x"}}, 400, config.ErrInvalidRequest},
		{"env key with =", config.ExecRequest{Argv: []string{"env"}, Env: map[string]string{"A=B": "x"}}, 400, config.ErrInvalidRequest},
		{"NUL in env value", config.ExecRequest{Argv: []string{"env"}, Env: map[string]string{"A": "x\x00"}}, 400, config.ErrInvalidRequest},
		{"relative cwd", config.ExecRequest{Argv: []string{"pwd"}, Cwd: "tmp"}, 400, config.ErrInvalidRequest},
		{"unknown field", `{"argv":["true"],"user":"root"}`, 400, config.ErrInvalidRequest},
		{"two objects", `{"argv":["true"]}{"argv":["false"]}`, 400, config.ErrInvalidRequest},
		{"not JSON", `argv=true`, 400, config.ErrInvalidRequest},
		{"negative timeout", config.ExecRequest{Argv: []string{"true"}, TimeoutSeconds: -1}, 400, config.ErrInvalidRequest},
		{"timeout over max", config.ExecRequest{Argv: []string{"true"}, TimeoutSeconds: 3601}, 400, config.ErrInvalidRequest},
		{"negative max output", config.ExecRequest{Argv: []string{"true"}, MaxOutputBytes: -1}, 400, config.ErrInvalidRequest},
		{"max output over ceiling", config.ExecRequest{Argv: []string{"true"}, MaxOutputBytes: 256<<20 + 1}, 400, config.ErrInvalidRequest},
		{"stdin not base64", config.ExecRequest{Argv: []string{"cat"}, Stdin: "%%%"}, 400, config.ErrInvalidRequest},
		{"stdin over max", config.ExecRequest{Argv: []string{"cat"}, Stdin: big}, 413, config.ErrRequestTooLarge},
		{"body over limit", `{"argv":["cat"],"stdin":"` + strings.Repeat("A", 2<<20) + `"}`, 413, config.ErrRequestTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &execFakeBackend{shed: runningShed(), exec: func(context.Context, string, backend.ExecOptions) error { return nil }}
			s := newExecTestServer(b, &config.ExecConfig{MaxStdinBytes: 100})
			res := postExec(t, s, "box", tt.body)
			if res.status != tt.wantCode {
				t.Fatalf("status %d, want %d (body %s)", res.status, tt.wantCode, res.body)
			}
			if got := apiErrorCode(t, res.body); got != tt.wantAPI {
				t.Errorf("code %s, want %s", got, tt.wantAPI)
			}
			if b.execCalls() != 0 {
				t.Error("the backend ran an invalid request")
			}
		})
	}
}

func TestExecStdinAtMaxIsAccepted(t *testing.T) {
	b := &execFakeBackend{shed: runningShed(), exec: func(_ context.Context, _ string, opts backend.ExecOptions) error {
		in, _ := io.ReadAll(opts.Stdin)
		if len(in) != 100 {
			return &vmutil.ExitError{Code: 1}
		}
		return nil
	}}
	s := newExecTestServer(b, &config.ExecConfig{MaxStdinBytes: 100})
	res := postExec(t, s, "box", config.ExecRequest{Argv: []string{"cat"}, Stdin: base64.StdEncoding.EncodeToString(make([]byte, 100))})
	if res.status != http.StatusOK || res.exit == nil || res.exit.Code != 0 {
		t.Fatalf("status %d exit %+v body %s", res.status, res.exit, res.body)
	}
}

func TestExecDefaultTimeoutCappedByMaxTimeout(t *testing.T) {
	var deadline time.Time
	b := &execFakeBackend{shed: runningShed(), exec: func(ctx context.Context, _ string, _ backend.ExecOptions) error {
		deadline, _ = ctx.Deadline()
		return nil
	}}
	s := newExecTestServer(b, &config.ExecConfig{MaxTimeout: config.Duration(5 * time.Second)})
	if res := postExec(t, s, "box", config.ExecRequest{Argv: []string{"true"}}); res.status != http.StatusOK {
		t.Fatalf("status %d", res.status)
	}
	if left := time.Until(deadline); left > 5*time.Second || left < 3*time.Second {
		t.Errorf("deadline in %s, want about 5s (the max, below the 60s default)", left)
	}
	res := postExec(t, s, "box", config.ExecRequest{Argv: []string{"true"}, TimeoutSeconds: 6})
	if res.status != http.StatusBadRequest {
		t.Errorf("timeout over a configured max: status %d, want 400", res.status)
	}
}

func TestExecConcurrencyLimit(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	b := &execFakeBackend{shed: runningShed(), exec: func(ctx context.Context, _ string, opts backend.ExecOptions) error {
		opts.OnStarted()
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}
	s := newExecTestServer(b, &config.ExecConfig{MaxConcurrentPerShed: 2})
	ts := httptest.NewServer(s.Router())
	defer ts.Close()

	post := func() *http.Response {
		body, _ := json.Marshal(config.ExecRequest{Argv: []string{"sleep", "1"}})
		resp, err := http.Post(ts.URL+"/api/sheds/box/exec", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	var held []*http.Response
	for range 2 {
		held = append(held, post())
		<-started
	}
	resp := post()
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || apiErrorCode(t, body) != config.ErrExecLimit {
		t.Fatalf("third exec: status %d body %s, want 429 EXEC_LIMIT", resp.StatusCode, body)
	}

	close(release)
	for _, r := range held {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	// The slots are released when the execs end.
	resp = post()
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("exec after release: status %d", resp.StatusCode)
	}
	s.execMu.Lock()
	left := len(s.execInFlight)
	s.execMu.Unlock()
	if left != 0 {
		t.Errorf("%d sheds still hold exec slots", left)
	}
}

// TestExecNeedsControlScope: in token mode exec is a control-scope route, so
// a credentials token (the host-agent's) is refused before the handler runs.
func TestExecNeedsControlScope(t *testing.T) {
	store := authtoken.NewStore()
	control, _, err := store.Mint("SHA256:test", authtoken.ScopeControl, authtoken.ClientCLI, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	creds, _, err := store.Mint("SHA256:test", authtoken.ScopeCredentials, authtoken.ClientHostAgent, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b := &execFakeBackend{shed: runningShed(), exec: func(context.Context, string, backend.ExecOptions) error { return nil }}
	s := NewServer(b, &config.ServerConfig{Name: "t", Auth: &config.AuthConfig{Mode: config.AuthModeToken}}, "", nil, nil)
	s.SetTokenStore(store)
	router := s.Router()

	do := func(token string) (int, []byte) {
		body, _ := json.Marshal(config.ExecRequest{Argv: []string{"true"}})
		req := httptest.NewRequest(http.MethodPost, "/api/sheds/box/exec", bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr.Code, rr.Body.Bytes()
	}
	if code, body := do(creds); code != http.StatusForbidden || apiErrorCode(t, body) != "FORBIDDEN" {
		t.Errorf("credentials token: %d %s, want 403 FORBIDDEN", code, body)
	}
	if code, _ := do(""); code != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", code)
	}
	if b.execCalls() != 0 {
		t.Fatal("exec ran for a refused caller")
	}
	if code, _ := do(control); code != http.StatusOK {
		t.Errorf("control token: %d, want 200", code)
	}
}

func TestServerFeaturesAdvertiseHTTPExec(t *testing.T) {
	s := newExecTestServer(&execFakeBackend{}, nil)
	if f := s.serverFeatures(); !sliceHas(f, FeatureHTTPExec) || !sliceHas(f, FeatureOverview) {
		t.Errorf("features %v", f)
	}
}
