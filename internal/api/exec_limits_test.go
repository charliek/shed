package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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

// TestInfoExecLimitsSameForEveryCaller: on an mtls server with a service
// client, a person and the client read the same exec member (the limits are
// the server's, not per caller), and a caller /api/info refuses learns
// nothing of them.
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
