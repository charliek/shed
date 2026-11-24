package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The machine identity a HostClient sends, and the server's credential-source
// refusals it reports.

const testMachineID = "0f3c9a2e7b1d4c6a8e5f2b9d1c7a3e60"

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// serverDecodeName reads a machine name header as shed-server does
// (internal/api/source_machine.go: machineFromRequest, which this module
// cannot import): printable ASCII only, then a path unescape. The server's
// own tests pin the decoding of a percent-encoded non-ASCII name
// (internal/api/source_admission_test.go: TestSourceAdmissionTable).
func serverDecodeName(raw string) (string, bool) {
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] > 0x7e {
			return "", false
		}
	}
	name, err := url.PathUnescape(raw)
	return name, err == nil
}

// TestEncodeMachineNameMatchesTheBroker: the bytes on the wire are the Rust
// source's for the same name (crates/shed-source/src/delivery.rs:
// percent_encode_name), and the server's decoding gives the name back.
func TestEncodeMachineNameMatchesTheBroker(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"laptop", "laptop"},
		{"Mini-2.lan_x~", "Mini-2.lan_x~"},
		{"laptop (work)", "laptop%20%28work%29"},
		{"a+b%c/d", "a%2Bb%25c%2Fd"},
		{"büro", "b%C3%BCro"},
		{"日本", "%E6%97%A5%E6%9C%AC"},
		{"büro 100% (work)", "b%C3%BCro%20100%25%20%28work%29"},
	} {
		got := encodeMachineName(tc.name)
		if got != tc.want {
			t.Errorf("encodeMachineName(%q) = %q, want %q", tc.name, got, tc.want)
		}
		if back, ok := serverDecodeName(got); !ok || back != tc.name {
			t.Errorf("the server decodes %q as %q (ok %v), want %q", got, back, ok, tc.name)
		}
	}
}

// recordedHeaders is what a test server saw on each call.
type recordedHeaders struct {
	mu        sync.Mutex
	subscribe http.Header
	respond   http.Header
}

// headerServer serves one SSE event on subscribe and 204 on respond,
// recording each call's headers.
func headerServer(t *testing.T, rec *recordedHeaders) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/messages") {
			rec.subscribe = r.Header.Clone()
		} else {
			rec.respond = r.Header.Clone()
		}
		rec.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/respond") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		data, _ := json.Marshal(NewEnvelope("ns", MessageTypeRequest, json.RawMessage(`{}`)))
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// subscribeAndRespond subscribes, takes one envelope, and answers it.
func subscribeAndRespond(t *testing.T, c *HostClient) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch := c.Subscribe(ctx, "ns")
	env, ok := <-ch
	if !ok {
		t.Fatal("subscription ended before an envelope")
	}
	if err := c.Respond(ctx, "ns", NewResponse(env.ID, "ns", json.RawMessage(`{}`))); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	cancel()
	for range ch {
	}
}

// TestMachineHeadersOnSubscribeAndRespond: with WithMachine both calls carry
// the id as is and the name percent-encoded, and the server's decoding gives
// the name back (spaces, non-ASCII and a percent sign included).
func TestMachineHeadersOnSubscribeAndRespond(t *testing.T) {
	const name = "büro 100% (work)"
	rec := &recordedHeaders{}
	srv := headerServer(t, rec)
	c := NewHostClient(WithServerURL(srv.URL), WithLogger(quietLogger()), WithMachine(testMachineID, name))
	subscribeAndRespond(t, c)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for call, h := range map[string]http.Header{"subscribe": rec.subscribe, "respond": rec.respond} {
		if h == nil {
			t.Fatalf("%s: never reached the server", call)
		}
		if got := h.Values(HeaderMachineID); len(got) != 1 || got[0] != testMachineID {
			t.Errorf("%s: %s = %q, want [%q]", call, HeaderMachineID, got, testMachineID)
		}
		got := h.Values(HeaderMachineName)
		if len(got) != 1 || got[0] != "b%C3%BCro%20100%25%20%28work%29" {
			t.Errorf("%s: %s = %q, want the exact percent-encoded bytes", call, HeaderMachineName, got)
			continue
		}
		if back, ok := serverDecodeName(got[0]); !ok || back != name {
			t.Errorf("%s: the server decodes the name as %q (ok %v), want %q", call, back, ok, name)
		}
	}
}

