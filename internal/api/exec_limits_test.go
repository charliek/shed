package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// execLimitsCases are the exec configs the /api/info exec tests run against,
// with what the server must report for each.
var execLimitsCases = []struct {
	name string
	exec *config.ExecConfig
	want config.ExecLimits
}{
	{
		name: "defaults",
		exec: nil,
		want: config.ExecLimits{
			MaxStdinBytes: 1 << 20, MaxTimeoutSeconds: 3600, DefaultTimeoutSeconds: 60,
			MaxOutputBytes: 268435456, DefaultOutputBytes: 4194304, MaxConcurrentPerShed: 8,
			MaxArgvElements: 1024, MaxEnvKeys: 256, MaxArgvEnvBytes: 1 << 20,
		},
	},
	{
		name: "configured",
		exec: &config.ExecConfig{MaxStdinBytes: 64 << 10, MaxTimeout: config.Duration(2 * time.Hour), MaxConcurrentPerShed: 3},
		want: config.ExecLimits{
			MaxStdinBytes: 64 << 10, MaxTimeoutSeconds: 7200, DefaultTimeoutSeconds: 60,
			MaxOutputBytes: 268435456, DefaultOutputBytes: 4194304, MaxConcurrentPerShed: 3,
			MaxArgvElements: 1024, MaxEnvKeys: 256, MaxArgvEnvBytes: 1 << 20,
		},
	},
	{
		// A max_timeout below 60 s is also the default.
		name: "max timeout below the default",
		exec: &config.ExecConfig{MaxTimeout: config.Duration(30 * time.Second), MaxConcurrentPerShed: 1},
		want: config.ExecLimits{
			MaxStdinBytes: 1 << 20, MaxTimeoutSeconds: 30, DefaultTimeoutSeconds: 30,
			MaxOutputBytes: 268435456, DefaultOutputBytes: 4194304, MaxConcurrentPerShed: 1,
			MaxArgvElements: 1024, MaxEnvKeys: 256, MaxArgvEnvBytes: 1 << 20,
		},
	},
	{
		// Reported in whole seconds, rounded down (59), while the route
		// applies the exact 59.9 s as the default.
		name: "max timeout fractional below the default",
		exec: &config.ExecConfig{MaxTimeout: config.Duration(59900 * time.Millisecond)},
		want: config.ExecLimits{
			MaxStdinBytes: 1 << 20, MaxTimeoutSeconds: 59, DefaultTimeoutSeconds: 59,
			MaxOutputBytes: 268435456, DefaultOutputBytes: 4194304, MaxConcurrentPerShed: 8,
			MaxArgvElements: 1024, MaxEnvKeys: 256, MaxArgvEnvBytes: 1 << 20,
		},
	},
	{
		// Reported in whole seconds, rounded down: 90 is accepted, 91 is not.
		name: "max timeout not whole seconds",
		exec: &config.ExecConfig{MaxTimeout: config.Duration(90500 * time.Millisecond)},
		want: config.ExecLimits{
			MaxStdinBytes: 1 << 20, MaxTimeoutSeconds: 90, DefaultTimeoutSeconds: 60,
			MaxOutputBytes: 268435456, DefaultOutputBytes: 4194304, MaxConcurrentPerShed: 8,
			MaxArgvElements: 1024, MaxEnvKeys: 256, MaxArgvEnvBytes: 1 << 20,
		},
	},
}

// getInfo fetches GET /api/info from h (open mode, so no certificate) and
// returns the raw members and the typed decode.
func getInfo(t *testing.T, h http.Handler) (map[string]json.RawMessage, config.ServerInfo) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/info", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/info = %d: %s", rr.Code, rr.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse /api/info: %v", err)
	}
	var info config.ServerInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode /api/info: %v", err)
	}
	return raw, info
}

