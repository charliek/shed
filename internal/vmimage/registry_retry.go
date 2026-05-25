// Transient-failure retry wrapper for the registry-direct pull path.
//
// Without retries, a single TCP reset / DNS hiccup / 5xx during a
// shed image pull surfaces as a hard failure. That's particularly
// painful for the kernel/initrd loose-blob fetches that ride along
// with `pull-images`: lose one of those mid-stream and the user has
// to re-run the whole command. Adding three-attempt exponential
// backoff covers the common "blip during a 10-minute pull" failure
// mode without hiding real outages (since 4xx errors and context
// cancellations skip the retry loop).

package vmimage

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// retryAttempts and retryBackoffs configure the retry envelope.
//
// 1 s / 4 s = ~5 s total wait on the worst case, which is short
// enough that a foreground `shed image pull` doesn't feel hung
// but long enough to absorb a typical TCP retransmit window. Two
// retries (three attempts total) is the sweet spot: one retry
// catches the common case, two catches the unlucky cascade, and
// adding a third costs more than it earns since persistent
// failures are usually persistent (DNS misconfigured, auth wrong,
// etc.).
var retryBackoffs = []time.Duration{1 * time.Second, 4 * time.Second}

// withRetry calls fn, and on transient failure waits + retries
// according to retryBackoffs. opName is included in the log line
// emitted on each retry so the user can see which fetch is
// stuttering. Returns the last attempt's error verbatim — no
// wrapping that would hide retry semantics from a caller that
// pattern-matches on the original error.
func withRetry(ctx context.Context, opName string, fn func() error) error {
	err := fn()
	if err == nil || !isRetryablePullErr(err) {
		return err
	}
	for i, backoff := range retryBackoffs {
		log.Printf("retrying %s after transient error (attempt %d/%d in %v): %v", opName, i+2, len(retryBackoffs)+1, backoff, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		err = fn()
		if err == nil || !isRetryablePullErr(err) {
			return err
		}
	}
	return err
}

// isRetryablePullErr returns true for the error shapes a brief
// registry blip typically produces. Conservative on purpose:
// retrying a 401/403/404 just spams the registry and delays the
// real diagnostic.
func isRetryablePullErr(err error) bool {
	if err == nil {
		return false
	}
	// Context cancellation / deadline is the caller saying "stop",
	// not a transient failure.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Mid-stream TCP cut. Common on flaky links during the
	// multi-hundred-MB layer pulls.
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	// Network-layer issues (DNS, connection refused, TCP reset).
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// go-containerregistry classifies HTTP failures via
	// transport.Error. 5xx + 429 are worth retrying; 4xx (auth,
	// not-found) are not.
	var transErr *transport.Error
	if errors.As(err, &transErr) {
		code := transErr.StatusCode
		return code >= 500 || code == 429
	}
	// Fallback for string-matched lower-layer errors that don't
	// implement Is/As nicely. Lowercased comparison — the net/http
	// "tls handshake timeout" string is lowercase but historical
	// reports of it appear in mixed case, and lowercasing once is
	// cheaper than maintaining both spellings.
	msg := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"connection reset by peer",
		"connection refused",
		"i/o timeout",
		"tls handshake timeout",
		"no such host",
	} {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}