// TestNoMachineSendsNoHeaders: without WithMachine neither call carries a
// machine header, as before.
func TestNoMachineSendsNoHeaders(t *testing.T) {
	rec := &recordedHeaders{}
	srv := headerServer(t, rec)
	c := NewHostClient(WithServerURL(srv.URL), WithLogger(quietLogger()))
	subscribeAndRespond(t, c)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	for call, h := range map[string]http.Header{"subscribe": rec.subscribe, "respond": rec.respond} {
		for _, k := range []string{HeaderMachineID, HeaderMachineName} {
			if v, ok := h[http.CanonicalHeaderKey(k)]; ok {
				t.Errorf("%s: sent %s = %q with no identity set", call, k, v)
			}
		}
	}
}

// TestValidateMachine is the server's rules, on the client.
func TestValidateMachine(t *testing.T) {
	for _, tc := range []struct {
		what     string
		id, name string
		ok       bool
	}{
		{"valid", testMachineID, "laptop", true},
		{"63-byte name", testMachineID, strings.Repeat("é", 31) + "x", true},
		{"spaces, punctuation and non-ASCII", testMachineID, "büro 100% (work) ’", true},
		{"empty id", "", "laptop", false},
		{"uppercase id", strings.ToUpper(testMachineID), "laptop", false},
		{"hyphenated id", "0f3c9a2e-7b1d-4c6a-8e5f-2b9d1c7a3e60", "laptop", false},
		{"short id", testMachineID[:31], "laptop", false},
		{"long id", testMachineID + "0", "laptop", false},
		{"non-hex id", strings.Repeat("g", 32), "laptop", false},
		{"empty name", testMachineID, "", false},
		{"64-byte name", testMachineID, strings.Repeat("é", 32), false},
		{"newline in the name", testMachineID, "a\nb", false},
		{"NUL in the name", testMachineID, "a\x00b", false},
		{"DEL in the name", testMachineID, "a\x7fb", false},
		{"C1 control in the name", testMachineID, "a\u0085b", false},
		{"invalid UTF-8", testMachineID, "a\xffb", false},
	} {
		err := ValidateMachine(tc.id, tc.name)
		if tc.ok && err != nil {
			t.Errorf("%s: %v, want valid", tc.what, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalidMachine) {
			t.Errorf("%s: %v, want ErrInvalidMachine", tc.what, err)
		}
	}
}

// TestInvalidMachineSendsNothing: a client built with an invalid identity
// never reaches the server. Respond returns the validation error; the
// subscription stops at once as ConnRejected, carrying it.
func TestInvalidMachineSendsNothing(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewHostClient(WithServerURL(srv.URL), WithLogger(quietLogger()), WithMachine("not-an-id", "laptop"))

	if err := c.Respond(context.Background(), "ns", NewResponse("r", "ns", nil)); !errors.Is(err, ErrInvalidMachine) {
		t.Fatalf("Respond = %v, want ErrInvalidMachine", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case _, ok := <-c.Subscribe(ctx, "ns"):
		if ok {
			t.Fatal("an envelope from a subscription that should not have started")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the subscription with an invalid identity did not stop")
	}
	st, _ := nsState(c, "ns")
	if st.State != ConnRejected || !errors.Is(st.Err, ErrInvalidMachine) {
		t.Fatalf("status = %+v, want ConnRejected with ErrInvalidMachine", st)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the server was reached %d times", n)
	}
}

// padTo pads a JSON body with trailing spaces to exactly n bytes.
func padTo(body string, n int) string {
	if len(body) > n {
		panic("body longer than the padding")
	}
	return body + strings.Repeat(" ", n-len(body))
}

// wantRetryAfter checks a rate-limit refusal's RetryAfter.
func wantRetryAfter(want time.Duration) func(t *testing.T, se *SourceError) {
	return func(t *testing.T, se *SourceError) {
		if se.RetryAfter != want {
			t.Errorf("RetryAfter = %s, want %s", se.RetryAfter, want)
		}
	}
}

// refusalBody is a server's refusal body.
func refusalBody(code, message string, source map[string]any) string {
	body := map[string]any{"error": map[string]string{"code": code, "message": message}}
	if source != nil {
		body["source"] = source
	}
	data, _ := json.Marshal(body)
	return string(data)
}

// TestRespondReportsSourceRefusals: each refusal the server sends is a
// *SourceError matching its sentinel, with the bound machine or the
// Retry-After; anything else (another code, a code with the wrong status, a
// body that is not JSON or is cut short) is reported as before.
func TestRespondReportsSourceRefusals(t *testing.T) {
	notBound := refusalBody(CodeSourceNotBound, "not this machine", map[string]any{"machine_id": testMachineID, "machine_name": "desk ’"})
	for _, tc := range []struct {
		what       string
		status     int
		body       string
		retryAfter string
		want       error // nil: not a SourceError
		check      func(t *testing.T, se *SourceError)
	}{
		{"not bound", 403, notBound, "", ErrSourceNotBound, func(t *testing.T, se *SourceError) {
			if se.MachineID != testMachineID || se.MachineName != "desk ’" || se.Message != "not this machine" {
				t.Errorf("fields = %+v, want the bound machine and the message", se)
			}
		}},
		{"not bound, no machine is the source", 403, refusalBody(CodeSourceNotBound, "none", nil), "", ErrSourceNotBound, func(t *testing.T, se *SourceError) {
			if se.MachineID != "" || se.MachineName != "" {
				t.Errorf("fields = %+v, want no machine", se)
			}
		}},
		{"not bound, wrongly typed source fields", 403, `{"error":{"code":"SOURCE_NOT_BOUND"},"source":{"machine_id":7,"machine_name":null}}`, "", ErrSourceNotBound, func(t *testing.T, se *SourceError) {
			if se.MachineID != "" || se.MachineName != "" {
				t.Errorf("fields = %+v, want a wrongly typed machine_id and machine_name read as empty", se)
			}
		}},
		{"machine id required", 403, refusalBody(CodeSourceMachineIDRequired, "upgrade", nil), "", ErrSourceMachineIDRequired, nil},
		{"duplicate stream", 409, refusalBody(CodeSourceDuplicateStream, "already holds", nil), "", ErrSourceDuplicateStream, nil},
		{"rate limited", 429, refusalBody(CodeSourceRateLimited, "slow down", nil), "7", ErrSourceRateLimited, func(t *testing.T, se *SourceError) {
			if se.RetryAfter != 7*time.Second {
				t.Errorf("RetryAfter = %s, want 7s", se.RetryAfter)
			}
		}},
		{"rate limited, no Retry-After", 429, refusalBody(CodeSourceRateLimited, "slow down", nil), "", ErrSourceRateLimited, func(t *testing.T, se *SourceError) {
			if se.RetryAfter != 0 {
				t.Errorf("RetryAfter = %s, want 0", se.RetryAfter)
			}
		}},
		{"rate limited, at the ten-minute cap", 429, refusalBody(CodeSourceRateLimited, "slow down", nil), "600", ErrSourceRateLimited, wantRetryAfter(10 * time.Minute)},
		{"rate limited, past the ten-minute cap", 429, refusalBody(CodeSourceRateLimited, "slow down", nil), "601", ErrSourceRateLimited, wantRetryAfter(10 * time.Minute)},
		// Seconds that overflow a Duration when multiplied (to a negative and
		// to a tiny wait) are capped, not wrapped.
		{"rate limited, a Retry-After that overflows to negative", 429, refusalBody(CodeSourceRateLimited, "slow down", nil), "9223372037", ErrSourceRateLimited, wantRetryAfter(10 * time.Minute)},
		{"rate limited, a Retry-After that wraps to milliseconds", 429, refusalBody(CodeSourceRateLimited, "slow down", nil), "18446744074", ErrSourceRateLimited, wantRetryAfter(10 * time.Minute)},
		{"rate limited, a Retry-After too large for an int", 429, refusalBody(CodeSourceRateLimited, "slow down", nil), "99999999999999999999", ErrSourceRateLimited, wantRetryAfter(0)},
		{"rate limited, a negative Retry-After", 429, refusalBody(CodeSourceRateLimited, "slow down", nil), "-5", ErrSourceRateLimited, wantRetryAfter(0)},
		{"rate limited, a garbage Retry-After", 429, refusalBody(CodeSourceRateLimited, "slow down", nil), "soon", ErrSourceRateLimited, wantRetryAfter(0)},
		{"rate limited, an HTTP-date Retry-After", 429, refusalBody(CodeSourceRateLimited, "slow down", nil), "Wed, 21 Oct 2015 07:28:00 GMT", ErrSourceRateLimited, wantRetryAfter(0)},
		// A whole refusal padded to exactly 1024 bytes is complete; one more
		// byte and it is not.
		{"a refusal of exactly 1024 bytes", 409, padTo(refusalBody(CodeSourceDuplicateStream, "already holds", nil), 1024), "", ErrSourceDuplicateStream, nil},
		{"a refusal of 1025 bytes", 409, padTo(refusalBody(CodeSourceDuplicateStream, "already holds", nil), 1025), "", nil, nil},
		{"another 403", 403, refusalBody("FORBIDDEN", "no", nil), "", nil, nil},
		{"not bound under the wrong status", 409, refusalBody(CodeSourceNotBound, "x", nil), "", nil, nil},
		{"duplicate under the wrong status", 403, refusalBody(CodeSourceDuplicateStream, "x", nil), "", nil, nil},
		{"not JSON", 403, "forbidden", "", nil, nil},
		{"cut short", 403, refusalBody(CodeSourceNotBound, strings.Repeat("m", 1100), nil), "", nil, nil},
		// What is read (the first 1024 bytes) parses on its own, but the body
		// goes on: never classified.
		{"cut short after a whole refusal", 403, notBound + strings.Repeat(" ", 1100), "", nil, nil},
	} {
		t.Run(tc.what, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := NewHostClient(WithServerURL(srv.URL), WithMachine(testMachineID, "laptop"))
			err := c.Respond(context.Background(), "ns", NewResponse("r", "ns", nil))
			var se *SourceError
			if tc.want == nil {
				if errors.As(err, &se) {
					t.Fatalf("Respond = %v, want an error that is not a SourceError", err)
				}
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unexpected status %d", tc.status)) {
					t.Fatalf("Respond = %v, want today's unexpected status error", err)
				}
				return
			}
			if !errors.Is(err, tc.want) || !errors.As(err, &se) {
				t.Fatalf("Respond = %v, want a SourceError matching %v", err, tc.want)
			}
			for _, other := range []error{ErrSourceNotBound, ErrSourceMachineIDRequired, ErrSourceDuplicateStream, ErrSourceRateLimited} {
				if other != tc.want && errors.Is(err, other) {
					t.Errorf("Respond = %v also matches %v", err, other)
				}
			}
			if se.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", se.StatusCode, tc.status)
			}
			if tc.check != nil {
				tc.check(t, se)
			}
		})
	}
}

