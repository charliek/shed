package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/remote/provider"
)

// The client's status matrix, end to end over HTTP: each test serves the
// replies it names and checks what the client makes of them.

const testShed = "q6c3g4ymb5m2dkxwe7kyz5hhzu"

// fakeSession hands out a new bearer for each token, and counts forgets.
type fakeSession struct {
	issuer  string
	hc      *http.Client
	mu      sync.Mutex
	n       int
	forgot  []string
	current string
	denied  bool
}

func (s *fakeSession) ComputeAccess(context.Context) (directory.ComputeAccess, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.denied {
		return directory.ComputeAccess{}, directory.ErrComputeDenied
	}
	if s.current == "" {
		s.n++
		s.current = fmt.Sprintf("bearer-%d", s.n)
	}
	return directory.ComputeAccess{Bearer: s.current, Assertion: "assertion-" + s.current, Server: "srv-a", Issuer: s.issuer, HTTP: s.hc}, nil
}

func (s *fakeSession) ForgetComputeToken(b string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgot = append(s.forgot, b)
	if s.current == b {
		s.current = ""
	}
}

// serve runs handler as the service and returns a client of it.
func serve(t *testing.T, handler http.HandlerFunc) (*Client, *fakeSession) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	s := &fakeSession{issuer: ts.URL, hc: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	c, err := New(Config{Session: s, TestDrivers: true, PollFirst: time.Millisecond, PollAfter: time.Millisecond, CallDeadline: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func machineJSON(shedID, state string) map[string]any {
	return map[string]any{
		"shed_id": shedID, "name": "widgets", "server": "srv-a", "user": "ada", "client": nil,
		"state": state, "size": map[string]any{"name": "standard", "cpus": 2, "memory_mb": 8192, "arch": "amd64"},
		"disk_gb": 50, "image": map[string]any{"requested": "default", "resolved": "shed-remote-full-v1"},
		"provider": "ref", "created_at": "2026-11-16T06:00:00Z", "started_at": nil, "stopped_at": nil,
		"lease":  map[string]any{"expires_at": "2026-11-17T06:00:00Z", "max_expires_at": "2026-11-21T06:00:00Z", "extendable": true, "presets_h": []int{6}},
		"access": []any{}, "labels": map[string]any{}, "spec": "s",
	}
}

func writeMachine(w http.ResponseWriter, status int, m map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"machine": m})
}

func TestReauthOnce(t *testing.T) {
	var bearers []string
	var mu sync.Mutex
	c, s := serve(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearers = append(bearers, r.Header.Get("Authorization"))
		n := len(bearers)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeMachine(w, 200, machineJSON(testShed, "running"))
	})
	inst, err := c.Get(context.Background(), testShed)
	if err != nil || inst.State != provider.StateRunning {
		t.Fatalf("Get after a 401 = %+v, %v", inst.State, err)
	}
	if len(bearers) != 2 || bearers[0] == bearers[1] {
		t.Errorf("bearers sent = %v, want two different", bearers)
	}
	if len(s.forgot) != 1 || "Bearer "+s.forgot[0] != bearers[0] {
		t.Errorf("forgotten = %v, want the refused one", s.forgot)
	}
}

func TestSecond401IsUnavailable(t *testing.T) {
	var n atomic.Int32
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := c.Get(context.Background(), testShed)
	if !errors.Is(err, provider.ErrUnavailable) || n.Load() != 2 {
		t.Fatalf("Get after two 401s = %v (%d attempts), want unavailable after two", err, n.Load())
	}
}

func TestRetryAfter(t *testing.T) {
	var n atomic.Int32
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeMachine(w, 200, machineJSON(testShed, "running"))
	})
	if _, err := c.Get(context.Background(), testShed); err != nil || n.Load() != 2 {
		t.Fatalf("Get after a 429 = %v (%d attempts)", err, n.Load())
	}
	c2, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	start := time.Now()
	_, err := c2.Get(context.Background(), testShed)
	if !errors.Is(err, provider.ErrUnavailable) || provider.CodeOf(err) != "RATE_LIMITED" {
		t.Fatalf("a 429 past the deadline = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("a wait past the deadline was waited out: %s", time.Since(start))
	}
}

