// Package retry is the shared "call this, back off, try again" loop
// for transient-failure-prone operations across the codebase.
//
// Originally just the registry-pull wrapper in internal/vmimage, the
// loop was lifted here so the in-guest mount call sites (VZ VirtioFS,
// FC 9P) could share the same backoff and cancellation contract
// without a cycle through vmutil/config/vmimage. The HTTP-specific
// retryable classifier stays in vmimage; this package owns only the
// loop + a permissive default classifier suitable for opaque RPC
// failures.
package retry

import (
	"context"
	"errors"
	"log"
	"time"
)

// Do calls fn with the supplied backoff schedule. After fn returns a
// non-nil error, Do consults isRetryable(err):
//
//   - true  → wait the next backoff (or return ctx.Err() if the caller
//     cancelled in the meantime) and call fn again.
//   - false → return the error verbatim.
//
// Total attempts = 1 + len(backoffs). The first attempt runs
// immediately; backoff[i] is the wait BEFORE attempt i+2.
//
// opName is included in the retry log line emitted on each retry so
// the operator can see which call is stuttering. The final error is
// returned without wrapping so callers that pattern-match on a
// sentinel still see it.
//
// If isRetryable is nil, Do uses [DefaultIsRetryable], which retries
// any non-context-cancellation error.
func Do(ctx context.Context, opName string, backoffs []time.Duration, isRetryable func(error) bool, fn func() error) error {
	if isRetryable == nil {
		isRetryable = DefaultIsRetryable
	}
	err := fn()
	if err == nil || !isRetryable(err) {
		return err
	}
	totalAttempts := len(backoffs) + 1
	for i, backoff := range backoffs {
		log.Printf("retrying %s after transient error (attempt %d/%d in %v): %v", opName, i+2, totalAttempts, backoff, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		err = fn()
		if err == nil || !isRetryable(err) {
			return err
		}
	}
	return err
}

// DefaultIsRetryable treats any non-nil error that isn't a context
// cancellation or deadline as retryable. Appropriate for in-guest RPC
// calls (mounts, agent exec) where the failure shapes are
// unpredictable but most short blips are worth one more attempt.
//
// Callers with a tighter classification (e.g., HTTP 4xx is permanent)
// pass their own classifier to [Do].
func DefaultIsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}