// countingTokens is a TokenProvider that counts Invalidate calls.
type countingTokens struct{ invalidated atomic.Int32 }

func (c *countingTokens) Token() (string, error) { return "tok", nil }
func (c *countingTokens) Invalidate()            { c.invalidated.Add(1) }

// TestSubscribeRetriesSourceRefusals: a subscription refused as not the
// source, or as a duplicate of this machine's own stream, keeps retrying on
// the backoff (a duplicate 409 is not the terminal 409), shows the refusal in
// Status as a SourceError, and never re-mints the credential.
func TestSubscribeRetriesSourceRefusals(t *testing.T) {
	for _, tc := range []struct {
		what   string
		status int
		body   string
		want   error
	}{
		{"not the source", 403, refusalBody(CodeSourceNotBound, "not this machine", map[string]any{"machine_id": testMachineID, "machine_name": "desk"}), ErrSourceNotBound},
		{"duplicate stream", 409, refusalBody(CodeSourceDuplicateStream, "already holds", nil), ErrSourceDuplicateStream},
		{"machine id required", 403, refusalBody(CodeSourceMachineIDRequired, "name this machine", nil), ErrSourceMachineIDRequired},
	} {
		t.Run(tc.what, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			tokens := &countingTokens{}
			c := NewHostClient(WithServerURL(srv.URL), WithLogger(quietLogger()), WithTokenProvider(tokens), WithMachine(testMachineID, "laptop"))
			ctx, cancel := context.WithCancel(context.Background())
			ch := c.Subscribe(ctx, "ns")
			waitFor(t, "a retry", func() bool { return hits.Load() >= 2 })
			st, _ := nsState(c, "ns")
			var se *SourceError
			if st.State != ConnReconnecting || !errors.Is(st.Err, tc.want) || !errors.As(st.Err, &se) {
				t.Fatalf("status = %+v, want reconnecting with a SourceError matching %v", st, tc.want)
			}
			if tc.want == ErrSourceNotBound && se.MachineName != "desk" {
				t.Errorf("status names %q, want the bound machine desk", se.MachineName)
			}
			if n := tokens.invalidated.Load(); n != 0 {
				t.Errorf("the credential was invalidated %d times on a source refusal", n)
			}
			cancel()
			for range ch {
			}
		})
	}
}