// TestInfoExecLimits: /api/info carries exec with the configured values (or
// their defaults), and the exec_limits token exactly when it does.
func TestInfoExecLimits(t *testing.T) {
	for _, tc := range execLimitsCases {
		t.Run(tc.name, func(t *testing.T) {
			raw, info := getInfo(t, newExecTestServer(&execFakeBackend{}, tc.exec).Router())
			_, present := raw["exec"]
			if !present || info.Exec == nil {
				t.Fatalf("/api/info has no exec member: %s", raw["exec"])
			}
			if !info.HasFeature(FeatureExecLimits) {
				t.Fatalf("exec is present but features %v lack %s", info.Features, FeatureExecLimits)
			}
			if *info.Exec != tc.want {
				t.Errorf("exec = %+v\nwant   %+v", *info.Exec, tc.want)
			}
			// Every member is on the wire, by these names.
			var members map[string]json.Number
			if err := json.Unmarshal(raw["exec"], &members); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{
				"max_stdin_bytes", "max_timeout_seconds", "default_timeout_seconds",
				"max_output_bytes", "default_output_bytes", "max_concurrent_per_shed",
				"max_argv_elements", "max_env_keys", "max_argv_env_bytes",
			} {
				if _, ok := members[k]; !ok {
					t.Errorf("exec lacks %q: %s", k, raw["exec"])
				}
			}
			if len(members) != 9 {
				t.Errorf("exec has %d members, want 9: %s", len(members), raw["exec"])
			}
		})
	}
}

// limitsBackend is an exec backend whose behaviour the request's argv[0]
// picks: "block" holds the exec until release closes, "flood" writes argv[1]
// bytes of output, "deadline" records how long the command was given, and
// anything else writes "ok".
type limitsBackend struct {
	execFakeBackend
	started chan struct{}
	release chan struct{}

	mu       sync.Mutex
	deadline time.Time
}

func newLimitsBackend() *limitsBackend {
	b := &limitsBackend{started: make(chan struct{}, 64), release: make(chan struct{})}
	b.shed = runningShed()
	b.exec = func(ctx context.Context, _ string, opts backend.ExecOptions) error {
		opts.OnStarted()
		switch opts.Cmd[0] {
		case "block":
			b.started <- struct{}{}
			select {
			case <-b.release:
			case <-ctx.Done():
			}
		case "flood":
			n, err := strconv.Atoi(opts.Cmd[1])
			if err != nil {
				return err
			}
			_, _ = opts.Stdout.Write(bytes.Repeat([]byte("y"), n))
		case "deadline":
			dl, ok := ctx.Deadline()
			if !ok {
				return fmt.Errorf("exec context has no deadline")
			}
			b.mu.Lock()
			b.deadline = dl
			b.mu.Unlock()
		default:
			_, _ = opts.Stdout.Write([]byte("ok"))
		}
		return nil
	}
	return b
}

