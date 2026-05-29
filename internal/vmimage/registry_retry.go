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
	"net"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/charliek/shed/internal/retry"
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

// withRetry is a registry-flavoured wrapper around retry.Do: same
// loop and cancellation contract, paired with the HTTP/network
// classifier below. Mount-side callers in the VZ/FC clients use
// retry.Do directly with the permissive default classifier.
func withRetry(ctx context.Context, opName string, fn func() error) error {
	return retry.Do(ctx, opName, retryBackoffs, isRetryablePullErr, fn)
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