// TestSubscribeWaitsForRetryAfter: a rate-limit refusal's Retry-After holds
// the next attempt back past the 1 s backoff.
func TestSubscribeWaitsForRetryAfter(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, refusalBody(CodeSourceRateLimited, "slow down", nil))
	}))
	defer srv.Close()
	c := NewHostClient(WithServerURL(srv.URL), WithLogger(quietLogger()), WithMachine(testMachineID, "laptop"))
	ctx, cancel := context.WithCancel(context.Background())
	ch := c.Subscribe(ctx, "ns")
	waitFor(t, "a retry", func() bool { mu.Lock(); defer mu.Unlock(); return len(at) >= 2 })
	cancel()
	for range ch {
	}
	mu.Lock()
	defer mu.Unlock()
	if gap := at[1].Sub(at[0]); gap < 2*time.Second {
		t.Fatalf("retried after %s, want at least the 2 s Retry-After", gap)
	}
}

// TestSubscribeHugeRetryAfterDoesNotRetryEarly: a Retry-After so large that
// it would overflow a Duration still holds the next attempt back (to the
// ten-minute cap), instead of wrapping to a negative or tiny wait that lets
// the 1 s backoff retry.
func TestSubscribeHugeRetryAfterDoesNotRetryEarly(t *testing.T) {
	for _, value := range []string{"9223372037", "18446744074"} {
		t.Run(value, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("Retry-After", value)
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, refusalBody(CodeSourceRateLimited, "slow down", nil))
			}))
			defer srv.Close()
			c := NewHostClient(WithServerURL(srv.URL), WithLogger(quietLogger()), WithMachine(testMachineID, "laptop"))
			ctx, cancel := context.WithCancel(context.Background())
			ch := c.Subscribe(ctx, "ns")
			waitFor(t, "the first attempt", func() bool { return hits.Load() >= 1 })
			time.Sleep(initialBackoff + 500*time.Millisecond)
			cancel()
			for range ch {
			}
			if n := hits.Load(); n != 1 {
				t.Fatalf("the server was asked %d times, want once (Retry-After %s holds the retry back)", n, value)
			}
		})
	}
}