func TestServerErrors(t *testing.T) {
	for _, status := range []int{500, 502, 503, 400, 405, 410} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"code":"UNAVAILABLE","message":"x"}}`)
			})
			_, err := c.Get(context.Background(), testShed)
			if !errors.Is(err, provider.ErrUnavailable) {
				t.Errorf("a read answered %d = %v, want unavailable", status, err)
			}
			_, err = c.Stop(context.Background(), testShed, "")
			if !errors.Is(err, provider.ErrUnavailable) {
				t.Errorf("a mutation answered %d = %v, want unavailable (outcome unknown)", status, err)
			}
		})
	}
}

func TestSilenceIsUnavailable(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(release) }) // runs first: the handler returns, then the server closes
	s := &fakeSession{issuer: ts.URL, hc: ts.Client()}
	c, _ := New(Config{Session: s, CallDeadline: 150 * time.Millisecond})
	start := time.Now()
	_, err := c.Start(context.Background(), testShed, "")
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("a call with no answer = %v, want unavailable", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the call waited %s, past its deadline", d)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var target atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { target.Add(1) }))
	t.Cleanup(other.Close)
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	})
	_, err := c.Get(context.Background(), testShed)
	if !errors.Is(err, provider.ErrUnavailable) || target.Load() != 0 {
		t.Fatalf("a redirect = %v, %d requests to its target; want unavailable and none", err, target.Load())
	}
}

func TestEchoedBearerRefused(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		m := machineJSON(testShed, "running")
		m["spec"] = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		writeMachine(w, 200, m)
	})
	_, err := c.Get(context.Background(), testShed)
	if !errors.Is(err, provider.ErrInvalidReply) {
		t.Fatalf("a reply that echoes the bearer = %v, want an invalid reply", err)
	}
}

func TestMessageNeverShown(t *testing.T) {
	const secret = "a-message-the-server-must-not-repeat"
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"BUSY","message":"`+secret+`"}}`)
	})
	_, err := c.Stop(context.Background(), testShed, "")
	if !errors.Is(err, provider.ErrBusy) || provider.CodeOf(err) != "BUSY" {
		t.Fatalf("a 409 BUSY = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error shows the service's message: %v", err)
	}
}

func TestOversizeReplyIsInvalid(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		m := machineJSON(testShed, "running")
		m["pad"] = strings.Repeat("a", MaxReply)
		writeMachine(w, 200, m)
	})
	_, err := c.Get(context.Background(), testShed)
	if !errors.Is(err, provider.ErrInvalidReply) {
		t.Fatalf("a reply over 64 KiB = %v, want an invalid reply", err)
	}
}

func TestEveryMutationCarriesARequestID(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RequestID string `json:"request_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = body.RequestID
		mu.Unlock()
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/ingress"):
			_, _ = io.WriteString(w, `{"sources":[],"exclusive":true,"applied_at":null}`)
		default:
			writeMachine(w, 200, machineJSON(testShed, "running"))
		}
	})
	ctx := context.Background()
	_, _ = c.Start(ctx, testShed, "")
	_, _ = c.Stop(ctx, testShed, "")
	_, _ = c.Destroy(ctx, testShed, "")
	_, _ = c.SetLabels(ctx, testShed, "", map[string]*string{"a": provider.Str("b")}, nil)
	_, _ = c.Extend(ctx, testShed, "", time.Hour)
	_, _ = c.SetIngress(ctx, "", nil)
	if len(seen) != 6 {
		t.Fatalf("calls seen = %v", seen)
	}
	for call, rid := range seen {
		if !ValidRequestID(rid) {
			t.Errorf("%s carried request id %q", call, rid)
		}
	}
}

func TestDeniedAtTheToken(t *testing.T) {
	c, s := serve(t, func(w http.ResponseWriter, r *http.Request) { t.Error("a call was sent without a token") })
	s.denied = true
	_, err := c.Get(context.Background(), testShed)
	if !errors.Is(err, provider.ErrDenied) || provider.CodeOf(err) != "COMPUTE_DENIED" {
		t.Fatalf("a token the service refuses = %v, want denied", err)
	}
}

func TestWaitFollowsTheMachineWhenTheOperationIsUnknown(t *testing.T) {
	var gets atomic.Int32
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/compute/operations/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"NOT_FOUND","message":"x"}}`)
		default:
			state := "starting"
			if gets.Add(1) > 2 {
				state = "running"
			}
			writeMachine(w, 200, machineJSON(testShed, state))
		}
	})
	inst, err := c.Wait(context.Background(), provider.Pending{Ref: testShed, Operation: "op-1", Want: provider.StateRunning})
	if err != nil || inst.State != provider.StateRunning || gets.Load() < 3 {
		t.Fatalf("Wait on an unknown operation = %s, %v (%d reads)", inst.State, err, gets.Load())
	}
}