// TestInfoExecLimitsAreWhatTheExecRouteEnforces: for each config, every value
// /api/info reports is the one the exec route holds a request to. A request
// at each max_* is accepted and one past it is refused; a request that sets
// no timeout or output budget gets the default_* values. So the report and
// the route cannot come apart, whichever of them changes.
func TestInfoExecLimitsAreWhatTheExecRouteEnforces(t *testing.T) {
	for _, tc := range execLimitsCases {
		t.Run(tc.name, func(t *testing.T) {
			b := newLimitsBackend()
			s := newExecTestServer(b, tc.exec)
			_, info := getInfo(t, s.Router())
			if info.Exec == nil {
				t.Fatal("/api/info has no exec member")
			}
			lim := *info.Exec

			accepted := func(t *testing.T, what string, req config.ExecRequest) execResult {
				t.Helper()
				res := postExec(t, s, "box", req)
				if res.status != http.StatusOK || res.exit == nil || res.exit.Code != 0 {
					t.Fatalf("%s: status %d body %.300s, want 200 and exit 0", what, res.status, res.body)
				}
				return res
			}
			refused := func(t *testing.T, what string, req config.ExecRequest, status int, code string) {
				t.Helper()
				res := postExec(t, s, "box", req)
				if res.status != status || apiErrorCode(t, res.body) != code {
					t.Fatalf("%s: status %d body %.300s, want %d %s", what, res.status, res.body, status, code)
				}
			}

			t.Run("timeout", func(t *testing.T) {
				accepted(t, "timeout_seconds at max_timeout_seconds",
					config.ExecRequest{Argv: []string{"x"}, TimeoutSeconds: int(lim.MaxTimeoutSeconds)})
				refused(t, "timeout_seconds past max_timeout_seconds",
					config.ExecRequest{Argv: []string{"x"}, TimeoutSeconds: int(lim.MaxTimeoutSeconds) + 1},
					http.StatusBadRequest, config.ErrInvalidRequest)

				// The applied default is 60 s, or the configured maximum
				// (exact, fraction included) when that is lower. The report
				// drops the fraction; the route applies it.
				applied := 60 * time.Second
				if tc.exec != nil && tc.exec.MaxTimeout > 0 && time.Duration(tc.exec.MaxTimeout) < applied {
					applied = time.Duration(tc.exec.MaxTimeout)
				}
				if d := time.Duration(lim.DefaultTimeoutSeconds) * time.Second; applied < d || applied-d >= time.Second {
					t.Fatalf("default_timeout_seconds %d is not the applied default %s rounded down", lim.DefaultTimeoutSeconds, applied)
				}
				before := time.Now()
				accepted(t, "no timeout_seconds", config.ExecRequest{Argv: []string{"deadline"}})
				after := time.Now()
				b.mu.Lock()
				dl := b.deadline
				b.mu.Unlock()
				// The deadline was set while the request ran, so it lies
				// between the two clock readings plus the applied default:
				// a default off by even a second falls outside.
				if dl.Before(before.Add(applied)) || dl.After(after.Add(applied)) {
					t.Errorf("a request with no timeout_seconds was given %s, want exactly %s (the applied default)", dl.Sub(before), applied)
				}
			})

			t.Run("output", func(t *testing.T) {
				accepted(t, "max_output_bytes at max_output_bytes",
					config.ExecRequest{Argv: []string{"x"}, MaxOutputBytes: lim.MaxOutputBytes})
				refused(t, "max_output_bytes past max_output_bytes",
					config.ExecRequest{Argv: []string{"x"}, MaxOutputBytes: lim.MaxOutputBytes + 1},
					http.StatusBadRequest, config.ErrInvalidRequest)

				res := accepted(t, "no max_output_bytes",
					config.ExecRequest{Argv: []string{"flood", strconv.FormatInt(lim.DefaultOutputBytes+1, 10)}})
				if n := int64(len(res.data[config.ExecFrameStdout])); n != lim.DefaultOutputBytes || !res.exit.Truncated {
					t.Errorf("a request with no max_output_bytes kept %d bytes (truncated %v), want %d (default_output_bytes) and truncated",
						n, res.exit.Truncated, lim.DefaultOutputBytes)
				}
			})

			t.Run("stdin", func(t *testing.T) {
				stdin := func(n int64) string {
					return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("s"), int(n)))
				}
				accepted(t, "stdin at max_stdin_bytes",
					config.ExecRequest{Argv: []string{"x"}, Stdin: stdin(lim.MaxStdinBytes)})
				refused(t, "stdin past max_stdin_bytes",
					config.ExecRequest{Argv: []string{"x"}, Stdin: stdin(lim.MaxStdinBytes + 1)},
					http.StatusRequestEntityTooLarge, config.ErrRequestTooLarge)
			})

			t.Run("argv and env", func(t *testing.T) {
				argv := func(n int) []string {
					a := make([]string, n)
					for i := range a {
						a[i] = "a"
					}
					return a
				}
				env := func(n int) map[string]string {
					e := map[string]string{}
					for i := range n {
						e["K"+strconv.Itoa(i)] = "v"
					}
					return e
				}
				accepted(t, "argv at max_argv_elements", config.ExecRequest{Argv: argv(lim.MaxArgvElements)})
				refused(t, "argv past max_argv_elements", config.ExecRequest{Argv: argv(lim.MaxArgvElements + 1)},
					http.StatusBadRequest, config.ErrInvalidRequest)
				accepted(t, "env at max_env_keys", config.ExecRequest{Argv: []string{"x"}, Env: env(lim.MaxEnvKeys)})
				refused(t, "env past max_env_keys", config.ExecRequest{Argv: []string{"x"}, Env: env(lim.MaxEnvKeys + 1)},
					http.StatusBadRequest, config.ErrInvalidRequest)
				big := func(n int64) []string { return []string{strings.Repeat("b", int(n))} }
				accepted(t, "argv at max_argv_env_bytes", config.ExecRequest{Argv: big(lim.MaxArgvEnvBytes)})
				refused(t, "argv past max_argv_env_bytes", config.ExecRequest{Argv: big(lim.MaxArgvEnvBytes + 1)},
					http.StatusBadRequest, config.ErrInvalidRequest)
				// The route counts every argv element, then each env entry
				// as key + "=" + value: argv "x" (1) + "K" (1) + "=" (1)
				// + the value.
				withEnv := func(n int64) config.ExecRequest {
					return config.ExecRequest{Argv: []string{"x"}, Env: map[string]string{"K": strings.Repeat("v", int(n-3))}}
				}
				accepted(t, "argv and env at max_argv_env_bytes", withEnv(lim.MaxArgvEnvBytes))
				refused(t, "argv and env past max_argv_env_bytes", withEnv(lim.MaxArgvEnvBytes+1),
					http.StatusBadRequest, config.ErrInvalidRequest)
			})

			t.Run("concurrency", func(t *testing.T) {
				ts := httptest.NewServer(s.Router())
				defer ts.Close()
				client := &http.Client{Timeout: 20 * time.Second}
				post := func(cmd string) *http.Response {
					body, _ := json.Marshal(config.ExecRequest{Argv: []string{cmd}})
					resp, err := client.Post(ts.URL+"/api/sheds/box/exec", "application/json", bytes.NewReader(body))
					if err != nil {
						t.Fatalf("POST exec %q: %v", cmd, err)
					}
					return resp
				}
				var held []*http.Response
				var releaseOnce sync.Once
				drain := func() {
					releaseOnce.Do(func() { close(b.release) })
					for _, r := range held {
						_, _ = io.Copy(io.Discard, r.Body)
						_ = r.Body.Close()
					}
					held = nil
				}
				defer drain()
				for range lim.MaxConcurrentPerShed {
					resp := post("block")
					held = append(held, resp)
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("exec %d of max_concurrent_per_shed (%d): status %d", len(held), lim.MaxConcurrentPerShed, resp.StatusCode)
					}
					select {
					case <-b.started:
					case <-time.After(10 * time.Second):
						t.Fatalf("exec %d of max_concurrent_per_shed (%d) never reached the backend", len(held), lim.MaxConcurrentPerShed)
					}
				}
				resp := post("x")
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusTooManyRequests || apiErrorCode(t, body) != config.ErrExecLimit {
					t.Fatalf("one exec past max_concurrent_per_shed (%d): status %d body %s, want 429 EXEC_LIMIT",
						lim.MaxConcurrentPerShed, resp.StatusCode, body)
				}

				// With the held execs finished the slots are free again: one
				// more exec is admitted (the slot is released just after the
				// response ends, so allow a moment).
				drain()
				deadline := time.Now().Add(10 * time.Second)
				for {
					resp := post("x")
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						break
					}
					if resp.StatusCode != http.StatusTooManyRequests || time.Now().After(deadline) {
						t.Fatalf("an exec after the held ones finished: status %d, want 200 (the slots came back)", resp.StatusCode)
					}
					time.Sleep(20 * time.Millisecond)
				}
			})
		})
	}
}