// TestSubStatusErrClearedAfterRecovery: once a refused subscription
// connects, its status carries no error, as a value or as text.
func TestSubStatusErrClearedAfterRecovery(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, refusalBody(CodeSourceNotBound, "not this machine", nil))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		data, _ := json.Marshal(NewEnvelope("ns", MessageTypeRequest, json.RawMessage(`{}`)))
		fmt.Fprintf(w, "data: %s\n\n", data)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := NewHostClient(WithServerURL(srv.URL), WithLogger(quietLogger()), WithMachine(testMachineID, "laptop"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch := c.Subscribe(ctx, "ns")
	if _, ok := <-ch; !ok {
		t.Fatal("the subscription ended before it recovered")
	}
	if n := hits.Load(); n < 2 {
		t.Fatalf("connected after %d attempts, want a refusal first", n)
	}
	st, _ := nsState(c, "ns")
	if st.State != ConnConnected || st.Err != nil || st.LastError != "" {
		t.Fatalf("status = %+v, want connected with no error", st)
	}
	cancel()
	for range ch {
	}
}

// TestSubStatusJSONHasNoErr: Err is not part of a status's JSON, so a
// consumer of that JSON sees the members it always did.
func TestSubStatusJSONHasNoErr(t *testing.T) {
	data, err := json.Marshal(SubStatus{Namespace: "ns", State: ConnReconnecting, LastError: "refused", Err: errors.New("refused")})
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		t.Fatal(err)
	}
	if _, ok := members["Err"]; ok || len(members) != 4 {
		t.Fatalf("SubStatus JSON = %s, want Namespace, State, LastError and Since alone", data)
	}
}

