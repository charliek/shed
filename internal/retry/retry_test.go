package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDo(t *testing.T) {
	t.Run("succeeds on first try", func(t *testing.T) {
		calls := 0
		err := Do(context.Background(), "op", []time.Duration{0, 0}, nil, func() error {
			calls++
			return nil
		})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1", calls)
		}
	})

	t.Run("succeeds after transient failure", func(t *testing.T) {
		calls := 0
		err := Do(context.Background(), "op", []time.Duration{0, 0}, nil, func() error {
			calls++
			if calls == 1 {
				return errors.New("transient")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if calls != 2 {
			t.Errorf("calls = %d, want 2 (one failure + one success)", calls)
		}
	})

	t.Run("exhausts attempts and returns last error", func(t *testing.T) {
		sentinel := errors.New("persistent")
		calls := 0
		err := Do(context.Background(), "op", []time.Duration{0, 0}, nil, func() error {
			calls++
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Errorf("Do: got %v, want %v", err, sentinel)
		}
		if calls != 3 {
			t.Errorf("calls = %d, want 3 (1 + len(backoffs))", calls)
		}
	})

	t.Run("non-retryable short-circuits", func(t *testing.T) {
		sentinel := errors.New("hard fail")
		calls := 0
		err := Do(context.Background(), "op",
			[]time.Duration{0, 0},
			func(err error) bool { return !errors.Is(err, sentinel) },
			func() error {
				calls++
				return sentinel
			})
		if !errors.Is(err, sentinel) {
			t.Errorf("Do: got %v, want %v", err, sentinel)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (non-retryable should not retry)", calls)
		}
	})

	t.Run("context cancellation aborts during backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		errCh := make(chan error, 1)
		go func() {
			errCh <- Do(ctx, "op", []time.Duration{1 * time.Second, 1 * time.Second}, nil, func() error {
				calls++
				return errors.New("transient")
			})
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Do: got %v, want context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Do did not return after context cancel")
		}
		if calls < 1 {
			t.Errorf("calls = %d, want at least 1", calls)
		}
	})

	t.Run("default classifier treats context.Canceled as non-retryable", func(t *testing.T) {
		calls := 0
		err := Do(context.Background(), "op", []time.Duration{0, 0}, nil, func() error {
			calls++
			return context.Canceled
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Do: got %v, want context.Canceled", err)
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (context.Canceled is non-retryable)", calls)
		}
	})
}

func TestDefaultIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not retryable", nil, false},
		{"context.Canceled is not retryable", context.Canceled, false},
		{"context.DeadlineExceeded is not retryable", context.DeadlineExceeded, false},
		{"plain error is retryable", errors.New("oops"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultIsRetryable(tc.err); got != tc.want {
				t.Errorf("DefaultIsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