// hugeTimeouts are timeout_seconds values a request can carry far past any
// maximum: up to the largest integer a JSON number decodes into
// (math.MaxInt64). With timeout_seconds multiplied by a second before the
// comparison, each of these overflows (MaxInt64 to -1 s), and the request
// would be accepted with a nonsense timeout.
var hugeTimeouts = []int64{
	// 9223372036, the largest whose product with a second fits, and the
	// first that overflows.
	math.MaxInt64 / int64(time.Second),
	math.MaxInt64/int64(time.Second) + 1,
	math.MaxInt64 / int64(time.Second) * 2,
	// The largest integer a float64 holds exactly, and the next.
	1 << 53,
	1<<53 + 1,
	// The largest a JSON number decodes into, and the one before.
	math.MaxInt64 - 1,
	math.MaxInt64,
}

// TestExecLimitsAtConfigBounds: a server whose exec settings are all at
// their upper bounds (config.MaxExecMax*) reports them, and the route holds
// a request to exactly what it reports: a timeout at max_timeout_seconds is
// given that whole timeout, one past it and every far larger one (up to the
// largest JSON integer) is refused; a request with stdin is not refused as
// too large (the body cap computed from max_stdin_bytes does not overflow);
// and max_concurrent_per_shed execs fill the shed, one more is refused.
//
// A request at, or past, a 1 GiB max_stdin_bytes is not sent: the server
// would buffer several GiB. The stdin check itself is the one the
// configured-values cases drive at each reported maximum; here the body cap
// is checked to leave room for a stdin of max_stdin_bytes.
func TestExecLimitsAtConfigBounds(t *testing.T) {
	atBounds := &config.ExecConfig{
		MaxStdinBytes:        config.MaxExecMaxStdinBytes,
		MaxTimeout:           config.Duration(config.MaxExecMaxTimeout),
		MaxConcurrentPerShed: config.MaxExecMaxConcurrentPerShed,
	}
	// That such a config loads is internal/config's TestExecConfigBoundsFromYAML.
	b := newLimitsBackend()
	s := newExecTestServer(b, atBounds)
	_, info := getInfo(t, s.Router())
	if info.Exec == nil {
		t.Fatal("/api/info has no exec member")
	}
	lim := *info.Exec
	want := config.ExecLimits{
		MaxStdinBytes: 1 << 30, MaxTimeoutSeconds: 7 * 24 * 3600, DefaultTimeoutSeconds: 60,
		MaxOutputBytes: 268435456, DefaultOutputBytes: 4194304, MaxConcurrentPerShed: 4096,
		MaxArgvElements: 1024, MaxEnvKeys: 256, MaxArgvEnvBytes: 1 << 20,
	}
	if lim != want {
		t.Fatalf("exec = %+v\nwant   %+v", lim, want)
	}

	t.Run("timeout", func(t *testing.T) {
		before := time.Now()
		res := postExec(t, s, "box", config.ExecRequest{Argv: []string{"deadline"}, TimeoutSeconds: int(lim.MaxTimeoutSeconds)})
		after := time.Now()
		if res.status != http.StatusOK || res.exit == nil || res.exit.Code != 0 {
			t.Fatalf("timeout_seconds at max_timeout_seconds: status %d body %.300s, want 200 and exit 0", res.status, res.body)
		}
		b.mu.Lock()
		dl := b.deadline
		b.mu.Unlock()
		given := time.Duration(lim.MaxTimeoutSeconds) * time.Second
		if dl.Before(before.Add(given)) || dl.After(after.Add(given)) {
			t.Errorf("timeout_seconds %d was given %s, want %s", lim.MaxTimeoutSeconds, dl.Sub(before), given)
		}
		for _, n := range append([]int64{lim.MaxTimeoutSeconds + 1}, hugeTimeouts...) {
			res := postExec(t, s, "box", config.ExecRequest{Argv: []string{"x"}, TimeoutSeconds: int(n)})
			if res.status != http.StatusBadRequest || apiErrorCode(t, res.body) != config.ErrInvalidRequest {
				t.Errorf("timeout_seconds %d (max %d): status %d body %.300s, want 400 INVALID_REQUEST", n, lim.MaxTimeoutSeconds, res.status, res.body)
			}
		}
		// One past the largest integer: the body does not decode.
		res = postExec(t, s, "box", `{"argv":["x"],"timeout_seconds":9223372036854775808}`)
		if res.status != http.StatusBadRequest || apiErrorCode(t, res.body) != config.ErrInvalidRequest {
			t.Errorf("timeout_seconds 2^63: status %d body %.300s, want 400 INVALID_REQUEST", res.status, res.body)
		}
	})

	t.Run("stdin", func(t *testing.T) {
		res := postExec(t, s, "box", config.ExecRequest{Argv: []string{"x"}, Stdin: base64.StdEncoding.EncodeToString([]byte("hi"))})
		if res.status != http.StatusOK || res.exit == nil || res.exit.Code != 0 {
			t.Fatalf("a small stdin: status %d body %.300s, want 200", res.status, res.body)
		}
		room := int64(base64.StdEncoding.EncodedLen(int(lim.MaxStdinBytes))) + 64<<10
		if got := s.execLimits().bodyLimit(); got < room {
			t.Errorf("body cap %d leaves no room for a stdin of max_stdin_bytes (%d) and its request (%d)", got, lim.MaxStdinBytes, room)
		}
	})

	t.Run("concurrency", func(t *testing.T) {
		var releases []func()
		defer func() {
			for _, r := range releases {
				r()
			}
		}()
		for i := range lim.MaxConcurrentPerShed {
			r, ok := s.acquireExecSlot("box", s.execLimits().maxConcurrentPerShed)
			if !ok {
				t.Fatalf("slot %d of max_concurrent_per_shed (%d) refused", i+1, lim.MaxConcurrentPerShed)
			}
			releases = append(releases, r)
		}
		res := postExec(t, s, "box", config.ExecRequest{Argv: []string{"x"}})
		if res.status != http.StatusTooManyRequests || apiErrorCode(t, res.body) != config.ErrExecLimit {
			t.Fatalf("one exec past max_concurrent_per_shed (%d): status %d body %.300s, want 429 EXEC_LIMIT",
				lim.MaxConcurrentPerShed, res.status, res.body)
		}
		releases[0]()
		releases = releases[1:]
		res = postExec(t, s, "box", config.ExecRequest{Argv: []string{"x"}})
		if res.status != http.StatusOK {
			t.Fatalf("the last slot of max_concurrent_per_shed (%d): status %d body %.300s, want 200",
				lim.MaxConcurrentPerShed, res.status, res.body)
		}
	})
}