// shortErrorBodyTimeout shortens the time a non-success body may take, for
// the rest of the test.
func shortErrorBodyTimeout(t *testing.T) {
	t.Helper()
	prev := errorBodyReadTimeout
	errorBodyReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { errorBodyReadTimeout = prev })
}

// respondWithin runs Respond and fails the test when it has not returned
// within d.
func respondWithin(t *testing.T, c *HostClient, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Respond(context.Background(), "ns", NewResponse("r", "ns", nil)) }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("Respond did not return within %s", d)
		return nil
	}
}

// checkUnclassified409: a 409 whose body was not read whole is not a
// refusal. A subscription ends as the terminal ConnRejected (the bare 409);
// Respond reports today's unexpected status.
func checkUnclassified409(t *testing.T, srvURL string) {
	t.Helper()
	c := NewHostClient(WithServerURL(srvURL), WithLogger(quietLogger()), WithMachine(testMachineID, "laptop"))
	err := respondWithin(t, c, 3*time.Second)
	var se *SourceError
	if errors.As(err, &se) || err == nil || !strings.Contains(err.Error(), "unexpected status 409") {
		t.Errorf("Respond = %v, want the unexpected status 409 error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := c.Subscribe(ctx, "ns")
	waitFor(t, "the subscription to stop", func() bool {
		st, _ := nsState(c, "ns")
		return st.State == ConnRejected
	})
	st, _ := nsState(c, "ns")
	if !errors.Is(st.Err, errSubscribeConflict) || errors.As(st.Err, &se) {
		t.Errorf("status = %+v, want the bare 409's terminal error", st)
	}
	for range ch {
	}
}

// TestStalledRefusalBodyEndsInTime: a chunked 409 that sends a whole
// refusal, padded to exactly 1024 bytes, and then neither ends nor closes is
// read for a bounded time only; it did not end, so it is not a refusal.
func TestStalledRefusalBodyEndsInTime(t *testing.T) {
	shortErrorBodyTimeout(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, padTo(refusalBody(CodeSourceDuplicateStream, "already holds", nil), 1024))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	checkUnclassified409(t, srv.URL)
}

// TestTruncatedRefusalBodyIsNotClassified: a whole refusal whose response
// promised more bytes than the transport delivered (the connection closed
// early) is not complete, so it is not a refusal.
func TestTruncatedRefusalBodyIsNotClassified(t *testing.T) {
	body := refusalBody(CodeSourceDuplicateStream, "already holds", nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(buf, "HTTP/1.1 409 Conflict\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body)+100, body)
		_ = buf.Flush()
	}))
	defer srv.Close()
	checkUnclassified409(t, srv.URL)
}

// TestRedirectAllowedTable is the redirect rule on its own (the Rust broker's
// redirect_allowed_table, the same rows where the rules agree).
func TestRedirectAllowedTable(t *testing.T) {
	u := func(s string) *url.URL {
		parsed, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	base := u("https://shed.example:8443/")
	for _, tc := range []struct {
		configured *url.URL
		next       string
		hops       int
		want       bool
	}{
		{base, "https://shed.example:8443/api/x", 1, true},
		{base, "https://SHED.example:8443/api/x", maxRedirects, true},
		{base, "https://shed.example:8444/api/x", 1, false},
		{base, "https://other.example:8443/api/x", 1, false},
		{base, "https://evil.shed.example:8443/api/x", 1, false},
		{base, "http://shed.example:8443/api/x", 1, false},
		{base, "https://shed.example:8443/api/x", maxRedirects + 1, false},
		{nil, "https://shed.example:8443/", 1, false},
		// The default port is the same origin as the explicit one.
		{u("https://shed.example/"), "https://shed.example:443/x", 1, true},
		{u("https://shed.example/"), "https://shed.example:8443/x", 1, false},
		// A plain-http server's own origin; never to https on another port.
		{u("http://127.0.0.1:8080/"), "http://127.0.0.1:8080/x", 1, true},
		{u("http://127.0.0.1:8080/"), "http://localhost:8080/x", 1, false},
		{u("http://127.0.0.1:8080/"), "https://127.0.0.1:8443/x", 1, false},
		{u("http://shed.example/"), "http://shed.example:80/x", 1, true},
	} {
		if got := redirectAllowed(tc.configured, u(tc.next), tc.hops); got != tc.want {
			t.Errorf("redirectAllowed(%v, %s, %d) = %v, want %v", tc.configured, tc.next, tc.hops, got, tc.want)
		}
	}
}

// redirecting serves a 307 to target for the subscribe and respond paths,
// and counts what reaches every other path (a redirect's target on the same
// server).
func redirecting(target func(r *http.Request) string, elsewhere *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/plugins/listeners/") {
			http.Redirect(w, r, target(r), http.StatusTemporaryRedirect)
			return
		}
		elsewhere.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}
}

