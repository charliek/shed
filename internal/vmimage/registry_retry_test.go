package vmimage

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Tests use a zeroed backoff so they don't actually wait between
// retries. The retry envelope is what we care about (right number
// of attempts, right termination on success / non-retryable);
// timing is configuration, not behavior worth verifying here.
func setFastBackoff(t *testing.T) {
	t.Helper()
	orig := retryBackoffs
	retryBackoffs = []time.Duration{0, 0}
	t.Cleanup(func() { retryBackoffs = orig })
}

func TestIsRetryablePullErr(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"net.OpError", &net.OpError{Op: "read", Err: errors.New("reset")}, true},
		{"transport 500", &transport.Error{StatusCode: http.StatusInternalServerError}, true},
		{"transport 502", &transport.Error{StatusCode: http.StatusBadGateway}, true},
		{"transport 503", &transport.Error{StatusCode: http.StatusServiceUnavailable}, true},
		{"transport 429", &transport.Error{StatusCode: http.StatusTooManyRequests}, true},
		{"transport 401", &transport.Error{StatusCode: http.StatusUnauthorized}, false},
		{"transport 403", &transport.Error{StatusCode: http.StatusForbidden}, false},
		{"transport 404", &transport.Error{StatusCode: http.StatusNotFound}, false},
		{"connection reset string", errors.New("read tcp: connection reset by peer"), true},
		{"i/o timeout string", errors.New("dial tcp 1.2.3.4: i/o timeout"), true},
		{"no such host string", errors.New("lookup foo.invalid: no such host"), true},
		{"random string", errors.New("disk full"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetryablePullErr(tt.err)
			if got != tt.want {
				t.Errorf("isRetryablePullErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestWithRetrySucceedsAfterTransient verifies the happy-recovery
// path: fail once with a retryable error, then succeed.
func TestWithRetrySucceedsAfterTransient(t *testing.T) {
	setFastBackoff(t)
	calls := 0
	err := withRetry(context.Background(), "op", func() error {
		calls++
		if calls == 1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withRetry: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (one transient failure + one success)", calls)
	}
}

// TestWithRetryExhausts verifies the failure path runs the full
// attempt budget and returns the last error.
func TestWithRetryExhausts(t *testing.T) {
	setFastBackoff(t)
	calls := 0
	err := withRetry(context.Background(), "op", func() error {
		calls++
		return io.ErrUnexpectedEOF
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("withRetry: got %v, want io.ErrUnexpectedEOF", err)
	}
	want := len(retryBackoffs) + 1
	if calls != want {
		t.Errorf("calls = %d, want %d (initial attempt + len(backoffs) retries)", calls, want)
	}
}

// TestWithRetryDoesNotRetryNonRetryable confirms a 401 / not-found
// short-circuits immediately rather than spamming the registry.
func TestWithRetryDoesNotRetryNonRetryable(t *testing.T) {
	setFastBackoff(t)
	calls := 0
	err := withRetry(context.Background(), "op", func() error {
		calls++
		return &transport.Error{StatusCode: http.StatusUnauthorized}
	})
	if err == nil {
		t.Fatal("withRetry: expected error, got nil")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (non-retryable should not retry)", calls)
	}
}

// TestWithRetryRespectsContext confirms a cancelled context aborts
// the backoff sleep instead of running through it.
func TestWithRetryRespectsContext(t *testing.T) {
	// Use real backoff so cancellation actually has something to interrupt.
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	errCh := make(chan error, 1)
	go func() {
		errCh <- withRetry(ctx, "op", func() error {
			calls++
			return io.ErrUnexpectedEOF
		})
	}()
	// Let the first attempt run, then cancel during backoff.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("withRetry: got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("withRetry did not return after context cancel")
	}
	if calls < 1 {
		t.Errorf("calls = %d, want at least 1", calls)
	}
}