// TestExecRouteArithmeticWithoutValidation: the route's own arithmetic is
// safe whatever exec values it is handed, not only those config validation
// lets through. With the largest representable max_timeout, timeout_seconds
// one past the reported maximum is still refused (multiplied first, it
// overflowed and escaped the refusal); with a max_stdin_bytes near int64's
// end, the body cap saturates instead of going negative (which refused
// every request as too large).
func TestExecRouteArithmeticWithoutValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec *config.ExecConfig
	}{
		{"largest durations and sizes", &config.ExecConfig{MaxStdinBytes: math.MaxInt64, MaxTimeout: config.Duration(math.MaxInt64)}},
		{"stdin 2^61", &config.ExecConfig{MaxStdinBytes: 1 << 61, MaxTimeout: config.Duration(math.MaxInt64 - 1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Past the bounds on purpose: the route must not rely on them.
			if tc.exec.MaxStdinBytes <= config.MaxExecMaxStdinBytes || tc.exec.MaxTimeout.Duration() <= config.MaxExecMaxTimeout {
				t.Fatalf("%+v is within the config bounds; this test wants values only the route sees", *tc.exec)
			}
			s := newExecTestServer(newLimitsBackend(), tc.exec)
			_, info := getInfo(t, s.Router())
			if info.Exec == nil {
				t.Fatal("/api/info has no exec member")
			}
			lim := *info.Exec
			if lim.MaxStdinBytes != tc.exec.MaxStdinBytes || lim.MaxTimeoutSeconds != int64(time.Duration(tc.exec.MaxTimeout)/time.Second) {
				t.Fatalf("exec = %+v, want the configured values", lim)
			}
			if got := s.execLimits().bodyLimit(); got != math.MaxInt64 {
				t.Errorf("body cap %d, want it saturated at %d", got, int64(math.MaxInt64))
			}

			res := postExec(t, s, "box", config.ExecRequest{Argv: []string{"x"}, Stdin: base64.StdEncoding.EncodeToString([]byte("hi"))})
			if res.status != http.StatusOK || res.exit == nil || res.exit.Code != 0 {
				t.Fatalf("a small request: status %d body %.300s, want 200 (the body cap must not overflow)", res.status, res.body)
			}
			res = postExec(t, s, "box", config.ExecRequest{Argv: []string{"x"}, TimeoutSeconds: int(lim.MaxTimeoutSeconds)})
			if res.status != http.StatusOK {
				t.Fatalf("timeout_seconds at max_timeout_seconds (%d): status %d body %.300s, want 200", lim.MaxTimeoutSeconds, res.status, res.body)
			}
			for _, n := range hugeTimeouts {
				if n <= lim.MaxTimeoutSeconds {
					continue
				}
				res := postExec(t, s, "box", config.ExecRequest{Argv: []string{"x"}, TimeoutSeconds: int(n)})
				if res.status != http.StatusBadRequest || apiErrorCode(t, res.body) != config.ErrInvalidRequest {
					t.Errorf("timeout_seconds %d (max %d): status %d body %.300s, want 400 INVALID_REQUEST", n, lim.MaxTimeoutSeconds, res.status, res.body)
				}
			}
		})
	}
}