// checkRedirectNotFollowed: neither call follows the configured server's
// redirect, and each reports it as an unexpected 307.
func checkRedirectNotFollowed(t *testing.T, c *HostClient) {
	t.Helper()
	err := respondWithin(t, c, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "unexpected status 307") {
		t.Errorf("Respond = %v, want the redirect as an unexpected status 307", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := c.Subscribe(ctx, "ns")
	waitFor(t, "the subscription's first attempt", func() bool {
		st, _ := nsState(c, "ns")
		return strings.Contains(st.LastError, "unexpected status")
	})
	st, _ := nsState(c, "ns")
	if !strings.Contains(st.LastError, "unexpected status 307") {
		t.Errorf("status = %+v, want the redirect as an unexpected status 307", st)
	}
	cancel()
	for range ch {
	}
}

// TestRedirectsNeverLeaveTheConfiguredOrigin: a redirect to another port, or
// to another host name for the same server, is not followed: the other origin
// receives no request at all.
func TestRedirectsNeverLeaveTheConfiguredOrigin(t *testing.T) {
	t.Run("another port", func(t *testing.T) {
		var other atomic.Int32
		otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			other.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))
		defer otherSrv.Close()
		var elsewhere atomic.Int32
		srv := httptest.NewServer(redirecting(func(*http.Request) string { return otherSrv.URL + "/moved" }, &elsewhere))
		defer srv.Close()
		c := NewHostClient(WithServerURL(srv.URL), WithLogger(quietLogger()), WithToken("tok"), WithMachine(testMachineID, "laptop"))
		checkRedirectNotFollowed(t, c)
		if n := other.Load(); n != 0 {
			t.Fatalf("the other origin received %d requests", n)
		}
	})
	t.Run("another host name", func(t *testing.T) {
		var elsewhere atomic.Int32
		// The same server under the name localhost: its listener (and so its
		// port) exists before it starts serving.
		srv := httptest.NewUnstartedServer(nil)
		port := srv.Listener.Addr().(*net.TCPAddr).Port
		srv.Config.Handler = redirecting(func(*http.Request) string { return fmt.Sprintf("http://localhost:%d/moved", port) }, &elsewhere)
		srv.Start()
		defer srv.Close()
		c := NewHostClient(WithServerURL(srv.URL), WithLogger(quietLogger()), WithToken("tok"), WithMachine(testMachineID, "laptop"))
		checkRedirectNotFollowed(t, c)
		if n := elsewhere.Load(); n != 0 {
			t.Fatalf("the other host name received %d requests", n)
		}
	})
}

// TestHTTPSRedirectToHTTPIsNotFollowed: an https server's redirect to plain
// http is not followed.
func TestHTTPSRedirectToHTTPIsNotFollowed(t *testing.T) {
	var plain atomic.Int32
	plainSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plain.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer plainSrv.Close()
	var elsewhere atomic.Int32
	srv := httptest.NewTLSServer(redirecting(func(*http.Request) string { return plainSrv.URL + "/moved" }, &elsewhere))
	defer srv.Close()
	c := NewHostClient(WithServerURL(srv.URL), WithHTTPClient(srv.Client()), WithLogger(quietLogger()), WithToken("tok"), WithMachine(testMachineID, "laptop"))
	checkRedirectNotFollowed(t, c)
	if n := plain.Load(); n != 0 {
		t.Fatalf("the plain-http origin received %d requests", n)
	}
}

// TestSameOriginRedirectIsFollowed: a redirect within the configured origin
// is followed, and the request it leads to still carries the machine headers
// and the credential.
func TestSameOriginRedirectIsFollowed(t *testing.T) {
	rec := &recordedHeaders{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/moved" {
			http.Redirect(w, r, "/moved", http.StatusTemporaryRedirect)
			return
		}
		rec.mu.Lock()
		rec.respond = r.Header.Clone()
		rec.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewHostClient(WithServerURL(srv.URL), WithLogger(quietLogger()), WithToken("tok"), WithMachine(testMachineID, "laptop"))
	if err := respondWithin(t, c, 3*time.Second); err != nil {
		t.Fatalf("Respond = %v, want the same-origin redirect followed", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.respond == nil {
		t.Fatal("the redirect's target was never reached")
	}
	if got := rec.respond.Get(HeaderMachineID); got != testMachineID {
		t.Errorf("%s = %q after the redirect, want %q", HeaderMachineID, got, testMachineID)
	}
	if got := rec.respond.Get(HeaderMachineName); got != "laptop" {
		t.Errorf("%s = %q after the redirect, want laptop", HeaderMachineName, got)
	}
	if got := rec.respond.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q after the redirect, want the bearer token", got)
	}
}

// TestRedirectHopLimit: at most three redirects are followed; the fourth is
// the answer.
func TestRedirectHopLimit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/again", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := NewHostClient(WithServerURL(srv.URL), WithLogger(quietLogger()), WithMachine(testMachineID, "laptop"))
	err := respondWithin(t, c, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "unexpected status 307") {
		t.Errorf("Respond = %v, want the fourth redirect as an unexpected status 307", err)
	}
	if n := hits.Load(); n != 1+maxRedirects {
		t.Fatalf("the server was asked %d times, want %d (the request and %d redirects)", n, 1+maxRedirects, maxRedirects)
	}
}

// TestCallerCheckRedirectRunsAfterTheRule: a caller's own CheckRedirect
// cannot widen the rule (a policy that follows everything still does not
// leave the origin, and is not even asked), is asked about a redirect the
// rule allows, and can refuse it.
func TestCallerCheckRedirectRunsAfterTheRule(t *testing.T) {
	var asked atomic.Int32
	followAll := func(*http.Request, []*http.Request) error { asked.Add(1); return nil }

	var other atomic.Int32
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		other.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer otherSrv.Close()
	var elsewhere atomic.Int32
	away := httptest.NewServer(redirecting(func(*http.Request) string { return otherSrv.URL + "/moved" }, &elsewhere))
	defer away.Close()
	hc := &http.Client{CheckRedirect: followAll}
	c := NewHostClient(WithServerURL(away.URL), WithHTTPClient(hc), WithLogger(quietLogger()), WithMachine(testMachineID, "laptop"))
	if err := respondWithin(t, c, 3*time.Second); err == nil || !strings.Contains(err.Error(), "unexpected status 307") {
		t.Errorf("Respond = %v, want the cross-origin redirect as an unexpected status 307", err)
	}
	if n, a := other.Load(), asked.Load(); n != 0 || a != 0 {
		t.Fatalf("the other origin received %d requests and the caller's policy was asked %d times, want neither", n, a)
	}

	home := httptest.NewServer(redirecting(func(*http.Request) string { return "/moved" }, &elsewhere))
	defer home.Close()
	c = NewHostClient(WithServerURL(home.URL), WithHTTPClient(hc), WithLogger(quietLogger()), WithMachine(testMachineID, "laptop"))
	if err := respondWithin(t, c, 3*time.Second); err != nil {
		t.Fatalf("Respond = %v, want the same-origin redirect followed", err)
	}
	if a := asked.Load(); a != 1 {
		t.Fatalf("the caller's policy was asked %d times about the same-origin redirect, want once", a)
	}

	refuse := errors.New("the caller refuses every redirect")
	c = NewHostClient(WithServerURL(home.URL), WithHTTPClient(&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return refuse }}),
		WithLogger(quietLogger()), WithMachine(testMachineID, "laptop"))
	if err := respondWithin(t, c, 3*time.Second); !errors.Is(err, refuse) {
		t.Fatalf("Respond = %v, want the caller's own refusal", err)
	}
}