// TestInfoExecLimitsSameForEveryCaller: on an mtls server with a service
// client, a person and the client read the same exec member (the limits are
// the server's, not per caller), and a caller /api/info refuses learns
// nothing of them.
// TestExecDefaultConfigRefusesAnOverflowingTimeout: on a server with no exec
// settings at all, a timeout_seconds so large that multiplying it by a second
// wraps to a negative duration is refused like any other value past the
// maximum. Before the comparison was made in whole seconds, such a request
// passed the check on every server, the default one included.
func TestExecDefaultConfigRefusesAnOverflowingTimeout(t *testing.T) {
	s := newExecTestServer(newLimitsBackend(), nil)
	_, info := getInfo(t, s.Router())
	if info.Exec == nil {
		t.Fatal("/api/info has no exec member")
	}
	if got, want := info.Exec.MaxTimeoutSeconds, int64(config.DefaultExecMaxTimeout/time.Second); got != want {
		t.Fatalf("default max_timeout_seconds = %d, want %d", got, want)
	}
	for _, n := range hugeTimeouts {
		res := postExec(t, s, "box", config.ExecRequest{Argv: []string{"x"}, TimeoutSeconds: int(n)})
		if res.status != http.StatusBadRequest || apiErrorCode(t, res.body) != config.ErrInvalidRequest {
			t.Errorf("timeout_seconds %d on a default server: status %d body %.300s, want 400 INVALID_REQUEST", n, res.status, res.body)
		}
	}
}

func TestInfoExecLimitsSameForEveryCaller(t *testing.T) {
	s, router := clientTestServer(t, newClientFakeBackend(), defaultTestPolicy())
	s.cfg.Exec = &config.ExecConfig{MaxTimeout: config.Duration(20 * time.Minute), MaxConcurrentPerShed: 2}

	read := func(who string, r apiResp) config.ExecLimits {
		t.Helper()
		var info config.ServerInfo
		if r.code != http.StatusOK {
			t.Fatalf("%s: GET /api/info = %d %s", who, r.code, r.body)
		}
		if err := json.Unmarshal(r.body, &info); err != nil {
			t.Fatal(err)
		}
		if info.Exec == nil || !info.HasFeature(FeatureExecLimits) {
			t.Fatalf("%s: exec %v, features %v", who, info.Exec, info.Features)
		}
		return *info.Exec
	}
	person := read("a person", do(router, adminLeaf(), "GET", "/api/info", ""))
	client := read("a service client", do(router, serviceLeaf(testClientName), "GET", "/api/info", ""))
	if person != client {
		t.Errorf("a person reads %+v, a service client %+v", person, client)
	}
	if person.MaxTimeoutSeconds != 1200 || person.MaxConcurrentPerShed != 2 {
		t.Errorf("exec = %+v, want the configured max_timeout 1200 s and 2 per shed", person)
	}

	for _, tc := range []struct {
		who  string
		resp apiResp
		code int
	}{
		{"no certificate", do(router, nil, "GET", "/api/info", ""), http.StatusUnauthorized},
		{"a credentials certificate", do(router, testClientCert(testAdminFP, "credentials", time.Hour), "GET", "/api/info", ""), http.StatusForbidden},
	} {
		if tc.resp.code != tc.code {
			t.Errorf("%s: GET /api/info = %d, want %d", tc.who, tc.resp.code, tc.code)
		}
		if bytes.Contains(tc.resp.body, []byte("max_timeout_seconds")) || bytes.Contains(tc.resp.body, []byte(FeatureExecLimits)) {
			t.Errorf("%s: the refusal carries the exec limits: %s", tc.who, tc.resp.body)
		}
	}
}
